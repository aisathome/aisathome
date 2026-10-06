// End-to-end tests: user → gateway → node (outbound WebSocket) → mock llama-server.
//
// SPDX-License-Identifier: Apache-2.0
package e2e

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"aisathome/internal/gateway"
	"aisathome/internal/node"
	"aisathome/internal/protocol"
)

// hits counts requests that reached the failing mock engine.
var failHits atomic.Int32

// mockEngine imitates llama-server's streaming /v1/chat/completions, including the final
// usage chunk and llama.cpp timings. fail makes it return HTTP 500.
func mockEngine(t *testing.T, fail bool) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if fail {
			failHits.Add(1)
			http.Error(w, "engine broken", http.StatusInternalServerError)
			return
		}
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		if body["stream"] != true {
			t.Errorf("node must request stream=true, got %v", body["stream"])
		}
		w.Header().Set("Content-Type", "text/event-stream")
		model := body["model"]
		for _, piece := range []string{"Hello", ", ", "world"} {
			fmt.Fprintf(w, "data: {\"model\":%q,\"choices\":[{\"index\":0,\"delta\":{\"content\":%q},\"finish_reason\":null}]}\n\n", model, piece)
			w.(http.Flusher).Flush()
		}
		fmt.Fprint(w, "data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n")
		fmt.Fprint(w, "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":12,\"completion_tokens\":3},\"timings\":{\"cache_n\":8,\"prompt_n\":4}}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
}

type rig struct {
	gw       *httptest.Server
	usageLog string
	cancel   context.CancelFunc
}

// newRig starts a gateway and one node agent per engine, and waits until all are registered.
func newRig(t *testing.T, engines ...*httptest.Server) *rig {
	t.Helper()
	dir := t.TempDir()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	usage := filepath.Join(dir, "usage.jsonl")
	g, err := gateway.New(gateway.Config{
		GatewayID:  "test-gw",
		APIKeys:    map[string]string{"user-key": "alice"},
		Keys:       map[string]gateway.KeyInfo{"limited-key": {User: "bob", RPM: 2}},
		NodeTokens: map[string]string{"node-token": "op1"},
		UsageLog:   usage,
	}, log)
	if err != nil {
		t.Fatal(err)
	}
	gw := httptest.NewServer(g.Handler())
	ctx, cancel := context.WithCancel(context.Background())
	for i, e := range engines {
		a := node.New(node.Config{
			NodeID:     fmt.Sprintf("node-%d", i),
			GatewayURL: "ws" + strings.TrimPrefix(gw.URL, "http") + "/v1/node/connect",
			Token:      "node-token",
			Upstream:   e.URL,
			Models:     []protocol.ModelInfo{{Name: "qwen3.8-27b-q4", Roles: []string{"deep"}, Slots: 2}},
		}, log)
		go a.Run(ctx)
	}
	r := &rig{gw: gw, usageLog: usage, cancel: cancel}
	t.Cleanup(func() { cancel(); gw.Close() })
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if g.NodeCount() == len(engines) {
			return r
		}
	}
	t.Fatalf("only %d of %d nodes registered", g.NodeCount(), len(engines))
	return nil
}

