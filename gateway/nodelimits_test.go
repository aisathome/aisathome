// SPDX-License-Identifier: Apache-2.0
package gateway

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aisathome/aisathome/protocol"
	"github.com/coder/websocket"
)

// A node announces at most maxSlots slots per model; the reference node announces one.
func TestHelloSlotsBounded(t *testing.T) {
	hello := func(slots int) protocol.Msg {
		return protocol.Msg{NodeID: "x", Models: []protocol.ModelInfo{{Name: "m", Slots: slots}}}
	}
	for _, c := range []struct {
		slots int
		ok    bool
	}{{0, false}, {1, true}, {2, true}, {8, true}, {9, false}, {64, false}} {
		if got := validHello(hello(c.slots)) == ""; got != c.ok {
			t.Errorf("%d slots: accepted %v, want %v", c.slots, got, c.ok)
		}
	}
}

// Each frame type has its limit, and a queued message keeps only what its relay reads.
func TestFrameLimitsByType(t *testing.T) {
	if frameLimit(protocol.TypeChunk) != maxChunkBytes+4<<10 || frameLimit(protocol.TypeDone) != maxControlFrame ||
		frameLimit(protocol.TypeHeartbeat) != maxControlFrame || frameLimit("other") != maxControlFrame ||
		frameLimit(protocol.TypeError) != maxErrorFrame || maxFrame < maxErrorFrame || maxFrame < maxHelloFrame {
		t.Fatal("frame limits")
	}
	big := strings.Repeat("x", 10<<10)
	m := queuedMsg(protocol.Msg{Type: protocol.TypeChunk, ReqID: "r", Data: json.RawMessage(`{}`), Error: big, Body: json.RawMessage(`"` + big + `"`),
		Models: []protocol.ModelInfo{{Name: big}}, Agent: big})
	if m.ReqID != "r" || string(m.Data) != `{}` || m.Error != "" || m.Body != nil || m.Models != nil || m.Agent != "" {
		t.Fatalf("chunk kept more than its data: %+v", m)
	}
	if m := queuedMsg(protocol.Msg{Type: protocol.TypeError, ReqID: "r", Error: big, Data: json.RawMessage(`"` + big + `"`)}); len(m.Error) != maxErrorText || m.Data != nil {
		t.Fatalf("error: %d bytes of text, data %d", len(m.Error), len(m.Data))
	}
	u := &protocol.Usage{PromptTokens: 3}
	if m := queuedMsg(protocol.Msg{Type: protocol.TypeDone, ReqID: "r", Usage: u, Error: big}); m.Usage != u || m.Error != "" {
		t.Fatalf("done: %+v", m)
	}
}

