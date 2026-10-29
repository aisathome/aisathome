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

const tasksPath = "/v1/projects/tauceti/tasks"

func newTask(id, kind string) map[string]any {
	return map[string]any{"id": id, "kind": kind, "title": "Task " + id, "created_by": "owner",
		"spec": map[string]any{"attempt": id, "target": "TauCeti.Foo", "notes": []any{"x"}}, "base_ref": "a57def6"}
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
	if t := r.g.tasks.get("tauceti", id); t != nil {
		return t.State
	}
	return ""
}

var okResult = map[string]any{"outcome": "candidate", "checks_passed": true, "diff": "--- a\n+++ b\n", "steps": 12,
	"prompt_tokens": 1000, "completion_tokens": 100, "model_min": 3.5, "awake_min": 9, "score": map[string]any{"lake_build": "pass"},
	"pr_body": "Adds Foo.", "commit_msg": "feat: foo", "raw_ref": "raw/2026-10-27/T-A"}

// Only the project's own key may use its queue: none or a non-project key 401, another
// project's 403; nothing is created by a refused call.
func TestTaskAuth(t *testing.T) {
	r := newTaskRig(t, "")
	r.create("T-A", "attempt")
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
			{"POST", tasksPath + "/T-A/cancel"}, {"POST", tasksPath + "/T-A/review"}, {"POST", tasksPath + "/T-A/decide"},
			{"POST", tasksPath + "/T-A/exported"},
		} {
			body := map[string]any{"worker": "w1"}
			if call.path == tasksPath {
				body = newTask("T-B", "attempt")
			}
			if w, _ := r.call(call.method, call.path, c.key, body); w.Code != c.want {
				t.Errorf("%s %s with %s: HTTP %d, want %d", call.method, call.path, c.name, w.Code, c.want)
			}
		}
	}
	if r.g.tasks.get("tauceti", "T-B") != nil || r.state("T-A") != TaskQueued {
		t.Fatal("a refused call changed the queue")
	}
	// The other project's queue is its own.
	w, out := r.call("GET", "/v1/projects/other/tasks", otherProjectKey, nil)
	if w.Code != 200 || len(out["tasks"].([]any)) != 0 {
		t.Fatalf("other project's list: HTTP %d %s", w.Code, w.Body)
	}
}

// Claim order: fix rounds and follow-ups, then first attempts, then other work; oldest first
// within a class; a task that came back from a lease goes first.
func TestTaskClaimOrder(t *testing.T) {
	r := newTaskRig(t, "")
	for _, c := range [][2]string{{"A1", "attempt"}, {"O1", "other"}, {"A2", "attempt"}, {"F1", "fix_round"}, {"U1", "followup"}, {"F2", "fix_round"}} {
		r.create(c[0], c[1])
		r.now = r.now.Add(time.Second)
	}
	// The list shows the queue in claim order.
	_, out := r.call("GET", tasksPath+"?state=queued", testProjectKey, nil)
	var listed []string
	for _, x := range out["tasks"].([]any) {
		listed = append(listed, x.(map[string]any)["id"].(string))
	}
	if got := strings.Join(listed, " "); got != "F1 U1 F2 A1 A2 O1" {
		t.Errorf("listed queue %q", got)
	}
	var got []string
	for {
		id, _ := r.claim("w1", 600)
		if id == "" {
			break
		}
		got = append(got, id)
	}
	if s := strings.Join(got, " "); s != "F1 U1 F2 A1 A2 O1" {
		t.Fatalf("claim order %q, want fix/follow-up, then attempts, then other, oldest first", s)
	}

	// A requeued first attempt goes before a newer fix round (as the runner resumes interrupted
	// work first); its last worker takes it (TestTaskAffinity: others wait affinityWindow).
	r2 := newTaskRig(t, "")
	r2.create("A1", "attempt")
	id, lease := r2.claim("w1", 60)
	r2.create("F9", "fix_round")
	if w := r2.lease(id, "fail", "w1", lease, map[string]any{"requeue": true, "reason": "spot stop"}); w.Code != 200 {
		t.Fatalf("requeue: HTTP %d %s", w.Code, w.Body)
	}
	if id, _ := r2.claim("w1", 60); id != "A1" {
		t.Fatalf("after a requeue claimed %q, want A1", id)
	}
	// kinds narrows a claim.
	r2.create("A3", "attempt")
	w, out := r2.call("POST", tasksPath+"/claim", testProjectKey, map[string]any{"worker": "w3", "kinds": []string{"attempt"}})
	if w.Code != 200 || out["task"].(map[string]any)["id"] != "A3" {
		t.Fatalf("claim kinds=attempt: HTTP %d %s", w.Code, w.Body)
	}
}