func (r *rig) do(t *testing.T, method, path, key, body string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(method, r.gw.URL+path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func (r *rig) usage(t *testing.T) []map[string]any {
	t.Helper()
	b, _ := os.ReadFile(r.usageLog)
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		if line == "" {
			continue
		}
		var m map[string]any
		json.Unmarshal([]byte(line), &m)
		out = append(out, m)
	}
	return out
}

func TestStreamingByRole(t *testing.T) {
	r := newRig(t, mockEngine(t, false))
	resp := r.do(t, "POST", "/v1/chat/completions", "user-key",
		`{"model":"deep","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	defer resp.Body.Close()
	if resp.StatusCode != 200 || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		t.Fatalf("status %d, content-type %q", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	var text strings.Builder
	var sawDone bool
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		line := strings.TrimPrefix(sc.Text(), "data: ")
		if line == "[DONE]" {
			sawDone = true
			break
		}
		var c struct {
			Choices []struct{ Delta struct{ Content string } } `json:"choices"`
		}
		if json.Unmarshal([]byte(line), &c) == nil {
			for _, ch := range c.Choices {
				text.WriteString(ch.Delta.Content)
			}
		}
	}
	if text.String() != "Hello, world" || !sawDone {
		t.Fatalf("got %q, done=%v", text.String(), sawDone)
	}
	u := waitUsage(t, r, 1)
	if u[0]["user"] != "alice" || u[0]["model"] != "qwen3.8-27b-q4" || u[0]["requested"] != "deep" {
		t.Fatalf("bad usage record: %v", u[0])
	}
	usage := u[0]["usage"].(map[string]any)
	if usage["prompt_tokens"] != 12.0 || usage["completion_tokens"] != 3.0 || usage["cached_tokens"] != 8.0 {
		t.Fatalf("bad metering: %v", usage)
	}
}

func TestNonStreaming(t *testing.T) {
	r := newRig(t, mockEngine(t, false))
	resp := r.do(t, "POST", "/v1/chat/completions", "user-key",
		`{"model":"qwen3.8-27b-q4","messages":[{"role":"user","content":"hi"}]}`)
	defer resp.Body.Close()
	var out struct {
		Choices []struct {
			Message      struct{ Content string } `json:"message"`
			FinishReason string                   `json:"finish_reason"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
		} `json:"usage"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil || resp.StatusCode != 200 {
		t.Fatalf("status %d err %v", resp.StatusCode, err)
	}
	if out.Choices[0].Message.Content != "Hello, world" || out.Choices[0].FinishReason != "stop" ||
		out.Usage.PromptTokens != 12 || out.Usage.CompletionTokens != 3 {
		t.Fatalf("bad response: %+v", out)
	}
}

func TestFailoverToSecondNode(t *testing.T) {
	failHits.Store(0)
	broken, healthy := mockEngine(t, true), mockEngine(t, false)
	r := newRig(t, broken, healthy)
	// Two requests: whichever node round-robin picks first, both must succeed.
	for i := 0; i < 2; i++ {
		resp := r.do(t, "POST", "/v1/chat/completions", "user-key",
			`{"model":"deep","messages":[{"role":"user","content":"hi"}]}`)
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 || !strings.Contains(string(b), "Hello, world") {
			t.Fatalf("request %d: status %d body %s", i, resp.StatusCode, b)
		}
	}
	if failHits.Load() == 0 {
		t.Fatal("the broken node was never tried, so failover was not exercised")
	}
	for _, u := range waitUsage(t, r, 2) {
		if u["node"] != "node-1" {
			t.Fatalf("usage must be recorded for the healthy node only: %v", u)
		}
	}
}

func TestErrors(t *testing.T) {
	r := newRig(t, mockEngine(t, false))
	if resp := r.do(t, "POST", "/v1/chat/completions", "wrong-key", `{"model":"deep"}`); resp.StatusCode != 401 {
		t.Fatalf("bad key: got %d", resp.StatusCode)
	}
	if resp := r.do(t, "POST", "/v1/chat/completions", "user-key", `{"model":"no-such-model"}`); resp.StatusCode != 503 {
		t.Fatalf("unknown model: got %d", resp.StatusCode)
	}
}

func waitUsage(t *testing.T, r *rig, n int) []map[string]any {
	t.Helper()
	for i := 0; i < 100; i++ {
		if u := r.usage(t); len(u) >= n {
			return u
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("expected %d usage records, got %d", n, len(r.usage(t)))
	return nil
}

func TestRateLimit(t *testing.T) {
	r := newRig(t, mockEngine(t, false))
	codes := []int{}
	for i := 0; i < 3; i++ {
		resp := r.do(t, "POST", "/v1/chat/completions", "limited-key",
			`{"model":"deep","messages":[{"role":"user","content":"hi"}]}`)
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		codes = append(codes, resp.StatusCode)
	}
	if codes[0] != 200 || codes[1] != 200 || codes[2] != 429 {
		t.Fatalf("expected 200,200,429 with rpm=2, got %v", codes)
	}
}
