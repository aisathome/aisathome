// Stand-ins for the gateway's connector API and for BOINC, for the bridge's tests.
//
// SPDX-License-Identifier: Apache-2.0
package bridge

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

const testKey = "ck-test-connector-key-0123456789abcdefghijkl"

// fakeGW is the connector API with the state the bridge drives.
type fakeGW struct {
	mu       sync.Mutex
	pilots   map[string]*Pilot
	order    []string // creation order
	creds    map[string]string
	demand   Demand
	credits  map[string]Credit
	requests []Request
	answers  map[string]Answer
	binds    map[string][3]string
	reported map[string]string
	createSt int // answer POST .../pilots with this status instead
	answerSt int // answer POST .../requests/{rid} with this status instead
	n        int
	now      func() time.Time
}

func newFakeGW(t *testing.T, now func() time.Time) (*fakeGW, string) {
	f := &fakeGW{pilots: map[string]*Pilot{}, creds: map[string]string{}, credits: map[string]Credit{},
		answers: map[string]Answer{}, binds: map[string][3]string{}, reported: map[string]string{}, now: now}
	mux := http.NewServeMux()
	auth := func(h http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") != "Bearer "+testKey || r.PathValue("id") != "boinc" {
				jsonErr(w, 401, "no")
				return
			}
			f.mu.Lock()
			defer f.mu.Unlock()
			h(w, r)
		}
	}
	mux.HandleFunc("GET /v1/connectors/{id}/demand", auth(func(w http.ResponseWriter, r *http.Request) {
		writeJ(w, 200, f.demand)
	}))
	mux.HandleFunc("POST /v1/connectors/{id}/pilots", auth(func(w http.ResponseWriter, r *http.Request) {
		if f.createSt != 0 {
			jsonErr(w, f.createSt, "refused")
			return
		}
		var body struct {
			Deadline time.Time `json:"deadline"`
		}
		if json.NewDecoder(r.Body).Decode(&body) != nil || body.Deadline.IsZero() {
			jsonErr(w, 400, "deadline")
			return
		}
		f.n++
		id := fmt.Sprintf("p-%012x", f.n)
		p := &Pilot{ID: id, State: pilotCreated, Deadline: body.Deadline, Created: f.now()}
		f.pilots[id] = p
		f.order = append(f.order, id)
		cred := fmt.Sprintf("pc-%012x.secret-%040d", f.n, f.n)
		f.creds[id] = cred
		writeJ(w, 201, map[string]any{"pilot": p, "credential": cred})
	}))
	mux.HandleFunc("GET /v1/connectors/{id}/pilots", auth(func(w http.ResponseWriter, r *http.Request) {
		st := r.URL.Query().Get("state")
		out := []*Pilot{}
		for i := len(f.order) - 1; i >= 0; i-- { // newest first
			p := f.pilots[f.order[i]]
			if st == "" || p.State == st {
				out = append(out, p)
			}
		}
		writeJ(w, 200, map[string]any{"pilots": out})
	}))
	mux.HandleFunc("POST /v1/connectors/{id}/pilots/{pid}/bind", auth(func(w http.ResponseWriter, r *http.Request) {
		p := f.pilots[r.PathValue("pid")]
		var b struct{ User, Host, Team string }
		json.NewDecoder(r.Body).Decode(&b)
		if p == nil || p.State == "ended" {
			jsonErr(w, 409, "ended")
			return
		}
		f.binds[p.ID] = [3]string{b.User, b.Host, b.Team}
		p.State, p.User, p.Host, p.Team = pilotBound, b.User, b.Host, b.Team
		writeJ(w, 200, map[string]any{"pilot": p})
	}))
	mux.HandleFunc("POST /v1/connectors/{id}/pilots/{pid}/reported", auth(func(w http.ResponseWriter, r *http.Request) {
		p := f.pilots[r.PathValue("pid")]
		var b struct{ Outcome string }
		json.NewDecoder(r.Body).Decode(&b)
		if p == nil {
			jsonErr(w, 404, "no pilot")
			return
		}
		f.reported[p.ID] = b.Outcome
		p.State = "ended"
		writeJ(w, 200, map[string]any{"pilot": p})
	}))
	mux.HandleFunc("GET /v1/connectors/{id}/pilots/{pid}/credit", auth(func(w http.ResponseWriter, r *http.Request) {
		c, ok := f.credits[r.PathValue("pid")]
		if !ok {
			jsonErr(w, 404, "no pilot")
			return
		}
		writeJ(w, 200, c)
	}))
	mux.HandleFunc("GET /v1/connectors/{id}/requests", auth(func(w http.ResponseWriter, r *http.Request) {
		out := []Request{}
		for _, q := range f.requests {
			if _, done := f.answers[q.ID]; !done {
				out = append(out, q)
			}
		}
		writeJ(w, 200, map[string]any{"requests": out})
	}))
	mux.HandleFunc("POST /v1/connectors/{id}/requests/{rid}", auth(func(w http.ResponseWriter, r *http.Request) {
		if f.answerSt != 0 {
			jsonErr(w, f.answerSt, "not now")
			return
		}
		var a Answer
		dec := json.NewDecoder(r.Body)
		dec.DisallowUnknownFields()
		if err := dec.Decode(&a); err != nil {
			jsonErr(w, 400, err.Error())
			return
		}
		f.answers[r.PathValue("rid")] = a
		writeJ(w, 200, map[string]any{"result": "ok"})
	}))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return f, srv.URL
}

func writeJ(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func jsonErr(w http.ResponseWriter, code int, msg string) {
	writeJ(w, code, map[string]any{"error": map[string]any{"message": msg}})
}

func (f *fakeGW) state(id string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.pilots[id].State
}

func (f *fakeGW) ids() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.order)
}

