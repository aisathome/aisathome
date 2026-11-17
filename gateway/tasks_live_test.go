// SPDX-License-Identifier: Apache-2.0
package gateway

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aisathome/aisathome/protocol"
)

// liveCall is a relayed call of task in project as commit hands it to countLive.
func (r *taskRig) liveCall(project, task, outcome string, u protocol.Usage) {
	r.g.countLive(&chatReq{class: ClassScience, project: project, task: task}, &usageRecord{Time: r.now, Outcome: outcome, Usage: u})
}

var liveUsage = protocol.Usage{PromptTokens: 1200, CachedTokens: 1000, CompletionTokens: 80, DurationMs: 2500}

// The live count: only ok calls of a running task of a public project count; a queued, unknown or
// finished task, another project's, a project without a public queue and a failed call do not. The
// public views show it while the task runs and not after; a heartbeat saves it.
func TestTaskLiveCount(t *testing.T) {
	r := v1Rig(t)
	for _, id := range []string{"t6-a", "t6-q"} {
		r.p2m("", map[string]any{"id": id, "kind": "p2m-v7", "created_by": "owner", "role": "hole", "spec": map[string]any{}})
	}
	lease := r.p2m("/claim", map[string]any{"worker": "w1", "task": "t6-a", "lease_seconds": 600})["lease"].(map[string]any)["id"]
	if w, _ := r.call("POST", "/v1/projects/lab/tasks", otherProjectKey, newTask("LAB-1", "p2m-v7")); w.Code != 201 {
		t.Fatalf("lab task: HTTP %d", w.Code)
	}
	if w, _ := r.call("POST", "/v1/projects/lab/tasks/claim", otherProjectKey, map[string]any{"worker": "w9", "task": "LAB-1"}); w.Code != 200 {
		t.Fatalf("lab claim: HTTP %d", w.Code)
	}

	r.liveCall("prove2me", "t6-a", "ok", liveUsage)
	r.now = r.now.Add(time.Minute)
	r.liveCall("prove2me", "t6-a", "ok", protocol.Usage{PromptTokens: 1300, CachedTokens: 5000, CompletionTokens: 20, DurationMs: 500})
	for _, o := range []string{"failed", "cancelled", outcomePreempted, outcomeBlocked} {
		r.liveCall("prove2me", "t6-a", o, liveUsage)
	}
	r.liveCall("prove2me", "t6-q", "ok", liveUsage)   // queued
	r.liveCall("prove2me", "nosuch", "ok", liveUsage) // unknown
	r.liveCall("lab", "LAB-1", "ok", liveUsage)       // running, but lab's queue is not public
	r.liveCall("lab", "t6-a", "ok", liveUsage)        // another project's task
	r.liveCall("prove2me", "", "ok", liveUsage)       // no header

	s := r.g.tasks
	want := taskLive{Requests: 2, PromptTokens: 2500, CachedTokens: 2300, CompletionTokens: 100, ModelMs: 3000, UpdatedAt: r.now}
	if got := s.get("prove2me", "t6-a").Live; got == nil || *got != want {
		t.Fatalf("live %+v, want %+v", got, want)
	}
	for _, c := range [][2]string{{"prove2me", "t6-q"}, {"lab", "LAB-1"}} {
		if got := s.get(c[0], c[1]).Live; got != nil {
			t.Errorf("%s/%s counted: %+v", c[0], c[1], got)
		}
	}
	// A copy for a response is a copy.
	v := s.get("prove2me", "t6-a")
	v.Live.Requests = 99
	if s.get("prove2me", "t6-a").Live.Requests != 2 {
		t.Fatal("a view shares the live count")
	}

	// GET /v1/tasks: live for the running task, null for the queued one.
	_, body, page := r.v1("?project=prove2me")
	by := map[string]map[string]any{}
	for _, tk := range page.Tasks {
		by[tk["id"].(string)] = tk
	}
	live, _ := by["t6-a"]["live"].(map[string]any)
	if live == nil || live["requests"] != 2.0 || live["prompt_tokens"] != 2500.0 || live["cached_tokens"] != 2300.0 ||
		live["completion_tokens"] != 100.0 || live["model_s"] != 3.0 || live["updated_at"] != "2026-11-15T12:01:00Z" ||
		live["uncached_prompt_tokens"] != 200.0 || live["tokens_computed"] != 300.0 || live["tokens_with_cache"] != 2600.0 || len(live) != 9 {
		t.Fatalf("running task's live: %s", body)
	}
	if l, ok := by["t6-q"]["live"]; !ok || l != nil {
		t.Errorf("queued task's live: %v (present %v)", l, ok)
	}
	// The project's queue: the same fields.
	_, qbody, _ := r.publicQueue()
	if !strings.Contains(qbody, `"live":{"requests":2,"prompt_tokens":2500,"cached_tokens":2300,"completion_tokens":100,"uncached_prompt_tokens":200,"tokens_computed":300,"tokens_with_cache":2600,"model_s":3,"updated_at":"2026-11-15T12:01:00Z"}`) ||
		strings.Count(qbody, `"live"`) != 1 {
		t.Fatalf("public queue: %s", qbody)
	}

	// A heartbeat saves it; the end keeps it private and the public views drop it.
	if w := r.p2mLease("t6-a", "heartbeat", "w1", lease, nil); w != 200 {
		t.Fatalf("heartbeat: HTTP %d", w)
	}
	raw, _ := os.ReadFile(filepath.Join(r.g.cfg.StateDir, "tasks-prove2me", "t6-a.json"))
	if !strings.Contains(string(raw), `"live":{"requests":2,`) {
		t.Fatalf("not saved with the heartbeat: %s", raw)
	}
	if w := r.p2mLease("t6-a", "complete", "w1", lease, map[string]any{"result": okResult}); w != 200 {
		t.Fatalf("complete: HTTP %d", w)
	}
	r.liveCall("prove2me", "t6-a", "ok", liveUsage) // finished: not counted
	if got := s.get("prove2me", "t6-a").Live; got == nil || got.Requests != 2 {
		t.Fatalf("a finished task's count changed: %+v", got)
	}
	r.freshV1()
	_, body, page = r.v1("?project=prove2me")
	for _, tk := range page.Tasks {
		if tk["live"] != nil {
			t.Errorf("%s: live after its end: %s", tk["id"], body)
		}
	}
}

