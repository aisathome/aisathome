// The BOINC bridge end to end against the real gateway: demand makes a
// pilot and its work unit, BOINC sending it makes the bind, a real node agent connects with the
// credential the bridge staged and serves science, the result's end reports it, and the final
// credit file equals the gateway's record. People's "Run on BOINC" and claim requests go through the
// bridge to BOINC's account RPCs and back. BOINC itself is a small in-memory stand-in (database
// rows, tools and RPCs), as the bridge sees it.
//
// SPDX-License-Identifier: Apache-2.0
package e2e

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aisathome/aisathome/boinc/bridge"
	"github.com/aisathome/aisathome/gateway"
)

// boincSim is the BOINC project as the bridge sees it.
type boincSim struct {
	mu     sync.Mutex
	jobs   map[string]bridge.Job
	staged map[string][]byte
	works  map[string]string   // work unit → credential file
	models map[string]string   // work unit → its model's URL
	users  map[string]*simUser // by account key
	ids    map[string]int64    // work unit ids
	valid  []int64             // work units the validator ran on
	emails map[string]*simUser // by address
	nextID int64
}

type simUser struct {
	id         int64
	auth, hash string
}

func newBoincSim() *boincSim {
	return &boincSim{jobs: map[string]bridge.Job{}, staged: map[string][]byte{}, works: map[string]string{},
		users: map[string]*simUser{}, emails: map[string]*simUser{}, nextID: 500, ids: map[string]int64{}}
}

func (s *boincSim) Jobs(_ context.Context, names []string) (map[string]bridge.Job, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[string]bridge.Job{}
	for _, n := range names {
		if j, ok := s.jobs[n]; ok {
			out[n] = j
		}
	}
	return out, nil
}

func (s *boincSim) AwaitingCredit(_ context.Context, after int64, _ int) ([]bridge.Awaiting, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if after > 0 {
		return nil, nil
	}
	var out []bridge.Awaiting
	for n, j := range s.jobs {
		if j.ServerState == bridge.ServerStateOver && j.Outcome == bridge.OutcomeSuccess &&
			(j.ValidateState == bridge.ValidateStateInit || j.ValidateState == bridge.ValidateStateInconclusive) {
			out = append(out, bridge.Awaiting{ResultID: s.ids[n] + 1000, WorkunitID: s.ids[n], Name: n, App: j.App, ValidateState: j.ValidateState})
		}
	}
	return out, nil
}

func (s *boincSim) Stage(_ context.Context, name string, b []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.staged[name] = b
	return nil
}

func (s *boincSim) Unstage(_ context.Context, name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.staged, name)
	return nil
}

func (s *boincSim) CreateWork(_ context.Context, name, cred string, t bridge.Tier) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.works[name] = cred
	s.ids[name] = int64(len(s.ids) + 1)
	if s.models == nil {
		s.models = map[string]string{}
	}
	s.models[name] = t.Model.URL
	s.jobs[name] = bridge.Job{App: t.App, HasResult: true, ServerState: bridge.ServerStateUnsent}
	return nil
}

// Validate marks the work unit valid (the pilot validator, which the bridge runs once the credit
// file is there).
func (s *boincSim) Validate(_ context.Context, id int64, _ string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.valid = append(s.valid, id)
	for n, i := range s.ids {
		if i == id {
			j := s.jobs[n]
			j.ValidateState, j.Claimed = bridge.ValidateStateValid, true
			s.jobs[n] = j
		}
	}
	return "", nil
}

func (s *boincSim) Cancel(_ context.Context, name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.jobs[name] = bridge.Job{HasResult: true, ServerState: bridge.ServerStateOver, Outcome: bridge.OutcomeDidntNeed,
		ErrorMask: bridge.WUErrorCancelled}
	return nil
}

func (s *boincSim) addUser(id int64, auth, hash string) *simUser {
	u := &simUser{id: id, auth: auth, hash: hash}
	s.users[auth] = u
	return u
}

func (s *boincSim) CreateAccount(_ context.Context, email, hash, _ string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if u, ok := s.emails[email]; ok {
		if u.hash != hash {
			return "", &bridge.RPCError{Num: -137}
		}
		return u.auth, nil
	}
	s.nextID++
	u := s.addUser(s.nextID, fmt.Sprintf("%032x", s.nextID), hash)
	s.emails[email] = u
	return u.auth, nil
}

