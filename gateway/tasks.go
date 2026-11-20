// Science task coordinator (decision 0015, phase 1; decision 0019): a project's task queue lives in
// the gateway. The project's tools submit tasks; workers claim the next queued task of their kinds
// with a lease, renew it with heartbeats, and report a result or a failure. A lease that is not
// renewed in time returns the task to the queue (or fails it after max_tries tries), so a worker
// that dies never holds a task for ever. The gateway is the single source of truth for the queue
// and its state; what a result means (the next task, a review, a ledger) is the project's business,
// outside it: the gateway acts on no project's workflow (decision 0019, which removed TauCeti's
// review loop from here).
//
// Tries and interruptions (owner 2026-10-29: "a try should not be counted when there is spot
// interruption"): a claim starts a try, unless it continues one that was interrupted. A lease
// ends in an interruption when its worker gives the task back (`fail` with `requeue`: the
// runner's SIGTERM path on a spot stop) — the next claim, by any worker, continues that try — or
// when it expires and the same worker takes the task back within its hold (a stop without
// SIGTERM, a gateway outage). Each continued try counts one interruption; at maxInterruptions a
// further give-back or expiry fails the task. A lease that expired on the last try waits for its
// worker for the hold and fails if it does not come back.
//
// Endpoints, all with the project's own key (401 without one, 403 with another project's):
//
//	POST /v1/projects/{id}/tasks                    create (idempotent by the client's task id)
//	GET  /v1/projects/{id}/tasks?state=&kind=&role= list (summaries; also an admin's session)
//	GET  /v1/projects/{id}/tasks/{task}             one task with its spec and result (same)
//	POST /v1/projects/{id}/tasks/claim              take the next task of the given kinds and a lease (204: none)
//	POST /v1/projects/{id}/tasks/{task}/heartbeat   extend the lease
//	POST /v1/projects/{id}/tasks/{task}/complete    the result (state done), with closed_by when no work was needed
//	POST /v1/projects/{id}/tasks/{task}/fail        give the task up: failed, or with requeue given back
//	POST /v1/projects/{id}/tasks/{task}/cancel      queued or leased -> cancelled (closed_by: what made it unnecessary)
//	POST /v1/projects/{id}/tasks/{task}/max_tries   change an open task's max_tries (1-10, ≥ tries)
//	POST /v1/projects/{id}/tasks/{task}/exported    a finished task is in the project's snapshot (optional)
//
// And without a key, the public fields of the projects that publish their queue (queue_public.go,
// tasks_public.go): GET /v1/projects/{id}/queue and GET /v1/tasks.
//
// The task schema (decision 0019, D1-D3): a task carries, written at its creation, its `kind` (the
// claim key: one scaffold generation of a project, "p2m-v7", from the project's declared kinds, or
// "other", which every project may use), its `role` in the project's pipeline (outline, hole, ...),
// its relations to other tasks of the project (`parent`, `needs`, `retry_of`, `replaces`: names only;
// the gateway schedules nothing by them), a `labels` map of short public strings (trial, leaf, ...)
// and a `hole_id`. `closed_by` comes with a task's end, when the task ended without work because
// something else closed it (another task's id, or "reuse:<by>"). All of these are public. A task made
// before them has none; its spec still says what the public views read for it (queue_public.go).
//
// Claim order (the owner's work priority, 2026-10-29): tasks that were leased before and came back
// (an expired lease, a worker's requeue) first, then by the task's `order` (lower first, default 0;
// a project that runs its tasks in a planned sequence, such as prove2me's calibration leaves before
// its comparison set, owner 2026-11-11), then oldest first. A claim names the kinds its worker runs
// (`kinds`; none: any), so a worker never takes a task of a scaffold generation it does not have. A
// task that came back is held for its last worker for the lease's hold (affinityWindow unless the
// claim set hold_seconds; it has the checkpoint; owner 2026-10-27, decision 5), then anyone may
// claim it. A claim may name one task (`task`): a worker takes back the task whose lease it lost, to
// resume from its own checkpoint.
//
// Retention (owner 2026-11-17, replacing the export gate of 2026-10-27): the gateway keeps every
// task. A finished task is never dropped and never makes create refuse a new one; `exported` is an
// optional mark for a project's own snapshots and gates nothing. A task that ended at least the
// project's task_compact_days ago (default 30) is compacted: its spec, events, lease, error and live
// counts go, and what the public views show (tasks_public.go, queue_public.go) and the result stay.
// A compacted task keeps the spec's SHA-256 (spec_sha256), its title and creator, so a repeated
// create of it is still recognised; a task made before the typed fields keeps the part of its spec
// the public views read (legacySpecRef). Finished tasks not yet compacted that hold more than
// maxFullBytes are compacted early, oldest first, so memory stays bounded either way.
//
// Private: the spec, the title, prompts, workers, leases, errors and events stay with the project
// key and the admins. Each task is one file under state_dir/tasks-<project>/, written atomically
// on every change, so the queue survives a restart; the open tasks and the bytes of finished tasks
// not yet compacted are bounded per project.
//
// A failed write (a full disk, a read-only directory) is treated as a crash of that project's
// queue, and recovers the same way (owner 2026-10-31). What is on disk is the truth: a call whose
// change did not reach the disk answers 503 and changes nothing, so it may be repeated, and the
// project's memory is dropped and read again at the next call or sweep, with no rollback to get
// wrong. Two exceptions, information only: the classes of the nodes that served a running task's
// requests (node_classes) and the live counts of its model calls (live, tasks_live.go) are noted in
// memory and reach the disk with the task's next save (a heartbeat or its end).
//
// SPDX-License-Identifier: Apache-2.0
package gateway

