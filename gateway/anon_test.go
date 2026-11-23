// SPDX-License-Identifier: Apache-2.0
package gateway

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/aisathome/aisathome/node/modelcheck"
	"github.com/aisathome/aisathome/protocol"
)

func testStore(t *testing.T) *AccountStore {
	t.Helper()
	s, err := OpenAccountStore(filepath.Join(t.TempDir(), "accounts.json"))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// A node without an account is an unclaimed "ax:" account with one "a-" node whose token is stored
// hashed; its token connects; it counts in its own pool, never in the accounts' max_nodes; the switches
// and the pool stop new ones.
func TestRegisterAnon(t *testing.T) {
	s := testStore(t)
	t0 := time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)
	token, id, err := s.RegisterAnon(t0)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(token, "nt-"+id+".") || !validNodeID(id) || !strings.HasPrefix(id, "a-") {
		t.Fatalf("token %q, id %q", token, id)
	}
	if got, ok := splitNodeToken(token); !ok || got != id {
		t.Fatalf("splitNodeToken: %q %v", got, ok)
	}
	f := s.Snapshot()
	if len(f.Accounts) != 1 {
		t.Fatalf("%d accounts", len(f.Accounts))
	}
	a := f.Accounts[0]
	if !strings.HasPrefix(a.ID, "ax:") || len(a.ID) != 3+32 || a.Status != StatusUnclaimed || a.FirstNode == nil || len(a.Nodes) != 1 {
		t.Fatalf("account %+v", a)
	}
	n := a.Nodes[0]
	if n.ID != id || n.Hash != keyHash(token) || strings.Contains(n.Prefix, token[len(n.Prefix):]) || n.Via != viaAnon {
		t.Fatalf("node %+v", n)
	}
	acct, nid, ok := s.nodeToken(token)
	if !ok || acct != a.ID || nid != id {
		t.Fatalf("nodeToken: %q %q %v", acct, nid, ok)
	}
	if _, _, ok := s.nodeLive(keyHash(token)); !ok {
		t.Fatal("nodeLive refuses a node without an account")
	}
	if f.nodeCount() != 0 || f.anonCount() != 1 {
		t.Fatalf("counts: nodes %d, anonymous %d", f.nodeCount(), f.anonCount())
	}
	// Another unclaimed account (a connector's BOINC user) still holds no node token.
	s.Update(func(f *AccountsFile) error {
		f.Accounts = append(f.Accounts, &Account{ID: "cx:00", Status: StatusUnclaimed,
			Nodes: []AccountNode{{ID: "n-00000001", Hash: keyHash("nt-n-00000001.x")}}})
		return nil
	})
	if _, _, ok := s.nodeToken("nt-n-00000001.x"); ok {
		t.Fatal("a connector's unclaimed account's node token connects")
	}
	// A banned one is cut off.
	if err := s.SetStatus(a.ID, StatusBanned); err != nil {
		t.Fatal(err)
	}
	if _, _, ok := s.nodeToken(token); ok {
		t.Fatal("a banned node without an account still connects")
	}

	for name, set := range map[string]func(*AccountSettings){
		"anon_closed":  func(st *AccountSettings) { st.AnonClosed = true },
		"nodes_closed": func(st *AccountSettings) { st.NodesClosed = true },
	} {
		s := testStore(t)
		s.Update(func(f *AccountsFile) error { set(&f.Settings); return nil })
		if _, _, err := s.RegisterAnon(t0); !errors.Is(err, errAnonClosed) {
			t.Errorf("%s: %v", name, err)
		}
	}
	s = testStore(t)
	s.Update(func(f *AccountsFile) error { f.Settings.MaxAnonNodes = 2; f.Settings.MaxNodes = 1; return nil })
	for i := 0; i < 2; i++ {
		if _, _, err := s.RegisterAnon(t0); err != nil {
			t.Fatalf("anonymous node %d with max_nodes 1: %v", i+1, err)
		}
	}
	if _, _, err := s.RegisterAnon(t0); !errors.Is(err, errAnonFull) {
		t.Fatalf("over the pool: %v", err)
	}
	if DefaultMaxAnonNodes != 25 || (AccountSettings{}).MaxAnonNodesOrDefault() != 25 {
		t.Error("the default pool is not 25")
	}
}

