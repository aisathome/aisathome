// SPDX-License-Identifier: Apache-2.0
package node

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aisathome/aisathome/protocol"
)

// runAgent runs a until the test ends and returns Run's result (once; then closed).
func runAgent(t *testing.T, a *Agent) <-chan error {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx); close(done) }() // closed: the cleanup sees it even if the test took the result
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("the agent did not stop")
		}
	})
	return done
}

// stillRunning fails the test if Run returns within d.
func stillRunning(t *testing.T, done <-chan error, d time.Duration, what string) {
	t.Helper()
	select {
	case err := <-done:
		t.Fatalf("%s: the agent stopped: %v", what, err)
	case <-time.After(d):
	}
}

// The 2026-11-10 hang: the gateway end of the connection goes silent (it reads nothing and
// answers nothing) but the TCP connection stays open, so every heartbeat write succeeds. The
// node must notice that pings go unanswered and reconnect, instead of waiting in its read for
// the next request forever (16 hours on the prod node).
func TestSilentGatewayIsDropped(t *testing.T) {
	var hits atomic.Int32
	gw, conns := fakeGateway(t) // it never reads after the welcome: no pongs
	cfg := testConfig(gw, testEngine(t, &hits).URL, "")
	cfg.Heartbeat = 20 * time.Millisecond // the writes go through all along
	cfg.PingInterval, cfg.PingTimeout = 50*time.Millisecond, 200*time.Millisecond
	cfg.Watchdog = -1 // the connection itself must recover, without the watchdog
	a := New(cfg, quietLog())
	done := runAgent(t, a)
	<-conns
	select {
	case <-conns: // reconnected
	case err := <-done:
		t.Fatalf("the agent stopped: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("the node stayed on a gateway connection that answers nothing")
	}
	if ev := lastEvent(a, "disconnected"); !strings.Contains(ev, errNoPong.Error()) {
		t.Fatalf("the disconnect should say why (no pong), got %q", ev)
	}
}

func lastEvent(a *Agent, kind string) string {
	for _, e := range a.Status().Events { // newest first
		if e.Kind == kind {
			return e.Text
		}
	}
	return ""
}

// Defence in depth: when the link stays dead for longer than the watchdog's limit and
// nothing else recovers it (here the pings are off), Run returns a *LinkDeadError so the
// process exits non-zero and its service manager restarts it.
func TestWatchdogEndsAStuckAgent(t *testing.T) {
	var hits atomic.Int32
	gw, conns := fakeGateway(t)
	cfg := testConfig(gw, testEngine(t, &hits).URL, "")
	cfg.PingInterval = -1 // stand-in for a stuck session the pings do not catch
	cfg.Watchdog = 300 * time.Millisecond
	done := runAgent(t, New(cfg, quietLog()))
	<-conns
	select {
	case err := <-done:
		var dead *LinkDeadError
		if !errors.As(err, &dead) || dead.For < cfg.Watchdog {
			t.Fatalf("want a LinkDeadError after %v, got %v", cfg.Watchdog, err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the watchdog did not end an agent whose gateway link is dead")
	}
}

// slowEngine streams n chunks, one every delay.
func slowEngine(t *testing.T, n int, delay time.Duration) *httptest.Server {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		for i := 0; i < n; i++ {
			time.Sleep(delay)
			fmt.Fprintf(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"t%d \"},\"finish_reason\":null}]}\n\n", i)
			w.(http.Flusher).Flush()
		}
		fmt.Fprint(w, "data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n")
		fmt.Fprintf(w, "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":5,\"completion_tokens\":%d}}\n\n", n)
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	t.Cleanup(s.Close)
	return s
}

// A healthy node in the middle of a long answer receives nothing from the gateway (it only
// sends), so a message alone is no proof of life; the pongs are. The watchdog must not end
// an answer that runs for several times its limit.
func TestWatchdogSparesALongAnswer(t *testing.T) {
	gw, conns := fakeGateway(t)
	cfg := testConfig(gw, slowEngine(t, 15, 100*time.Millisecond).URL, "")
	cfg.PingInterval, cfg.PingTimeout = 50*time.Millisecond, time.Second
	cfg.Watchdog = 300 * time.Millisecond
	done := runAgent(t, New(cfg, quietLog()))
	c := <-conns
	sendTo(t, c, protocol.Msg{Type: protocol.TypeRequest, ReqID: "long", Body: json.RawMessage(`{"model":"m","messages":[]}`)})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for { // read like the gateway does: every frame, so pings get their pongs
		var m protocol.Msg
		if err := read(ctx, c, &m); err != nil {
			select {
			case err := <-done:
				t.Fatalf("the agent stopped mid-answer: %v", err)
			default:
			}
			t.Fatalf("reading the answer: %v", err)
		}
		if m.Type == protocol.TypeError {
			t.Fatalf("the answer failed: %s", m.Error)
		}
		if m.Type == protocol.TypeDone {
			break
		}
	}
	select {
	case err := <-done:
		t.Fatalf("the agent stopped: %v", err)
	default:
	}
}

// The watchdog does not count time the node stays away on purpose: while the engine is
// unhealthy (a restart of the agent would not heal it, and a model can take minutes to load)
// and while the volunteer paused it.
func TestWatchdogSparesDeliberateBreaks(t *testing.T) {
	t.Run("engine unhealthy", func(t *testing.T) {
		var healthy atomic.Bool
		eng := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !healthy.Load() {
				http.Error(w, "loading model", http.StatusServiceUnavailable)
			}
		}))
		t.Cleanup(eng.Close)
		gw, conns := fakeGateway(t)
		cfg := testConfig(gw, eng.URL, "")
		cfg.Watchdog = 200 * time.Millisecond
		done := runAgent(t, New(cfg, quietLog()))
		stillRunning(t, done, time.Second, "engine unhealthy for 5 times the watchdog's limit")
		healthy.Store(true)
		select {
		case <-conns:
		case err := <-done:
			t.Fatalf("the agent stopped: %v", err)
		case <-time.After(5 * time.Second):
			t.Fatal("no connection once the engine was healthy")
		}
	})
	t.Run("paused", func(t *testing.T) {
		var hits atomic.Int32
		gw, conns := fakeGateway(t)
		cfg := testConfig(gw, testEngine(t, &hits).URL, "")
		cfg.PingInterval = -1 // a dead link that nothing else would catch
		cfg.Watchdog = 200 * time.Millisecond
		a := New(cfg, quietLog())
		a.Pause()
		done := runAgent(t, a)
		<-conns
		stillRunning(t, done, time.Second, "paused")
		a.Resume() // the watchdog counts again, from the resume
		select {
		case err := <-done:
			var dead *LinkDeadError
			if !errors.As(err, &dead) {
				t.Fatalf("want a LinkDeadError after the resume, got %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("the watchdog did not count again after the resume")
		}
	})
}

// The default limit leaves room for a normal reconnect: the longest backoff (15 s, +20%
// jitter), a handshake that times out, and a missing pong. A limit inside that would restart
// agents that are only reconnecting.
func TestDefaultWatchdogOutlastsAReconnect(t *testing.T) {
	cfg := New(Config{}, quietLog()).cfg
	reconnect := 18*time.Second + handshakeTimeout + cfg.PingInterval + cfg.PingTimeout
	if cfg.Watchdog < 2*reconnect {
		t.Fatalf("watchdog %v is too close to a normal reconnect (%v)", cfg.Watchdog, reconnect)
	}
}
