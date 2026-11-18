// The task schema of decision 0019: kinds per project, typed fields, closed_by, compatibility with
// tasks and clients from before, and the removed review loop.
//
// SPDX-License-Identifier: Apache-2.0
package gateway

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// claimID claims one task by id and returns its lease.
func (r *taskRig) claimID(worker, id string) string {
	r.t.Helper()
	code, lease := r.takeBack(worker, id)
	if code != http.StatusOK {
		r.t.Fatalf("claim %s: HTTP %d", id, code)
	}
	return lease
}

// createWith makes a task with extra fields and returns the HTTP status.
func (r *taskRig) createWith(id, kind string, extra map[string]any) (int, string) {
	r.t.Helper()
	n := newTask(id, kind)
	for k, v := range extra {
		n[k] = v
	}
	w, _ := r.call("POST", tasksPath, testProjectKey, n)
	return w.Code, w.Body.String()
}

// The claim order without classes: tasks that came back first, then the client's order, then age;
// the list shows the queue in that order.
func TestTaskClaimOrder(t *testing.T) {
	r := newTaskRig(t, "")
	r.createWith("N1", "p2m-v7", map[string]any{"order": 2})
	r.createWith("K1", "p2m-v7", map[string]any{"order": 1})
	r.createWith("C1", "p2m-v7", map[string]any{"order": -1})
	r.create("O1", "other")
	r.createWith("K2", "other", map[string]any{"order": 1})
	if got, want := r.listed("?state=queued"), "C1 O1 K1 K2 N1"; got != want {
		t.Fatalf("queue %q, want %q", got, want)
	}
	id, lease := r.claim("w1", 60)
	if id != "C1" {
		t.Fatalf("claimed %q, want C1", id)
	}
	r.lease(id, "fail", "w1", lease, map[string]any{"requeue": true, "reason": "spot stop"})
	r.createWith("C0", "p2m-v7", map[string]any{"order": -5})
	if got, want := r.listed("?state=queued"), "C1 C0 O1 K1 K2 N1"; got != want { // a task that came back goes first
		t.Fatalf("after a requeue: queue %q, want %q", got, want)
	}
}

// Kinds are the project's: "other" and the kinds its configuration declares; a worker claims only
// the kinds it names. Another project's kinds are not this one's.
func TestTaskKinds(t *testing.T) {
	r := newTaskRig(t, "")
	r.create("V7", "p2m-v7")
	r.create("OT", "other")
	if code, body := r.createWith("V8", "p2m-v8", nil); code != 400 || !strings.Contains(body, "not one of this project's kinds") {
		t.Fatalf("an undeclared kind: HTTP %d %s", code, body)
	}
	for _, k := range []string{"attempt", "fix_round", "review", "followup"} { // TauCeti's, no longer built in
		if code, _ := r.createWith("X-"+k, k, nil); code != 400 {
			t.Errorf("kind %s: HTTP %d, want 400", k, code)
		}
	}
	if w, _ := r.call("POST", "/v1/projects/other/tasks", otherProjectKey, newTask("V7", "p2m-v7")); w.Code != 400 {
		t.Errorf("a kind another project declares: HTTP %d", w.Code)
	}
	claimKinds := func(kinds ...string) (int, string) {
		w, out := r.call("POST", tasksPath+"/claim", testProjectKey, map[string]any{"worker": "w1", "kinds": kinds})
		if w.Code != 200 {
			return w.Code, ""
		}
		return w.Code, out["task"].(map[string]any)["id"].(string)
	}
	if code, _ := claimKinds("p2m-v6"); code != http.StatusNoContent { // a worker of another generation gets nothing
		t.Fatalf("claim p2m-v6: HTTP %d", code)
	}
	if _, id := claimKinds("other"); id != "OT" {
		t.Fatalf("claim other: %q", id)
	}
	if _, id := claimKinds("p2m-v7", "other"); id != "V7" {
		t.Fatalf("claim p2m-v7: %q", id)
	}
	for name, kinds := range map[string][]string{"malformed": {"P2M v7"}, "too many": slices.Repeat([]string{"a"}, maxTaskClaimKinds+1)} {
		if code, _ := claimKinds(kinds...); code != 400 {
			t.Errorf("claim %s kinds: HTTP %d", name, code)
		}
	}
	if w, _ := r.call("GET", tasksPath+"?kind=Bad+Kind", testProjectKey, nil); w.Code != 400 {
		t.Errorf("list with a malformed kind: HTTP %d", w.Code)
	}
}