// A lease that is not renewed returns the task to the queue: the old lease can no longer
// heartbeat or complete, another worker gets the task, and after max_tries leases it fails.
func TestTaskLeaseExpiry(t *testing.T) {
	r := newTaskRig(t, "")
	r.create("T-A", "attempt")
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
	// The same worker claims it again: only the new lease counts.
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
	r.now = r.now.Add(time.Minute)
	// Tries 2 and 3 expire too: the task fails instead of going round for ever.
	r.now = r.now.Add(61 * time.Second)
	if id3, _ := r.claim("w1", 60); id3 != "T-A" { // the claim itself sweeps; held for w1
		t.Fatalf("third claim got %q", id3)
	}
	r.now = r.now.Add(61 * time.Second)
	r.g.tasks.sweepAll()
	task := r.g.tasks.get("tauceti", "T-A")
	if task.State != TaskFailed || task.Tries != 3 || !strings.Contains(task.Error, "lease expired") {
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
	r.create("T-A", "attempt")
	w, out := r.call("POST", tasksPath, testProjectKey, newTask("T-A", "attempt"))
	if w.Code != 200 || out["created"] != false {
		t.Fatalf("same task again: HTTP %d %s", w.Code, w.Body)
	}
	// Whitespace in the spec does not matter.
	raw := `{"id":"T-A","kind":"attempt","title":"Task T-A","created_by":"owner","base_ref":"a57def6",
		"spec": { "attempt" : "T-A", "target":"TauCeti.Foo", "notes":["x"] }}`
	if w, _ := r.call("POST", tasksPath, testProjectKey, raw); w.Code != 200 {
		t.Fatalf("same task, other whitespace: HTTP %d %s", w.Code, w.Body)
	}
	other := newTask("T-A", "attempt")
	other["spec"] = map[string]any{"attempt": "T-A", "target": "TauCeti.Bar"}
	if w, _ := r.call("POST", tasksPath, testProjectKey, other); w.Code != http.StatusConflict {
		t.Fatalf("other spec, same id: HTTP %d", w.Code)
	}
	if w, _ := r.call("POST", tasksPath, testProjectKey, newTask("T-A", "fix_round")); w.Code != http.StatusConflict {
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
		m := newTask("T-X", "attempt")
		mod(m)
		if w, _ := r.call("POST", tasksPath, testProjectKey, m); w.Code != http.StatusBadRequest {
			t.Errorf("create %s: HTTP %d %s", name, w.Code, w.Body)
		}
	}
	bad("unknown field", func(m map[string]any) { m["priority"] = 0 })
	bad("unknown kind", func(m map[string]any) { m["kind"] = "urgent" })
	bad("bad id", func(m map[string]any) { m["id"] = "../etc/passwd" })
	bad("no spec", func(m map[string]any) { delete(m, "spec") })
	bad("spec not an object", func(m map[string]any) { m["spec"] = []any{1} })
	bad("spec too big", func(m map[string]any) { m["spec"] = map[string]any{"x": strings.Repeat("a", maxTaskSpec)} })
	bad("secret in title", func(m map[string]any) { m["title"] = "ghp_" + strings.Repeat("x", 36) })
	bad("no created_by", func(m map[string]any) { delete(m, "created_by") })
	bad("bad base_ref", func(m map[string]any) { m["base_ref"] = "a b" })
	bad("max_tries 11", func(m map[string]any) { m["max_tries"] = 11 })
	req := httptest.NewRequest("POST", tasksPath, bytes.NewReader(statusJSON(newTask("T-X", "attempt"))))
	req.Header.Set("Authorization", "Bearer "+testProjectKey)
	w := httptest.NewRecorder()
	r.g.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusUnsupportedMediaType {
		t.Errorf("no content type: HTTP %d", w.Code)
	}
	if r.g.tasks.get("tauceti", "T-X") != nil {
		t.Fatal("a refused task was stored")
	}

	r.create("T-A", "attempt")
	id, lease := r.claim("w1", 60)
	if w, _ := r.call("POST", tasksPath+"/claim", testProjectKey, map[string]any{"worker": "w1", "lease_seconds": 5}); w.Code != 400 {
		t.Errorf("lease_seconds 5: HTTP %d", w.Code)
	}
	big := map[string]any{}
	for k, v := range okResult {
		big[k] = v
	}
	big["diff"] = strings.Repeat("+", maxTaskDiff+1)
	if w := r.lease(id, "complete", "w1", lease, map[string]any{"result": big}); w.Code != http.StatusBadRequest {
		t.Errorf("diff over 1 MB: HTTP %d", w.Code)
	}
	big["diff"] = strings.Repeat("\"", maxTaskCompleteBody/2+1) // escaped, the body is over 3 MB
	if w := r.lease(id, "complete", "w1", lease, map[string]any{"result": big}); w.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("body over 3 MB: HTTP %d", w.Code)
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
	// A diff of exactly 1 MB is fine.
	big["diff"] = strings.Repeat("+", maxTaskDiff)
	if w := r.lease(id, "complete", "w1", lease, map[string]any{"result": big}); w.Code != 200 {
		t.Fatalf("1 MB diff: HTTP %d %s", w.Code, w.Body)
	}
}

// complete, fail (with and without requeue) and cancel move a task through its states; a
// repeated report by the same lease is accepted; a finished task stays finished.
func TestTaskStateMachine(t *testing.T) {
	r := newTaskRig(t, "")
	r.create("T-A", "attempt")
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
	full := r.g.tasks.get("tauceti", "T-A")
	if full.State != TaskDone || full.Result.Diff != "--- a\n+++ b\n" || full.Result.FinishedAt.IsZero() || full.Lease.ID != "" {
		t.Fatalf("done task: %+v", full)
	}

	// fail without requeue: failed; with requeue: back in the queue until max_tries.
	r.create("T-B", "attempt")
	id, lease = r.claim("w1", 60)
	if w := r.lease(id, "fail", "w1", lease, map[string]any{"reason": "lake_build infra error"}); w.Code != 200 || r.state("T-B") != TaskFailed {
		t.Fatalf("fail: HTTP %d, %s", w.Code, r.state("T-B"))
	}
	if w := r.lease(id, "fail", "w1", lease, map[string]any{"reason": "lake_build infra error"}); w.Code != 200 {
		t.Errorf("the same fail again: HTTP %d", w.Code)
	}
	m := newTask("T-C", "attempt")
	m["max_tries"] = 2
	r.call("POST", tasksPath, testProjectKey, m)
	for try := 1; try <= 2; try++ {
		id, lease = r.claim("w1", 60)
		r.lease(id, "fail", "w1", lease, map[string]any{"requeue": true, "reason": "spot stop"})
	}
	if c := r.g.tasks.get("tauceti", "T-C"); c.State != TaskFailed || c.Tries != 2 {
		t.Fatalf("requeue after max_tries: %s, %d tries", c.State, c.Tries)
	}

	// cancel: a queued task, and a leased one whose worker then loses its lease.
	r.create("T-D", "attempt")
	r.create("T-E", "attempt")
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

// The list carries summaries (no spec, diff or PR text) and counts; one task in full has its
// spec and result but never the lease id.
func TestTaskListAndGet(t *testing.T) {
	r := newTaskRig(t, "")
	r.create("T-A", "attempt")
	r.create("T-F", "fix_round")
	id, lease := r.claim("w1", 60)
	r.lease(id, "complete", "w1", lease, map[string]any{"result": okResult})
	w, out := r.call("GET", tasksPath+"?state=done,queued", testProjectKey, nil)
	if w.Code != 200 || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("list: HTTP %d", w.Code)
	}
	body := w.Body.String()
	if strings.Contains(body, "TauCeti.Foo") || strings.Contains(body, "+++ b") || strings.Contains(body, "Adds Foo") || strings.Contains(body, lease) {
		t.Errorf("the list carries a spec, diff, PR body or lease id: %s", body)
	}
	if c := out["counts"].(map[string]any); c["done"] != 1.0 || c["queued"] != 1.0 {
		t.Errorf("counts %v", c)
	}
	if w, _ := r.call("GET", tasksPath+"?kind=fix_round", testProjectKey, nil); strings.Contains(w.Body.String(), `"id":"T-A"`) {
		t.Errorf("kind filter: %s", w.Body)
	}
	if w, _ := r.call("GET", tasksPath+"?state=running", testProjectKey, nil); w.Code != 400 {
		t.Errorf("unknown state filter: HTTP %d", w.Code)
	}
	w, _ = r.call("GET", tasksPath+"/"+id, testProjectKey, nil)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "TauCeti.Foo") || !strings.Contains(w.Body.String(), "+++ b") || strings.Contains(w.Body.String(), lease) {
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
	r.create("T-A", "attempt")
	r.create("T-B", "attempt")
	r.create("T-C", "fix_round")
	idC, leaseC := r.claim("w1", 600)
	idA, leaseA := r.claim("w1", 600)
	r.lease(idA, "complete", "w1", leaseA, map[string]any{"result": okResult})
	r.g.Close()

	r2 := newTaskRig(t, dir)
	r2.now = r.now.Add(time.Minute)
	if r2.state("T-A") != TaskDone || r2.state("T-B") != TaskQueued || r2.state("T-C") != TaskLeased {
		t.Fatalf("after restart: A %s, B %s, C %s", r2.state("T-A"), r2.state("T-B"), r2.state("T-C"))
	}
	if d := r2.g.tasks.get("tauceti", "T-A"); d.Result == nil || d.Result.Diff != "--- a\n+++ b\n" {
		t.Fatal("the result was lost")
	}
	if w := r2.lease(idC, "heartbeat", "w1", leaseC, nil); w.Code != 200 {
		t.Fatalf("the lease did not survive: HTTP %d %s", w.Code, w.Body)
	}
	r2.create("T-D", "attempt")
	if d, b := r2.g.tasks.get("tauceti", "T-D"), r2.g.tasks.get("tauceti", "T-B"); d.Seq <= b.Seq {
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
		r.create(fmt.Sprintf("T-%d", i), "attempt")
	}
	if w, _ := r.call("POST", tasksPath, testProjectKey, newTask("T-over", "attempt")); w.Code != http.StatusTooManyRequests {
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
	if w, _ := r.call("POST", tasksPath, testProjectKey, newTask("T-new", "attempt")); w.Code != http.StatusTooManyRequests || !strings.Contains(w.Body.String(), "export") {
		t.Fatalf("create with %d finished tasks waiting for export: HTTP %d %s", maxKeptTasks+10, w.Code, w.Body)
	}
	// Export the oldest 30: the 10 oldest are dropped (210 finished, 200 kept), the newer stay.
	r.export(t, 30)
	_, out = r.call("GET", tasksPath+"?state=cancelled", testProjectKey, nil)
	if n := len(out["tasks"].([]any)); n != maxKeptTasks {
		t.Fatalf("after the export %d cancelled tasks kept, want %d", n, maxKeptTasks)
	}
	if r.g.tasks.get("tauceti", "T-9") != nil || r.g.tasks.get("tauceti", "T-10") == nil || r.g.tasks.get("tauceti", fmt.Sprintf("T-%d", maxKeptTasks+9)) == nil {
		t.Fatal("pruning dropped the wrong tasks: the oldest exported go first")
	}
	// 190 wait for export now: create works again.
	if w, _ := r.call("POST", tasksPath, testProjectKey, newTask("T-new", "attempt")); w.Code != http.StatusCreated {
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
	r.create("A1", "attempt")
	r.create("A2", "attempt")
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
	if got := r.g.tasks.get("tauceti", "A1"); got.PreferWorker != "w1" || got.PreferUntil == nil {
		t.Fatalf("held: %+v", got)
	}
	// After the window, anyone.
	r.now = r.now.Add(affinityWindow)
	if id, _ := r.claim("w2", 60); id != "A1" {
		t.Fatalf("after the window w2 claimed %q, want A1", id)
	}
	if got := r.g.tasks.get("tauceti", "A1"); got.PreferWorker != "" || got.PreferUntil != nil {
		t.Fatalf("a claim keeps the hold: %+v", got)
	}

	// An expired lease holds the task for its worker too, and the worker takes it back by id
	// although another task is first in the claim order.
	r2 := newTaskRig(t, "")
	r2.create("A1", "attempt")
	id, _ = r2.claim("w1", 60)
	r2.now = r2.now.Add(61 * time.Second)
	r2.create("F1", "fix_round")
	if id, _ := r2.claim("w2", 60); id != "F1" {
		t.Fatalf("w2 claimed %q, want F1 (A1 is held for w1)", id)
	}
	w, out := r2.call("POST", tasksPath+"/claim", testProjectKey, map[string]any{"worker": "w1", "task": "A1"})
	if w.Code != 200 || out["task"].(map[string]any)["id"] != "A1" || out["task"].(map[string]any)["tries"] != 2.0 {
		t.Fatalf("w1 claims A1 back: HTTP %d %s", w.Code, w.Body)
	}
	for _, c := range []struct {
		body map[string]any
		want int
	}{
		{map[string]any{"worker": "w1", "task": "A1"}, http.StatusConflict},               // leased now
		{map[string]any{"worker": "w1", "task": "nope"}, http.StatusNotFound},             // no such task
		{map[string]any{"worker": "w1", "task": "A1", "kinds": []string{"attempt"}}, 400}, // one or the other
		{map[string]any{"worker": "w1", "task": "../x"}, 400},                             // not an id
	} {
		if w, _ := r2.call("POST", tasksPath+"/claim", testProjectKey, c.body); w.Code != c.want {
			t.Errorf("claim %v: HTTP %d, want %d", c.body, w.Code, c.want)
		}
	}
	if w := r2.lease("A1", "heartbeat", "w1", out["lease"].(map[string]any)["id"].(string), map[string]any{"task": "A1"}); w.Code != 400 {
		t.Errorf("heartbeat with task: HTTP %d", w.Code)
	}
}

// The internal review's verdict and the owner's decision are stored on a done task only, with
// the project key; they show in the task and (without the detail) in the list, and a task
// changed by them needs another export.
func TestTaskReviewDecisionExport(t *testing.T) {
	r := newTaskRig(t, "")
	r.create("T-A", "attempt")
	review := map[string]any{"verdict": "red", "by": "owner-codex", "round": 1, "findings": 3, "summary": "3 findings",
		"detail": map[string]any{"findings": []any{"rename", "docstring", "simp"}}}
	if w, _ := r.call("POST", tasksPath+"/T-A/review", testProjectKey, review); w.Code != http.StatusConflict {
		t.Fatalf("review of a queued task: HTTP %d", w.Code)
	}
	id, lease := r.claim("w1", 60)
	if w, _ := r.call("POST", tasksPath+"/T-A/exported", testProjectKey, map[string]any{"seen": r.now}); w.Code != http.StatusConflict {
		t.Fatalf("exported while leased: HTTP %d", w.Code)
	}
	r.lease(id, "complete", "w1", lease, map[string]any{"result": okResult})
	for name, mod := range map[string]func(m map[string]any){
		"verdict":       func(m map[string]any) { m["verdict"] = "amber" },
		"no reviewer":   func(m map[string]any) { delete(m, "by") },
		"at":            func(m map[string]any) { m["at"] = "2026-10-27T12:00:00Z" },
		"unknown field": func(m map[string]any) { m["score"] = 1 },
		"secret":        func(m map[string]any) { m["summary"] = "ghp_" + strings.Repeat("x", 36) },
		"detail array":  func(m map[string]any) { m["detail"] = []any{1} },
		"detail big":    func(m map[string]any) { m["detail"] = map[string]any{"x": strings.Repeat("a", maxTaskReviewDetail)} },
	} {
		m := map[string]any{}
		for k, v := range review {
			m[k] = v
		}
		mod(m)
		if w, _ := r.call("POST", tasksPath+"/T-A/review", testProjectKey, m); w.Code != 400 {
			t.Errorf("review with bad %s: HTTP %d %s", name, w.Code, w.Body)
		}
	}
	// The export takes the done task; then a review changes it, so it waits for export again.
	r.export(t, 1)
	if got := r.g.tasks.get("tauceti", "T-A"); got.ExportedAt == nil {
		t.Fatal("not marked exported")
	}
	before := r.g.tasks.get("tauceti", "T-A").UpdatedAt
	r.now = r.now.Add(time.Minute)
	w, out := r.call("POST", tasksPath+"/T-A/review", testProjectKey, review)
	if w.Code != 200 || out["review"].(map[string]any)["verdict"] != "red" || out["review"].(map[string]any)["at"] == nil {
		t.Fatalf("review: HTTP %d %s", w.Code, w.Body)
	}
	got := r.g.tasks.get("tauceti", "T-A")
	if got.ExportedAt != nil || got.Review == nil || *got.Review.Findings != 3 || !strings.Contains(string(got.Review.Detail), "docstring") {
		t.Fatalf("after the review: exported %v, review %+v", got.ExportedAt, got.Review)
	}
	// The export of the old state is refused (it changed since); the list shows it as not exported.
	if w, _ := r.call("POST", tasksPath+"/T-A/exported", testProjectKey, map[string]any{"seen": before}); w.Code != http.StatusConflict {
		t.Fatalf("exported with a stale updated_at: HTTP %d", w.Code)
	}
	w, _ = r.call("GET", tasksPath+"?exported=false", testProjectKey, nil)
	if !strings.Contains(w.Body.String(), `"verdict":"red"`) || strings.Contains(w.Body.String(), "docstring") {
		t.Fatalf("list: the review without its detail: %s", w.Body)
	}
	if w, _ := r.call("GET", tasksPath+"?exported=maybe", testProjectKey, nil); w.Code != 400 {
		t.Errorf("exported=maybe: HTTP %d", w.Code)
	}
	// A later green review replaces it.
	review["verdict"], review["round"], review["findings"] = "green", 2, 0
	if w, _ := r.call("POST", tasksPath+"/T-A/review", testProjectKey, review); w.Code != 200 {
		t.Fatalf("second review: HTTP %d", w.Code)
	}
	if got := r.g.tasks.get("tauceti", "T-A"); got.Review.Verdict != "green" || got.Review.Round != 2 {
		t.Fatalf("second review: %+v", got.Review)
	}

	// The decision: opened needs the PR, the others none.
	for _, c := range []struct {
		body map[string]any
		want int
	}{
		{map[string]any{"decision": "opened", "by": "owner"}, 400},
		{map[string]any{"decision": "approved", "by": "owner", "pr": 7}, 400},
		{map[string]any{"decision": "merged", "by": "owner"}, 400},
		{map[string]any{"decision": "approved"}, 400},
		{map[string]any{"decision": "approved", "by": "owner"}, 200},
		{map[string]any{"decision": "opened", "by": "owner", "pr": 22500}, 200},
	} {
		if w, _ := r.call("POST", tasksPath+"/T-A/decide", testProjectKey, c.body); w.Code != c.want {
			t.Errorf("decide %v: HTTP %d, want %d %s", c.body, w.Code, c.want, w.Body)
		}
	}
	if got := r.g.tasks.get("tauceti", "T-A"); got.Decision == nil || got.Decision.Decision != "opened" || got.Decision.PR != 22500 {
		t.Fatalf("decision: %+v", got.Decision)
	}
	if w, _ := r.call("POST", tasksPath+"/nope/decide", testProjectKey, map[string]any{"decision": "approved", "by": "owner"}); w.Code != 404 {
		t.Errorf("decide unknown: HTTP %d", w.Code)
	}
	// Persisted: the review and decision survive a restart (TestTaskPersistence covers files).
	if w, _ := r.call("POST", tasksPath+"/T-A/exported", testProjectKey, map[string]any{}); w.Code != 400 {
		t.Errorf("exported without seen: HTTP %d", w.Code)
	}
}
