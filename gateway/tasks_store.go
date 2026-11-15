// The task queue's storage and state changes (tasks.go): one queue per project, in memory and in
// state_dir, with the claims, leases and ends of tasks.
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
	"log/slog"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"
)

// projectTasks is one project's queue.
type projectTasks struct {
	byID map[string]*Task
	seq  int64
	// stale: a write failed, so this memory may be ahead of the disk; load replaces it.
	stale bool
}

// taskStore keeps every project's tasks in memory and in state_dir.
type taskStore struct {
	mu   sync.Mutex
	dir  string // state dir; "" = memory only
	now  func() time.Time
	log  *slog.Logger
	byPr map[string]*projectTasks
	// write stores one file; a test stops it to crash between two writes.
	write func(path string, raw []byte) error
}

func newTaskStore(dir string, log *slog.Logger) *taskStore {
	return &taskStore{dir: dir, now: time.Now, log: log, byPr: map[string]*projectTasks{}, write: writeFileAtomic}
}

func (s *taskStore) pdir(project string) string { return filepath.Join(s.dir, "tasks-"+project) }

// load returns a project's queue as the disk has it: read the first time and after a failed write.
// Holds s.mu.
func (s *taskStore) load(project string) (*projectTasks, error) {
	if p, ok := s.byPr[project]; ok && !p.stale {
		return p, nil
	}
	p, err := s.read(project)
	if err != nil {
		s.log.Error("task queue unreadable", "project", project, "err", err)
		return nil, terr(http.StatusServiceUnavailable, "the task queue cannot be read; try again shortly")
	}
	s.byPr[project] = p
	return p, nil
}

// read reads a project's queue from state_dir. A file that does not parse is left out, as at every
// start; a disk that does not answer is an error, not an empty queue.
func (s *taskStore) read(project string) (*projectTasks, error) {
	p := &projectTasks{byID: map[string]*Task{}}
	if s.dir == "" || !projectID.MatchString(project) {
		return p, nil
	}
	ents, err := os.ReadDir(s.pdir(project))
	if errors.Is(err, os.ErrNotExist) {
		return p, nil
	}
	if err != nil {
		return nil, err
	}
	for _, e := range ents {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".json") || strings.HasPrefix(name, ".") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(s.pdir(project), name))
		if err != nil {
			return nil, err
		}
		var t Task
		err = json.Unmarshal(raw, &t)
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
	return p, nil
}

// put writes one file of a project's queue, or marks the project stale and says why not, for a 503.
// Holds s.mu.
func (s *taskStore) put(project, dir, name string, v any) ([]byte, error) {
	raw, err := json.Marshal(v)
	if err == nil && s.dir != "" {
		if err = os.MkdirAll(dir, 0o700); err == nil {
			err = s.write(filepath.Join(dir, name+".json"), raw)
		}
	}
	if err != nil {
		s.byPr[project].stale = true
		s.log.Error("task queue write failed", "project", project, "file", name, "err", err)
		return nil, terr(http.StatusServiceUnavailable, "the task queue could not save this; nothing changed, try again shortly")
	}
	return raw, nil
}

// save writes one task. On an error the caller stops: see the top of the file. Holds s.mu.
func (s *taskStore) save(t *Task) error {
	raw, err := s.put(t.Project, s.pdir(t.Project), t.ID, t)
	if err == nil {
		t.size = len(raw)
	}
	return err
}

// drop forgets a finished task once its file is gone; a file that stays keeps the task, and the
// next prune tries again. Holds s.mu.
func (s *taskStore) drop(t *Task) bool {
	if s.dir != "" {
		if err := os.Remove(filepath.Join(s.pdir(t.Project), t.ID+".json")); err != nil && !errors.Is(err, os.ErrNotExist) {
			s.log.Warn("removing task failed", "project", t.Project, "task", t.ID, "err", err)
			return false
		}
	}
	delete(s.byPr[t.Project].byID, t.ID)
	return true
}

// How a lease ended when the next claim may continue its try (Task.Interrupted).
const (
	interruptGivenBack = "given_back" // the worker gave it back: any next claim continues the try
	interruptExpired   = "expired"    // the lease expired: only its worker's take-back within the window
)

