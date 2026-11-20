// SPDX-License-Identifier: Apache-2.0
package gateway

import (
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// finalTerms stands in a final operator terms version for one test (the code's is a draft).
func finalTerms(t *testing.T) {
	old := OperatorTermsVersion
	OperatorTermsVersion = "test-final-1"
	t.Cleanup(func() { OperatorTermsVersion = old })
}

func TestRulesFor(t *testing.T) {
	finalTerms(t)
	g := newStickyRig(t).g
	for _, c := range []struct {
		name  string
		tok   NodeToken
		terms bool
	}{
		{"config token, nothing", NodeToken{}, false},
		{"a removed trust counts for nothing", NodeToken{LegacyTrust: "trusted"}, false},
		{"accepted by config", NodeToken{OperatorTerms: OperatorTermsVersion}, true},
		{"old terms version", NodeToken{OperatorTerms: "old"}, false},
	} {
		if r := g.rulesFor("", c.tok); r.terms != c.terms {
			t.Errorf("%s: terms %v, want %v", c.name, r.terms, c.terms)
		}
	}
}

// A draft of the operator terms is accepted by nobody: not on the account page, not by config, not by a
// connector. Only the requester's own nodes serve people's requests until the final text ships.
func TestDraftTermsNeverCount(t *testing.T) {
	old := OperatorTermsVersion
	OperatorTermsVersion = "draft-2026-12-01"
	t.Cleanup(func() { OperatorTermsVersion = old })
	s, err := OpenAccountStore(filepath.Join(t.TempDir(), "accounts.json"))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	s.SignIn("gh:1", now)
	s.SetStatus("gh:1", StatusActive)
	if err := s.AcceptOperatorTerms("gh:1", OperatorTermsVersion, ViaAccountPage, now); err != nil {
		t.Fatal(err)
	}
	if s.operatorTerms("gh:1") {
		t.Fatal("an acceptance of the draft counts")
	}
	if _, applied, terms, _ := s.connectorTerms("boinc", "42", OperatorTermsVersion, now, now); !applied || terms {
		t.Fatalf("connector, draft: applied %v terms %v", applied, terms)
	}
	g := newStickyRig(t).g
	if g.rulesFor("", NodeToken{OperatorTerms: OperatorTermsVersion}).terms {
		t.Fatal("a config token naming the draft counts")
	}
}

// The routing rule: which nodes people's requests and science may use.
func TestChatRulesAllow(t *testing.T) {
	s := newStickyRig(t)
	now := time.Now()
	mk := func(id string, terms bool) *nodeConn {
		n := s.node(id, "op-"+id, 0)
		n.rules = nodeRules{terms: terms}
		return n
	}
	nodes := map[string]*nodeConn{
		"accepted": mk("accepted", true),
		"noterms":  mk("noterms", false),
		"own":      mk("own", false), // the requester's own node, nothing accepted
	}
	order := []string{"accepted", "noterms", "own"}
	for _, c := range []struct {
		name    string
		rt      route
		allowed []string
	}{
		{"chat", route{rules: true, class: ClassChat, self: "op-own"}, []string{"accepted", "own"}},
		{"api", route{rules: true, class: ClassAgent, self: "op-own"}, []string{"accepted", "own"}},
		{"science is not affected", route{rules: true, class: ClassScience}, []string{"accepted", "noterms", "own"}},
		{"rules off", route{class: ClassChat}, []string{"accepted", "noterms", "own"}},
		{"personal data: the requester's own node only", route{rules: true, pii: piiEmail, class: ClassChat, self: "op-own"}, []string{"own"}},
		{"personal data without an account: none", route{pii: piiEmail, class: ClassChat}, nil},
	} {
		s.g.mu.Lock()
		var got []string
		for _, id := range order {
			if c.rt.allows(nodes[id], now) {
				got = append(got, id)
			}
		}
		s.g.mu.Unlock()
		if strings.Join(got, ",") != strings.Join(c.allowed, ",") {
			t.Errorf("%s: allowed %v, want %v", c.name, got, c.allowed)
		}
	}
}

// A pilot (a stranger's machine bound to an account) is never "one's own" for the rules either.
func TestChatRulesPilotNotOwn(t *testing.T) {
	s := newStickyRig(t)
	n := s.node("p-000000000001", "op-me", 0)
	n.pilot, n.deadline = "p-000000000001", time.Now().Add(time.Hour)
	rt := route{rules: true, class: ClassChat, self: "op-me"}
	s.g.mu.Lock()
	defer s.g.mu.Unlock()
	if rt.allows(n, time.Now()) {
		t.Fatal("a pilot without the operator terms served the account's own chat")
	}
	n.rules = nodeRules{terms: true}
	if !rt.allows(n, time.Now()) {
		t.Fatal("an accepted pilot refused chat")
	}
}

func TestOperatorTermsPersist(t *testing.T) {
	finalTerms(t)
	path := filepath.Join(t.TempDir(), "accounts.json")
	s, err := OpenAccountStore(path)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	if _, err := s.SignIn("gh:1", now); err != nil {
		t.Fatal(err)
	}
	if s.operatorTerms("gh:1") {
		t.Fatal("accepted before accepting")
	}
	if err := s.AcceptOperatorTerms("gh:1", OperatorTermsVersion, ViaAccountPage, now); err != nil {
		t.Fatal(err)
	}
	s2, err := OpenAccountStore(path) // a restart
	if err != nil {
		t.Fatal(err)
	}
	if !s2.operatorTerms("gh:1") {
		t.Fatal("the acceptance did not survive a restart")
	}
	a, _, _ := s2.Get("gh:1")
	if a.OperatorTerms.Version != OperatorTermsVersion || !a.OperatorTerms.At.Equal(now) || a.OperatorTerms.Via != ViaAccountPage {
		t.Fatalf("record: %+v", a.OperatorTerms)
	}
	s2.Update(func(f *AccountsFile) error { f.find("gh:1").OperatorTerms.Version = "older"; return nil })
	if s2.operatorTerms("gh:1") {
		t.Fatal("an older version still counts")
	}
	s2.WithdrawOperatorTerms("gh:1")
	a, _, _ = s2.Get("gh:1")
	if a.OperatorTerms != nil || a.TermsWithdrawn == nil {
		t.Fatal("withdrawal not recorded")
	}
	// A file written while the opt-in to transfers existed (staging, 2026-11-19) still loads.
	s2.Update(func(f *AccountsFile) error { return nil })
}

func TestConnectorTermsMapsTheUser(t *testing.T) {
	finalTerms(t)
	s, err := OpenAccountStore(filepath.Join(t.TempDir(), "accounts.json"))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	unclaimed, applied, terms, err := s.connectorTerms("boinc", "42", OperatorTermsVersion, now.Add(-time.Hour), now)
	if err != nil || !unclaimed || !applied || !terms {
		t.Fatalf("unclaimed %v applied %v terms %v err %v", unclaimed, applied, terms, err)
	}
	id, _, err := s.pilotAccount("boinc", "42", now) // the first pilot lands on the same account
	if err != nil {
		t.Fatal(err)
	}
	if !s.operatorTerms(id) {
		t.Fatal("the pilot's account has no acceptance")
	}
	// An older record does not replace a newer acceptance, whatever its version, and says so.
	if _, applied, terms, _ := s.connectorTerms("boinc", "42", "older", now.Add(-48*time.Hour), now); applied || !terms {
		t.Fatalf("older record: applied %v terms %v", applied, terms)
	}
	a, _, _ := s.Get(id)
	if !a.OperatorTerms.At.Equal(now.Add(-time.Hour)) || a.OperatorTerms.Via != "connector:boinc" {
		t.Fatalf("record: %+v", a.OperatorTerms)
	}
	s.SignIn("gh:7", now)
	s.SetStatus("gh:7", StatusActive)
	s.Link("gh:7", "boinc", "43", now)
	if unclaimed, _, terms, err := s.connectorTerms("boinc", "43", OperatorTermsVersion, now, now); err != nil || unclaimed || !terms {
		t.Fatalf("linked: unclaimed %v terms %v err %v", unclaimed, terms, err)
	}
	s.SetStatus("gh:7", StatusBanned)
	if _, _, _, err := s.connectorTerms("boinc", "43", OperatorTermsVersion, now, now); err != errNoOperator {
		t.Fatalf("banned account: %v", err)
	}
}

// A withdrawal is not undone by a connector resending an acceptance from before it.
func TestConnectorTermsAfterWithdrawal(t *testing.T) {
	finalTerms(t)
	s, err := OpenAccountStore(filepath.Join(t.TempDir(), "accounts.json"))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	s.SignIn("gh:7", now)
	s.SetStatus("gh:7", StatusActive)
	s.Link("gh:7", "boinc", "43", now)
	if err := s.AcceptOperatorTerms("gh:7", OperatorTermsVersion, ViaAccountPage, now); err != nil {
		t.Fatal(err)
	}
	if err := s.WithdrawOperatorTerms("gh:7"); err != nil {
		t.Fatal(err)
	}
	if _, applied, terms, _ := s.connectorTerms("boinc", "43", OperatorTermsVersion, now.Add(-time.Minute), now); applied || terms {
		t.Fatalf("a resent acceptance from before the withdrawal: applied %v terms %v", applied, terms)
	}
	if _, _, terms, _ := s.connectorTerms("boinc", "43", OperatorTermsVersion, time.Now().Add(time.Second), now); !terms {
		t.Fatal("a new acceptance after the withdrawal did not count")
	}
}

// The approved text, word for word where the tests can see it, and its version is final.
func TestOperatorTermsText(t *testing.T) {
	if !termsFinal() || OperatorTermsVersion != "2026-11-19" {
		t.Fatalf("version %q", OperatorTermsVersion)
	}
	for _, want := range []string{
		"Users may not put personal data into their chats, and we send your node no account, name, email address or IP address. Whatever a request contains, you agree to:\n1. Use requests only",
		"\n2. Not log, keep, copy, read or share prompts or answers, and not try to find out who sent them. Nothing from a request may outlive the request.\n",
		"\n5. Tell us at contact@aisathome.org without delay, within 24 hours if you can,",
		"\n8. Let us check your node (model checks, safety checks on answers, our records of what it served), and answer our questions about an incident.\n",
		"Your node answers other people's chats only after you have accepted these terms; until then it does science work only.",
	} {
		if !strings.Contains(OperatorTermsText, want) {
			t.Errorf("operator terms lack %q", want)
		}
	}
	if len(operatorTermsPoints) != 8 {
		t.Errorf("%d points, want 8", len(operatorTermsPoints))
	}
}

// X-Forwarded-For counts only from loopback and the configured proxies: a peer on a private network
// that is not a listed proxy cannot claim another address (for the per-address node limit, email
// sign-in's limits, connectors' allow_from).
func TestClientIPTrustedProxies(t *testing.T) {
	g := &Gateway{}
	var err error
	if g.proxies, err = parsePrefixes([]string{"10.89.0.0/24", "192.0.2.10"}); err != nil {
		t.Fatal(err)
	}
	cases := []struct{ remote, xff, want string }{
		{"203.0.113.9:4000", "198.51.100.1", "203.0.113.9"}, // a public peer: XFF ignored
		{"127.0.0.1:4000", "198.51.100.1, 198.51.100.2", "198.51.100.2"},
		{"10.89.0.5:4000", "1.1.1.1, 198.51.100.7", "198.51.100.7"}, // the listed proxy network: rightmost hop
		{"192.0.2.10:4000", "198.51.100.8", "198.51.100.8"},
		{"10.88.0.5:4000", "198.51.100.7", "10.88.0.5"},       // private, but not a listed proxy: spoofed XFF ignored
		{"192.168.1.20:4000", "198.51.100.7", "192.168.1.20"}, // the same
		{"[::1]:4000", "2001:db8:1:2:3:4:5:6", "2001:db8:1:2::/64"},
		{"[2001:db8:a:b:c::1]:4000", "", "2001:db8:a:b::/64"},
		{"127.0.0.1:4000", "garbage", "127.0.0.1"},
		{"127.0.0.1:4000", "", "127.0.0.1"},
		{"[::ffff:127.0.0.1]:4000", "198.51.100.3", "198.51.100.3"},
	}
	for _, c := range cases {
		r, _ := http.NewRequest("POST", "/", nil)
		r.RemoteAddr = c.remote
		if c.xff != "" {
			r.Header.Set("X-Forwarded-For", c.xff)
		}
		if got := g.clientIP(r); got != c.want {
			t.Errorf("clientIP(%s, %q) = %q, want %q", c.remote, c.xff, got, c.want)
		}
	}
	if _, err := parsePrefixes([]string{"not-an-address"}); err == nil {
		t.Error("a bad trusted_proxies entry was accepted")
	}
}

// Creating a node accepts the operator terms, in the same write, unless the form did not show the line,
// the account has accepted the current version already or switched to science only; an outdated
// acceptance is renewed.
func TestAcceptOnNodeCreate(t *testing.T) {
	s, err := OpenAccountStore(filepath.Join(t.TempDir(), "accounts.json"))
	if err != nil {
		t.Fatal(err)
	}
	t0 := time.Date(2026, 11, 20, 9, 0, 0, 0, time.UTC)
	s.SignIn("gh:1", t0)
	s.SetStatus("gh:1", StatusActive)
	add := func(at time.Time, terms bool) bool {
		t.Helper()
		_, _, recorded, err := s.addNode("gh:1", "", at, nil, terms)
		if err != nil {
			t.Fatal(err)
		}
		return recorded
	}
	if add(t0, false) || s.operatorTerms("gh:1") {
		t.Fatal("a form without the line accepted the terms")
	}
	if !add(t0, true) {
		t.Fatal("first node under the line: nothing recorded")
	}
	a, _, _ := s.Get("gh:1")
	if a.OperatorTerms == nil || a.OperatorTerms.Via != ViaNodeCreated || !a.OperatorTerms.At.Equal(t0) || !s.operatorTerms("gh:1") || len(a.Nodes) != 2 {
		t.Fatalf("record: %+v, %d nodes", a.OperatorTerms, len(a.Nodes))
	}
	if add(t0.Add(time.Hour), true) {
		t.Fatal("an acceptance of the current version was renewed")
	}
	s.Update(func(f *AccountsFile) error { f.find("gh:1").OperatorTerms.Version = "older"; return nil })
	if !add(t0.Add(2*time.Hour), true) || !s.operatorTerms("gh:1") {
		t.Fatal("an outdated acceptance was not renewed by a new node")
	}
	s.WithdrawOperatorTerms("gh:1") // "Science only on my nodes"
	if add(time.Now().Add(time.Hour), true) || s.operatorTerms("gh:1") {
		t.Fatal("a new node undid science only")
	}
	s.AcceptOperatorTerms("gh:1", OperatorTermsVersion, ViaAccountPage, time.Now().Add(time.Minute))
	if add(time.Now().Add(time.Hour), true) {
		t.Fatal("renewed after switching back")
	}
}
