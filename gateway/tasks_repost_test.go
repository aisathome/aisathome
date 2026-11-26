// A cancelled task that never ran may be created again under its id (tasks.go, Re-creating a
// cancelled task).
//
// SPDX-License-Identifier: Apache-2.0
package gateway

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"
)

func (r *taskRig) cancelTask(id string) {
	r.t.Helper()
	if w, _ := r.call("POST", tasksPath+"/"+id+"/cancel", testProjectKey, map[string]any{"reason": "withdrawn", "closed_by": "T-X"}); w.Code != 200 {
		r.t.Fatalf("cancel %s: HTTP %d %s", id, w.Code, w.Body)
	}
}

// reposted is newTask with repost_cancelled.
func reposted(id, kind string) map[string]any {
	n := newTask(id, kind)
	n["repost_cancelled"] = true
	return n
}

// repost is the create of id with repost_cancelled and another spec, title, order, labels and
// max_tries.
func repost(id string) map[string]any {
	n := reposted(id, "other")
	n["title"], n["order"], n["max_tries"], n["role"] = "Again "+id, -3, 5, "outline"
	n["spec"] = map[string]any{"task": id, "scaffold": "v2"}
	n["labels"] = map[string]string{"trial": "t9"}
	return n
}

// A cancelled task that never ran is replaced by a create of its id with repost_cancelled: 201 and
// created, the new content, queued, nothing of the cancel left, a new place in creation order, and
// claimed by its new order. The same create again is then an ordinary repeat (200). Without the flag
// a create answers as before: 200 for the same content, 409 for other, and the task stays cancelled.
func TestTaskRepostCancelledUnrun(t *testing.T) {
	r := newTaskRig(t, "")
	r.createWith("T-A", "p2m-v7", map[string]any{"order": 5})
	r.createWith("T-B", "p2m-v7", map[string]any{"order": -1})
	r.createWith("T-C", "p2m-v7", map[string]any{"order": 1})
	if w, _ := r.call("POST", tasksPath+"/T-A/max_tries", testProjectKey, map[string]any{"max_tries": 4}); w.Code != 200 {
		t.Fatalf("max_tries: HTTP %d %s", w.Code, w.Body)
	}
	r.cancelTask("T-A")
	before := r.g.tasks.get("lab", "T-A")
	r.now = r.now.Add(time.Hour)
	other := repost("T-A")
	delete(other, "repost_cancelled")
	if w, _ := r.call("POST", tasksPath, testProjectKey, other); w.Code != http.StatusConflict {
		t.Fatalf("other content without the flag: HTTP %d %s", w.Code, w.Body)
	}
	same := newTask("T-A", "p2m-v7")
	same["order"], same["max_tries"] = 5, 4
	if w, out := r.call("POST", tasksPath, testProjectKey, same); w.Code != 200 || out["created"] != false {
		t.Fatalf("same content without the flag: HTTP %d %s", w.Code, w.Body)
	}
	if a := r.g.tasks.get("lab", "T-A"); a.State != TaskCancelled || a.Seq != before.Seq || !a.UpdatedAt.Equal(before.UpdatedAt) {
		t.Fatalf("a create without the flag changed the cancelled task: %+v", a)
	}

	w, out := r.call("POST", tasksPath, testProjectKey, repost("T-A"))
	if w.Code != http.StatusCreated || out["created"] != true {
		t.Fatalf("repost: HTTP %d %s", w.Code, w.Body)
	}
	a := r.g.tasks.get("lab", "T-A")
	if a.State != TaskQueued || a.Kind != "other" || a.Title != "Again T-A" || a.Order != -3 || a.MaxTries != 5 ||
		a.Role != "outline" || a.Labels["trial"] != "t9" || string(a.Spec) != `{"scaffold":"v2","task":"T-A"}` {
		t.Fatalf("replaced task: %+v spec %s", a, a.Spec)
	}
	if a.Error != "" || a.ClosedBy != "" || a.Tries != 0 || a.Try != 1 || a.Live != nil || len(a.Events) != 1 ||
		a.Events[0].What != "created" || !a.CreatedAt.Equal(r.now) || !a.UpdatedAt.Equal(r.now) || a.Seq <= before.Seq {
		t.Fatalf("replaced task keeps the old one: %+v", a)
	}
	if got, want := r.listed("?state=queued"), "T-A T-B T-C"; got != want {
		t.Fatalf("queue %q, want %q", got, want)
	}
	if id, _ := r.claim("w1", 60); id != "T-A" {
		t.Fatalf("claimed %q, want T-A (order -3)", id)
	}
	if w, out := r.call("POST", tasksPath, testProjectKey, repost("T-A")); w.Code != 200 || out["created"] != false {
		t.Fatalf("repeat after the repost: HTTP %d %s", w.Code, w.Body)
	}

	// The same content as before the cancel (a bench task posted again as it was) replaces it too.
	r.create("T-D", "p2m-v7")
	r.cancelTask("T-D")
	if w, out := r.call("POST", tasksPath, testProjectKey, reposted("T-D", "p2m-v7")); w.Code != http.StatusCreated || out["created"] != true {
		t.Fatalf("repost with the same content: HTTP %d %s", w.Code, w.Body)
	}
	if r.state("T-D") != TaskQueued {
		t.Fatalf("T-D %s", r.state("T-D"))
	}
	_, out = r.call("GET", tasksPath, testProjectKey, nil)
	if n := len(out["tasks"].([]any)); n != 4 {
		t.Fatalf("%d tasks, want 4", n)
	}
}

