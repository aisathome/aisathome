// Live counts of a running task's model calls: a task runs for up to about two
// hours, and until its worker reports the result the public views showed nothing of its progress.
//
// The gateway counts, per running task, the model calls it relays itself for it: the calls whose
// request names the task in the X-Aisathome-Task header (nodework.go). A call counts when its answer
// reached the worker whole (outcome ok; streaming or not), with the numbers the gateway meters for
// it: the node's usage, clamped as for credit (usage.go), and the time the gateway measured from
// sending the request to the node to its end (prefill and any wait on the node included). A call
// that failed, was cancelled or preempted does not count (a preempted call is sent again and counts
// then), so `requests` is close to the worker's steps.
//
// Only for a task that runs (leased) in a project that publishes its queue (public_queue): a header
// naming any other task, or none of the project's, is ignored, with no error to the client. The
// counts live on the task in memory, like node_classes, and reach the disk with its next save (a
// heartbeat, its end), so a restart loses at most the calls since then; a claim that starts a new try
// starts from zero, one that continues an interrupted try goes on counting. Bounded by the task
// queue's own bounds; never prompt text, node ids or their operators.
//
// The public views show them as `live` while the task runs (tasks_public.go, queue_public.go) and
// not once it ended: then the result's numbers, which the worker reports, are the ones that count.
//
// In flight: the counts change only when a call ends, so a long generation looked like a task doing
// nothing with the model. The gateway also counts, per project and task, the calls that named the
// task and have not ended yet (`in_flight`): from the moment the gateway took the request (its wait
// for a node included) to its end, whatever the outcome (ok, failed, cancelled, preempted, refused).
// In memory only, never saved, under its own lock so that a request's start never waits for the
// queue's; an entry goes when its count is back to 0, so the map holds at most the requests in
// flight. A running task shows it in the public views (also before its first call counted) and in
// the project's own task list and task (GET /v1/projects/{id}/tasks): a task's worker can tell a
// task that waits on the model from one that works without it (its tools, its checks after the
// session), and size its claims by that.
//
// SPDX-License-Identifier: Apache-2.0
package gateway

import (
	"sync"
	"time"

	"github.com/aisathome/aisathome/protocol"
)

// taskLive is a task's live count (Task.Live), as kept on disk.
type taskLive struct {
	Requests         int64     `json:"requests"`
	PromptTokens     int64     `json:"prompt_tokens"` // cached ones included, as a node reports them
	CachedTokens     int64     `json:"cached_tokens"` // the part of prompt_tokens read from the cache
	CompletionTokens int64     `json:"completion_tokens"`
	ModelMs          int64     `json:"model_ms"`
	UpdatedAt        time.Time `json:"updated_at,omitzero"` // the last counted call's end; zero before the first
	// InFlight: the calls naming the task that have not ended (liveBegin), set only on a running
	// task's copy for a response (withInFlight), never on the task itself, so never saved.
	InFlight *int64 `json:"in_flight,omitempty"`
}

// publicLive is a running task's live count in the public views. PromptTokens includes the cached
// ones (unlike a result's); the explicit counts have the same meaning as in a task's result
// (tasks_public.go).
type publicLive struct {
	Requests             int64     `json:"requests"`
	PromptTokens         int64     `json:"prompt_tokens"`
	CachedTokens         int64     `json:"cached_tokens"`
	CompletionTokens     int64     `json:"completion_tokens"`
	UncachedPromptTokens int64     `json:"uncached_prompt_tokens"`
	TokensComputed       int64     `json:"tokens_computed"`
	TokensWithCache      int64     `json:"tokens_with_cache"`
	ModelS               float64   `json:"model_s"`
	InFlight             int64     `json:"in_flight"`
	UpdatedAt            time.Time `json:"updated_at,omitzero"` // absent before the first counted call
}

