// SPDX-License-Identifier: Apache-2.0
package bridge

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

var ctx = context.Background()

func TestJobReported(t *testing.T) {
	cases := []struct {
		name    string
		j       Job
		outcome string
		done    bool
	}{
		{"no result yet", Job{}, "", false},
		{"unsent", Job{HasResult: true, ServerState: ServerStateUnsent}, "", false},
		{"in progress", Job{HasResult: true, ServerState: ServerStateInProgress, Host: 3}, "", false},
		{"success", Job{HasResult: true, ServerState: ServerStateOver, Outcome: OutcomeSuccess}, "success", true},
		{"client error", Job{HasResult: true, ServerState: ServerStateOver, Outcome: OutcomeClientError}, "error", true},
		{"no reply", Job{HasResult: true, ServerState: ServerStateOver, Outcome: OutcomeNoReply}, "expired", true},
		{"didn't need", Job{HasResult: true, ServerState: ServerStateOver, Outcome: OutcomeDidntNeed}, "cancelled", true},
		// A host has it: reported when the client reports or aborts it, not before.
		{"cancelled while running", Job{HasResult: true, ServerState: ServerStateInProgress, ErrorMask: WUErrorCancelled}, "", false},
		{"cancelled before the result", Job{ErrorMask: WUErrorCancelled}, "cancelled", true},
		{"other work-unit error, unsent", Job{ErrorMask: 1}, "error", true},
		{"other work-unit error while running", Job{HasResult: true, ServerState: ServerStateInProgress, ErrorMask: 8}, "", false},
	}
	for _, c := range cases {
		o, done := c.j.reported()
		if o != c.outcome || done != c.done {
			t.Errorf("%s: got %q %v, want %q %v", c.name, o, done, c.outcome, c.done)
		}
	}
}

func TestDemandCreatesPilotsAndWorkUnits(t *testing.T) {
	r := newRig(t)
	r.gw.demand = Demand{Wanted: 2}
	if err := r.b.Demand(ctx); err != nil {
		t.Fatal(err)
	}
	ids := r.gw.ids()
	if len(ids) != 2 {
		t.Fatalf("pilots created: %v", ids)
	}
	nameRe := regexp.MustCompile(`^pc-p-[0-9a-f]{12}-[0-9a-f]{32}$`)
	for _, id := range ids {
		file, ok := r.boinc.works[id]
		if !ok {
			t.Fatalf("no work unit named %s", id)
		}
		if !nameRe.MatchString(file) || !strings.HasPrefix(file, "pc-"+id+"-") {
			t.Errorf("credential file name %q", file)
		}
		if got := string(r.boinc.staged[file]); got != r.gw.creds[id]+"\n" {
			t.Errorf("staged %q, want the pilot's credential", got)
		}
		// The deadline the bridge asked for: deadline_hours ahead.
		if d := r.gw.pilots[id].Deadline.Sub(r.clk.now()); d != 60*time.Hour {
			t.Errorf("deadline %v ahead", d)
		}
	}
}

func TestDemandStopsWhenTheGatewaySaysNoMore(t *testing.T) {
	for _, st := range []int{409, 429} {
		r := newRig(t)
		r.gw.demand = Demand{Wanted: 3}
		r.gw.createSt = st
		if err := r.b.Demand(ctx); err != nil {
			t.Errorf("%d: %v (a full connector is not an error)", st, err)
		}
		if len(r.boinc.works) != 0 {
			t.Errorf("%d: work units %v", st, r.boinc.works)
		}
	}
}

func TestFailedWorkUnitEndsItsPilot(t *testing.T) {
	r := newRig(t)
	r.gw.demand = Demand{Wanted: 1}
	r.boinc.createErr = errors.New("create_work: no such template")
	if err := r.b.Demand(ctx); err == nil {
		t.Fatal("no error")
	}
	id := r.gw.ids()[0]
	if r.gw.reported[id] != "error" {
		t.Errorf("reported %q, want error", r.gw.reported[id])
	}
	if len(r.boinc.staged) != 0 {
		t.Errorf("credential left staged: %v", r.boinc.staged)
	}

	r = newRig(t)
	r.gw.demand = Demand{Wanted: 1}
	r.boinc.stageErr = errors.New("disk full")
	r.b.Demand(ctx)
	if id := r.gw.ids()[0]; r.gw.reported[id] != "error" || len(r.boinc.works) != 0 {
		t.Errorf("stage failure: reported %q, works %v", r.gw.reported[id], r.boinc.works)
	}
}

// created makes pilots at the fake gateway and their work units in the fake BOINC.
func (r *rig) created(t *testing.T, n int) []string {
	t.Helper()
	r.gw.demand = Demand{Wanted: n}
	if err := r.b.Demand(ctx); err != nil {
		t.Fatal(err)
	}
	r.gw.demand = Demand{}
	return r.gw.ids()[len(r.gw.ids())-n:]
}

