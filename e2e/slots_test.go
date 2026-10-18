// A node's slot is freed exactly once on every way a request can end: never leaked (the node
// would look busy for good), never freed twice (it would take one request too many).
//
// SPDX-License-Identifier: Apache-2.0
package e2e

import (
	"bufio"
	"context"
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

// busySlots is the number of reserved slots on the network, from the public statistics.
func busySlots(t *testing.T, r *rig) int {
	s, raw := getStats(t, r.gw.URL)
	nodes, ok := s["nodes"].(map[string]any)
	if !ok {
		t.Fatalf("stats: %s", raw)
	}
	return int(nodes["busy"].(float64))
}

// slotsSettle waits until no slot is reserved; a negative count (a slot freed twice) fails
// at once.
func slotsSettle(t *testing.T, r *rig) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		b := busySlots(t, r)
		if b < 0 {
			t.Fatalf("%d slots reserved: a slot was freed twice", b)
		}
		if b == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d slots still reserved after the request ended", b)
		}
	}
	// And it stays there (a late second free would show now).
	time.Sleep(100 * time.Millisecond)
	if b := busySlots(t, r); b != 0 {
		t.Fatalf("%d slots reserved after settling", b)
	}
}

func TestSlotFreedOnceOnEveryExit(t *testing.T) {
	stream := `{"model":"deep","stream":true,"messages":[{"role":"user","content":"hi"}]}`
	type tc struct {
		name   string
		cfg    func(*gateway.Config)
		engine func(t *testing.T) *httptest.Server
		run    func(t *testing.T, r *rig, nodeCancel context.CancelFunc)
	}
	mock := func(t *testing.T) *httptest.Server { return mockEngine(t, false) }
	slow := func(t *testing.T) *httptest.Server { return slowEngine(t, 100, 20*time.Millisecond) }
	// readFirst reads the stream up to its first text chunk and returns the open response.
	readFirst := func(t *testing.T, r *rig) *http.Response {
		resp := r.do(t, "POST", "/v1/chat/completions", "user-key", stream)
		br := bufio.NewReader(resp.Body)
		for {
			line, err := br.ReadString('\n')
			if err != nil {
				t.Fatalf("stream ended early: %v", err)
			}
			if strings.Contains(line, `"content"`) {
				return resp
			}
		}
	}
	cases := []tc{
		{"done, streamed", nil, mock, func(t *testing.T, r *rig, _ context.CancelFunc) {
			if code, body := ask(t, r, "user-key", stream); code != 200 || !strings.Contains(body, "[DONE]") {
				t.Fatalf("%d %s", code, body)
			}
		}},
		{"done, not streamed", nil, mock, func(t *testing.T, r *rig, _ context.CancelFunc) {
			if code, body := ask(t, r, "user-key", chatBody); code != 200 {
				t.Fatalf("%d %s", code, body)
			}
		}},
		{"node error", nil, func(t *testing.T) *httptest.Server { return mockEngine(t, true) }, func(t *testing.T, r *rig, _ context.CancelFunc) {
			if code, _ := ask(t, r, "user-key", stream); code == 200 {
				t.Fatal("a failing engine answered 200")
			}
		}},
		{"client cancels mid-stream", nil, slow, func(t *testing.T, r *rig, _ context.CancelFunc) {
			readFirst(t, r).Body.Close()
		}},
		{"client gone at the end", nil, mock, func(t *testing.T, r *rig, _ context.CancelFunc) {
			h := cutClient(t, r.g.Handler(), true, func(p []byte) bool { return strings.Contains(string(p), "[DONE]") })
			req, _ := http.NewRequest("POST", h.URL+"/v1/chat/completions", strings.NewReader(stream))
			req.Header.Set("Authorization", "Bearer user-key")
			if resp, err := http.DefaultClient.Do(req); err == nil {
				io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
			}
		}},
		{"node stalls", func(c *gateway.Config) { c.FirstChunkTimeoutSec = 1 }, func(t *testing.T) *httptest.Server { return hangEngine(t) },
			func(t *testing.T, r *rig, _ context.CancelFunc) {
				if code, _ := ask(t, r, "user-key", stream); code == 200 {
					t.Fatal("a silent engine answered 200")
				}
			}},
		{"node leaves mid-stream, then comes back", nil, slow, func(t *testing.T, r *rig, nodeCancel context.CancelFunc) {
			resp := readFirst(t, r)
			nodeCancel()
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			r.waitNodes(t, 0)
			// A node that left is no longer counted; its new connection must start free.
			ctx, cancel := context.WithCancel(context.Background())
			t.Cleanup(cancel)
			r.startNode(ctx, "node-0", "node-token", mockEngine(t, false), 1)
			r.waitNodes(t, 1)
			if code, body := ask(t, r, "user-key", stream); code != 200 {
				t.Fatalf("after the node came back: %d %s", code, body)
			}
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := newRigCfg(t, c.cfg)
			ctx, cancel := context.WithCancel(context.Background())
			t.Cleanup(cancel)
			eng := c.engine(t)
			r.startNode(ctx, "node-0", "node-token", eng, 1)
			r.waitNodes(t, 1)
			c.run(t, r, cancel)
			slotsSettle(t, r)
		})
	}
}