// What request queues hold counts against the request's, the node's and the gateway's budget. A
// message that would exceed one waits while the queues drain; when nothing drains for
// deliverGrace it is not queued and the request is given up on, with the budget as the reason.
// The budgets get back what the relay takes, and all of a request's share once it is finished.
func TestQueueBudgets(t *testing.T) {
	var nodeQ, totalQ atomic.Int64
	newReq := func() *pendingReq {
		p := newPendingReq()
		p.nodeQ, p.totalQ = &nodeQ, &totalQ
		return p
	}
	chunk := protocol.Msg{Type: protocol.TypeChunk, Data: json.RawMessage(`"` + strings.Repeat("x", 60<<10) + `"`)}
	size := queuedSize(chunk)
	slow := func(p *pendingReq) bool {
		select {
		case <-p.slow:
			return true
		default:
			return false
		}
	}

	// One request whose relay takes nothing: its own budget.
	p := newReq()
	over, sent := "", 0
	start := time.Now()
	for ; over == "" && sent < 64; sent++ {
		over = p.deliver(chunk)
	}
	if over != whyClientSlow || p.why != whyClientSlow || !slow(p) || int64(len(p.ch))*size > maxQueuedPerReq ||
		nodeQ.Load() != int64(len(p.ch))*size || totalQ.Load() != nodeQ.Load() {
		t.Fatalf("over %q after %d, slow %v, queued %d (node %d, total %d)", over, sent, slow(p), len(p.ch), nodeQ.Load(), totalQ.Load())
	}
	if d := time.Since(start); d < deliverGrace || d > maxDeliverWait {
		t.Fatalf("given up after %v: not after deliverGrace without draining", d)
	}
	if p.deliver(chunk) != "" || nodeQ.Load() != int64(len(p.ch))*size {
		t.Fatal("a request given up on still queues")
	}
	p.took(<-p.ch)
	if nodeQ.Load() != int64(len(p.ch))*size {
		t.Fatalf("taking a message did not free it: %d", nodeQ.Load())
	}
	close(p.done)
	p.finish()
	if nodeQ.Load() != 0 || totalQ.Load() != 0 {
		t.Fatalf("finished request still counts: node %d, total %d", nodeQ.Load(), totalQ.Load())
	}
	if p.deliver(chunk); nodeQ.Load() != 0 {
		t.Fatal("a finished request counts again")
	}

	// Several requests of one node whose relays take nothing: the node's budget.
	var reqs []*pendingReq
	over = ""
	for i := 0; over == "" && i < 16; i++ {
		r := newReq()
		reqs = append(reqs, r)
		for j := 0; j < 15 && over == ""; j++ { // 15 × 60 KiB: under one request's budget
			over = r.deliver(chunk)
		}
	}
	if last := reqs[len(reqs)-1]; over != whyNodeQueue || last.why != whyNodeQueue || nodeQ.Load() > maxQueuedPerNode || !slow(last) || slow(reqs[0]) {
		t.Fatalf("node budget: over %q with %d requests, node %d", over, len(reqs), nodeQ.Load())
	}
	for _, r := range reqs {
		close(r.done)
		r.finish()
	}
	if nodeQ.Load() != 0 || totalQ.Load() != 0 {
		t.Fatalf("node %d, total %d", nodeQ.Load(), totalQ.Load())
	}

	// The gateway's budget, shared by all nodes.
	totalQ.Store(maxQueuedTotal - size/2)
	if p := newReq(); p.deliver(chunk) != whyGatewayQ || p.why != whyGatewayQ || !slow(p) || nodeQ.Load() != 0 {
		t.Fatal("gateway budget")
	}
	totalQ.Store(0)

	// A message that cannot be queued after all (the request ended meanwhile) gives its share back.
	p = newReq()
	for len(p.ch) < cap(p.ch) {
		p.ch <- protocol.Msg{}
	}
	close(p.done)
	if p.deliver(protocol.Msg{Type: protocol.TypeChunk}) != "" || nodeQ.Load() != 0 {
		t.Fatalf("unqueued message still counts: %d", nodeQ.Load())
	}
}

// A fast engine's burst of large chunks (a long tool-call argument, logprobs) to a client that
// keeps up goes through whole: the request's budget fills, but its relay drains it, so nothing is
// given up on.
func TestQueueBudgetBurstThatDrains(t *testing.T) {
	var nodeQ, totalQ atomic.Int64
	p := newPendingReq()
	p.nodeQ, p.totalQ = &nodeQ, &totalQ
	chunk := protocol.Msg{Type: protocol.TypeChunk, Data: json.RawMessage(`"` + strings.Repeat("x", 60<<10) + `"`)}
	const burst = 200 // 12 MiB, far over the request's budget
	got := make(chan int)
	go func() { // a relay whose client keeps up, but more slowly than the node sends
		n := 0
		for n < burst {
			m := <-p.ch
			p.took(m)
			n++
			time.Sleep(time.Millisecond)
		}
		got <- n
	}()
	for i := 0; i < burst; i++ {
		if over := p.deliver(chunk); over != "" {
			t.Fatalf("chunk %d: given up on (%s) while the relay drains", i, over)
		}
	}
	if n := <-got; n != burst || nodeQ.Load() != 0 {
		t.Fatalf("delivered %d of %d, %d bytes still counted", n, burst, nodeQ.Load())
	}
}