// Any other existing task stays as it is, with repost_cancelled too: a cancelled task that ran (a
// try, or a lease that was given back or cancelled), and a queued, leased, done or failed one. The
// same content answers 200 (not created), other content 409.
func TestTaskRepostOnlyUnrun(t *testing.T) {
	r := newTaskRig(t, "")
	// cancelled after an interruption: tries 1, back in the queue, then cancelled
	r.create("RUN-1", "p2m-v7")
	lease := r.claimID("w1", "RUN-1")
	r.lease("RUN-1", "fail", "w1", lease, map[string]any{"requeue": true, "reason": "spot stop"})
	r.cancelTask("RUN-1")
	// cancelled while leased
	r.create("RUN-2", "p2m-v7")
	r.claimID("w1", "RUN-2")
	r.cancelTask("RUN-2")
	r.create("Q", "p2m-v7")
	r.create("L", "p2m-v7")
	r.claimID("w1", "L")
	r.create("D", "p2m-v7")
	r.lease("D", "complete", "w1", r.claimID("w1", "D"), map[string]any{"result": okResult})
	r.create("F", "p2m-v7")
	r.lease("F", "fail", "w1", r.claimID("w1", "F"), map[string]any{"reason": "broken"})

	for id, state := range map[string]string{"RUN-1": TaskCancelled, "RUN-2": TaskCancelled, "Q": TaskQueued,
		"L": TaskLeased, "D": TaskDone, "F": TaskFailed} {
		before := r.g.tasks.get("lab", id)
		if w, _ := r.call("POST", tasksPath, testProjectKey, repost(id)); w.Code != http.StatusConflict {
			t.Errorf("%s (%s), other content: HTTP %d, want 409", id, state, w.Code)
		}
		if w, out := r.call("POST", tasksPath, testProjectKey, reposted(id, "p2m-v7")); w.Code != 200 || out["created"] != false {
			t.Errorf("%s (%s), same content: HTTP %d %s, want 200", id, state, w.Code, w.Body)
		}
		after := r.g.tasks.get("lab", id)
		if after.State != state || after.Seq != before.Seq || !after.UpdatedAt.Equal(before.UpdatedAt) || string(after.Spec) != string(before.Spec) {
			t.Errorf("%s changed: %s -> %s", id, state, after.State)
		}
	}
}

// Each piece of evidence of a run alone keeps a cancelled task from being replaced.
func TestTaskCancelledUnrunStrict(t *testing.T) {
	at := time.Now()
	base := func() *Task {
		return &Task{State: TaskCancelled, Events: []taskEvent{{What: "created"}, {What: "max_tries"}, {What: "cancelled"}}}
	}
	if !cancelledUnrun(base()) {
		t.Fatal("a cancelled task that never ran")
	}
	for name, mark := range map[string]func(*Task){
		"queued":        func(t *Task) { t.State = TaskQueued },
		"tries":         func(t *Task) { t.Tries = 1 },
		"started":       func(t *Task) { t.StartedAt = &at },
		"lease":         func(t *Task) { t.Lease = &taskLease{Worker: "w1"} },
		"result":        func(t *Task) { t.Result = &TaskResult{Outcome: "proved"} },
		"interruptions": func(t *Task) { t.Interruptions = 1 },
		"interrupted":   func(t *Task) { t.Interrupted = interruptGivenBack },
		"requeued":      func(t *Task) { t.Requeued = true },
		"prefer_worker": func(t *Task) { t.PreferWorker = "w1" },
		"prefer_until":  func(t *Task) { t.PreferUntil = &at },
		"live":          func(t *Task) { t.Live = &taskLive{} },
		"node_classes":  func(t *Task) { t.NodeClasses = []string{"volunteer"} },
		"compacted":     func(t *Task) { t.CompactedAt = &at },
		"claimed event": func(t *Task) { t.Events = append(t.Events, taskEvent{What: "claimed"}) },
		"a worker":      func(t *Task) { t.Events[2].Worker = "w1" },
	} {
		tk := base()
		mark(tk)
		if cancelledUnrun(tk) {
			t.Errorf("%s: taken for a task that never ran", name)
		}
	}
}

