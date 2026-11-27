// SPDX-License-Identifier: Apache-2.0
package gateway

import (
	"context"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"sync/atomic"
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

// waitQueued waits until one science request is queued: it found no node and was not refused as
// unknown, so it waits for capacity.
func waitQueued(t *testing.T, g *Gateway) {
	t.Helper()
	for end := time.Now().Add(5 * time.Second); g.queuedSc.Load() != 1; time.Sleep(5 * time.Millisecond) {
		if time.Now().After(end) {
			t.Fatal("the request never queued")
		}
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
			waitQueued(t, s.g) // past its first look and the unknown check: it really waits
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
			"only_operator":    func(rt *route) { rt.operator = "lab-gpu" },
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
		// A request that waits on the connector's models is woken by a reload that drops them and
		// refused as unknown then, not at its timeout.
		r := newPilotRig(t, func(c *Config) { c.Connectors[0].Models = []string{"deep"} })
		r.g.start = time.Now().Add(-2 * startupGrace)
		got := make(chan string, 1)
		go func() {
			_, _, why := r.g.pickWait(context.Background(), sciRoute("deep"), time.Now().Add(time.Minute))
			got <- why
		}()
		waitQueued(t, r.g)
		cfg := r.g.cfg
		cfg.Connectors = testConnectors() // no models
		if err := r.g.Reload(cfg); err != nil {
			t.Fatal(err)
		}
		select {
		case why := <-got:
			if why != unknownModel {
				t.Fatalf("why %q, want unknown", why)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("the reload left the request waiting")
		}
	})
}

// The check for a pilot that may connect later (pilotsMayServe) answers as route.allows answers for
// a pilot that has connected, through every field of route: a restriction added to allows reaches
// the wait for pilots too. Every field of route must be listed, so a new one is looked at here; its
// variants are set one at a time on top of each class, with the connector's pilots science only and
// with chat. A field with no variant does not decide who may see a request, or is decided elsewhere
// (want: the connector's models).
func TestPilotCheckMatchesAllows(t *testing.T) {
	on := new(atomic.Bool)
	on.Store(true)
	other := &nodeConn{id: "n-00000009"}
	variants := map[string][]func(*route){
		"want":             nil, // the connector's models decide (TestPilotModelsOnlyWhenAPilotCouldServe)
		"model":            {func(rt *route) { rt.model = "qwen3.8-27b" }},
		"pii":              {func(rt *route) { rt.pii = piiEmail }},
		"ownOnlyMode":      {func(rt *route) { rt.ownOnlyMode = true }},
		"ownOnlyNow":       {func(rt *route) { rt.ownOnlyNow = on }},
		"operator":         {func(rt *route) { rt.operator = "lab-gpu" }},
		"self":             {func(rt *route) { rt.self = "pseud-x" }}, // the pilot's operator: still not its own node
		"acct":             {func(rt *route) { rt.acct = "acct-x" }},
		"onlySelf":         {func(rt *route) { rt.onlySelf = true }},
		"preferSelf":       {func(rt *route) { rt.preferSelf = true }},
		"tools":            {func(rt *route) { rt.tools = true }, func(rt *route) { rt.tools, rt.consent = true, true }},
		"consent":          {func(rt *route) { rt.consent = true }},
		"need":             {func(rt *route) { rt.need = 1 << 20 }},
		"needMin":          {func(rt *route) { rt.needMin = 1 << 20 }},
		"exclude":          {func(rt *route) { rt.exclude[other] = true }}, // connections that failed it; never a new one
		"who":              {func(rt *route) { rt.who = "acct-y" }},
		"conv":             {func(rt *route) { rt.conv = []uint64{1, 2} }},
		"class":            nil, // every class is the base of each case below
		"project":          {func(rt *route) { rt.project = "lab" }},
		"share":            {func(rt *route) { rt.share = 0.5 }},
		"pilotProbationOK": {func(rt *route) { rt.pilotProbationOK = true }},
		"rules":            {func(rt *route) { rt.rules = true }},
		"minCtx":           nil, // the connector's pilot tiers decide (connector.mayClear, TestPilotTiersMayServe)
	}
	var missing []string
	rtType := reflect.TypeFor[route]()
	for i := range rtType.NumField() {
		if _, ok := variants[rtType.Field(i).Name]; !ok {
			missing = append(missing, rtType.Field(i).Name)
		}
	}
	if len(missing) > 0 {
		t.Fatalf("route fields %v are not in this test: add how each may keep a pilot from a request", missing)
	}
	fields := make([]string, 0, len(variants))
	for f := range variants {
		fields = append(fields, f)
	}
	slices.Sort(fields)
	for _, chat := range []bool{false, true} {
		g := newStickyRig(t).g
		setConnector(t, g, func(c *Connector) { c.PilotsChatEnabled = chat })
		now := time.Now()
		// A pilot of that connector as it is once connected and registered, its account past probation
		// and with the operator terms accepted.
		pilot := &nodeConn{id: "p-000000000001", pilot: "p-000000000001", connector: "boinc", operator: "pseud-x",
			account: "acct-x", deadline: now.Add(time.Hour), probEnd: now.Add(-time.Hour), scienceOnly: !chat,
			rules: nodeRules{terms: true}}
		for _, class := range []string{ClassChat, ClassAgent, ClassScience} {
			base := func() *route { return &route{want: "deep", class: class, exclude: map[*nodeConn]bool{}} }
			check := func(name string, rt *route) {
				g.mu.Lock()
				want := rt.allows(pilot, now)
				g.mu.Unlock()
				if got := g.pilotsMayServe(rt); got != want {
					t.Errorf("pilots_chat_enabled %v, %s, %s: pilotsMayServe %v, a connected pilot allowed %v", chat, class, name, got, want)
				}
			}
			check("as is", base())
			for _, f := range fields {
				for i, mod := range variants[f] {
					rt := base()
					mod(rt)
					check(f+"#"+string(rune('0'+i)), rt)
				}
			}
		}
	}
}