// At most 3 registrations per address a day and 30 an hour over the gateway, in memory; a registration
// that failed gives its count back.
func TestAnonRegLimits(t *testing.T) {
	var r anonRegs
	t0 := time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)
	for i := 0; i < 3; i++ {
		if _, err := r.take("203.0.113.5", t0.Add(time.Duration(i)*time.Hour)); err != nil {
			t.Fatalf("registration %d: %v", i+1, err)
		}
	}
	if _, err := r.take("203.0.113.5", t0.Add(23*time.Hour)); !errors.Is(err, errAnonBusy) {
		t.Fatalf("a 4th within a day: %v", err)
	}
	if _, err := r.take("203.0.113.5", t0.Add(24*time.Hour)); err != nil {
		t.Fatalf("a day after the first: %v", err)
	}
	undo, err := r.take("2001:db8::/64", t0)
	if err != nil {
		t.Fatal(err)
	}
	undo()
	for i := 0; i < 3; i++ {
		if _, err := r.take("2001:db8::/64", t0); err != nil {
			t.Fatalf("after a failed one given back, registration %d: %v", i+1, err)
		}
	}
	var g anonRegs
	for i := 0; i < 30; i++ {
		if _, err := g.take(fmt.Sprintf("10.0.0.%d", i), t0); err != nil {
			t.Fatalf("network-wide %d: %v", i+1, err)
		}
	}
	if _, err := g.take("10.0.1.1", t0.Add(59*time.Minute)); !errors.Is(err, errAnonRush) {
		t.Fatalf("31st in an hour: %v", err)
	}
	if _, err := g.take("10.0.1.1", t0.Add(time.Hour)); err != nil {
		t.Fatalf("an hour later: %v", err)
	}
}

// Retention: a node that never connected goes 24 h after it registered, one not seen for 90 days goes;
// a connected one, a recent one and accounts' nodes stay.
func TestAnonRetention(t *testing.T) {
	s := testStore(t)
	t0 := time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)
	_, never, _ := s.RegisterAnon(t0)
	_, seen, _ := s.RegisterAnon(t0)
	_, live, _ := s.RegisterAnon(t0)
	s.SignIn("gh:1", t0)
	s.SetStatus("gh:1", StatusActive)
	s.addNodeVia("gh:1", "", t0, nil, false, "")
	acctOf := func(id string) string {
		for _, a := range s.Snapshot().Accounts {
			for _, n := range a.Nodes {
				if n.ID == id {
					return a.ID
				}
			}
		}
		return ""
	}
	if err := s.anonSeen(acctOf(seen), seen, t0.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	connected := func(id string) bool { return id == live }
	if gone, _ := s.expireAnon(t0.Add(23*time.Hour), connected); len(gone) != 0 {
		t.Fatalf("within a day: %v", gone)
	}
	gone, err := s.expireAnon(t0.Add(25*time.Hour), connected)
	if err != nil || len(gone) != 1 || gone[0] != never {
		t.Fatalf("after a day: %v %v", gone, err)
	}
	if acctOf(seen) == "" || acctOf(live) == "" {
		t.Fatal("a node that connected was deleted")
	}
	// Seen again within the hour: the time is not rewritten (a node that reconnects often).
	lastSeen := func() time.Time {
		a, _, _ := s.Get(acctOf(seen))
		return *a.Nodes[0].LastSeen
	}
	s.anonSeen(acctOf(seen), seen, t0.Add(90*time.Minute))
	if !lastSeen().Equal(t0.Add(time.Hour)) {
		t.Errorf("last seen rewritten within anonSeenMin: %v", lastSeen())
	}
	s.anonSeen(acctOf(seen), seen, t0.Add(2*time.Hour))
	if !lastSeen().Equal(t0.Add(2 * time.Hour)) {
		t.Errorf("last seen not rewritten after anonSeenMin: %v", lastSeen())
	}
	if gone, _ := s.expireAnon(t0.Add(2*time.Hour+anonKeep-time.Minute), connected); len(gone) != 0 {
		t.Fatalf("just under 90 days: %v", gone)
	}
	gone, _ = s.expireAnon(t0.Add(2*time.Hour+anonKeep+time.Minute), connected)
	if len(gone) != 1 || gone[0] != seen {
		t.Fatalf("after 90 days: %v", gone)
	}
	if acctOf(live) == "" {
		t.Fatal("a connected node was deleted")
	}
	if a, _, ok := s.Get("gh:1"); !ok || len(a.Nodes) != 1 {
		t.Fatal("an account's node was touched")
	}
	// A held node goes 24 h after its hold, connected or not (it would keep its pool slot).
	if err := s.HoldNode(NodeHold{Node: live, Rule: "download-and-execute", At: t0}); err != nil {
		t.Fatal(err)
	}
	if gone, _ := s.expireAnon(t0.Add(anonHeldKeep-time.Minute), connected); len(gone) != 0 {
		t.Fatalf("a hold under a day old: %v", gone)
	}
	gone, _ = s.expireAnon(t0.Add(anonHeldKeep), connected)
	if len(gone) != 1 || gone[0] != live {
		t.Fatalf("a hold a day old: %v", gone)
	}
	if hs := s.nodeHolds(); len(hs) != 0 {
		t.Fatalf("the deleted node's hold stays: %+v", hs)
	}
	_, live, _ = s.RegisterAnon(t0)
	// A revoked node leaves an empty account: it goes too.
	if err := s.RevokeNode("", live); err != nil {
		t.Fatal(err)
	}
	if gone, _ := s.expireAnon(t0, connected); len(gone) != 1 || !strings.HasPrefix(gone[0], "ax:") {
		t.Fatalf("an empty anonymous account: %v", gone)
	}
}