// The replacement is on disk: after a restart the task has its new content, queued, and is
// claimed. A failed write leaves the cancelled task as it was (503).
func TestTaskRepostPersists(t *testing.T) {
	dir := t.TempDir()
	r := newTaskRig(t, dir)
	r.create("T-A", "p2m-v7")
	r.create("T-B", "p2m-v7")
	r.cancelTask("T-A")
	r.diskFull(anyFile)
	if w, _ := r.call("POST", tasksPath, testProjectKey, repost("T-A")); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("repost on a full disk: HTTP %d", w.Code)
	}
	r.diskBack()
	if a := r.g.tasks.get("lab", "T-A"); a.State != TaskCancelled || a.Title != "Task T-A" {
		t.Fatalf("after a failed repost: %s %q", a.State, a.Title)
	}
	if w, _ := r.call("POST", tasksPath, testProjectKey, repost("T-A")); w.Code != http.StatusCreated {
		t.Fatalf("repost: HTTP %d %s", w.Code, w.Body)
	}
	r.memoryIsDisk(dir)
	r.g.Close()

	r2 := newTaskRig(t, dir)
	r2.now = r.now.Add(time.Minute)
	a := r2.g.tasks.get("lab", "T-A")
	if a == nil || a.State != TaskQueued || string(a.Spec) != `{"scaffold":"v2","task":"T-A"}` || a.Error != "" {
		t.Fatalf("after restart: %+v", a)
	}
	if id, _ := r2.claim("w1", 60); id != "T-A" {
		t.Fatalf("claimed %q, want T-A", id)
	}
}

// The public views show the replaced task queued, and a replacement whose labels name a finished
// campaign reopens it, as a new task does.
func TestTaskRepostPublicAndCampaign(t *testing.T) {
	r := autoRig(t, p2mAuto())
	r.putCamp("prove2me", "t9", CampaignRunning, nil)
	r.addTask("prove2me", "o1", map[string]string{"trial": "t9"}, 201)
	r.want(200, "POST", "/v1/projects/prove2me/tasks/o1/cancel", testProjectKey, map[string]any{"reason": "withdrawn"})
	r.now = r.now.Add(7 * time.Hour)
	r.g.sweepCampaigns(r.now)
	r.wantStatus("prove2me", "t9", CampaignFinished, at(r.now.Add(-7*time.Hour)))

	r.now = r.now.Add(time.Minute)
	n := map[string]any{"id": "o1", "kind": "p2m-v7", "title": "o1", "created_by": "owner",
		"spec": map[string]any{"leaf": "o1", "scaffold": "v2"}, "labels": map[string]string{"trial": "t9"}, "repost_cancelled": true}
	r.want(201, "POST", "/v1/projects/prove2me/tasks", testProjectKey, n)
	r.wantStatus("prove2me", "t9", CampaignRunning, nil)

	r.freshV1()
	_, _, page := r.v1("?project=prove2me")
	if len(page.Tasks) != 1 || page.Tasks[0]["state"] != TaskQueued || page.Tasks[0]["kind"] != "p2m-v7" {
		t.Fatalf("public tasks: %v", page.Tasks)
	}
	r.now = r.now.Add(time.Minute) // past the queue view's cache
	w, _ := r.call("GET", "/v1/projects/prove2me/queue", "", nil)
	var q struct {
		Tasks []map[string]any `json:"tasks"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &q); err != nil || len(q.Tasks) != 1 || q.Tasks[0]["state"] != TaskQueued {
		t.Fatalf("public queue: HTTP %d %s", w.Code, w.Body)
	}
}
