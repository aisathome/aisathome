// SPDX-License-Identifier: Apache-2.0
package gateway

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// retentionRig: prove2me (public, kinds p2m-v7) with task_compact_days days (0: the default), in dir.
func retentionRig(t *testing.T, dir string, days int) *taskRig {
	t.Helper()
	g := newTestGateway(t, Config{StateDir: dir, Projects: []Project{
		{ID: "prove2me", KeySHA256: []string{keyHash(testProjectKey)}, PublicQueue: true, Kinds: []string{"p2m-v7"}, TaskCompactDays: days},
	}})
	r := &taskRig{t: t, g: g, now: time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)}
	g.tasks.now = r.clock
	t.Cleanup(func() { g.Close() })
	return r
}

// publicViews are the tasks of GET /v1/tasks and of the public queue, built afresh.
func (r *taskRig) publicViews() (string, string) {
	r.t.Helper()
	r.freshV1()
	r.g.pubq.mu.Lock()
	r.g.pubq.byID = nil
	r.g.pubq.mu.Unlock()
	code, body, page := r.v1("?limit=500")
	if code != 200 {
		r.t.Fatalf("/v1/tasks: HTTP %d %s", code, body)
	}
	code, body, q := r.publicQueue()
	if code != 200 {
		r.t.Fatalf("queue: HTTP %d %s", code, body)
	}
	a, _ := json.Marshal(page.Tasks)
	b, _ := json.Marshal(q["tasks"])
	return string(a), string(b)
}

var (
	// typedNew: a task with the typed fields and a private spec.
	typedNew = map[string]any{"id": "t9-p2m-aaaaaa-s1-h02", "kind": "p2m-v7", "title": "secret title", "created_by": "owner",
		"role": "hole", "parent": "t9-p2m-aaaaaa-s1", "needs": []string{"t9-p2m-aaaaaa-s1-h01"}, "hole_id": "h-0123456789ab",
		"labels": map[string]string{"trial": "t9", "leaf": "p2m-aaaaaa", "outline": "1", "hole": "2"},
		"spec":   map[string]any{"prompt": "secret prompt " + strings.Repeat("x", 4000), "path": "/srv/runner/x"}}
	// legacyNew: a task from before the typed fields, whose spec gives its public place.
	legacyNew = map[string]any{"id": "t5-F01-h02", "kind": "other", "title": "secret title", "created_by": "owner",
		"spec": p2mSpec("F01", "CodedSorting.proposition_7_1_transpose", map[string]any{"role": "hole", "gen": 1, "index": 1})}
	retentionResult = map[string]any{"outcome": "proved", "checks_passed": true, "steps": 12, "prompt_tokens": 1000,
		"cached_tokens": 800, "completion_tokens": 100, "model_min": 3.5, "awake_min": 9,
		"score": map[string]any{"leaf": "p2m-aaaaaa", "provenance": "kept"}, "raw_ref": "raw/2026-11-17/t9"}
)

// retentionTasks makes three finished tasks: T done (typed), L failed (legacy), C cancelled.
func (r *taskRig) retentionTasks() {
	r.t.Helper()
	r.p2m("", typedNew)
	r.p2m("", legacyNew)
	r.p2m("", map[string]any{"id": "t9-C", "kind": "p2m-v7", "created_by": "owner", "role": "outline", "spec": map[string]any{"x": 1}})
	r.advance(time.Minute)
	lease := r.p2m("/claim", map[string]any{"worker": "runner-host-7", "task": "t9-p2m-aaaaaa-s1-h02", "lease_seconds": 600})["lease"].(map[string]any)["id"]
	r.g.tasks.noteNode("prove2me", "t9-p2m-aaaaaa-s1-h02", nodeClassTrusted)
	r.advance(time.Minute)
	r.p2m("/t9-p2m-aaaaaa-s1-h02/complete", map[string]any{"worker": "runner-host-7", "lease": lease, "result": retentionResult})
	r.advance(time.Minute) // distinct ends: the queue view orders finished tasks by them
	lease = r.p2m("/claim", map[string]any{"worker": "runner-host-7", "task": "t5-F01-h02", "lease_seconds": 600})["lease"].(map[string]any)["id"]
	r.p2m("/t5-F01-h02/fail", map[string]any{"worker": "runner-host-7", "lease": lease, "reason": "secret failure"})
	r.advance(time.Minute)
	r.p2m("/t9-C/cancel", map[string]any{"reason": "secret cancel"})
}