// publicLive is the task's live count for the public views, with inFlight its calls in flight
// (taskStore.inFlight): nil unless it runs and a call counted or is in flight.
func (t *Task) publicLive(inFlight int64) *publicLive {
	if t.State != TaskLeased {
		return nil
	}
	l := t.Live
	if l == nil {
		l = &taskLive{}
	}
	inFlight = max(inFlight, 0)
	if l.Requests <= 0 && inFlight == 0 {
		return nil
	}
	prompt, compl := max(l.PromptTokens, 0), max(l.CompletionTokens, 0)
	cached := min(max(l.CachedTokens, 0), prompt)
	return &publicLive{Requests: max(l.Requests, 0), PromptTokens: prompt, CachedTokens: cached, CompletionTokens: compl,
		UncachedPromptTokens: prompt - cached, TokensComputed: prompt - cached + compl, TokensWithCache: prompt + compl,
		ModelS: float64(max(l.ModelMs, 0)) / 1000, InFlight: inFlight, UpdatedAt: l.UpdatedAt}
}

// liveKey names a project's task in taskStore.inflight.
type liveKey struct{ project, task string }

// liveBegin counts one call of a project's task as in flight until end runs (once, however often
// it is called; the caller defers it, so every way out of the request ends it).
func (s *taskStore) liveBegin(project, id string) (end func()) {
	k := liveKey{project, id}
	s.liveMu.Lock()
	s.inflight[k]++
	s.liveMu.Unlock()
	return sync.OnceFunc(func() {
		s.liveMu.Lock()
		defer s.liveMu.Unlock()
		if s.inflight[k]--; s.inflight[k] <= 0 {
			delete(s.inflight, k)
		}
	})
}

// inFlight is a project's task's calls in flight, 0 for a task that does not run. Any lock order:
// liveMu is taken last and alone.
func (s *taskStore) inFlight(t *Task) int64 {
	if t.State != TaskLeased {
		return 0
	}
	s.liveMu.Lock()
	defer s.liveMu.Unlock()
	return s.inflight[liveKey{t.Project, t.ID}]
}

// withInFlight adds a running task's calls in flight to its copy c for the project's own views: a
// running task always has `live.in_flight` there once a call counted or while one is in flight, so
// a worker can tell this gateway from one that does not count them.
func (s *taskStore) withInFlight(c *Task) *Task {
	if c.State != TaskLeased {
		return c
	}
	n := s.inFlight(c)
	if c.Live == nil && n == 0 {
		return c
	}
	if c.Live == nil {
		c.Live = &taskLive{}
	}
	c.Live.InFlight = &n
	return c
}

// liveStart counts a relayed request as in flight for the task it names, when its project publishes
// its queue (as countLive); the returned func ends it.
func (g *Gateway) liveStart(req *chatReq) (end func()) {
	if req.task == "" || req.project == "" || g.tasks == nil {
		return func() {}
	}
	if pt := g.projects.Load(); pt == nil || pt.byID[req.project] == nil || !pt.byID[req.project].PublicQueue {
		return func() {}
	}
	return g.tasks.liveBegin(req.project, req.task)
}

// noteLive adds one model call of a project's running task to its live count (u: the call's metered
// usage, clamped). A task not loaded, not running or unknown is left alone; nothing is written.
func (s *taskStore) noteLive(project, id string, u protocol.Usage, at time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.byPr[project]
	if p == nil || p.stale {
		return
	}
	t := p.byID[id]
	if t == nil || t.State != TaskLeased {
		return
	}
	if t.Live == nil {
		t.Live = &taskLive{}
	}
	l := t.Live
	l.Requests++
	l.PromptTokens += int64(max(u.PromptTokens, 0))
	l.CachedTokens += int64(min(max(u.CachedTokens, 0), max(u.PromptTokens, 0)))
	l.CompletionTokens += int64(max(u.CompletionTokens, 0))
	l.ModelMs += max(u.DurationMs, 0)
	l.UpdatedAt = at.UTC()
}

// countLive counts a relayed call's metered record for the task its request named, when the call
// ended ok and the task's project publishes its queue.
func (g *Gateway) countLive(req *chatReq, rec *usageRecord) {
	if rec.Outcome != "ok" || req.task == "" || req.project == "" || g.tasks == nil {
		return
	}
	if pt := g.projects.Load(); pt == nil || pt.byID[req.project] == nil || !pt.byID[req.project].PublicQueue {
		return
	}
	g.tasks.noteLive(req.project, req.task, rec.Usage, rec.Time)
}
