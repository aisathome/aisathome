// Science task coordinator (decision 0015, phase 1): a project's task queue lives in the gateway.
// The project's tools submit tasks; workers (the agent runner) claim the highest-priority queued
// task with a lease, renew it with heartbeats, and report a result or a failure. A lease that is
// not renewed in time returns the task to the queue (or fails it after max_tries tries), so a
// worker that dies never holds a task for ever. The gateway is the single source of truth for
// the queue and its state; validating a result (review, CI) is a separate step outside it.
//
// Tries and interruptions (owner 2026-10-29: "a try should not be counted when there is spot
// interruption"): a claim starts a try, unless it continues one that was interrupted. A lease
// ends in an interruption when its worker gives the task back (`fail` with `requeue`: the
// runner's SIGTERM path on a spot stop) — the next claim, by any worker, continues that try — or
// when it expires and the same worker takes the task back within affinityWindow (a stop without
// SIGTERM, a gateway outage). Each continued try counts one interruption; at maxInterruptions a
// further give-back or expiry fails the task. A lease that expired on the last try waits for its
// worker for affinityWindow and fails if it does not come back.
//
// Endpoints, all with the project's own key (401 without one, 403 with another project's):
//
//	POST /v1/projects/{id}/tasks                    create (idempotent by the client's task id)
//	GET  /v1/projects/{id}/tasks?state=&kind=       list (summaries; also an admin's session)
//	GET  /v1/projects/{id}/tasks/{task}             one task with its spec and result (same)
//	POST /v1/projects/{id}/tasks/claim              take the next task and a lease (204: none)
//	POST /v1/projects/{id}/tasks/{task}/heartbeat   extend the lease
//	POST /v1/projects/{id}/tasks/{task}/complete    the result (state done)
//	POST /v1/projects/{id}/tasks/{task}/fail        give the task up: failed, or with requeue given back
//	POST /v1/projects/{id}/tasks/{task}/cancel      queued or leased -> cancelled
//	POST /v1/projects/{id}/tasks/{task}/max_tries   change an open task's max_tries (1-10, ≥ tries)
//	POST /v1/projects/{id}/tasks/{task}/review      a review's verdict on a done task (phase 3); judge qwen keeps it apart
//	POST /v1/projects/{id}/tasks/{task}/decide      the owner's decision on a done task's candidate
//	POST /v1/projects/{id}/tasks/{task}/exported    a finished task is in the owner's ledger
//	POST /v1/projects/{id}/tasks/{task}/continue    schedule the round waiting on a needs-operator task, with a note if any
//
// Claim order (the owner's work priority, 2026-10-29): by class, then within a class tasks that
// were leased before and came back (an expired lease, a worker's requeue), then oldest first.
// Classes: 1 fix after a failed TauCeti review, 2 fix after a failed internal Codex review, 3 fix
// after a failed internal Qwen review, 4 a Qwen review, 5 first attempts, 6 other work. A fix
// round's findings_source says which of 1-3 (none: 1); a follow-up takes the class of the round
// it finishes (none: 5). So interrupted work resumes first only within its own class. A claim may
// be limited to classes up to max_class: a worker with an interrupted task asks only for work that
// goes before it. A task that came back is held for its last worker for affinityWindow (it has
// the checkpoint; owner 2026-10-27, decision 5), then anyone may claim it. A claim may name one
// task (`task`): a worker takes back the task whose lease it lost, to resume from its own
// checkpoint.
//
// The review loop (owner 2026-10-31; decision 0017 ended the reviews for TauCeti): with auto_rounds on
// and review_loop off, a task that ends with checks_passed is ready-for-operator at once, with no
// review task, and everything below about failed checks, skips and the limit applies. A candidate
// still in-review when reviews go off goes to ready-for-operator at the next sweep, its open review
// cancelled; a verdict that arrives after that records and schedules nothing. When a project has
// review_loop on, a task that ends with checks_passed gets a review task from the gateway, which a
// runner executes (no model code here). Its `complete` carries a verdict. Green: the candidate is
// ready-for-operator (a Codex review by the operator comes next; nothing here opens anything).
// Red: the runner's recipe (result.fix) becomes a fix round at class 3. A task that fails its local
// checks goes the same way: its recipe (more time, its tree if it has one) runs at its own class.
// Both count against the chain's max_review_rounds automatic rounds; past them the task is
// needs-operator and the recipe waits there for the operator, who schedules it with `continue`, as an
// operator round, without sending it again (it may be 2 MiB; a create takes 64 KiB). A task a re-check skipped moves to a
// fresh base when the runner found one (a recipe with `fail`), counted apart since no model failed,
// else it is needs-operator. A chain is an attempt and all its rounds, derived from
// the task id (chainOf) and kept in its own file, so neither a new id nor a restart resets it, and
// the count lives here, not in the runner. A person's task is never counted as automatic, and a
// review a person asked for only records. A verdict goes to the review task and the parent's
// qwen_review, never to the parent's review, which is the operator's. What a finished task owes
// the loop is saved with its end (loop_pending) and cleared when all of it is done; a crash between
// the two is finished at load, each step noticing what is done already (taskloop.go, settle).
//
// Retention (owner 2026-10-27, decision 6): a finished task is dropped only after the owner's
// export to the ledger has marked it (`exported`); a review or decision after that needs another
// export. While too many finished tasks wait for export, create refuses new tasks.
//
// Nothing here is public: specs, prompts and diffs before a pull request stay with the project
// key and the admins. Each task is one file under state_dir/tasks-<project>/, written atomically
// on every change, so the queue survives a restart; the number of tasks and the bytes kept are
// bounded per project.
//
// A failed write (a full disk, a read-only directory) is treated as a crash of that project's
// queue, and recovers the same way (owner 2026-10-31). What is on disk is the truth: a call whose
// change did not reach the disk answers 503 and changes nothing, so it may be repeated, and the
// project's memory is dropped and read again at the next call or sweep, with no rollback to get
// wrong. The save of a task's end, with what it owes the loop, is what decides a call's answer:
// after it a restart loses nothing, so the call answers ok even when a later step of the loop
// fails. That step stops at the failed write, since a chain's count must reach the disk before the
// round it counts, the next call or sweep (every taskSweepEvery) does the debt again, and until it
// is paid a claim answers 503.
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
	// maxInterruptions: a task interrupted this often fails at its next give-back or expiry, so
	// a task that keeps stopping its worker still ends.
	maxInterruptions = 50
	maxTaskEvents    = 20
	// taskSweepEvery: expired leases return to the queue at least this often, and on every call.
	taskSweepEvery = 30 * time.Second
	// affinityWindow: a task that came back from a lease waits this long for its last worker,
	// which holds its checkpoint (owner 2026-10-27: about 10 minutes).
	affinityWindow      = 10 * time.Minute
	maxTaskReviewDetail = 16 << 10
)

