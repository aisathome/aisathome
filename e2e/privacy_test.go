// End-to-end tests for privacy (identity stripping, secret redaction, no prompts in logs),
// trust-based routing and per-user limits.
//
// SPDX-License-Identifier: Apache-2.0
package e2e

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/aisathome/aisathome/gateway"
)

const secret = "sk-proj-AbCdEfGhIjKlMnOpQrStUvWxYz0123456789"

// echoEngine streams the last user message back in 3-character chunks, then as tool-call
// arguments, like a model copying a value it was given. It records every request body.
func echoEngine(t *testing.T, seen *[]string, mu *sync.Mutex) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if health(w, r) {
			return
		}
		raw, _ := io.ReadAll(r.Body)
		mu.Lock()
		*seen = append(*seen, string(raw))
		mu.Unlock()
		var body struct {
			Messages []struct {
				Content string `json:"content"`
			} `json:"messages"`
		}
		json.Unmarshal(raw, &body)
		last := body.Messages[len(body.Messages)-1].Content
		w.Header().Set("Content-Type", "text/event-stream")
		send := func(delta any) {
			b, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"index": 0, "delta": delta}}})
			fmt.Fprintf(w, "data: %s\n\n", b)
		}
		for i := 0; i < len(last); i += 3 {
			send(map[string]any{"content": last[i:min(i+3, len(last))]})
		}
		args := `{"text":"` + last + `"}`
		send(map[string]any{"tool_calls": []any{map[string]any{"index": 0, "id": "c1", "type": "function",
			"function": map[string]any{"name": "write", "arguments": ""}}}})
		for i := 0; i < len(args); i += 3 {
			send(map[string]any{"tool_calls": []any{map[string]any{"index": 0, "function": map[string]any{"arguments": args[i:min(i+3, len(args))]}}}})
		}
		fmt.Fprint(w, "data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\n")
		fmt.Fprint(w, "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":20,\"completion_tokens\":10}}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
}

type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) { s.mu.Lock(); defer s.mu.Unlock(); return s.b.Write(p) }
func (s *syncBuf) String() string              { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

func TestSecretsNeverReachNodeOrLogs(t *testing.T) {
	logs := &syncBuf{}
	logSink = logs
	defer func() { logSink = io.Discard }()
	var seen []string
	var mu sync.Mutex
	r := newRig(t, echoEngine(t, &seen, &mu))
	prompt := "store OPENAI_API_KEY=" + secret + " in .env"

	for _, stream := range []bool{true, false} {
		body, _ := json.Marshal(map[string]any{"model": "deep", "stream": stream, "user": "alice@example.com",
			"metadata": map[string]any{"chat_id": "c-123"},
			"messages": []any{map[string]any{"role": "user", "content": prompt, "name": "Alice"}}})
		resp := r.do(t, "POST", "/v1/chat/completions", "restore-key", string(body))
		var text, args strings.Builder
		if stream {
			sc := bufio.NewScanner(resp.Body)
			for sc.Scan() {
				line := strings.TrimPrefix(sc.Text(), "data: ")
				var c struct {
					Choices []struct {
						Delta struct {
							Content   string `json:"content"`
							ToolCalls []struct {
								Function struct{ Arguments string } `json:"function"`
							} `json:"tool_calls"`
						} `json:"delta"`
					} `json:"choices"`
				}
				if json.Unmarshal([]byte(line), &c) == nil {
					for _, ch := range c.Choices {
						text.WriteString(ch.Delta.Content)
						for _, tc := range ch.Delta.ToolCalls {
							args.WriteString(tc.Function.Arguments)
						}
					}
				}
			}
		} else {
			var out struct {
				Choices []struct {
					Message struct {
						Content   *string `json:"content"`
						ToolCalls []struct {
							Function struct{ Arguments string } `json:"function"`
						} `json:"tool_calls"`
					} `json:"message"`
				} `json:"choices"`
			}
			json.NewDecoder(resp.Body).Decode(&out)
			if c := out.Choices[0].Message.Content; c != nil {
				text.WriteString(*c)
			}
			args.WriteString(out.Choices[0].Message.ToolCalls[0].Function.Arguments)
		}
		resp.Body.Close()
		// Text gets the real secret back; tool arguments keep the placeholder (restore policy).
		if text.String() != prompt {
			t.Fatalf("stream=%v: user must get the real secret back in text: %q", stream, text.String())
		}
		if strings.Contains(args.String(), secret) || !strings.Contains(args.String(), "sk-proj-AISX") {
			t.Fatalf("stream=%v: tool arguments must keep the placeholder: %q", stream, args.String())
		}
	}
	mu.Lock()
	defer mu.Unlock()
	for _, b := range seen {
		for _, leak := range []string{secret, "alice@example.com", "c-123", "Alice"} {
			if strings.Contains(b, leak) {
				t.Fatalf("node saw %q: %s", leak, b)
			}
		}
	}
	waitUsage(t, r, 2)
	usage, _ := os.ReadFile(r.usageLog)
	for name, s := range map[string]string{"logs": logs.String(), "usage log": string(usage)} {
		if strings.Contains(s, "OPENAI_API_KEY") || strings.Contains(s, secret) {
			t.Fatalf("prompt content in %s", name)
		}
	}
}

// countEngine is mockEngine with a hit counter.
func countEngine(t *testing.T, hits *atomic.Int32) *httptest.Server {
	inner := mockEngine(t, false)
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if health(w, r) {
			return
		}
		hits.Add(1)
		resp, err := http.Post(inner.URL+r.URL.Path, "application/json", r.Body)
		if err != nil {
			return
		}
		defer resp.Body.Close()
		w.Header().Set("Content-Type", resp.Header.Get("Content-Type"))
		io.Copy(w, resp.Body)
	}))
}

