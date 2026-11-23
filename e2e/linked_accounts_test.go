// Linked accounts end to end: a connector user's pilots serve under
// an unclaimed account until its person claims it from the account page; the connector checks the key
// and answers; the pilot reconnects as the person's node, with the account's chat switch. "Run on
// BOINC" makes a linked user from the account page.
//
// SPDX-License-Identifier: Apache-2.0
package e2e

import (
	"context"
	"net/url"
	"strings"
	"testing"

	"github.com/aisathome/aisathome/gateway"
)

func linkRig(t *testing.T) *acctRig {
	t.Helper()
	a := newAcctRigNodes(t, 100, nil, func(c *gateway.Config) {
		c.Projects = []gateway.Project{{ID: "lab", KeySHA256: []string{sha(pilotSciKey)}, AgentOK: true, PilotProbationOK: true}}
		c.Connectors = []gateway.Connector{{ID: "boinc", KeySHA256: []string{sha(connectorKey)}, MaxPilots: 5, PilotsChatEnabled: true,
			Accounts: &gateway.ConnectorAccounts{Name: "BOINC", AttachURL: "https://boinc.site.test/proj/",
				AttachCommand: "boinccmd --project_attach {url} {key}", AddressDomain: "nomail.site.test"}}}
		c.QueueTimeoutSec = 2
	})
	return a
}

// pendingRequest returns the connector's one pending request.
func (a *acctRig) pendingRequest(t *testing.T) map[string]any {
	t.Helper()
	var l []any
	waitUntil(t, "one pending request", func() bool {
		l, _ = a.connector(t, "GET", "/requests", nil)["requests"].([]any)
		return len(l) == 1
	})
	return l[0].(map[string]any)
}

func TestClaimEndToEnd(t *testing.T) {
	a := linkRig(t)
	engine := mockEngine(t, false)
	t.Cleanup(engine.Close)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// A BOINC-first volunteer: the pilot serves under an unclaimed account.
	id, cred := a.newPilot(t)
	a.runPilot(ctx, cred, engine.URL)
	a.connector(t, "POST", "/pilots/"+id+"/bind", map[string]any{"user": "5", "host": "11"})
	waitUntil(t, "the pilot to connect", func() bool { return a.g.NodeCount() == 1 })
	if code, _ := ask(t, a.rig, pilotSciKey, deepBody); code != 200 {
		t.Fatalf("science: %d", code)
	}
	before := waitUsage(t, a.rig, 1)[0]["operator"]
	if p := a.pilotRecord(t, id); p["account"] != "unclaimed" || p["credit"].(float64) <= 0 {
		t.Fatalf("before the claim: %v", p)
	}

	// The person signs in and claims it with the key BOINC shows them.
	s, _ := a.signIn(t, 7)
	if _, page := a.get(t, "/account", s); !strings.Contains(page, "Claim a BOINC account") {
		t.Fatal("no claim form on the account page")
	}
	acctKey, key := strings.Repeat("0f", 16), "5_"+strings.Repeat("ab", 16) // the account key, the weak key BOINC answers
	if resp, _ := a.post(t, "/account/connectors/boinc/claim", s, url.Values{"key": {key}}.Encode()); resp.StatusCode != 400 {
		t.Fatalf("a weak key claimed: %d", resp.StatusCode)
	}
	if resp, body := a.post(t, "/account/connectors/boinc/claim", s, url.Values{"key": {acctKey}}.Encode()); resp.StatusCode != 303 {
		t.Fatalf("claim: %d %s", resp.StatusCode, body)
	}
	q := a.pendingRequest(t)
	if q["kind"] != "claim" || q["account_key"] != acctKey || q["user"] != nil {
		t.Fatalf("claim as the connector sees it: %v", q)
	}
	out := a.connector(t, "POST", "/requests/"+q["id"].(string), map[string]any{"ok": true, "user": "5", "key": key})
	if st := out["request"].(map[string]any)["state"]; st != "done" {
		t.Fatalf("after the answer: %v", out)
	}

	// The pilot reconnects as the person's node; its record and credit follow.
	waitUntil(t, "the pilot to reconnect", func() bool {
		p := a.pilotRecord(t, id)
		return p["connections"].(float64) >= 2 && p["state"] == gateway.PilotConnected && a.g.NodeCount() == 1
	})
	if p := a.pilotRecord(t, id); p["account"] != "linked" || p["credit"].(float64) <= 0 {
		t.Fatalf("after the claim: %v", p)
	}
	waitUntil(t, "science on the reconnected pilot", func() bool { code, _ := ask(t, a.rig, pilotSciKey, deepBody); return code == 200 })
	var after any
	for _, r := range waitUsage(t, a.rig, 2) {
		if r["node"] == id && r["outcome"] == "ok" {
			after = r["operator"]
		}
	}
	if after == before || after == nil {
		t.Errorf("operator before %v, after %v: the pilot still serves as the unclaimed account", before, after)
	}
	_, page := a.get(t, "/account", s)
	for _, want := range []string{id, "boinccmd --project_attach https://boinc.site.test/proj/ " + key, "(claimed)"} {
		if !strings.Contains(page, want) {
			t.Errorf("account page lacks %q", want)
		}
	}
	a.store.Refresh()
	for _, x := range a.store.Snapshot().Accounts {
		if x.Status == gateway.StatusUnclaimed {
			t.Errorf("unclaimed account left: %s", x.ID)
		}
	}

	// The account page's switch keeps chat off the pilot, science goes on.
	waitUntil(t, "chat to reach the pilot", func() bool { code, _ := ask(t, a.rig, "user-key", deepBody); return code == 200 })
	if resp, _ := a.post(t, "/account/pilots/chat", s, "chat=off"); resp.StatusCode != 303 {
		t.Fatalf("switch: %d", resp.StatusCode)
	}
	waitUntil(t, "the switch to keep chat off", func() bool { code, _ := ask(t, a.rig, "user-key", deepBody); return code == 503 })
	if code, _ := ask(t, a.rig, pilotSciKey, deepBody); code != 200 {
		t.Fatal("science stopped with the switch")
	}

	// A second account cannot claim the same user, whatever the connector says.
	s2, _ := a.signIn(t, 8)
	a.post(t, "/account/connectors/boinc/claim", s2, url.Values{"key": {acctKey}}.Encode())
	q = a.pendingRequest(t)
	out = a.connector(t, "POST", "/requests/"+q["id"].(string), map[string]any{"ok": true, "user": "5", "key": key})
	if r := out["request"].(map[string]any); r["state"] != "failed" || !strings.Contains(r["error"].(string), "linked to another") {
		t.Fatalf("second claim: %v", out)
	}
	if p := a.pilotRecord(t, id); p["state"] != gateway.PilotConnected {
		t.Errorf("the refused claim disturbed the pilot: %v", p["state"])
	}
}

