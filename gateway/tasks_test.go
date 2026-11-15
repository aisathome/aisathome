// SPDX-License-Identifier: Apache-2.0
package gateway

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// taskRig is a gateway with two projects and a clock the test moves.
type taskRig struct {
	t   *testing.T
	g   *Gateway
	now time.Time
}

func newTaskRig(t *testing.T, dir string) *taskRig {
	t.Helper()
	r := &taskRig{t: t, g: statusGateway(t, dir), now: time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)}
	r.g.tasks.now = func() time.Time { return r.now }
	t.Cleanup(func() { r.g.Close() })
	return r
}

func (r *taskRig) call(method, path, key string, body any) (*httptest.ResponseRecorder, map[string]any) {
	r.t.Helper()
	var rd *bytes.Reader
	switch b := body.(type) {
	case nil:
		rd = bytes.NewReader(nil)
	case []byte:
		rd = bytes.NewReader(b)
	case string:
		rd = bytes.NewReader([]byte(b))
	default:
		rd = bytes.NewReader(statusJSON(b))
	}
	req := httptest.NewRequest(method, path, rd)
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.g.Handler().ServeHTTP(w, req)
	var out map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return w, out
}

const tasksPath = "/v1/projects/lab/tasks"

func newTask(id, kind string) map[string]any {
	return map[string]any{"id": id, "kind": kind, "title": "Task " + id, "created_by": "owner",
		"spec": map[string]any{"task": id, "target": "Lab.Foo", "notes": []any{"x"}}}
}

func (r *taskRig) create(id, kind string) {
	r.t.Helper()
	if w, _ := r.call("POST", tasksPath, testProjectKey, newTask(id, kind)); w.Code != http.StatusCreated {
		r.t.Fatalf("create %s: HTTP %d %s", id, w.Code, w.Body)
	}
}

// claim returns the claimed task's id and lease id ("" when nothing is queued).
func (r *taskRig) claim(worker string, ttl int) (string, string) {
	r.t.Helper()
	w, out := r.call("POST", tasksPath+"/claim", testProjectKey, map[string]any{"worker": worker, "lease_seconds": ttl})
	if w.Code == http.StatusNoContent {
		return "", ""
	}
	if w.Code != http.StatusOK {
		r.t.Fatalf("claim: HTTP %d %s", w.Code, w.Body)
	}
	return out["task"].(map[string]any)["id"].(string), out["lease"].(map[string]any)["id"].(string)
}

func (r *taskRig) lease(task, op, worker, lease string, extra map[string]any) *httptest.ResponseRecorder {
	r.t.Helper()
	body := map[string]any{"worker": worker, "lease": lease}
	for k, v := range extra {
		body[k] = v
	}
	w, _ := r.call("POST", tasksPath+"/"+task+"/"+op, testProjectKey, body)
	return w
}

func (r *taskRig) state(id string) string {
	r.t.Helper()
	if t := r.g.tasks.get("lab", id); t != nil {
		return t.State
	}
	return ""
}

var okResult = map[string]any{"outcome": "proved", "checks_passed": true, "steps": 12,
	"prompt_tokens": 1000, "completion_tokens": 100, "model_min": 3.5, "awake_min": 9, "score": map[string]any{"lake_build": "pass", "secret_note": "private"},
	"raw_ref": "raw/2026-10-27/T-A"}

// Only the project's own key may use its queue: none or a non-project key 401, another
// project's 403; nothing is created by a refused call.
func TestTaskAuth(t *testing.T) {
	r := newTaskRig(t, "")
	r.create("T-A", "p2m-v7")
	for _, c := range []struct {
		name, key string
		want      int
	}{
		{"no key", "", http.StatusUnauthorized},
		{"not a project key", "ak-not-a-project-key-at-all-000", http.StatusUnauthorized},
		{"other project's key", otherProjectKey, http.StatusForbidden},
	} {
		for _, call := range []struct{ method, path string }{
			{"POST", tasksPath}, {"GET", tasksPath}, {"GET", tasksPath + "/T-A"}, {"POST", tasksPath + "/claim"},
			{"POST", tasksPath + "/T-A/heartbeat"}, {"POST", tasksPath + "/T-A/complete"}, {"POST", tasksPath + "/T-A/fail"},
			{"POST", tasksPath + "/T-A/cancel"},
			{"POST", tasksPath + "/T-A/exported"}, {"POST", tasksPath + "/T-A/max_tries"},
		} {
			body := map[string]any{"worker": "w1"}
			if call.path == tasksPath {
				body = newTask("T-B", "p2m-v7")
			}
			if w, _ := r.call(call.method, call.path, c.key, body); w.Code != c.want {
				t.Errorf("%s %s with %s: HTTP %d, want %d", call.method, call.path, c.name, w.Code, c.want)
			}
		}
	}
	if r.g.tasks.get("lab", "T-B") != nil || r.state("T-A") != TaskQueued {
		t.Fatal("a refused call changed the queue")
	}
	// The other project's queue is its own.
	w, out := r.call("GET", "/v1/projects/other/tasks", otherProjectKey, nil)
	if w.Code != 200 || len(out["tasks"].([]any)) != 0 {
		t.Fatalf("other project's list: HTTP %d %s", w.Code, w.Body)
	}
}

func (r *taskRig) listed(query string) string {
	r.t.Helper()
	_, out := r.call("GET", tasksPath+query, testProjectKey, nil)
	var ids []string
	for _, x := range out["tasks"].([]any) {
		ids = append(ids, x.(map[string]any)["id"].(string))
	}
	return strings.Join(ids, " ")
}

