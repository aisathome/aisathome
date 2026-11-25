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
	users  map[string]*simUser // by account key
	emails map[string]*simUser // by address
	nextID int64
}

type simUser struct {
	id         int64
	auth, hash string
}

func newBoincSim() *boincSim {
	return &boincSim{jobs: map[string]bridge.Job{}, staged: map[string][]byte{}, works: map[string]string{},
		users: map[string]*simUser{}, emails: map[string]*simUser{}, nextID: 500}
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
			out = append(out, bridge.Awaiting{ResultID: int64(len(out) + 1), Name: n})
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

func (s *boincSim) CreateWork(_ context.Context, name, cred string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.works[name] = cred
	s.jobs[name] = bridge.Job{HasResult: true, ServerState: bridge.ServerStateUnsent}
	return nil
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
		CreditDir: creditDir, WebURL: "http://boinc.test/proj/"}
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
	// ends it and drops its connection. The validator's first look, before the credit file exists,
	// leaves the result inconclusive; the credit is final a day later, and the file the validator
	// reads then holds exactly the record's credit.
	sim.setJob(id, bridge.Job{HasResult: true, ServerState: bridge.ServerStateOver, Outcome: bridge.OutcomeSuccess, User: 5, Host: 11, Team: 3})
	if err := b.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	if p := a.pilotRecord(t, id); p["state"] != gateway.PilotEnded || p["end_reason"] != "reported: success" {
		t.Fatalf("after BOINC's result: %v", p)
	}
	waitUntil(t, "the reported pilot to be dropped", func() bool { return a.g.NodeCount() == 0 })
	ag.Leave("pilot: idle", 2*time.Second)
	sim.setJob(id, bridge.Job{HasResult: true, ServerState: bridge.ServerStateOver, Outcome: bridge.OutcomeSuccess, User: 5, Host: 11, Team: 3,
		ValidateState: bridge.ValidateStateInconclusive})
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
}
