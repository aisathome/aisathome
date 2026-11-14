// SPDX-License-Identifier: Apache-2.0
package gateway

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"
)

// publicRig: prove2me with a public queue (key testProjectKey), tauceti without one.
func publicRig(t *testing.T) *taskRig {
	t.Helper()
	g := newTestGateway(t, Config{StateDir: t.TempDir(), Projects: []Project{
		{ID: "prove2me", KeySHA256: []string{keyHash(testProjectKey)}, PublicQueue: true},
		{ID: "tauceti", KeySHA256: []string{keyHash(otherProjectKey)}},
	}})
	r := &taskRig{t: t, g: g, now: time.Date(2026, 11, 13, 20, 0, 0, 0, time.UTC)}
	g.tasks.now = func() time.Time { return r.now }
	t.Cleanup(func() { g.Close() })
	return r
}

const p2mTasks = "/v1/projects/prove2me/tasks"

func p2mSpec(leaf, theorem string, v5 map[string]any) map[string]any {
	s := map[string]any{"trial": "t5", "leaf": leaf, "group": "open", "theorem": theorem, "selection": "2026-11-13",
		"scaffold": "v5", "caps": map[string]any{"steps": 90}, "private_note": "runner path /srv/runner/x"}
	if v5 != nil {
		s["v5"] = v5
	}
	return s
}

func (r *taskRig) p2m(path string, body any) map[string]any {
	r.t.Helper()
	w, out := r.call("POST", p2mTasks+path, testProjectKey, body)
	if w.Code != http.StatusOK && w.Code != http.StatusCreated {
		r.t.Fatalf("%s: HTTP %d %s", path, w.Code, w.Body)
	}
	return out
}

func (r *taskRig) publicQueue() (int, string, map[string]any) {
	r.t.Helper()
	w, out := r.call("GET", "/v1/projects/prove2me/queue", "", nil)
	return w.Code, w.Body.String(), out
}

