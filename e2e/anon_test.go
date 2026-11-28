// Nodes without an account (gateway/anon.go) end to end: the node's own client registers it, a real node
// agent connects with the token in front of a fake engine, the model check admits it after its checks,
// it serves science and never a person's request, earns no credit, a wrong model is held before it
// serves anything, the limits hold, and linking moves it to an account.
//
// SPDX-License-Identifier: Apache-2.0
package e2e

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aisathome/aisathome/gateway"
	"github.com/aisathome/aisathome/node"
	"github.com/aisathome/aisathome/node/accountlink"
	"github.com/aisathome/aisathome/node/modelcheck"
	"github.com/aisathome/aisathome/protocol"
)

// anonRig is an accounts gateway with the model check (admission every few milliseconds), the science
// projects, the chat rules as configured, and no node.
func anonRig(t *testing.T, rulesOff bool, mod func(*gateway.Config)) (*acctRig, *linkClock) {
	t.Helper()
	entry, _ := mcReference(t)
	admit, _ := mcReferenceOf(t, "q", "Admission question %d, please.")
	clk := &linkClock{t: time.Now()}
	a := newAcctRigNodes(t, 100, nil, func(c *gateway.Config) {
		c.ChatRulesOff = rulesOff
		c.QueueTimeoutSec = 2 // a request no node may serve fails soon
		c.AnonNow = clk.now
		c.StatsRefresh = time.Millisecond
		c.ModelCheck = &gateway.ModelCheckConfig{Enabled: true, References: []gateway.ModelCheckReference{entry},
			AdmitReferences: []gateway.ModelCheckReference{admit},
			Tick:            10 * time.Millisecond, Interval: 300 * time.Millisecond, Recheck: 300 * time.Millisecond, Admit: 20 * time.Millisecond}
		withProjects(c)
		if mod != nil {
			mod(c)
		}
	})
	return a, clk
}

// bothSets is a reference holding the secret set's and the admission set's prompts and answers, so that a
// fake engine answers both like the right model.
func bothSets(t *testing.T) (*modelcheck.Reference, map[string]bool) {
	_, main := mcReference(t)
	_, admit := mcReferenceOf(t, "q", "Admission question %d, please.")
	both := *main
	both.Results = append(append([]modelcheck.Result(nil), main.Results...), admit.Results...)
	secret := map[string]bool{}
	for _, r := range main.Results {
		secret[r.Text] = true
	}
	return &both, secret
}

// probeTexts are the prompts of the probes an engine got, in order.
func probeTexts(e *mcEngine) []string {
	var out []string
	for _, b := range e.probeBodies() {
		out = append(out, b["messages"].([]any)[0].(map[string]any)["content"].(string))
	}
	return out
}

func anonClient(a *acctRig, token string) *accountlink.Client {
	return &accountlink.Client{Base: a.gw.URL, Token: token, Sleep: func(ctx context.Context, d time.Duration) error {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(20 * time.Millisecond):
			return nil
		}
	}}
}

// waitSelf polls GET /v1/node/self until its state is want.
func waitSelf(t *testing.T, c *accountlink.Client, want string) protocol.NodeSelf {
	t.Helper()
	var s protocol.NodeSelf
	for deadline := time.Now().Add(20 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		var err error
		if s, err = c.Self(context.Background()); err == nil && s.State == want {
			return s
		}
	}
	t.Fatalf("node state %q, want %q: %+v", s.State, want, s)
	return s
}