// giveBack returns a leased task to the queue, held for its worker for the lease's hold (default
// affinityWindow; hold_seconds at the claim): given
// back by the worker (fail with requeue, kind interruptGivenBack) or expired (interruptExpired).
// Neither uses up the try yet (claim decides); a task interrupted maxInterruptions times fails.
// Holds s.mu.
func (s *taskStore) giveBack(p *projectTasks, t *Task, now time.Time, kind, reason string) error {
	worker, hold := t.Lease.Worker, t.Lease.hold()
	what := map[string]string{interruptGivenBack: "requeued", interruptExpired: "expired"}[kind]
	t.Error = reason
	if t.Interruptions >= maxInterruptions {
		t.State, t.Interrupted = TaskFailed, ""
		t.event(now, what, worker, fmt.Sprintf("%s; failed after %d interruptions", reason, t.Interruptions))
	} else if kind == interruptExpired && t.Tries >= t.MaxTries && hold <= 0 {
		// held for nobody: the wait for its worker (the sweep below) is over at once
		t.State, t.Interrupted, t.Lease = TaskFailed, "", nil
		t.Error = fmt.Sprintf("lease expired on try %d of %d (no hold for its worker)", t.Tries, t.MaxTries)
		t.event(now, "failed", worker, t.Error)
	} else {
		t.State, t.Requeued, t.Interrupted = TaskQueued, true, kind
		t.Lease = nil
		// a hold of 0 (the worker's checkpoints are shared) holds it for nobody: until is now, so the task
		// is free at once (an expiry on the last try failed above)
		until := now.Add(hold)
		t.PreferWorker, t.PreferUntil = worker, &until
		note := reason + "; back in the queue"
		if kind == interruptExpired && t.Tries >= t.MaxTries {
			note += fmt.Sprintf(" for %s only (try %d of %d): failed unless it takes it back by %s", worker, t.Tries, t.MaxTries, until.UTC().Format(time.RFC3339))
		}
		t.event(now, what, worker, note)
	}
	if err := s.save(t); err != nil {
		return err
	}
	s.log.Info("task given back", "project", t.Project, "task", t.ID, "worker", worker, "why", what, "state", t.State,
		"tries", t.Tries, "interruptions", t.Interruptions)
	return nil
}

// continues: a claim by worker now continues the task's interrupted try instead of starting one.
func continues(t *Task, worker string, now time.Time) bool {
	switch t.Interrupted {
	case interruptGivenBack:
		return true
	case interruptExpired:
		return t.PreferWorker == worker && t.PreferUntil != nil && now.Before(*t.PreferUntil)
	}
	return false
}

// sweep returns the project's expired leases to the queue, and fails a task whose lease expired on
// its last try when its worker has not taken it back within the hold. It stops at a failed write,
// leaving p stale. Holds s.mu.
func (s *taskStore) sweep(p *projectTasks, now time.Time) {
	for _, t := range p.byID {
		if p.stale {
			return
		}
		switch {
		case t.State == TaskLeased && !now.Before(t.Lease.ExpiresAt):
			s.giveBack(p, t, now, interruptExpired, "lease expired (no heartbeat)")
		case t.State == TaskQueued && t.Interrupted == interruptExpired && t.Tries >= t.MaxTries &&
			t.PreferUntil != nil && !now.Before(*t.PreferUntil):
			worker := t.PreferWorker
			t.State, t.Interrupted, t.PreferWorker, t.PreferUntil = TaskFailed, "", "", nil
			t.Error = fmt.Sprintf("lease expired on try %d of %d and %s did not take it back in time", t.Tries, t.MaxTries, worker)
			t.event(now, "failed", worker, t.Error)
			if s.save(t) == nil {
				s.log.Info("task failed", "project", t.Project, "task", t.ID, "worker", worker, "why", "expired on the last try")
			}
		}
	}
}

// sweepAll sweeps every loaded project (the background check), so that an expired lease does not
// wait for a call.
func (s *taskStore) sweepAll() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for project := range s.byPr {
		s.open(project)
	}
}

// open loads and sweeps a project's queue, and gives the memory that matches the disk even when the
// sweep could not write. The caller holds s.mu.
func (s *taskStore) open(project string) (*projectTasks, time.Time, error) {
	now := s.now()
	p, err := s.load(project)
	if err == nil {
		s.sweep(p, now)
		p, err = s.load(project)
	}
	return p, now, err
}

// ready: the project's queue can be read; a 503 otherwise. For the calls that only read, which
// would show an unreadable queue as an empty one.
func (s *taskStore) ready(project string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, _, err := s.open(project)
	return err
}