func TestTaskLeaseExpiry(t *testing.T) {
	r := newTaskRig(t, "")
	r.create("T-A", "p2m-v7")
	id, lease := r.claim("w1", 60)
	r.now = r.now.Add(59 * time.Second)
	if w := r.lease(id, "heartbeat", "w1", lease, nil); w.Code != 200 {
		t.Fatalf("heartbeat in time: HTTP %d %s", w.Code, w.Body)
	}
	r.now = r.now.Add(59 * time.Second) // 118 s after the claim, 59 s after the heartbeat
	if r.state("T-A") != TaskLeased {
		t.Fatal("a heartbeat did not extend the lease")
	}
	if w := r.lease(id, "heartbeat", "w2", lease, nil); w.Code != http.StatusConflict {
		t.Errorf("another worker's heartbeat with the lease: HTTP %d", w.Code)
	}
	r.now = r.now.Add(time.Second) // exactly the TTL after the heartbeat
	r.g.tasks.sweepAll()           // the background check, without any call
	if r.state("T-A") != TaskQueued {
		t.Fatalf("expired lease: state %s, want queued", r.state("T-A"))
	}
	if w := r.lease(id, "heartbeat", "w1", lease, nil); w.Code != http.StatusConflict {
		t.Errorf("heartbeat with an expired lease: HTTP %d", w.Code)
	}
	if w := r.lease(id, "complete", "w1", lease, map[string]any{"result": okResult}); w.Code != http.StatusConflict {
		t.Errorf("complete with an expired lease: HTTP %d", w.Code)
	}
	// The same worker claims it again: only the new lease counts (and it continues try 1,
	// TestTaskInterruptions).
	id2, lease2 := r.claim("w1", 60)
	if id2 != "T-A" || lease2 == lease {
		t.Fatalf("re-claim: %q, new lease %v", id2, lease2 != lease)
	}
	if w := r.lease(id, "heartbeat", "w1", lease, nil); w.Code != http.StatusConflict {
		t.Errorf("heartbeat with the old lease after a re-claim: HTTP %d", w.Code)
	}
	if w := r.lease(id, "heartbeat", "w1", lease2, nil); w.Code != 200 {
		t.Errorf("heartbeat with the new lease: HTTP %d", w.Code)
	}
	// Leases that expire and are claimed by other workers after the window use up tries 2 and
	// 3; the last one expires too and nobody takes it back: the task fails instead of going
	// round for ever.
	for _, w := range []string{"w2", "w3"} {
		r.now = r.now.Add(61 * time.Second)
		r.g.tasks.sweepAll()
		r.now = r.now.Add(affinityWindow)
		if id, _ := r.claim(w, 60); id != "T-A" {
			t.Fatalf("%s claimed %q", w, id)
		}
	}
	if task := r.g.tasks.get("lab", "T-A"); task.Tries != 3 || task.Interruptions != 1 {
		t.Fatalf("tries %d, interruptions %d, want 3 and 1", task.Tries, task.Interruptions)
	}
	r.now = r.now.Add(61 * time.Second)
	r.g.tasks.sweepAll()
	if r.state("T-A") != TaskQueued { // held for w3 for the window
		t.Fatalf("the last try expired: %s, want queued for w3", r.state("T-A"))
	}
	if id, _ := r.claim("w4", 60); id != "" {
		t.Fatalf("w4 claimed %s, held for w3", id)
	}
	r.now = r.now.Add(affinityWindow)
	r.g.tasks.sweepAll()
	task := r.g.tasks.get("lab", "T-A")
	if task.State != TaskFailed || task.Tries != 3 || !strings.Contains(task.Error, "lease expired on try 3 of 3 and w3 did not take it back") {
		t.Fatalf("after 3 expired leases: %s, tries %d, error %q", task.State, task.Tries, task.Error)
	}
	if id, _ := r.claim("w4", 60); id != "" {
		t.Fatalf("a failed task was claimed: %s", id)
	}
}

// Create is idempotent by the client's id: the same content again gives the task (200, not a
// second one), other content under the same id is refused.
func TestTaskCreateIdempotent(t *testing.T) {
	r := newTaskRig(t, "")
	r.create("T-A", "p2m-v7")
	w, out := r.call("POST", tasksPath, testProjectKey, newTask("T-A", "p2m-v7"))
	if w.Code != 200 || out["created"] != false {
		t.Fatalf("same task again: HTTP %d %s", w.Code, w.Body)
	}
	// Whitespace in the spec does not matter.
	raw := `{"id":"T-A","kind":"p2m-v7","title":"Task T-A","created_by":"owner",
		"spec": { "task" : "T-A", "target":"Lab.Foo", "notes":["x"] }}`
	if w, _ := r.call("POST", tasksPath, testProjectKey, raw); w.Code != 200 {
		t.Fatalf("same task, other whitespace: HTTP %d %s", w.Code, w.Body)
	}
	other := newTask("T-A", "p2m-v7")
	other["spec"] = map[string]any{"task": "T-A", "target": "Lab.Bar"}
	if w, _ := r.call("POST", tasksPath, testProjectKey, other); w.Code != http.StatusConflict {
		t.Fatalf("other spec, same id: HTTP %d", w.Code)
	}
	if w, _ := r.call("POST", tasksPath, testProjectKey, newTask("T-A", "other")); w.Code != http.StatusConflict {
		t.Fatalf("other kind, same id: HTTP %d", w.Code)
	}
	_, out = r.call("GET", tasksPath, testProjectKey, nil)
	if n := len(out["tasks"].([]any)); n != 1 {
		t.Fatalf("%d tasks, want 1", n)
	}
}