// The claim classes, lower first (the owner's work priority, 2026-10-29).
const (
	classFixReview    = 1 // the PR is open and reviewers wait; main drifts
	classFixPrereview = 2 // after a failed internal Codex review
	classFixQwen      = 3 // after a failed internal Qwen review
	classReview       = 4 // a Qwen review of a candidate
	classAttempt      = 5
	classOther        = 6
)

// Where a fix round's findings came from (Task.FindingsSource).
const (
	findingsReview        = "review"
	findingsPrereview     = "prereview"
	findingsPrereviewQwen = "prereview-qwen"
	kindsHelp             = "attempt, fix_round, followup, review or other"
)

// The review loop.
const (
	// gatewayCreator marks a task the loop created; a client cannot claim the name.
	gatewayCreator = "gateway"
	// Where a candidate stands in the loop (Task.Stage).
	StageInReview       = "in-review"
	StageReady          = "ready-for-operator"
	StageFixScheduled   = "fix-scheduled"
	StageNeedsOperator  = "needs-operator"
	reviewSuffix        = ".review"
	skippedPrefix       = "skipped:" // a failure's reason when a re-check skipped the task
	judgeQwen           = "qwen"
	defaultReviewRounds = 3
	maxChainVerdicts    = 100
	maxTaskRecipeSpec   = 2 << 20 // a fix round's spec from a runner: its findings and tree patch
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
	// taskKinds and their claim class without a findings source (taskClass).
	taskKinds = map[string]int{"fix_round": classFixReview, "followup": classAttempt, "review": classReview,
		"attempt": classAttempt, "other": classOther}
	findingsClass = map[string]int{findingsReview: classFixReview, findingsPrereview: classFixPrereview,
		findingsPrereviewQwen: classFixQwen}
	taskRefRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]{0,199}$`)
)

// taskClass is a task's claim class: a fix round's or a follow-up's from where its findings came
// from, any other from its kind. A follow-up without a source finishes a first attempt.
func taskClass(kind, source string) int {
	if c, ok := findingsClass[source]; ok && (kind == "fix_round" || kind == "followup") {
		return c
	}
	return taskKinds[kind]
}

// Task is one unit of work in a project's queue.
type Task struct {
	ID      string `json:"id"` // the client's id (the runner's attempt id), unique per project
	Project string `json:"project"`
	Kind    string `json:"kind"`  // attempt, fix_round, followup, review, other
	Class   int    `json:"class"` // the claim class (taskClass), recomputed at load
	// FindingsSource: what a fix round answers, or what the round a follow-up finishes answers:
	// review (TauCeti's), prereview (internal, Codex), prereview-qwen (internal, Qwen).
	FindingsSource string `json:"findings_source,omitempty"`
	// Parent: the task whose candidate a review task reviews.
	Parent string `json:"parent,omitempty"`
	// Chain: the attempt and all its rounds (chainOf). Stage: where a candidate stands in the loop.
	Chain string `json:"chain,omitempty"`
	Stage string `json:"stage,omitempty"`
	// RoundKind: on a task the loop made, why (taskloop.go).
	RoundKind string          `json:"round_kind,omitempty"`
	Title     string          `json:"title,omitempty"`
	Spec      json.RawMessage `json:"spec,omitempty"` // opaque to the gateway: what the worker needs
	BaseRef   string          `json:"base_ref,omitempty"`
	PR        int             `json:"pr,omitempty"` // the pull request a fix round revises
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
	Requeued  bool        `json:"requeued,omitempty"`
	Seq       int64       `json:"seq"` // creation order within the project
	CreatedAt time.Time   `json:"created_at"`
	UpdatedAt time.Time   `json:"updated_at"`
	Lease     *taskLease  `json:"lease,omitempty"` // the current lease, or the last one when finished
	Result    *TaskResult `json:"result,omitempty"`
	Error     string      `json:"error,omitempty"` // the last failure, expiry or cancel reason
	Events    []taskEvent `json:"events,omitempty"`
	// PreferWorker holds a task that came back for its last worker until PreferUntil.
	PreferWorker string     `json:"prefer_worker,omitempty"`
	PreferUntil  *time.Time `json:"prefer_until,omitempty"`
	// Review is the operator's internal review verdict, Codex's (optional: the internal review is
	// temporary, owner 2026-10-27), Decision the owner's; both only on a done task. QwenReview is
	// the loop's verdict on this candidate: it never replaces Review. A review task's own Review
	// holds its verdict.
	Review     *TaskReview   `json:"review,omitempty"`
	QwenReview *TaskReview   `json:"qwen_review,omitempty"`
	Decision   *TaskDecision `json:"decision,omitempty"`
	// NextFix: on a review task whose fix round was not scheduled (the limit): the recipe, for the
	// operator (coord.py add <review task>).
	NextFix *taskNew `json:"next_fix,omitempty"`
	// ContinuedAs: the round the operator made of NextFix (continue).
	ContinuedAs string `json:"continued_as,omitempty"`
	// LoopPending: what this finished task still owes the loop, on disk only between the save of
	// its end and the end of settle (taskloop.go).
	LoopPending *loopPending `json:"loop_pending,omitempty"`
	// ExportedAt: the owner's export took the task as it was at that UpdatedAt; nil = not (again).
	ExportedAt *time.Time `json:"exported_at,omitempty"`

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
	// Review: a review task's verdict, with its `complete`; Fix: the fix round a red verdict asks
	// for. The gateway moves both out of the result and acts on them in that one step.
	Review *TaskReview `json:"review,omitempty"`
	Fix    *taskNew    `json:"fix,omitempty"`
}

// TaskReview is the internal pre-review's verdict on a done task's result.
type TaskReview struct {
	Verdict  string          `json:"verdict"`            // green or red
	By       string          `json:"by"`                 // the reviewer: owner-codex, qwen-judge, ...
	Judge    string          `json:"judge,omitempty"`    // the judge model: qwen goes to qwen_review
	Round    int             `json:"round,omitempty"`    // the internal review round
	Findings *int            `json:"findings,omitempty"` // how many findings
	Summary  string          `json:"summary,omitempty"`  // one line
	Detail   json.RawMessage `json:"detail,omitempty"`   // opaque object, ≤ 16 KB
	At       time.Time       `json:"at"`
}

// TaskDecision is the owner's decision on a done task's candidate (runner_service.py decide).
type TaskDecision struct {
	Decision string    `json:"decision"` // approved, rejected, opened
	PR       int       `json:"pr,omitempty"`
	By       string    `json:"by"`
	Note     string    `json:"note,omitempty"`
	At       time.Time `json:"at"`
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
	// FindingsSource and Parent: Task's.
	FindingsSource string `json:"findings_source,omitempty"`
	Parent         string `json:"parent,omitempty"`
}

// taskControl is the body of claim, heartbeat, complete, fail and cancel (the fields each needs).
type taskControl struct {
	Worker   string      `json:"worker,omitempty"`
	Lease    string      `json:"lease,omitempty"`
	LeaseSec int         `json:"lease_seconds,omitempty"` // claim: the lease's TTL
	Kinds    []string    `json:"kinds,omitempty"`         // claim: only these kinds
	MaxClass int         `json:"max_class,omitempty"`     // claim: only classes up to this one
	Result   *TaskResult `json:"result,omitempty"`        // complete
	Reason   string      `json:"reason,omitempty"`        // fail, cancel
	Requeue  bool        `json:"requeue,omitempty"`       // fail: give the task back (an interruption: its try continues)
	Fix      *taskNew    `json:"fix,omitempty"`           // fail: the move of a skipped task to a fresh base
	Task     string      `json:"task,omitempty"`          // claim: this task only (a worker taking its own back)
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
	if !full {
		c.Spec = nil
		c.NextFix = nil
		if t.LoopPending != nil { // the step stays in sight; its recipe can be megabytes
			lp := *t.LoopPending
			lp.Fix = nil
			c.LoopPending = &lp
		}
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
	if t.Review != nil {
		v := *t.Review
		c.Review = &v
	}
	if t.QwenReview != nil {
		v := *t.QwenReview
		c.QwenReview = &v
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
	if c.Review != nil {
		c.Review.Detail = nil
	}
	if c.QwenReview != nil {
		c.QwenReview.Detail = nil
	}
	return c
}