// prune drops the oldest exported finished tasks while the finished ones are beyond
// maxKeptTasks or maxKeptBytes. A task not exported (again) is never dropped. Holds s.mu.
func (s *taskStore) prune(p *projectTasks) {
	if p.stale {
		return
	}
	var fin []*Task
	n, kept := 0, 0
	for _, t := range p.byID {
		if finished(t.State) {
			n++
			kept += t.size
			if t.ExportedAt != nil {
				fin = append(fin, t)
			}
		}
	}
	sort.Slice(fin, func(i, j int) bool { return fin[i].UpdatedAt.Before(fin[j].UpdatedAt) })
	for len(fin) > 0 && (n > maxKeptTasks || kept > maxKeptBytes) {
		if s.drop(fin[0]) {
			n--
			kept -= fin[0].size
		}
		fin = fin[1:]
	}
}

// unexported counts the finished tasks that wait for the owner's export, and their bytes.
func unexported(p *projectTasks) (n, size int) {
	for _, t := range p.byID {
		if finished(t.State) && t.ExportedAt == nil {
			n++
			size += t.size
		}
	}
	return n, size
}

// taskErr is an error with its HTTP status.
type taskErr struct {
	code int
	msg  string
}

func (e *taskErr) Error() string { return e.msg }

func terr(code int, format string, a ...any) error { return &taskErr{code, fmt.Sprintf(format, a...)} }

// sameNew: an existing task was made by this create request (its content, not its state).
func sameNew(t *Task, n *taskNew, spec json.RawMessage, maxTries int) bool {
	return t.Kind == n.Kind && t.Title == n.Title && bytes.Equal(t.Spec, spec) && t.CreatedBy == n.CreatedBy &&
		t.MaxTries == maxTries && t.Order == n.Order && t.Role == n.Role && t.Parent == n.Parent &&
		slices.Equal(t.Needs, n.Needs) && t.RetryOf == n.RetryOf && t.Replaces == n.Replaces &&
		t.HoleID == n.HoleID && maps.Equal(t.Labels, n.Labels)
}

// create adds a task, or returns the existing one with the same id and content (created false).
func (s *taskStore) create(project string, n *taskNew) (*Task, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, now, err := s.open(project)
	if err != nil {
		return nil, false, err
	}
	spec, err := canonicalJSON(n.Spec)
	if err != nil {
		return nil, false, terr(http.StatusBadRequest, "spec: %v", err)
	}
	maxTries := n.MaxTries
	if maxTries == 0 {
		maxTries = defaultMaxTries
	}
	if len(n.Needs) == 0 {
		n.Needs = nil
	}
	if len(n.Labels) == 0 {
		n.Labels = nil
	}
	if t, ok := p.byID[n.ID]; ok {
		if !sameNew(t, n, spec, maxTries) {
			return nil, false, terr(http.StatusConflict, "task %s exists with other content; a new task needs a new id", n.ID)
		}
		return t.view(true), false, nil
	}
	if err := admit(p); err != nil {
		return nil, false, err
	}
	p.seq++
	t := &Task{ID: n.ID, Project: project, Kind: n.Kind, Title: n.Title, Spec: spec, CreatedBy: n.CreatedBy,
		State: TaskQueued, MaxTries: maxTries, Seq: p.seq, CreatedAt: now, Order: n.Order, Role: n.Role,
		Parent: n.Parent, Needs: slices.Clone(n.Needs), RetryOf: n.RetryOf, Replaces: n.Replaces, HoleID: n.HoleID,
		Labels: maps.Clone(n.Labels), Try: 1}
	if n.RetryOf != "" { // one more than the try before; that task may have been dropped after its export
		t.Try = 2
		if prev := p.byID[n.RetryOf]; prev != nil && prev.Try > 0 {
			t.Try = prev.Try + 1
		}
	}
	t.event(now, "created", "", n.CreatedBy)
	p.byID[t.ID] = t
	if err := s.save(t); err != nil {
		return nil, false, err
	}
	s.prune(p)
	s.log.Info("task created", "project", project, "task", t.ID, "kind", t.Kind, "role", t.Role, "by", t.CreatedBy)
	return t.view(true), true, nil
}

// admit: the queue may take one more task (429).
func admit(p *projectTasks) error {
	open := 0
	for _, t := range p.byID {
		if !finished(t.State) {
			open++
		}
	}
	if open >= maxOpenTasks {
		return terr(http.StatusTooManyRequests, "at most %d open tasks per project", maxOpenTasks)
	}
	if n, size := unexported(p); n >= maxKeptTasks || size >= maxKeptBytes {
		return terr(http.StatusTooManyRequests, "%d finished tasks (%d bytes) wait for the export to the ledger; export them first", n, size)
	}
	return nil
}

// claimOrder sorts queued tasks: requeued first (interrupted work resumes first), then the
// client's order, then creation order.
func claimOrder(ts []*Task) {
	sort.Slice(ts, func(i, j int) bool {
		a, b := ts[i], ts[j]
		if a.Requeued != b.Requeued {
			return a.Requeued
		}
		if a.Order != b.Order {
			return a.Order < b.Order
		}
		return a.Seq < b.Seq
	})
}