// Requests are checked: shape, unknown fields, content type, sizes and secret-like text.
func TestTaskValidation(t *testing.T) {
	r := newTaskRig(t, "")
	bad := func(name string, mod func(m map[string]any)) {
		t.Helper()
		m := newTask("T-X", "p2m-v7")
		mod(m)
		if w, _ := r.call("POST", tasksPath, testProjectKey, m); w.Code != http.StatusBadRequest {
			t.Errorf("create %s: HTTP %d %s", name, w.Code, w.Body)
		}
	}
	bad("unknown field", func(m map[string]any) { m["priority"] = 0 })
	bad("undeclared kind", func(m map[string]any) { m["kind"] = "urgent" })
	bad("malformed kind", func(m map[string]any) { m["kind"] = "P2M v7" })
	bad("a removed field", func(m map[string]any) { m["base_ref"] = "a57def6" })
	bad("findings_source", func(m map[string]any) { m["findings_source"] = "review" })
	bad("bad id", func(m map[string]any) { m["id"] = "../etc/passwd" })
	bad("no spec", func(m map[string]any) { delete(m, "spec") })
	bad("spec not an object", func(m map[string]any) { m["spec"] = []any{1} })
	bad("spec too big", func(m map[string]any) { m["spec"] = map[string]any{"x": strings.Repeat("a", maxTaskSpec)} })
	bad("secret in title", func(m map[string]any) { m["title"] = "ghp_" + strings.Repeat("x", 36) })
	bad("no created_by", func(m map[string]any) { delete(m, "created_by") })
	bad("max_tries 11", func(m map[string]any) { m["max_tries"] = 11 })
	req := httptest.NewRequest("POST", tasksPath, bytes.NewReader(statusJSON(newTask("T-X", "p2m-v7"))))
	req.Header.Set("Authorization", "Bearer "+testProjectKey)
	w := httptest.NewRecorder()
	r.g.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusUnsupportedMediaType {
		t.Errorf("no content type: HTTP %d", w.Code)
	}
	if r.g.tasks.get("lab", "T-X") != nil {
		t.Fatal("a refused task was stored")
	}

	r.create("T-A", "p2m-v7")
	id, lease := r.claim("w1", 60)
	if w, _ := r.call("POST", tasksPath+"/claim", testProjectKey, map[string]any{"worker": "w1", "lease_seconds": 5}); w.Code != 400 {
		t.Errorf("lease_seconds 5: HTTP %d", w.Code)
	}
	big := map[string]any{}
	for k, v := range okResult {
		big[k] = v
	}
	big["score"] = map[string]any{"x": strings.Repeat("a", maxTaskScore)}
	if w := r.lease(id, "complete", "w1", lease, map[string]any{"result": big}); w.Code != http.StatusBadRequest {
		t.Errorf("score over 16 KB: HTTP %d", w.Code)
	}
	big["score"] = map[string]any{"x": strings.Repeat("\"", maxTaskCompleteBody/2+1)} // escaped, the body is over 64 KB
	if w := r.lease(id, "complete", "w1", lease, map[string]any{"result": big}); w.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("body over 64 KB: HTTP %d", w.Code)
	}
	for _, f := range []string{"diff", "pr_body", "commit_msg", "review", "fix"} { // the review loop's, removed
		m := map[string]any{"outcome": "candidate", f: "x"}
		if w := r.lease(id, "complete", "w1", lease, map[string]any{"result": m}); w.Code != 400 {
			t.Errorf("result with %s: HTTP %d", f, w.Code)
		}
	}
	if w := r.lease(id, "complete", "w1", lease, map[string]any{"result": map[string]any{"outcome": ""}}); w.Code != 400 {
		t.Errorf("no outcome: HTTP %d", w.Code)
	}
	if w := r.lease(id, "complete", "w1", lease, map[string]any{"result": map[string]any{"outcome": "x", "verdict": "ok"}}); w.Code != 400 {
		t.Errorf("unknown result field: HTTP %d", w.Code)
	}
	if w := r.lease(id, "complete", "w1", "not-a-lease", map[string]any{"result": okResult}); w.Code != 400 {
		t.Errorf("malformed lease: HTTP %d", w.Code)
	}
	// A score just under the limit is fine.
	big["score"] = map[string]any{"x": strings.Repeat("a", maxTaskScore-16)}
	if w := r.lease(id, "complete", "w1", lease, map[string]any{"result": big}); w.Code != 200 {
		t.Fatalf("a big score: HTTP %d %s", w.Code, w.Body)
	}
}