// Readers of different nodes reserve from the gateway's budget at once: together they never
// exceed it.
func TestGatewayBudgetIsAtomic(t *testing.T) {
	var totalQ, peak atomic.Int64
	const size = maxQueuedTotal / 8
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 2000; j++ {
				if !reserve(&totalQ, size, maxQueuedTotal) {
					continue
				}
				for v := totalQ.Load(); v > peak.Load(); v = totalQ.Load() {
					peak.CompareAndSwap(peak.Load(), v)
				}
				totalQ.Add(-size)
			}
		}()
	}
	wg.Wait()
	if peak.Load() > maxQueuedTotal || totalQ.Load() != 0 {
		t.Fatalf("peak %d over the budget %d (left %d)", peak.Load(), int64(maxQueuedTotal), totalQ.Load())
	}
}

// rawNode connects a hand-written node to srv and waits until the gateway registered it.
func rawNode(t *testing.T, g *Gateway, srv *httptest.Server, id string) (c *websocket.Conn, write func(protocol.Msg), n *nodeConn) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	h := http.Header{}
	h.Set("Authorization", "Bearer node-token")
	c, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http")+"/v1/node/connect", &websocket.DialOptions{HTTPHeader: h})
	if err != nil {
		t.Fatal(err)
	}
	c.SetReadLimit(1 << 20)
	write = func(m protocol.Msg) { b, _ := json.Marshal(m); c.Write(ctx, websocket.MessageText, b) }
	write(protocol.Msg{Type: protocol.TypeHello, NodeID: id, Protocol: protocol.LegacyVersion,
		Models: []protocol.ModelInfo{{Name: "qwen3.8-27b-q4", Roles: []string{"deep"}, Slots: 2}}})
	go func() { // read what the gateway sends, so that its writes and close handshakes complete
		for {
			if _, _, err := c.Read(ctx); err != nil {
				return
			}
		}
	}()
	for deadline := time.Now().Add(5 * time.Second); n == nil; time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("node did not connect")
		}
		g.mu.Lock()
		n = g.node[id]
		g.mu.Unlock()
	}
	return c, write, n
}

func limitsGateway(t *testing.T) (*Gateway, *httptest.Server) {
	g := newTestGateway(t, Config{NodeTokens: map[string]NodeToken{"node-token": {Operator: "op1"}}})
	srv := httptest.NewServer(g.Handler())
	t.Cleanup(func() { srv.Close(); g.Close() })
	return g, srv
}

// waitGone waits until node n's connection ended.
func waitGone(t *testing.T, n *nodeConn, what string) {
	t.Helper()
	select {
	case <-n.closed:
	case <-time.After(5 * time.Second):
		t.Fatalf("%s: the node is still connected", what)
	}
}

// A node that floods a request whose client does not read (its relay takes nothing) fills that
// request's budget and no more: the request is given up on, and the node's other messages are
// still read (its reader never waits on the stalled request).
func TestNodeFloodingAStalledRequest(t *testing.T) {
	g, srv := limitsGateway(t)
	_, write, n := rawNode(t, g, srv, "flood")
	p := g.newPending(n) // a request whose relay never takes anything: its client stalled
	n.mu.Lock()
	n.pending["r1"] = p
	n.mu.Unlock()
	data := json.RawMessage(`{"choices":[{"index":0,"delta":{"content":"` + strings.Repeat("x", 60<<10) + `"}}]}`)
	for i := 0; i < 200; i++ {
		write(protocol.Msg{Type: protocol.TypeChunk, ReqID: "r1", Data: data})
	}
	write(protocol.Msg{Type: protocol.TypeHeartbeat, Active: 7})
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(5 * time.Millisecond) {
		n.mu.Lock()
		active := n.active
		n.mu.Unlock()
		if active == 7 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the node's heartbeat after the flood was not read")
		}
	}
	select {
	case <-p.slow:
	default:
		t.Fatal("the stalled request was not given up on")
	}
	if q := n.queued.Load(); q > maxQueuedPerReq || q != int64(len(p.ch))*queuedSize(protocol.Msg{Data: data}) || g.qbytes.Load() != q {
		t.Fatalf("queued: node %d, gateway %d, %d messages", q, g.qbytes.Load(), len(p.ch))
	}
	close(p.done)
	p.finish()
	if n.queued.Load() != 0 || g.qbytes.Load() != 0 {
		t.Fatalf("after the request: node %d, gateway %d", n.queued.Load(), g.qbytes.Load())
	}
}