func TestCancelUnsentNewestFirstNeverSent(t *testing.T) {
	r := newRig(t)
	ids := r.created(t, 3)
	r.boinc.setJob(ids[0], Job{HasResult: true, ServerState: ServerStateUnsent})
	r.boinc.setJob(ids[1], Job{HasResult: true, ServerState: ServerStateUnsent})
	r.boinc.setJob(ids[2], Job{HasResult: true, ServerState: ServerStateInProgress, User: 5, Host: 6}) // sent, not bound yet
	r.gw.demand = Demand{CancelUnsent: 1}
	if err := r.b.Demand(ctx); err != nil {
		t.Fatal(err)
	}
	if len(r.boinc.cancelled) != 1 || r.boinc.cancelled[0] != ids[1] {
		t.Fatalf("cancelled %v, want the newest unsent %s", r.boinc.cancelled, ids[1])
	}
	if r.gw.reported[ids[1]] != "cancelled" || r.gw.reported[ids[2]] != "" || r.gw.reported[ids[0]] != "" {
		t.Errorf("reported %v", r.gw.reported)
	}
}

func TestCancelUnsentTooCloseToItsDeadline(t *testing.T) {
	r := newRig(t)
	ids := r.created(t, 2)
	r.clk.add(7 * time.Hour) // 53 h left, under the 54 h runway
	r.boinc.setJob(ids[1], Job{HasResult: true, ServerState: ServerStateInProgress, User: 1, Host: 2})
	if err := r.b.Demand(ctx); err != nil {
		t.Fatal(err)
	}
	if len(r.boinc.cancelled) != 1 || r.boinc.cancelled[0] != ids[0] || r.gw.reported[ids[0]] != "cancelled" {
		t.Errorf("cancelled %v reported %v: want only the unsent %s", r.boinc.cancelled, r.gw.reported, ids[0])
	}
}

func TestSyncBindsThenReports(t *testing.T) {
	r := newRig(t)
	ids := r.created(t, 2)
	r.boinc.setJob(ids[0], Job{HasResult: true, ServerState: ServerStateInProgress, User: 7, Host: 9, Team: 3})
	r.boinc.setJob(ids[1], Job{HasResult: true, ServerState: ServerStateInProgress, User: 8, Host: 10})
	if err := r.b.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	if r.gw.binds[ids[0]] != [3]string{"7", "9", "3"} || r.gw.binds[ids[1]] != [3]string{"8", "10", ""} {
		t.Fatalf("binds %v", r.gw.binds)
	}
	if len(r.gw.reported) != 0 {
		t.Fatalf("reported running pilots: %v", r.gw.reported)
	}
	r.gw.pilots[ids[0]].State = pilotConnected
	r.boinc.setJob(ids[0], Job{HasResult: true, ServerState: ServerStateOver, Outcome: OutcomeSuccess, User: 7, Host: 9, Team: 3})
	r.boinc.setJob(ids[1], Job{HasResult: true, ServerState: ServerStateOver, Outcome: OutcomeNoReply, User: 8, Host: 10})
	if err := r.b.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	if r.gw.reported[ids[0]] != "success" || r.gw.reported[ids[1]] != "expired" {
		t.Errorf("reported %v", r.gw.reported)
	}
}

func TestSyncBindsAPilotThatFinishedBeforeItWasSeen(t *testing.T) {
	r := newRig(t)
	id := r.created(t, 1)[0]
	r.boinc.setJob(id, Job{HasResult: true, ServerState: ServerStateOver, Outcome: OutcomeClientError, User: 4, Host: 5})
	if err := r.b.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	if r.gw.binds[id] != [3]string{"4", "5", ""} || r.gw.reported[id] != "error" {
		t.Errorf("binds %v reported %v: the record should say who ran it, then end", r.gw.binds, r.gw.reported)
	}
}

func TestSyncEndsPilotsWithoutAWorkUnit(t *testing.T) {
	r := newRig(t)
	r.gw.demand = Demand{Wanted: 1}
	r.boinc.createErr = errors.New("x")
	r.gw.mu.Lock()
	// A pilot the gateway made whose work unit the bridge never made (it stopped in between).
	r.gw.n++
	r.gw.pilots["p-0000000000aa"] = &Pilot{ID: "p-0000000000aa", State: pilotCreated, Created: r.clk.now(), Deadline: r.clk.now().Add(time.Hour * 100)}
	r.gw.order = append(r.gw.order, "p-0000000000aa")
	r.gw.mu.Unlock()
	r.b.Sync(ctx)
	if r.gw.reported["p-0000000000aa"] != "" {
		t.Fatal("ended a pilot whose work unit may still be in the making")
	}
	r.clk.add(orphanAfter)
	if err := r.b.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	if r.gw.reported["p-0000000000aa"] != "error" {
		t.Errorf("orphan reported %q, want error", r.gw.reported["p-0000000000aa"])
	}
}

func readCredit(t *testing.T, r *rig, id string) (string, bool) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(r.cfg.CreditDir, id))
	if errors.Is(err, os.ErrNotExist) {
		return "", false
	}
	if err != nil {
		t.Fatal(err)
	}
	return string(b), true
}

