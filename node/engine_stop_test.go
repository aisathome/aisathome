// SPDX-License-Identifier: Apache-2.0
package node

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/aisathome/aisathome/node/engine"
	"github.com/aisathome/aisathome/protocol"
)

// stateControl is an engine control whose process state follows Stop and Start, a moment
// later (the supervisor acts on its own goroutine).
type stateControl struct {
	mu    sync.Mutex
	state string
}

func (c *stateControl) set(s string) {
	go func() {
		time.Sleep(50 * time.Millisecond)
		c.mu.Lock()
		c.state = s
		c.mu.Unlock()
	}()
}
func (c *stateControl) Stop()    { c.set(engine.StateStopped) }
func (c *stateControl) Start()   { c.set(engine.StateRunning) }
func (c *stateControl) Restart() { c.set(engine.StateRunning) }
func (c *stateControl) Status() engine.Status {
	c.mu.Lock()
	defer c.mu.Unlock()
	return engine.Status{State: c.state}
}

// Stop engine (Windows) leaves the gateway at once, not after three failed health checks, and
// the page shows the engine as stopped, not unhealthy. Its /health may still answer for a
// moment (or another program may answer on its port): the node must not come back until the
// volunteer starts the engine again.
func TestStopEngineLeavesAtOnce(t *testing.T) {
	var hits atomic.Int32
	gw, conns := fakeGateway(t)
	cfg := testConfig(gw, testEngine(t, &hits).URL, "") // its /health always answers
	cfg.HealthInterval = time.Second                    // three failures would take 3 s
	ctl := &stateControl{state: engine.StateRunning}
	cfg.EngineControl = ctl
	a := New(cfg, quietLog())
	runAgent(t, a)
	c := <-conns
	waitFor(t, func() bool { return a.Status().Connected }, "connected")
	if err := a.StopEngine(); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	waitFor(t, func() bool { return !a.Status().Connected }, "left the gateway")
	if d := time.Since(start); d > 500*time.Millisecond {
		t.Fatalf("left the gateway after %v, want at once", d)
	}
	_ = c
	select {
	case <-conns:
		t.Fatal("reconnected while the engine is stopped")
	case <-time.After(1500 * time.Millisecond):
	}
	st := a.Status()
	if st.Engine != EngineStopped {
		t.Fatalf("engine %q, want stopped", st.Engine)
	}
	for _, e := range st.Events {
		if e.Kind == "engine_unhealthy" {
			t.Fatalf("a stopped engine was reported unhealthy: %+v (all: %+v)", e, st.Events)
		}
	}
	if err := a.StartEngine(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-conns:
	case <-time.After(5 * time.Second):
		t.Fatal("did not reconnect after Start engine")
	}
}

func waitFor(t *testing.T, ok func() bool, what string) {
	t.Helper()
	for i := 0; i < 300; i++ {
		if ok() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting: %s", what)
}

// A gateway that refuses the token (HTTP 401 before the upgrade) or the hello (a policy close)
// gives a reason; the agent shows it (the setup page needs it) until it connects.
func TestGatewayRejectionReasonIsShown(t *testing.T) {
	var mode atomic.Int32 // 0: 401, 1: policy close, 2: welcome
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if mode.Load() == 0 {
			http.Error(w, "unknown node token\x1b[31m", http.StatusUnauthorized)
			return
		}
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		var hello protocol.Msg
		if read(r.Context(), c, &hello) != nil {
			return
		}
		if mode.Load() == 1 {
			c.Close(websocket.StatusPolicyViolation, "node id is reserved")
			return
		}
		sendTo(t, c, protocol.Msg{Type: protocol.TypeWelcome, GatewayID: "gw"})
		<-r.Context().Done()
	}))
	t.Cleanup(s.Close)
	var hits atomic.Int32
	a := New(testConfig("ws"+strings.TrimPrefix(s.URL, "http"), testEngine(t, &hits).URL, ""), quietLog())
	runAgent(t, a)
	waitFor(t, func() bool { return a.Status().Rejected != "" }, "a rejection")
	if got := a.Status().Rejected; got != "HTTP 401: unknown node token [31m" {
		t.Fatalf("rejected %q", got)
	}
	mode.Store(1)
	waitFor(t, func() bool { return a.Status().Rejected == "node id is reserved" }, "the close reason")
	mode.Store(2)
	waitFor(t, func() bool { return a.Status().Connected }, "connected")
	if got := a.Status().Rejected; got != "" {
		t.Fatalf("rejected %q after connecting", got)
	}
}

// An engine the agent runs that is not running (waiting to restart: its port is held by
// another program, whose /health answers 200) must not count as healthy: the node would send
// volunteers' requests to that program (Codex review).
func TestEngineNotRunningIsNotHealthy(t *testing.T) {
	var hits atomic.Int32
	gw, conns := fakeGateway(t)
	cfg := testConfig(gw, testEngine(t, &hits).URL, "") // the other program: /health answers
	ctl := &stateControl{state: engine.StateRestarting}
	cfg.EngineControl = ctl
	a := New(cfg, quietLog())
	runAgent(t, a)
	select {
	case <-conns:
		t.Fatal("connected while the engine process is not running")
	case <-time.After(500 * time.Millisecond):
	}
	if st := a.Status(); st.Engine != EngineUnhealthy || st.Connected {
		t.Fatalf("status %+v", st)
	}
	ctl.Start()
	select {
	case <-conns:
	case <-time.After(5 * time.Second):
		t.Fatal("did not connect once the engine runs")
	}
}
