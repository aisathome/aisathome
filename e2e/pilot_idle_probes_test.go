// SPDX-License-Identifier: Apache-2.0
package e2e

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/aisathome/aisathome/gateway"
	"github.com/aisathome/aisathome/node"
	"github.com/aisathome/aisathome/protocol"
)

// The model check leaves a pilot without requests alone (gateway/modelcheck.go, pilot_idle_seconds): a
// new pilot is probed for pilot_idle after it connects, then no more, so that it can leave by its own
// idle rule; a request to it brings the probes back for pilot_idle after it ends.
func TestModelCheckLeavesAnIdlePilot(t *testing.T) {
	const idle = 600 * time.Millisecond
	entry, ref := mcReference(t)
	eng := &mcEngine{ref: ref}
	srv := httptest.NewServer(eng)
	t.Cleanup(srv.Close)
	a := newAcctRigNodes(t, 100, nil, func(c *gateway.Config) {
		c.Projects = []gateway.Project{{ID: "lab", KeySHA256: []string{sha(pilotSciKey)}, AgentOK: true, PilotProbationOK: true}}
		c.Connectors = []gateway.Connector{{ID: "boinc", KeySHA256: []string{sha(connectorKey)}, MaxPilots: 5}}
		c.QueueTimeoutSec = 2
		c.ModelCheck = &gateway.ModelCheckConfig{Enabled: true, References: []gateway.ModelCheckReference{entry},
			Tick: 10 * time.Millisecond, Interval: 20 * time.Millisecond, Recheck: 20 * time.Millisecond, PilotIdle: idle}
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	id, cred := a.newPilot(t)
	// Two slots: a probe that falls due while the request holds one is not put off (30 to 90 s).
	ag := node.New(node.Config{Token: cred, GatewayURL: "ws" + strings.TrimPrefix(a.gw.URL, "http") + "/v1/node/connect",
		Upstream: srv.URL, HealthInterval: 50 * time.Millisecond,
		Models: []protocol.ModelInfo{{Name: mcModel, Roles: []string{"deep"}, Slots: 2, Ctx: 32768}}}, quiet())
	go ag.Run(ctx)
	a.connector(t, "POST", "/pilots/"+id+"/bind", map[string]any{"user": "5", "host": "11", "team": "3"})
	waitUntil(t, "the bound pilot to connect", func() bool { return a.g.NodeCount() == 1 })
	probes := func() int { return len(eng.probeBodies()) }

	// stopsAfter waits for pilot_idle (and a probe's answer) to pass after since, then checks that no probe
	// comes for a while.
	stopsAfter := func(what string, since time.Time) {
		t.Helper()
		time.Sleep(time.Until(since.Add(idle + 300*time.Millisecond)))
		before := probes()
		time.Sleep(3 * idle / 2)
		if got := probes(); got != before {
			t.Fatalf("%s: %d probes more after pilot_idle", what, got-before)
		}
	}

	waitUntil(t, "a probe of the new pilot", func() bool { return probes() > 0 })
	stopsAfter("a new pilot without requests", time.Now()) // it connected before now
	if eng.people.Load() != 0 {
		t.Fatal("the engine got a request that is no probe")
	}

	idleCount := probes()
	if code, body := ask(t, a.rig, pilotSciKey, deepBody); code != 200 || !strings.Contains(body, "Hello") {
		t.Fatalf("science on the pilot: %d %s", code, body)
	}
	served := time.Now()
	waitUntil(t, "a probe after the request", func() bool { return probes() > idleCount })
	stopsAfter("after its request ended", served)
	if eng.people.Load() != 1 {
		t.Fatalf("the engine served %d requests that are no probe", eng.people.Load())
	}
}