func TestCreditWaitsForFinalThenWritesIt(t *testing.T) {
	r := newRig(t)
	id := r.created(t, 1)[0]
	r.boinc.setJob(id, Job{HasResult: true, ServerState: ServerStateOver, Outcome: OutcomeSuccess, User: 1, Host: 2})
	finalAt := r.clk.now().Add(24 * time.Hour)
	r.gw.credits[id] = Credit{FinalAt: &finalAt}
	if err := r.b.Credit(ctx); err != nil {
		t.Fatal(err)
	}
	if _, ok := readCredit(t, r, id); ok {
		t.Fatal("credit file before the credit is final")
	}
	// The gateway is not asked again before final_at.
	v := 1234.5
	r.gw.credits[id] = Credit{Final: true, FinalAt: &finalAt, Credit: &v}
	r.clk.add(time.Hour)
	r.b.Credit(ctx)
	if _, ok := readCredit(t, r, id); ok {
		t.Fatal("asked again before final_at")
	}
	r.clk.add(24 * time.Hour)
	if err := r.b.Credit(ctx); err != nil {
		t.Fatal(err)
	}
	if got, ok := readCredit(t, r, id); !ok || got != "1234.5\n" {
		t.Fatalf("credit file %q %v", got, ok)
	}
	if fs, _ := filepath.Glob(filepath.Join(r.cfg.CreditDir, ".*")); len(fs) != 0 {
		t.Errorf("temporary files left: %v", fs)
	}
}

func TestCreditWithheldAndZeroCredit(t *testing.T) {
	r := newRig(t)
	id := r.created(t, 1)[0]
	r.boinc.setJob(id, Job{HasResult: true, ServerState: ServerStateOver, Outcome: OutcomeSuccess, User: 1, Host: 2})
	zero := 0.0
	r.gw.credits[id] = Credit{Final: true, Credit: &zero} // withheld: final 0
	r.b.Credit(ctx)
	if got, _ := readCredit(t, r, id); got != "0\n" {
		t.Errorf("withheld: %q", got)
	}

	r = newRig(t, func(c *Config) { c.ZeroCredit = true })
	id = r.created(t, 1)[0]
	r.boinc.setJob(id, Job{HasResult: true, ServerState: ServerStateOver, Outcome: OutcomeSuccess, User: 1, Host: 2})
	v := 999.0
	r.gw.credits[id] = Credit{Final: true, Credit: &v}
	r.b.Credit(ctx)
	if got, _ := readCredit(t, r, id); got != "0\n" {
		t.Errorf("zero_credit: %q", got)
	}
}

func TestCreditRefusesNonsense(t *testing.T) {
	r := newRig(t)
	id := r.created(t, 1)[0]
	r.boinc.setJob(id, Job{HasResult: true, ServerState: ServerStateOver, Outcome: OutcomeSuccess, User: 1, Host: 2})
	v := -5.0
	r.gw.credits[id] = Credit{Final: true, Credit: &v}
	if err := r.b.Credit(ctx); err == nil {
		t.Error("no error for negative credit")
	}
	if _, ok := readCredit(t, r, id); ok {
		t.Error("wrote negative credit")
	}
}

func TestCleanupRemovesOnlyFinishedCreditFiles(t *testing.T) {
	r := newRig(t)
	ids := []string{"p-000000000001", "p-000000000002", "p-000000000003", "p-000000000004"}
	for _, id := range ids {
		writeCredit(r.cfg.CreditDir, id, 1)
	}
	r.boinc.setJob(ids[0], Job{HasResult: true, ServerState: ServerStateOver, Outcome: OutcomeSuccess, ValidateState: ValidateStateValid})
	r.boinc.setJob(ids[1], Job{HasResult: true, ServerState: ServerStateOver, Outcome: OutcomeSuccess, ValidateState: ValidateStateInconclusive}) // the validator looks again
	// ids[2]: not this bridge's (or purged), new; ids[3]: the same, a month old.
	old := r.clk.now().Add(-31 * 24 * time.Hour)
	os.Chtimes(filepath.Join(r.cfg.CreditDir, ids[3]), old, old)
	if err := r.b.Cleanup(ctx); err != nil {
		t.Fatal(err)
	}
	for i, want := range []bool{false, true, true, false} {
		if _, ok := readCredit(t, r, ids[i]); ok != want {
			t.Errorf("%s: present %v, want %v", ids[i], ok, want)
		}
	}
}

