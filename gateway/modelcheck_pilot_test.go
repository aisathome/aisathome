// SPDX-License-Identifier: Apache-2.0
package gateway

import (
	"testing"
	"time"

	"github.com/aisathome/aisathome/protocol"
)

// probeRig is a gateway with the model check on a fake clock (a mean of 1 minute between probe
// prompts) and no node yet.
func probeRig(t *testing.T) (*Gateway, *time.Time) {
	t.Helper()
	entry, _ := testMCRef(t, 20)
	now := new(time.Time)
	*now = time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)
	mc := testChecker(t, ModelCheckConfig{Enabled: true, References: []ModelCheckReference{entry},
		Interval: time.Minute, Recheck: time.Minute, Now: func() time.Time { return *now }}, "")
	g := &Gateway{mc: mc, log: quietLog(), node: map[string]*nodeConn{}, waiting: map[*route]struct{}{},
		science: map[*sciRun]struct{}{}, wake: make(chan struct{})}
	return g, now
}

// probeNode adds a node (a pilot when pilot is set) that connected at since, with two slots. Its
// connection is closed, so a probe that starts fails at its send and counts as dropped: Dropped
// counts the probes started.
func probeNode(t *testing.T, g *Gateway, id, pilot string, since time.Time) *nodeConn {
	t.Helper()
	c := wsPair(t)
	c.CloseNow()
	n := &nodeConn{id: id, pilot: pilot, since: since, conn: c, pending: map[string]*pendingReq{}, closed: make(chan struct{}),
		models: []protocol.ModelInfo{{Name: "qwen3.8-27b", Slots: 2}}}
	if pilot != "" {
		n.deadline = time.Now().Add(time.Hour) // mcReserve reads the wall clock
	}
	g.node[id] = n
	return n
}

func probesStarted(g *Gateway, id string) int {
	g.mc.mu.Lock()
	defer g.mc.mu.Unlock()
	if s := g.mc.st.Nodes[id]; s != nil {
		return s.Dropped
	}
	return 0
}

// A pilot with no request but probes gets none once pilot_idle has passed since it connected or since
// its last request ended, so that it leaves by its own idle rule (30 minutes without a request); one
// with a request in flight, and every node that is no pilot, is probed as before. Probes themselves
// never count as a request.
func TestModelCheckLeavesIdlePilots(t *testing.T) {
	g, now := probeRig(t)
	t0 := *now
	idle := g.mc.pilotIdleFor() // the default, 10 minutes
	if idle != 10*time.Minute {
		t.Fatalf("default pilot idle %v", idle)
	}
	fresh := probeNode(t, g, "p-fresh", "p-fresh", t0)     // a new pilot that never gets a request
	busy := probeNode(t, g, "p-busy", "p-busy", t0)        // a pilot serving one request all along
	later := probeNode(t, g, "p-later", "p-later", t0)     // a pilot whose one request ends at 20 min
	node := probeNode(t, g, "n-1", "", t0.Add(-time.Hour)) // a node that is no pilot, never a request
	busy.realWork, busy.inflight = 1, 1

	starts := map[string][]time.Duration{}
	for at := time.Duration(0); at <= 60*time.Minute; at += 5 * time.Second {
		*now = t0.Add(at)
		if at == 15*time.Minute {
			later.mu.Lock()
			later.realWork, later.inflight = 1, 1
			later.mu.Unlock()
		}
		if at == 20*time.Minute { // its request ends, as relay.run's free records it
			later.mu.Lock()
			later.realWork, later.inflight, later.realEnd = 0, 0, *now
			later.mu.Unlock()
		}
		before := map[string]int{}
		for id := range g.node {
			before[id] = probesStarted(g, id)
		}
		g.mcTick()
		g.bg.Wait()
		for id := range g.node {
			if probesStarted(g, id) > before[id] {
				starts[id] = append(starts[id], at)
			}
		}
	}

	// The new pilot: probed while new (it may get a check before work comes), never from pilot_idle on.
	if len(starts["p-fresh"]) == 0 {
		t.Error("a new pilot got no probe in its first 10 minutes")
	}
	for _, at := range starts["p-fresh"] {
		if at >= idle {
			t.Errorf("an idle pilot was probed %v after it connected", at)
		}
	}
	// The pilot whose request ended at 20 minutes: probed during and after it, up to 30 minutes, and
	// not between 10 and 15 (idle) nor from 30 on.
	var during int
	for _, at := range starts["p-later"] {
		switch {
		case at >= idle && at < 15*time.Minute, at >= 20*time.Minute+idle:
			t.Errorf("pilot probed at %v while idle", at)
		case at >= 15*time.Minute:
			during++
		}
	}
	if during == 0 {
		t.Error("a pilot with a request (and within 10 minutes of its end) got no probe")
	}
	// The busy pilot and the node: probed all along, also in the last half hour.
	for _, id := range []string{"p-busy", "n-1"} {
		late := 0
		for _, at := range starts[id] {
			if at >= 30*time.Minute {
				late++
			}
		}
		if late < 5 { // about 30 expected (a mean of 1 minute)
			t.Errorf("%s: %d probes in the last 30 minutes", id, late)
		}
	}
	if busy.inflight != 1 || node.inflight != 0 {
		t.Errorf("slots not given back: busy %d, node %d", busy.inflight, node.inflight)
	}
	if fresh.realWork != 0 || !fresh.realEnd.IsZero() || node.realWork != 0 || !node.realEnd.IsZero() {
		t.Error("a probe counted as a request")
	}
}