func TestRunOnBOINCEndToEnd(t *testing.T) {
	a := linkRig(t)
	s, _ := a.signIn(t, 9)
	if resp, body := a.post(t, "/account/connectors/boinc/run", s, url.Values{"name": {"Ada"}, "password": {"a long password"}}.Encode()); resp.StatusCode != 303 {
		t.Fatalf("run: %d %s", resp.StatusCode, body)
	}
	q := a.pendingRequest(t)
	addr, _ := q["email"].(string)
	if q["kind"] != "create_link" || !strings.HasSuffix(addr, "@nomail.site.test") || q["user_name"] != "Ada" || len(q["passwd_hash"].(string)) != 32 {
		t.Fatalf("create_link as the connector sees it: %v", q)
	}
	key := "321_" + strings.Repeat("9a", 16)
	a.connector(t, "POST", "/requests/"+q["id"].(string), map[string]any{"ok": true, "user": "321", "key": key})
	_, page := a.get(t, "/account", s)
	for _, want := range []string{"boinccmd --project_attach https://boinc.site.test/proj/ " + key, addr, "BOINC user <strong>321</strong>"} {
		if !strings.Contains(page, want) {
			t.Errorf("account page lacks %q", want)
		}
	}
	// The user's first pilot serves as this account's node at once: no unclaimed account.
	id, _ := a.newPilot(t)
	a.connector(t, "POST", "/pilots/"+id+"/bind", map[string]any{"user": "321", "host": "12"})
	if p := a.pilotRecord(t, id); p["account"] != "linked" {
		t.Fatalf("pilot of a user made here: %v", p["account"])
	}
	a.store.Refresh()
	if raw := a.store.Snapshot(); strings.Contains(snapshotText(raw), "@") {
		t.Error("an address in the accounts file")
	}
}

func snapshotText(f gateway.AccountsFile) string {
	var b strings.Builder
	for _, a := range f.Accounts {
		b.WriteString(a.ID)
		for _, l := range a.Links {
			b.WriteString(" " + l.Connector + " " + l.User + " " + l.AttachKey)
		}
		b.WriteString(" " + a.ContactEmail + "\n")
	}
	return b.String()
}

// A waitlisted account claims its BOINC user: the pilot reconnects as its node
// and keeps serving science; the account still has no chat.
func TestWaitlistedClaimEndToEnd(t *testing.T) {
	a := linkRig(t)
	engine := mockEngine(t, false)
	t.Cleanup(engine.Close)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	id, cred := a.newPilot(t)
	a.runPilot(ctx, cred, engine.URL)
	a.connector(t, "POST", "/pilots/"+id+"/bind", map[string]any{"user": "6", "host": "13"})
	waitUntil(t, "the pilot to connect", func() bool { return a.g.NodeCount() == 1 })

	s, _ := a.signIn(t, 10)
	if err := a.store.SetStatus("gh:10", gateway.StatusWaitlisted); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, "the gateway to see the waitlist", func() bool {
		_, page := a.get(t, "/account", s)
		return strings.Contains(page, "You are on the waitlist") && strings.Contains(page, "Claim a BOINC account")
	})
	acctKey := strings.Repeat("1e", 16)
	if resp, body := a.post(t, "/account/connectors/boinc/claim", s, url.Values{"key": {acctKey}}.Encode()); resp.StatusCode != 303 {
		t.Fatalf("claim: %d %s", resp.StatusCode, body)
	}
	q := a.pendingRequest(t)
	if st := a.connector(t, "POST", "/requests/"+q["id"].(string), map[string]any{"ok": true, "user": "6", "key": "6_" + strings.Repeat("cd", 16)})["request"].(map[string]any)["state"]; st != "done" {
		t.Fatalf("waitlisted claim: %v", st)
	}
	waitUntil(t, "the pilot to reconnect", func() bool {
		p := a.pilotRecord(t, id)
		return p["connections"].(float64) >= 2 && p["state"] == gateway.PilotConnected && p["account"] == "linked"
	})
	waitUntil(t, "science on the waitlisted account's pilot", func() bool { code, _ := ask(t, a.rig, pilotSciKey, deepBody); return code == 200 })
	if resp, _ := a.get(t, "/account/auth", s); resp.StatusCode == 200 {
		t.Error("the waitlisted account opened the chat")
	}
	if _, page := a.get(t, "/account", s); !strings.Contains(page, id) {
		t.Error("the waitlist page does not list the pilot")
	}
}