// complete, fail (with and without requeue) and cancel move a task through its states; a
// repeated report by the same lease is accepted; a finished task stays finished.
func TestTaskStateMachine(t *testing.T) {
	r := newTaskRig(t, "")
	r.create("T-A", "p2m-v7")
	id, lease := r.claim("w1", 60)
	if w := r.lease(id, "complete", "w1", lease, map[string]any{"result": okResult}); w.Code != 200 {
		t.Fatalf("complete: HTTP %d %s", w.Code, w.Body)
	}
	if w := r.lease(id, "complete", "w1", lease, map[string]any{"result": okResult}); w.Code != 200 {
		t.Errorf("the same complete again: HTTP %d", w.Code)
	}
	if w := r.lease(id, "fail", "w1", lease, nil); w.Code != http.StatusConflict {
		t.Errorf("fail after done: HTTP %d", w.Code)
	}
	if w, _ := r.call("POST", tasksPath+"/T-A/cancel", testProjectKey, map[string]any{}); w.Code != http.StatusConflict {
		t.Errorf("cancel after done: HTTP %d", w.Code)
	}
	full := r.g.tasks.get("lab", "T-A")
	if full.State != TaskDone || full.Result.RawRef != "raw/2026-10-27/T-A" || full.Result.FinishedAt.IsZero() || full.Lease.ID != "" {
		t.Fatalf("done task: %+v", full)
	}

	// fail without requeue: failed; with requeue: given back, without using up a try.
	r.create("T-B", "p2m-v7")
	id, lease = r.claim("w1", 60)
	if w := r.lease(id, "fail", "w1", lease, map[string]any{"reason": "lake_build infra error"}); w.Code != 200 || r.state("T-B") != TaskFailed {
		t.Fatalf("fail: HTTP %d, %s", w.Code, r.state("T-B"))
	}
	if w := r.lease(id, "fail", "w1", lease, map[string]any{"reason": "lake_build infra error"}); w.Code != 200 {
		t.Errorf("the same fail again: HTTP %d", w.Code)
	}
	m := newTask("T-C", "p2m-v7")
	m["max_tries"] = 2
	r.call("POST", tasksPath, testProjectKey, m)
	for range 3 {
		id, lease = r.claim("w1", 60)
		r.lease(id, "fail", "w1", lease, map[string]any{"requeue": true, "reason": "spot stop"})
	}
	if c := r.g.tasks.get("lab", "T-C"); c.State != TaskQueued || c.Tries != 1 || c.Interruptions != 2 {
		t.Fatalf("3 give-backs with max_tries 2: %s, %d tries, %d interruptions", c.State, c.Tries, c.Interruptions)
	}
	r.call("POST", tasksPath+"/T-C/cancel", testProjectKey, map[string]any{})

	// cancel: a queued task, and a leased one whose worker then loses its lease.
	r.create("T-D", "p2m-v7")
	r.create("T-E", "p2m-v7")
	if w, _ := r.call("POST", tasksPath+"/T-D/cancel", testProjectKey, map[string]any{"reason": "superseded"}); w.Code != 200 || r.state("T-D") != TaskCancelled {
		t.Fatalf("cancel queued: HTTP %d %s", w.Code, r.state("T-D"))
	}
	if w, _ := r.call("POST", tasksPath+"/T-D/cancel", testProjectKey, map[string]any{}); w.Code != 200 {
		t.Errorf("cancel twice: HTTP %d", w.Code)
	}
	id, lease = r.claim("w1", 60)
	if id != "T-E" {
		t.Fatalf("claimed %q after a cancel", id)
	}
	r.call("POST", tasksPath+"/T-E/cancel", testProjectKey, map[string]any{})
	if w := r.lease(id, "heartbeat", "w1", lease, nil); w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "cancelled") {
		t.Errorf("heartbeat after cancel: HTTP %d %s", w.Code, w.Body)
	}
	if w, _ := r.call("POST", tasksPath+"/nope/cancel", testProjectKey, map[string]any{}); w.Code != 404 {
		t.Errorf("cancel unknown: HTTP %d", w.Code)
	}
}

// The list carries summaries (no spec or score) and counts; one task in full has its spec and
// result but never the lease id.
func TestTaskListAndGet(t *testing.T) {
	r := newTaskRig(t, "")
	r.create("T-A", "p2m-v7")
	r.create("T-F", "other")
	id, lease := r.claim("w1", 60)
	r.lease(id, "complete", "w1", lease, map[string]any{"result": okResult})
	w, out := r.call("GET", tasksPath+"?state=done,queued", testProjectKey, nil)
	if w.Code != 200 || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("list: HTTP %d", w.Code)
	}
	body := w.Body.String()
	if strings.Contains(body, "Lab.Foo") || strings.Contains(body, "secret_note") || strings.Contains(body, lease) {
		t.Errorf("the list carries a spec, score or lease id: %s", body)
	}
	if c := out["counts"].(map[string]any); c["done"] != 1.0 || c["queued"] != 1.0 {
		t.Errorf("counts %v", c)
	}
	if w, _ := r.call("GET", tasksPath+"?kind=other", testProjectKey, nil); strings.Contains(w.Body.String(), `"id":"T-A"`) || !strings.Contains(w.Body.String(), `"id":"T-F"`) {
		t.Errorf("kind filter: %s", w.Body)
	}
	if w, _ := r.call("GET", tasksPath+"?state=running", testProjectKey, nil); w.Code != 400 {
		t.Errorf("unknown state filter: HTTP %d", w.Code)
	}
	w, _ = r.call("GET", tasksPath+"/"+id, testProjectKey, nil)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "Lab.Foo") || !strings.Contains(w.Body.String(), "secret_note") || strings.Contains(w.Body.String(), lease) {
		t.Fatalf("get: HTTP %d %s", w.Code, w.Body)
	}
	if w, _ := r.call("GET", tasksPath+"/nope", testProjectKey, nil); w.Code != 404 {
		t.Errorf("get unknown: HTTP %d", w.Code)
	}
}

// The queue, leases and results survive a restart.
func TestTaskPersistence(t *testing.T) {
	dir := t.TempDir()
	r := newTaskRig(t, dir)
	r.create("T-A", "p2m-v7")
	r.create("T-B", "p2m-v7")
	if code, body := r.createWith("T-C", "other", map[string]any{"order": -1}); code != http.StatusCreated {
		t.Fatalf("create T-C: HTTP %d %s", code, body)
	}
	idC, leaseC := r.claim("w1", 600)
	idA, leaseA := r.claim("w1", 600)
	r.lease(idA, "complete", "w1", leaseA, map[string]any{"result": okResult})
	r.g.Close()

	r2 := newTaskRig(t, dir)
	r2.now = r.now.Add(time.Minute)
	if r2.state("T-A") != TaskDone || r2.state("T-B") != TaskQueued || r2.state("T-C") != TaskLeased {
		t.Fatalf("after restart: A %s, B %s, C %s", r2.state("T-A"), r2.state("T-B"), r2.state("T-C"))
	}
	if d := r2.g.tasks.get("lab", "T-A"); d.Result == nil || d.Result.RawRef != "raw/2026-10-27/T-A" {
		t.Fatal("the result was lost")
	}
	if w := r2.lease(idC, "heartbeat", "w1", leaseC, nil); w.Code != 200 {
		t.Fatalf("the lease did not survive: HTTP %d %s", w.Code, w.Body)
	}
	r2.create("T-D", "p2m-v7")
	if d, b := r2.g.tasks.get("lab", "T-D"), r2.g.tasks.get("lab", "T-B"); d.Seq <= b.Seq {
		t.Fatalf("creation order restarted: D %d, B %d", d.Seq, b.Seq)
	}
	// A lease that expired while the gateway was down is returned at the first call.
	r2.g.Close()
	r3 := newTaskRig(t, dir)
	r3.now = r.now.Add(time.Hour)
	if id, _ := r3.claim("w1", 60); id != "T-C" {
		t.Fatalf("after a long restart claimed %q, want T-C back first", id)
	}
}