// pilotIdleLocked: idle from pilot_idle after its last request ended, or after it first connected; a
// reconnect keeps what the pilot's earlier connections had (its own idle clock runs on); never for a
// node that is no pilot.
func TestPilotIdle(t *testing.T) {
	entry, _ := testMCRef(t, 20)
	t0 := time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)
	for name, c := range map[string]struct {
		n    *nodeConn
		at   time.Duration
		want bool
	}{
		"new pilot":                       {&nodeConn{id: "p", pilot: "p", since: t0}, 9 * time.Minute, false},
		"pilot never served":              {&nodeConn{id: "p", pilot: "p", since: t0}, 10 * time.Minute, true},
		"pilot, last request ended late":  {&nodeConn{id: "p", pilot: "p", since: t0, realEnd: t0.Add(5 * time.Minute)}, 14 * time.Minute, false},
		"pilot, last request ended early": {&nodeConn{id: "p", pilot: "p", since: t0, realEnd: t0.Add(5 * time.Minute)}, 15 * time.Minute, true},
		"pilot serving":                   {&nodeConn{id: "p", pilot: "p", since: t0, realWork: 1}, 5 * time.Hour, false},
		"no pilot":                        {&nodeConn{id: "n", since: t0}, 5 * time.Hour, false},
	} {
		mc := testChecker(t, ModelCheckConfig{Enabled: true, References: []ModelCheckReference{entry}}, "")
		now := t0.Add(c.at)
		if got := mc.pilotIdleLocked(c.n, c.n.lastRealWork(now), now); got != c.want {
			t.Errorf("%s: idle %v, want %v", name, got, c.want)
		}
	}

	// A lost link: the pilot was idle, and its new connection does not open a new window.
	mc := testChecker(t, ModelCheckConfig{Enabled: true, References: []ModelCheckReference{entry}}, "")
	first := &nodeConn{id: "p", pilot: "p", since: t0, realEnd: t0.Add(time.Minute)}
	mc.pilotIdleLocked(first, first.lastRealWork(t0.Add(5*time.Minute)), t0.Add(5*time.Minute))
	back := &nodeConn{id: "p", pilot: "p", since: t0.Add(12 * time.Minute)}
	if now := t0.Add(12 * time.Minute); !mc.pilotIdleLocked(back, back.lastRealWork(now), now) {
		t.Error("a reconnect opened a new probe window for an idle pilot")
	}
	back.realEnd = t0.Add(20 * time.Minute) // a request on the new connection counts
	if now := t0.Add(25 * time.Minute); mc.pilotIdleLocked(back, back.lastRealWork(now), now) {
		t.Error("idle 5 minutes after a request")
	}
}