func TestRunOnBOINCWithoutAPassword(t *testing.T) {
	r := newRig(t)
	email := "b0123456789abcdef0123@nomail.example"
	r.gw.requests = []Request{{ID: "r-0000000000000001", Kind: "create_link", Email: email, UserName: "aisathome-abc123"}}
	// The answer fails the first time: the user exists in BOINC, and the next try must find it.
	r.gw.answerSt = 503
	if err := r.b.Requests(ctx); err == nil {
		t.Fatal("no error")
	}
	r.gw.answerSt = 0
	r.b.Requests(ctx)
	if len(r.gw.answers) != 0 {
		t.Fatal("tried again before the retry time")
	}
	r.clk.add(requestRetry)
	if err := r.b.Requests(ctx); err != nil {
		t.Fatal(err)
	}
	a := r.gw.answers["r-0000000000000001"]
	acct := r.boinc.byEmail[email]
	if !a.OK || acct == nil || a.User != "101" || !regexp.MustCompile(`^101_[0-9a-f]{32}$`).MatchString(a.Key) {
		t.Fatalf("answer %+v", a)
	}
	if len(r.boinc.creates) != 2 || r.boinc.creates[0] != r.boinc.creates[1] || !md5Re.MatchString(r.boinc.creates[0]) {
		t.Errorf("password hashes sent: %v (want the same one twice)", r.boinc.creates)
	}
	if len(r.boinc.accounts) != 1 {
		t.Errorf("%d users made", len(r.boinc.accounts))
	}
}

func TestRunOnBOINCWithThePersonsPassword(t *testing.T) {
	r := newRig(t)
	ph := md5Hex("hunter22" + "me@example.org")
	r.gw.requests = []Request{{ID: "r-0000000000000002", Kind: "create_link", Email: "Me@Example.org", UserName: "me",
		PasswdHash: ph, OwnEmail: true}}
	if err := r.b.Requests(ctx); err != nil {
		t.Fatal(err)
	}
	if a := r.gw.answers["r-0000000000000002"]; !a.OK || r.boinc.creates[0] != ph {
		t.Errorf("answer %+v, hash sent %v", a, r.boinc.creates)
	}
}

func TestRunOnBOINCRefusals(t *testing.T) {
	r := newRig(t)
	r.boinc.CreateAccount(ctx, "taken@example.org", md5Hex("other"), "x")
	r.gw.requests = []Request{
		{ID: "r-0000000000000003", Kind: "create_link", Email: "taken@example.org", UserName: "me", PasswdHash: md5Hex("mine")},
		{ID: "r-0000000000000004", Kind: "create_link", Email: "", UserName: "me"},
		{ID: "r-0000000000000005", Kind: "create_link", Email: "a@example.org", UserName: "me", PasswdHash: "nothex"},
		{ID: "r-0000000000000006", Kind: "set_email", User: "5", Email: "a@example.org"},
		{ID: "r-0000000000000007", Kind: "something_new"},
	}
	if err := r.b.Requests(ctx); err != nil {
		t.Fatal(err)
	}
	for id, want := range map[string]string{"r-0000000000000003": "claim it", "r-0000000000000004": "no address",
		"r-0000000000000005": "BOINC's form", "r-0000000000000006": "BOINC site", "r-0000000000000007": "does not know"} {
		a := r.gw.answers[id]
		if a.OK || !strings.Contains(a.Error, want) {
			t.Errorf("%s: %+v, want a refusal with %q", id, a, want)
		}
	}
}

func TestCreateAccountErrorsThatWait(t *testing.T) {
	for _, err := range []error{errors.New("connection refused"), &RPCError{Num: -1, Msg: "Invalid invitation code"},
		&RPCError{Num: errDBCantConnect}} {
		r := newRig(t)
		r.boinc.rpcErr = err
		r.gw.requests = []Request{{ID: "r-0000000000000008", Kind: "create_link", Email: "a@example.org", UserName: "me"}}
		if r.b.Requests(ctx) == nil {
			t.Errorf("%v: no error", err)
		}
		if len(r.gw.answers) != 0 {
			t.Errorf("%v: answered %v; it should wait and try again", err, r.gw.answers)
		}
	}
	a, err := createRefusal(&RPCError{Num: errBadEmailAddr})
	if err != nil || a.OK || a.Error != "BOINC refused the address" {
		t.Errorf("bad address: %+v %v", a, err)
	}
	a, _ = createRefusal(&RPCError{Num: -999})
	if a.OK || !strings.Contains(a.Error, "-999") {
		t.Errorf("unknown error: %+v", a)
	}
}

func TestClaim(t *testing.T) {
	r := newRig(t)
	auth, _ := r.boinc.CreateAccount(ctx, "x@example.org", md5Hex("p"), "x")
	r.boinc.creates = nil
	r.gw.requests = []Request{
		{ID: "r-0000000000000009", Kind: "claim", AccountKey: auth},
		{ID: "r-000000000000000a", Kind: "claim", AccountKey: md5Hex("nobody")},
		{ID: "r-000000000000000b", Kind: "claim", AccountKey: "101_" + md5Hex("weak")},
	}
	if err := r.b.Requests(ctx); err != nil {
		t.Fatal(err)
	}
	if a := r.gw.answers["r-0000000000000009"]; !a.OK || a.User != "101" || !strings.HasPrefix(a.Key, "101_") {
		t.Errorf("claim: %+v", a)
	}
	if a := r.gw.answers["r-000000000000000a"]; a.OK || !strings.Contains(a.Error, "no BOINC account") {
		t.Errorf("unknown key: %+v", a)
	}
	if a := r.gw.answers["r-000000000000000b"]; a.OK || !strings.Contains(a.Error, "not a BOINC account key") {
		t.Errorf("weak key: %+v", a)
	}
}

