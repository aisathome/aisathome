// The end of an answer: a node that leaves right after it, and a client that leaves right
// before it.
//
// SPDX-License-Identifier: Apache-2.0
package e2e

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/coder/websocket"

	"github.com/aisathome/aisathome/gateway"
	"github.com/aisathome/aisathome/protocol"
)

// A node that sends the whole answer and its end, then disconnects at once: the answer is
// still the client's, complete with [DONE], and recorded once as ok. The gateway sees the
// disconnect while the end is still buffered, so the race is run many times.
func TestNodeLeavingRightAfterTheAnswer(t *testing.T) {
	r := newRigCfg(t, func(c *gateway.Config) { c.QueueTimeoutSec = 1 })
	const runs = 200
	for i := 0; i < runs; i++ {
		var c *websocket.Conn
		ready := make(chan struct{})
		c = rawNodeWith(t, r, "brief", "node-token", func(write func(protocol.Msg), _ func([]byte), m protocol.Msg) {
			<-ready
			for _, piece := range []string{"Hello", ", ", "world", "!"} {
				write(chunkMsg(m.ReqID, piece))
			}
			write(protocol.Msg{Type: protocol.TypeDone, ReqID: m.ReqID, Usage: &protocol.Usage{PromptTokens: 5, CompletionTokens: 4}})
			c.CloseNow()
		})
		close(ready)
		r.waitNodes(t, 1)
		resp := r.do(t, "POST", "/v1/chat/completions", "user-key", chatBody)
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 || !strings.Contains(string(b), "world") || !strings.HasSuffix(strings.TrimSpace(string(b)), "data: [DONE]") {
			t.Fatalf("run %d: status %d, the answer did not end with [DONE]: %.300s", i, resp.StatusCode, b)
		}
		r.waitNodes(t, 0)
	}
	u := waitUsage(t, r, runs)
	if len(u) != runs {
		t.Fatalf("%d usage records for %d answers", len(u), runs)
	}
	for i, rec := range u {
		if rec["outcome"] != "ok" {
			t.Fatalf("record %d: %v", i, rec)
		}
	}
}

// cutClient serves h to a client that is gone from the first write gone returns true: the
// request's context ends, and with fail that write and every later one fail, as on a closed
// connection. Without fail the writes still succeed, as they do for a while into a closed
// TCP connection's buffer.
func cutClient(t *testing.T, h http.Handler, fail bool, gone func(p []byte) bool) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithCancel(r.Context())
		defer cancel()
		h.ServeHTTP(&cutWriter{ResponseWriter: w, gone: gone, fail: fail, cancel: cancel}, r.WithContext(ctx))
	}))
	t.Cleanup(srv.Close)
	return srv
}

type cutWriter struct {
	http.ResponseWriter
	gone   func([]byte) bool
	fail   bool
	cancel func()
	cut    bool
}

func (c *cutWriter) Write(p []byte) (int, error) {
	if !c.cut && c.gone(p) {
		c.cut = true
		defer c.cancel()
	}
	if c.cut && c.fail {
		return 0, errors.New("client gone")
	}
	return c.ResponseWriter.Write(p)
}

func (c *cutWriter) Unwrap() http.ResponseWriter { return c.ResponseWriter }