// heldFor: a task that came back waits for its last worker until PreferUntil.
func heldFor(t *Task, worker string, now time.Time) bool {
	return t.PreferWorker != "" && t.PreferWorker != worker && t.PreferUntil != nil && now.Before(*t.PreferUntil)
}

// claim leases the next queued task of the given kinds (none: any) to worker, or returns nil when
// none is queued. With only, it leases that task if it is queued and not held for another worker
// (404 or 409 otherwise).
func (s *taskStore) claim(project, worker string, ttl int, kinds []string, only string, hold *int) (*Task, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, now, err := s.open(project)
	if err != nil {
		return nil, err
	}
	var t *Task
	if only != "" {
		t = p.byID[only]
		switch {
		case t == nil:
			return nil, terr(http.StatusNotFound, "no task %s", only)
		case t.State != TaskQueued:
			return nil, terr(http.StatusConflict, "task %s is %s", only, t.State)
		case heldFor(t, worker, now):
			return nil, terr(http.StatusConflict, "task %s is held for %s until %s", only, t.PreferWorker, t.PreferUntil.UTC().Format(time.RFC3339))
		}
	} else {
		var q []*Task
		for _, t := range p.byID {
			if t.State == TaskQueued && (len(kinds) == 0 || slices.Contains(kinds, t.Kind)) && !heldFor(t, worker, now) {
				q = append(q, t)
			}
		}
		if len(q) == 0 {
			return nil, nil
		}
		claimOrder(q)
		t = q[0]
	}
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return nil, err
	}
	note := ""
	if continues(t, worker, now) {
		t.Interruptions++
		note = fmt.Sprintf("try %d of %d continues after an interruption (%s; %d so far)", t.Tries, t.MaxTries, t.Interrupted, t.Interruptions)
	} else {
		t.Tries++
		note = fmt.Sprintf("try %d of %d", t.Tries, t.MaxTries)
	}
	t.PreferWorker, t.PreferUntil, t.Interrupted = "", nil, ""
	t.State, t.Error = TaskLeased, ""
	if t.StartedAt == nil {
		at := now.UTC()
		t.StartedAt = &at
	}
	t.Lease = &taskLease{ID: hex.EncodeToString(id[:]), Worker: worker, TTLSec: ttl, Since: now, HeartbeatAt: now,
		ExpiresAt: now.Add(time.Duration(ttl) * time.Second), HoldSec: hold}
	t.event(now, "claimed", worker, note)
	if err := s.save(t); err != nil {
		return nil, err
	}
	s.log.Info("task claimed", "project", project, "task", t.ID, "worker", worker, "try", t.Tries, "interruptions", t.Interruptions)
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
	p, now, err := s.open(project)
	if err != nil {
		return nil, err
	}
	t, err := s.leased(p, id, worker, lease)
	if err != nil {
		return nil, err
	}
	t.Lease.HeartbeatAt = now
	t.Lease.ExpiresAt = now.Add(time.Duration(t.Lease.TTLSec) * time.Second)
	t.UpdatedAt = now
	if err := s.save(t); err != nil {
		return nil, err
	}
	return t.view(false), nil
}

func (s *taskStore) complete(project, id, worker, lease string, r *TaskResult, closedBy string) (*Task, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, now, err := s.open(project)
	if err != nil {
		return nil, err
	}
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
	t.ClosedBy = closedBy
	note := r.Outcome
	if closedBy != "" {
		note += "; closed by " + closedBy
	}
	t.event(now, "done", worker, note)
	if err := s.save(t); err != nil {
		return nil, err
	}
	s.prune(p)
	s.log.Info("task done", "project", project, "task", id, "worker", worker, "outcome", r.Outcome)
	return t.view(false), nil
}

func (s *taskStore) fail(project, id, worker, lease, reason string, requeue bool) (*Task, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, now, err := s.open(project)
	if err != nil {
		return nil, err
	}
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
		if err := s.giveBack(p, t, now, interruptGivenBack, reason); err != nil {
			return nil, err
		}
	} else {
		t.State, t.Error = TaskFailed, reason
		t.event(now, "failed", worker, reason)
		if err := s.save(t); err != nil {
			return nil, err
		}
		s.log.Info("task failed", "project", project, "task", id, "worker", worker)
	}
	s.prune(p)
	return t.view(false), nil
}