// busyEngine is a one-slot llama-server (-np 1) that, like the real one, goes on working on a
// request for linger after its client went away. It answers /slots, and counts requests that
// reached it while it was still busy with another (they would wait behind it).
type busyEngine struct {
	*httptest.Server
	mu         sync.Mutex
	processing int
	overlaps   atomic.Int32
}

func newBusyEngine(t *testing.T, linger time.Duration) *busyEngine {
	e := &busyEngine{}
	e.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/health":
			return
		case "/slots":
			e.mu.Lock()
			busy := e.processing > 0
			e.mu.Unlock()
			fmt.Fprintf(w, `[{"id":0,"is_processing":%v}]`, busy)
			return
		}
		e.mu.Lock()
		if e.processing > 0 {
			e.overlaps.Add(1)
		}
		e.processing++
		e.mu.Unlock()
		defer func() { e.mu.Lock(); e.processing--; e.mu.Unlock() }()
		w.Header().Set("Content-Type", "text/event-stream")
		for i := 0; i < 50; i++ {
			select {
			case <-r.Context().Done():
				time.Sleep(linger) // the engine notices the closed connection only later
				return
			case <-time.After(20 * time.Millisecond):
			}
			fmt.Fprintf(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"t%d \"},\"finish_reason\":null}]}\n\n", i)
			w.(http.Flusher).Flush()
		}
		fmt.Fprint(w, "data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	t.Cleanup(e.Close)
	return e
}

// readFirstChunk starts a streamed request and reads it up to its first text chunk.
func readFirstChunk(t *testing.T, r *rig, body string) *http.Response {
	t.Helper()
	resp := r.do(t, "POST", "/v1/chat/completions", "user-key", body)
	br := bufio.NewReader(resp.Body)
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			t.Fatalf("stream ended early: %v", err)
		}
		if strings.Contains(line, `"content"`) {
			return resp
		}
	}
}

// A cancelled request keeps its node's slot until the node's engine has let go of it: the
// next request goes to another free node, or waits, but never queues behind the cancelled one
// in the engine (TauCeti review test, 2026-10-15: 502s after the 300 s first-chunk timeout).
func TestCancelledSlotWaitsForTheEngine(t *testing.T) {
	stream := `{"model":"deep","stream":true,"messages":[{"role":"user","content":"hi"}]}`
	const linger = 1500 * time.Millisecond
	for _, other := range []bool{false, true} {
		name := map[bool]string{false: "one node: waits", true: "two nodes: goes to the free one"}[other]
		t.Run(name, func(t *testing.T) {
			r := newRigCfg(t, nil)
			ctx, cancel := context.WithCancel(context.Background())
			t.Cleanup(cancel)
			eng := newBusyEngine(t, linger)
			r.startNode(ctx, "node-0", "node-token", eng.Server, 1)
			r.waitNodes(t, 1)
			a := readFirstChunk(t, r, stream) // on node-0, the only node
			if other {
				r.startNode(ctx, "node-1", "node-token", mockEngine(t, false), 1)
				r.waitNodes(t, 2)
			}
			a.Body.Close() // the client gives up; node-0's engine goes on for linger
			cancelled := time.Now()
			time.Sleep(200 * time.Millisecond) // the gateway has seen the cancel and told the node
			if b := busySlots(t, r); b != 1 {
				t.Errorf("%d slots reserved while node-0's engine still works on the cancelled request, want 1", b)
			}
			code, body := ask(t, r, "user-key", stream)
			if code != 200 || !strings.Contains(body, "[DONE]") {
				t.Fatalf("next request: %d %.300s", code, body)
			}
			if n := eng.overlaps.Load(); n != 0 {
				t.Fatalf("%d requests were sent to node-0 while its engine was busy with the cancelled one", n)
			}
			took := time.Since(cancelled)
			if other && took > linger/2 {
				t.Fatalf("the next request took %v though node-1 was free", took)
			}
			if !other && took < linger {
				t.Fatalf("the next request finished %v after the cancel, before the engine was free", took)
			}
			slotsSettle(t, r)
		})
	}
}

// A node that never ends a cancelled request (an older or broken one) does not hold its slot
// for good: it is freed after cancel_ack_timeout_seconds, with a warning.
func TestCancelledSlotFreedForSilentNode(t *testing.T) {
	logs := &syncBuf{}
	logSink = logs
	defer func() { logSink = io.Discard }()
	r := newRigCfg(t, func(c *gateway.Config) { c.CancelAckTimeoutSec = 1 })
	cancels := scriptNode(t, r, func(_ *websocket.Conn, write func(protocol.Msg), id string) {
		write(chunkMsg(id, "Hello")) // and nothing more, not even after the cancel
	})
	readFirstChunk(t, r, `{"model":"deep","stream":true,"messages":[{"role":"user","content":"hi"}]}`).Body.Close()
	wantCancel(t, cancels)
	time.Sleep(300 * time.Millisecond)
	if b := busySlots(t, r); b != 1 {
		t.Fatalf("%d slots reserved before the node ended the cancelled request, want 1", b)
	}
	slotsSettle(t, r)
	if !strings.Contains(logs.String(), "node did not end a cancelled request") {
		t.Fatal("no warning for the slot freed without the node's confirmation")
	}
}
