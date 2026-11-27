// SPDX-License-Identifier: Apache-2.0
package gateway

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// pilotModelsRig is a stickyRig past its start-up grace, with one connector whose pilots serve the
// role "deep", and no node that ever offered it: the gateway knows "deep" only from the connector.
func pilotModelsRig(t *testing.T, mod func(*Connector)) *stickyRig {
	t.Helper()
	s := newStickyRig(t)
	s.g.start = time.Now().Add(-2 * startupGrace)
	setConnector(t, s.g, mod)
	return s
}

func setConnector(t *testing.T, g *Gateway, mod func(*Connector)) {
	t.Helper()
	c := Connector{ID: "boinc", KeySHA256: []string{keyHash(testConnectorKey)}, MaxPilots: 4, Models: []string{"deep"}}
	if mod != nil {
		mod(&c)
	}
	ct, err := newConnectorTable([]Connector{c}, nil)
	if err != nil {
		t.Fatal(err)
	}
	g.connCfgs.Store(ct)
}

// connectPilot adds a connected, registered science-only pilot serving "deep" and wakes waiters.
func (s *stickyRig) connectPilot() *nodeConn {
	n := s.node("p-000000000001", "acct-x", 0)
	s.g.mu.Lock()
	n.pilot, n.connector, n.deadline, n.scienceOnly = "p-000000000001", "boinc", time.Now().Add(time.Hour), true
	s.g.mu.Unlock()
	s.g.signal()
	return n
}

// unknownAtOnce checks that rt is answered "unknown" without waiting (the deadline is far off).
func unknownAtOnce(t *testing.T, s *stickyRig, rt *route) {
	t.Helper()
	start := time.Now()
	n, _, why := s.g.pickWait(context.Background(), rt, start.Add(time.Minute))
	if n != nil || why != unknownModel || time.Since(start) > 5*time.Second {
		t.Fatalf("node %v, why %q after %v, want unknown at once", n, why, time.Since(start))
	}
}

func sciRoute(want string) *route {
	return &route{want: want, class: ClassScience, project: "lab", exclude: map[*nodeConn]bool{}}
}

// A model no node offered recently and no connector lists is still unknown at once.
func TestPilotModelsUnknownStillRefused(t *testing.T) {
	s := pilotModelsRig(t, nil)
	unknownAtOnce(t, s, sciRoute("nope"))
	w := httptest.NewRecorder()
	s.g.unavailable(w, sciRoute("nope"), unknownModel)
	if w.Code != http.StatusNotFound || !strings.Contains(w.Body.String(), "is not served on this network") {
		t.Fatalf("HTTP %d: %s", w.Code, w.Body)
	}
}

// A science request for a role the connector's pilots serve waits for capacity, cold (no node has
// offered it since the start), and is served by the pilot that connects meanwhile. A plain node
// that offers it serves it as well.
func TestPilotModelsWaitForPilot(t *testing.T) {
	for _, pilot := range []bool{true, false} {
		name := "a pilot connects"
		if !pilot {
			name = "a node connects"
		}
		t.Run(name, func(t *testing.T) {
			s := pilotModelsRig(t, nil)
			got := make(chan *nodeConn, 1)
			go func() {
				n, _, _ := s.g.pickWait(context.Background(), sciRoute("deep"), time.Now().Add(10*time.Second))
				got <- n
			}()
			s.waitWaiters(1)
			var want *nodeConn
			if pilot {
				want = s.connectPilot()
			} else {
				want = s.node("n-00000001", "vol", 0)
				s.g.signal()
			}
			select {
			case n := <-got:
				if n != want {
					t.Fatalf("got %v, want %v", n, want)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("still waiting after the node connected")
			}
		})
	}
}

// Without a pilot or node it waits the whole queue timeout and ends with the usual timeout, which
// says no node serving it is online (503, Retry-After), not "not served on this network".
func TestPilotModelsTimeOut(t *testing.T) {
	s := pilotModelsRig(t, nil)
	start, d := time.Now(), 200*time.Millisecond
	n, _, why := s.g.pickWait(context.Background(), sciRoute("deep"), start.Add(d))
	if n != nil || why != timedOut || time.Since(start) < d {
		t.Fatalf("node %v, why %q after %v, want a timeout after %v", n, why, time.Since(start), d)
	}
	w := httptest.NewRecorder()
	s.g.unavailable(w, sciRoute("deep"), why)
	if w.Code != http.StatusServiceUnavailable || w.Header().Get("Retry-After") == "" ||
		!strings.Contains(w.Body.String(), `no node serving \"deep\" is online right now`) {
		t.Fatalf("HTTP %d: %s", w.Code, w.Body)
	}
}

// Only a connector that can bring a pilot able to serve the request makes it wait: max_pilots 0, a
// chat request while its pilots serve science only, and requests no pilot may ever take (personal
// data, own nodes only, the emergency switch, tools without consent) stay unknown at once. The table
// is read as it is now: a reload that drops the model ends the wait's grounds.
func TestPilotModelsOnlyWhenAPilotCouldServe(t *testing.T) {
	chat := func() *route { return &route{want: "deep", class: ClassChat, exclude: map[*nodeConn]bool{}} }
	t.Run("max_pilots 0", func(t *testing.T) {
		unknownAtOnce(t, pilotModelsRig(t, func(c *Connector) { c.MaxPilots = 0 }), sciRoute("deep"))
	})
	t.Run("chat, pilots science only", func(t *testing.T) {
		unknownAtOnce(t, pilotModelsRig(t, nil), chat())
	})
	t.Run("chat, pilots_chat_enabled", func(t *testing.T) {
		s := pilotModelsRig(t, func(c *Connector) { c.PilotsChatEnabled = true })
		start, d := time.Now(), 200*time.Millisecond
		if n, _, why := s.g.pickWait(context.Background(), chat(), start.Add(d)); n != nil || why != timedOut {
			t.Fatalf("node %v, why %q, want a wait until the timeout", n, why)
		}
		agent := chat()
		agent.class, agent.tools = ClassAgent, true
		unknownAtOnce(t, s, agent) // tools without consent: never on a volunteer's machine
		agent.consent = true
		if n, _, why := s.g.pickWait(context.Background(), agent, time.Now().Add(d)); n != nil || why != timedOut {
			t.Fatalf("agent with consent: node %v, why %q", n, why)
		}
		for name, mod := range map[string]func(*route){
			"personal data":    func(rt *route) { rt.pii = piiEmail },
			"own nodes only":   func(rt *route) { rt.onlySelf = true },
			"emergency switch": func(rt *route) { rt.ownOnlyMode = true },
		} {
			rt := chat()
			mod(rt)
			start := time.Now()
			if n, _, why := s.g.pickWait(context.Background(), rt, start.Add(time.Minute)); n != nil || why == timedOut || time.Since(start) > 5*time.Second {
				t.Errorf("%s: node %v, why %q after %v, want a refusal at once", name, n, why, time.Since(start))
			}
		}
	})
	t.Run("reloaded without the model", func(t *testing.T) {
		s := pilotModelsRig(t, nil)
		setConnector(t, s.g, func(c *Connector) { c.Models = []string{"other"} })
		unknownAtOnce(t, s, sciRoute("deep"))
	})
}
