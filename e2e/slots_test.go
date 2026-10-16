// A node's slot is freed exactly once on every way a request can end: never leaked (the node
// would look busy for good), never freed twice (it would take one request too many).
//
// SPDX-License-Identifier: Apache-2.0
package e2e

import (
	"bufio"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/aisathome/aisathome/gateway"
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
