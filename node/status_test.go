// SPDX-License-Identifier: Apache-2.0
package node

import (
	"context"
	"encoding/json"
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

	"github.com/coder/websocket"

	"github.com/aisathome/aisathome/protocol"
)

func quietLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// engine is a minimal llama-server: /health, and a short stream on completions; hits counts
// completion requests.
func engine(t *testing.T, hits *atomic.Int32) *httptest.Server {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			return
		}
		hits.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"},\"finish_reason\":\"stop\"}]}\n\n")
		io.WriteString(w, "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":7,\"completion_tokens\":2}}\n\n")
		io.WriteString(w, "data: [DONE]\n\n")
	}))
	t.Cleanup(s.Close)
	return s
}

// fakeGateway accepts one node connection, welcomes it and hands the connection to the test.
// It knows nothing about "paused": it behaves like a gateway from before that field.
func fakeGateway(t *testing.T) (url string, conns chan *websocket.Conn) {
	conns = make(chan *websocket.Conn, 4)
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		var hello protocol.Msg
		if read(r.Context(), c, &hello) != nil || hello.Type != protocol.TypeHello {
			return
		}
		b, _ := json.Marshal(protocol.Msg{Type: protocol.TypeWelcome, GatewayID: "fake-gw", Protocol: protocol.Version})
		c.Write(r.Context(), websocket.MessageText, b)
		conns <- c
		<-r.Context().Done()
	}))
	t.Cleanup(s.Close)
	return "ws" + strings.TrimPrefix(s.URL, "http"), conns
}

func testConfig(gw, upstream, state string) Config {
	return Config{NodeID: "n1", GatewayURL: gw, Token: "tok", Upstream: upstream, StateFile: state,
		Models:    []protocol.ModelInfo{{Name: "m", Slots: 2, Ctx: 4096}},
		Heartbeat: time.Hour, HealthInterval: 20 * time.Millisecond}
}

// nextOf reads frames until one of the given type arrives.
func nextOf(t *testing.T, c *websocket.Conn, typ string) protocol.Msg {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	for {
		var m protocol.Msg
		if err := read(ctx, c, &m); err != nil {
			t.Fatalf("waiting for %s: %v", typ, err)
		}
		if m.Type == typ {
			return m
		}
	}
}

func sendTo(t *testing.T, c *websocket.Conn, m protocol.Msg) {
	t.Helper()
	b, _ := json.Marshal(m)
	if err := c.Write(context.Background(), websocket.MessageText, b); err != nil {
		t.Fatal(err)
	}
}

// A paused node refuses new requests itself, before touching the engine, so that a gateway
// that does not know "paused" retries them elsewhere; and it tells the gateway at once.
func TestPausedNodeRefusesRequests(t *testing.T) {
	var hits atomic.Int32
	gw, conns := fakeGateway(t)
	a := New(testConfig(gw, engine(t, &hits).URL, ""), quietLog())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go a.Run(ctx)
	c := <-conns

	sendTo(t, c, protocol.Msg{Type: protocol.TypeRequest, ReqID: "r1", Body: json.RawMessage(`{"model":"m","messages":[]}`)})
	if d := nextOf(t, c, protocol.TypeDone); d.ReqID != "r1" || hits.Load() != 1 {
		t.Fatalf("unpaused: done %+v, engine hits %d", d, hits.Load())
	}

	a.Pause()
	if hb := nextOf(t, c, protocol.TypeHeartbeat); !hb.Paused {
		t.Fatalf("pause must send a heartbeat with paused=true at once, got %+v", hb)
	}
	sendTo(t, c, protocol.Msg{Type: protocol.TypeRequest, ReqID: "r2", Body: json.RawMessage(`{"model":"m","messages":[]}`)})
	if e := nextOf(t, c, protocol.TypeError); e.ReqID != "r2" || e.Error != ErrPausedMsg {
		t.Fatalf("paused node must answer %q, got %+v", ErrPausedMsg, e)
	}
	if hits.Load() != 1 {
		t.Fatalf("a paused node sent a request to the engine (hits %d)", hits.Load())
	}

	a.Resume()
	if hb := nextOf(t, c, protocol.TypeHeartbeat); hb.Paused {
		t.Fatalf("resume must send a heartbeat with paused=false, got %+v", hb)
	}
	sendTo(t, c, protocol.Msg{Type: protocol.TypeRequest, ReqID: "r3", Body: json.RawMessage(`{"model":"m","messages":[]}`)})
	if d := nextOf(t, c, protocol.TypeDone); d.ReqID != "r3" || hits.Load() != 2 {
		t.Fatalf("resumed: done %+v, engine hits %d", d, hits.Load())
	}
	// The agent counts a request once its done message is sent, so the count may land a
	// moment after the gateway (here the test) has it: wait briefly (flaky in CI, 2026-10-11).
	deadline := time.Now().Add(2 * time.Second)
	st := a.Status()
	for st.Today.Requests != 2 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
		st = a.Status()
	}
	if st.Today.Requests != 2 || st.Today.PromptTokens != 14 || st.Today.CompletionTokens != 4 || st.Total.Requests != 2 {
		t.Fatalf("counters: %+v", st)
	}
}