// A finished task is compacted once it ended task_compact_days ago (default 30): its spec, events,
// lease and error go; its public fields, result, updated_at and export mark stay, so both public
// views are the same as before; a repeated create is still recognised by the spec's hash; and the
// compacted task reads back the same after a restart.
func TestTaskCompaction(t *testing.T) {
	dir := t.TempDir()
	r := retentionRig(t, dir, 0)
	r.retentionTasks()
	ended := r.now
	before := r.g.tasks.get("prove2me", "t9-p2m-aaaaaa-s1-h02")
	v1Before, queueBefore := r.publicViews()
	if !strings.Contains(v1Before, `"hole":"2"`) || !strings.Contains(v1Before, "CodedSorting.proposition_7_1_transpose") || !strings.Contains(queueBefore, `"gen":1`) {
		t.Fatalf("the public views before: %s %s", v1Before, queueBefore)
	}
	file := filepath.Join(dir, "tasks-prove2me", "t9-p2m-aaaaaa-s1-h02.json")
	fileBefore, _ := os.ReadFile(file)
	// an export mark stays, and gates nothing
	if w, _ := r.call("POST", p2mTasks+"/t9-C/exported", testProjectKey, map[string]any{"seen": r.g.tasks.get("prove2me", "t9-C").UpdatedAt}); w.Code != 200 {
		t.Fatalf("exported: HTTP %d %s", w.Code, w.Body)
	}

	r.setNow(before.UpdatedAt.Add(30*24*time.Hour - time.Minute))
	r.call("GET", p2mTasks, testProjectKey, nil)
	if got := r.g.tasks.get("prove2me", "t9-p2m-aaaaaa-s1-h02"); got.CompactedAt != nil || got.Spec == nil || len(got.Events) == 0 {
		t.Fatalf("compacted before 30 days: %+v", got)
	}
	r.setNow(ended.Add(30*24*time.Hour + time.Minute))
	r.call("GET", p2mTasks, testProjectKey, nil) // any call with the key opens the queue: compacts it

	check := func(r *taskRig, when string) {
		t.Helper()
		got := r.g.tasks.get("prove2me", "t9-p2m-aaaaaa-s1-h02")
		if got.CompactedAt == nil || got.Spec != nil || got.Events != nil || got.Lease != nil || got.Live != nil || got.SpecSHA256 == "" {
			t.Fatalf("%s: not compacted: %+v", when, got)
		}
		gr, _ := json.Marshal(got.Result)
		br, _ := json.Marshal(before.Result)
		if string(gr) != string(br) || !strings.Contains(string(gr), "provenance") || !got.UpdatedAt.Equal(before.UpdatedAt) ||
			got.Title != before.Title || got.CreatedBy != before.CreatedBy || got.StartedAt == nil {
			t.Fatalf("%s: lost the result or a kept field: %s / %+v", when, gr, got)
		}
		for id, want := range map[string]string{"t5-F01-h02": TaskFailed, "t9-C": TaskCancelled} {
			got := r.g.tasks.get("prove2me", id)
			if got.State != want || got.CompactedAt == nil || got.Error != "" || got.Events != nil {
				t.Fatalf("%s: %s not compacted: %+v", when, id, got)
			}
		}
		if c := r.g.tasks.get("prove2me", "t9-C"); c.ExportedAt == nil {
			t.Fatalf("%s: the export mark went", when)
		}
		// the older task keeps the public part of its spec only
		if l := r.g.tasks.get("prove2me", "t5-F01-h02"); strings.Contains(string(l.Spec), "private_note") || strings.Contains(string(l.Spec), "selection") ||
			!strings.Contains(string(l.Spec), "CodedSorting") {
			t.Fatalf("%s: the older task's spec: %s", when, l.Spec)
		}
		v1, queue := r.publicViews()
		if v1 != v1Before || queue != queueBefore {
			t.Fatalf("%s: the public views changed:\n%s\n%s\nwant\n%s\n%s", when, v1, queue, v1Before, queueBefore)
		}
		raw, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		for _, gone := range []string{"secret prompt", "/srv/runner", `"events"`, `"lease"`, "runner-host-7"} {
			if strings.Contains(string(raw), gone) {
				t.Fatalf("%s: the file still has %q: %s", when, gone, raw)
			}
		}
		if len(raw) >= len(fileBefore)/2 {
			t.Fatalf("%s: the file is %d bytes, was %d", when, len(raw), len(fileBefore))
		}
		// a repeated create is still the same task; other content is still a conflict
		for _, n := range []map[string]any{typedNew, legacyNew} {
			if w, _ := r.call("POST", p2mTasks, testProjectKey, n); w.Code != http.StatusOK {
				t.Fatalf("%s: repeated create of %s: HTTP %d %s", when, n["id"], w.Code, w.Body)
			}
		}
		other := map[string]any{}
		for k, v := range typedNew {
			other[k] = v
		}
		other["spec"] = map[string]any{"prompt": "another"}
		if w, _ := r.call("POST", p2mTasks, testProjectKey, other); w.Code != http.StatusConflict {
			t.Fatalf("%s: create with another spec: HTTP %d %s", when, w.Code, w.Body)
		}
	}
	check(r, "compacted")
	at := *r.g.tasks.get("prove2me", "t9-p2m-aaaaaa-s1-h02").CompactedAt

	// a restart reads the compacted tasks back as they were, and does not compact them again
	r.g.Close()
	r2 := retentionRig(t, dir, 0)
	r2.setNow(r.now.Add(time.Hour))
	check(r2, "after a restart")
	if got := r2.g.tasks.get("prove2me", "t9-p2m-aaaaaa-s1-h02"); !got.CompactedAt.Equal(at) {
		t.Fatalf("compacted again at %s (first at %s)", got.CompactedAt, at)
	}
}