func trustRig(t *testing.T, homeHits, strangerHits *atomic.Int32) *rig {
	r := newRigCfg(t, func(c *gateway.Config) {
		c.NodeTokens["home-token"] = gateway.NodeToken{Operator: "owner", Trust: "trusted"}
		c.NodeTokens["stranger-token"] = gateway.NodeToken{Operator: "volunteer"}
		c.Keys["chat-key"] = gateway.KeyInfo{User: "chat", MinTrust: "trusted"}
		c.Keys["own-key"] = gateway.KeyInfo{User: "vol", OnlyOperator: "volunteer"}
	})
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	r.startNode(ctx, "home", "home-token", countEngine(t, homeHits), 4)
	r.startNode(ctx, "stranger", "stranger-token", countEngine(t, strangerHits), 4)
	r.waitNodes(t, 2)
	return r
}

func TestTrustRouting(t *testing.T) {
	var home, stranger atomic.Int32
	r := trustRig(t, &home, &stranger)
	ask := func(key, content string) int {
		b, _ := json.Marshal(map[string]any{"model": "deep", "messages": []any{map[string]any{"role": "user", "content": content}}})
		resp := r.do(t, "POST", "/v1/chat/completions", key, string(b))
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		return resp.StatusCode
	}
	for i := 0; i < 6; i++ { // min_trust key: never the stranger
		if c := ask("chat-key", "hi"); c != 200 {
			t.Fatalf("chat-key: %d", c)
		}
	}
	if stranger.Load() != 0 {
		t.Fatalf("a min_trust=trusted request reached an untrusted node (%d)", stranger.Load())
	}
	home.Store(0)
	for i := 0; i < 6; i++ { // personal data: only trusted nodes, even on a plain key
		ask("user-key", "my email is ola.nordmann@example.no")
	}
	if stranger.Load() != 0 || home.Load() != 6 {
		t.Fatalf("PII reached an untrusted node: home %d stranger %d", home.Load(), stranger.Load())
	}
	home.Store(0)
	for i := 0; i < 4; i++ { // only_operator: only the volunteer's own node
		ask("own-key", "hi")
	}
	if home.Load() != 0 || stranger.Load() != 4 {
		t.Fatalf("only_operator ignored: home %d stranger %d", home.Load(), stranger.Load())
	}
}

func TestPerUserLimits(t *testing.T) {
	r := newRigCfg(t, func(c *gateway.Config) {
		c.Keys["owui-key"] = gateway.KeyInfo{User: "owui", ForwardUsers: true,
			UserLimits: &gateway.UserLimits{RPM: 2, TokensPerDay: 10}}
	}, mockEngine(t, false))
	ask := func(uid string) *http.Response {
		req, _ := http.NewRequest("POST", r.front.URL+"/v1/chat/completions", strings.NewReader(chatBody))
		req.Header.Set("Authorization", "Bearer owui-key")
		if uid != "" {
			req.Header.Set("X-OpenWebUI-User-Id", uid)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		return resp
	}
	// mockEngine reports 12 prompt (8 cached) + 3 completion tokens: weighted 4 + 0 + 3 = 7.
	if c := ask("u1").StatusCode; c != 200 {
		t.Fatalf("u1 first: %d", c)
	}
	waitUsage(t, r, 1)
	if c := ask("u1").StatusCode; c != 200 { // 7 < 10: still allowed
		t.Fatalf("u1 second: %d", c)
	}
	waitUsage(t, r, 2)
	resp := ask("u1") // 14 >= 10: budget used up (and RPM 2 reached)
	if resp.StatusCode != 429 || resp.Header.Get("Retry-After") == "" {
		t.Fatalf("u1 third: %d (Retry-After %q), want 429", resp.StatusCode, resp.Header.Get("Retry-After"))
	}
	if c := ask("u2").StatusCode; c != 200 { // another user has their own limits
		t.Fatalf("u2: %d", c)
	}
	u := waitUsage(t, r, 3)
	if u[2]["user"] != "owui/u2" {
		t.Fatalf("usage must be recorded per end user: %v", u[2]["user"])
	}
}