func TestAnonNodeServesScienceOnly(t *testing.T) {
	for _, rulesOff := range []bool{false, true} { // science only whatever the chat rules say
		name := "chat rules on"
		if rulesOff {
			name = "chat rules off"
		}
		t.Run(name, func(t *testing.T) {
			a, _ := anonRig(t, rulesOff, nil)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			// The rig's clock for nodes without an account started before the funnel's counters did.
			a.store.EditForTest(func(f *gateway.AccountsFile) error { f.Funnel.Since = time.Now().Add(-time.Hour); return nil })
			reg := anonClient(a, "")
			if _, err := reg.Register(ctx, false); err == nil || !strings.Contains(err.Error(), "18 or older") {
				t.Fatalf("register without accepting the terms: %v", err)
			}
			r, err := reg.Register(ctx, true)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.HasPrefix(r.NodeID, "a-") || !strings.HasPrefix(r.Token, "nt-"+r.NodeID+".") {
				t.Fatalf("registered %+v", r)
			}
			c := anonClient(a, r.Token)
			if s := waitSelf(t, c, protocol.SelfOffline); !s.Anonymous || !s.ScienceOnly || s.ChecksNeeded != 3 {
				t.Fatalf("before connecting: %+v", s)
			}
			ref, secret := bothSets(t)
			eng := &mcEngine{ref: ref}
			srv := httptest.NewServer(eng)
			t.Cleanup(srv.Close)
			a.startTokenNode(ctx, r.Token, srv, 1, 32768)
			a.waitNodes(t, 1)
			// Its first connection is counted, once, for the ops page's funnel (gateway/funnel.go): just
			// after the node is registered, so wait for the write.
			firsts := func() int {
				a.store.Refresh()
				if f := a.store.Snapshot().Funnel; f != nil && f.Days[time.Now().UTC().Format(time.DateOnly)] != nil {
					return f.Days[time.Now().UTC().Format(time.DateOnly)].FirstConnect
				}
				return 0
			}
			for end := time.Now().Add(5 * time.Second); firsts() == 0 && time.Now().Before(end); {
				time.Sleep(10 * time.Millisecond)
			}
			if n := firsts(); n != 1 {
				t.Fatalf("first connections counted: %d", n)
			}

			// Admitted after its checks, never before: a science request before then gets no node.
			if s, _ := c.Self(ctx); s.State == protocol.SelfAdmitted && s.ChecksPassed < 3 {
				t.Fatalf("admitted with %d checks", s.ChecksPassed)
			}
			s := waitSelf(t, c, protocol.SelfAdmitted)
			if s.ChecksPassed < 3 {
				t.Fatalf("admitted after %d passed checks", s.ChecksPassed)
			}
			// Until its admission it got the admission set only, never a prompt of the secret set.
			before := probeTexts(eng)
			if len(before) < 12 {
				t.Fatalf("admitted after %d probe prompts", len(before))
			}
			for i, p := range before[:12] {
				if secret[p] || !strings.HasPrefix(p, "Admission question") {
					t.Fatalf("probe %d before admission: %q", i, p)
				}
			}

			// Admitted, it gets the secret set like any node (the rig's interval is long: wait for one).
			for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(20 * time.Millisecond) {
				got := false
				for _, p := range probeTexts(eng) {
					got = got || secret[p]
				}
				if got {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("no secret-set probe after admission")
				}
			}
			// Science: served by it.
			resp := a.do(t, "POST", "/v1/chat/completions", projectKey, chatBody)
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode != 200 || resp.Header.Get(servedByHeader) != r.NodeID {
				t.Fatalf("science: %d by %q: %s", resp.StatusCode, resp.Header.Get(servedByHeader), body)
			}
			// People's requests never: a chat, an agent with consent, an account's API key.
			_, key := a.apiUser(t, 7)
			for name, k := range map[string]string{"chat": "user-key", "agent": "agent-key", "account key": key} {
				body := chatBody
				if name == "agent" {
					body = `{"model":"deep","stream":true,"messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"f","parameters":{}}}]}`
				}
				resp := a.do(t, "POST", "/v1/chat/completions", k, body)
				io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
				if resp.StatusCode == 200 || resp.Header.Get(servedByHeader) == r.NodeID {
					t.Errorf("%s: %d, served by %q", name, resp.StatusCode, resp.Header.Get(servedByHeader))
				}
			}
			if n := eng.people.Load(); n != 1 {
				t.Errorf("the engine got %d requests that were not probes; want the one science request", n)
			}

			// No credit: the record says 0 and the ledger has nothing.
			recs := waitUsage(t, a.rig, 1)
			for _, u := range recs {
				if u["class"] == "science" && (u["node"] != r.NodeID || u["credit"] != nil) {
					t.Errorf("science record: %v", u)
				}
			}
			board := statsJSON(t, a.rig, "/v1/stats/credit")
			total, _ := board["total"].(map[string]any)
			nodes, _ := board["nodes"].(map[string]any)
			if total["credit"] != float64(0) || nodes["with_credit"] != float64(0) {
				t.Errorf("credit counted for a node without an account: %v", board)
			}
		})
	}
}