// A project's kinds, page and anchor are checked with its config.
func TestProjectKindsAndPageChecked(t *testing.T) {
	h := keyHash(testProjectKey)
	for name, p := range map[string]Project{
		"malformed kind": {Kinds: []string{"P2M"}},
		"kind twice":     {Kinds: []string{"p2m-v7", "p2m-v7"}},
		"other declared": {Kinds: []string{"other"}},
		"too many kinds": {Kinds: func() []string {
			var ks []string
			for i := range maxProjectKinds + 1 {
				ks = append(ks, fmt.Sprintf("k%d", i))
			}
			return ks
		}()},
		"page not a path":     {Page: "https://example.org/x"},
		"page with //":        {Page: "/science//x"},
		"anchor without page": {PageAnchor: "leaf"},
		"bad anchor":          {Page: "/science/lab/", PageAnchor: "Leaf"},
	} {
		p.ID, p.KeySHA256 = "lab", []string{h}
		if _, err := newProjectTable([]Project{p}); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	ok := Project{ID: "lab", KeySHA256: []string{h}, Kinds: []string{"p2m-v7", "p2m_v8"}, Page: "/science/lab/", PageAnchor: "leaf"}
	if _, err := newProjectTable([]Project{ok}); err != nil {
		t.Fatal(err)
	}
}

// The typed fields are stored at creation as sent, shown with the task (and in the list), take
// part in the create's idempotence, and are checked: ids, sizes, public-safe labels.
func TestTaskTypedFields(t *testing.T) {
	r := newTaskRig(t, "")
	typed := map[string]any{"role": "hole", "parent": "t6-p2m-a146ce-s2", "needs": []string{"t6-p2m-a146ce-s2-h01", "t6-p2m-a146ce-s2-h02"},
		"replaces": "t6-p2m-a146ce-s1", "hole_id": "h-3f9c0a1b2c3d",
		"labels": map[string]string{"trial": "t6", "leaf": "p2m-a146ce", "alias": "C11", "outline": "2", "hole": "3",
			"theorem": "CodedSorting.proposition_7_1_transpose_with_a_long_lean_name₂"}}
	if code, body := r.createWith("t6-p2m-a146ce-s2-h03", "p2m-v7", typed); code != http.StatusCreated {
		t.Fatalf("create: HTTP %d %s", code, body)
	}
	got := r.g.tasks.get("lab", "t6-p2m-a146ce-s2-h03")
	if got.Role != "hole" || got.Parent != "t6-p2m-a146ce-s2" || !slices.Equal(got.Needs, []string{"t6-p2m-a146ce-s2-h01", "t6-p2m-a146ce-s2-h02"}) ||
		got.Replaces != "t6-p2m-a146ce-s1" || got.HoleID != "h-3f9c0a1b2c3d" || got.Labels["alias"] != "C11" || got.Try != 1 || got.RetryOf != "" {
		t.Fatalf("stored %+v", got)
	}
	w, _ := r.call("GET", tasksPath+"?role=hole", testProjectKey, nil)
	for _, want := range []string{`"role":"hole"`, `"hole_id":"h-3f9c0a1b2c3d"`, `"needs":["t6-p2m-a146ce-s2-h01"`, `"alias":"C11"`} {
		if !strings.Contains(w.Body.String(), want) {
			t.Errorf("the list lacks %s: %s", want, w.Body)
		}
	}
	if got := r.listed("?role=outline"); got != "" {
		t.Errorf("role=outline lists %q", got)
	}
	// idempotence: the same again is the same task; another value of any typed field is another task
	if code, body := r.createWith("t6-p2m-a146ce-s2-h03", "p2m-v7", typed); code != 200 {
		t.Fatalf("the same create again: HTTP %d %s", code, body)
	}
	for field, v := range map[string]any{"role": "rest", "parent": "t6-x-s2", "needs": []string{"t6-p2m-a146ce-s2-h01"}, "replaces": "t6-x-s1",
		"hole_id": "h-000000000000", "retry_of": "t6-x-h03", "labels": map[string]string{"trial": "t6"}} {
		m := map[string]any{}
		for k, x := range typed {
			m[k] = x
		}
		m[field] = v
		if code, _ := r.createWith("t6-p2m-a146ce-s2-h03", "p2m-v7", m); code != http.StatusConflict {
			t.Errorf("another %s under the same id: HTTP %d, want 409", field, code)
		}
	}
	for name, extra := range map[string]map[string]any{
		"bad role":      {"role": "Hole"},
		"bad parent":    {"parent": "../x"},
		"self parent":   {"parent": "B1"},
		"self retry_of": {"retry_of": "B1"},
		"self in needs": {"needs": []string{"B1"}},
		"needs twice":   {"needs": []string{"A", "A"}},
		"bad needs id":  {"needs": []string{"a b"}},
		"too many needs": {"needs": func() []string {
			var n []string
			for i := range maxTaskNeeds + 1 {
				n = append(n, fmt.Sprintf("n%d", i))
			}
			return n
		}()},
		"bad hole_id":       {"hole_id": "H 1"},
		"bad label name":    {"labels": map[string]string{"Trial": "t6"}},
		"empty label":       {"labels": map[string]string{"trial": ""}},
		"long label":        {"labels": map[string]string{"trial": strings.Repeat("x", maxTaskLabelValue+1)}},
		"control in label":  {"labels": map[string]string{"trial": "t6\n"}},
		"key in a label":    {"labels": map[string]string{"note": "ak-" + strings.Repeat("a", 20)}},
		"node token label":  {"labels": map[string]string{"note": fakeNodeToken}},
		"e-mail in a label": {"labels": map[string]string{"by": "someone@example.org"}},
		"too many labels": {"labels": func() map[string]string {
			m := map[string]string{}
			for i := range maxTaskLabels + 1 {
				m["l"+string(rune('a'+i))] = "x"
			}
			return m
		}()},
		"closed_by at create": {"closed_by": "x"},
	} {
		if code, body := r.createWith("B1", "p2m-v7", extra); code != 400 {
			t.Errorf("%s: HTTP %d %s", name, code, body)
		}
	}
	if r.g.tasks.get("lab", "B1") != nil {
		t.Fatal("a refused task was stored")
	}
}

// try: 1 for a first try, one more than the try a retry follows, 2 when that task is gone.
func TestTaskTry(t *testing.T) {
	r := newTaskRig(t, "")
	r.create("H", "p2m-v7")
	r.createWith("H-a2", "p2m-v7", map[string]any{"retry_of": "H"})
	r.createWith("H-a3", "p2m-v7", map[string]any{"retry_of": "H-a2"})
	r.createWith("G-a2", "p2m-v7", map[string]any{"retry_of": "G"}) // G was exported and dropped
	for id, want := range map[string]int{"H": 1, "H-a2": 2, "H-a3": 3, "G-a2": 2} {
		if got := r.g.tasks.get("lab", id); got.Try != want {
			t.Errorf("%s: try %d, want %d", id, got.Try, want)
		}
	}
}

// closed_by comes with a complete or a cancel, when something else closed the task's work; it is
// checked and kept, and belongs to no other call.
func TestTaskClosedBy(t *testing.T) {
	r := newTaskRig(t, "")
	r.create("A", "p2m-v7")
	lease := r.claimID("w1", "A")
	for _, bad := range []string{"a b", "ghp_" + strings.Repeat("x", 36), strings.Repeat("x", 201)} {
		if w := r.lease("A", "complete", "w1", lease, map[string]any{"result": map[string]any{"outcome": "closed"}, "closed_by": bad}); w.Code != 400 {
			t.Errorf("closed_by %q: HTTP %d", bad, w.Code)
		}
	}
	for _, op := range []string{"heartbeat", "fail"} {
		if w := r.lease("A", op, "w1", lease, map[string]any{"closed_by": "B"}); w.Code != 400 {
			t.Errorf("%s with closed_by: HTTP %d", op, w.Code)
		}
	}
	if w, _ := r.call("POST", tasksPath+"/claim", testProjectKey, map[string]any{"worker": "w1", "closed_by": "B"}); w.Code != 400 {
		t.Errorf("claim with closed_by: HTTP %d", w.Code)
	}
	if w := r.lease("A", "complete", "w1", lease, map[string]any{"result": map[string]any{"outcome": "closed"}, "closed_by": "reuse:formalpedia:Nat.foo_bar₁"}); w.Code != 200 {
		t.Fatalf("complete closed: HTTP %d %s", w.Code, w.Body)
	}
	if got := r.g.tasks.get("lab", "A"); got.ClosedBy != "reuse:formalpedia:Nat.foo_bar₁" || got.State != TaskDone || got.Result.Outcome != "closed" {
		t.Fatalf("done: %+v", got)
	}
	r.create("B", "p2m-v7")
	if w, _ := r.call("POST", tasksPath+"/B/cancel", testProjectKey, map[string]any{"reason": "its hole is closed", "closed_by": "A"}); w.Code != 200 {
		t.Fatalf("cancel closed: HTTP %d %s", w.Code, w.Body)
	}
	if got := r.g.tasks.get("lab", "B"); got.ClosedBy != "A" || got.State != TaskCancelled {
		t.Fatalf("cancelled: %+v", got)
	}
}

// Older clients keep working: a create with no typed field, a claim with max_class (ignored now), and
// a claim without kinds takes any kind.
func TestTaskOlderClients(t *testing.T) {
	r := newTaskRig(t, "")
	r.create("A", "other")
	r.create("B", "p2m-v7")
	w, out := r.call("POST", tasksPath+"/claim", testProjectKey, map[string]any{"worker": "w1", "max_class": 4})
	if w.Code != 200 || out["task"].(map[string]any)["id"] != "A" {
		t.Fatalf("claim with max_class 4 (B and A were classes 5 and 6 once): HTTP %d %s", w.Code, w.Body)
	}
	if w, _ := r.call("POST", tasksPath+"/claim", testProjectKey, map[string]any{"worker": "w1", "max_class": legacyMaxClass + 1}); w.Code != 400 {
		t.Errorf("max_class out of the old range: HTTP %d", w.Code)
	}
	if id, _ := r.claim("w2", 60); id != "B" {
		t.Fatalf("a claim without kinds: %q", id)
	}
	if got := r.g.tasks.get("lab", "A"); got.Role != "" || got.Labels != nil || got.Needs != nil || got.Try != 1 {
		t.Fatalf("an untyped task: %+v", got)
	}
}

// A task file written before decision 0019 (TauCeti's review loop: class, chain, stage, findings,
// reviews, a debt) loads, can be listed, claimed by its kind, finished and exported, and is written
// back without the removed fields; a chain record left on disk is never read.
func TestTaskFileFromBeforeLoads(t *testing.T) {
	dir := t.TempDir()
	r := newTaskRig(t, dir)
	r.create("NEW", "other")
	r.g.Close()
	old := `{"id":"T5-NS-3.a-A","project":"lab","kind":"attempt","class":5,"findings_source":"prereview","chain":"T5-NS-3.a",
"stage":"ready-for-operator","round_kind":"","title":"An old attempt","spec":{"attempt":"T5-NS-3.a-A"},"base_ref":"a57def6","pr":12,
"created_by":"owner","state":"queued","tries":0,"max_tries":3,"seq":7,"created_at":"2026-10-30T10:00:00Z","updated_at":"2026-10-30T10:00:00Z",
"review":{"verdict":"red","by":"owner-codex","at":"2026-10-30T11:00:00Z"},"qwen_review":{"verdict":"green","by":"q","judge":"qwen","at":"2026-10-30T11:00:00Z"},
"decision":{"decision":"approved","by":"owner","at":"2026-10-30T12:00:00Z"},"loop_pending":{"step":"stop","why":"x"},"next_fix":{"id":"P1","kind":"fix_round","spec":{}}}`
	if err := os.WriteFile(taskFile(dir, "T5-NS-3.a-A"), []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}
	chains := filepath.Join(dir, "task-chains-lab")
	if err := os.MkdirAll(chains, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(chains, "T5-NS-3.a.json"), []byte(`{not json`), 0o600); err != nil {
		t.Fatal(err)
	}
	r = newTaskRig(t, dir)
	if got := r.listed(""); !strings.Contains(got, "T5-NS-3.a-A") || !strings.Contains(got, "NEW") {
		t.Fatalf("list %q", got)
	}
	w, out := r.call("POST", tasksPath+"/claim", testProjectKey, map[string]any{"worker": "w1", "kinds": []string{"attempt"}})
	if w.Code != 200 || out["task"].(map[string]any)["id"] != "T5-NS-3.a-A" {
		t.Fatalf("claim the old kind: HTTP %d %s", w.Code, w.Body)
	}
	lease := out["lease"].(map[string]any)["id"].(string)
	if w := r.lease("T5-NS-3.a-A", "complete", "w1", lease, map[string]any{"result": okResult}); w.Code != 200 {
		t.Fatalf("complete: HTTP %d %s", w.Code, w.Body)
	}
	r.export(t, 1)
	raw, err := os.ReadFile(taskFile(dir, "T5-NS-3.a-A"))
	if err != nil {
		t.Fatal(err)
	}
	for _, gone := range []string{"loop_pending", "next_fix", "qwen_review", "findings_source", `"chain"`, `"stage"`, `"class"`, "base_ref", `"decision"`, `"review"`} {
		if strings.Contains(string(raw), gone) {
			t.Errorf("written back with %s: %s", gone, raw)
		}
	}
	var back Task
	if err := json.Unmarshal(raw, &back); err != nil || back.State != TaskDone || back.ExportedAt == nil || back.Kind != "attempt" {
		t.Fatalf("written back: %v %+v", err, back)
	}
}

// The review loop is gone: its endpoints, and the science results endpoint, answer 404 to the
// project's own key; nothing is created or changed.
func TestTaskLoopEndpointsGone(t *testing.T) {
	r := newTaskRig(t, "")
	r.create("A", "p2m-v7")
	lease := r.claimID("w1", "A")
	r.lease("A", "complete", "w1", lease, map[string]any{"result": okResult})
	for _, c := range []struct {
		method, path string
		body         any
	}{
		{"POST", tasksPath + "/A/review", map[string]any{"verdict": "green", "by": "owner-codex"}},
		{"POST", tasksPath + "/A/decide", map[string]any{"decision": "approved", "by": "owner"}},
		{"POST", tasksPath + "/A/continue", map[string]any{"created_by": "owner"}},
		{"POST", "/v1/projects/lab/science", map[string]any{"project": "Lab"}},
		{"GET", "/v1/projects/lab/science", nil},
	} {
		if w, _ := r.call(c.method, c.path, testProjectKey, c.body); w.Code != http.StatusNotFound && w.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s %s: HTTP %d, want 404", c.method, c.path, w.Code)
		}
	}
	if got := r.listed(""); got != "A" {
		t.Fatalf("tasks %q: the loop made something", got)
	}
}