// The queue is bounded: open tasks per project, and finished tasks kept. Only exported
// finished tasks are dropped (owner 2026-10-27, decision 6): while too many wait for the
// export, create refuses new tasks.
func TestTaskBounds(t *testing.T) {
	r := newTaskRig(t, "")
	for i := 0; i < maxOpenTasks; i++ {
		r.create(fmt.Sprintf("T-%d", i), "p2m-v7")
	}
	if w, _ := r.call("POST", tasksPath, testProjectKey, newTask("T-over", "p2m-v7")); w.Code != http.StatusTooManyRequests {
		t.Fatalf("task %d: HTTP %d", maxOpenTasks+1, w.Code)
	}
	for i := 0; i < maxKeptTasks+10; i++ {
		r.now = r.now.Add(time.Second)
		r.call("POST", fmt.Sprintf("%s/T-%d/cancel", tasksPath, i), testProjectKey, map[string]any{})
	}
	// Nothing exported: all 210 finished tasks are kept, and create waits for the export.
	_, out := r.call("GET", tasksPath+"?state=cancelled", testProjectKey, nil)
	if n := len(out["tasks"].([]any)); n != maxKeptTasks+10 {
		t.Fatalf("%d cancelled tasks kept, want all %d (none exported)", n, maxKeptTasks+10)
	}
	if w, _ := r.call("POST", tasksPath, testProjectKey, newTask("T-new", "p2m-v7")); w.Code != http.StatusTooManyRequests || !strings.Contains(w.Body.String(), "export") {
		t.Fatalf("create with %d finished tasks waiting for export: HTTP %d %s", maxKeptTasks+10, w.Code, w.Body)
	}
	// Export the oldest 30: the 10 oldest are dropped (210 finished, 200 kept), the newer stay.
	r.export(t, 30)
	_, out = r.call("GET", tasksPath+"?state=cancelled", testProjectKey, nil)
	if n := len(out["tasks"].([]any)); n != maxKeptTasks {
		t.Fatalf("after the export %d cancelled tasks kept, want %d", n, maxKeptTasks)
	}
	if r.g.tasks.get("lab", "T-9") != nil || r.g.tasks.get("lab", "T-10") == nil || r.g.tasks.get("lab", fmt.Sprintf("T-%d", maxKeptTasks+9)) == nil {
		t.Fatal("pruning dropped the wrong tasks: the oldest exported go first")
	}
	// 190 wait for export now: create works again.
	if w, _ := r.call("POST", tasksPath, testProjectKey, newTask("T-new", "p2m-v7")); w.Code != http.StatusCreated {
		t.Fatalf("create after the export: HTTP %d %s", w.Code, w.Body)
	}
}

// export marks the oldest n finished, unexported tasks as exported, as the owner's export does.
func (r *taskRig) export(t *testing.T, n int) {
	t.Helper()
	_, out := r.call("GET", tasksPath+"?exported=false", testProjectKey, nil)
	ts := out["tasks"].([]any)
	for i := len(ts) - 1; i >= 0 && n > 0; i, n = i-1, n-1 { // the list is newest first
		x := ts[i].(map[string]any)
		if w, _ := r.call("POST", tasksPath+"/"+x["id"].(string)+"/exported", testProjectKey, map[string]any{"seen": x["updated_at"]}); w.Code != 200 {
			t.Fatalf("exported %s: HTTP %d %s", x["id"], w.Code, w.Body)
		}
	}
}