// task_compact_days sets the age per project, from 1 to maxCompactDays.
func TestTaskCompactDays(t *testing.T) {
	r := retentionRig(t, t.TempDir(), 2)
	r.retentionTasks()
	r.advance(2*24*time.Hour - time.Minute)
	r.call("GET", p2mTasks, testProjectKey, nil)
	if r.g.tasks.get("prove2me", "t9-C").CompactedAt != nil {
		t.Fatal("compacted before 2 days")
	}
	r.advance(2 * time.Minute)
	r.call("GET", p2mTasks, testProjectKey, nil)
	if r.g.tasks.get("prove2me", "t9-C").CompactedAt == nil {
		t.Fatal("not compacted after 2 days")
	}
	for _, days := range []int{-1, maxCompactDays + 1} {
		if _, err := newProjectTable([]Project{{ID: "x", KeySHA256: []string{keyHash(testProjectKey)}, TaskCompactDays: days}}); err == nil {
			t.Errorf("task_compact_days %d taken", days)
		}
	}
}

// Finished tasks not yet compacted are bounded in bytes: beyond maxFull the oldest are compacted
// early. One pass compacts at most maxCompactPerPass tasks; the next goes on. An open task is never
// compacted.
func TestTaskCompactionBounds(t *testing.T) {
	r := retentionRig(t, "", 0)
	n := maxCompactPerPass + 50
	for i := range n {
		r.p2m("", map[string]any{"id": fmt.Sprintf("t9-%04d", i), "kind": "p2m-v7", "created_by": "owner", "spec": map[string]any{"pad": strings.Repeat("x", 1000)}})
		r.advance(time.Second)
		r.p2m(fmt.Sprintf("/t9-%04d/cancel", i), map[string]any{})
	}
	r.p2m("", map[string]any{"id": "t9-open", "kind": "p2m-v7", "created_by": "owner", "spec": map[string]any{"pad": strings.Repeat("x", 1000)}})
	compacted := func() (n int) {
		s := r.g.tasks
		s.mu.Lock()
		defer s.mu.Unlock()
		for _, t := range s.byPr["prove2me"].byID {
			if t.CompactedAt != nil {
				n++
			}
		}
		return n
	}
	if c := compacted(); c != 0 {
		t.Fatalf("%d compacted while young and small", c)
	}
	pass := func() { // one open: one compaction pass (a call may open the queue more than once)
		s := r.g.tasks
		s.mu.Lock()
		defer s.mu.Unlock()
		s.open("prove2me")
	}
	// bytes: allow about 10 full tasks; the oldest go first, in passes of maxCompactPerPass
	r.g.tasks.maxFull = 10 * r.g.tasks.get("prove2me", "t9-0000").size
	pass()
	if c := compacted(); c != maxCompactPerPass {
		t.Fatalf("first pass compacted %d, want %d", c, maxCompactPerPass)
	}
	pass()
	if c := compacted(); c < n-10 || c >= n {
		t.Fatalf("after the second pass %d compacted, want the oldest %d to %d", c, n-10, n-1)
	}
	if r.g.tasks.get("prove2me", "t9-0000").CompactedAt == nil || r.g.tasks.get("prove2me", fmt.Sprintf("t9-%04d", n-1)).CompactedAt != nil {
		t.Fatal("not the oldest first")
	}
	// age: everything finished; the open task stays whole
	r.advance(31 * 24 * time.Hour)
	r.call("GET", p2mTasks, testProjectKey, nil)
	if c := compacted(); c != n {
		t.Fatalf("after 31 days %d compacted, want %d", c, n)
	}
	if got := r.g.tasks.get("prove2me", "t9-open"); got.CompactedAt != nil || got.Spec == nil || got.State != TaskQueued {
		t.Fatalf("an open task was compacted: %+v", got)
	}
}

