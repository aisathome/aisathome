// Science task coordinator (decision 0015, phase 1): a project's task queue lives in the gateway.
// The project's tools submit tasks; workers (the agent runner) claim the highest-priority queued
// task with a lease, renew it with heartbeats, and report a result or a failure. A lease that is
// not renewed in time returns the task to the queue (or fails it after max_tries leases), so a
// worker that dies never holds a task for ever. The gateway is the single source of truth for
// the queue and its state; validating a result (review, CI) is a separate step outside it.
//
// Endpoints, all with the project's own key (401 without one, 403 with another project's):
//
//	POST /v1/projects/{id}/tasks                    create (idempotent by the client's task id)
//	GET  /v1/projects/{id}/tasks?state=&kind=       list (summaries; also an admin's session)
//	GET  /v1/projects/{id}/tasks/{task}             one task with its spec and result (same)
//	POST /v1/projects/{id}/tasks/claim              take the next task and a lease (204: none)
//	POST /v1/projects/{id}/tasks/{task}/heartbeat   extend the lease
//	POST /v1/projects/{id}/tasks/{task}/complete    the result (state done)
//	POST /v1/projects/{id}/tasks/{task}/fail        give the task up (requeue or failed)
//	POST /v1/projects/{id}/tasks/{task}/cancel      queued or leased -> cancelled
//
// Claim order: tasks that were leased before and came back (an expired lease, a worker's
// requeue) first, as the runner resumes interrupted work first; then by class (1 fix rounds and
// follow-ups, 2 first attempts, 3 other work); oldest first within a class.
//
// Nothing here is public: specs, prompts and diffs before a pull request stay with the project
// key and the admins. Each task is one file under state_dir/tasks-<project>/, written atomically
// on every change, so the queue survives a restart; the number of tasks and the bytes kept are
// bounded per project.
//
// SPDX-License-Identifier: Apache-2.0
package gateway

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	maxTaskCreateBody   = 128 << 10 // a create request (spec at most maxTaskSpec)
	maxTaskSpec         = 64 << 10  // the opaque spec the worker needs
	maxTaskControlBody  = 8 << 10   // claim, heartbeat, fail, cancel
	maxTaskCompleteBody = 3 << 20   // a result: the diff (≤ 1 MB) plus JSON escaping
	maxTaskDiff         = 1 << 20
	maxTaskPRBody       = 64 << 10
	maxTaskCommitMsg    = 8 << 10
	maxTaskScore        = 16 << 10

	// maxOpenTasks bounds queued plus leased tasks per project; create refuses beyond it.
	maxOpenTasks = 500
	// maxKeptTasks and maxKeptBytes bound finished tasks (done, failed, cancelled) per project:
	// beyond either, the oldest finished ones are dropped (phase 3 exports them to the ledger).
	maxKeptTasks = 200
	maxKeptBytes = 64 << 20

	defaultLeaseSec = 600
	minLeaseSec     = 30
	maxLeaseSec     = 3600
	defaultMaxTries = 3 // the runner's max_tries
	maxMaxTries     = 10
	maxTaskEvents   = 20
	// taskSweepEvery: expired leases return to the queue at least this often, and on every call.
	taskSweepEvery = 30 * time.Second
)

// Task states.
const (
	TaskQueued    = "queued"
	TaskLeased    = "leased"
	TaskDone      = "done"
	TaskFailed    = "failed"
	TaskCancelled = "cancelled"
)

