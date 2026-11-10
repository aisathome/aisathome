// The node's link to the gateway when something in between goes silent (2026-11-10).
//
// SPDX-License-Identifier: Apache-2.0
package e2e

import (
	"context"
	"io"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aisathome/aisathome/gateway"
	"github.com/aisathome/aisathome/node"
	"github.com/aisathome/aisathome/protocol"
)

// freezeProxy forwards TCP connections to a backend. freeze() makes the connections open at
// that moment go silent the way the prod node's did: the node's bytes are still accepted
// (so its writes succeed) but go nowhere, nothing comes back, and the backend closing its end
// does not reach the node. Later connections are forwarded normally.
type freezeProxy struct {
	ln       net.Listener
	accepted atomic.Int32
	mu       sync.Mutex
	conns    []*atomic.Bool // one frozen flag per connection
}

func newFreezeProxy(t *testing.T, backend string) *freezeProxy {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := &freezeProxy{ln: ln}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			front, err := ln.Accept()
			if err != nil {
				return
			}
			back, err := net.Dial("tcp", backend)
			if err != nil {
				front.Close()
				continue
			}
			t.Cleanup(func() { front.Close(); back.Close() })
			frozen := new(atomic.Bool)
			p.mu.Lock()
			p.conns = append(p.conns, frozen)
			p.mu.Unlock()
			p.accepted.Add(1)
			go pipe(front, back, frozen, false) // node → gateway
			go pipe(back, front, frozen, true)  // gateway → node
		}
	}()
	return p
}

// pipe copies src to dst until src ends, dropping everything once frozen. closeDst passes
// the end on to dst, unless frozen.
func pipe(src, dst net.Conn, frozen *atomic.Bool, closeDst bool) {
	buf := make([]byte, 32<<10)
	for {
		n, err := src.Read(buf)
		if n > 0 && !frozen.Load() {
			if _, werr := dst.Write(buf[:n]); werr != nil {
				return
			}
		}
		if err != nil {
			if closeDst && !frozen.Load() {
				dst.Close()
			}
			return
		}
	}
}

func (p *freezeProxy) freeze() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, f := range p.conns {
		f.Store(true)
	}
}

// The prod node (2026-11-10) kept a connection that had gone silent for 16 hours: its
// heartbeat writes kept succeeding, the gateway had long dropped it, and nothing told the
// node. With pings, the node finds out within a ping interval and timeout, reconnects and
// serves again.
func TestNodeRecoversFromSilentLink(t *testing.T) {
	r := newRigCfg(t, func(c *gateway.Config) { c.HeartbeatTimeout = 500 * time.Millisecond })
	proxy := newFreezeProxy(t, strings.TrimPrefix(r.gw.URL, "http://"))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- node.New(node.Config{NodeID: "silent-link", Token: "node-token", Upstream: mockEngine(t, false).URL,
			GatewayURL: "ws://" + proxy.ln.Addr().String() + "/v1/node/connect", Heartbeat: 100 * time.Millisecond,
			HealthInterval: 50 * time.Millisecond, PingInterval: 100 * time.Millisecond, PingTimeout: 400 * time.Millisecond,
			Watchdog: -1, // the link itself must recover; the watchdog is the last resort
			Models:   []protocol.ModelInfo{{Name: "qwen3.8-27b-q4", Roles: []string{"deep"}, Slots: 2}}}, quiet()).Run(ctx)
	}()
	t.Cleanup(func() { cancel(); <-done })
	r.waitNodes(t, 1)
	chat := func() int {
		resp := r.do(t, "POST", "/v1/chat/completions", "user-key",
			`{"model":"qwen3.8-27b-q4","messages":[{"role":"user","content":"hi"}]}`)
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		return resp.StatusCode
	}
	if code := chat(); code != 200 {
		t.Fatalf("before the freeze: status %d", code)
	}

	proxy.freeze()
	for deadline := time.Now().Add(10 * time.Second); proxy.accepted.Load() < 2; time.Sleep(20 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("the node stayed on the silent connection (gateway sees %d nodes)", r.g.NodeCount())
		}
	}
	r.waitNodes(t, 1)
	if code := chat(); code != 200 {
		t.Fatalf("after the reconnect: status %d", code)
	}
}
