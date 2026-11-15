// The task schema of decision 0019 through the real server: kinds declared per project, typed fields,
// GET /v1/tasks across projects, older task files and clients, and the removed review loop.
//
// SPDX-License-Identifier: Apache-2.0
package e2e

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/aisathome/aisathome/gateway"
)

// withV7 adds prove2me with a public queue, kind p2m-v7 and its page to sciproj (not public).
func withV7(c *gateway.Config) {
	withProjects(c)
	c.Projects = append(c.Projects, gateway.Project{ID: "prove2me", KeySHA256: []string{sha(p2mKey)}, AgentOK: true,
		PublicQueue: true, Kinds: []string{"p2m-v7"}, Page: "/science/prove2me/", PageAnchor: "leaf"})
}

func (l *queueGateway) p2m(t *testing.T, method, path string, body any) (int, map[string]any) {
	t.Helper()
	return l.send(t, method, "/v1/projects/prove2me/tasks"+path, p2mKey, body)
}

// publicGet reads a public path without a key: its status and body.
func (l *queueGateway) publicGet(t *testing.T, path string) (int, string) {
	t.Helper()
	resp, err := http.Get(l.url + path)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(raw)
}

// A v7 outline and hole task with typed fields, a task made the old way, a worker that claims only
// its kind and names its task in its model request, a hole closed by reuse: GET /v1/tasks shows them
// all in one schema with public fields only, the public queue reads the typed fields, and all of it
// survives a restart.
func TestTaskV7ThroughTheServer(t *testing.T) {
	l := &queueGateway{}
	r := newRigCfg(t, func(c *gateway.Config) {
		c.StateDir = t.TempDir()
		withV7(c)
		l.cfg = *c
	}, mockEngine(t, false))
	l.g, l.srv, l.url = r.g, r.gw, r.gw.URL

	outline := map[string]any{"id": "t6-p2m-a146ce-s1", "kind": "p2m-v7", "created_by": "owner", "role": "outline", "order": -1,
		"title": "private title", "labels": map[string]string{"trial": "t6", "leaf": "p2m-a146ce", "alias": "F01", "outline": "1",
			"theorem": "CodedSorting.proposition_7_1_transpose"}, "spec": map[string]any{"prompt": "private prompt 9d2e"}}
	hole := map[string]any{"id": "t6-p2m-a146ce-s1-h01", "kind": "p2m-v7", "created_by": "owner", "role": "hole",
		"parent": "t6-p2m-a146ce-s1", "hole_id": "h-3f9c0a1b2c3d", "labels": map[string]string{"trial": "t6", "leaf": "p2m-a146ce", "outline": "1", "hole": "1"},
		"spec": map[string]any{"statement": "private statement"}}
	old := p2mTask("t5-F01", "F01", "CodedSorting.proposition_7_1_transpose") // kind other, its place in the spec
	for _, tk := range []map[string]any{old, outline, hole} {
		if code, out := l.p2m(t, "POST", "", tk); code != http.StatusCreated {
			t.Fatalf("create %s: HTTP %d %v", tk["id"], code, out)
		}
	}
	for _, k := range []string{"attempt", "fix_round", "review", "p2m-v8"} {
		if code, _ := l.p2m(t, "POST", "", map[string]any{"id": "X-" + k, "kind": k, "created_by": "owner", "spec": map[string]any{}}); code != 400 {
			t.Errorf("kind %s: HTTP %d, want 400", k, code)
		}
	}

	// A v6 worker gets nothing; the v7 worker gets the outline, then the hole, and never the old task.
	if code, _ := l.p2m(t, "POST", "/claim", map[string]any{"worker": "runner-v6", "kinds": []string{"p2m-v6"}}); code != http.StatusNoContent {
		t.Fatalf("a p2m-v6 claim: HTTP %d", code)
	}
	code, out := l.p2m(t, "POST", "/claim", map[string]any{"worker": "runner-host-7", "kinds": []string{"p2m-v7"}, "hold_seconds": 0})
	if code != 200 || out["task"].(map[string]any)["id"] != "t6-p2m-a146ce-s1" {
		t.Fatalf("v7 claim: HTTP %d %v", code, out)
	}
	outlineLease := out["lease"].(map[string]any)["id"].(string)
	if code := askAs(t, r, p2mKey, "outline it", map[string]string{"X-Aisathome-Task": "t6-p2m-a146ce-s1"}); code != 200 {
		t.Fatalf("the worker's model request: HTTP %d", code)
	}
	if code, out := l.p2m(t, "POST", "/t6-p2m-a146ce-s1/complete", map[string]any{"worker": "runner-host-7", "lease": outlineLease,
		"result": map[string]any{"outcome": "outline", "steps": 40, "prompt_tokens": 9000, "cached_tokens": 7000, "completion_tokens": 900,
			"model_min": 12, "raw_ref": "raw/2026-11-15/private-ref"}}); code != 200 {
		t.Fatalf("complete the outline: HTTP %d %v", code, out)
	}
	code, out = l.p2m(t, "POST", "/claim", map[string]any{"worker": "runner-host-7", "kinds": []string{"p2m-v7"}})
	if code != 200 || out["task"].(map[string]any)["id"] != "t6-p2m-a146ce-s1-h01" {
		t.Fatalf("claim the hole: HTTP %d %v", code, out)
	}
	if code, out := l.p2m(t, "POST", "/t6-p2m-a146ce-s1-h01/complete", map[string]any{"worker": "runner-host-7", "lease": out["lease"].(map[string]any)["id"],
		"closed_by": "reuse:formalpedia:Finset.sum_comm", "result": map[string]any{"outcome": "closed", "model_min": 0}}); code != 200 {
		t.Fatalf("complete the hole as closed: HTTP %d %v", code, out)
	}
	// a retry of the hole: try 2
	if code, out := l.p2m(t, "POST", "", map[string]any{"id": "t6-p2m-a146ce-s1-h01-a2", "kind": "p2m-v7", "created_by": "owner", "role": "hole",
		"retry_of": "t6-p2m-a146ce-s1-h01", "spec": map[string]any{}}); code != http.StatusCreated {
		t.Fatalf("create the retry: HTTP %d %v", code, out)
	}
	// sciproj's own task: not public
	l.create(t, "SCI-1", "p2m-v7")

	check := func(when string) {
		t.Helper()
		code, body := l.publicGet(t, "/v1/tasks?project=prove2me")
		if code != 200 {
			t.Fatalf("%s: GET /v1/tasks: HTTP %d %s", when, code, body)
		}
		for _, bad := range []string{"private", "runner-host-7", "SCI-1", "sciproj", "9d2e", "raw/", "spec", "title", "worker", "lease"} {
			if strings.Contains(body, bad) {
				t.Errorf("%s: GET /v1/tasks contains %q: %s", when, bad, body)
			}
		}
		var page struct {
			Tasks []map[string]any `json:"tasks"`
		}
		json.Unmarshal([]byte(body), &page)
		by := map[string]map[string]any{}
		for _, tk := range page.Tasks {
			by[tk["id"].(string)] = tk
		}
		if len(by) != 4 {
			t.Fatalf("%s: tasks %v", when, by)
		}
		o := by["t6-p2m-a146ce-s1"]
		if o["role"] != "outline" || o["state"] != "done" || o["outcome"] != "outline" || o["tokens"] != 9900.0 || o["cached_tokens"] != 7000.0 ||
			o["labels"].(map[string]any)["alias"] != "F01" || !slices.Equal(o["node_classes"].([]any), []any{"volunteer"}) || o["started_at"] == nil {
			t.Errorf("%s: outline %v", when, o)
		}
		h := by["t6-p2m-a146ce-s1-h01"]
		if h["parent"] != "t6-p2m-a146ce-s1" || h["hole_id"] != "h-3f9c0a1b2c3d" || h["closed_by"] != "reuse:formalpedia:Finset.sum_comm" || h["try"] != 1.0 {
			t.Errorf("%s: hole %v", when, h)
		}
		if a2 := by["t6-p2m-a146ce-s1-h01-a2"]; a2["try"] != 2.0 || a2["retry_of"] != "t6-p2m-a146ce-s1-h01" || a2["state"] != "queued" {
			t.Errorf("%s: retry %v", when, a2)
		}
		if f := by["t5-F01"]; f["kind"] != "other" || f["role"] != "skeleton" || f["parent"] != nil || f["labels"].(map[string]any)["leaf"] != "F01" {
			t.Errorf("%s: the old task %v", when, f)
		}
		// the public queue (the site, until it moves) reads the typed fields
		code, body = l.publicGet(t, "/v1/projects/prove2me/queue")
		if code != 200 || !strings.Contains(body, `"id":"t6-p2m-a146ce-s1-h01","state":"done"`) || !strings.Contains(body, `"leaf":"p2m-a146ce"`) ||
			!strings.Contains(body, `"role":"hole"`) || strings.Contains(body, "private") {
			t.Errorf("%s: public queue: HTTP %d %s", when, code, body)
		}
	}
	check("running")

	// A task file from before decision 0019 (TauCeti's loop fields), planted before a restart.
	l.srv.Close()
	l.g.Close()
	oldFile := `{"id":"T5-NS-3.a-A","project":"sciproj","kind":"attempt","class":5,"findings_source":"prereview","chain":"T5-NS-3.a",
"stage":"ready-for-operator","spec":{"attempt":"T5-NS-3.a-A"},"base_ref":"a57def6","created_by":"owner","state":"queued","tries":0,
"max_tries":3,"seq":90,"created_at":"2026-10-30T10:00:00Z","updated_at":"2026-10-30T10:00:00Z","loop_pending":{"step":"stop"}}`
	if err := os.WriteFile(filepath.Join(l.cfg.StateDir, "tasks-sciproj", "T5-NS-3.a-A.json"), []byte(oldFile), 0o600); err != nil {
		t.Fatal(err)
	}
	l.restart(t)
	check("after a restart")
	if code, out := l.call(t, "POST", "/claim", map[string]any{"worker": "runner-old", "kinds": []string{"attempt"}}); code != 200 ||
		out["task"].(map[string]any)["id"] != "T5-NS-3.a-A" {
		t.Fatalf("claim the old task by its kind: HTTP %d %v", code, out)
	}

	// The review loop and the science results are gone.
	for _, c := range []struct{ method, path string }{
		{"POST", "/v1/projects/prove2me/tasks/t6-p2m-a146ce-s1/review"}, {"POST", "/v1/projects/prove2me/tasks/t6-p2m-a146ce-s1/decide"},
		{"POST", "/v1/projects/prove2me/tasks/t6-p2m-a146ce-s1/continue"}, {"POST", "/v1/projects/prove2me/science"},
		{"GET", "/v1/projects/prove2me/science"}, {"GET", "/v1/projects/sciproj/science"},
	} {
		if code, _ := l.send(t, c.method, c.path, p2mKey, map[string]any{"verdict": "green", "by": "x"}); code != 404 && code != 405 {
			t.Errorf("%s %s: HTTP %d, want 404", c.method, c.path, code)
		}
	}
}
