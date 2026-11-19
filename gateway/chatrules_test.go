// SPDX-License-Identifier: Apache-2.0
package gateway

import (
	"bytes"
	"compress/gzip"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const geoCSV = `0.0.0.0,0.255.255.255,ZZ
10.0.0.0,10.0.0.255,NO
10.0.1.0,10.0.1.255,NO
10.0.2.0,10.0.2.255,US
127.0.0.0,127.255.255.255,SE
198.51.100.0,198.51.100.255,CN
"2001:db8::","2001:db8:0:ffff:ffff:ffff:ffff:ffff","DE"
2001:db8:1::,2001:db8:1::ff,FR
not,a,line
1.2.3.4,1.2.3.4,toolong
`

func TestGeoTable(t *testing.T) {
	gt, err := parseGeoTable(strings.NewReader(geoCSV))
	if err != nil {
		t.Fatal(err)
	}
	for addr, want := range map[string]string{
		"10.0.0.7":          "NO",
		"10.0.1.200":        "NO", // merged with its neighbour
		"10.0.2.1":          "US",
		"10.0.3.1":          "", // in no range
		"0.1.2.3":           "", // ZZ: unknown
		"127.0.0.1":         "SE",
		"198.51.100.9":      "CN",
		"::ffff:10.0.0.7":   "NO",
		"2001:db8::/64":     "DE", // clientIP's form for IPv6 clients
		"2001:db8:0:1::/64": "DE",
		"2001:db8:1::/64":   "", // the range covers only part of the /64
		"2001:db8:1::5":     "FR",
		"2001:db9::/64":     "",
		"1.2.3.4":           "",
		"":                  "",
		"garbage":           "",
	} {
		if got := gt.country(addr); got != want {
			t.Errorf("country(%q) = %q, want %q", addr, got, want)
		}
	}
	if n := len(gt.ranges); n != 6 {
		t.Errorf("%d ranges after merging, want 6", n)
	}
	var nilTable *geoTable
	if nilTable.country("10.0.0.7") != "" {
		t.Error("no table must know no country")
	}
	if _, err := parseGeoTable(strings.NewReader("nothing here\n")); err == nil {
		t.Error("a file without ranges must fail")
	}
}

func TestGeoTableGzipFile(t *testing.T) {
	var buf bytes.Buffer
	z := gzip.NewWriter(&buf)
	z.Write([]byte(geoCSV))
	z.Close()
	path := filepath.Join(t.TempDir(), "dbip-country-lite.csv.gz")
	os.WriteFile(path, buf.Bytes(), 0o600)
	gt, err := loadGeoTable(path)
	if err != nil {
		t.Fatal(err)
	}
	if gt.country("10.0.0.1") != "NO" {
		t.Error("gzip table not read")
	}
	if _, err := loadGeoTable(filepath.Join(t.TempDir(), "missing.csv")); err == nil {
		t.Error("a missing file must fail")
	}
}

func TestTransferCountries(t *testing.T) {
	for _, c := range []string{"NO", "SE", "DE", "IS", "LI", "GB", "CH", "JP", "KR", "BR", "NZ", "UY", "IL"} {
		if !transferFree[c] {
			t.Errorf("%s must be in the EEA or adequate", c)
		}
	}
	// The US (DPF-certified organisations only) and Canada (commercial organisations only) do not
	// cover a private volunteer; the others have no adequacy decision.
	for _, c := range []string{"US", "CA", "CN", "RU", "IN", "TR", "TW", "ZZ", "", "other"} {
		if transferFree[c] {
			t.Errorf("%s must not be in the list", c)
		}
	}
	if len(transferFree) != 44 { // EU 27 + IS, LI, NO + 14 adequacy decisions (Commission, 2026-11-19)
		t.Errorf("%d countries, want 44", len(transferFree))
	}
}

// rulesRig: a gateway with the chat rules on, a GeoIP table, and hand-made nodes.
func rulesRig(t *testing.T) *stickyRig {
	s := newStickyRig(t)
	gt, err := parseGeoTable(strings.NewReader(geoCSV))
	if err != nil {
		t.Fatal(err)
	}
	s.g.geo = gt
	return s
}

func TestRulesFor(t *testing.T) {
	s := rulesRig(t)
	g := s.g
	for _, c := range []struct {
		name     string
		trusted  bool
		tok      NodeToken
		ip       string
		terms    bool
		adequate bool
	}{
		{"config token, nothing", false, NodeToken{}, "10.0.0.1", false, false},
		{"trusted counts as accepted", true, NodeToken{}, "10.0.0.1", true, false},
		{"accepted, country matches", false, NodeToken{OperatorTerms: OperatorTermsVersion, Country: "no"}, "10.0.0.1", true, true},
		{"old terms version", false, NodeToken{OperatorTerms: "old", Country: "NO"}, "10.0.0.1", false, true},
		{"stated country differs", false, NodeToken{OperatorTerms: OperatorTermsVersion, Country: "SE"}, "10.0.0.1", true, false},
		{"no stated country", false, NodeToken{OperatorTerms: OperatorTermsVersion}, "10.0.0.1", true, false},
		{"unknown address", false, NodeToken{OperatorTerms: OperatorTermsVersion, Country: "NO"}, "10.9.9.9", true, false},
		{"matches, but not adequate", false, NodeToken{OperatorTerms: OperatorTermsVersion, Country: "US"}, "10.0.2.1", true, false},
		{"other matches nothing", false, NodeToken{OperatorTerms: OperatorTermsVersion, Country: "other"}, "198.51.100.1", true, false},
	} {
		r := g.rulesFor(c.trusted, "", c.tok, c.ip)
		if r.terms != c.terms || r.adequate != c.adequate {
			t.Errorf("%s: terms %v adequate %v, want %v %v", c.name, r.terms, r.adequate, c.terms, c.adequate)
		}
	}
}

// The routing rule: which nodes people's requests and science may use, with and without the opt-in.
func TestChatRulesAllow(t *testing.T) {
	s := rulesRig(t)
	now := time.Now()
	accepted := nodeRules{terms: true, geo: "NO", stated: "NO", adequate: true}
	abroad := nodeRules{terms: true, geo: "US", stated: "US"}
	mismatch := nodeRules{terms: true, geo: "US", stated: "NO"}
	noTerms := nodeRules{geo: "NO", stated: "NO", adequate: true}
	mk := func(id string, trusted bool, r nodeRules) *nodeConn {
		n := s.node(id, "op-"+id, trusted, 0)
		n.rules = r
		return n
	}
	nodes := map[string]*nodeConn{
		"eea":      mk("eea", false, accepted),
		"abroad":   mk("abroad", false, abroad),
		"mismatch": mk("mismatch", false, mismatch),
		"noterms":  mk("noterms", false, noTerms),
		"trusted":  mk("trusted", true, nodeRules{terms: true}),
		"own":      mk("own", false, nodeRules{}), // the requester's own node, nothing accepted, country unknown
	}
	for _, c := range []struct {
		name    string
		rt      route
		allowed []string
	}{
		{"chat, no opt-in", route{rules: true, class: ClassChat, self: "op-own"}, []string{"eea", "trusted", "own"}},
		{"api, no opt-in", route{rules: true, class: ClassAgent, self: "op-own"}, []string{"eea", "trusted", "own"}},
		{"chat, opt-in", route{rules: true, transfer: true, class: ClassChat, self: "op-own"}, []string{"eea", "abroad", "mismatch", "trusted", "own"}},
		{"science is not affected", route{rules: true, class: ClassScience}, []string{"eea", "abroad", "mismatch", "noterms", "trusted", "own"}},
		{"rules off", route{class: ClassChat}, []string{"eea", "abroad", "mismatch", "noterms", "trusted", "own"}},
		{"personal data: trusted only, as before", route{rules: true, transfer: true, trusted: true, class: ClassChat}, []string{"trusted"}},
	} {
		s.g.mu.Lock()
		var got []string
		for _, id := range []string{"eea", "abroad", "mismatch", "noterms", "trusted", "own"} {
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
	s := rulesRig(t)
	n := s.node("p-000000000001", "op-me", false, 0)
	n.pilot, n.deadline = "p-000000000001", time.Now().Add(time.Hour)
	rt := route{rules: true, class: ClassChat, self: "op-me"}
	s.g.mu.Lock()
	defer s.g.mu.Unlock()
	if rt.allows(n, time.Now()) {
		t.Fatal("a pilot without the operator terms served the account's own chat")
	}
	n.rules = nodeRules{terms: true, geo: "SE", stated: "SE", adequate: true}
	if !rt.allows(n, time.Now()) {
		t.Fatal("an accepted pilot in Sweden refused chat")
	}
}

func TestAccountAcceptancesPersist(t *testing.T) {
	path := filepath.Join(t.TempDir(), "accounts.json")
	s, err := OpenAccountStore(path)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	if _, err := s.SignIn("gh:1", now); err != nil {
		t.Fatal(err)
	}
	if terms, _ := s.operatorState("gh:1"); terms {
		t.Fatal("accepted before accepting")
	}
	if s.transferOK("gh:1") {
		t.Fatal("opted in by default")
	}
	if err := s.AcceptOperatorTerms("gh:1", OperatorTermsVersion, ViaAccountPage, "XX", now); err == nil {
		t.Fatal("a country outside the list was stored")
	}
	if err := s.AcceptOperatorTerms("gh:1", OperatorTermsVersion, ViaAccountPage, "NO", now); err != nil {
		t.Fatal(err)
	}
	if err := s.SetTransferConsent("gh:1", true, ViaSignIn, now); err != nil {
		t.Fatal(err)
	}
	// A restart: the decisions come back from the file.
	s2, err := OpenAccountStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if terms, country := s2.operatorState("gh:1"); !terms || country != "NO" {
		t.Fatalf("after a restart: terms %v country %q", terms, country)
	}
	if !s2.transferOK("gh:1") {
		t.Fatal("the opt-in did not survive a restart")
	}
	a, _, _ := s2.Get("gh:1")
	if a.OperatorTerms.Version != OperatorTermsVersion || !a.OperatorTerms.At.Equal(now) || a.OperatorTerms.Via != ViaAccountPage ||
		a.TransferConsent.Version != TransferConsentVersion || a.TransferConsent.Via != ViaSignIn {
		t.Fatalf("records: %+v %+v", a.OperatorTerms, a.TransferConsent)
	}
	// A new version of the texts needs a new acceptance.
	s2.Update(func(f *AccountsFile) error {
		f.find("gh:1").OperatorTerms.Version = "older"
		f.find("gh:1").TransferConsent.Version = "older"
		return nil
	})
	if terms, _ := s2.operatorState("gh:1"); terms || s2.transferOK("gh:1") {
		t.Fatal("an older version still counts")
	}
	s2.WithdrawOperatorTerms("gh:1")
	s2.SetTransferConsent("gh:1", false, ViaAccountPage, now)
	a, _, _ = s2.Get("gh:1")
	if a.OperatorTerms != nil || a.TransferConsent != nil {
		t.Fatal("withdrawals not recorded")
	}
}

func TestConnectorTermsMapsTheUser(t *testing.T) {
	s, err := OpenAccountStore(filepath.Join(t.TempDir(), "accounts.json"))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	unclaimed, err := s.connectorTerms("boinc", "42", OperatorTermsVersion, "SE", now.Add(-time.Hour), now)
	if err != nil || !unclaimed {
		t.Fatalf("unclaimed %v err %v", unclaimed, err)
	}
	id, _, err := s.pilotAccount("boinc", "42", now) // the first pilot lands on the same account
	if err != nil {
		t.Fatal(err)
	}
	if terms, country := s.operatorState(id); !terms || country != "SE" {
		t.Fatalf("terms %v country %q", terms, country)
	}
	// An older record of the same version does not replace a newer acceptance.
	if _, err := s.connectorTerms("boinc", "42", OperatorTermsVersion, "", now.Add(-48*time.Hour), now); err != nil {
		t.Fatal(err)
	}
	a, _, _ := s.Get(id)
	if !a.OperatorTerms.At.Equal(now.Add(-time.Hour)) || a.OperatorCountry != "SE" || a.OperatorTerms.Via != "connector:boinc" {
		t.Fatalf("record: %+v %q", a.OperatorTerms, a.OperatorCountry)
	}
	// A linked active account gets the acceptance.
	s.SignIn("gh:7", now)
	s.SetStatus("gh:7", StatusActive)
	s.Link("gh:7", "boinc", "43", now)
	if unclaimed, err := s.connectorTerms("boinc", "43", OperatorTermsVersion, "", now, now); err != nil || unclaimed {
		t.Fatalf("linked: unclaimed %v err %v", unclaimed, err)
	}
	if terms, _ := s.operatorState("gh:7"); !terms {
		t.Fatal("linked account did not get the acceptance")
	}
	s.SetStatus("gh:7", StatusBanned)
	if _, err := s.connectorTerms("boinc", "43", OperatorTermsVersion, "", now, now); err != errNoOperator {
		t.Fatalf("banned account: %v", err)
	}
}

// A withdrawal is not undone by a connector resending an acceptance from before it, and a record
// older than the account's acceptance changes nothing, its country included.
func TestConnectorTermsAfterWithdrawal(t *testing.T) {
	s, err := OpenAccountStore(filepath.Join(t.TempDir(), "accounts.json"))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	s.SignIn("gh:7", now)
	s.SetStatus("gh:7", StatusActive)
	s.Link("gh:7", "boinc", "43", now)
	if err := s.AcceptOperatorTerms("gh:7", OperatorTermsVersion, ViaAccountPage, "NO", now); err != nil {
		t.Fatal(err)
	}
	s.connectorTerms("boinc", "43", "older-version", "SE", now.Add(-time.Hour), now)
	if terms, country := s.operatorState("gh:7"); !terms || country != "NO" {
		t.Fatalf("an older record replaced the account's acceptance: %v %q", terms, country)
	}
	if err := s.WithdrawOperatorTerms("gh:7"); err != nil {
		t.Fatal(err)
	}
	s.connectorTerms("boinc", "43", OperatorTermsVersion, "", now.Add(-time.Minute), now)
	if terms, _ := s.operatorState("gh:7"); terms {
		t.Fatal("a resent acceptance from before the withdrawal brought it back")
	}
	s.connectorTerms("boinc", "43", OperatorTermsVersion, "", time.Now().Add(time.Second), now)
	if terms, _ := s.operatorState("gh:7"); !terms {
		t.Fatal("a new acceptance after the withdrawal did not count")
	}
}

// A range inside another of a different country makes the enclosing range's country unknown.
func TestGeoTableNestedConflict(t *testing.T) {
	gt, err := parseGeoTable(strings.NewReader("10.0.0.0,10.0.255.255,NO\n10.0.1.0,10.0.1.255,US\n10.1.0.0,10.1.255.255,SE\n10.1.2.0,10.1.2.255,SE\n"))
	if err != nil {
		t.Fatal(err)
	}
	for addr, want := range map[string]string{"10.0.1.5": "", "10.0.0.5": "", "10.1.2.5": "SE", "10.1.0.5": "SE"} {
		if got := gt.country(addr); got != want {
			t.Errorf("country(%s) = %q, want %q", addr, got, want)
		}
	}
}

func TestOAuthFlowCarriesTransfer(t *testing.T) {
	f := oauthFlow{provider: "github", state: "s", contact: "1", transfer: "1"}
	p := strings.Split(f.String(), "|")
	if len(p) != 8 || p[7] != "1" {
		t.Fatalf("flow %q", f.String())
	}
}

// A request still waiting for a node follows the account's opt-in as it is now: withdrawn while it
// waited (or before a retry), it does not reach a node abroad.
func TestWaitingRequestFollowsWithdrawal(t *testing.T) {
	s := rulesRig(t)
	store, err := OpenAccountStore(filepath.Join(t.TempDir(), "accounts.json"))
	if err != nil {
		t.Fatal(err)
	}
	s.g.accounts = store
	store.SignIn("gh:1", time.Now())
	n := s.node("abroad", "op-abroad", false, 0)
	n.rules = nodeRules{terms: true, geo: "US", stated: "US"}
	rt := &route{want: "deep", rules: true, transfer: true, acct: "gh:1", class: ClassChat, exclude: map[*nodeConn]bool{}}
	got, _, why := s.g.pickWait(context.Background(), rt, time.Now().Add(300*time.Millisecond))
	if got != nil || why != timedOut {
		t.Fatalf("withdrawn opt-in: node %v, why %q", got, why)
	}
	store.SetTransferConsent("gh:1", true, ViaAccountPage, time.Now())
	if got, _, why := s.g.pickWait(context.Background(), rt, time.Now().Add(300*time.Millisecond)); got != n {
		t.Fatalf("with the opt-in: node %v, why %q", got, why)
	}
}