// A task that came back from a lease waits for its last worker (it holds the checkpoint) for
// affinityWindow; then anyone may claim it. A worker can claim its own task back by id.
func TestTaskAffinity(t *testing.T) {
	r := newTaskRig(t, "")
	r.create("A1", "p2m-v7")
	r.create("A2", "p2m-v7")
	id, lease := r.claim("w1", 60)
	if id != "A1" {
		t.Fatalf("claimed %q", id)
	}
	if w := r.lease(id, "fail", "w1", lease, map[string]any{"requeue": true, "reason": "spot stop"}); w.Code != 200 {
		t.Fatalf("requeue: HTTP %d", w.Code)
	}
	// Another worker gets the next task, not the one held for w1.
	if id, _ := r.claim("w2", 60); id != "A2" {
		t.Fatalf("w2 claimed %q, want A2 (A1 is held for w1)", id)
	}
	if id, _ := r.claim("w2", 60); id != "" {
		t.Fatalf("w2 claimed %q, want nothing", id)
	}
	if w, _ := r.call("POST", tasksPath+"/claim", testProjectKey, map[string]any{"worker": "w2", "task": "A1"}); w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "held for w1") {
		t.Fatalf("w2 claims A1 by id: HTTP %d %s", w.Code, w.Body)
	}
	// The list and the summary show for whom it is held.
	if got := r.g.tasks.get("lab", "A1"); got.PreferWorker != "w1" || got.PreferUntil == nil {
		t.Fatalf("held: %+v", got)
	}
	// After the window, anyone.
	r.now = r.now.Add(affinityWindow)
	if id, _ := r.claim("w2", 60); id != "A1" {
		t.Fatalf("after the window w2 claimed %q, want A1", id)
	}
	if got := r.g.tasks.get("lab", "A1"); got.PreferWorker != "" || got.PreferUntil != nil {
		t.Fatalf("a claim keeps the hold: %+v", got)
	}

	// An expired lease holds the task for its worker too, and the worker takes it back by id
	// although another task is first in the claim order.
	r2 := newTaskRig(t, "")
	r2.create("A1", "p2m-v7")
	id, _ = r2.claim("w1", 60)
	r2.now = r2.now.Add(61 * time.Second)
	r2.create("F1", "other")
	if id, _ := r2.claim("w2", 60); id != "F1" {
		t.Fatalf("w2 claimed %q, want F1 (A1 is held for w1)", id)
	}
	w, out := r2.call("POST", tasksPath+"/claim", testProjectKey, map[string]any{"worker": "w1", "task": "A1"})
	if w.Code != 200 || out["task"].(map[string]any)["id"] != "A1" || out["task"].(map[string]any)["tries"] != 1.0 ||
		out["task"].(map[string]any)["interruptions"] != 1.0 {
		t.Fatalf("w1 claims A1 back: HTTP %d %s", w.Code, w.Body)
	}
	for _, c := range []struct {
		body map[string]any
		want int
	}{
		{map[string]any{"worker": "w1", "task": "A1"}, http.StatusConflict},              // leased now
		{map[string]any{"worker": "w1", "task": "nope"}, http.StatusNotFound},            // no such task
		{map[string]any{"worker": "w1", "task": "A1", "kinds": []string{"p2m-v7"}}, 400}, // one or the other
		{map[string]any{"worker": "w1", "task": "../x"}, 400},                            // not an id
	} {
		if w, _ := r2.call("POST", tasksPath+"/claim", testProjectKey, c.body); w.Code != c.want {
			t.Errorf("claim %v: HTTP %d, want %d", c.body, w.Code, c.want)
		}
	}
	if w := r2.lease("A1", "heartbeat", "w1", out["lease"].(map[string]any)["id"].(string), map[string]any{"task": "A1"}); w.Code != 400 {
		t.Errorf("heartbeat with task: HTTP %d", w.Code)
	}
}

// A worker whose checkpoints any worker can resume claims with hold_seconds 0: a task it gives back,
// or whose lease expires, is held for nobody (the stateless spot runner, 2026-11-12). Between 0 and
// affinityWindow the hold is that long; out of range, or on another call, it is refused.
func TestTaskHoldSeconds(t *testing.T) {
	r := newTaskRig(t, "")
	r.create("A1", "p2m-v7")
	claim := func(worker string, hold any) (string, string) {
		t.Helper()
		w, out := r.call("POST", tasksPath+"/claim", testProjectKey, map[string]any{"worker": worker, "lease_seconds": 60, "hold_seconds": hold})
		if w.Code == http.StatusNoContent {
			return "", ""
		}
		if w.Code != 200 {
			t.Fatalf("claim: HTTP %d %s", w.Code, w.Body)
		}
		return out["task"].(map[string]any)["id"].(string), out["lease"].(map[string]any)["id"].(string)
	}
	_, lease := claim("w1", 0)
	if w := r.lease("A1", "fail", "w1", lease, map[string]any{"requeue": true, "reason": "spot notice"}); w.Code != 200 {
		t.Fatalf("requeue: HTTP %d", w.Code)
	}
	// another worker at once, continuing the try
	id, lease := claim("w2", 0)
	if got := r.g.tasks.get("lab", "A1"); id != "A1" || got.Tries != 1 || got.Interruptions != 1 {
		t.Fatalf("w2 claimed %q: %+v", id, got)
	}
	if w := r.lease("A1", "heartbeat", "w2", lease, map[string]any{"hold_seconds": 0}); w.Code != 400 {
		t.Errorf("heartbeat with hold_seconds: HTTP %d", w.Code)
	}
	// an expiry: free at once, as a new try (only a give-back continues a try for another worker)
	r.now = r.now.Add(61 * time.Second)
	if id, _ := claim("w3", 120); id != "A1" {
		t.Fatalf("after w2's expiry w3 claimed %q, want A1", id)
	}
	if got := r.g.tasks.get("lab", "A1"); got.Tries != 2 {
		t.Fatalf("tries after the expiry: %+v", got)
	}
	// hold_seconds 120: held for w3 that long, then free
	r.now = r.now.Add(61 * time.Second)
	if id, _ := claim("w4", 0); id != "" {
		t.Fatalf("w4 claimed %q within w3's 120 s hold", id)
	}
	r.now = r.now.Add(120 * time.Second)
	if id, _ := claim("w4", 0); id != "A1" {
		t.Fatalf("after w3's hold w4 claimed %q, want A1", id)
	}
	// an expiry on the last try with hold 0 fails at the next sweep instead of waiting
	r.now = r.now.Add(61 * time.Second)
	if id, _ := claim("w5", 0); id != "" {
		t.Fatalf("w5 claimed %q after the expiry on the last try", id)
	}
	if st := r.state("A1"); st != TaskFailed {
		t.Fatalf("A1 is %s after an expiry on the last try with hold 0, want failed", st)
	}
	for _, h := range []any{-1, int(affinityWindow/time.Second) + 1, "x"} {
		if w, _ := r.call("POST", tasksPath+"/claim", testProjectKey, map[string]any{"worker": "w1", "hold_seconds": h}); w.Code != 400 {
			t.Errorf("claim with hold_seconds %v: HTTP %d", h, w.Code)
		}
	}
}

