// SPDX-License-Identifier: Apache-2.0
package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/aisathome/aisathome/protocol"
)

const (
	testConnectorKey  = "ak-connector-test-key-0123456789abcdefghij"
	otherConnectorKey = "ak-other-connector-key-0123456789abcdefghi"
	pilotsPath        = "/v1/connectors/boinc/pilots"
)

// pilotRig is a gateway with accounts, two projects (lab: agents on volunteer nodes, pilots in
// probation allowed; other: no agents on volunteers) and two connectors, on a clock of its own.
type pilotRig struct {
	t   *testing.T
	g   *Gateway
	mu  sync.Mutex
	now time.Time
}

func (r *pilotRig) clock() time.Time { r.mu.Lock(); defer r.mu.Unlock(); return r.now }
func (r *pilotRig) advance(d time.Duration) {
	r.mu.Lock()
	r.now = r.now.Add(d)
	r.mu.Unlock()
}

func testConnectors() []Connector {
	return []Connector{
		{ID: "boinc", KeySHA256: []string{keyHash(testConnectorKey)}, MaxPilots: 4, MintPerHour: 6,
			NoChatUsers: []string{"900"}, TrustedUsers: []string{"1"}, MaxCreditPerPilot: 10, MaxCreditPerAccountDay: 15},
		{ID: "other", KeySHA256: []string{keyHash(otherConnectorKey)}, MaxPilots: 1, AllowFrom: []string{"203.0.113.0/24"}},
	}
}

func newPilotRig(t *testing.T, mod func(*Config)) *pilotRig {
	t.Helper()
	dir := t.TempDir()
	r := &pilotRig{t: t, now: time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)}
	cfg := Config{StateDir: filepath.Join(dir, "state"),
		Accounts: &AccountsConfig{File: filepath.Join(dir, "accounts.json"), PublicURL: "https://site.test",
			SessionSecret: strings.Repeat("s", 32), GitHubClientID: "id", GitHubClientSecret: "secret", NodeProbation: "168h"},
		Projects: []Project{
			{ID: "lab", KeySHA256: []string{keyHash(testProjectKey)}, AgentOK: true, PilotProbationOK: true, Kinds: []string{"p2m-v7"}},
			{ID: "other", KeySHA256: []string{keyHash(otherProjectKey)}},
		},
		Connectors: testConnectors(),
		PilotsNow:  r.clock, TasksNow: r.clock}
	if mod != nil {
		mod(&cfg)
	}
	r.g = newTestGateway(t, cfg)
	t.Cleanup(func() { r.g.Close() })
	return r
}

// call runs one request from addr (default a public address).
func (r *pilotRig) call(method, path, key string, body any, addr string) (*httptest.ResponseRecorder, map[string]any) {
	r.t.Helper()
	var rd *bytes.Reader
	if body == nil {
		rd = bytes.NewReader(nil)
	} else {
		rd = bytes.NewReader(statusJSON(body))
	}
	req := httptest.NewRequest(method, path, rd)
	req.RemoteAddr = "198.51.100.7:4000"
	if addr != "" {
		req.RemoteAddr = addr
	}
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.g.Handler().ServeHTTP(w, req)
	var out map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return w, out
}

func (r *pilotRig) want(code int, method, path, key string, body any) map[string]any {
	r.t.Helper()
	w, out := r.call(method, path, key, body, "")
	if w.Code != code {
		r.t.Fatalf("%s %s: HTTP %d, want %d: %s", method, path, w.Code, code, w.Body)
	}
	return out
}

// create makes a pilot with a deadline in 2 days; it returns its id and credential.
func (r *pilotRig) create() (string, string) {
	r.t.Helper()
	out := r.want(201, "POST", pilotsPath, testConnectorKey, map[string]any{"deadline": r.clock().Add(48 * time.Hour)})
	p := out["pilot"].(map[string]any)
	return p["id"].(string), out["credential"].(string)
}

func (r *pilotRig) bind(id, user string) map[string]any {
	r.t.Helper()
	return r.want(200, "POST", pilotsPath+"/"+id+"/bind", testConnectorKey, map[string]any{"user": user, "host": "h" + user, "team": "7"})
}

// The connector key opens only its own connector's endpoints, from its allowed addresses; no other
// key opens them.
func TestConnectorAuth(t *testing.T) {
	r := newPilotRig(t, nil)
	demand := "/v1/connectors/boinc/demand"
	for _, key := range []string{"", "nonsense", otherConnectorKey, testProjectKey} {
		if w, _ := r.call("GET", demand, key, nil, ""); w.Code != 401 {
			t.Errorf("key %q: HTTP %d, want 401", key, w.Code)
		}
	}
	r.want(200, "GET", demand, testConnectorKey, nil)
	if w, _ := r.call("GET", "/v1/connectors/nope/demand", testConnectorKey, nil, ""); w.Code != 401 {
		t.Errorf("unknown connector: HTTP %d", w.Code)
	}
	// The connector key is no API or project key.
	for _, path := range []string{"/v1/projects/lab/tasks", "/v1/models", "/v1/account/settings"} {
		if w, _ := r.call("GET", path, testConnectorKey, nil, ""); w.Code != 401 {
			t.Errorf("connector key on %s: HTTP %d, want 401", path, w.Code)
		}
	}
	if w, _ := r.call("POST", "/v1/chat/completions", testConnectorKey, map[string]any{"model": "x", "messages": []any{}}, ""); w.Code != 401 {
		t.Errorf("connector key on chat: HTTP %d, want 401", w.Code)
	}
	// allow_from: "other" takes calls only from 203.0.113.0/24, also through a local proxy.
	od := "/v1/connectors/other/demand"
	if w, _ := r.call("GET", od, otherConnectorKey, nil, ""); w.Code != 403 {
		t.Errorf("other from a public address outside allow_from: HTTP %d, want 403", w.Code)
	}
	if w, _ := r.call("GET", od, otherConnectorKey, nil, "203.0.113.9:1"); w.Code != 200 {
		t.Errorf("other from allow_from: HTTP %d", w.Code)
	}
	req := httptest.NewRequest("GET", od, nil)
	req.RemoteAddr, req.Header = "127.0.0.1:5", http.Header{"Authorization": {"Bearer " + otherConnectorKey}, "X-Forwarded-For": {"203.0.113.20"}}
	w := httptest.NewRecorder()
	r.g.Handler().ServeHTTP(w, req)
	if w.Code != 200 {
		t.Errorf("other through a local proxy from allow_from: HTTP %d", w.Code)
	}
	req.Header.Set("X-Forwarded-For", "198.51.100.1")
	w = httptest.NewRecorder()
	r.g.Handler().ServeHTTP(w, req)
	if w.Code != 403 {
		t.Errorf("other through a local proxy from outside: HTTP %d", w.Code)
	}
	// One connector's key does not open another's pilots.
	id, _ := r.create()
	if w, _ := r.call("GET", "/v1/connectors/other/pilots/"+id, otherConnectorKey, nil, "203.0.113.9:1"); w.Code != 404 {
		t.Errorf("another connector's pilot: HTTP %d, want 404", w.Code)
	}
}