// A frame larger than its type allows ends the node's connection before anything is queued; an
// error frame within its limit is kept with its text cut to maxErrorText.
func TestNodeOversizedFrames(t *testing.T) {
	pad := func(n int) string { return strings.Repeat("x", n) }
	small := json.RawMessage(`{"choices":[]}`)
	for _, c := range []struct {
		name string
		m    protocol.Msg
	}{
		{"chunk data", protocol.Msg{Type: protocol.TypeChunk, ReqID: "r1", Data: json.RawMessage(`"` + pad(maxChunkBytes) + `"`)}},
		{"chunk beyond the read limit", protocol.Msg{Type: protocol.TypeChunk, ReqID: "r1", Data: small, Error: pad(maxFrame)}},
		{"chunk frame", protocol.Msg{Type: protocol.TypeChunk, ReqID: "r1", Data: json.RawMessage(`"` + pad(60<<10) + `"`), Error: pad(10 << 10)}},
		{"done", protocol.Msg{Type: protocol.TypeDone, ReqID: "r1", Error: pad(maxControlFrame)}},
		{"heartbeat", protocol.Msg{Type: protocol.TypeHeartbeat, Body: json.RawMessage(`"` + pad(maxControlFrame) + `"`)}},
		{"unknown type", protocol.Msg{Type: "status", Error: pad(maxControlFrame)}},
		{"error", protocol.Msg{Type: protocol.TypeError, ReqID: "r1", Error: pad(maxErrorFrame)}},
	} {
		t.Run(c.name, func(t *testing.T) {
			g, srv := limitsGateway(t)
			_, write, n := rawNode(t, g, srv, "big")
			p := g.newPending(n)
			n.mu.Lock()
			n.pending["r1"] = p
			n.mu.Unlock()
			write(c.m)
			waitGone(t, n, c.name)
			if len(p.ch) != 0 || n.queued.Load() != 0 {
				t.Fatalf("%d messages queued", len(p.ch))
			}
		})
	}

	g, srv := limitsGateway(t)
	_, write, n := rawNode(t, g, srv, "err")
	p := g.newPending(n)
	n.mu.Lock()
	n.pending["r1"] = p
	n.mu.Unlock()
	write(protocol.Msg{Type: protocol.TypeError, ReqID: "r1", Error: "engine returned 500: " + pad(10<<10)})
	select {
	case m := <-p.ch:
		if len(m.Error) != maxErrorText || !strings.HasPrefix(m.Error, "engine returned 500") {
			t.Fatalf("error text: %d bytes", len(m.Error))
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the error was not delivered")
	}

	// A hello beyond its limit is refused.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	h := http.Header{}
	h.Set("Authorization", "Bearer node-token")
	c, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http")+"/v1/node/connect", &websocket.DialOptions{HTTPHeader: h})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(protocol.Msg{Type: protocol.TypeHello, NodeID: "huge", Protocol: protocol.LegacyVersion, Agent: pad(maxHelloFrame),
		Models: []protocol.ModelInfo{{Name: "qwen3.8-27b-q4", Slots: 1}}})
	c.Write(ctx, websocket.MessageText, b)
	if _, _, err := c.Read(ctx); websocket.CloseStatus(err) != websocket.StatusPolicyViolation {
		t.Fatalf("hello beyond its limit: %v", err)
	}
}

// An answer may hold at most its byte budget: by its chunks, more with logprobs, never more than
// maxStreamed, also for a max_tokens so large that the multiplication would overflow.
func TestStreamBudget(t *testing.T) {
	for _, c := range []struct {
		chunks   int
		logprobs bool
		want     int
	}{
		{258, false, streamBase + 258<<10},
		{258, true, streamBase + 258<<13},
		{2*16384 + 256, false, streamBase + (2*16384+256)<<10},
		{1 << 40, false, maxStreamed},
		{-5, false, maxStreamed},
	} {
		if got := streamBudget(c.chunks, c.logprobs); got != c.want {
			t.Errorf("%d chunks, logprobs %v: %d, want %d", c.chunks, c.logprobs, got, c.want)
		}
	}
}