// A node that computes like another model is held after its first check and never serves.
func TestAnonWrongModelHeld(t *testing.T) {
	a, _ := anonRig(t, false, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r, err := anonClient(a, "").Register(ctx, true)
	if err != nil {
		t.Fatal(err)
	}
	ref, secret := bothSets(t)
	eng := &mcEngine{ref: ref, skew: true}
	srv := httptest.NewServer(eng)
	t.Cleanup(srv.Close)
	a.startTokenNode(ctx, r.Token, srv, 1, 32768)
	c := anonClient(a, r.Token)
	s := waitSelf(t, c, protocol.SelfHeld)
	if strings.Contains(s.Message, "KL") || strings.Contains(s.Message, "flip") || strings.Contains(s.Message, "admit:") {
		t.Errorf("the node is told which check failed: %q", s.Message)
	}
	// It is told why in general terms, what to do, and when it is deleted.
	for _, want := range []string{"Failed the model check before admission", "curl -fsSL https://site.test/node.sh | sh", "deleted at "} {
		if !strings.Contains(s.Message, want) {
			t.Errorf("held message lacks %q: %q", want, s.Message)
		}
	}
	if s.DeleteAt == nil || time.Until(*s.DeleteAt) < 23*time.Hour || time.Until(*s.DeleteAt) > 25*time.Hour {
		t.Errorf("delete_at %v", s.DeleteAt)
	}
	resp := a.do(t, "POST", "/v1/chat/completions", projectKey, chatBody)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode == 200 || eng.people.Load() != 0 {
		t.Fatalf("a held node served science: %d, %d requests", resp.StatusCode, eng.people.Load())
	}
	for _, p := range probeTexts(eng) {
		if secret[p] {
			t.Fatalf("a node never admitted got a secret-set prompt: %q", p)
		}
	}
	// Held, it cannot be linked to an account to get out.
	if _, err := c.Start(ctx, protocol.LinkInfo{}); err == nil || !strings.Contains(err.Error(), "held") {
		t.Fatalf("link of a held node: %v", err)
	}
}

// A node without an account keeps the guard's stop and hold (an account's node is only flagged,
// TestGuardFlagsOnAccountNodes): an answer reading a credential file is stopped, nothing of it
// reaches the client, and the node is held.
func TestAnonNodeGuardStopsAndHolds(t *testing.T) {
	a, _ := anonRig(t, false, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r, err := anonClient(a, "").Register(ctx, true)
	if err != nil {
		t.Fatal(err)
	}
	ref, _ := bothSets(t)
	eng := &mcEngine{ref: ref, toolCmd: `cat ~/.ssh/id_example`}
	srv := httptest.NewServer(eng)
	t.Cleanup(srv.Close)
	a.startTokenNode(ctx, r.Token, srv, 1, 32768)
	c := anonClient(a, r.Token)
	waitSelf(t, c, protocol.SelfAdmitted)
	resp := a.do(t, "POST", "/v1/chat/completions", projectKey, chatBody)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 502 || strings.Contains(string(body), ".ssh") || !strings.Contains(string(body), "safety check") {
		t.Fatalf("science answer reading ~/.ssh: %d %s", resp.StatusCode, body)
	}
	waitSelf(t, c, protocol.SelfHeld)
	a.store.Refresh()
	held := false
	for _, p := range a.store.Snapshot().NodeHolds {
		held = held || (p.Node == r.NodeID && p.Rule == "credential-path")
	}
	if !held || eng.people.Load() != 1 {
		t.Fatalf("hold recorded %v, %d requests", held, eng.people.Load())
	}
}

// Registration limits: the terms, 3 per address a day, the pool, the switch, and no model check.
func TestAnonRegisterLimits(t *testing.T) {
	a, clk := anonRig(t, false, nil)
	ctx := context.Background()
	reg := anonClient(a, "")
	for i := 0; i < 3; i++ {
		if _, err := reg.Register(ctx, true); err != nil {
			t.Fatalf("registration %d: %v", i+1, err)
		}
	}
	if _, err := reg.Register(ctx, true); err == nil || !strings.Contains(err.Error(), "too many nodes registered from this address") {
		t.Fatalf("a 4th from one address: %v", err)
	}
	clk.add(24 * time.Hour)
	a.store.SetMaxAnonNodes(4)
	if _, err := reg.Register(ctx, true); err != nil {
		t.Fatalf("a day later: %v", err)
	}
	if _, err := reg.Register(ctx, true); err == nil || !strings.Contains(err.Error(), "pool for nodes without an account is full") {
		t.Fatalf("over the pool: %v", err)
	}
	// A full pool takes nothing from the address's count: with room again, it registers.
	a.store.SetMaxAnonNodes(5)
	a.store.SetAnonClosed(true)
	if _, err := reg.Register(ctx, true); err == nil || !strings.Contains(err.Error(), "not being accepted") {
		t.Fatalf("anon_closed: %v", err)
	}
	a.store.SetAnonClosed(false)
	if _, err := reg.Register(ctx, true); err != nil {
		t.Fatalf("open again: %v", err)
	}

	// Without the model check there is no way to admit one: registering is refused.
	b, _ := anonRig(t, false, func(c *gateway.Config) { c.ModelCheck = nil })
	if _, err := anonClient(b, "").Register(ctx, true); err == nil || !strings.Contains(err.Error(), "model check") {
		t.Fatalf("without the model check: %v", err)
	}
}

// Linking a node without an account: it sends its token, the page says what it is, approving gives the
// account a new node of its own (linked, so not its own until confirmed), and the identity without an
// account is gone once the node picked up its token. Another token, or none valid, is refused.
func TestAnonLinkToAccount(t *testing.T) {
	stats := filepath.Join(t.TempDir(), "stats.json")
	today := time.Now().UTC().Format(time.DateOnly)
	os.WriteFile(stats, []byte(`{"host":"site.test","updated":"2026-11-25T10:00:00Z","days":{"`+today+`":{"visitors":7,"pages":{"/node/":3},"installs":2,"install_fetches":{"curl":2}}}}`), 0o644)
	a, _ := anonRig(t, false, func(c *gateway.Config) { c.SiteStatsFile = stats })
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	r, err := anonClient(a, "").Register(ctx, true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := anonClient(a, "nt-a-00000000."+strings.Repeat("x", 43)).Start(ctx, protocol.LinkInfo{}); err == nil {
		t.Fatal("a link with an unknown token was started")
	}
	_, acctToken, _ := a.operator(t, 5)
	if _, err := anonClient(a, acctToken).Start(ctx, protocol.LinkInfo{}); err == nil {
		t.Fatal("a link with an account's node token was started")
	}
	c := anonClient(a, r.Token)
	st, err := c.Start(ctx, protocol.LinkInfo{Name: "box"})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan protocol.LinkPoll, 1)
	go func() { p, _ := c.Wait(ctx, st); done <- p }()
	session, _ := a.signIn(t, 42)
	_, page := a.get(t, "/link?code="+st.UserCode, session)
	for _, want := range []string{"running without an account", r.NodeID, "earns no", "its credit starts now"} {
		if !strings.Contains(page, want) {
			t.Errorf("approval page lacks %q", want)
		}
	}
	f := linkForm(t, page)
	f.Set("code", st.UserCode)
	f.Set("action", "approve")
	if _, page := a.post(t, "/link", session, f.Encode()); !strings.Contains(page, "Approved.") {
		t.Fatalf("approve: %s", page)
	}
	p := <-done
	if p.Status != protocol.LinkApproved || !strings.HasPrefix(p.NodeID, "n-") {
		t.Fatalf("poll: %+v", p)
	}
	a.store.Refresh()
	for _, x := range a.store.Snapshot().Accounts {
		if strings.HasPrefix(x.ID, "ax:") {
			t.Fatalf("the identity without an account is still there: %+v", x)
		}
	}
	acct, _, _ := a.store.Get("gh:42")
	if len(acct.Nodes) != 1 || acct.Nodes[0].ID != p.NodeID || acct.Nodes[0].Via != gateway.ViaNodeLink || acct.Nodes[0].Confirmed != nil {
		t.Fatalf("the account's nodes: %+v", acct.Nodes)
	}
	if _, err := anonClient(a, r.Token).Self(ctx); err == nil {
		t.Fatal("the old token still works")
	}
	req, _ := http.NewRequest("GET", a.gw.URL+protocol.SelfPath, nil)
	req.Header.Set("Authorization", "Bearer "+p.Token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("the new token: HTTP %d", resp.StatusCode)
	}

	// The ops page's funnel card counts it: registered without an account, a link approval, linked (the
	// operator's node above is the other node added to an account); and
	// the site's visitors and install starts from the statistics file. Numbers only.
	admin, _ := a.signIn(t, 1)
	_, ops := a.get(t, "/account/ops", admin)
	i := strings.Index(ops, `data-funnel="`+today+`"`)
	if i < 0 {
		t.Fatalf("no funnel row for today: %s", ops)
	}
	row := ops[i : i+strings.Index(ops[i:], "</tr>")]
	for _, want := range []string{`data-anon>1<`, `data-account>2<`, `data-links>1<`, `data-anon-linked>1<`, `data-first>0<`, `data-installs>2<`, `>7<`, `>3<`} {
		if !strings.Contains(row, want) {
			t.Errorf("today's funnel row lacks %s: %s", want, row)
		}
	}
	if strings.Contains(row, r.NodeID) || strings.Contains(row, p.NodeID) {
		t.Errorf("the funnel row names a node: %s", row)
	}
}

// A node without an account announces one model: the one its admission checks.
func TestAnonOneModel(t *testing.T) {
	a, _ := anonRig(t, false, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r, err := anonClient(a, "").Register(ctx, true)
	if err != nil {
		t.Fatal(err)
	}
	eng := mockEngine(t, false)
	a.startNodeCfg(ctx, node.Config{Token: r.Token, Upstream: eng.URL, Models: []protocol.ModelInfo{
		{Name: mcModel, Roles: []string{"deep"}, Slots: 1, Ctx: 32768}, {Name: "other-model", Roles: []string{"fast"}, Slots: 1, Ctx: 32768}}})
	time.Sleep(time.Second)
	if n := a.g.NodeCount(); n != 0 {
		t.Fatalf("a node without an account with two models connected (%d nodes)", n)
	}
	a.startTokenNode(ctx, r.Token, eng, 1, 32768)
	a.waitNodes(t, 1)
}