// node_classes: a running task's requests note their node's class in memory; the next save keeps it.
func TestTaskNodeClasses(t *testing.T) {
	dir := t.TempDir()
	r := newTaskRig(t, dir)
	r.create("A", "p2m-v7")
	r.create("Q", "p2m-v7")
	lease := r.claimID("w1", "A")
	s := r.g.tasks
	s.noteNode("lab", "A", nodeClassVolunteer)
	s.noteNode("lab", "A", nodeClassTrusted)
	s.noteNode("lab", "A", nodeClassTrusted)
	s.noteNode("lab", "Q", nodeClassTrusted) // queued: not running
	s.noteNode("lab", "nope", nodeClassTrusted)
	s.noteNode("nosuch", "A", nodeClassTrusted)
	if got := s.get("lab", "A").NodeClasses; !slices.Equal(got, []string{"trusted", "volunteer"}) {
		t.Fatalf("node classes %v", got)
	}
	if got := s.get("lab", "Q").NodeClasses; got != nil {
		t.Fatalf("a queued task got %v", got)
	}
	r.lease("A", "heartbeat", "w1", lease, nil)
	if raw, _ := os.ReadFile(taskFile(dir, "A")); !strings.Contains(string(raw), `"node_classes":["trusted","volunteer"]`) {
		t.Fatalf("not saved with the heartbeat: %s", raw)
	}
}