// A request given up on because the gateway's or its node's queue budget stayed full ends its
// stream with an error event that says so, not as a slow client and not cut off silently.
func TestQueueBudgetEndsStreamWithError(t *testing.T) {
	for _, c := range []struct {
		name string
		fill func(g *Gateway, n *nodeConn)
		want string
	}{
		{"gateway", func(g *Gateway, _ *nodeConn) { g.qbytes.Store(maxQueuedTotal) }, "the gateway is busy; try again"},
		{"node", func(_ *Gateway, n *nodeConn) { n.queued.Store(maxQueuedPerNode) }, "the node sent more than the gateway can hold for it"},
	} {
		t.Run(c.name, func(t *testing.T) {
			g := newTestGateway(t, Config{Keys: map[string]KeyInfo{"user-key": {User: "alice"}},
				NodeTokens: map[string]NodeToken{"node-token": {Operator: "op1", OperatorTerms: OperatorTermsVersion}}})
			srv := httptest.NewServer(g.Handler())
			t.Cleanup(func() { srv.Close(); g.Close() })
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			h := http.Header{}
			h.Set("Authorization", "Bearer node-token")
			ws, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http")+"/v1/node/connect", &websocket.DialOptions{HTTPHeader: h})
			if err != nil {
				t.Fatal(err)
			}
			write := func(m protocol.Msg) { b, _ := json.Marshal(m); ws.Write(ctx, websocket.MessageText, b) }
			write(protocol.Msg{Type: protocol.TypeHello, NodeID: "q", Protocol: protocol.LegacyVersion,
				Models: []protocol.ModelInfo{{Name: "qwen3.8-27b-q4", Roles: []string{"deep"}, Slots: 2}}})
			reqID := make(chan string, 1)
			go func() {
				for {
					_, b, err := ws.Read(ctx)
					if err != nil {
						return
					}
					var m protocol.Msg
					json.Unmarshal(b, &m)
					if m.Type == protocol.TypeRequest {
						reqID <- m.ReqID
					}
				}
			}()
			var n *nodeConn
			for deadline := time.Now().Add(5 * time.Second); n == nil; time.Sleep(5 * time.Millisecond) {
				if time.Now().After(deadline) {
					t.Fatal("node did not connect")
				}
				g.mu.Lock()
				n = g.node["q"]
				g.mu.Unlock()
			}
			req, _ := http.NewRequest("POST", srv.URL+"/v1/chat/completions", strings.NewReader(`{"model":"deep","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
			req.Header.Set("Authorization", "Bearer user-key")
			done := make(chan string, 1)
			go func() {
				resp, err := http.DefaultClient.Do(req)
				if err != nil {
					done <- err.Error()
					return
				}
				defer resp.Body.Close()
				b, _ := io.ReadAll(resp.Body)
				done <- string(b)
			}()
			id := <-reqID
			write(protocol.Msg{Type: protocol.TypeChunk, ReqID: id, Data: json.RawMessage(`{"choices":[{"index":0,"delta":{"content":"first"}}]}`)})
			for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(5 * time.Millisecond) { // the answer started
				n.mu.Lock()
				p := n.pending[id]
				n.mu.Unlock()
				if p != nil && len(p.ch) == 0 && n.queued.Load() == 0 {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("first chunk not taken")
				}
			}
			time.Sleep(50 * time.Millisecond)
			c.fill(g, n)
			write(protocol.Msg{Type: protocol.TypeChunk, ReqID: id, Data: json.RawMessage(`{"choices":[{"index":0,"delta":{"content":"second"}}]}`)})
			select {
			case out := <-done:
				if !strings.Contains(out, "first") || strings.Contains(out, "second") || !strings.Contains(out, `"error":{"message":"`+c.want+`"}`) {
					t.Fatalf("stream: %s", out)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("the stream did not end")
			}
		})
	}
}