import (
	"encoding/json"
	"regexp"
	"slices"
	"time"
)

const (
	maxTaskCreateBody   = 128 << 10 // a create request (spec at most maxTaskSpec)
	maxTaskSpec         = 64 << 10  // the opaque spec the worker needs
	maxTaskControlBody  = 8 << 10   // claim, heartbeat, fail, cancel
	maxTaskCompleteBody = 64 << 10  // a result: the score (≤ maxTaskScore) and the counts
	maxTaskScore        = 16 << 10

	// maxOpenTasks bounds queued plus leased tasks per project; create refuses beyond it.
	maxOpenTasks = 500
	// Compaction of finished tasks (see Retention above): after the project's task_compact_days
	// (default defaultCompactDays, at most maxCompactDays); earlier, oldest first, while the
	// finished tasks not yet compacted hold more than maxFullBytes. At most maxCompactPerPass tasks
	// a pass, so one call never waits for a long rewrite; the next sweep goes on.
	defaultCompactDays = 30
	maxCompactDays     = 3650
	maxFullBytes       = 64 << 20
	maxCompactPerPass  = 200

	defaultLeaseSec = 600
	minLeaseSec     = 30
	maxLeaseSec     = 3600
	defaultMaxTries = 3 // the runner's max_tries
	maxMaxTries     = 10
	// maxInterruptions: a task interrupted this often fails at its next give-back or expiry, so
	// a task that keeps stopping its worker still ends.
	maxInterruptions = 50
	maxTaskEvents    = 20
	// taskSweepEvery: expired leases return to the queue at least this often, and on every call.
	taskSweepEvery = 30 * time.Second
	// affinityWindow: a task that came back from a lease waits this long for its last worker,
	// which holds its checkpoint (owner 2026-10-27: about 10 minutes).
	affinityWindow = 10 * time.Minute
	// maxTaskOrder bounds a task's order (Task.Order) either way.
	maxTaskOrder = 1_000_000
	// legacyMaxClass: a claim's max_class, from before decision 0019 removed the claim classes, is
	// still taken (older clients) and means nothing now; its range is the old one.
	legacyMaxClass = 6

	// The typed fields' bounds (decision 0019).
	maxTaskNeeds       = 64  // ids in needs
	maxTaskLabels      = 16  // entries in labels
	maxTaskLabelValue  = 200 // bytes per label value
	maxTaskClaimKinds  = 32  // kinds in one claim
	maxProjectKinds    = 32  // kinds a project declares
	maxTaskNodeClasses = 4   // node_classes kept per task
	// kindOther: the kind every project may use without declaring it; the kind of every task made
	// before decision 0019 by prove2me.
	kindOther = "other"
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
	// taskKindRe: a kind ("p2m-v7", "other") or a role ("outline", "hole"), also a label's key.
	taskKindRe = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,31}$`)
	// taskLabelKeyRe: a label's name.
	taskLabelKeyRe = regexp.MustCompile(`^[a-z][a-z0-9_]{0,31}$`)
	// taskHoleRe: a hole's id (prove2me writes "h-" and 12 hex of its ledger key).
	taskHoleRe = regexp.MustCompile(`^[a-z0-9][a-z0-9._:-]{0,63}$`)
	// taskClosedByRe: what closed a task's work: a task's id, or "reuse:<by>" with a library and a
	// Lean declaration's name (letters of any script, digits, subscripts and _ . ' ! ? -).
	taskClosedByRe = regexp.MustCompile(`^[\p{L}\p{N}_][\p{L}\p{N}_.:'!?/\x{2080}-\x{2089}-]{0,199}$`)
)

