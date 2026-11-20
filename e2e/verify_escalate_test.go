// Escalation of failed answer checks (BACKLOG 11e; gateway/verify_escalate.go, account_holds.go):
// repeated failures hold the node, repeated node holds hold its account; only in enforce mode, never
// for trusted nodes, and for standalone community nodes and BOINC pilots alike.
//
// SPDX-License-Identifier: Apache-2.0
package e2e

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/aisathome/aisathome/gateway"
	"github.com/aisathome/aisathome/protocol"
)

// padAll answers every request at once with 2,000 chunks: far past any speed limit.
func padAll(write func(protocol.Msg), _ func([]byte), m protocol.Msg) {
	for i := 0; i < 2000; i++ {
		write(deltaChunk(m.ReqID, map[string]any{"content": "hello hello "}, ""))
	}
	write(deltaChunk(m.ReqID, map[string]any{}, "stop"))
	write(protocol.Msg{Type: protocol.TypeDone, ReqID: m.ReqID, Usage: &protocol.Usage{CompletionTokens: 2000}})
}

func heldNodes(t *testing.T, url string) int {
	t.Helper()
	resp, err := http.Get(url + "/v1/stats")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var st struct {
		Nodes map[string]int `json:"nodes"`
	}
	json.NewDecoder(resp.Body).Decode(&st)
	return st.Nodes["held"]
}

// Two failed answers (node_after 2) hold an untrusted config-token node in enforce mode: it then
// gets no requests. The first failure alone does not.
func TestEscalationHoldsNode(t *testing.T) {
	r := newRigCfg(t, func(c *gateway.Config) {
		c.TokenizerFile = miniTokenizer
		c.QueueTimeoutSec = 1
		c.Verify = gateway.VerifyConfig{Mode: gateway.VerifyEnforce, Escalate: gateway.EscalateConfig{NodeAfter: 2}}
	})
	rawNodeWith(t, r, "pad-node", "node-token", padAll)
	r.waitNodes(t, 1)
	ask(t, r, "user-key", chatBody)
	if h := heldNodes(t, r.gw.URL); h != 0 {
		t.Fatalf("held after one failure: %d", h)
	}
	ask(t, r, "user-key", chatBody)
	waitUntil(t, "the node to be held", func() bool { return heldNodes(t, r.gw.URL) == 1 })
	if code, _ := ask(t, r, "user-key", chatBody); code != 503 {
		t.Fatalf("a held node still served: HTTP %d", code)
	}
}

// Observe mode never holds; a trusted node is never held.
func TestEscalationObserveAndTrusted(t *testing.T) {
	for _, c := range []struct {
		name, mode string
		trust      string
	}{{"observe", gateway.VerifyObserve, ""}, {"trusted", gateway.VerifyEnforce, "trusted"}} {
		t.Run(c.name, func(t *testing.T) {
			r := newRigCfg(t, func(cfg *gateway.Config) {
				cfg.TokenizerFile = miniTokenizer
				cfg.NodeTokens["node-token"] = gateway.NodeToken{Operator: "op1", Trust: c.trust}
				cfg.Verify = gateway.VerifyConfig{Mode: c.mode, Escalate: gateway.EscalateConfig{NodeAfter: 1}}
			})
			rawNodeWith(t, r, "pad-node", "node-token", padAll)
			r.waitNodes(t, 1)
			for i := 0; i < 3; i++ {
				if code, _ := ask(t, r, "user-key", chatBody); code != 200 {
					t.Fatalf("answer %d: HTTP %d", i, code)
				}
			}
			if h := heldNodes(t, r.gw.URL); h != 0 {
				t.Fatalf("held: %d", h)
			}
		})
	}
}

// A standalone community node (an account's node token): its hold names the account; with
// account_after 1 the account is held too, so the account's other node is held as it connects;
// account-clear and node-clear let both serve again.
func TestEscalationHoldsAccountOfCommunityNode(t *testing.T) {
	a := newAcctRigNodes(t, 100, nil, func(c *gateway.Config) {
		c.TokenizerFile = miniTokenizer
		c.QueueTimeoutSec = 1
		c.Verify = gateway.VerifyConfig{Mode: gateway.VerifyEnforce, Escalate: gateway.EscalateConfig{NodeAfter: 1, AccountAfter: 1}}
	})
	session, token, id := a.operator(t, 7)
	token2, id2, _ := a.addNode(t, session, "second")
	rawNodeWith(t, a.rig, id, token, padAll)
	a.waitNodes(t, 1)
	ask(t, a.rig, "user-key", chatBody)
	waitUntil(t, "the account to be held", func() bool { a.store.Refresh(); return len(a.store.AccountHeldList()) == 1 })
	h := a.store.AccountHeldList()[0]
	if h.Account != "gh:7" || h.Rule != "answer-checks" || !strings.Contains(h.Reason, "1 of its nodes") {
		t.Fatalf("account hold %+v", h)
	}
	var hold gateway.NodeHold
	for _, p := range a.store.Snapshot().NodeHolds {
		if p.Node == id {
			hold = p
		}
	}
	if hold.Rule != "answer-checks" || hold.Account != "gh:7" || !strings.Contains(hold.Reason, "failed the answer checks") {
		t.Fatalf("node hold %+v", hold)
	}
	rawNodeWith(t, a.rig, id2, token2, padAll) // the account's other node: held as it connects
	a.waitNodes(t, 2)
	if h := heldNodes(t, a.gw.URL); h != 2 {
		t.Fatalf("the held account's node connected unheld (%d held)", h)
	}
	time.Sleep(2500 * time.Millisecond) // past a sweep of the nodes: it stays held
	if h := heldNodes(t, a.gw.URL); h != 2 {
		t.Fatalf("a sweep let the held account's node serve (%d held)", h)
	}
	if err := a.store.ClearAccountHold("gh:7"); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, "the second node to serve again", func() bool { return heldNodes(t, a.gw.URL) == 1 })
	if err := a.store.ClearNode(id); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, "both to serve again", func() bool { return heldNodes(t, a.gw.URL) == 0 })
}