// The whole round through Step, then the log: no credential, key, address or password hash.
func TestStepLogsNoSecrets(t *testing.T) {
	r := newRig(t)
	auth, _ := r.boinc.CreateAccount(ctx, "claimer@example.org", md5Hex("pw"), "c")
	ph := md5Hex("secretpw" + "own@example.org")
	r.gw.requests = []Request{
		{ID: "r-00000000000000c1", Kind: "create_link", Email: "own@example.org", UserName: "me", PasswdHash: ph, OwnEmail: true},
		{ID: "r-00000000000000c2", Kind: "claim", AccountKey: auth},
	}
	r.gw.demand = Demand{Wanted: 1}
	r.b.Step(ctx)
	id := r.gw.ids()[0]
	r.boinc.setJob(id, Job{HasResult: true, ServerState: ServerStateOver, Outcome: OutcomeSuccess, User: 101, Host: 2})
	v := 10.0
	r.gw.credits[id] = Credit{Final: true, Credit: &v}
	r.clk.add(time.Hour)
	r.b.Step(ctx)
	if got, _ := readCredit(t, r, id); got != "10\n" {
		t.Fatalf("credit %q", got)
	}
	logs := r.logs.String()
	secrets := []string{r.gw.creds[id], auth, ph, "own@example.org", "claimer@example.org", testKey, r.gw.answers["r-00000000000000c1"].Key,
		r.gw.answers["r-00000000000000c2"].Key}
	for f := range r.boinc.works {
		secrets = append(secrets, r.boinc.works[f])
	}
	for _, s := range secrets {
		if s != "" && strings.Contains(logs, s) {
			t.Errorf("the log holds a secret %q:\n%s", s, logs)
		}
	}
	if !strings.Contains(logs, "bridge status") {
		t.Errorf("no status line:\n%s", logs)
	}
}

func TestPasswordIsPerRequestAndKey(t *testing.T) {
	r := newRig(t)
	p1, p2 := r.b.password("r-1"), r.b.password("r-2")
	if p1 == p2 || len(p1) != 32 || p1 != r.b.password("r-1") {
		t.Errorf("passwords %q %q", p1, p2)
	}
	other, _ := New(r.cfg, "another-key", r.boinc, r.boinc, r.boinc, r.b.log)
	if other.password("r-1") == p1 {
		t.Error("the password does not depend on the key")
	}
}

// BOINC sends the work unit while cancel_jobs runs: the pilot is not ended; Sync binds it.
func TestCancelRaceKeepsASentPilot(t *testing.T) {
	r := newRig(t)
	id := r.created(t, 1)[0]
	r.boinc.setJob(id, Job{HasResult: true, ServerState: ServerStateUnsent})
	r.boinc.sendOnCancel = 21
	r.gw.demand = Demand{CancelUnsent: 1}
	if err := r.b.Demand(ctx); err != nil {
		t.Fatal(err)
	}
	if len(r.boinc.cancelled) != 1 || r.gw.reported[id] != "" {
		t.Fatalf("cancelled %v, reported %v: a pilot BOINC just sent was ended", r.boinc.cancelled, r.gw.reported)
	}
	r.boinc.setJob(id, Job{HasResult: true, ServerState: ServerStateInProgress, ErrorMask: WUErrorCancelled, User: 1, Host: 21})
	if err := r.b.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	if r.gw.binds[id] != [3]string{"1", "21", ""} || r.gw.reported[id] != "" {
		t.Errorf("binds %v reported %v", r.gw.binds, r.gw.reported)
	}
}

// create_work inserted the work unit, then failed (a timeout): the credential and the pilot stay.
func TestCreateWorkThatFailedAfterInserting(t *testing.T) {
	r := newRig(t)
	r.boinc.createWorkThenFail = true
	r.gw.demand = Demand{Wanted: 1}
	if err := r.b.Demand(ctx); err != nil {
		t.Fatal(err)
	}
	id := r.gw.ids()[0]
	if r.gw.reported[id] != "" || len(r.boinc.staged) != 1 {
		t.Errorf("reported %v, staged %d: a work unit that exists lost its pilot or credential", r.gw.reported, len(r.boinc.staged))
	}
}

// The validator's first look, before the file exists, leaves a lone result inconclusive: its
// credit is still written. And a backlog of results waiting in the gateway's hold does not keep a
// later result whose credit is final out of the pass.
func TestCreditForInconclusiveResultsBeyondTheFirstPage(t *testing.T) {
	r := newRig(t)
	for i := 0; i < creditPage+50; i++ {
		r.boinc.setJob(fmt.Sprintf("p-%012x", i), Job{HasResult: true, ServerState: ServerStateOver, Outcome: OutcomeSuccess})
	}
	last := fmt.Sprintf("p-%012x", creditPage+49)
	r.boinc.setJob(last, Job{HasResult: true, ServerState: ServerStateOver, Outcome: OutcomeSuccess, ValidateState: ValidateStateInconclusive})
	v := 42.0
	r.gw.credits[last] = Credit{Final: true, Credit: &v} // the others: unknown to the gateway (404)
	r.b.Credit(ctx)
	if got, _ := readCredit(t, r, last); got != "42\n" {
		t.Fatalf("credit of the inconclusive result past the first page: %q", got)
	}
	if len(r.boinc.validated) != 0 {
		t.Errorf("validator run without one configured: %v", r.boinc.validated)
	}
	if r.boinc.creditCalls != 2 {
		t.Errorf("%d pages read, want 2", r.boinc.creditCalls)
	}
}