// Task is one unit of work in a project's queue.
type Task struct {
	ID      string `json:"id"` // the client's id, unique per project
	Project string `json:"project"`
	// Kind is the claim key: one of the project's declared kinds (a scaffold generation, p2m-v7), or
	// "other". Tasks from before decision 0019 may carry an old kind (attempt, fix_round, ...).
	Kind string `json:"kind"`
	// Role: the task's part in its project's pipeline (outline, hole, rest, assembly), public.
	Role string `json:"role,omitempty"`
	// The relations to other tasks of the project, written at creation (decision 0019, D1): Parent
	// (the task that defined this one's place), Needs (the tasks whose results it uses; information
	// only), RetryOf (the try before this one), Replaces (the task this one supersedes). ClosedBy:
	// set at the task's end when it ended without work, what closed it.
	Parent   string   `json:"parent,omitempty"`
	Needs    []string `json:"needs,omitempty"`
	RetryOf  string   `json:"retry_of,omitempty"`
	Replaces string   `json:"replaces,omitempty"`
	ClosedBy string   `json:"closed_by,omitempty"`
	// Try: 1 for a first try, else one more than the try of RetryOf (2 when that task is gone).
	Try int `json:"try,omitempty"`
	// Labels: the project's own public names for the task (trial, leaf, ...): short strings.
	Labels map[string]string `json:"labels,omitempty"`
	// HoleID: the hole a hole task works on (prove2me's ledger), public.
	HoleID    string          `json:"hole_id,omitempty"`
	Title     string          `json:"title,omitempty"`
	Spec      json.RawMessage `json:"spec,omitempty"` // opaque to the gateway: what the worker needs
	CreatedBy string          `json:"created_by"`
	State     string          `json:"state"`
	Tries     int             `json:"tries"` // tries started; a claim that continues an interrupted try does not count
	MaxTries  int             `json:"max_tries"`
	// Interruptions: leases that ended without using up their try (see the top of the file).
	Interruptions int `json:"interruptions,omitempty"`
	// Interrupted: how the last lease ended while the next claim may continue its try:
	// "given_back" (any claim continues it) or "expired" (only PreferWorker's, until PreferUntil).
	Interrupted string `json:"interrupted,omitempty"`
	// Requeued: the task was leased before and came back to the queue; it is claimed first.
	Requeued bool `json:"requeued,omitempty"`
	// Order: the client's place for the task, lower first (claimOrder); 0 by default.
	Order     int        `json:"order,omitempty"`
	Seq       int64      `json:"seq"` // creation order within the project
	CreatedAt time.Time  `json:"created_at"`
	UpdatedAt time.Time  `json:"updated_at"`
	StartedAt *time.Time `json:"started_at,omitempty"` // the first claim
	// NodeClasses: the classes (volunteer, pilot; trusted before 2026-11-20) of the nodes that served the task's model
	// requests, as far as its worker named the task (X-Aisathome-Task, nodework.go).
	NodeClasses []string `json:"node_classes,omitempty"`
	// Live: the gateway's own count of the model calls its worker named the task in, this try
	// (tasks_live.go); the result's numbers, from the worker, stay the authoritative ones.
	Live   *taskLive   `json:"live,omitempty"`
	Lease  *taskLease  `json:"lease,omitempty"` // the current lease, or the last one when finished
	Result *TaskResult `json:"result,omitempty"`
	Error  string      `json:"error,omitempty"` // the last failure, expiry or cancel reason
	Events []taskEvent `json:"events,omitempty"`
	// PreferWorker holds a task that came back for its last worker until PreferUntil.
	PreferWorker string     `json:"prefer_worker,omitempty"`
	PreferUntil  *time.Time `json:"prefer_until,omitempty"`
	// ExportedAt: the owner's export took the task as it was at that UpdatedAt; nil = not (again).
	ExportedAt *time.Time `json:"exported_at,omitempty"`
	// CompactedAt: when the task was compacted (see Retention above); SpecSHA256: the SHA-256 (hex)
	// of its spec as created, for a repeated create.
	CompactedAt *time.Time `json:"compacted_at,omitempty"`
	SpecSHA256  string     `json:"spec_sha256,omitempty"`

	size int // bytes on disk, for maxFullBytes
}

type taskLease struct {
	ID          string    `json:"id,omitempty"` // only the claimer gets it; views leave it out
	Worker      string    `json:"worker"`
	TTLSec      int       `json:"ttl_seconds"`
	Since       time.Time `json:"since"`
	HeartbeatAt time.Time `json:"heartbeat_at"`
	ExpiresAt   time.Time `json:"expires_at"`
	HoldSec     *int      `json:"hold_seconds,omitempty"` // the claim's hold_seconds; absent: affinityWindow
}