// BOINC pilots: each pilot is a node of its own. The first pilot's failure holds it and withholds
// its credit; the same user's second held pilot holds the account, and the user's next pilot is
// refused with how to reach us.
func TestEscalationHoldsAccountOfPilots(t *testing.T) {
	a := newAcctRigNodes(t, 100, nil, func(c *gateway.Config) {
		c.Projects = []gateway.Project{{ID: "lab", KeySHA256: []string{sha(pilotSciKey)}, AgentOK: true, PilotProbationOK: true}}
		c.Connectors = []gateway.Connector{{ID: "boinc", KeySHA256: []string{sha(connectorKey)}, MaxPilots: 5}}
		c.QueueTimeoutSec = 2
		// mockEngine writes 3 tokens at once: past a burst of 1 at a crawl of a rate.
		c.Verify = gateway.VerifyConfig{Mode: gateway.VerifyEnforce, SpeedBurst: 1, SpeedRate: 0.001,
			Escalate: gateway.EscalateConfig{NodeAfter: 1, AccountAfter: 2}}
	})
	engine := mockEngine(t, false)
	t.Cleanup(engine.Close)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var stop context.CancelFunc
	run := func() string {
		t.Helper()
		id, cred := a.newPilot(t)
		var pctx context.Context
		pctx, stop = context.WithCancel(ctx)
		t.Cleanup(stop)
		a.runPilot(pctx, cred, engine.URL)
		a.connector(t, "POST", "/pilots/"+id+"/bind", map[string]any{"user": "5", "host": "11"})
		return id
	}
	// A pilot of the same user that served nothing and went away (a suspended task): its credit is
	// not final, so the account hold withholds it too.
	p0 := run()
	waitUntil(t, "pilot 0 to connect", func() bool { return a.g.NodeCount() == 1 })
	stop()
	waitUntil(t, "pilot 0 to leave", func() bool { return a.g.NodeCount() == 0 })
	p1 := run()
	waitUntil(t, "pilot 1 to connect", func() bool { return a.g.NodeCount() == 1 })
	ask(t, a.rig, pilotSciKey, deepBody)
	waitUntil(t, "pilot 1 held", func() bool { return a.pilotRecord(t, p1)["withheld"] == "answer checks" })
	if a.store.Refresh(); len(a.store.AccountHeldList()) != 0 {
		t.Fatal("the account was held after one pilot hold")
	}
	p2 := run()
	waitUntil(t, "pilot 2 to connect", func() bool { return a.g.NodeCount() == 2 })
	ask(t, a.rig, pilotSciKey, deepBody)
	waitUntil(t, "the account held", func() bool { a.store.Refresh(); return len(a.store.AccountHeldList()) == 1 })
	if a.pilotRecord(t, p2)["withheld"] == "" {
		t.Fatalf("pilot 2's credit not withheld: %v", a.pilotRecord(t, p2))
	}
	p3 := run()
	time.Sleep(1500 * time.Millisecond) // the agent retries within a second
	if n := a.g.NodeCount(); n != 2 || a.pilotRecord(t, p3)["first_connected_at"] != nil {
		t.Fatalf("a pilot of a held account connected (%d nodes): %v", n, a.pilotRecord(t, p3))
	}
	if w := a.pilotRecord(t, p0)["withheld"]; w != "answer checks: account held" {
		t.Fatalf("pilot 0 (not final, not held itself) withheld %v", w)
	}
	// Pilot 2's credit is withheld by its own hold or the account's, whichever came first.
	if w := a.pilotRecord(t, p2)["withheld"]; w != "answer checks" && w != "answer checks: account held" {
		t.Fatalf("pilot 2 withheld %v", w)
	}
	if w := a.pilotRecord(t, p1)["withheld"]; w != "answer checks" { // the first reason stands
		t.Fatalf("pilot 1 withheld %v", w)
	}
}

// The operations page lists held accounts with their reason, and drops one once it is cleared.
func TestOpsPageShowsHeldAccounts(t *testing.T) {
	a := newAcctRigWith(t, 100, nil, withProjects)
	admin, _ := a.signIn(t, 1)
	if err := a.store.HoldAccount(gateway.AccountHold{Account: "cx:0123456789abcdef0123456789abcdef", Rule: "answer-checks",
		Reason: "2 of its nodes were held by the answer checks", At: time.Now()}); err != nil {
		t.Fatal(err)
	}
	var page string
	waitFor(t, func() bool {
		_, page = a.get(t, "/account/ops", admin)
		return strings.Contains(page, `data-held-account="cx:0123456789abcdef0123456789abcdef"`)
	})
	for _, want := range []string{"Accounts held for review (1)", "2 of its nodes were held by the answer checks", "accounts account-clear"} {
		if !strings.Contains(page, want) {
			t.Fatalf("the page lacks %q: %s", want, page)
		}
	}
	a.store.ClearAccountHold("cx:0123456789abcdef0123456789abcdef")
	waitFor(t, func() bool {
		_, page = a.get(t, "/account/ops", admin)
		return !strings.Contains(page, "data-held-account")
	})
}
