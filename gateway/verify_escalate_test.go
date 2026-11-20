// SPDX-License-Identifier: Apache-2.0
package gateway

import (
	"path/filepath"
	"testing"
	"time"
)

func TestEscalateDefaults(t *testing.T) {
	v, err := VerifyConfig{}.withDefaults()
	if err != nil || v.Escalate != (EscalateConfig{NodeAfter: 3, AccountAfter: 2, WindowHours: 24}) {
		t.Fatalf("%+v %v", v.Escalate, err)
	}
	if v, err := (VerifyConfig{Escalate: EscalateConfig{NodeAfter: -1, AccountAfter: -1}}).withDefaults(); err != nil ||
		v.Escalate.NodeAfter != -1 || v.Escalate.AccountAfter != -1 {
		t.Fatalf("off: %+v %v", v.Escalate, err)
	}
	for _, bad := range []EscalateConfig{{NodeAfter: -2}, {AccountAfter: -3}, {WindowHours: -1}} {
		if _, err := (VerifyConfig{Escalate: bad}).withDefaults(); err == nil {
			t.Errorf("%+v accepted", bad)
		}
	}
}

func TestFailureWindow(t *testing.T) {
	var c nodeChecks
	t0 := time.Now()
	h := time.Hour
	if k := c.failure("n", t0, 24*h, 3); k != 1 {
		t.Fatal(k)
	}
	if k := c.failure("n", t0.Add(23*h), 24*h, 3); k != 2 {
		t.Fatal(k)
	}
	if k := c.failure("n", t0.Add(25*h), 24*h, 3); k != 2 { // the first is out of the window
		t.Fatal(k)
	}
	for i := 0; i < 5; i++ {
		c.failure("n", t0.Add(26*h), 24*h, 3)
	}
	if k := len(c.by["n"].fails); k != 3 {
		t.Fatalf("%d remembered, want at most 3", k)
	}
	if k := c.failure("other", t0, 24*h, 3); k != 1 {
		t.Fatalf("another node shares the count: %d", k)
	}
}

func TestAccountHolds(t *testing.T) {
	s, err := OpenAccountStore(filepath.Join(t.TempDir(), "accounts.json"))
	if err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 11, 20, 9, 0, 0, 0, time.UTC)
	for _, p := range []NodeHold{{Node: "p-000000000001", Rule: ruleAnswerChecks, Account: "cx:a", At: at},
		{Node: "n-00000002", Rule: "file-upload", Account: "cx:a", At: at}, // a guard's, not the checks'
		{Node: "n-00000003", Rule: ruleAnswerChecks, Account: "gh:2", At: at}} {
		if err := s.HoldNode(p); err != nil {
			t.Fatal(err)
		}
	}
	if n := s.checkHoldsOf("cx:a"); n != 1 {
		t.Fatalf("check holds of cx:a: %d", n)
	}
	if s.isAccountHeld("cx:a") || s.isAccountHeld("") {
		t.Fatal("held before HoldAccount")
	}
	h := AccountHold{Account: "cx:a", Rule: ruleAnswerChecks, Reason: "first", At: at}
	if err := s.HoldAccount(h); err != nil {
		t.Fatal(err)
	}
	s.HoldAccount(AccountHold{Account: "cx:a", Reason: "second"}) // the first stands
	if l := s.accountHolds(); len(l) != 1 || l[0] != h || !s.isAccountHeld("cx:a") || s.isAccountHeld("gh:2") {
		t.Fatalf("%+v", l)
	}
	again, _ := OpenAccountStore(s.path) // in the file
	if !again.isAccountHeld("cx:a") {
		t.Fatal("the hold is not in the file")
	}
	if err := s.ClearAccountHold("cx:a"); err != nil || s.isAccountHeld("cx:a") {
		t.Fatalf("clear: %v", err)
	}
	if err := s.ClearAccountHold("cx:a"); err == nil {
		t.Fatal("cleared an account that is not held")
	}
}

// Only credit that is not final yet is withheld: pilots that have not ended, and ended ones within
// the hold after their last disconnect; another account's and an already withheld pilot are untouched.
func TestWithholdAccountPilots(t *testing.T) {
	s, err := openScienceDB("")
	if err != nil {
		t.Fatal(err)
	}
	defer s.db.Close()
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	mk := func(account string, connectAt time.Time, end bool) string {
		t.Helper()
		p, _, err := s.createPilot("boinc", now.Add(48*time.Hour), 100, 100, connectAt.Add(-time.Minute))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.bindPilot("boinc", p.ID, "5", "11", "", account, connectAt.Add(-time.Minute)); err != nil {
			t.Fatal(err)
		}
		if _, err := s.pilotConnected(p.ID, "agent", connectAt); err != nil {
			t.Fatal(err)
		}
		if end {
			if err := s.pilotDisconnected(p.ID, "pilot: idle", connectAt, connectAt.Add(time.Minute)); err != nil {
				t.Fatal(err)
			}
		}
		return p.ID
	}
	running := mk("cx:a", now.Add(-time.Hour), false)
	recent := mk("cx:a", now.Add(-2*time.Hour), true) // ended 2 h ago: not final
	old := mk("cx:a", now.Add(-30*time.Hour), true)   // final
	other := mk("cx:b", now.Add(-time.Hour), false)   // another account
	done := mk("cx:a", now.Add(-time.Hour), false)
	s.withholdPilot(done, "guard: file-upload")
	if err := s.withholdAccountPilots("cx:a", "answer checks: account held", now); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{running: "answer checks: account held", recent: "answer checks: account held", old: "",
		other: "", done: "guard: file-upload"}
	for id, w := range want {
		p, err := s.pilot("", id)
		if err != nil {
			t.Fatal(err)
		}
		if p.Withheld != w {
			t.Errorf("%s (%s): withheld %q, want %q", id, p.State, p.Withheld, w)
		}
	}
}

// A claim merges an unclaimed account into the claimant: its holds by the answer checks go with it,
// so the same BOINC user's next pilots (now the claimant's) stay held; an existing hold stands.
func TestClaimKeepsCheckHolds(t *testing.T) {
	r := newLinkRig(t, nil)
	pid, _ := r.create()
	r.bind(pid, "5")
	p, _ := r.g.sci.pilot("boinc", pid)
	cx := p.account
	at := r.clock()
	r.g.accounts.HoldNode(NodeHold{Node: pid, Rule: ruleAnswerChecks, Account: cx, At: at})
	r.g.accounts.HoldAccount(AccountHold{Account: cx, Rule: ruleAnswerChecks, Reason: "2 of its nodes", At: at})
	r.addAccount("gh:42", StatusActive)
	if _, err := r.g.accounts.claimLink("gh:42", "boinc", "5", AccountLink{AttachKey: weakKey("5"), Via: LinkClaimed}, r.clock()); err != nil {
		t.Fatal(err)
	}
	if !r.g.accounts.isAccountHeld("gh:42") || r.g.accounts.isAccountHeld(cx) || r.g.accounts.checkHoldsOf("gh:42") != 1 {
		t.Fatalf("after the claim: holds %+v, node holds %+v", r.g.accounts.accountHolds(), r.g.accounts.nodeHolds())
	}

	f := AccountsFile{AccountHolds: []AccountHold{{Account: "gh:1", Reason: "own"}, {Account: "cx:a", Reason: "merged"}}}
	f.moveCheckHolds("cx:a", "gh:1")
	if len(f.AccountHolds) != 1 || f.AccountHolds[0].Reason != "own" {
		t.Fatalf("the claimant's own hold should stand: %+v", f.AccountHolds)
	}
}