func (r *taskRig) p2mLease(task, op, worker string, lease any, extra map[string]any) int {
	r.t.Helper()
	body := map[string]any{"worker": worker, "lease": lease}
	for k, v := range extra {
		body[k] = v
	}
	w, _ := r.call("POST", p2mTasks+"/"+task+"/"+op, testProjectKey, body)
	return w.Code
}

// A claim that continues an interrupted try goes on counting; one that starts a new try starts at 0.
func TestTaskLiveTries(t *testing.T) {
	r := v1Rig(t)
	r.p2m("", map[string]any{"id": "t6-a", "kind": "p2m-v7", "created_by": "owner", "spec": map[string]any{}})
	lease := r.p2m("/claim", map[string]any{"worker": "w1", "task": "t6-a", "lease_seconds": 30, "hold_seconds": 0})["lease"].(map[string]any)["id"]
	r.liveCall("prove2me", "t6-a", "ok", liveUsage)
	if w := r.p2mLease("t6-a", "fail", "w1", lease, map[string]any{"requeue": true, "reason": "spot stop"}); w != 200 {
		t.Fatalf("give back: HTTP %d", w)
	}
	r.liveCall("prove2me", "t6-a", "ok", liveUsage) // queued again: not counted
	r.p2m("/claim", map[string]any{"worker": "w2", "task": "t6-a", "lease_seconds": 30, "hold_seconds": 0})
	r.liveCall("prove2me", "t6-a", "ok", liveUsage)
	if tk := r.g.tasks.get("prove2me", "t6-a"); tk.Tries != 1 || tk.Live == nil || tk.Live.Requests != 2 {
		t.Fatalf("continued try: tries %d, live %+v", tk.Tries, tk.Live)
	}
	// The lease expires and another worker claims it: a new try.
	r.now = r.now.Add(time.Minute)
	r.p2m("/claim", map[string]any{"worker": "w3", "task": "t6-a", "lease_seconds": 30})
	if tk := r.g.tasks.get("prove2me", "t6-a"); tk.Tries != 2 || tk.Live != nil {
		t.Fatalf("new try: tries %d, live %+v", tk.Tries, tk.Live)
	}
}

// Calls counted from many goroutines while the public views are built: every call counts once (and
// -race finds no race).
func TestTaskLiveConcurrent(t *testing.T) {
	r := v1Rig(t)
	r.p2m("", map[string]any{"id": "t6-a", "kind": "p2m-v7", "created_by": "owner", "spec": map[string]any{}})
	r.p2m("/claim", map[string]any{"worker": "w1", "task": "t6-a"})
	const workers, calls = 8, 50
	var wg sync.WaitGroup
	for range workers {
		wg.Add(2)
		go func() {
			defer wg.Done()
			for range calls {
				r.g.countLive(&chatReq{project: "prove2me", task: "t6-a"}, &usageRecord{Time: time.Now(), Outcome: "ok", Usage: liveUsage})
			}
		}()
		go func() {
			defer wg.Done()
			for range 5 {
				if _, err := r.g.tasks.publicTasks([]string{"prove2me"}); err != nil {
					t.Error(err)
				}
				if _, _, err := r.g.tasks.publicList("prove2me"); err != nil {
					t.Error(err)
				}
				_ = r.g.tasks.get("prove2me", "t6-a")
			}
		}()
	}
	wg.Wait()
	l := r.g.tasks.get("prove2me", "t6-a").Live
	if n := int64(workers * calls); l == nil || l.Requests != n || l.PromptTokens != n*1200 || l.ModelMs != n*2500 {
		t.Fatalf("live %+v after %d calls", l, workers*calls)
	}
}