// With a validator configured, the bridge runs it on the work unit right after writing its credit
// file; a failed run is tried again on a later pass (not sooner than validateRetry); a validated
// result is left alone.
func TestCreditRunsTheValidator(t *testing.T) {
	r := newRig(t, func(c *Config) { c.Validator, c.MaxGrantedCredit = "bin/pilot_validator", 150000 })
	ids := r.created(t, 2)
	for _, id := range ids {
		r.boinc.setJob(id, Job{HasResult: true, ServerState: ServerStateOver, Outcome: OutcomeSuccess, User: 1, Host: 2})
	}
	v := 7.5
	r.gw.credits[ids[1]] = Credit{Final: true, Credit: &v} // ids[0]: still in the hold
	finalAt := r.clk.now().Add(time.Hour)
	r.gw.credits[ids[0]] = Credit{FinalAt: &finalAt}
	if err := r.b.Credit(ctx); err != nil {
		t.Fatal(err)
	}
	want := r.boinc.wuIDs[ids[1]]
	if len(r.boinc.validated) != 1 || r.boinc.validated[0] != want || r.boinc.jobs[ids[1]].ValidateState != ValidateStateValid {
		t.Fatalf("validated %v (want only work unit %d), state %d", r.boinc.validated, want, r.boinc.jobs[ids[1]].ValidateState)
	}
	r.clk.add(time.Minute)
	r.b.Credit(ctx)
	if len(r.boinc.validated) != 1 {
		t.Fatalf("ran again: %v", r.boinc.validated)
	}

	// A failed run (the bridge's file is there, the result still waits): tried again, not at once.
	r.clk.add(time.Hour)
	r.gw.credits[ids[0]] = Credit{Final: true, FinalAt: &finalAt, Credit: &v}
	r.boinc.validateErr = errors.New("validator: exit status 1")
	if err := r.b.Credit(ctx); err == nil {
		t.Fatal("no error for a failed validator run")
	}
	r.boinc.validateErr = nil
	r.clk.add(time.Minute)
	r.b.Credit(ctx)
	if n := len(r.boinc.validated); n != 2 {
		t.Fatalf("%d runs: retried before validateRetry", n)
	}
	r.clk.add(validateRetry)
	if err := r.b.Credit(ctx); err != nil {
		t.Fatal(err)
	}
	if n := len(r.boinc.validated); n != 3 || r.boinc.jobs[ids[0]].ValidateState != ValidateStateValid {
		t.Errorf("%d runs, state %d: the retry did not validate it", n, r.boinc.jobs[ids[0]].ValidateState)
	}
}

// A validator run killed after its claim (between the framework's writes): reported, and never run
// again on that result, since running again could grant twice.
func TestValidatorRunInterruptedAfterItsClaim(t *testing.T) {
	r := newRig(t, func(c *Config) { c.Validator, c.MaxGrantedCredit = "bin/pilot_validator", 150000 })
	id := r.created(t, 1)[0]
	r.boinc.setJob(id, Job{HasResult: true, ServerState: ServerStateOver, Outcome: OutcomeSuccess, User: 1, Host: 2})
	v := 3.0
	r.gw.credits[id] = Credit{Final: true, Credit: &v}
	r.boinc.dieAfterClaim = true
	err := r.b.Credit(ctx)
	if err == nil || !strings.Contains(err.Error(), "claimed it but did not finish") {
		t.Fatalf("error %v", err)
	}
	r.boinc.dieAfterClaim = false
	for i := 0; i < 3; i++ {
		r.clk.add(validateRetry)
		err = r.b.Credit(ctx)
	}
	if len(r.boinc.validated) != 1 {
		t.Fatalf("the validator ran %d times on an interrupted result", len(r.boinc.validated))
	}
	if err == nil || !strings.Contains(err.Error(), "check by hand") {
		t.Errorf("later passes: %v", err)
	}
}

// The validator ends with 0 having granted nothing (a check_set error): an error with its output,
// judged by the database, not counted as validated.
func TestValidatorExitZeroWithoutAGrant(t *testing.T) {
	r := newRig(t, func(c *Config) { c.Validator, c.MaxGrantedCredit = "bin/pilot_validator", 150000 })
	id := r.created(t, 1)[0]
	r.boinc.setJob(id, Job{HasResult: true, ServerState: ServerStateOver, Outcome: OutcomeSuccess, User: 1, Host: 2})
	v := 3.0
	r.gw.credits[id] = Credit{Final: true, Credit: &v}
	r.boinc.noGrant = true
	err := r.b.Credit(ctx)
	if err == nil || !strings.Contains(err.Error(), "left it waiting") || !strings.Contains(err.Error(), "check_set() error") {
		t.Fatalf("error %v", err)
	}
	if r.b.st.validated != 0 {
		t.Error("counted as validated")
	}
}

