// Review 2026-10-10 (third): token revocation while a node is connecting, tool-call output
// in budgets, n > 1, and finish_reason with JSON spacing.
//
// SPDX-License-Identifier: Apache-2.0
package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/aisathome/aisathome/gateway"
	"github.com/aisathome/aisathome/protocol"
)

// A token revoked between the WebSocket handshake and hello must not register the node.
func TestRevokedWhileConnecting(t *testing.T) {
	r := newRigCfg(t, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h := http.Header{}
	h.Set("Authorization", "Bearer node-token")
	c, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(r.gw.URL, "http")+"/v1/node/connect", &websocket.DialOptions{HTTPHeader: h})
	if err != nil {
		t.Fatal(err)
	}
	defer c.CloseNow()
	time.Sleep(100 * time.Millisecond) // the gateway has read the token and waits for hello
	r.g.Reload(gateway.Config{APIKeys: map[string]string{"user-key": "alice"}, NodeTokens: map[string]gateway.NodeToken{}})
	b, _ := json.Marshal(protocol.Msg{Type: protocol.TypeHello, NodeID: "late", Protocol: protocol.Version,
		Models: []protocol.ModelInfo{{Name: "qwen3.8-27b-q4", Roles: []string{"deep"}, Slots: 2}}})
	c.Write(ctx, websocket.MessageText, b)
	time.Sleep(300 * time.Millisecond)
	if n := r.g.NodeCount(); n != 0 {
		t.Fatalf("a node with a revoked token registered (%d nodes)", n)
	}
}

// longToolEngine answers with one long tool call only, and reports that output honestly.
func longToolEngine() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if health(w, r) {
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		args := `{"path":"notes.txt","text":"` + strings.Repeat("lorem ipsum ", 300) + `"}`
		first, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"index": 0, "delta": map[string]any{
			"tool_calls": []any{map[string]any{"index": 0, "id": "c1", "type": "function", "function": map[string]any{"name": "write", "arguments": ""}}}}}}})
		fmt.Fprintf(w, "data: %s\n\n", first)
		for i := 0; i < len(args); i += 40 {
			b, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"index": 0, "delta": map[string]any{
				"tool_calls": []any{map[string]any{"index": 0, "function": map[string]any{"arguments": args[i:min(i+40, len(args))]}}}}}}})
			fmt.Fprintf(w, "data: %s\n\n", b)
		}
		fmt.Fprint(w, "data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\n")
		fmt.Fprint(w, "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":900}}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
}

// Tool-call arguments are generated output and count against the daily budget.
func TestToolCallsChargeTheBudget(t *testing.T) {
	r := newRigCfg(t, func(c *gateway.Config) {
		c.Keys["tool-key"] = gateway.KeyInfo{User: "dev", TokensPerDay: 500}
	}, longToolEngine())
	ask := func() int {
		resp := r.do(t, "POST", "/v1/chat/completions", "tool-key", chatBody)
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		return resp.StatusCode
	}
	if c := ask(); c != 200 {
		t.Fatalf("first request: %d", c)
	}
	waitUsage(t, r, 1)
	if c := ask(); c != 429 {
		t.Fatalf("a 900-token tool call should use up a 500-token budget; second request got %d", c)
	}
}

func TestOnlyOneChoice(t *testing.T) {
	r := newRig(t, mockEngine(t, false))
	resp := r.do(t, "POST", "/v1/chat/completions", "user-key", `{"model":"deep","n":2,"messages":[{"role":"user","content":"hi"}]}`)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 400 {
		t.Fatalf("n=2: got %d, want 400", resp.StatusCode)
	}
}

// spacedEngine ends with `"finish_reason": "stop"` (a space after the colon) and closes the
// stream without [DONE]: a complete answer, not a truncated one.
func spacedEngine() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if health(w, r) {
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\": [{\"index\": 0, \"delta\": {\"content\": \"done\"}}]}\n\n")
		fmt.Fprint(w, "data: {\"choices\": [{\"index\": 0, \"delta\": {}, \"finish_reason\": \"stop\"}]}\n\n")
	}))
}

func TestFinishReasonWithSpaces(t *testing.T) {
	r := newRig(t, spacedEngine())
	for _, stream := range []bool{true, false} {
		body := fmt.Sprintf(`{"model":"deep","stream":%v,"messages":[{"role":"user","content":"hi"}]}`, stream)
		resp := r.do(t, "POST", "/v1/chat/completions", "user-key", body)
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		complete := resp.StatusCode == 200 && strings.Contains(string(b), "done") && !strings.Contains(string(b), `"error"`)
		if stream {
			complete = complete && strings.Contains(string(b), "[DONE]")
		}
		if !complete {
			t.Fatalf("stream=%v: a finished answer was reported as truncated: %d %.300s", stream, resp.StatusCode, b)
		}
	}
}