// GET /v1/tasks pages through every kept task, compacted or not, past the old cap and the page limit:
// each task once, newest first, the last page without next.
func TestPublicTasksPagesManyTasks(t *testing.T) {
	r := retentionRig(t, t.TempDir(), 0)
	const n = 1203
	for i := range n {
		id := fmt.Sprintf("t9-%04d", i)
		r.p2m("", map[string]any{"id": id, "kind": "p2m-v7", "created_by": "owner", "role": "hole", "spec": map[string]any{}})
		r.p2m("/"+id+"/cancel", map[string]any{})
		r.advance(time.Second)
		if i == n/2 { // the older half is compacted
			r.advance(31 * 24 * time.Hour)
		}
	}
	r.call("GET", p2mTasks, testProjectKey, nil)
	r.call("GET", p2mTasks, testProjectKey, nil)
	if got := r.g.tasks.get("prove2me", "t9-0000"); got.CompactedAt == nil {
		t.Fatal("the older half not compacted")
	}
	r.freshV1()
	seen := map[string]bool{}
	var ids []string
	q, pages := "?limit=500", 0
	for {
		code, body, page := r.v1(q)
		if code != 200 || len(page.Tasks) > 500 {
			t.Fatalf("%s: HTTP %d %.200s", q, code, body)
		}
		pages++
		for _, tk := range page.Tasks {
			id := tk["id"].(string)
			if seen[id] {
				t.Fatalf("%s twice", id)
			}
			seen[id] = true
			ids = append(ids, id)
		}
		if page.Next == nil {
			break
		}
		q = "?limit=500&after=" + url.QueryEscape(*page.Next)
	}
	if len(ids) != n || pages != 3 || ids[0] != fmt.Sprintf("t9-%04d", n-1) || ids[n-1] != "t9-0000" {
		t.Fatalf("%d tasks in %d pages, first %s, last %s", len(ids), pages, ids[0], ids[len(ids)-1])
	}
}