// A result another validator pass left inconclusive gets its file but no validator run (BOINC
// would not look at it): reported for a person.
func TestInconclusiveResultIsReportedNotRun(t *testing.T) {
	r := newRig(t, func(c *Config) { c.Validator, c.MaxGrantedCredit = "bin/pilot_validator", 150000 })
	id := r.created(t, 1)[0]
	r.boinc.setJob(id, Job{HasResult: true, ServerState: ServerStateOver, Outcome: OutcomeSuccess, User: 1, Host: 2,
		ValidateState: ValidateStateInconclusive})
	v := 3.0
	r.gw.credits[id] = Credit{Final: true, Credit: &v}
	err := r.b.Credit(ctx)
	if err == nil || !strings.Contains(err.Error(), "need_validate") || len(r.boinc.validated) != 0 {
		t.Fatalf("error %v, runs %v", err, r.boinc.validated)
	}
	if got, _ := readCredit(t, r, id); got != "3\n" {
		t.Errorf("credit file %q", got)
	}
}

// Validator runs in one pass start only within the budget; the rest run on the next pass.
func TestValidateBudget(t *testing.T) {
	r := newRig(t, func(c *Config) { c.Validator, c.MaxGrantedCredit = "bin/pilot_validator", 150000 })
	r.b.validateBudget = time.Millisecond
	r.boinc.validateDelay = 20 * time.Millisecond
	ids := r.created(t, 3)
	v := 1.0
	for _, id := range ids {
		r.boinc.setJob(id, Job{HasResult: true, ServerState: ServerStateOver, Outcome: OutcomeSuccess, User: 1, Host: 2})
		r.gw.credits[id] = Credit{Final: true, Credit: &v}
	}
	r.b.Credit(ctx)
	if n := len(r.boinc.validated); n != 1 {
		t.Fatalf("%d runs in the first pass, want 1", n)
	}
	r.clk.add(time.Minute)
	r.b.Credit(ctx)
	if n := len(r.boinc.validated); n != 2 {
		t.Errorf("%d runs after the second pass, want 2", n)
	}
}

// tiered is a rig whose bridge has two tiers, as the 24 GB and 16 GB apps of the project.
func tiered(t *testing.T) *rig {
	return newRig(t, func(c *Config) {
		c.App, c.Model = "", RemoteFile{}
		c.Tiers = []Tier{
			{Name: "big", App: "pilot", Model: RemoteFile{URL: "https://example.org/iq4.gguf", NBytes: 14252845984, MD5: strings.Repeat("a", 32)}},
			{Name: "standard", App: "pilot_std", Model: RemoteFile{URL: "https://example.org/q3.gguf", NBytes: 13146393504, MD5: strings.Repeat("b", 32)}},
		}
	})
}

func TestTieredDemandCreatesEachTiersPilotsInItsApp(t *testing.T) {
	r := tiered(t)
	r.gw.demand = Demand{Wanted: 3, Tiers: map[string]TierDemand{"big": {Wanted: 1}, "standard": {Wanted: 2}}}
	if err := r.b.Demand(ctx); err != nil {
		t.Fatal(err)
	}
	byTier := map[string]int{}
	for _, id := range r.gw.ids() {
		p := r.gw.pilots[id]
		byTier[p.Tier]++
		app, model := r.boinc.jobs[id].App, r.boinc.models[id]
		switch p.Tier {
		case "big":
			if app != "pilot" || model != "https://example.org/iq4.gguf" {
				t.Errorf("big pilot %s: app %q, model %q", id, app, model)
			}
		case "standard":
			if app != "pilot_std" || model != "https://example.org/q3.gguf" {
				t.Errorf("standard pilot %s: app %q, model %q", id, app, model)
			}
		default:
			t.Errorf("pilot %s of tier %q", id, p.Tier)
		}
	}
	if byTier["big"] != 1 || byTier["standard"] != 2 {
		t.Errorf("pilots by tier: %v", byTier)
	}
}

func TestTieredBridgeNeedsTheGatewaysTiers(t *testing.T) {
	r := tiered(t)
	// A connector without pilot_tiers: nothing is created, and the bridge says why.
	r.gw.demand = Demand{Wanted: 2}
	if err := r.b.Demand(ctx); err == nil || !strings.Contains(err.Error(), "pilot_tiers") {
		t.Errorf("demand without tiers: %v", err)
	}
	if len(r.gw.ids()) != 0 {
		t.Errorf("created %v without the gateway's tiers", r.gw.ids())
	}
	// A tier the bridge does not have: an error, the others still served.
	r.gw.demand = Demand{Wanted: 2, Tiers: map[string]TierDemand{"huge": {Wanted: 1}, "standard": {Wanted: 1}}}
	if err := r.b.Demand(ctx); err == nil || !strings.Contains(err.Error(), `"huge"`) {
		t.Errorf("unknown tier: %v", err)
	}
	if ids := r.gw.ids(); len(ids) != 1 || r.gw.pilots[ids[0]].Tier != "standard" {
		t.Errorf("pilots: %v", ids)
	}
}

