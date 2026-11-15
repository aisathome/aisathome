// SPDX-License-Identifier: Apache-2.0
package gateway

import (
	"encoding/json"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"
)

// v1Rig: prove2me (public, kinds p2m-v7), lab (not public).
func v1Rig(t *testing.T) *taskRig {
	t.Helper()
	g := newTestGateway(t, Config{StateDir: t.TempDir(), Projects: []Project{
		{ID: "prove2me", KeySHA256: []string{keyHash(testProjectKey)}, PublicQueue: true, Kinds: []string{"p2m-v7"}},
		{ID: "lab", KeySHA256: []string{keyHash(otherProjectKey)}, Kinds: []string{"p2m-v7"}},
	}})
	r := &taskRig{t: t, g: g, now: time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)}
	g.tasks.now = func() time.Time { return r.now }
	t.Cleanup(func() { g.Close() })
	return r
}

type v1Page struct {
	Updated  string           `json:"updated"`
	Projects []string         `json:"projects"`
	Tasks    []map[string]any `json:"tasks"`
	Next     *string          `json:"next"`
}

func (r *taskRig) v1(query string) (int, string, v1Page) {
	r.t.Helper()
	w, _ := r.call("GET", "/v1/tasks"+query, "", nil)
	var p v1Page
	json.Unmarshal(w.Body.Bytes(), &p)
	return w.Code, w.Body.String(), p
}

// freshV1 drops the built list, as publicQueueTTL would.
func (r *taskRig) freshV1() {
	r.g.ptasks.mu.Lock()
	r.g.ptasks.list = nil
	r.g.ptasks.mu.Unlock()
}