// cancel ends an open task; closedBy names what made its work unnecessary, if anything.
func (s *taskStore) cancel(project, id, reason, closedBy string) (*Task, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, now, err := s.open(project)
	if err != nil {
		return nil, err
	}
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
	t.State, t.Error, t.ClosedBy = TaskCancelled, reason, closedBy
	note := reason
	if closedBy != "" {
		note += "; closed by " + closedBy
	}
	t.event(now, "cancelled", worker, note)
	if err := s.save(t); err != nil {
		return nil, err
	}
	s.log.Info("task cancelled", "project", t.Project, "task", t.ID, "worker", worker)
	s.prune(p)
	return t.view(false), nil
}

// setMaxTries changes an open task's max_tries (n is 1 to maxMaxTries): not below the tries it
// has started (409), and not on a finished task (409).
func (s *taskStore) setMaxTries(project, id string, n int) (*Task, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, now, err := s.open(project)
	if err != nil {
		return nil, err
	}
	t, ok := p.byID[id]
	switch {
	case !ok:
		return nil, terr(http.StatusNotFound, "no task %s", id)
	case finished(t.State):
		return nil, terr(http.StatusConflict, "task %s is %s already", id, t.State)
	case n < t.Tries:
		return nil, terr(http.StatusConflict, "task %s has started %d tries; max_tries cannot be lower", id, t.Tries)
	}
	if n != t.MaxTries {
		t.event(now, "max_tries", "", fmt.Sprintf("%d -> %d", t.MaxTries, n))
		t.MaxTries = n
		if err := s.save(t); err != nil {
			return nil, err
		}
		s.log.Info("task max_tries", "project", project, "task", id, "max_tries", n)
	}
	return t.view(false), nil
}

// exported marks a finished task as taken into the ledger as it was at seen (its updated_at
// when the export read it): a task changed since gives 409, so the next
// export takes the change. It does not change updated_at (the pruning order).
func (s *taskStore) exported(project, id string, seen time.Time) (*Task, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, now, err := s.open(project)
	if err != nil {
		return nil, err
	}
	t, ok := p.byID[id]
	switch {
	case !ok:
		return nil, terr(http.StatusNotFound, "no task %s", id)
	case !finished(t.State):
		return nil, terr(http.StatusConflict, "task %s is %s, not finished", id, t.State)
	case !t.UpdatedAt.Equal(seen):
		return nil, terr(http.StatusConflict, "task %s changed at %s after the export read it; export it again", id, t.UpdatedAt.UTC().Format(time.RFC3339Nano))
	}
	if t.ExportedAt == nil {
		at := now.UTC()
		t.ExportedAt = &at
		if err := s.save(t); err != nil {
			return nil, err
		}
		s.prune(p)
	}
	return t.view(false), nil
}

// get returns one task in full (without the lease id), or nil.
func (s *taskStore) get(project, id string) *Task {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, _, err := s.open(project)
	if err != nil {
		return nil
	}
	if t, ok := p.byID[id]; ok {
		return t.view(true)
	}
	return nil
}

// list returns summaries of a project's tasks in the given states, kinds and roles (empty: all), and
// with exported "true" or "false" only the finished tasks exported or not: open tasks in claim
// order first, then finished ones, newest first; and the count per state.
func (s *taskStore) list(project string, states, kinds, roles []string, exported string) ([]*Task, map[string]int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	counts := map[string]int{}
	p, _, err := s.open(project)
	if err != nil {
		return nil, counts
	}
	var queued, leased, fin []*Task
	for _, t := range p.byID {
		counts[t.State]++
		if (len(states) > 0 && !slices.Contains(states, t.State)) || (len(kinds) > 0 && !slices.Contains(kinds, t.Kind)) ||
			(len(roles) > 0 && !slices.Contains(roles, t.Role)) {
			continue
		}
		if exported != "" && (!finished(t.State) || (t.ExportedAt != nil) != (exported == "true")) {
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

// noteNode records that a node of class (trusted, volunteer) served a request of a project's running
// task, the task its worker named (nodework.go). In memory only: it reaches the disk with the task's
// next save (a heartbeat, its end), and a task not loaded or not running is left alone. Information
// for the public views, never a reason to refuse anything.
func (s *taskStore) noteNode(project, id, class string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.byPr[project]
	if p == nil || p.stale {
		return
	}
	t := p.byID[id]
	if t == nil || t.State != TaskLeased || slices.Contains(t.NodeClasses, class) || len(t.NodeClasses) >= maxTaskNodeClasses {
		return
	}
	t.NodeClasses = append(t.NodeClasses, class)
	slices.Sort(t.NodeClasses)
}