func TestTieredDemandCancelsPerTier(t *testing.T) {
	r := tiered(t)
	r.gw.demand = Demand{Wanted: 4, Tiers: map[string]TierDemand{"big": {Wanted: 2}, "standard": {Wanted: 2}}}
	if err := r.b.Demand(ctx); err != nil {
		t.Fatal(err)
	}
	ids := r.gw.ids() // big, big, standard, standard
	r.clk.add(time.Minute)
	r.gw.demand = Demand{CancelUnsent: 1, Tiers: map[string]TierDemand{"big": {CancelUnsent: 1}, "standard": {}}}
	if err := r.b.Demand(ctx); err != nil {
		t.Fatal(err)
	}
	// The newest big one only; the tier comes from the work unit's app.
	if len(r.boinc.cancelled) != 1 || r.boinc.cancelled[0] != ids[1] {
		t.Errorf("cancelled %v, want [%s]", r.boinc.cancelled, ids[1])
	}
	// Too close to its deadline: cancelled whatever its tier's demand says.
	r.clk.add(7 * time.Hour)
	r.gw.demand = Demand{Tiers: map[string]TierDemand{"big": {}, "standard": {}}}
	if err := r.b.Demand(ctx); err != nil {
		t.Fatal(err)
	}
	if len(r.boinc.cancelled) != 4 {
		t.Errorf("cancelled %v: every unsent one is past its runway", r.boinc.cancelled)
	}
}

func TestTieredCreditRunsEachWorkUnitsValidatorApp(t *testing.T) {
	r := tiered(t)
	r.b.cfg.Validator, r.b.cfg.MaxGrantedCredit = "bin/pilot_validator", 150000
	r.gw.demand = Demand{Wanted: 2, Tiers: map[string]TierDemand{"big": {Wanted: 1}, "standard": {Wanted: 1}}}
	if err := r.b.Demand(ctx); err != nil {
		t.Fatal(err)
	}
	five := 5.0
	for _, id := range r.gw.ids() {
		j := r.boinc.jobs[id]
		j.HasResult, j.ServerState, j.Outcome, j.Host, j.User = true, ServerStateOver, OutcomeSuccess, 3, 4
		r.boinc.setJob(id, j)
		r.gw.credits[id] = Credit{Final: true, Credit: &five}
	}
	if err := r.b.Credit(ctx); err != nil {
		t.Fatal(err)
	}
	if len(r.boinc.validatedApps) != 2 {
		t.Errorf("validator runs %v", r.boinc.validatedApps)
	}
	for _, id := range r.gw.ids() {
		if got, want := r.boinc.validatedFor[r.boinc.wuIDs[id]], r.boinc.jobs[id].App; got != want {
			t.Errorf("work unit %s of app %s validated with --app %q", id, want, got)
		}
	}
}

func TestTieredCheckWantsTheGatewaysTiers(t *testing.T) {
	r := tiered(t)
	for _, p := range []string{"bin/create_work", "bin/cancel_jobs", "bin/dir_hier_path"} {
		os.MkdirAll(filepath.Join(r.cfg.ProjectDir, "bin"), 0o755)
		os.WriteFile(filepath.Join(r.cfg.ProjectDir, p), []byte("#!/bin/sh\n"), 0o755)
	}
	for _, c := range []struct {
		tiers map[string]TierDemand
		ok    bool
	}{
		{nil, false},
		{map[string]TierDemand{"big": {}}, false},
		{map[string]TierDemand{"big": {}, "standard": {}, "huge": {}}, false},
		{map[string]TierDemand{"big": {}, "standard": {}}, true},
	} {
		r.gw.demand = Demand{Tiers: c.tiers}
		if err := r.b.Check(ctx); (err == nil) != c.ok {
			t.Errorf("gateway tiers %v: %v", c.tiers, err)
		}
	}
}

func TestTieredDemandGoesOnPastAFailingTier(t *testing.T) {
	r := tiered(t)
	r.boinc.failApp = "pilot" // the big tier's app is broken
	r.gw.demand = Demand{Wanted: 2, Tiers: map[string]TierDemand{"big": {Wanted: 1}, "standard": {Wanted: 1}}}
	if err := r.b.Demand(ctx); err == nil {
		t.Error("no error for the failing tier")
	}
	if len(r.boinc.works) != 1 {
		t.Fatalf("work units %v: the standard tier must still get its pilot", r.boinc.works)
	}
	for id := range r.boinc.works {
		if r.boinc.jobs[id].App != "pilot_std" {
			t.Errorf("work unit %s in %s", id, r.boinc.jobs[id].App)
		}
	}
}
