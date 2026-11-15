// Live counts of a running task's model calls through the real server (gateway/tasks_live.go).
//
// SPDX-License-Identifier: Apache-2.0
package e2e

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/aisathome/aisathome/gateway"
)

// askTask sends one chat request with key, naming task in X-Aisathome-Task ("" = no header), streamed
// or not, and reads the whole answer.
func askTask(t *testing.T, url, key, task string, stream bool) {
	t.Helper()
	b, _ := json.Marshal(map[string]any{"model": "deep", "stream": stream, "messages": []any{map[string]any{"role": "user", "content": "go on"}}})
	req, _ := http.NewRequest("POST", url+"/v1/chat/completions", strings.NewReader(string(b)))
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	if task != "" {
		req.Header.Set("X-Aisathome-Task", task)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !strings.Contains(string(raw), "world") {
		t.Fatalf("task %q, stream %v: HTTP %d %s", task, stream, resp.StatusCode, raw)
	}
}

// liveOf finds task id in a public answer's tasks and returns its live field (nil when null or absent).
func liveOf(t *testing.T, body, id string) map[string]any {
	t.Helper()
	var page struct {
		Tasks []map[string]any `json:"tasks"`
	}
	if err := json.Unmarshal([]byte(body), &page); err != nil {
		t.Fatalf("%v: %s", err, body)
	}
	for _, tk := range page.Tasks {
		if tk["id"] == id {
			l, _ := tk["live"].(map[string]any)
			return l
		}
	}
	t.Fatalf("no task %s in %s", id, body)
	return nil
}

// A worker's streamed and non-streamed model calls that name its running task count on that task,
// and GET /v1/tasks and the project's queue show them as live; calls naming a queued task, an unknown
// one, another project's or a task of a project without a public queue count nowhere and still get
// their answer. The count is saved with the task; after the task's end nothing more counts and the
// public views drop it.
func TestTaskLiveCountsThroughTheServer(t *testing.T) {
	l := &queueGateway{}
	r := newRigCfg(t, func(c *gateway.Config) {
		c.StateDir = t.TempDir()
		withV7(c)
		l.cfg = *c
	}, mockEngine(t, false))
	l.g, l.srv, l.url = r.g, r.gw, r.gw.URL

	for _, id := range []string{"t7-a", "t7-q"} {
		if code, out := l.p2m(t, "POST", "", map[string]any{"id": id, "kind": "p2m-v7", "created_by": "owner", "role": "hole",
			"spec": map[string]any{"prompt": "private"}}); code != http.StatusCreated {
			t.Fatalf("create %s: HTTP %d %v", id, code, out)
		}
	}
	code, out := l.p2m(t, "POST", "/claim", map[string]any{"worker": "runner-1", "task": "t7-a"})
	if code != 200 {
		t.Fatalf("claim: HTTP %d %v", code, out)
	}
	lease := out["lease"].(map[string]any)["id"]
	l.create(t, "SCI-1", "p2m-v7") // sciproj: no public queue
	l.claim(t, "SCI-1")

	askTask(t, l.url, p2mKey, "t7-a", true)
	askTask(t, l.url, p2mKey, "t7-a", false)
	askTask(t, l.url, p2mKey, "t7-q", true)       // queued
	askTask(t, l.url, p2mKey, "t7-nosuch", true)  // unknown
	askTask(t, l.url, p2mKey, "", true)           // no header
	askTask(t, l.url, projectKey, "t7-a", true)   // another project's task
	askTask(t, l.url, projectKey, "SCI-1", false) // running, but not public

	// mockEngine: 12 prompt tokens (8 from the cache) and 3 written per call.
	check := func(when, body, id string) {
		t.Helper()
		lv := liveOf(t, body, id)
		if lv == nil || lv["requests"] != 2.0 || lv["prompt_tokens"] != 24.0 || lv["cached_tokens"] != 16.0 || lv["completion_tokens"] != 6.0 ||
			lv["model_s"] == nil || lv["model_s"].(float64) < 0 || lv["updated_at"] == nil {
			t.Fatalf("%s: live of %s: %v in %s", when, id, lv, body)
		}
	}
	code, body := l.publicGet(t, "/v1/tasks?project=prove2me")
	if code != 200 {
		t.Fatalf("GET /v1/tasks: HTTP %d", code)
	}
	check("running", body, "t7-a")
	if lv := liveOf(t, body, "t7-q"); lv != nil {
		t.Errorf("the queued task counted: %v", lv)
	}
	code, qbody := l.publicGet(t, "/v1/projects/prove2me/queue")
	if code != 200 || strings.Count(qbody, `"live"`) != 1 {
		t.Fatalf("public queue: HTTP %d %s", code, qbody)
	}
	check("queue", qbody, "t7-a")
	if sci := l.get(t, "SCI-1"); sci["live"] != nil {
		t.Errorf("a task of a project without a public queue counted: %v", sci["live"])
	}

	// A heartbeat and the end save the count. After the end a call that names the task still gets its
	// answer and counts nowhere; the public views drop the live count, the project's own view keeps it.
	if code, out := l.p2m(t, "POST", "/t7-a/heartbeat", map[string]any{"worker": "runner-1", "lease": lease}); code != 200 {
		t.Fatalf("heartbeat: HTTP %d %v", code, out)
	}
	if code, out := l.p2m(t, "POST", "/t7-a/complete", map[string]any{"worker": "runner-1", "lease": lease,
		"result": map[string]any{"outcome": "closed", "steps": 2, "prompt_tokens": 24, "completion_tokens": 6, "model_min": 0.1}}); code != 200 {
		t.Fatalf("complete: HTTP %d %v", code, out)
	}
	askTask(t, l.url, p2mKey, "t7-a", true)
	own := func(when string) {
		t.Helper()
		code, out := l.p2m(t, "GET", "/t7-a", nil)
		if code != 200 {
			t.Fatalf("%s: GET t7-a: HTTP %d", when, code)
		}
		if lv, _ := out["task"].(map[string]any)["live"].(map[string]any); lv == nil || lv["requests"] != 2.0 {
			t.Fatalf("%s: the project's own view of the finished task: %v", when, out["task"])
		}
	}
	own("after the end")
	l.restart(t) // also a fresh public list
	own("after a restart")
	_, body = l.publicGet(t, "/v1/tasks?project=prove2me")
	if lv := liveOf(t, body, "t7-a"); lv != nil {
		t.Errorf("live after the end: %s", body)
	}
	_, qbody = l.publicGet(t, "/v1/projects/prove2me/queue")
	if strings.Contains(qbody, `"live"`) {
		t.Errorf("queue: live after the end: %s", qbody)
	}
}