// Science only, as a hard rule: a node without an account never gets a person's chat, API or agent
// request, whatever the chat rules or the request's consent say, and no science before its admission.
func TestAnonRoutes(t *testing.T) {
	now := time.Now()
	n := &nodeConn{id: "a-00000001", operator: "acct-x", account: "ax:00", anon: true}
	sci := &route{class: ClassScience, rules: true, exclude: map[*nodeConn]bool{}}
	if sci.allows(n, now) {
		t.Fatal("science on a node without an account before its admission")
	}
	n.admitted = true
	if !sci.allows(n, now) {
		t.Fatal("no science on an admitted node without an account")
	}
	for _, rules := range []bool{true, false} {
		n.rules.terms = true // even if something set it
		for _, rt := range []*route{
			{class: ClassChat, rules: rules},
			{class: ClassAgent, rules: rules, tools: true, consent: true},
			{class: ClassChat, rules: rules, self: "acct-x"}, // as if its own operator asked
			{class: "api", rules: rules},
		} {
			rt.exclude = map[*nodeConn]bool{}
			if rt.allows(n, now) {
				t.Errorf("rules %v: %+v went to a node without an account", rules, rt)
			}
		}
	}
	// Agent calls of a science project (prove2me) during probation, as for pilots.
	n.probEnd = now.Add(time.Hour)
	agents := &route{class: ClassScience, tools: true, consent: true, exclude: map[*nodeConn]bool{}}
	if agents.allows(n, now) {
		t.Error("science agents in probation without the project's exception")
	}
	agents.pilotProbationOK = true
	if !agents.allows(n, now) {
		t.Error("science agents in probation refused despite the project's exception")
	}
	if noChatWhy(&route{class: ClassChat, rules: true}, n, now) != whyAnon {
		t.Error("the serve capacity does not name a node without an account")
	}
	// Its account never counts as having accepted the operator terms, even with a record.
	s := testStore(t)
	s.Update(func(f *AccountsFile) error {
		f.Accounts = append(f.Accounts, &Account{ID: "ax:01", Status: StatusUnclaimed,
			OperatorTerms: &Acceptance{Version: OperatorTermsVersion, At: now}})
		return nil
	})
	g := &Gateway{accounts: s}
	if g.rulesFor("ax:01", NodeToken{}).terms {
		t.Error("a node without an account counts as having accepted the operator terms")
	}
}

