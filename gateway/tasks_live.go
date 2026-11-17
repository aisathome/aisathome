// Live counts of a running task's model calls (owner, 2026-11-15): a task runs for up to about two
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
// SPDX-License-Identifier: Apache-2.0
package gateway

import (
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
	UpdatedAt        time.Time `json:"updated_at"` // the last counted call's end
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
	UpdatedAt            time.Time `json:"updated_at"`
}

// publicLive is the task's live count for the public views: nil unless it runs and a call counted.
func (t *Task) publicLive() *publicLive {
	l := t.Live
	if t.State != TaskLeased || l == nil || l.Requests <= 0 {
		return nil
	}
	prompt, compl := max(l.PromptTokens, 0), max(l.CompletionTokens, 0)
	cached := min(max(l.CachedTokens, 0), prompt)
	return &publicLive{Requests: l.Requests, PromptTokens: prompt, CachedTokens: cached, CompletionTokens: compl,
		UncachedPromptTokens: prompt - cached, TokensComputed: prompt - cached + compl, TokensWithCache: prompt + compl,
		ModelS: float64(max(l.ModelMs, 0)) / 1000, UpdatedAt: l.UpdatedAt}
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