// fakeBOINC is the project: its database (Store), its tools and its web RPCs.
type fakeBOINC struct {
	mu        sync.Mutex
	jobs      map[string]Job
	staged    map[string][]byte
	works     map[string]string // work unit → credential file
	cancelled []string
	stageErr  error
	createErr error
	cancelErr error
	storeErr  error

	accounts map[string]*fakeAccount // by account key
	byEmail  map[string]*fakeAccount
	rpcErr   error // every RPC fails with it
	creates  []string
	nextID   int64
}

type fakeAccount struct {
	id         int64
	email      string
	passwdHash string
	name       string
	auth       string
}

func newFakeBOINC() *fakeBOINC {
	return &fakeBOINC{jobs: map[string]Job{}, staged: map[string][]byte{}, works: map[string]string{},
		accounts: map[string]*fakeAccount{}, byEmail: map[string]*fakeAccount{}, nextID: 100}
}

func (f *fakeBOINC) setJob(name string, j Job) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.jobs[name] = j
}

func (f *fakeBOINC) Jobs(_ context.Context, names []string) (map[string]Job, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.storeErr != nil {
		return nil, f.storeErr
	}
	out := map[string]Job{}
	for _, n := range names {
		if j, ok := f.jobs[n]; ok {
			out[n] = j
		}
	}
	return out, nil
}

func (f *fakeBOINC) AwaitingCredit(_ context.Context, limit int) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for n, j := range f.jobs {
		if j.HasResult && j.ServerState == ServerStateOver && j.Outcome == OutcomeSuccess && j.ValidateState == ValidateStateInit {
			out = append(out, n)
		}
	}
	sort.Strings(out)
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (f *fakeBOINC) Stage(_ context.Context, name string, content []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.stageErr != nil {
		return f.stageErr
	}
	f.staged[name] = content
	return nil
}

func (f *fakeBOINC) Unstage(_ context.Context, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.staged, name)
	return nil
}

func (f *fakeBOINC) CreateWork(_ context.Context, name, cred string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.createErr != nil {
		return f.createErr
	}
	if _, ok := f.staged[cred]; !ok {
		return errors.New("create_work: input file not staged")
	}
	f.works[name] = cred
	f.jobs[name] = Job{} // the transitioner has not made the result yet
	return nil
}

func (f *fakeBOINC) Cancel(_ context.Context, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.cancelErr != nil {
		return f.cancelErr
	}
	f.cancelled = append(f.cancelled, name)
	j := f.jobs[name]
	j.ErrorMask |= WUErrorCancelled
	if j.HasResult && j.ServerState == ServerStateUnsent {
		j.ServerState, j.Outcome = ServerStateOver, OutcomeDidntNeed
	}
	f.jobs[name] = j
	return nil
}

func (f *fakeBOINC) CreateAccount(_ context.Context, email, passwdHash, userName string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.rpcErr != nil {
		return "", f.rpcErr
	}
	f.creates = append(f.creates, passwdHash)
	if a, ok := f.byEmail[strings.ToLower(email)]; ok { // create_account.php's rule
		if a.passwdHash != passwdHash {
			return "", &RPCError{Num: errDBNotUnique, Msg: "Name or email address is not unique"}
		}
		return a.auth, nil
	}
	f.nextID++
	a := &fakeAccount{id: f.nextID, email: email, passwdHash: passwdHash, name: userName, auth: md5Hex(fmt.Sprint("auth", f.nextID))}
	f.accounts[a.auth] = a
	f.byEmail[strings.ToLower(email)] = a
	return a.auth, nil
}

func (f *fakeBOINC) AccountInfo(_ context.Context, key string) (Account, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.rpcErr != nil {
		return Account{}, f.rpcErr
	}
	a, ok := f.accounts[key]
	if !ok {
		return Account{}, &RPCError{Num: errDBNotFound, Msg: "Not found"}
	}
	return Account{ID: a.id, WeakAuth: fmt.Sprintf("%d_%s", a.id, md5Hex(a.auth+a.passwdHash))}, nil
}

// clock is a settable time.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) add(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// rig is a bridge on the fakes, with its log captured.
type rig struct {
	b     *Bridge
	gw    *fakeGW
	boinc *fakeBOINC
	clk   *clock
	logs  *bytes.Buffer
	cfg   Config
}

func testConfig(t *testing.T, gw string) Config {
	dir := t.TempDir()
	c := Config{Gateway: gw, Connector: "boinc", KeyFile: "/dev/null", PilotGateway: "wss://gw.example/v1/node/connect",
		ProjectDir: dir, Batch: 1, Model: RemoteFile{URL: "https://example.org/model.gguf", NBytes: 1000, MD5: strings.Repeat("a", 32)},
		WebURL: "http://apache/project/"}
	if err := c.check(); err != nil {
		t.Fatal(err)
	}
	return c
}

func newRig(t *testing.T, edit ...func(*Config)) *rig {
	clk := &clock{t: time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)}
	gw, url := newFakeGW(t, clk.now)
	cfg := testConfig(t, url)
	for _, e := range edit {
		e(&cfg)
	}
	fb := newFakeBOINC()
	logs := &bytes.Buffer{}
	log := slog.New(slog.NewTextHandler(&lockedWriter{w: logs}, &slog.HandlerOptions{Level: slog.LevelDebug}))
	b, err := New(cfg, testKey, fb, fb, fb, log)
	if err != nil {
		t.Fatal(err)
	}
	b.Now = clk.now
	return &rig{b: b, gw: gw, boinc: fb, clk: clk, logs: logs, cfg: cfg}
}

type lockedWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}