// Admission: probes only until anonAdmitChecks checks passed; a failed check before then holds it,
// whatever the withhold switch; after it, one failed check does not, a fail result does.
func TestAnonAdmission(t *testing.T) {
	entry, _ := testMCRef(t, 20)
	small, _ := testMCRef(t, 20)
	small.Model = "qwen3.8-27b-q3"
	admitBig, _ := testMCRef(t, 20)
	admitSmall, _ := testMCRef(t, 20)
	admitSmall.Model = "qwen3.8-27b-q3"
	mc := testChecker(t, ModelCheckConfig{Enabled: true, References: []ModelCheckReference{entry, small},
		AdmitReferences: []ModelCheckReference{admitBig, admitSmall}, Admit: time.Second}, "")
	g := &Gateway{mc: mc, log: quietLog()}
	now := time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)
	pass := ModelCheckResult{At: now, Verdict: modelcheck.Pass}
	fail := ModelCheckResult{At: now, Verdict: modelcheck.Fail, KL: 1}
	file := func(n *nodeConn, s *ModelCheckNode, c ModelCheckResult) string {
		mc.mu.Lock()
		defer mc.mu.Unlock()
		return g.mcFileLocked(n, s, c)
	}
	big := []protocol.ModelInfo{{Name: "qwen3.8-27b", Slots: 1}}
	n := &nodeConn{id: "a-1", anon: true, models: big}
	s := &ModelCheckNode{Node: n.id, Model: "qwen3.8-27b"}
	mc.st.Nodes[n.id] = s
	mc.mu.Lock()
	if m := mc.meanLocked(n); m != time.Second {
		t.Errorf("mean before admission %v, want the admission interval", m)
	}
	mc.mu.Unlock()
	for i := 0; i < anonAdmitChecks; i++ {
		if mc.admitted(n) {
			t.Fatalf("admitted after %d passed checks", i)
		}
		if why := file(n, s, pass); why != "" {
			t.Fatalf("held on a pass: %s", why)
		}
	}
	if !mc.admitted(n) || mc.passedInWindow(n.id) != anonAdmitChecks {
		t.Fatalf("not admitted after %d passes", anonAdmitChecks)
	}
	mc.mu.Lock()
	if m := mc.meanLocked(n); m == time.Second {
		t.Error("still the admission interval after admission")
	}
	mc.mu.Unlock()
	// Admitted for the model its checks were of only: back with another model (one with a reference, then
	// one without), it is not, and the other model's checks start afresh.
	none := &nodeConn{id: n.id, anon: true, models: []protocol.ModelInfo{{Name: "some-model", Slots: 1}}}
	if mc.admitted(none) {
		t.Fatal("admitted with a model that has no reference")
	}
	mc.mu.Lock()
	mc.nodeLocked(n, "qwen3.8-27b", now) // the same model again keeps its checks
	mc.mu.Unlock()
	if !mc.admitted(n) {
		t.Fatal("a reconnect with the same model lost the admission")
	}
	if why := file(n, s, fail); why != "" || !mc.admitted(n) {
		t.Fatalf("one failed check after admission: held %q, admitted %v", why, mc.admitted(n))
	}
	if why := file(n, s, fail); why == "" || !strings.Contains(why, "a node without an account") || s.HeldAt.IsZero() || mc.admitted(n) {
		t.Fatalf("a fail result after admission: held %q", why)
	}

	// Before admission: one failed check holds it (withholding off, the observing period not over).
	if mc.withholding() {
		t.Fatal("withholding is on in this test")
	}
	n2 := &nodeConn{id: "a-2", anon: true, models: big}
	s2 := &ModelCheckNode{Node: n2.id, Model: "qwen3.8-27b"}
	mc.st.Nodes[n2.id] = s2
	file(n2, s2, pass)
	if why := file(n2, s2, fail); why == "" || !strings.Contains(why, "before its admission") {
		t.Fatalf("a failed check before admission: %q", why)
	}
	if mc.admitted(n2) {
		t.Fatal("a held node is admitted")
	}
	// Admitted with one model, back with another that has a reference: not admitted, its checks start afresh.
	n4 := &nodeConn{id: "a-4", anon: true, models: big}
	s4 := &ModelCheckNode{Node: n4.id, Model: "qwen3.8-27b"}
	mc.st.Nodes[n4.id] = s4
	for i := 0; i < anonAdmitChecks; i++ {
		file(n4, s4, pass)
	}
	if !mc.admitted(n4) {
		t.Fatal("not admitted")
	}
	q3 := &nodeConn{id: n4.id, anon: true, models: []protocol.ModelInfo{{Name: "qwen3.8-27b-q3", Slots: 1}}}
	if mc.admitted(q3) {
		t.Fatal("admitted with another model before any check of it")
	}
	mc.mu.Lock()
	mc.nodeLocked(q3, "qwen3.8-27b-q3", now)
	mc.mu.Unlock()
	if len(s4.Checks) != 0 || mc.admitted(q3) || mc.admitted(n4) {
		t.Fatalf("after coming back with another model: %d checks kept", len(s4.Checks))
	}

	// An account's node is never held while observing.
	n3 := &nodeConn{id: "n-3"}
	s3 := &ModelCheckNode{Node: n3.id}
	file(n3, s3, fail)
	if why := file(n3, s3, fail); why != "" {
		t.Fatalf("an account's node held while observing: %q", why)
	}
}