func TestConnectorConfigChecked(t *testing.T) {
	p := []Project{{ID: "lab", KeySHA256: []string{keyHash(testProjectKey)}}}
	pt, err := newProjectTable(p)
	if err != nil {
		t.Fatal(err)
	}
	ok := Connector{ID: "boinc", KeySHA256: []string{keyHash(testConnectorKey)}}
	for name, c := range map[string]Connector{
		"bad id":           {ID: "BOINC", KeySHA256: ok.KeySHA256},
		"no key":           {ID: "boinc"},
		"short hash":       {ID: "boinc", KeySHA256: []string{"abc"}},
		"a project's key":  {ID: "boinc", KeySHA256: []string{keyHash(testProjectKey)}},
		"bad allow_from":   {ID: "boinc", KeySHA256: ok.KeySHA256, AllowFrom: []string{"example.org"}},
		"negative":         {ID: "boinc", KeySHA256: ok.KeySHA256, MaxPilots: -1},
		"email as user":    {ID: "boinc", KeySHA256: ok.KeySHA256, NoChatUsers: []string{"a@b.org"}},
		"bad credit cap":   {ID: "boinc", KeySHA256: ok.KeySHA256, MaxCreditPerPilot: -2},
		"nan credit cap":   {ID: "boinc", KeySHA256: ok.KeySHA256, MaxCreditPerAccountDay: math.NaN()},
		"twice, same id":   {ID: "boinc", KeySHA256: []string{keyHash("ak-x")}},
		"twice, same hash": {ID: "b2", KeySHA256: ok.KeySHA256},
	} {
		cs := []Connector{c}
		if strings.HasPrefix(name, "twice") {
			cs = []Connector{ok, c}
		}
		if _, err := newConnectorTable(cs, pt); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, err := newConnectorTable([]Connector{ok}, pt); err != nil {
		t.Errorf("valid connector refused: %v", err)
	}
	// Connectors need accounts.
	if _, err := New(Config{Connectors: []Connector{ok}}, nil); err == nil {
		t.Error("connectors without accounts accepted")
	}
}

// Pilots are created within max_pilots and mint_per_hour, with a deadline in range; the credential
// is shown once and only its hash is stored.
func TestPilotCreate(t *testing.T) {
	r := newPilotRig(t, nil)
	for _, d := range []any{nil, "soon", r.clock().Add(-time.Minute), r.clock().Add(15 * 24 * time.Hour)} {
		if w, _ := r.call("POST", pilotsPath, testConnectorKey, map[string]any{"deadline": d}, ""); w.Code != 400 {
			t.Errorf("deadline %v: HTTP %d, want 400", d, w.Code)
		}
	}
	if w, _ := r.call("POST", pilotsPath, testConnectorKey, map[string]any{"deadline": r.clock().Add(time.Hour), "x": 1}, ""); w.Code != 400 {
		t.Errorf("unknown field: HTTP %d", w.Code)
	}
	id, cred := r.create()
	if !validPilotID(id) {
		t.Fatalf("pilot id %q", id)
	}
	if got, ok := splitPilotCredential(cred); !ok || got != id {
		t.Fatalf("credential %q does not carry %s", cred[:16], id)
	}
	var stored string
	r.g.sci.db.QueryRow(`SELECT cred_sha256 FROM pilots WHERE id = ?`, id).Scan(&stored)
	if stored != keyHash(cred) {
		t.Error("the credential's hash is not what is stored")
	}
	var leaks int
	r.g.sci.db.QueryRow(`SELECT COUNT(*) FROM pilots WHERE cred_sha256 LIKE ? OR id LIKE ?`, "%"+cred[16:30]+"%", "%"+cred[16:30]+"%").Scan(&leaks)
	if leaks != 0 {
		t.Error("the credential's secret is stored")
	}
	get := r.want(200, "GET", pilotsPath+"/"+id, testConnectorKey, nil)
	if strings.Contains(statusText(get), cred) || get["credential"] != nil {
		t.Error("a pilot read shows its credential")
	}
	if p := get["pilot"].(map[string]any); p["state"] != PilotCreated {
		t.Errorf("state %v", p["state"])
	}
	ids := []string{id}
	for i := 0; i < 3; i++ {
		x, _ := r.create()
		ids = append(ids, x)
	}
	over := func(code int) {
		t.Helper()
		if w, _ := r.call("POST", pilotsPath, testConnectorKey, map[string]any{"deadline": r.clock().Add(time.Hour)}, ""); w.Code != code {
			t.Fatalf("HTTP %d, want %d: %s", w.Code, code, w.Body)
		}
	}
	over(409) // max_pilots 4: a fifth waits until one ends
	report := func(id string) {
		r.want(200, "POST", pilotsPath+"/"+id+"/reported", testConnectorKey, map[string]any{"outcome": "cancelled"})
	}
	report(ids[0])
	r.create() // the fifth this hour
	report(ids[1])
	r.create() // the sixth: mint_per_hour 6
	report(ids[2])
	over(429)
	r.advance(61 * time.Minute)
	r.create()
	list := r.want(200, "GET", pilotsPath+"?state=created", testConnectorKey, nil)
	if n := len(list["pilots"].([]any)); n != 4 {
		t.Errorf("created pilots listed: %d, want 4", n)
	}
	r.want(400, "GET", pilotsPath+"?state=gone", testConnectorKey, nil)
}

func statusText(v any) string { return string(statusJSON(v)) }

// A bind names the connector's user, host and team (ids, never addresses); the pilot then belongs
// to the account linked to that user, or to an unclaimed account made for it, the same for every
// pilot of that user.
func TestPilotBind(t *testing.T) {
	r := newPilotRig(t, nil)
	a, _ := r.create()
	b, _ := r.create()
	for _, body := range []map[string]any{{"user": "a@b.org", "host": "1"}, {"user": "5"}, {"user": "5", "host": "1", "team": "x y"}} {
		if w, _ := r.call("POST", pilotsPath+"/"+a+"/bind", testConnectorKey, body, ""); w.Code != 400 {
			t.Errorf("bind %v: HTTP %d, want 400", body, w.Code)
		}
	}
	p := r.bind(a, "5")["pilot"].(map[string]any)
	if p["state"] != PilotBound || p["user"] != "5" || p["account"] != "unclaimed" {
		t.Fatalf("bound: %v", p)
	}
	r.bind(a, "5") // again, the same: no change
	if w, _ := r.call("POST", pilotsPath+"/"+a+"/bind", testConnectorKey, map[string]any{"user": "6", "host": "h6"}, ""); w.Code != 409 {
		t.Errorf("rebind to another user: HTTP %d, want 409", w.Code)
	}
	r.bind(b, "5")
	pa, _ := r.g.sci.pilot("boinc", a)
	pb, _ := r.g.sci.pilot("boinc", b)
	if pa.account == "" || pa.account != pb.account {
		t.Fatalf("two pilots of one user: accounts %q and %q", pa.account, pb.account)
	}
	acct, _, _ := r.g.accounts.Get(pa.account)
	if acct.Status != StatusUnclaimed || !strings.HasPrefix(acct.ID, "cx:") || acct.FirstNode == nil ||
		len(acct.Links) != 1 || acct.Links[0].Connector != "boinc" || acct.Links[0].User != "5" {
		t.Fatalf("unclaimed account: %+v", acct)
	}
	if strings.Contains(acct.ID, "5") && len(acct.ID) < 10 {
		t.Error("the unclaimed account's id is derived from the user id")
	}
	// A linked account gets its user's pilots.
	r.g.accounts.Update(func(f *AccountsFile) error {
		f.Accounts = append(f.Accounts, &Account{ID: "gh:42", Status: StatusActive, Created: r.clock()})
		return nil
	})
	if err := r.g.accounts.Link("gh:42", "boinc", "42", r.clock()); err != nil {
		t.Fatal(err)
	}
	if err := r.g.accounts.Link("gh:42", "boinc", "5", r.clock()); err != errLinkTaken {
		t.Errorf("linking a user held by an unclaimed account: %v", err)
	}
	c, _ := r.create()
	if p := r.bind(c, "42")["pilot"].(map[string]any); p["account"] != "linked" {
		t.Errorf("linked user's pilot: account %v", p["account"])
	}
	pc, _ := r.g.sci.pilot("boinc", c)
	if pc.account != "gh:42" {
		t.Errorf("linked user's pilot belongs to %q", pc.account)
	}
	// An ended pilot cannot be bound; an unknown one is 404.
	d, _ := r.create()
	r.want(200, "POST", pilotsPath+"/"+d+"/reported", testConnectorKey, map[string]any{"outcome": "error"})
	if w, _ := r.call("POST", pilotsPath+"/"+d+"/bind", testConnectorKey, map[string]any{"user": "8", "host": "h"}, ""); w.Code != 409 {
		t.Errorf("bind of an ended pilot: HTTP %d", w.Code)
	}
	if w, _ := r.call("POST", pilotsPath+"/p-000000000000/bind", testConnectorKey, map[string]any{"user": "8", "host": "h"}, ""); w.Code != 404 {
		t.Errorf("unknown pilot: HTTP %d", w.Code)
	}
	// No address anywhere in the accounts file.
	if raw := statusText(r.g.accounts.Snapshot()); strings.Contains(raw, "@") {
		t.Error("an address in the accounts file")
	}
}

// pilotNode is a connected pilot as authPilot and handleNodeConnect make it.
func (r *pilotRig) pilotNode(cred string, slots int) *nodeConn {
	r.t.Helper()
	pl, status, why := r.g.authPilot(cred)
	if pl == nil {
		r.t.Fatalf("authPilot: %d %s", status, why)
	}
	n := &nodeConn{id: pl.pilot.ID, operator: pl.tok.Operator, token: cred, trusted: pl.tok.Trust == "trusted", account: pl.account,
		probEnd: pl.first.Add(r.g.acfg.probation), pilot: pl.pilot.ID, connector: pl.conn.ID, extUser: pl.pilot.User,
		deadline: pl.pilot.Deadline, scienceOnly: pl.scienceOnly, closed: make(chan struct{}),
		models: []protocol.ModelInfo{{Name: "m", Roles: []string{"deep"}, Slots: slots}}}
	r.g.mu.Lock()
	r.g.node[n.id] = n
	r.g.mu.Unlock()
	return n
}

// A pilot connects only once bound, before it ends or its deadline passes, while its account may run
// pilots.
func TestPilotCredentialRules(t *testing.T) {
	r := newPilotRig(t, nil)
	id, cred := r.create()
	if _, status, why := r.g.authPilot(cred); status != http.StatusConflict || !strings.Contains(why, "not bound") {
		t.Errorf("before the bind: %d %s", status, why)
	}
	r.bind(id, "5")
	if pl, _, _ := r.g.authPilot(cred); pl == nil || pl.tok.NodeID != id || pl.tok.Trust != "" {
		t.Fatalf("after the bind: %+v", pl)
	}
	for _, bad := range []string{"", "pc-", cred[:len(cred)-1] + "x", strings.Replace(cred, "pc-", "nt-", 1), "pc-000000000000." + strings.Repeat("a", 43)} {
		if pl, _, _ := r.g.authPilot(bad); pl != nil {
			t.Errorf("credential %q accepted", bad)
		}
	}
	// Ban the account: its pilots may not connect.
	p, _ := r.g.sci.pilot("boinc", id)
	r.g.accounts.SetStatus(p.account, StatusBanned)
	if pl, status, _ := r.g.authPilot(cred); pl != nil || status != http.StatusForbidden {
		t.Errorf("banned account's pilot: %d", status)
	}
	r.g.accounts.SetStatus(p.account, StatusUnclaimed)
	// Unlinked from its user (the operator, or a claim moving the link): its pilots stop.
	if err := r.g.accounts.Unlink(p.account, "boinc", "5"); err != nil {
		t.Fatal(err)
	}
	if pl, status, _ := r.g.authPilot(cred); pl != nil || status != http.StatusForbidden {
		t.Errorf("unlinked user's pilot: %d", status)
	}
	if err := r.g.accounts.Link(p.account, "boinc", "5", r.clock()); err != nil {
		t.Fatal(err)
	}
	if pl, _, _ := r.g.authPilot(cred); pl == nil {
		t.Error("relinked: refused")
	}
	// After its deadline.
	r.advance(49 * time.Hour)
	if pl, _, why := r.g.authPilot(cred); pl != nil || !strings.Contains(why, "ended") {
		t.Errorf("after the deadline: %s", why)
	}
	r.g.sweepPilots()
	if p, _ := r.g.sci.pilot("boinc", id); p.State != PilotEnded || p.EndReason != "deadline" {
		t.Errorf("past its deadline: %s %s", p.State, p.EndReason)
	}
	// After its end.
	id2, cred2 := r.create()
	r.bind(id2, "5")
	r.want(200, "POST", pilotsPath+"/"+id2+"/reported", testConnectorKey, map[string]any{"outcome": "success"})
	if pl, _, _ := r.g.authPilot(cred2); pl != nil {
		t.Error("an ended pilot's credential accepted")
	}
	// A trusted_users user's pilot serves chat before pilots_chat_enabled, but is never trusted: a copy
	// of its credential must not receive personal data.
	id3, cred3 := r.create()
	r.bind(id3, "1")
	pl, _, _ := r.g.authPilot(cred3)
	if pl == nil || pl.tok.Trust != "" || pl.scienceOnly {
		t.Fatalf("trusted user's pilot: %+v", pl)
	}
	n := r.pilotNode(cred3, 1)
	if (&route{class: ClassChat, trusted: true, pii: "email"}).allows(n, r.clock()) {
		t.Error("personal data reached a trusted user's pilot")
	}
	if !(&route{class: ClassChat}).allows(n, r.clock()) {
		t.Error("a trusted user's pilot does not serve chat")
	}
}

// What a pilot may serve: science always; chat and agents only with chat enabled for its connector,
// its account's switch on, and its user not in no_chat_users; personal data never (untrusted); tools
// past probation or for a project with pilot_probation_ok.
func TestPilotRouting(t *testing.T) {
	now := time.Now()
	pilot := &nodeConn{id: "p-000000000001", operator: "acct-x", account: "cx:1", pilot: "p-000000000001", probEnd: now.Add(time.Hour),
		deadline: now.Add(time.Hour)}
	sci := &route{class: ClassScience, project: "lab"}
	chat := &route{class: ClassChat}
	agent := &route{class: ClassAgent, tools: true, consent: true}
	sciTools := &route{class: ClassScience, tools: true, consent: true, project: "lab"}
	sciToolsOK := &route{class: ClassScience, tools: true, consent: true, project: "lab", pilotProbationOK: true}
	pii := &route{class: ClassChat, trusted: true, pii: "email"}
	check := func(what string, rt *route, want bool) {
		t.Helper()
		if got := rt.allows(pilot, now); got != want {
			t.Errorf("%s: allows %v, want %v", what, got, want)
		}
	}
	pilot.scienceOnly = true
	check("science-only pilot, science", sci, true)
	check("science-only pilot, chat", chat, false)
	check("science-only pilot, agent", agent, false)
	check("science-only pilot, personal data", pii, false)
	pilot.scienceOnly = false
	check("chat pilot, chat", chat, true)
	check("chat pilot, personal data", pii, false)
	check("pilot in probation, agent", agent, false)
	check("pilot in probation, science tools", sciTools, false)
	check("pilot in probation, science tools with pilot_probation_ok", sciToolsOK, true)
	node := &nodeConn{id: "n-00000001", operator: "acct-y", account: "gh:1", probEnd: now.Add(time.Hour)}
	if sciToolsOK.allows(node, now) {
		t.Error("pilot_probation_ok let a standalone node in probation run tools")
	}
	pilot.probEnd = now.Add(-time.Second)
	check("pilot past probation, agent with consent", agent, true)
	check("pilot past probation, agent without consent", &route{class: ClassAgent, tools: true}, false)
	// The own-node exemption is not for pilots: the connector made the link, not the person.
	own := &route{class: ClassChat, trusted: true, pii: "email", self: pilot.operator}
	check("own-account pilot, personal data", own, false)
	check("own-account pilot, only my own nodes", &route{class: ClassChat, self: pilot.operator, onlySelf: true}, false)
	ownNode := &nodeConn{id: "n-00000002", operator: pilot.operator, account: "cx:1", probEnd: now}
	if !own.allows(ownNode, now) {
		t.Error("an account's own standalone node lost its exemption")
	}
	// Past its deadline a pilot gets nothing, before the sweep drops it.
	pilot.deadline = now.Add(-time.Second)
	check("pilot past its deadline, science", sci, false)
	pilot.deadline = time.Time{}
	check("pilot without a deadline, science", sci, false)
	pilot.deadline = now.Add(time.Hour)

	// The switches: no_chat_users wins over all; the account's switch; pilots_chat_enabled unless trusted.
	c := &connector{Connector: Connector{PilotsChatEnabled: true}, noChat: map[string]bool{"900": true}, trusted: map[string]bool{}}
	for _, tc := range []struct {
		user             string
		chatOff, trusted bool
		enabled, want    bool
	}{
		{"5", false, false, true, false},
		{"5", true, false, true, true},
		{"900", false, false, true, true},
		{"900", false, true, true, true},
		{"5", false, false, false, true},
		{"5", false, true, false, false},
		{"5", true, true, false, true},
	} {
		c.PilotsChatEnabled = tc.enabled
		if got := pilotScienceOnly(c, tc.user, tc.chatOff, tc.trusted); got != tc.want {
			t.Errorf("%+v: science only %v", tc, got)
		}
	}
}

// sweepNodes applies a change of the account's switch or of the connector's settings to a connected
// pilot within one sweep (a banned account's pilots: TestPilotCredentialRules and the e2e).
func TestPilotSweep(t *testing.T) {
	r := newPilotRig(t, func(c *Config) { c.Connectors[0].PilotsChatEnabled = true })
	id, cred := r.create()
	r.bind(id, "5")
	n := r.pilotNode(cred, 1)
	if n.scienceOnly {
		t.Fatal("chat enabled, switch on: science only")
	}
	p, _ := r.g.sci.pilot("boinc", id)
	r.g.accounts.SetPilotChat(p.account, false)
	r.g.sweepNodes()
	r.g.mu.Lock()
	so := n.scienceOnly
	r.g.mu.Unlock()
	if !so {
		t.Fatal("switch off: the pilot still serves chat")
	}
	r.g.accounts.SetPilotChat(p.account, true)
	cfg := r.g.cfg
	cfg.Connectors = testConnectors() // pilots_chat_enabled false again
	if err := r.g.Reload(cfg); err != nil {
		t.Fatal(err)
	}
	r.g.sweepNodes()
	r.g.mu.Lock()
	so = n.scienceOnly
	r.g.mu.Unlock()
	if !so {
		t.Fatal("pilots_chat_enabled off: the pilot still serves chat")
	}
}

// The ledger rules for pilots: credit by the formula into the pilot's record only, nothing for
// self-served or failed answers, the per-pilot and per-account-day caps; final 24 h after the last
// disconnect once ended; 0 when withheld; never in the public ledger.
func TestPilotCredit(t *testing.T) {
	r := newPilotRig(t, nil)
	id, cred := r.create()
	r.bind(id, "5")
	n := r.pilotNode(cred, 1)
	served := func(outcome string, self bool, credit float64) {
		rec := &usageRecord{Time: r.clock(), Outcome: outcome, SelfServed: self, Credit: credit,
			Tokens: &creditTokens{Prompt: 100, Cached: 10, Completion: 20}}
		r.g.pilotServed(n, rec)
	}
	served("ok", false, 3.5)
	served("ok", true, 3.5) // self-served: no credit
	served("error", false, 3.5)
	served(outcomeBlocked, false, 3.5)
	served("cancelled", false, 1.25)
	p, _ := r.g.sci.pilot("boinc", id)
	if p.Credit != 4.75 || p.Metered != 4.75 || p.Requests != 3 || p.Failed != 2 || p.SelfServed != 1 || p.Tokens.Completion != 100 {
		t.Fatalf("after five answers: credit %v metered %v requests %d failed %d self %d tokens %+v", p.Credit, p.Metered, p.Requests, p.Failed, p.SelfServed, p.Tokens)
	}
	served("ok", false, 9) // the pilot's cap is 10
	if p, _ = r.g.sci.pilot("boinc", id); p.Credit != 10 || p.Metered != 13.75 {
		t.Fatalf("capped per pilot: credit %v metered %v", p.Credit, p.Metered)
	}
	// The account's day cap (15) spans its pilots.
	id2, cred2 := r.create()
	r.bind(id2, "5")
	n2 := r.pilotNode(cred2, 1)
	r.g.pilotServed(n2, &usageRecord{Time: r.clock(), Outcome: "ok", Credit: 8, Tokens: &creditTokens{}})
	if p2, _ := r.g.sci.pilot("boinc", id2); p2.Credit != 5 {
		t.Fatalf("capped per account and day: %v", p2.Credit)
	}
	r.advance(24 * time.Hour) // a new UTC day
	r.g.pilotServed(n2, &usageRecord{Time: r.clock(), Outcome: "ok", Credit: 2, Tokens: &creditTokens{}})
	if p2, _ := r.g.sci.pilot("boinc", id2); p2.Credit != 7 {
		t.Fatalf("next day: %v", p2.Credit)
	}
	// Nothing in the public ledger.
	if e := r.g.credit.node(id); e != nil {
		t.Error("pilot credit in the public ledger (node)")
	}
	if e := r.g.credit.account(p.account); e != nil {
		t.Error("pilot credit in the public ledger (account)")
	}
	// Final only after its end and 24 h after its last disconnect.
	credit := func(pid string) map[string]any {
		return r.want(200, "GET", pilotsPath+"/"+pid+"/credit", testConnectorKey, nil)
	}
	if c := credit(id); c["final"] != false || c["credit"] != nil || c["final_at"] != nil {
		t.Fatalf("while bound: %v", c)
	}
	r.g.sci.pilotConnected(id, "", r.clock())
	since := r.clock()
	r.advance(time.Hour)
	r.g.sci.pilotDisconnected(id, "pilot: idle", since, r.clock())
	c := credit(id)
	if c["final"] != false || c["final_at"] == nil || c["state"] != PilotEnded {
		t.Fatalf("ended, in the hold: %v", c)
	}
	r.advance(pilotCreditHold - time.Minute)
	if c := credit(id); c["final"] != false {
		t.Fatalf("a minute before the hold ends: %v", c)
	}
	r.advance(time.Minute)
	if c := credit(id); c["final"] != true || c["credit"] != 10.0 {
		t.Fatalf("final: %v", c)
	}
	// Withheld (a guard held the pilot): final 0, with the reason.
	r.g.holdNode(n2, guardHit{rule: "download-and-execute", why: "x"}, "req")
	r.g.mu.Lock()
	delete(r.g.node, n.id) // test nodes have no connection to close
	delete(r.g.node, n2.id)
	r.g.mu.Unlock()
	r.want(200, "POST", pilotsPath+"/"+id2+"/reported", testConnectorKey, map[string]any{"outcome": "success"})
	r.advance(pilotCreditHold)
	if c := credit(id2); c["final"] != true || c["credit"] != 0.0 || c["withheld"] != "guard: download-and-execute" {
		t.Fatalf("withheld: %v", c)
	}
}

// The record carries a pilot's totals over every connection of its credential (a suspended and
// resumed work unit): "pilot: sigterm" leaves it disconnected, a final reason ends it;
// a gateway restart counts its serve time until it was last seen.
func TestPilotRecordAcrossRestarts(t *testing.T) {
	r := newPilotRig(t, nil)
	id, _ := r.create()
	r.bind(id, "5")
	db := r.g.sci
	if ok, _ := db.pilotConnected(id, "aisathome-node/0.6.0", r.clock()); !ok {
		t.Fatal("bound pilot may not connect")
	}
	t0 := r.clock()
	// A record left "connected" (its disconnect not written) may be continued by the next connection
	// (the gateway checks first that none is live).
	if ok, _ := db.pilotConnected(id, "aisathome-node/0.6.0", r.clock()); !ok {
		t.Fatal("a record stranded in connected cannot be continued")
	}
	db.db.Exec(`UPDATE pilots SET connections = 1 WHERE id = ?`, id)
	r.advance(10 * time.Minute)
	db.pilotDisconnected(id, "pilot: sigterm", t0, r.clock())
	p, _ := db.pilot("boinc", id)
	if p.State != PilotDisconnected || p.ServedSec != 600 || p.CloseReason != "pilot: sigterm" {
		t.Fatalf("after sigterm: %s %d %q", p.State, p.ServedSec, p.CloseReason)
	}
	r.advance(time.Minute)
	if ok, _ := db.pilotConnected(id, "aisathome-node/0.6.0", r.clock()); !ok {
		t.Fatal("resumed pilot may not connect")
	}
	t1 := r.clock()
	r.advance(5 * time.Minute)
	db.pilotsSeen([]string{id}, r.clock())
	r.advance(3 * time.Minute) // the gateway stops here, unseen
	if err := db.recoverPilots(); err != nil {
		t.Fatal(err)
	}
	p, _ = db.pilot("boinc", id)
	if p.State != PilotDisconnected || p.ServedSec != 900 || p.Connections != 2 || p.CloseReason != "gateway restart" {
		t.Fatalf("after a restart: %s %d s %d connections %q", p.State, p.ServedSec, p.Connections, p.CloseReason)
	}
	_ = t1
	db.pilotConnected(id, "", r.clock())
	t2 := r.clock()
	r.advance(20 * time.Minute)
	db.pilotDisconnected(id, "pilot: idle", t2, r.clock())
	p, _ = db.pilot("boinc", id)
	if p.State != PilotEnded || p.EndReason != "pilot: idle" || p.ServedSec != 2100 || p.Connections != 3 {
		t.Fatalf("after idle: %s %q %d s %d", p.State, p.EndReason, p.ServedSec, p.Connections)
	}
	if ok, _ := db.pilotConnected(id, "", r.clock()); ok {
		t.Error("an ended pilot connected again")
	}
}

// The demand formula (connector_demand.go).
func TestPilotDemand(t *testing.T) {
	r := newPilotRig(t, func(c *Config) {
		c.Connectors[0].MaxPilots, c.Connectors[0].MintPerHour, c.Connectors[0].UnsentBuffer = 6, 10, 3
		c.NodeTokens = map[string]NodeToken{"home-token": {Operator: "owner", Trust: "trusted"}}
	})
	demand := func() map[string]any { return r.want(200, "GET", "/v1/connectors/boinc/demand", testConnectorKey, nil) }
	num := func(m map[string]any, k string) int { return int(m[k].(float64)) }
	d := demand()
	if num(d, "pilots_wanted") != 0 || num(d, "target") != 0 {
		t.Fatalf("no science, no chat: %v", d)
	}
	// 9 open tasks of lab, 5 workers seen (claims), 3 of them holding a lease: 5 sessions.
	tr := &taskRig{t: t, g: r.g}
	for i := 0; i < 9; i++ {
		tr.call("POST", "/v1/projects/lab/tasks", testProjectKey, newTask("T"+string(rune('a'+i)), "p2m-v7"))
	}
	for i := 0; i < 5; i++ {
		if i < 3 {
			tr.call("POST", "/v1/projects/lab/tasks/claim", testProjectKey, map[string]any{"worker": "w" + string(rune('0'+i))})
		} else {
			r.g.tasks.mu.Lock()
			r.g.tasks.sawWorker("lab", "w"+string(rune('0'+i)), r.clock())
			r.g.tasks.mu.Unlock()
		}
	}
	// "other" has open tasks but no agents on volunteers: not counted.
	for i := 0; i < 4; i++ {
		tr.call("POST", "/v1/projects/other/tasks", otherProjectKey, newTask("O"+string(rune('a'+i)), "other"))
		r.g.tasks.mu.Lock()
		r.g.tasks.sawWorker("other", "x"+string(rune('0'+i)), r.clock())
		r.g.tasks.mu.Unlock()
	}
	d = demand()
	sci := d["science"].(map[string]any)
	if num(sci, "open_tasks") != 9 || num(sci, "runner_workers") != 5 || num(sci, "sessions") != 5 || num(sci, "pilots") != 3 {
		t.Fatalf("science: %v", sci)
	}
	// One trusted node with one slot online: 5 sessions - 2 = 3 → 2 pilots; buffer 3, none unsent.
	r.g.mu.Lock()
	r.g.node["home"] = &nodeConn{id: "home", token: "home-token", operator: "owner", trusted: true, models: []protocol.ModelInfo{{Name: "m", Slots: 1}}}
	r.g.mu.Unlock()
	d = demand()
	if num(d, "target") != 2 || num(d, "pilots_wanted") != 2 || num(d["science"].(map[string]any), "slots_online") != 1 {
		t.Fatalf("with one slot online: %v", d)
	}
	// Two created (unsent) pilots: wanted falls by them; with the buffer of 3, at most one more.
	r.create()
	r.create()
	if d = demand(); num(d, "pilots_wanted") != 0 || num(d["pilots"].(map[string]any), "outstanding") != 2 {
		t.Fatalf("two unsent pilots outstanding: %v", d)
	}
	// Chat headroom counts only while chat on pilots is enabled.
	cfg := r.g.cfg
	cfg.Connectors = testConnectors()
	cfg.Connectors[0].MaxPilots, cfg.Connectors[0].MintPerHour, cfg.Connectors[0].UnsentBuffer, cfg.Connectors[0].ChatHeadroom = 6, 10, 3, 2
	r.g.Reload(cfg)
	if d = demand(); num(d, "target") != 2 {
		t.Fatalf("headroom with chat off: %v", d)
	}
	cfg.Connectors[0].PilotsChatEnabled = true
	r.g.Reload(cfg)
	if d = demand(); num(d, "target") != 4 || num(d, "pilots_wanted") != 1 {
		t.Fatalf("headroom with chat on (target 4, 2 outstanding, buffer 3 - 2 unsent): %v", d)
	}
	// Bound pilots no longer count as unsent: the buffer frees.
	ps, _ := r.g.sci.pilots("boinc", PilotCreated, 10)
	r.bind(ps[0].ID, "5")
	if d = demand(); num(d, "pilots_wanted") != 2 {
		t.Fatalf("one bound, one unsent: %v", d)
	}
	// max_pilots and the spend cap bound the target; above it, unsent ones are to be cancelled.
	cfg.Connectors[0].MaxPilots = 3
	r.g.Reload(cfg)
	if d = demand(); num(d, "target") != 3 || d["capped_by"] != "max_pilots" || num(d, "pilots_wanted") != 1 {
		t.Fatalf("max_pilots 3: %v", d)
	}
	r.g.spendCap = func(c string) (int, bool) { return 1, c == "boinc" }
	if d = demand(); num(d, "target") != 1 || d["capped_by"] != "spend" || num(d, "pilots_wanted") != 0 || num(d, "cancel_unsent") != 1 {
		t.Fatalf("spend cap 1: %v", d)
	}
	r.g.spendCap = nil
	// This connector's own pilots online are not counted as slots (they are outstanding already).
	r.g.mu.Lock()
	r.g.node[ps[0].ID] = &nodeConn{id: ps[0].ID, pilot: ps[0].ID, connector: "boinc", models: []protocol.ModelInfo{{Name: "m", Slots: 1}}}
	r.g.mu.Unlock()
	if d = demand(); num(d["science"].(map[string]any), "slots_online") != 1 {
		t.Fatalf("own pilot counted as a slot: %v", d)
	}
	// In trusted-only mode no pilot can serve (pilots are never trusted): none are wanted for science.
	r.g.accounts.Update(func(f *AccountsFile) error { f.Settings.Routing = RoutingTrustedOnly; return nil })
	if d = demand(); num(d["science"].(map[string]any), "pilots") != 0 {
		t.Fatalf("science pilots wanted in trusted-only mode: %v", d)
	}
}

// An account's own API key reads and sets its "chat on pilots" switch; other keys cannot.
func TestAccountSettingsAPI(t *testing.T) {
	r := newPilotRig(t, nil)
	r.g.accounts.Update(func(f *AccountsFile) error {
		f.Accounts = append(f.Accounts, &Account{ID: "gh:7", Status: StatusActive, Created: r.clock()})
		return nil
	})
	key, err := r.g.accounts.AddKey("gh:7", 3, r.clock(), false, nil)
	if err != nil {
		t.Fatal(err)
	}
	if out := r.want(200, "GET", "/v1/account/settings", key, nil); out["chat_on_pilots"] != true {
		t.Fatalf("default: %v", out)
	}
	if out := r.want(200, "PATCH", "/v1/account/settings", key, map[string]any{"chat_on_pilots": false}); out["chat_on_pilots"] != false {
		t.Fatalf("off: %v", out)
	}
	if a, _, _ := r.g.accounts.Get("gh:7"); !a.PilotChatOff {
		t.Error("the switch is not in the accounts file")
	}
	r.want(400, "PATCH", "/v1/account/settings", key, map[string]any{})
	for _, k := range []string{"", testProjectKey, testConnectorKey} {
		if w, _ := r.call("PATCH", "/v1/account/settings", k, map[string]any{"chat_on_pilots": true}, ""); w.Code != 401 {
			t.Errorf("key %q: HTTP %d", k, w.Code)
		}
	}
}

// A pilot credential in a prompt never reaches a node, as a node token does not.
func TestPilotCredentialRedacted(t *testing.T) {
	_, cred, err := newPilotCredential()
	if err != nil {
		t.Fatal(err)
	}
	body := map[string]any{"messages": []any{map[string]any{"role": "user", "content": "the pilot uses " + cred + " to connect"}}}
	red := newRedactor()
	red.redactBody(body)
	if strings.Contains(statusText(body), cred[3:]) {
		t.Fatal("a pilot credential reached the node's copy of the request")
	}
	glued := map[string]any{"messages": []any{map[string]any{"role": "user", "content": "token_" + cred}}}
	newRedactor().redactBody(glued)
	if strings.Contains(statusText(glued), cred[3:]) {
		t.Fatal("a pilot credential glued to a word reached the node")
	}
}

// An account's own pilot is not "its own node" for placement either: it gets no preference and its
// placements count toward its operator's share.
func TestPilotNotOwnForPlacement(t *testing.T) {
	r := newPilotRig(t, nil)
	id, cred := r.create()
	r.bind(id, "5")
	n := r.pilotNode(cred, 1)
	n.deadline = time.Now().Add(time.Hour) // pick checks the deadline against the real clock, not the rig's
	rt := &route{want: "m", class: ClassScience, project: "lab", self: n.operator, preferSelf: true, exclude: map[*nodeConn]bool{}}
	got, _ := r.g.pick(rt)
	if got != n {
		t.Fatalf("picked %v", got)
	}
	r.g.mu.Lock()
	defer r.g.mu.Unlock()
	if len(r.g.shares) != 1 || r.g.shares[0].op != n.operator {
		t.Fatalf("the own pilot's placement was not counted toward its share: %+v", r.g.shares)
	}
}

// A pilot reported while its connection registers is never routable: it waits, registered but
// held back, until its record is read once more, and is then closed. "after": the report's drop finds
// the socket; "before": the drop ran before the socket was registered, so only the second read of the
// record closes it.
func TestPilotReportedDuringRegistration(t *testing.T) {
	for _, drop := range []bool{true, false} {
		t.Run(map[bool]string{true: "after", false: "before"}[drop], func(t *testing.T) { reportedDuringRegistration(t, drop) })
	}
}

func reportedDuringRegistration(t *testing.T, drop bool) {
	r := newPilotRig(t, nil)
	srv := httptest.NewServer(r.g.Handler())
	defer srv.Close()
	id, cred := r.create()
	r.bind(id, "5")
	var hooked, routable atomic.Bool
	r.g.pilotRegistered = func(pid string) {
		r.g.mu.Lock()
		n := r.g.node[pid]
		routable.Store(n != nil && (&route{class: ClassScience, project: "lab"}).allows(n, r.clock()))
		r.g.mu.Unlock()
		hooked.Store(true) // before the close below reaches the client
		r.g.sci.endPilot("boinc", pid, "reported: success", r.clock())
		if drop {
			r.g.dropPilot("boinc", pid, "pilot reported")
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http")+"/v1/node/connect",
		&websocket.DialOptions{HTTPHeader: http.Header{"Authorization": {"Bearer " + cred}}})
	if err != nil {
		t.Fatal(err)
	}
	hello, _ := json.Marshal(protocol.Msg{Type: protocol.TypeHello, NodeID: id, Protocol: protocol.LegacyVersion,
		Models: []protocol.ModelInfo{{Name: "m", Roles: []string{"deep"}, Slots: 1}}})
	c.Write(ctx, websocket.MessageText, hello)
	for {
		if _, _, err = c.Read(ctx); err != nil {
			break
		}
	}
	if !hooked.Load() {
		t.Fatalf("never registered: %v", err)
	}
	if routable.Load() {
		t.Fatal("a pilot was routable before its record was read again")
	}
	if websocket.CloseStatus(err) != websocket.StatusPolicyViolation {
		t.Fatalf("not closed as ended: %v", err)
	}
	r.g.mu.Lock()
	for _, n := range r.g.node {
		if (&route{class: ClassScience, project: "lab"}).allows(n, r.clock()) {
			t.Error("the ended pilot is routable")
		}
	}
	r.g.mu.Unlock()
	for deadline := time.Now().Add(5 * time.Second); r.g.NodeCount() != 0; time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the reported pilot stayed registered")
		}
	}
}

