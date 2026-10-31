// SPDX-License-Identifier: Apache-2.0
package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/aisathome/aisathome/protocol"
)

func prepared(t *testing.T, g *Gateway, c *chatCaller, content string, extra map[string]any) *chatJob {
	t.Helper()
	body := map[string]any{"model": "deep", "messages": []any{map[string]any{"role": "user", "content": content}}}
	for k, v := range extra {
		body[k] = v
	}
	w := httptest.NewRecorder()
	job, ok := g.chatPrepare(w, c, body)
	if !ok {
		t.Fatalf("prepare: HTTP %d %s", w.Code, w.Body)
	}
	return job
}

// Preparation runs in an order the answer depends on: routing and the guards read what the user
// wrote, and everything a node or the sticky routing sees comes from the redacted request.
func TestChatPrepareOrder(t *testing.T) {
	g := newTestGateway(t, Config{})
	defer g.Close()
	c := &chatCaller{acct: KeyInfo{User: "alice"}, userKey: "alice", class: ClassAgent, charge: []string{"alice"}}

	raw := "my key is " + fakeKey + strings.Repeat(" and more of the same", 2000) // whole blocks to hash
	job := prepared(t, g, c, raw, nil)
	sent, _ := json.Marshal(job.req.body)
	if strings.Contains(string(sent), fakeKey) {
		t.Fatalf("the secret is still in the request: %s", sent)
	}
	if !strings.Contains(job.reqText, strings.ToLower(fakeKey)) { // requestText lowers the case
		t.Fatalf("the guards get the redacted text: %q", job.reqText)
	}
	plain := map[string]any{"model": "deep", "messages": []any{map[string]any{"role": "user", "content": raw}}}
	if job.req.conv.prompt != g.pcache.print(forNode(job.req.body)).prompt || strings.Contains(job.req.conv.prompt, fakeKey) {
		t.Fatalf("the rendered conversation is not the redacted one: %q", job.req.conv.prompt)
	}
	if len(job.rt.conv) == 0 || slices.Equal(job.rt.conv, g.pcache.print(forNode(plain)).hashes) {
		t.Fatalf("routing hashes the secret, or nothing: %d hashes", len(job.rt.conv))
	}

	// A value the redaction takes away still counted as personal data for the route.
	if job := prepared(t, g, c, "password=alice@example.com", nil); !job.rt.trusted {
		t.Fatalf("personal data, hidden by the redaction, goes to any node: %+v; sent %v", job.rt, job.req.body)
	}

	// The route's context need comes from the bounded max_tokens.
	big := prepared(t, g, c, "hi", map[string]any{"max_tokens": 1e9})
	if want := len(mustJSON(forNode(big.req.body)))/3 + 16 + g.cfg.MaxMaxTokens; big.rt.need != want {
		t.Fatalf("need %d, want %d", big.rt.need, want)
	}
}

// A science project's rules reach the route: its share of nodes, its queue time, its class.
func TestChatPrepareProject(t *testing.T) {
	g := newTestGateway(t, Config{})
	defer g.Close()
	c := &chatCaller{acct: KeyInfo{User: "p"}, userKey: "p", class: ClassScience, charge: []string{"p"},
		proj: &Project{ID: "tauceti", MaxNodeShare: 0.7, QueueTimeoutSec: 77}}
	job := prepared(t, g, c, "hi", nil)
	if job.rt.project != "tauceti" || job.rt.share != 0.7 || job.rt.class != ClassScience || job.queueWait != 77 ||
		job.req.project != "tauceti" || job.req.class != ClassScience {
		t.Fatalf("route %+v, wait %d, request %+v", job.rt, job.queueWait, job.req)
	}
}

// When the gateway's own buffers are full the client hears 503 and comes back: no other node is
// tried, since none would do better.
func TestChatGatewayBusyIsNotRetried(t *testing.T) {
	g := newTestGateway(t, Config{Keys: map[string]KeyInfo{"user-key": {User: "alice", MinTrust: "any"}},
		NodeTokens: map[string]NodeToken{"node-token": {Operator: "op1", Trust: "trusted"}}})
	defer g.Close()
	srv := httptest.NewServer(g.Handler())
	defer srv.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var hits atomic.Int32
	for _, id := range []string{"a", "b"} {
		h := http.Header{}
		h.Set("Authorization", "Bearer node-token")
		c, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http")+"/v1/node/connect", &websocket.DialOptions{HTTPHeader: h})
		if err != nil {
			t.Fatal(err)
		}
		write := func(m protocol.Msg) { b, _ := json.Marshal(m); c.Write(ctx, websocket.MessageText, b) }
		write(protocol.Msg{Type: protocol.TypeHello, NodeID: id, Protocol: protocol.LegacyVersion,
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
					hits.Add(1)
					write(protocol.Msg{Type: protocol.TypeChunk, ReqID: m.ReqID, Data: []byte(`{"choices":[{"index":0,"delta":{"content":"x"}}]}`)})
				}
			}
		}()
	}
	for deadline := time.Now().Add(5 * time.Second); ; {
		g.mu.Lock()
		n := len(g.node)
		g.mu.Unlock()
		if n == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d nodes", n)
		}
		time.Sleep(10 * time.Millisecond)
	}
	g.buffered.Store(maxBufferedTotal)
	defer g.buffered.Store(0)
	req, _ := http.NewRequest("POST", srv.URL+"/v1/chat/completions", strings.NewReader(`{"model":"deep","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer user-key")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable || resp.Header.Get("Retry-After") != "10" || hits.Load() != 1 {
		t.Fatalf("HTTP %d, Retry-After %q, %d nodes tried", resp.StatusCode, resp.Header.Get("Retry-After"), hits.Load())
	}
}