// Requests waiting elsewhere do not hold back the probes of a node without an account that no request
// may use yet; once admitted, its probes yield like any node's.
func TestAnonReserve(t *testing.T) {
	g := &Gateway{node: map[string]*nodeConn{}, waiting: map[*route]struct{}{{class: ClassScience}: {}}}
	n := &nodeConn{id: "a-1", anon: true, models: []protocol.ModelInfo{{Name: "qwen3.8-27b", Slots: 1}}}
	g.node[n.id] = n
	if !g.mcReserve(n, "qwen3.8-27b") {
		t.Fatal("a node in admission gave no slot for a probe while a request waits elsewhere")
	}
	n.inflight, n.admitted = 0, true
	if g.mcReserve(n, "qwen3.8-27b") {
		t.Fatal("an admitted node took a probe while a request waits")
	}
}

// The secret prompts of the ongoing checks never go to a node without an account that is not admitted:
// its probes come from the admission set (ids "admit:…"), whatever its record says until it is admitted,
// also after a hold was cleared or it came back with another model; once admitted, from the secret set
// like any node. Without an admission set for its model, it gets no probe at all.
func TestAnonNeverGetsSecretPrompts(t *testing.T) {
	main, _ := testMCRef(t, 20)
	admit, _ := testMCRef(t, 20)
	mc := testChecker(t, ModelCheckConfig{Enabled: true, References: []ModelCheckReference{main},
		AdmitReferences: []ModelCheckReference{admit}}, "")
	secret := map[string]bool{}
	for _, id := range mc.refs["qwen3.8-27b"].ids {
		secret[id] = true
	}
	n := &nodeConn{id: "a-1", anon: true, models: []protocol.ModelInfo{{Name: "qwen3.8-27b", Slots: 1}}}
	now := time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)
	pick := func() (string, bool) {
		mc.mu.Lock()
		defer mc.mu.Unlock()
		s := mc.nodeLocked(n, mc.checkedModel(n), now)
		r := mc.probeRefLocked(n, s, "qwen3.8-27b")
		return mc.pickPromptLocked(s, r), r.admit
	}
	s := mc.st.Nodes
	for i := 0; i < 200; i++ { // more than the admission pool: its list starts again, still the admission set
		id, admitSet := pick()
		if secret[id] || !strings.HasPrefix(id, admitPrefix) || !admitSet {
			t.Fatalf("probe %d to a node not admitted: %q (admission set %v)", i, id, admitSet)
		}
	}
	rec := s[n.id]
	pass := ModelCheckResult{At: now, Verdict: modelcheck.Pass}
	rec.Checks = []ModelCheckResult{pass, pass, pass}
	if id, admitSet := pick(); !secret[id] || admitSet {
		t.Fatalf("an admitted node got %q (admission set %v), not the secret set", id, admitSet)
	}
	rec.Checks, rec.HeldAt = []ModelCheckResult{pass, pass, pass}, now // held: not admitted
	if id, _ := pick(); secret[id] {
		t.Fatalf("a held node got secret prompt %q", id)
	}
	rec.HeldAt = time.Time{}
	// An account's node gets the secret set from the start.
	acct := &nodeConn{id: "n-1", models: n.models}
	mc.mu.Lock()
	sa := mc.nodeLocked(acct, mc.checkedModel(acct), now)
	if r := mc.probeRefLocked(acct, sa, "qwen3.8-27b"); r.admit {
		t.Error("an account's node gets the admission set")
	}
	mc.mu.Unlock()
	// No admission set for its model: not checked at all, so no probe.
	q3 := &nodeConn{id: "a-2", anon: true, models: []protocol.ModelInfo{{Name: "qwen3.8-27b-q3", Slots: 1}}}
	big := testChecker(t, ModelCheckConfig{Enabled: true, References: []ModelCheckReference{main}}, "")
	if big.checks(n) || big.checks(q3) {
		t.Error("a node without an account is checked without an admission set")
	}
	// The two sets' groups never mix.
	g := &Gateway{mc: mc, log: quietLog()}
	ra := mc.admits["qwen3.8-27b"]
	mc.mu.Lock()
	sa2 := mc.nodeLocked(n, "qwen3.8-27b", now)
	sa2.Checks = nil
	mc.pending[n.id] = nil
	g.mcAnswerLocked(n, sa2, ra, mcAnswer{id: ra.ids[0], model: "qwen3.8-27b", admit: true, toks: ra.byID[ra.ids[0]].Tokens}, true, "", now)
	rm := mc.refs["qwen3.8-27b"]
	g.mcAnswerLocked(n, sa2, rm, mcAnswer{id: rm.ids[0], model: "qwen3.8-27b", toks: rm.byID[rm.ids[0]].Tokens}, true, "", now)
	if p := mc.pending[n.id]; len(p) != 1 || p[0].admit {
		t.Errorf("a group mixed the sets: %+v", p)
	}
	mc.mu.Unlock()
}