// The network's tasks in one schema, without a key: the public projects only, every field of the
// schema (null when the task has none), nothing private; an older task's role and labels from its
// spec; the done task's counts and times.
func TestPublicTasksSchema(t *testing.T) {
	r := v1Rig(t)
	r.p2m("", map[string]any{"id": "t5-F01-h02", "kind": "other", "title": "secret title", "created_by": "owner",
		"spec": p2mSpec("F01", "CodedSorting.proposition_7_1_transpose", map[string]any{"role": "hole", "gen": 1, "index": 1})})
	r.now = r.now.Add(time.Minute)
	r.p2m("", map[string]any{"id": "t6-p2m-a146ce-s2-h03", "kind": "p2m-v7", "title": "secret title", "created_by": "owner-private",
		"role": "hole", "parent": "t6-p2m-a146ce-s2", "needs": []string{"t6-p2m-a146ce-s1-h01"}, "replaces": "t6-p2m-a146ce-s1",
		"hole_id": "h-3f9c0a1b2c3d", "labels": map[string]string{"trial": "t6", "leaf": "p2m-a146ce", "alias": "F01"},
		"spec": map[string]any{"prompt": "secret prompt text", "path": "/srv/runner/x"}})
	r.now = r.now.Add(time.Minute)
	r.p2m("", map[string]any{"id": "t6-p2m-a146ce-s2-h03-a2", "kind": "p2m-v7", "created_by": "owner", "role": "hole",
		"retry_of": "t6-p2m-a146ce-s2-h03", "spec": map[string]any{}})
	if w, _ := r.call("POST", "/v1/projects/lab/tasks", otherProjectKey, newTask("LAB-1", "p2m-v7")); w.Code != http.StatusCreated {
		t.Fatalf("lab task: HTTP %d", w.Code)
	}
	r.now = r.now.Add(time.Minute)
	lease := r.p2m("/claim", map[string]any{"worker": "runner-host-7", "task": "t6-p2m-a146ce-s2-h03", "lease_seconds": 600})["lease"].(map[string]any)["id"]
	r.g.tasks.noteNode("prove2me", "t6-p2m-a146ce-s2-h03", nodeClassVolunteer)
	r.now = r.now.Add(time.Minute)
	r.p2m("/t6-p2m-a146ce-s2-h03/complete", map[string]any{"worker": "runner-host-7", "lease": lease, "closed_by": "reuse:formalpedia:Foo.bar",
		"result": map[string]any{"outcome": "closed", "steps": 3, "prompt_tokens": 1000, "cached_tokens": 800, "completion_tokens": 50,
			"model_min": 1.5, "raw_ref": "raw/2026-11-15/secret", "score": map[string]any{"note": "secret score"}}})

	code, body, page := r.v1("")
	if code != 200 {
		t.Fatalf("HTTP %d %s", code, body)
	}
	for _, bad := range []string{"secret", "runner-host-7", "owner-private", "/srv", "raw/", "LAB-1", `"lab"`, "lease", "worker", "spec", "title", "events", "created_by"} {
		if strings.Contains(body, bad) {
			t.Errorf("GET /v1/tasks contains %q: %s", bad, body)
		}
	}
	if !slices.Equal(page.Projects, []string{"prove2me"}) || page.Next != nil || len(page.Tasks) != 3 {
		t.Fatalf("page %+v", page)
	}
	fields := []string{"project", "id", "kind", "role", "state", "outcome", "try", "parent", "needs", "retry_of", "replaces", "closed_by",
		"labels", "hole_id", "steps", "model_min", "tokens", "prompt_tokens", "cached_tokens", "completion_tokens", "created_at",
		"started_at", "finished_at", "updated_at", "node_classes"}
	var ids []string
	for _, tk := range page.Tasks {
		ids = append(ids, tk["id"].(string))
		var keys []string
		for k := range tk {
			keys = append(keys, k)
		}
		slices.Sort(keys)
		want := slices.Clone(fields)
		slices.Sort(want)
		if !slices.Equal(keys, want) {
			t.Errorf("task %s fields %v, want %v", tk["id"], keys, want)
		}
	}
	if want := []string{"t6-p2m-a146ce-s2-h03-a2", "t6-p2m-a146ce-s2-h03", "t5-F01-h02"}; !slices.Equal(ids, want) {
		t.Fatalf("order %v, want newest first %v", ids, want)
	}
	done, retry, old := page.Tasks[1], page.Tasks[0], page.Tasks[2]
	if done["project"] != "prove2me" || done["kind"] != "p2m-v7" || done["role"] != "hole" || done["state"] != "done" || done["outcome"] != "closed" ||
		done["try"] != 1.0 || done["parent"] != "t6-p2m-a146ce-s2" || done["replaces"] != "t6-p2m-a146ce-s1" || done["retry_of"] != nil ||
		done["closed_by"] != "reuse:formalpedia:Foo.bar" || done["hole_id"] != "h-3f9c0a1b2c3d" || done["steps"] != 3.0 || done["model_min"] != 1.5 ||
		done["tokens"] != 1050.0 || done["prompt_tokens"] != 1000.0 || done["cached_tokens"] != 800.0 || done["completion_tokens"] != 50.0 ||
		done["started_at"] == nil || done["finished_at"] == nil {
		t.Errorf("done task: %v", done)
	}
	if !slices.Equal(done["needs"].([]any), []any{"t6-p2m-a146ce-s1-h01"}) || !slices.Equal(done["node_classes"].([]any), []any{"volunteer"}) {
		t.Errorf("done task's needs and node classes: %v %v", done["needs"], done["node_classes"])
	}
	if l := done["labels"].(map[string]any); l["alias"] != "F01" || l["leaf"] != "p2m-a146ce" || len(l) != 3 {
		t.Errorf("labels %v", l)
	}
	if retry["try"] != 2.0 || retry["retry_of"] != "t6-p2m-a146ce-s2-h03" || retry["state"] != "queued" || retry["outcome"] != nil ||
		retry["started_at"] != nil || retry["finished_at"] != nil || retry["tokens"] != nil || len(retry["labels"].(map[string]any)) != 0 {
		t.Errorf("queued retry: %v", retry)
	}
	// a task from before the typed fields: role and labels from its spec, no relations, no try
	if l := old["labels"].(map[string]any); old["kind"] != "other" || old["role"] != "hole" || old["try"] != 1.0 || old["parent"] != nil ||
		l["trial"] != "t5" || l["leaf"] != "F01" || l["outline"] != "1" || l["hole"] != "2" || l["theorem"] != "CodedSorting.proposition_7_1_transpose" {
		t.Errorf("older task: %v", old)
	}
}