// The pause switch, counters and events survive a restart of the agent, and a node that
// starts paused says so right after the welcome, before the gateway routes anything to it.
func TestStatePersistsAcrossRestart(t *testing.T) {
	var hits atomic.Int32
	state := filepath.Join(t.TempDir(), "node-state.json")
	gw, conns := fakeGateway(t)
	eng := engine(t, &hits)
	a := New(testConfig(gw, eng.URL, state), quietLog())
	a.served(protocol.Usage{PromptTokens: 5, CompletionTokens: 3, DurationMs: 1500})
	a.failed("engine returned 500")
	a.Pause()

	b := New(testConfig(gw, eng.URL, state), quietLog())
	st := b.Status()
	if !st.Paused || st.State != StatePaused || st.Total.Requests != 1 || st.Total.Failed != 1 || st.Total.BusyMs != 1500 ||
		st.Today.CompletionTokens != 3 {
		t.Fatalf("not restored: %+v", st)
	}
	if len(st.Events) < 2 || st.Events[0].Kind != "paused" || st.Events[1].Kind != "request_failed" {
		t.Fatalf("events not restored newest first: %+v", st.Events)
	}
	if fi, err := os.Stat(state); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("state file mode: %v %v", fi.Mode(), err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go b.Run(ctx)
	c := <-conns
	ctx2, cancel2 := context.WithTimeout(ctx, 3*time.Second)
	defer cancel2()
	var first protocol.Msg
	if err := read(ctx2, c, &first); err != nil || first.Type != protocol.TypeHeartbeat || !first.Paused {
		t.Fatalf("first frame after welcome must be a paused heartbeat, got %+v (%v)", first, err)
	}
}

// Today's counters start over on a new local date; the total does not.
func TestCountersRollOverAtMidnight(t *testing.T) {
	a := New(testConfig("ws://x", "http://x", ""), quietLog())
	day := time.Date(2026, 10, 11, 23, 59, 0, 0, time.Local)
	a.now = func() time.Time { return day }
	a.served(protocol.Usage{CompletionTokens: 10})
	day = day.Add(2 * time.Minute)
	a.served(protocol.Usage{CompletionTokens: 1})
	st := a.Status()
	if st.Day != "2026-10-12" || st.Today.Requests != 1 || st.Today.CompletionTokens != 1 || st.Total.Requests != 2 {
		t.Fatalf("rollover: %+v", st)
	}
}

// Repeated events fold into one entry, and the list is capped.
func TestEventsFoldAndCap(t *testing.T) {
	a := New(testConfig("ws://x", "http://x", ""), quietLog())
	for i := 0; i < 5; i++ {
		a.event("disconnected", "dial: connection refused")
	}
	if ev := a.Status().Events; len(ev) != 1 || ev[0].Count != 5 {
		t.Fatalf("not folded: %+v", ev)
	}
	for i := 0; i < 2*maxEvents; i++ {
		a.event("request_failed", "engine returned 5"+string(rune('0'+i%10))+string(rune('a'+i/10)))
	}
	if n := len(a.Status().Events); n != maxEvents {
		t.Fatalf("events not capped: %d", n)
	}
}

// The engine restart goes through the trigger file only, at most once a minute; without a
// trigger the agent says it cannot.
func TestRestartEngine(t *testing.T) {
	a := New(testConfig("ws://x", "http://x", ""), quietLog())
	if err := a.RestartEngine(); err != ErrRestartUnavailable {
		t.Fatalf("without trigger: %v", err)
	}
	trigger := filepath.Join(t.TempDir(), "restart-engine")
	cfg := testConfig("ws://x", "http://x", "")
	cfg.RestartTrigger = trigger
	a = New(cfg, quietLog())
	if err := a.RestartEngine(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(trigger); err != nil {
		t.Fatalf("trigger not written: %v", err)
	}
	if err := a.RestartEngine(); err != ErrRestartTooSoon {
		t.Fatalf("second restart within a minute: %v", err)
	}
	if !a.Status().CanRestart {
		t.Fatal("CanRestart false with a trigger")
	}
}