// The public view lists the open tasks first, then the finished ones, with only the public fields:
// nothing of the spec beyond trial, leaf, group, theorem and the hole's place, no title, worker,
// lease, error or raw evidence; malformed values are left out. A project without public_queue, or
// none, answers 404, and no key is needed.
func TestPublicQueueHasPublicFieldsOnly(t *testing.T) {
	r := publicRig(t)
	mk := func(id string, spec map[string]any) {
		r.p2m("", map[string]any{"id": id, "kind": "attempt", "title": "prove2me t5 secret title", "created_by": "owner", "spec": spec})
	}
	mk("t5-F01", p2mSpec("F01", "CodedSorting.proposition_7_1_transpose", map[string]any{"role": "skeleton", "gen": 1, "task": "t5-F01"}))
	mk("t5-F01-h02", p2mSpec("F01", "CodedSorting.proposition_7_1_transpose", map[string]any{"role": "hole", "gen": 1, "index": 1, "key": "abcdef0123"}))
	mk("t5-C12", p2mSpec("C12", "Bad name <script>", nil))
	mk("t5-X", p2mSpec("bad leaf/../x", "Ok.name", map[string]any{"role": "Not A Role", "gen": -2, "index": -1}))

	// t5-F01 runs, t5-C12 ends with its result, t5-X ends with an outcome that is not a plain word.
	r.p2m("/claim", map[string]any{"worker": "runner-host-7", "task": "t5-F01", "lease_seconds": 600})
	r.now = r.now.Add(time.Minute)
	lease := r.p2m("/claim", map[string]any{"worker": "runner-host-7", "task": "t5-C12", "lease_seconds": 600})["lease"].(map[string]any)["id"]
	r.p2m("/t5-C12/complete", map[string]any{"worker": "runner-host-7", "lease": lease, "result": map[string]any{"outcome": "proved",
		"checks_passed": true, "steps": 12, "prompt_tokens": 120000, "completion_tokens": 3000, "model_min": 4.5, "awake_min": 9,
		"raw_ref": "raw/2026-11-13/t5-C12", "diff": "--- secret diff", "score": map[string]any{"note": "secret score"}}})
	r.now = r.now.Add(time.Minute)
	lease = r.p2m("/claim", map[string]any{"worker": "runner-host-7", "task": "t5-X", "lease_seconds": 600})["lease"].(map[string]any)["id"]
	r.p2m("/t5-X/complete", map[string]any{"worker": "runner-host-7", "lease": lease, "result": map[string]any{"outcome": "Lake Build failed: /srv/x",
		"steps": 1, "prompt_tokens": 10, "completion_tokens": 1, "model_min": 0.1, "awake_min": 1}})

	code, body, out := r.publicQueue()
	if code != 200 {
		t.Fatalf("public queue: HTTP %d %s", code, body)
	}
	for _, bad := range []string{"runner-host-7", "secret", "/srv", "raw/", `"lease"`, "worker", "title", "spec", "selection", "caps",
		"<script>", "bad leaf", "Not A Role", "events", "error", `"key"`, "abcdef", "Lake"} {
		if strings.Contains(body, bad) {
			t.Errorf("the public view contains %q: %s", bad, body)
		}
	}
	var view struct {
		Project string         `json:"project"`
		Counts  map[string]int `json:"counts"`
		Tasks   []map[string]any
	}
	json.Unmarshal([]byte(body), &view)
	var ids []string
	for _, tk := range view.Tasks {
		ids = append(ids, tk["id"].(string))
		allowed := []string{"id", "state", "created_at", "updated_at", "since", "trial", "leaf", "group", "theorem", "role", "gen", "hole",
			"outcome", "prompt_tokens", "completion_tokens", "model_min"}
		for k := range tk {
			if !slices.Contains(allowed, k) {
				t.Errorf("task %s has field %q", tk["id"], k)
			}
		}
	}
	if want := []string{"t5-F01", "t5-F01-h02", "t5-X", "t5-C12"}; !slices.Equal(ids, want) {
		t.Fatalf("order: %v, want running, queued, then finished newest first %v", ids, want)
	}
	if view.Project != "prove2me" || view.Counts["leased"] != 1 || view.Counts["queued"] != 1 || view.Counts["done"] != 2 || view.Counts["failed"] != 0 {
		t.Fatalf("project and counts: %+v", out)
	}
	byID := map[string]map[string]any{}
	for _, tk := range view.Tasks {
		byID[tk["id"].(string)] = tk
	}
	if f := byID["t5-F01"]; f["state"] != "leased" || f["since"] == nil || f["theorem"] != "CodedSorting.proposition_7_1_transpose" ||
		f["leaf"] != "F01" || f["trial"] != "t5" || f["group"] != "open" || f["role"] != "skeleton" || f["gen"] != 1.0 || f["hole"] != nil {
		t.Errorf("running task: %v", f)
	}
	if h := byID["t5-F01-h02"]; h["role"] != "hole" || h["hole"] != 2.0 || h["since"] != nil {
		t.Errorf("hole task (index 1 is hole 2): %v", h)
	}
	if c := byID["t5-C12"]; c["outcome"] != "proved" || c["prompt_tokens"] != 120000.0 || c["completion_tokens"] != 3000.0 ||
		c["model_min"] != 4.5 || c["theorem"] != nil || c["leaf"] != "C12" {
		t.Errorf("finished task (a theorem that is not a Lean name left out): %v", c)
	}
	if x := byID["t5-X"]; x["outcome"] != "other" || x["leaf"] != nil || x["role"] != nil || x["gen"] != nil || x["hole"] != nil {
		t.Errorf("malformed values left out: %v", x)
	}

	// Served from memory for publicQueueTTL: a change shows after it, not before.
	r.p2m("", map[string]any{"id": "t5-O02", "kind": "attempt", "created_by": "owner", "spec": p2mSpec("O02", "BBPSSW.Distill.typical_set_sqrt", nil)})
	if _, again, _ := r.publicQueue(); again != body {
		t.Fatal("a second request within the TTL built the view again")
	}
	r.g.pubq.mu.Lock()
	b := r.g.pubq.byID["prove2me"]
	b.at = b.at.Add(-publicQueueTTL)
	r.g.pubq.byID["prove2me"] = b
	r.g.pubq.mu.Unlock()
	if _, later, _ := r.publicQueue(); !strings.Contains(later, "t5-O02") {
		t.Fatal("the view was not built again after the TTL")
	}

	for _, path := range []string{"/v1/projects/tauceti/queue", "/v1/projects/nosuch/queue"} {
		if w, _ := r.call("GET", path, "", nil); w.Code != http.StatusNotFound {
			t.Errorf("%s: HTTP %d, want 404 (no public queue)", path, w.Code)
		}
	}
}

// The queue's tasks stay private: the existing list still needs the key.
func TestPublicQueueLeavesTheTaskListPrivate(t *testing.T) {
	r := publicRig(t)
	if w, _ := r.call("GET", p2mTasks, "", nil); w.Code != http.StatusUnauthorized {
		t.Fatalf("task list without a key: HTTP %d", w.Code)
	}
}

// blockingWriter is a client that never reads: its Write blocks until released.
type blockingWriter struct {
	h       http.Header
	entered chan struct{}
	release chan struct{}
}

func (b *blockingWriter) Header() http.Header { return b.h }
func (b *blockingWriter) WriteHeader(int)     {}
func (b *blockingWriter) Write(p []byte) (int, error) {
	select {
	case b.entered <- struct{}{}:
	default:
	}
	<-b.release
	return len(p), nil
}

// A reader that never takes its bytes holds up nobody else: the cache lock covers the build, not
// the write (Codex review 2026-11-14: a slow client could block every project's public queue).
func TestPublicQueueSlowReaderBlocksNobody(t *testing.T) {
	r := publicRig(t)
	bw := &blockingWriter{h: http.Header{}, entered: make(chan struct{}, 1), release: make(chan struct{})}
	defer close(bw.release)
	req := httptest.NewRequest("GET", "/v1/projects/prove2me/queue", nil)
	req.SetPathValue("id", "prove2me")
	go r.g.handlePublicQueue(bw, req)
	select {
	case <-bw.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the slow reader's request never reached its write")
	}
	done := make(chan int, 1)
	go func() {
		code, _, _ := r.publicQueue()
		done <- code
	}()
	select {
	case code := <-done:
		if code != http.StatusOK {
			t.Fatalf("second reader: HTTP %d", code)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("a reader that never reads blocked the public queue for everyone")
	}
}