// A client that leaves right before the end of an answer (the tool calls the guard held back,
// or [DONE]) did not get it: the record says cancelled, not ok, and the node earns credit only
// for what reached the client (docs/credit.md): nothing for a non-streamed answer whose write
// failed. Budgets are charged as for any cancelled answer.
func TestClientLeavingBeforeTheEnd(t *testing.T) {
	tools := `"tools":[{"type":"function","function":{"name":"read_file"}}]`
	call := toolDelta("0", "c1", "read_file", `{"path":"README.md"}`)
	for _, c := range []struct {
		name   string
		stream bool
		chunks []string
		gone   func(p []byte) bool
		// want is the completion tokens credited, from the delivered answer's (dc)
		want func(tok *gateway.Tokenizer, dc int) int
	}{
		// Tool calls held to the end of the answer, released after the client left: only the
		// text before them reached the client.
		{"tool calls released at the end", true, []string{roleChunk, `{"choices":[{"index":0,"delta":{"content":"Let me look."},"finish_reason":null}]}`, call},
			func(p []byte) bool { return strings.Contains(string(p), "README") },
			func(tok *gateway.Tokenizer, _ int) int { return tok.Count("\n</think>\n\nLet me look.") }},
		// Everything but the final [DONE] reached the client: all but the end of turn.
		{"[DONE]", true, []string{roleChunk, call, finishTools},
			func(p []byte) bool { return strings.Contains(string(p), "[DONE]") },
			func(_ *gateway.Tokenizer, dc int) int { return dc - 1 }},
		// Nothing reached the client.
		{"not streaming", false, []string{roleChunk, call, finishTools},
			func([]byte) bool { return true },
			func(*gateway.Tokenizer, int) int { return 0 }},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := newRigCfg(t, func(cfg *gateway.Config) {
				cfg.TokenizerFile = miniTokenizer
				cfg.Keys["agent-key-2"] = gateway.KeyInfo{User: "erin", VolunteerAgents: true}
			})
			scriptNode(t, r, func(_ *websocket.Conn, write func(protocol.Msg), id string) {
				for _, ch := range c.chunks {
					write(protocol.Msg{Type: protocol.TypeChunk, ReqID: id, Data: []byte(ch)})
				}
				write(protocol.Msg{Type: protocol.TypeDone, ReqID: id, Usage: &protocol.Usage{PromptTokens: 20, CompletionTokens: 12}})
			})
			body := fmt.Sprintf(`{"model":"deep","stream":%v,"messages":[{"role":"user","content":"read the readme"}],%s}`, c.stream, tools)
			// The same answer delivered, for comparison (another key: no cache credit between them).
			if code, b := ask(t, r, "agent-key-2", body); code != 200 || !strings.Contains(b, "README") {
				t.Fatalf("delivered answer: %d %.300s", code, b)
			}
			delivered := waitUsage(t, r, 1)[0]
			cut := cutClient(t, r.g.Handler(), true, c.gone)
			req, _ := http.NewRequest("POST", cut.URL+"/v1/chat/completions", strings.NewReader(body))
			req.Header.Set("Authorization", "Bearer agent-key")
			if resp, err := http.DefaultClient.Do(req); err == nil {
				io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
			}
			u := waitUsage(t, r, 2)
			if len(u) != 2 {
				t.Fatalf("%d usage records, want 2", len(u))
			}
			got := u[1]
			if delivered["outcome"] != "ok" || got["outcome"] != "cancelled" {
				t.Fatalf("outcomes %v and %v, want ok and cancelled: %v", delivered["outcome"], got["outcome"], got)
			}
			tok, err := gateway.LoadTokenizer(miniTokenizer, "")
			if err != nil {
				t.Fatal(err)
			}
			_, _, dc := tokensOf(t, delivered)
			gp, _, gc := tokensOf(t, got)
			want := 0.0
			if wc := c.want(tok, dc); wc > 0 {
				want = float64(gp)*gateway.CreditPerPromptToken + float64(wc)*gateway.CreditPerOutputToken
			}
			if gc != c.want(tok, dc) || !near(creditOf(got), want) {
				t.Fatalf("completion %d (want %d of the delivered answer's %d), credit %v (want %v)", gc, c.want(tok, dc), dc, creditOf(got), want)
			}
		})
	}
}

// A client that left while the end of the answer was on its way, though writes to it still
// succeed (a closed connection takes them for a while): its request has ended, so the answer
// is recorded as cancelled however the gateway learns of it first. Run many times: the
// gateway sees the end of the request and the end of the answer in either order.
func TestClientGoneWhileWritesSucceed(t *testing.T) {
	r := newRigCfg(t, nil)
	rawNodeWith(t, r, "scripted", "node-token", func(write func(protocol.Msg), _ func([]byte), m protocol.Msg) {
		id := m.ReqID
		write(chunkMsg(id, "Let me look."))
		write(protocol.Msg{Type: protocol.TypeChunk, ReqID: id, Data: []byte(toolDelta("0", "c1", "read_file", `{"path":"README.md"}`))})
		write(protocol.Msg{Type: protocol.TypeDone, ReqID: id, Usage: &protocol.Usage{PromptTokens: 20, CompletionTokens: 12}})
	})
	r.waitNodes(t, 1)
	cut := cutClient(t, r.g.Handler(), false, func(p []byte) bool { return strings.Contains(string(p), "Let me look.") })
	body := `{"model":"deep","stream":true,"messages":[{"role":"user","content":"read the readme"}],"tools":[{"type":"function","function":{"name":"read_file"}}]}`
	const runs = 40
	for i := 0; i < runs; i++ {
		req, _ := http.NewRequest("POST", cut.URL+"/v1/chat/completions", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer agent-key")
		if resp, err := http.DefaultClient.Do(req); err == nil {
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}
		waitUsage(t, r, i+1)
	}
	for i, rec := range r.usage(t) {
		if rec["outcome"] != "cancelled" {
			t.Fatalf("run %d: %v", i, rec)
		}
	}
}