// dialPilot connects a raw pilot with its credential and hello; the caller reads until it closes.
func dialPilot(t *testing.T, ctx context.Context, srv *httptest.Server, id, cred string) *websocket.Conn {
	t.Helper()
	c, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http")+"/v1/node/connect",
		&websocket.DialOptions{HTTPHeader: http.Header{"Authorization": {"Bearer " + cred}}})
	if err != nil {
		t.Fatal(err)
	}
	hello, _ := json.Marshal(protocol.Msg{Type: protocol.TypeHello, NodeID: id, Protocol: protocol.LegacyVersion,
		Models: []protocol.ModelInfo{{Name: "m", Roles: []string{"deep"}, Slots: 1}}})
	c.Write(ctx, websocket.MessageText, hello)
	return c
}

// readClose reads c until it closes and returns why (a context error if it never does).
func readClose(ctx context.Context, c *websocket.Conn) error {
	for {
		if _, _, err := c.Read(ctx); err != nil {
			return err
		}
	}
}

// A change made while a pilot's connection is being set up (after authPilot, before it is
// routable) applies when it registers, not at the next sweep: a ban closes it, the account's chat
// switch keeps it on science.
func TestPilotRechecksAccountWhenRegistered(t *testing.T) {
	for _, change := range []string{"ban", "chat off"} {
		t.Run(change, func(t *testing.T) {
			r := newPilotRig(t, func(c *Config) { c.Connectors[0].PilotsChatEnabled = true })
			srv := httptest.NewServer(r.g.Handler())
			defer srv.Close()
			id, cred := r.create()
			r.bind(id, "5")
			p, _ := r.g.sci.pilot("boinc", id)
			r.g.pilotRegistered = func(string) {
				if change == "ban" {
					r.g.accounts.SetStatus(p.account, StatusBanned)
				} else {
					r.g.accounts.SetPilotChat(p.account, false)
				}
			}
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			c := dialPilot(t, ctx, srv, id, cred)
			if change == "ban" {
				// Closed at registration (its reason), not by the sweep that would follow seconds later.
				if err := readClose(ctx, c); !strings.Contains(err.Error(), "this pilot's account is closed") {
					t.Fatalf("a pilot banned while registering: %v", err)
				}
				return
			}
			go readClose(ctx, c)
			var n *nodeConn
			for deadline := time.Now().Add(2 * time.Second); ; time.Sleep(5 * time.Millisecond) {
				r.g.mu.Lock()
				n = r.g.node[id]
				ready := n != nil && !n.registering
				r.g.mu.Unlock()
				if ready {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("the pilot never became routable")
				}
			}
			r.g.mu.Lock()
			chat := (&route{class: ClassChat}).allows(n, r.clock())
			r.g.mu.Unlock()
			if chat {
				t.Error("a pilot whose account turned chat off while it registered serves chat")
			}
		})
	}
}