func (s *boincSim) AccountInfo(_ context.Context, key string) (bridge.Account, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	u, ok := s.users[key]
	if !ok {
		return bridge.Account{}, &bridge.RPCError{Num: -136}
	}
	return bridge.Account{ID: u.id, WeakAuth: fmt.Sprintf("%d_%032x", u.id, u.id*7)}, nil
}

func (s *boincSim) setJob(name string, j bridge.Job) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.jobs[name] = j
}

type e2eClock struct {
	mu  sync.Mutex
	off time.Duration
}

func (c *e2eClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return time.Now().Add(c.off)
}

func (c *e2eClock) add(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.off += d
}

func TestBridgeEndToEnd(t *testing.T) {
	clk := &e2eClock{}
	a := newAcctRigNodes(t, 100, nil, func(c *gateway.Config) {
		c.Projects = []gateway.Project{{ID: "lab", KeySHA256: []string{sha(pilotSciKey)}, AgentOK: true, PilotProbationOK: true}}
		c.Connectors = []gateway.Connector{{ID: "boinc", KeySHA256: []string{sha(connectorKey)}, MaxPilots: 5,
			Accounts: &gateway.ConnectorAccounts{Name: "BOINC", AttachURL: "https://boinc.site.test/proj/",
				AttachCommand: "boinccmd --project_attach {url} {key}", AddressDomain: "nomail.site.test"}}}
		c.QueueTimeoutSec = 2
		c.PilotsNow = clk.now
	})
	engine := mockEngine(t, false)
	t.Cleanup(engine.Close)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sim := newBoincSim()
	creditDir := filepath.Join(t.TempDir(), "credit")
	cfg := bridge.Config{Gateway: a.gw.URL, Connector: "boinc", KeyFile: "unused", Batch: 1,
		PilotGateway: "ws" + strings.TrimPrefix(a.gw.URL, "http") + "/v1/node/connect", ProjectDir: t.TempDir(),
		Model:     bridge.RemoteFile{URL: "https://models.test/m.gguf", NBytes: 1, MD5: strings.Repeat("0", 32)},
		CreditDir: creditDir, WebURL: "http://boinc.test/proj/", Validator: "bin/pilot_validator", MaxGrantedCredit: 150000}
	b, err := bridge.New(cfg, connectorKey, sim, sim, sim, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	b.Now = clk.now

	// No science waiting: no pilot.
	if err := b.Demand(ctx); err != nil {
		t.Fatal(err)
	}
	if len(sim.works) != 0 {
		t.Fatalf("work units without demand: %v", sim.works)
	}
	// Two open tasks and two runner workers holding them, no GPU online: one pilot wanted.
	for i := 0; i < 2; i++ {
		task := map[string]any{"id": fmt.Sprintf("T-%d", i), "kind": "other", "created_by": "owner", "spec": map[string]any{"task": i}}
		if code, out := taskCall(t, a, "POST", "/v1/projects/lab/tasks", pilotSciKey, task); code != 201 {
			t.Fatalf("task: %d %v", code, out)
		}
		if code, out := taskCall(t, a, "POST", "/v1/projects/lab/tasks/claim", pilotSciKey, map[string]any{"worker": fmt.Sprintf("w%d", i)}); code != 200 {
			t.Fatalf("claim: %d %v", code, out)
		}
	}
	if err := b.Demand(ctx); err != nil {
		t.Fatal(err)
	}
	if len(sim.works) != 1 {
		t.Fatalf("work units: %v, want 1", sim.works)
	}
	var id, credFile string
	for id, credFile = range sim.works {
	}
	cred := strings.TrimSpace(string(sim.staged[credFile]))
	if p := a.pilotRecord(t, id); p["state"] != gateway.PilotCreated || !strings.HasPrefix(cred, "pc-"+strings.TrimPrefix(id, "p-")+".") {
		t.Fatalf("pilot %v, credential of another pilot", p)
	}

	// The pilot starts before the bind (BOINC sent it; the bridge has not looked yet): refused
	// until the bind.
	ag := a.runPilot(ctx, cred, engine.URL)
	sim.setJob(id, bridge.Job{HasResult: true, ServerState: bridge.ServerStateInProgress, User: 5, Host: 11, Team: 3})
	if err := b.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	if p := a.pilotRecord(t, id); p["user"] != "5" || p["host"] != "11" || p["team"] != "3" || p["account"] != "unclaimed" {
		t.Fatalf("after the bind: %v", p)
	}
	waitUntil(t, "the bound pilot to connect", func() bool { return a.g.NodeCount() == 1 })
	if code, body := ask(t, a.rig, pilotSciKey, deepBody); code != 200 || !strings.Contains(body, "Hello") {
		t.Fatalf("science on the pilot: %d %s", code, body)
	}
	if code, _ := ask(t, a.rig, "user-key", deepBody); code != 503 {
		t.Fatalf("chat reached a pilot with chat on pilots off: %d", code)
	}

	// The person behind BOINC user 5 claims it with the account key BOINC shows them; a wrong key
	// fails with the bridge's reason.
	acctKey := strings.Repeat("5e", 16)
	sim.addUser(5, acctKey, "h")
	s, _ := a.signIn(t, 7)
	a.post(t, "/account/connectors/boinc/claim", s, url.Values{"key": {strings.Repeat("0d", 16)}}.Encode())
	if err := b.Requests(ctx); err != nil {
		t.Fatal(err)
	}
	if _, page := a.get(t, "/account", s); !strings.Contains(page, "no BOINC account of this project has that account key") {
		t.Fatal("the failed claim's reason is not on the account page")
	}
	a.post(t, "/account/connectors/boinc/claim", s, url.Values{"key": {acctKey}}.Encode())
	if err := b.Requests(ctx); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, "the claimed pilot to serve as the person's node", func() bool {
		p := a.pilotRecord(t, id)
		return p["account"] == "linked" && p["state"] == gateway.PilotConnected && a.g.NodeCount() == 1
	})
	if _, page := a.get(t, "/account", s); !strings.Contains(page, "boinccmd --project_attach https://boinc.site.test/proj/ 5_") {
		t.Error("no attach command with the weak key on the account page")
	}
	waitUntil(t, "science after the claim", func() bool { code, _ := ask(t, a.rig, pilotSciKey, deepBody); return code == 200 })

	// "Run on BOINC" with a password: the bridge makes the BOINC user with the person's hash.
	s2, _ := a.signIn(t, 8)
	a.post(t, "/account/connectors/boinc/run", s2, url.Values{"name": {"Ada"}, "password": {"a long password"}}.Encode())
	if err := b.Requests(ctx); err != nil {
		t.Fatal(err)
	}
	if _, page := a.get(t, "/account", s2); !strings.Contains(page, "BOINC user <strong>501</strong>") ||
		!strings.Contains(page, "boinccmd --project_attach https://boinc.site.test/proj/ 501_") {
		t.Error("Run on BOINC: the account page lacks the new user and its attach command")
	}

	// BOINC has the result while the gateway still sees the pilot connected: the bridge's report
	// ends it and drops its connection. The credit is final a day later: the file the validator
	// reads then holds exactly the record's credit, and the bridge runs the validator on that work
	// unit once.
	sim.setJob(id, bridge.Job{HasResult: true, ServerState: bridge.ServerStateOver, Outcome: bridge.OutcomeSuccess, User: 5, Host: 11, Team: 3})
	if err := b.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	if p := a.pilotRecord(t, id); p["state"] != gateway.PilotEnded || p["end_reason"] != "reported: success" {
		t.Fatalf("after BOINC's result: %v", p)
	}
	waitUntil(t, "the reported pilot to be dropped", func() bool { return a.g.NodeCount() == 0 })
	ag.Leave("pilot: idle", 2*time.Second)
	if err := b.Credit(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(creditDir, id)); err == nil {
		t.Fatal("a credit file before the hold is over")
	}
	clk.add(25 * time.Hour)
	if err := b.Credit(ctx); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(creditDir, id))
	if err != nil {
		t.Fatal(err)
	}
	got, _ := strconv.ParseFloat(strings.TrimSpace(string(raw)), 64)
	p := a.pilotRecord(t, id)
	if want := p["credit"].(float64); got != want || want <= 0 {
		t.Fatalf("credit file %v, record %v", got, want)
	}
	if len(sim.valid) != 1 || sim.valid[0] != sim.ids[id] {
		t.Errorf("validator runs %v, want once on work unit %d", sim.valid, sim.ids[id])
	}
}