// hold: how long a task that came back from this lease waits for its worker.
func (l *taskLease) hold() time.Duration {
	if l == nil || l.HoldSec == nil {
		return affinityWindow
	}
	return time.Duration(*l.HoldSec) * time.Second
}

type taskEvent struct {
	At     time.Time `json:"at"`
	What   string    `json:"what"` // created, claimed, expired, done, failed, requeued, cancelled, max_tries
	Worker string    `json:"worker,omitempty"`
	Note   string    `json:"note,omitempty"`
}

// TaskResult is what a worker reports when it finishes a task. The raw evidence (sessions,
// logs, the sandbox) stays on the worker; RawRef names where.
type TaskResult struct {
	Outcome          string          `json:"outcome"` // short, the project's word: "proved", "outline", "closed", ...
	ChecksPassed     *bool           `json:"checks_passed,omitempty"`
	Score            json.RawMessage `json:"score,omitempty"` // a summary object, opaque
	Steps            int             `json:"steps"`
	PromptTokens     int64           `json:"prompt_tokens"`
	CachedTokens     *int64          `json:"cached_tokens,omitempty"` // re-read from the cache, not in PromptTokens; nil: not reported
	CompletionTokens int64           `json:"completion_tokens"`
	ModelMin         float64         `json:"model_min"`
	AwakeMin         float64         `json:"awake_min"`
	RawRef           string          `json:"raw_ref,omitempty"` // e.g. raw/2026-11-14/t5-F01 on the runner
	FinishedAt       time.Time       `json:"finished_at"`
}

// taskNew is a create request.
type taskNew struct {
	ID        string          `json:"id"`
	Kind      string          `json:"kind"`
	Title     string          `json:"title,omitempty"`
	Spec      json.RawMessage `json:"spec"`
	CreatedBy string          `json:"created_by"`
	MaxTries  int             `json:"max_tries,omitempty"`
	Order     int             `json:"order,omitempty"` // Task's
	// The typed fields: Task's.
	Role     string            `json:"role,omitempty"`
	Parent   string            `json:"parent,omitempty"`
	Needs    []string          `json:"needs,omitempty"`
	RetryOf  string            `json:"retry_of,omitempty"`
	Replaces string            `json:"replaces,omitempty"`
	Labels   map[string]string `json:"labels,omitempty"`
	HoleID   string            `json:"hole_id,omitempty"`
}

// taskControl is the body of claim, heartbeat, complete, fail and cancel (the fields each needs).
type taskControl struct {
	Worker   string      `json:"worker,omitempty"`
	Lease    string      `json:"lease,omitempty"`
	LeaseSec int         `json:"lease_seconds,omitempty"` // claim: the lease's TTL
	Kinds    []string    `json:"kinds,omitempty"`         // claim: only these kinds
	MaxClass int         `json:"max_class,omitempty"`     // claim: taken from older clients, ignored (legacyMaxClass)
	Result   *TaskResult `json:"result,omitempty"`        // complete
	Reason   string      `json:"reason,omitempty"`        // fail, cancel
	Requeue  bool        `json:"requeue,omitempty"`       // fail: give the task back (an interruption: its try continues)
	Task     string      `json:"task,omitempty"`          // claim: this task only (a worker taking its own back)
	ClosedBy string      `json:"closed_by,omitempty"`     // complete, cancel: what made the task's work unnecessary
	// HoldSec (claim): how long the task waits for this worker after its lease ends in a give-back or
	// an expiry, 0 to affinityWindow seconds; absent: affinityWindow. 0 for a worker whose checkpoints
	// any worker can resume (the stateless spot runner keeps them in S3, 2026-11-12).
	HoldSec *int `json:"hold_seconds,omitempty"`
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

// view copies a task for a response, without the lease id; full keeps the spec.
func (t *Task) view(full bool) *Task {
	c := *t
	c.Events = slices.Clone(t.Events)
	c.Needs = slices.Clone(t.Needs)
	c.NodeClasses = slices.Clone(t.NodeClasses)
	if t.Live != nil {
		l := *t.Live
		c.Live = &l
	}
	if t.Labels != nil {
		c.Labels = make(map[string]string, len(t.Labels))
		for k, v := range t.Labels {
			c.Labels[k] = v
		}
	}
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

// summary is a list entry: no spec, score or events.
func (t *Task) summary() *Task {
	c := t.view(false)
	c.Events = nil
	if c.Result != nil {
		c.Result.Score = nil
	}
	return c
}