// A report ends the pilot's connection even when the record cannot be written or read back: a
// reported pilot never keeps serving until its deadline.
func TestPilotReportedDropsOnDatabaseError(t *testing.T) {
	r := newPilotRig(t, nil)
	srv := httptest.NewServer(r.g.Handler())
	defer srv.Close()
	id, cred := r.create()
	r.bind(id, "5")
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	c := dialPilot(t, ctx, srv, id, cred)
	closed := make(chan error, 1)
	go func() { closed <- readClose(ctx, c) }()
	for deadline := time.Now().Add(2 * time.Second); ; time.Sleep(5 * time.Millisecond) {
		r.g.mu.Lock()
		n := r.g.node[id]
		ready := n != nil && !n.registering
		r.g.mu.Unlock()
		if ready {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the pilot never connected")
		}
	}
	r.g.sci.db.Close() // every write and read fails from here
	// Another connector cannot drop it, database or not.
	if w, _ := r.call("POST", "/v1/connectors/other/pilots/"+id+"/reported", otherConnectorKey, map[string]any{"outcome": "success"}, "203.0.113.9:1"); w.Code != 503 {
		t.Fatalf("another connector's report: HTTP %d", w.Code)
	}
	select {
	case err := <-closed:
		t.Fatalf("another connector's report dropped the pilot: %v", err)
	case <-time.After(300 * time.Millisecond):
	}
	if w, _ := r.call("POST", pilotsPath+"/"+id+"/reported", testConnectorKey, map[string]any{"outcome": "success"}, ""); w.Code != 503 {
		t.Fatalf("report with the database down: HTTP %d", w.Code)
	}
	if err := <-closed; websocket.CloseStatus(err) != websocket.StatusPolicyViolation {
		t.Fatalf("the reported pilot was not dropped: %v", err)
	}
}