// Filters and pages: a page of `limit` with a cursor to the next; the next page starts right after
// it, also when newer tasks came meanwhile; bad parameters are refused.
func TestPublicTasksPagesAndFilters(t *testing.T) {
	r := v1Rig(t)
	for i := range 7 {
		role := "hole"
		if i%3 == 0 {
			role = "outline"
		}
		r.p2m("", map[string]any{"id": "t6-T" + string(rune('A'+i)), "kind": "p2m-v7", "created_by": "owner", "role": role, "spec": map[string]any{}})
		r.now = r.now.Add(time.Second)
	}
	var all []string
	q := "?limit=3"
	for range 5 {
		code, body, page := r.v1(q)
		if code != 200 || len(page.Tasks) > 3 {
			t.Fatalf("%s: HTTP %d %s", q, code, body)
		}
		for _, tk := range page.Tasks {
			all = append(all, tk["id"].(string))
		}
		if page.Next == nil {
			break
		}
		if len(all) == 3 { // a newer task while the client pages: not in the later pages, nothing skipped
			r.p2m("", map[string]any{"id": "t6-NEW", "kind": "p2m-v7", "created_by": "owner", "spec": map[string]any{}})
			r.freshV1()
		}
		q = "?limit=3&after=" + url.QueryEscape(*page.Next)
	}
	if want := []string{"t6-TG", "t6-TF", "t6-TE", "t6-TD", "t6-TC", "t6-TB", "t6-TA"}; !slices.Equal(all, want) {
		t.Fatalf("pages %v, want %v", all, want)
	}
	r.freshV1()
	if _, _, page := r.v1("?role=outline&project=prove2me&state=queued&kind=p2m-v7"); len(page.Tasks) != 3 {
		t.Fatalf("role=outline: %d tasks", len(page.Tasks))
	}
	if _, _, page := r.v1("?project=lab"); len(page.Tasks) != 0 {
		t.Fatalf("a project without public_queue: %v", page.Tasks)
	}
	for _, bad := range []string{"?limit=0", "?limit=501", "?limit=x", "?state=running", "?kind=Bad", "?role=A+B", "?project=../x", "?after=nope", "?after=1~lab~x~y"} {
		if code, _, _ := r.v1(bad); code != 400 {
			t.Errorf("%s: HTTP %d", bad, code)
		}
	}
}

// The list is built at most every publicQueueTTL; a change shows after it.
func TestPublicTasksCached(t *testing.T) {
	r := v1Rig(t)
	r.p2m("", map[string]any{"id": "A", "kind": "p2m-v7", "created_by": "owner", "spec": map[string]any{}})
	_, first, _ := r.v1("")
	r.p2m("", map[string]any{"id": "B", "kind": "p2m-v7", "created_by": "owner", "spec": map[string]any{}})
	if _, again, _ := r.v1(""); again != first {
		t.Fatal("built again within the TTL")
	}
	r.g.ptasks.mu.Lock()
	r.g.ptasks.at = r.g.ptasks.at.Add(-publicQueueTTL)
	r.g.ptasks.mu.Unlock()
	if _, later, _ := r.v1(""); !strings.Contains(later, `"id":"B"`) {
		t.Fatal("not built again after the TTL")
	}
	w, _ := r.call("GET", "/v1/tasks", "", nil)
	if w.Header().Get("Cache-Control") != "public, max-age=15" || w.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("headers %v", w.Header())
	}
}