// takeBack claims one task by id: its HTTP status and lease id.
func (r *taskRig) takeBack(worker, task string) (int, string) {
	r.t.Helper()
	w, out := r.call("POST", tasksPath+"/claim", testProjectKey, map[string]any{"worker": worker, "task": task, "lease_seconds": 60})
	if w.Code != http.StatusOK {
		return w.Code, ""
	}
	return w.Code, out["lease"].(map[string]any)["id"].(string)
}

func (r *taskRig) counts(id string) (int, int) {
	r.t.Helper()
	t := r.g.tasks.get("lab", id)
	return t.Tries, t.Interruptions
}

// Interruptions do not use up tries (owner 2026-10-29): a task given back by its worker (the
// runner on a spot stop) continues its try at the next claim, by any worker; an expired lease
// continues it only when the same worker takes the task back within the window; a real failure
// ends the task; a task interrupted maxInterruptions times fails; the state survives a restart.
func TestTaskInterruptions(t *testing.T) {
	dir := t.TempDir()
	r := newTaskRig(t, dir)
	m := newTask("T-A", "p2m-v7")
	m["max_tries"] = 2
	r.call("POST", tasksPath, testProjectKey, m)

	// Spot stops: give back, take back by id, ten times; max_tries is 2 and it stays try 1.
	_, lease := r.takeBack("w1", "T-A")
	for i := 1; i <= 10; i++ {
		w, out := r.call("POST", tasksPath+"/T-A/fail", testProjectKey, map[string]any{"worker": "w1", "lease": lease, "requeue": true, "reason": "runner stop: service stop"})
		if w.Code != 200 || out["state"] != TaskQueued || out["tries"] != 1.0 || out["interruptions"] != float64(i-1) {
			t.Fatalf("give-back %d: HTTP %d %s", i, w.Code, w.Body)
		}
		r.now = r.now.Add(2 * time.Minute)
		var code int
		if code, lease = r.takeBack("w1", "T-A"); code != 200 {
			t.Fatalf("take-back %d: HTTP %d", i, code)
		}
		if tries, n := r.counts("T-A"); tries != 1 || n != i {
			t.Fatalf("after take-back %d: tries %d, interruptions %d", i, tries, n)
		}
	}
	if ev := r.g.tasks.get("lab", "T-A").Events; !strings.Contains(ev[len(ev)-1].Note, "try 1 of 2 continues after an interruption (given_back; 10 so far)") {
		t.Errorf("claim event: %+v", ev[len(ev)-1])
	}
	// The interruption survives a restart: given back, the gateway restarts, and the next
	// claim, by another worker after the window, still continues the try.
	r.lease("T-A", "fail", "w1", lease, map[string]any{"requeue": true, "reason": "spot stop"})
	r.g.Close()
	r = newTaskRig(t, dir)
	r.now = time.Date(2026, 10, 27, 13, 0, 0, 0, time.UTC) // the hold has passed: anyone may claim
	if id, _ := r.claim("w2", 60); id != "T-A" {
		t.Fatalf("w2 claimed %q", id)
	}
	if tries, n := r.counts("T-A"); tries != 1 || n != 11 {
		t.Fatalf("w2 after a give-back: tries %d, interruptions %d, want 1 and 11", tries, n)
	}

	// An expired lease taken back by the same worker within the window continues the try.
	r.now = r.now.Add(61 * time.Second)
	r.g.tasks.sweepAll()
	r.now = r.now.Add(affinityWindow - time.Second)
	if code, _ := r.takeBack("w2", "T-A"); code != 200 {
		t.Fatalf("w2 takes back within the window: HTTP %d", code)
	}
	if tries, n := r.counts("T-A"); tries != 1 || n != 12 {
		t.Fatalf("expired and taken back: tries %d, interruptions %d, want 1 and 12", tries, n)
	}
	// Expired, and the same worker comes back only after the window: a new try.
	r.now = r.now.Add(61 * time.Second)
	r.g.tasks.sweepAll()
	r.now = r.now.Add(affinityWindow)
	if code, _ := r.takeBack("w2", "T-A"); code != 200 {
		t.Fatalf("w2 takes back after the window: HTTP %d", code)
	}
	if tries, n := r.counts("T-A"); tries != 2 || n != 12 {
		t.Fatalf("taken back late: tries %d, interruptions %d, want 2 and 12", tries, n)
	}

	// Expired, and another worker takes it after the window: a new try (T-B, max 3).
	r.create("T-B", "p2m-v7")
	if code, _ := r.takeBack("w1", "T-B"); code != 200 {
		t.Fatalf("w1 claims T-B: HTTP %d", code)
	}
	r.now = r.now.Add(61 * time.Second)
	r.g.tasks.sweepAll()
	r.now = r.now.Add(affinityWindow)
	if code, _ := r.takeBack("w3", "T-B"); code != 200 {
		t.Fatalf("w3 claims T-B: HTTP %d", code)
	}
	if tries, n := r.counts("T-B"); tries != 2 || n != 0 {
		t.Fatalf("another worker after an expiry: tries %d, interruptions %d, want 2 and 0", tries, n)
	}
	// The last try expires: its worker taking it back within the window continues it.
	r.now = r.now.Add(61 * time.Second)
	r.g.tasks.sweepAll()
	r.now = r.now.Add(affinityWindow)
	if code, _ := r.takeBack("w4", "T-B"); code != 200 { // try 3 of 3
		t.Fatalf("w4 claims T-B: HTTP %d", code)
	}
	r.now = r.now.Add(61 * time.Second)
	r.g.tasks.sweepAll()
	r.now = r.now.Add(affinityWindow - time.Second)
	r.g.tasks.sweepAll()
	if r.state("T-B") != TaskQueued {
		t.Fatalf("the last try expired, inside the window: %s", r.state("T-B"))
	}
	code, lease := r.takeBack("w4", "T-B")
	if tries, n := r.counts("T-B"); code != 200 || tries != 3 || n != 1 {
		t.Fatalf("last try taken back: HTTP %d, tries %d, interruptions %d", code, tries, n)
	}

	// A real failure ends the task at once, whatever tries are left.
	if w := r.lease("T-B", "fail", "w4", lease, map[string]any{"reason": "exit 1 after 3 tries on w4"}); w.Code != 200 || r.state("T-B") != TaskFailed {
		t.Fatalf("fail: HTTP %d, %s", w.Code, r.state("T-B"))
	}
	r.create("T-C", "p2m-v7")
	_, lease = r.takeBack("w1", "T-C")
	if w := r.lease("T-C", "fail", "w1", lease, map[string]any{"reason": "lake_build infra error"}); w.Code != 200 || r.state("T-C") != TaskFailed {
		t.Fatalf("fail on try 1: HTTP %d, %s", w.Code, r.state("T-C"))
	}
	if tries, n := r.counts("T-C"); tries != 1 || n != 0 {
		t.Fatalf("failed: tries %d, interruptions %d", tries, n)
	}

	// The interruption cap: a task that keeps being interrupted still ends.
	r.create("T-D", "p2m-v7")
	_, lease = r.takeBack("w1", "T-D")
	for i := 0; i < maxInterruptions; i++ {
		r.lease("T-D", "fail", "w1", lease, map[string]any{"requeue": true, "reason": "spot stop"})
		_, lease = r.takeBack("w1", "T-D")
	}
	if tries, n := r.counts("T-D"); tries != 1 || n != maxInterruptions || r.state("T-D") != TaskLeased {
		t.Fatalf("at the cap: %s, tries %d, interruptions %d", r.state("T-D"), tries, n)
	}
	w, out := r.call("POST", tasksPath+"/T-D/fail", testProjectKey, map[string]any{"worker": "w1", "lease": lease, "requeue": true, "reason": "spot stop"})
	ev := r.g.tasks.get("lab", "T-D").Events
	if w.Code != 200 || out["state"] != TaskFailed || !strings.Contains(ev[len(ev)-1].Note, "failed after 50 interruptions") {
		t.Fatalf("give-back beyond the cap: HTTP %d %s", w.Code, w.Body)
	}
}