// TestBridgeTiersEndToEnd: a connector with pilot tiers and a bridge with the same tiers. A project
// whose sessions need a large context gets pilots of the big tier, in the big tier's BOINC app with
// its model; a project without a floor gets the standard tier's; when the demand goes, the unsent
// work units of each tier are cancelled.
func TestBridgeTiersEndToEnd(t *testing.T) {
	clk := &e2eClock{}
	const longKey, shortKey = "ak-long-project-key-0123456789", "ak-short-project-key-012345678"
	a := newAcctRigNodes(t, 100, nil, func(c *gateway.Config) {
		c.Projects = []gateway.Project{
			{ID: "long", KeySHA256: []string{sha(longKey)}, AgentOK: true, PilotProbationOK: true, MinNodeCtx: 131072},
			{ID: "short", KeySHA256: []string{sha(shortKey)}, AgentOK: true, PilotProbationOK: true}}
		c.Connectors = []gateway.Connector{{ID: "boinc", KeySHA256: []string{sha(connectorKey)}, MaxPilots: 5, UnsentBuffer: 4,
			PilotTiers: []gateway.PilotTier{{Name: "standard", Ctx: 81920}, {Name: "big", Ctx: 196608}}}}
		c.PilotsNow = clk.now
	})
	ctx := context.Background()
	sim := newBoincSim()
	big := bridge.RemoteFile{URL: "https://models.test/iq4.gguf", NBytes: 14252845984, MD5: strings.Repeat("a", 32)}
	std := bridge.RemoteFile{URL: "https://models.test/q3.gguf", NBytes: 13146393504, MD5: strings.Repeat("b", 32)}
	cfg := bridge.Config{Gateway: a.gw.URL, Connector: "boinc", KeyFile: "unused", Batch: 1,
		PilotGateway: "ws" + strings.TrimPrefix(a.gw.URL, "http") + "/v1/node/connect", ProjectDir: t.TempDir(),
		Tiers:  []bridge.Tier{{Name: "big", App: "pilot", Model: big}, {Name: "standard", App: "pilot_std", Model: std}},
		WebURL: "http://boinc.test/proj/"}
	b, err := bridge.New(cfg, connectorKey, sim, sim, sim, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	b.Now = clk.now
	if err := b.Check(ctx); err == nil || !strings.Contains(err.Error(), "program") {
		t.Fatalf("check: %v (the tiers agree; only BOINC's programs are missing here)", err)
	}
	claim := func(project, key string, n int) {
		for i := 0; i < n; i++ {
			task := map[string]any{"id": fmt.Sprintf("%s-%d", project, i), "kind": "other", "created_by": "owner", "spec": map[string]any{"task": i}}
			if code, out := taskCall(t, a, "POST", "/v1/projects/"+project+"/tasks", key, task); code != 201 {
				t.Fatalf("task: %d %v", code, out)
			}
			if code, out := taskCall(t, a, "POST", "/v1/projects/"+project+"/tasks/claim", key, map[string]any{"worker": fmt.Sprintf("%s-w%d", project, i)}); code != 200 {
				t.Fatalf("claim: %d %v", code, out)
			}
		}
	}
	// Two sessions of the long project: one big pilot, in the app whose plan classes need 22 GB, with the
	// 24 GB model. Two of the short project: one standard pilot, in pilot_std with the 16 GB model.
	claim("long", longKey, 2)
	claim("short", shortKey, 2)
	if err := b.Demand(ctx); err != nil {
		t.Fatal(err)
	}
	byTier := map[string]string{}
	for id := range sim.works {
		p := a.pilotRecord(t, id)
		tier, _ := p["tier"].(string)
		byTier[tier] = sim.jobs[id].App + " " + sim.models[id]
	}
	if len(sim.works) != 2 || byTier["big"] != "pilot "+big.URL || byTier["standard"] != "pilot_std "+std.URL {
		t.Fatalf("work units %v, apps by tier %v", sim.works, byTier)
	}
	// Both projects' work is done: neither tier wants its unsent pilot.
	for _, p := range []struct{ project, key string }{{"long", longKey}, {"short", shortKey}} {
		for i := 0; i < 2; i++ {
			path := fmt.Sprintf("/v1/projects/%s/tasks/%s-%d/cancel", p.project, p.project, i)
			if code, out := taskCall(t, a, "POST", path, p.key, map[string]any{"reason": "test over"}); code != 200 {
				t.Fatalf("cancel %s: %d %v", path, code, out)
			}
		}
	}
	clk.add(time.Minute)
	if err := b.Demand(ctx); err != nil {
		t.Fatal(err)
	}
	for id := range sim.works {
		if p := a.pilotRecord(t, id); p["state"] != gateway.PilotEnded {
			t.Errorf("pilot %s (%v) not cancelled: %v", id, p["tier"], p["state"])
		}
	}
}