var (
	queueStates = []string{TaskQueued, TaskLeased, TaskDone, TaskFailed, TaskCancelled}
	// taskKinds and their claim class (lower first): the owner's order of 2026-10-27, "fix
	// rounds have higher priority than regular tasks and other projects".
	taskKinds = map[string]int{"fix_round": 1, "followup": 1, "attempt": 2, "other": 3}
	taskRefRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]{0,199}$`)
)

// Task is one unit of work in a project's queue.
type Task struct {
	ID        string          `json:"id"` // the client's id (the runner's attempt id), unique per project
	Project   string          `json:"project"`
	Kind      string          `json:"kind"`  // attempt, fix_round, followup, other
	Class     int             `json:"class"` // 1 fix/follow-up, 2 attempt, 3 other (from kind)
	Title     string          `json:"title,omitempty"`
	Spec      json.RawMessage `json:"spec,omitempty"` // opaque to the gateway: what the worker needs
	BaseRef   string          `json:"base_ref,omitempty"`
	PR        int             `json:"pr,omitempty"` // the pull request a fix round revises
	CreatedBy string          `json:"created_by"`
	State     string          `json:"state"`
	Tries     int             `json:"tries"` // leases granted so far
	MaxTries  int             `json:"max_tries"`
	// Requeued: the task was leased before and came back to the queue; it is claimed first.
	Requeued  bool        `json:"requeued,omitempty"`
	Seq       int64       `json:"seq"` // creation order within the project
	CreatedAt time.Time   `json:"created_at"`
	UpdatedAt time.Time   `json:"updated_at"`
	Lease     *taskLease  `json:"lease,omitempty"` // the current lease, or the last one when finished
	Result    *TaskResult `json:"result,omitempty"`
	Error     string      `json:"error,omitempty"` // the last failure, expiry or cancel reason
	Events    []taskEvent `json:"events,omitempty"`

	size int // bytes on disk, for maxKeptBytes
}

type taskLease struct {
	ID          string    `json:"id,omitempty"` // only the claimer gets it; views leave it out
	Worker      string    `json:"worker"`
	TTLSec      int       `json:"ttl_seconds"`
	Since       time.Time `json:"since"`
	HeartbeatAt time.Time `json:"heartbeat_at"`
	ExpiresAt   time.Time `json:"expires_at"`
}

type taskEvent struct {
	At     time.Time `json:"at"`
	What   string    `json:"what"` // created, claimed, heartbeat-less expiry, done, failed, requeued, cancelled
	Worker string    `json:"worker,omitempty"`
	Note   string    `json:"note,omitempty"`
}

// TaskResult is what a worker reports when it finishes a task. The raw evidence (sessions,
// logs, the sandbox) stays on the worker; RawRef names where.
type TaskResult struct {
	Outcome          string          `json:"outcome"` // short: "candidate", "lake_build failed", ...
	ChecksPassed     *bool           `json:"checks_passed,omitempty"`
	Score            json.RawMessage `json:"score,omitempty"` // a summary object, opaque
	Diff             string          `json:"diff,omitempty"`  // model.diff
	PRBody           string          `json:"pr_body,omitempty"`
	CommitMsg        string          `json:"commit_msg,omitempty"`
	Steps            int             `json:"steps"`
	PromptTokens     int64           `json:"prompt_tokens"`
	CompletionTokens int64           `json:"completion_tokens"`
	ModelMin         float64         `json:"model_min"`
	AwakeMin         float64         `json:"awake_min"`
	RawRef           string          `json:"raw_ref,omitempty"` // e.g. raw/2026-10-27/T2-NS-2.a-F1 on the runner
	FinishedAt       time.Time       `json:"finished_at"`
}

func (r *TaskResult) validate() error {
	errs := []error{checkText("result.outcome", r.Outcome, 200), checkNum("result.model_min", r.ModelMin, 1e5),
		checkNum("result.awake_min", r.AwakeMin, 1e5)}
	if r.Outcome == "" {
		errs = append(errs, errors.New("result.outcome: required"))
	}
	if len(r.Diff) > maxTaskDiff || len(r.PRBody) > maxTaskPRBody || len(r.CommitMsg) > maxTaskCommitMsg {
		errs = append(errs, fmt.Errorf("result: diff at most %d bytes, pr_body %d, commit_msg %d", maxTaskDiff, maxTaskPRBody, maxTaskCommitMsg))
	}
	if r.Score != nil {
		if err := checkObject("result.score", r.Score, maxTaskScore); err != nil {
			errs = append(errs, err)
		}
	}
	if r.Steps < 0 || r.Steps > 100000 || r.PromptTokens < 0 || r.PromptTokens > 1e12 || r.CompletionTokens < 0 || r.CompletionTokens > 1e12 {
		errs = append(errs, errors.New("result: steps or tokens out of range"))
	}
	if r.RawRef != "" && (len(r.RawRef) > 512 || !utf8Printable(r.RawRef)) {
		errs = append(errs, errors.New("result.raw_ref: at most 512 printable bytes"))
	}
	return errors.Join(errs...)
}

func utf8Printable(s string) bool {
	for _, r := range s {
		if r < 0x20 || r == 0x7f || r == 0xfffd {
			return false
		}
	}
	return true
}

// checkObject accepts a JSON object of at most max bytes.
func checkObject(field string, raw json.RawMessage, max int) error {
	if len(raw) > max {
		return fmt.Errorf("%s: at most %d bytes", field, max)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil || m == nil {
		return fmt.Errorf("%s: must be a JSON object", field)
	}
	return nil
}

// canonicalJSON re-encodes a JSON value with sorted keys and no spaces (numbers kept as
// written), so that the same spec compares equal however a client formats it.
func canonicalJSON(raw json.RawMessage) (json.RawMessage, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	return json.Marshal(v)
}

// taskNew is a create request.
type taskNew struct {
	ID        string          `json:"id"`
	Kind      string          `json:"kind"`
	Title     string          `json:"title,omitempty"`
	Spec      json.RawMessage `json:"spec"`
	BaseRef   string          `json:"base_ref,omitempty"`
	PR        int             `json:"pr,omitempty"`
	CreatedBy string          `json:"created_by"`
	MaxTries  int             `json:"max_tries,omitempty"`
}

func (n *taskNew) validate() error {
	errs := []error{checkID("id", n.ID), checkID("created_by", n.CreatedBy), checkText("title", n.Title, 200)}
	if _, ok := taskKinds[n.Kind]; !ok {
		errs = append(errs, errors.New("kind: attempt, fix_round, followup or other"))
	}
	if n.Spec == nil {
		errs = append(errs, errors.New("spec: required (a JSON object)"))
	} else if err := checkObject("spec", n.Spec, maxTaskSpec); err != nil {
		errs = append(errs, err)
	}
	if n.BaseRef != "" && !taskRefRe.MatchString(n.BaseRef) {
		errs = append(errs, errors.New("base_ref: a branch or commit, 1-200 of letters, digits, '.', '_', '/' or '-'"))
	}
	if n.PR < 0 || n.PR > 999999999 {
		errs = append(errs, errors.New("pr: a pull request number"))
	}
	if n.MaxTries < 0 || n.MaxTries > maxMaxTries {
		errs = append(errs, fmt.Errorf("max_tries: 1 to %d (0: %d)", maxMaxTries, defaultMaxTries))
	}
	return errors.Join(errs...)
}

// taskControl is the body of claim, heartbeat, complete, fail and cancel (the fields each needs).
type taskControl struct {
	Worker   string      `json:"worker,omitempty"`
	Lease    string      `json:"lease,omitempty"`
	LeaseSec int         `json:"lease_seconds,omitempty"` // claim: the lease's TTL
	Kinds    []string    `json:"kinds,omitempty"`         // claim: only these kinds
	Result   *TaskResult `json:"result,omitempty"`        // complete
	Reason   string      `json:"reason,omitempty"`        // fail, cancel
	Requeue  bool        `json:"requeue,omitempty"`       // fail: back to the queue if tries are left
}

// projectTasks is one project's queue.
type projectTasks struct {
	byID map[string]*Task
	seq  int64
}

// taskStore keeps every project's tasks in memory and in state_dir.
type taskStore struct {
	mu   sync.Mutex
	dir  string // state dir; "" = memory only
	now  func() time.Time
	log  *slog.Logger
	byPr map[string]*projectTasks
}

func newTaskStore(dir string, log *slog.Logger) *taskStore {
	return &taskStore{dir: dir, now: time.Now, log: log, byPr: map[string]*projectTasks{}}
}

func (s *taskStore) pdir(project string) string { return filepath.Join(s.dir, "tasks-"+project) }

// load returns a project's queue, reading it from state_dir the first time. Holds s.mu.
func (s *taskStore) load(project string) *projectTasks {
	if p, ok := s.byPr[project]; ok {
		return p
	}
	p := &projectTasks{byID: map[string]*Task{}}
	s.byPr[project] = p
	if s.dir == "" || !projectID.MatchString(project) {
		return p
	}
	ents, err := os.ReadDir(s.pdir(project))
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			s.log.Warn("task dir unreadable", "project", project, "err", err)
		}
		return p
	}
	for _, e := range ents {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".json") || strings.HasPrefix(name, ".") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(s.pdir(project), name))
		var t Task
		if err == nil {
			err = json.Unmarshal(raw, &t)
		}
		if err == nil && (t.ID+".json" != name || t.Project != project || !slices.Contains(queueStates, t.State)) {
			err = errors.New("does not match its file name, project or a state")
		}
		if err != nil {
			s.log.Warn("task file unreadable, ignoring it", "project", project, "file", name, "err", err)
			continue
		}
		t.size = len(raw)
		p.byID[t.ID] = &t
		p.seq = max(p.seq, t.Seq)
	}
	return p
}

// save writes one task (holds s.mu).
func (s *taskStore) save(t *Task) {
	raw, _ := json.Marshal(t)
	t.size = len(raw)
	if s.dir == "" {
		return
	}
	if err := os.MkdirAll(s.pdir(t.Project), 0o700); err == nil {
		err = writeFileAtomic(filepath.Join(s.pdir(t.Project), t.ID+".json"), raw)
		if err == nil {
			return
		}
		s.log.Warn("saving task failed", "project", t.Project, "task", t.ID, "err", err)
	} else {
		s.log.Warn("task dir", "project", t.Project, "err", err)
	}
}

func (s *taskStore) drop(t *Task) {
	delete(s.byPr[t.Project].byID, t.ID)
	if s.dir != "" {
		if err := os.Remove(filepath.Join(s.pdir(t.Project), t.ID+".json")); err != nil && !errors.Is(err, os.ErrNotExist) {
			s.log.Warn("removing task failed", "project", t.Project, "task", t.ID, "err", err)
		}
	}
}

func (t *Task) event(at time.Time, what, worker, note string) {
	t.Events = append(t.Events, taskEvent{At: at, What: what, Worker: worker, Note: note})
	if n := len(t.Events) - maxTaskEvents; n > 0 {
		t.Events = slices.Delete(t.Events, 0, n)
	}
	t.UpdatedAt = at
}

func finished(state string) bool {
	return state == TaskDone || state == TaskFailed || state == TaskCancelled
}

// giveBack returns a leased task to the queue, or fails it when its tries are used up. Holds s.mu.
func (s *taskStore) giveBack(t *Task, now time.Time, what, reason string) {
	worker := t.Lease.Worker
	t.Error = reason
	if t.Tries < t.MaxTries {
		t.State, t.Requeued = TaskQueued, true
		t.Lease = nil
		t.event(now, what, worker, reason+"; back in the queue")
	} else {
		t.State = TaskFailed
		t.event(now, what, worker, fmt.Sprintf("%s; failed after %d tries", reason, t.Tries))
	}
	s.save(t)
	s.log.Info("task given back", "project", t.Project, "task", t.ID, "worker", worker, "why", what, "state", t.State, "tries", t.Tries)
}

// sweep returns the project's expired leases to the queue. Holds s.mu.
func (s *taskStore) sweep(p *projectTasks, now time.Time) {
	for _, t := range p.byID {
		if t.State == TaskLeased && !now.Before(t.Lease.ExpiresAt) {
			s.giveBack(t, now, "expired", "lease expired (no heartbeat)")
		}
	}
}

// sweepAll sweeps every loaded project (the background check).
func (s *taskStore) sweepAll() {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	for _, p := range s.byPr {
		s.sweep(p, now)
	}
}

// open loads and sweeps a project's queue; the caller holds s.mu.
func (s *taskStore) open(project string) (*projectTasks, time.Time) {
	now := s.now()
	p := s.load(project)
	s.sweep(p, now)
	return p, now
}

// prune drops the oldest finished tasks beyond maxKeptTasks or maxKeptBytes. Holds s.mu.
func (s *taskStore) prune(p *projectTasks) {
	var fin []*Task
	kept := 0
	for _, t := range p.byID {
		if finished(t.State) {
			fin = append(fin, t)
			kept += t.size
		}
	}
	sort.Slice(fin, func(i, j int) bool { return fin[i].UpdatedAt.Before(fin[j].UpdatedAt) })
	for len(fin) > 0 && (len(fin) > maxKeptTasks || kept > maxKeptBytes) {
		kept -= fin[0].size
		s.drop(fin[0])
		fin = fin[1:]
	}
}

// taskErr is an error with its HTTP status.
type taskErr struct {
	code int
	msg  string
}

func (e *taskErr) Error() string { return e.msg }

func terr(code int, format string, a ...any) error { return &taskErr{code, fmt.Sprintf(format, a...)} }

// create adds a task, or returns the existing one with the same id and content (created false).
func (s *taskStore) create(project string, n *taskNew) (*Task, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, now := s.open(project)
	spec, err := canonicalJSON(n.Spec)
	if err != nil {
		return nil, false, terr(http.StatusBadRequest, "spec: %v", err)
	}
	maxTries := n.MaxTries
	if maxTries == 0 {
		maxTries = defaultMaxTries
	}
	if t, ok := p.byID[n.ID]; ok {
		if t.Kind != n.Kind || t.Title != n.Title || !bytes.Equal(t.Spec, spec) || t.BaseRef != n.BaseRef ||
			t.PR != n.PR || t.CreatedBy != n.CreatedBy || t.MaxTries != maxTries {
			return nil, false, terr(http.StatusConflict, "task %s exists with other content; a new task needs a new id", n.ID)
		}
		return t.view(true), false, nil
	}
	open := 0
	for _, t := range p.byID {
		if !finished(t.State) {
			open++
		}
	}
	if open >= maxOpenTasks {
		return nil, false, terr(http.StatusTooManyRequests, "at most %d open tasks per project", maxOpenTasks)
	}
	p.seq++
	t := &Task{ID: n.ID, Project: project, Kind: n.Kind, Class: taskKinds[n.Kind], Title: n.Title, Spec: spec,
		BaseRef: n.BaseRef, PR: n.PR, CreatedBy: n.CreatedBy, State: TaskQueued, MaxTries: maxTries, Seq: p.seq, CreatedAt: now}
	t.event(now, "created", "", n.CreatedBy)
	p.byID[t.ID] = t
	s.save(t)
	s.prune(p)
	s.log.Info("task created", "project", project, "task", t.ID, "kind", t.Kind, "by", t.CreatedBy)
	return t.view(true), true, nil
}

// claimOrder sorts queued tasks: requeued first, then class, then creation order.
func claimOrder(ts []*Task) {
	sort.Slice(ts, func(i, j int) bool {
		a, b := ts[i], ts[j]
		if a.Requeued != b.Requeued {
			return a.Requeued
		}
		if a.Class != b.Class {
			return a.Class < b.Class
		}
		return a.Seq < b.Seq
	})
}

// claim leases the next queued task to worker, or returns nil when none is queued.
func (s *taskStore) claim(project, worker string, ttl int, kinds []string) (*Task, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, now := s.open(project)
	var q []*Task
	for _, t := range p.byID {
		if t.State == TaskQueued && (len(kinds) == 0 || slices.Contains(kinds, t.Kind)) {
			q = append(q, t)
		}
	}
	if len(q) == 0 {
		return nil, nil
	}
	claimOrder(q)
	t := q[0]
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return nil, err
	}
	t.State, t.Tries, t.Error = TaskLeased, t.Tries+1, ""
	t.Lease = &taskLease{ID: hex.EncodeToString(id[:]), Worker: worker, TTLSec: ttl, Since: now, HeartbeatAt: now,
		ExpiresAt: now.Add(time.Duration(ttl) * time.Second)}
	t.event(now, "claimed", worker, fmt.Sprintf("try %d of %d", t.Tries, t.MaxTries))
	s.save(t)
	s.log.Info("task claimed", "project", project, "task", t.ID, "worker", worker, "try", t.Tries)
	out := t.view(true)
	out.Lease.ID = t.Lease.ID // only the claimer sees it
	return out, nil
}

// leased finds a task held by worker with lease id, or explains why not (409, 404).
func (s *taskStore) leased(p *projectTasks, id, worker, lease string) (*Task, error) {
	t, ok := p.byID[id]
	if !ok {
		return nil, terr(http.StatusNotFound, "no task %s", id)
	}
	if t.State != TaskLeased || t.Lease == nil || t.Lease.ID != lease || t.Lease.Worker != worker {
		return t, terr(http.StatusConflict, "task %s is %s; this lease is no longer valid", id, t.State)
	}
	return t, nil
}

// sameLease: a repeated complete or fail by the lease that finished the task.
func sameLease(t *Task, worker, lease string) bool {
	return t != nil && t.Lease != nil && t.Lease.ID == lease && t.Lease.Worker == worker
}

func (s *taskStore) heartbeat(project, id, worker, lease string) (*Task, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, now := s.open(project)
	t, err := s.leased(p, id, worker, lease)
	if err != nil {
		return nil, err
	}
	t.Lease.HeartbeatAt = now
	t.Lease.ExpiresAt = now.Add(time.Duration(t.Lease.TTLSec) * time.Second)
	t.UpdatedAt = now
	s.save(t)
	return t.view(false), nil
}

func (s *taskStore) complete(project, id, worker, lease string, r *TaskResult) (*Task, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, now := s.open(project)
	t, err := s.leased(p, id, worker, lease)
	if err != nil {
		if t != nil && t.State == TaskDone && sameLease(t, worker, lease) {
			return t.view(false), nil // a retry of the same report
		}
		return nil, err
	}
	r.FinishedAt = now.UTC()
	if r.Score != nil {
		if b, err := canonicalJSON(r.Score); err == nil {
			r.Score = b
		}
	}
	t.State, t.Result, t.Error, t.Requeued = TaskDone, r, "", false
	t.event(now, "done", worker, r.Outcome)
	s.save(t)
	s.prune(p)
	s.log.Info("task done", "project", project, "task", id, "worker", worker, "outcome", r.Outcome, "diff_bytes", len(r.Diff))
	return t.view(false), nil
}

func (s *taskStore) fail(project, id, worker, lease, reason string, requeue bool) (*Task, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, now := s.open(project)
	t, err := s.leased(p, id, worker, lease)
	if err != nil {
		if t != nil && t.State == TaskFailed && sameLease(t, worker, lease) {
			return t.view(false), nil
		}
		return nil, err
	}
	if reason == "" {
		reason = "failed by the worker"
	}
	if requeue {
		s.giveBack(t, now, "requeued", reason)
	} else {
		t.State, t.Error = TaskFailed, reason
		t.event(now, "failed", worker, reason)
		s.save(t)
		s.log.Info("task failed", "project", project, "task", id, "worker", worker)
	}
	s.prune(p)
	return t.view(false), nil
}

func (s *taskStore) cancel(project, id, reason string) (*Task, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, now := s.open(project)
	t, ok := p.byID[id]
	switch {
	case !ok:
		return nil, terr(http.StatusNotFound, "no task %s", id)
	case t.State == TaskCancelled:
		return t.view(false), nil
	case finished(t.State):
		return nil, terr(http.StatusConflict, "task %s is %s already", id, t.State)
	}
	if reason == "" {
		reason = "cancelled"
	}
	worker := ""
	if t.Lease != nil {
		worker = t.Lease.Worker
	}
	t.State, t.Error = TaskCancelled, reason
	t.event(now, "cancelled", worker, reason)
	s.save(t)
	s.prune(p)
	s.log.Info("task cancelled", "project", project, "task", id, "worker", worker)
	return t.view(false), nil
}

// get returns one task in full (without the lease id), or nil.
func (s *taskStore) get(project, id string) *Task {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, _ := s.open(project)
	if t, ok := p.byID[id]; ok {
		return t.view(true)
	}
	return nil
}

// list returns summaries of a project's tasks in the given states and kinds (empty: all): open
// tasks in claim order first, then finished ones, newest first; and the count per state.
func (s *taskStore) list(project string, states, kinds []string) ([]*Task, map[string]int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, _ := s.open(project)
	counts := map[string]int{}
	var queued, leased, fin []*Task
	for _, t := range p.byID {
		counts[t.State]++
		if (len(states) > 0 && !slices.Contains(states, t.State)) || (len(kinds) > 0 && !slices.Contains(kinds, t.Kind)) {
			continue
		}
		switch t.State {
		case TaskQueued:
			queued = append(queued, t)
		case TaskLeased:
			leased = append(leased, t)
		default:
			fin = append(fin, t)
		}
	}
	claimOrder(queued)
	sort.Slice(leased, func(i, j int) bool { return leased[i].Lease.Since.Before(leased[j].Lease.Since) })
	sort.Slice(fin, func(i, j int) bool { return fin[i].UpdatedAt.After(fin[j].UpdatedAt) })
	out := make([]*Task, 0, len(queued)+len(leased)+len(fin))
	for _, ts := range [][]*Task{queued, leased, fin} {
		for _, t := range ts {
			out = append(out, t.summary())
		}
	}
	return out, counts
}

// view copies a task for a response, without the lease id; full keeps the spec.
func (t *Task) view(full bool) *Task {
	c := *t
	c.Events = slices.Clone(t.Events)
	if !full {
		c.Spec = nil
	}
	if t.Lease != nil {
		l := *t.Lease
		l.ID = ""
		c.Lease = &l
	}
	if t.Result != nil {
		r := *t.Result
		c.Result = &r
	}
	return &c
}

// summary is a list entry: no spec, no diff, PR body or commit message, no events.
func (t *Task) summary() *Task {
	c := t.view(false)
	c.Events = nil
	if c.Result != nil {
		c.Result.Diff, c.Result.PRBody, c.Result.CommitMsg, c.Result.Score = "", "", "", nil
	}
	return c
}

// --- HTTP ---

// decodeTaskBody reads a JSON body of at most limit bytes into v: unknown fields, trailing data
// and other content types are refused. It writes the error and returns false on failure.
func decodeTaskBody(w http.ResponseWriter, r *http.Request, limit int64, v any) bool {
	if mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || mt != "application/json" {
		apiError(w, http.StatusUnsupportedMediaType, "send application/json")
		return false
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, limit))
	if err != nil {
		apiError(w, http.StatusRequestEntityTooLarge, fmt.Sprintf("at most %d bytes", limit))
		return false
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		apiError(w, http.StatusBadRequest, "not a valid request: "+err.Error())
		return false
	}
	if dec.More() {
		apiError(w, http.StatusBadRequest, "not a valid request: data after the object")
		return false
	}
	return true
}

func taskError(w http.ResponseWriter, err error) {
	var te *taskErr
	if errors.As(err, &te) {
		apiError(w, te.code, te.msg)
		return
	}
	apiError(w, http.StatusInternalServerError, "task store error")
}

// tasksReader allows the project's own key or an admin's session (the operations view).
func (g *Gateway) tasksReader(w http.ResponseWriter, r *http.Request, id string) bool {
	if bearer(r) == "" && g.accounts != nil {
		if acct, ok := g.session(r); ok && slices.Contains(g.acfg.Admins, acct) && g.accounts.active(acct) {
			if t := g.projects.Load(); t == nil || t.byID[id] == nil {
				apiError(w, http.StatusNotFound, "no such project")
				return false
			}
			return true
		}
	}
	return g.projectFor(w, r, id)
}

func (g *Gateway) handleTaskCreate(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !g.projectFor(w, r, id) {
		return
	}
	var n taskNew
	if !decodeTaskBody(w, r, maxTaskCreateBody, &n) {
		return
	}
	if err := n.validate(); err != nil {
		apiError(w, http.StatusBadRequest, err.Error())
		return
	}
	t, created, err := g.tasks.create(id, &n)
	if err != nil {
		taskError(w, err)
		return
	}
	code := http.StatusOK
	if created {
		code = http.StatusCreated
	}
	writeJSON(w, code, map[string]any{"created": created, "task": t})
}

func splitList(v string) []string {
	if v == "" {
		return nil
	}
	return strings.Split(v, ",")
}

func (g *Gateway) handleTaskList(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !g.tasksReader(w, r, id) {
		return
	}
	states, kinds := splitList(r.URL.Query().Get("state")), splitList(r.URL.Query().Get("kind"))
	for _, s := range states {
		if !slices.Contains(queueStates, s) {
			apiError(w, http.StatusBadRequest, "state: "+strings.Join(queueStates, ", "))
			return
		}
	}
	for _, k := range kinds {
		if _, ok := taskKinds[k]; !ok {
			apiError(w, http.StatusBadRequest, "kind: attempt, fix_round, followup or other")
			return
		}
	}
	ts, counts := g.tasks.list(id, states, kinds)
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]any{"project": id, "counts": counts, "tasks": ts})
}

func (g *Gateway) handleTaskGet(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !g.tasksReader(w, r, id) {
		return
	}
	t := g.tasks.get(id, r.PathValue("task"))
	if t == nil {
		apiError(w, http.StatusNotFound, "no such task")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]any{"task": t})
}

func (g *Gateway) handleTaskClaim(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !g.projectFor(w, r, id) {
		return
	}
	var c taskControl
	if !decodeTaskBody(w, r, maxTaskControlBody, &c) {
		return
	}
	if err := checkID("worker", c.Worker); err != nil {
		apiError(w, http.StatusBadRequest, err.Error())
		return
	}
	if c.LeaseSec == 0 {
		c.LeaseSec = defaultLeaseSec
	}
	if c.LeaseSec < minLeaseSec || c.LeaseSec > maxLeaseSec {
		apiError(w, http.StatusBadRequest, fmt.Sprintf("lease_seconds: %d to %d", minLeaseSec, maxLeaseSec))
		return
	}
	for _, k := range c.Kinds {
		if _, ok := taskKinds[k]; !ok {
			apiError(w, http.StatusBadRequest, "kinds: attempt, fix_round, followup or other")
			return
		}
	}
	if c.Lease != "" || c.Result != nil || c.Reason != "" || c.Requeue {
		apiError(w, http.StatusBadRequest, "claim takes worker, lease_seconds and kinds only")
		return
	}
	t, err := g.tasks.claim(id, c.Worker, c.LeaseSec, c.Kinds)
	if err != nil {
		taskError(w, err)
		return
	}
	if t == nil {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"task": t, "lease": map[string]any{
		"id": t.Lease.ID, "expires_at": t.Lease.ExpiresAt, "ttl_seconds": t.Lease.TTLSec}})
}

// leaseRequest decodes and checks a heartbeat, complete or fail body.
func (g *Gateway) leaseRequest(w http.ResponseWriter, r *http.Request, limit int64) (string, *taskControl, bool) {
	id := r.PathValue("id")
	if !g.projectFor(w, r, id) {
		return "", nil, false
	}
	var c taskControl
	if !decodeTaskBody(w, r, limit, &c) {
		return "", nil, false
	}
	err := checkID("worker", c.Worker)
	if err == nil && (len(c.Lease) != 32 || strings.Trim(c.Lease, "0123456789abcdef") != "") {
		err = errors.New("lease: the id the claim returned")
	}
	if err == nil && (c.LeaseSec != 0 || len(c.Kinds) > 0) {
		err = errors.New("lease_seconds and kinds belong to a claim")
	}
	if err != nil {
		apiError(w, http.StatusBadRequest, err.Error())
		return "", nil, false
	}
	return id, &c, true
}

func (g *Gateway) handleTaskHeartbeat(w http.ResponseWriter, r *http.Request) {
	id, c, ok := g.leaseRequest(w, r, maxTaskControlBody)
	if !ok {
		return
	}
	if c.Result != nil || c.Reason != "" || c.Requeue {
		apiError(w, http.StatusBadRequest, "heartbeat takes worker and lease only")
		return
	}
	t, err := g.tasks.heartbeat(id, r.PathValue("task"), c.Worker, c.Lease)
	if err != nil {
		taskError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"state": t.State, "expires_at": t.Lease.ExpiresAt})
}

func (g *Gateway) handleTaskComplete(w http.ResponseWriter, r *http.Request) {
	id, c, ok := g.leaseRequest(w, r, maxTaskCompleteBody)
	if !ok {
		return
	}
	if c.Result == nil || c.Reason != "" || c.Requeue {
		apiError(w, http.StatusBadRequest, "complete takes worker, lease and result")
		return
	}
	if err := c.Result.validate(); err != nil {
		apiError(w, http.StatusBadRequest, err.Error())
		return
	}
	t, err := g.tasks.complete(id, r.PathValue("task"), c.Worker, c.Lease, c.Result)
	if err != nil {
		taskError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"state": t.State})
}

func (g *Gateway) handleTaskFail(w http.ResponseWriter, r *http.Request) {
	id, c, ok := g.leaseRequest(w, r, maxTaskControlBody)
	if !ok {
		return
	}
	if c.Result != nil {
		apiError(w, http.StatusBadRequest, "fail takes worker, lease, reason and requeue")
		return
	}
	if err := checkText("reason", c.Reason, 500); err != nil {
		apiError(w, http.StatusBadRequest, err.Error())
		return
	}
	t, err := g.tasks.fail(id, r.PathValue("task"), c.Worker, c.Lease, c.Reason, c.Requeue)
	if err != nil {
		taskError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"state": t.State, "tries": t.Tries, "max_tries": t.MaxTries})
}

func (g *Gateway) handleTaskCancel(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !g.projectFor(w, r, id) {
		return
	}
	var c taskControl
	if !decodeTaskBody(w, r, maxTaskControlBody, &c) {
		return
	}
	if c.Worker != "" || c.Lease != "" || c.LeaseSec != 0 || len(c.Kinds) > 0 || c.Result != nil || c.Requeue {
		apiError(w, http.StatusBadRequest, "cancel takes a reason only")
		return
	}
	if err := checkText("reason", c.Reason, 500); err != nil {
		apiError(w, http.StatusBadRequest, err.Error())
		return
	}
	t, err := g.tasks.cancel(id, r.PathValue("task"), c.Reason)
	if err != nil {
		taskError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"state": t.State})
}

func (g *Gateway) taskRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /v1/projects/{id}/tasks", g.handleTaskCreate)
	mux.HandleFunc("GET /v1/projects/{id}/tasks", g.handleTaskList)
	mux.HandleFunc("POST /v1/projects/{id}/tasks/claim", g.handleTaskClaim)
	mux.HandleFunc("GET /v1/projects/{id}/tasks/{task}", g.handleTaskGet)
	mux.HandleFunc("POST /v1/projects/{id}/tasks/{task}/heartbeat", g.handleTaskHeartbeat)
	mux.HandleFunc("POST /v1/projects/{id}/tasks/{task}/complete", g.handleTaskComplete)
	mux.HandleFunc("POST /v1/projects/{id}/tasks/{task}/fail", g.handleTaskFail)
	mux.HandleFunc("POST /v1/projects/{id}/tasks/{task}/cancel", g.handleTaskCancel)
}