// max_tries of an open task can be raised (or lowered to the tries started) with the project
// key: 1 to 10; not on a finished task.
func TestTaskMaxTries(t *testing.T) {
	r := newTaskRig(t, "")
	r.create("T-A", "p2m-v7")
	set := func(id string, body any) (int, map[string]any) {
		w, out := r.call("POST", tasksPath+"/"+id+"/max_tries", testProjectKey, body)
		return w.Code, out
	}
	for _, body := range []any{map[string]any{"max_tries": 0}, map[string]any{"max_tries": 11}, map[string]any{"max_tries": -1},
		map[string]any{}, map[string]any{"max_tries": "5"}, map[string]any{"max_tries": 5, "worker": "w1"}, `{"max_tries": 5} {}`} {
		if code, _ := set("T-A", body); code != 400 {
			t.Errorf("max_tries %v: HTTP %d, want 400", body, code)
		}
	}
	if code, _ := set("nope", map[string]any{"max_tries": 5}); code != 404 {
		t.Errorf("unknown task: HTTP %d", code)
	}
	if got := r.g.tasks.get("lab", "T-A").MaxTries; got != defaultMaxTries {
		t.Fatalf("a refused call changed max_tries to %d", got)
	}

	// Try 3 of 3 expired and its worker does not come back: raised to 5 within the window, the
	// task does not fail but goes to the next worker as try 4.
	for _, w := range []string{"w1", "w2", "w3"} {
		if id, _ := r.claim(w, 60); id != "T-A" {
			t.Fatalf("%s claimed %q", w, id)
		}
		r.now = r.now.Add(61 * time.Second)
		r.g.tasks.sweepAll()
		r.now = r.now.Add(affinityWindow - time.Minute)
		if w == "w3" {
			if code, out := set("T-A", map[string]any{"max_tries": 5}); code != 200 || out["max_tries"] != 5.0 || out["tries"] != 3.0 {
				t.Fatalf("raise: HTTP %d %v", code, out)
			}
		}
		r.now = r.now.Add(time.Minute)
	}
	r.g.tasks.sweepAll()
	if r.state("T-A") != TaskQueued {
		t.Fatalf("raised, after the window: %s, want queued", r.state("T-A"))
	}
	id, lease := r.claim("w4", 60)
	if tries, _ := r.counts("T-A"); id != "T-A" || tries != 4 {
		t.Fatalf("w4 claimed %q as try %d", id, tries)
	}
	if !hasEvent(r.g.tasks.get("lab", "T-A").Events, "max_tries", "3 -> 5") {
		t.Error("no max_tries event")
	}
	// Lower, but not below the tries started; the same value again is fine.
	if code, _ := set("T-A", map[string]any{"max_tries": 3}); code != 409 {
		t.Errorf("below the tries: HTTP %d", code)
	}
	for _, n := range []int{4, 4, 10} {
		if code, out := set("T-A", map[string]any{"max_tries": n}); code != 200 || out["max_tries"] != float64(n) || out["state"] != TaskLeased {
			t.Errorf("max_tries %d on a leased task: HTTP %d %v", n, code, out)
		}
	}
	// Not on a finished task.
	r.lease(id, "complete", "w4", lease, map[string]any{"result": okResult})
	if code, _ := set("T-A", map[string]any{"max_tries": 10}); code != 409 {
		t.Errorf("a done task: HTTP %d", code)
	}
}

func hasEvent(ev []taskEvent, what, note string) bool {
	for _, e := range ev {
		if e.What == what && e.Note == note {
			return true
		}
	}
	return false
}