// wsPair returns a server-side WebSocket connection (closed with the test), as a node's.
func wsPair(t *testing.T) *websocket.Conn {
	t.Helper()
	got := make(chan *websocket.Conn, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		got <- c
		<-r.Context().Done()
	}))
	t.Cleanup(srv.Close)
	cli, _, err := websocket.Dial(context.Background(), "ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cli.CloseNow() })
	return <-got
}

// Deleting a held node without an account deletes its hold too. A sweep that read the node's token as live
// before the deletion and its hold after it must never let the node take requests again (code review,
// 2026-11-23): the deletion waits for the sweep, and marks the node held and closes it before any later
// sweep can see the hold gone.
func TestAnonExpiryNeverUnholdsALiveNode(t *testing.T) {
	dir := t.TempDir()
	t0 := time.Date(2026, 11, 23, 12, 0, 0, 0, time.UTC)
	var mu sync.Mutex
	now := t0
	g, err := New(Config{StateDir: dir, AnonNow: func() time.Time { mu.Lock(); defer mu.Unlock(); return now },
		Accounts: &AccountsConfig{File: dir + "/accounts.json", PublicURL: "https://site.test",
			SessionSecret: strings.Repeat("s", 32), GitHubClientID: "id", GitHubClientSecret: "secret"}}, quietLog())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { g.Close() })
	token, id, err := g.accounts.RegisterAnon(t0)
	if err != nil {
		t.Fatal(err)
	}
	acct, _, _ := g.accounts.nodeToken(token)
	if err := g.accounts.HoldNode(NodeHold{Node: id, Rule: "download-and-execute", At: t0}); err != nil {
		t.Fatal(err)
	}
	n := &nodeConn{id: id, account: acct, anon: true, admitted: true, token: token, held: true, conn: wsPair(t),
		operator: "op", models: []protocol.ModelInfo{{Name: "qwen3.8-27b", Roles: []string{"deep"}, Slots: 1}}}
	g.mu.Lock()
	g.node[id] = n
	g.mu.Unlock()
	mu.Lock()
	now = t0.Add(anonHeldKeep + time.Minute)
	mu.Unlock()

	expired := make(chan struct{})
	var once sync.Once
	hook := func() { // the expiry starts while a sweep is between reading tokens and reading holds (once:
		// the gateway's own periodic sweep may be the one)
		once.Do(func() {
			go func() { g.sweepAnon(); close(expired) }()
			select {
			case <-expired:
			case <-time.After(300 * time.Millisecond):
			}
		})
	}
	g.sweepHook.Store(&hook)
	g.sweepNodes()
	g.mu.Lock()
	held, there := n.held, g.node[id] == n
	usable := (&route{class: ClassScience, exclude: map[*nodeConn]bool{}}).allows(n, time.Now())
	g.mu.Unlock()
	if there && (!held || usable) {
		t.Fatalf("a guard-held node is usable again during its deletion (held %v, science allowed %v)", held, usable)
	}
	<-expired
	g.sweepHook.Store(nil)
	g.mu.Lock()
	held = n.held
	g.mu.Unlock()
	if !held {
		t.Fatal("the deleted node is not marked held")
	}
	if a, _, ok := g.accounts.Get(acct); ok {
		t.Fatalf("the node without an account was not deleted: %+v", a)
	}
	if g.nodeHeld(id) {
		t.Fatal("its hold stays")
	}
}
