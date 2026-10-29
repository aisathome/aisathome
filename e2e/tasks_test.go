// The science task coordinator (decision 0015, phase 1): a project's tools submit tasks to the
// gateway, workers claim them with leases and report results, and the admins can read the queue.
//
// SPDX-License-Identifier: Apache-2.0
package e2e

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aisathome/aisathome/gateway"
)

func taskCall(t *testing.T, a *acctRig, method, path, key string, body any) (int, map[string]any) {
	t.Helper()
	b, _ := json.Marshal(body)
	resp := a.do(t, method, path, key, string(b))
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	return resp.StatusCode, out
}

// Four workers claim at once: every task is leased exactly once; a fix round goes first;
// a worker completes with a diff, the result is in the gateway, and only the project key or an
// admin's session can read it.
func TestTaskCoordinatorFlow(t *testing.T) {
	a := newAcctRigWith(t, 100, nil, withProjects)
	const path = "/v1/projects/tauceti/tasks"
	for i := 0; i < 20; i++ {
		kind := "attempt"
		if i%5 == 4 {
			kind = "fix_round"
		}
		task := map[string]any{"id": fmt.Sprintf("T3-X-%d.a-A", i), "kind": kind, "created_by": "owner",
			"spec": map[string]any{"attempt": fmt.Sprintf("T3-X-%d.a-A", i)}}
		if code, out := taskCall(t, a, "POST", path, projectKey, task); code != 201 {
			t.Fatalf("create %d: HTTP %d %v", i, code, out)
		}
	}
	if code, _ := taskCall(t, a, "POST", "/v1/projects/unlimited/tasks", unlimitedKey, map[string]any{"id": "x", "kind": "attempt", "created_by": "o", "spec": map[string]any{}}); code != 201 {
		t.Fatalf("the other project's own queue: HTTP %d", code)
	}
	if code, _ := taskCall(t, a, "POST", path+"/claim", unlimitedKey, map[string]any{"worker": "w"}); code != 403 {
		t.Fatalf("another project's key claimed: HTTP %d", code)
	}

	// The first claim takes the oldest fix round, though 4 attempts are older.
	code, out := taskCall(t, a, "POST", path+"/claim", projectKey, map[string]any{"worker": "runner-9"})
	if code != 200 || out["task"].(map[string]any)["id"] != "T3-X-4.a-A" {
		t.Fatalf("first claim: HTTP %d %v, want the fix round T3-X-4.a-A", code, out)
	}
	var mu sync.Mutex
	claimed := map[string]int{"T3-X-4.a-A": 1}
	var wg sync.WaitGroup
	for w := 0; w < 4; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for {
				b, _ := json.Marshal(map[string]any{"worker": fmt.Sprintf("runner-%d", w), "lease_seconds": 60})
				resp := a.do(t, "POST", path+"/claim", projectKey, string(b))
				raw, _ := io.ReadAll(resp.Body)
				resp.Body.Close()
				if resp.StatusCode == http.StatusNoContent {
					return
				}
				var out struct {
					Task struct {
						ID string
					}
				}
				if resp.StatusCode != 200 || json.Unmarshal(raw, &out) != nil {
					t.Errorf("claim: HTTP %d %s", resp.StatusCode, raw)
					return
				}
				mu.Lock()
				claimed[out.Task.ID]++
				mu.Unlock()
			}
		}(w)
	}
	wg.Wait()
	if len(claimed) != 20 {
		t.Fatalf("%d tasks claimed, want 20", len(claimed))
	}
	for id, n := range claimed {
		if n != 1 {
			t.Errorf("%s claimed %d times", id, n)
		}
	}

	// A fix round: claim, heartbeat, complete with a result.
	code, out = taskCall(t, a, "POST", path, projectKey, map[string]any{"id": "T3-Y.a-F1", "kind": "fix_round", "created_by": "owner",
		"pr": 21144, "base_ref": "a57def6", "spec": map[string]any{"attempt": "T3-Y.a-F1", "findings": []any{"rename the namespace"}}})
	if code != 201 {
		t.Fatalf("create the fix round: HTTP %d %v", code, out)
	}
	code, out = taskCall(t, a, "POST", path+"/claim", projectKey, map[string]any{"worker": "runner-0"})
	lease := out["lease"].(map[string]any)["id"].(string)
	if code != 200 || out["task"].(map[string]any)["id"] != "T3-Y.a-F1" {
		t.Fatalf("claim the fix round: HTTP %d %v", code, out)
	}
	if code, out := taskCall(t, a, "POST", path+"/T3-Y.a-F1/heartbeat", projectKey, map[string]any{"worker": "runner-0", "lease": lease}); code != 200 {
		t.Fatalf("heartbeat: HTTP %d %v", code, out)
	}
	result := map[string]any{"outcome": "candidate", "checks_passed": true, "diff": "--- a/TauCeti/Y.lean\n+++ b/TauCeti/Y.lean\n",
		"pr_body": "Renames the namespace.", "commit_msg": "fix: rename", "steps": 31, "prompt_tokens": 900000,
		"completion_tokens": 21000, "model_min": 22.5, "awake_min": 40, "raw_ref": "raw/2026-10-27/T3-Y.a-F1"}
	if code, out := taskCall(t, a, "POST", path+"/T3-Y.a-F1/complete", projectKey, map[string]any{"worker": "runner-0", "lease": lease, "result": result}); code != 200 || out["state"] != "done" {
		t.Fatalf("complete: HTTP %d %v", code, out)
	}

	admin, _ := a.signIn(t, 1) // gh:1 is in accounts.admins
	user, _ := a.signIn(t, 2)
	resp, page := a.get(t, path+"/T3-Y.a-F1", admin)
	if resp.StatusCode != 200 || !strings.Contains(page, "+++ b/TauCeti/Y.lean") || strings.Contains(page, lease) {
		t.Fatalf("admin reads the task: HTTP %d %s", resp.StatusCode, page)
	}
	resp, page = a.get(t, path+"?state=done", admin)
	if resp.StatusCode != 200 || !strings.Contains(page, "T3-Y.a-F1") || strings.Contains(page, "+++ b") {
		t.Fatalf("admin list: HTTP %d %s", resp.StatusCode, page)
	}
	for name, s := range map[string]string{"signed out": "", "not an admin": user} {
		if resp, page := a.get(t, path, s); resp.StatusCode != 401 || strings.Contains(page, "T3-") {
			t.Errorf("%s reads the queue: HTTP %d", name, resp.StatusCode)
		}
	}
	// An admin session does not write.
	req, _ := http.NewRequest("POST", a.gw.URL+path+"/T3-X-0.a-A/cancel", strings.NewReader("{}"))
	req.Header.Set("Cookie", admin)
	req.Header.Set("Content-Type", "application/json")
	if resp, err := http.DefaultClient.Do(req); err != nil || resp.StatusCode != 401 {
		t.Fatalf("admin session cancels: %v %v", err, resp.StatusCode)
	}
	// The public science endpoint does not list tasks.
	if resp, page := a.get(t, "/v1/projects/tauceti/science", ""); strings.Contains(page, "T3-Y") {
		t.Fatalf("public science shows a task: HTTP %d", resp.StatusCode)
	}
}

// Phases 2 and 3 (decision 0015): a runner gives its task back on a spot stop and takes it back
// by id to resume from its checkpoint, while a second runner gets other work; the result, the
// internal review's verdict, the owner's decision and the export marker are stored; the admins
// see the queue on /account/ops, nobody else.
func TestTaskCoordinatorLeasesReviewExport(t *testing.T) {
	a := newAcctRigWith(t, 100, nil, withProjects)
	const path = "/v1/projects/tauceti/tasks"
	for _, task := range []map[string]any{
		{"id": "T5-A.a-A", "kind": "attempt", "created_by": "owner", "spec": map[string]any{"attempt": "T5-A.a-A"}},
		{"id": "T5-B.a-F1", "kind": "fix_round", "created_by": "owner", "pr": 22500, "spec": map[string]any{"attempt": "T5-B.a-F1"}},
	} {
		if code, out := taskCall(t, a, "POST", path, projectKey, task); code != 201 {
			t.Fatalf("create: HTTP %d %v", code, out)
		}
	}
	claim := func(worker, task string) (int, string, string) {
		t.Helper()
		body := map[string]any{"worker": worker}
		if task != "" {
			body["task"] = task
		}
		code, out := taskCall(t, a, "POST", path+"/claim", projectKey, body)
		if code != 200 {
			return code, "", ""
		}
		return code, out["task"].(map[string]any)["id"].(string), out["lease"].(map[string]any)["id"].(string)
	}
	_, id, lease := claim("runner-a", "")
	if id != "T5-B.a-F1" {
		t.Fatalf("first claim %q, want the fix round", id)
	}
	// A spot stop: the runner gives the task back; the other runner does not get it (held for
	// the runner with the checkpoint) but the next task; runner-a takes its task back by id.
	if code, out := taskCall(t, a, "POST", path+"/"+id+"/fail", projectKey, map[string]any{"worker": "runner-a", "lease": lease, "requeue": true, "reason": "runner stop (spot)"}); code != 200 || out["state"] != "queued" {
		t.Fatalf("requeue: HTTP %d %v", code, out)
	}
	if _, got, _ := claim("runner-b", ""); got != "T5-A.a-A" {
		t.Fatalf("runner-b claimed %q, want T5-A.a-A", got)
	}
	if code, _, _ := claim("runner-b", "T5-B.a-F1"); code != 409 {
		t.Fatalf("runner-b claims the held task by id: HTTP %d", code)
	}
	code, id, lease := claim("runner-a", "T5-B.a-F1")
	if code != 200 || id != "T5-B.a-F1" {
		t.Fatalf("runner-a takes its task back: HTTP %d %q", code, id)
	}
	result := map[string]any{"outcome": "passes every local CI check", "checks_passed": true, "diff": "--- a/TauCeti/B.lean\n+++ b/TauCeti/B.lean\n",
		"pr_body": "Renames.", "commit_msg": "fix: rename", "steps": 20, "prompt_tokens": 500000, "completion_tokens": 9000,
		"model_min": 12.5, "awake_min": 30, "raw_ref": "raw/2026-10-29/T5-B.a-F1", "score": map[string]any{"ledger_attempt": map[string]any{"attempt": "T5-B.a-F1"}}}
	if code, out := taskCall(t, a, "POST", path+"/"+id+"/complete", projectKey, map[string]any{"worker": "runner-a", "lease": lease, "result": result}); code != 200 {
		t.Fatalf("complete: HTTP %d %v", code, out)
	}
	if code, out := taskCall(t, a, "POST", path+"/"+id+"/review", projectKey, map[string]any{"verdict": "red", "by": "owner-codex", "round": 1, "findings": 2, "summary": "2 findings"}); code != 200 {
		t.Fatalf("review: HTTP %d %v", code, out)
	}
	if code, out := taskCall(t, a, "POST", path+"/"+id+"/review", unlimitedKey, map[string]any{"verdict": "green", "by": "x"}); code != 403 {
		t.Fatalf("review with another project's key: HTTP %d %v", code, out)
	}
	if code, out := taskCall(t, a, "POST", path+"/"+id+"/decide", projectKey, map[string]any{"decision": "rejected", "by": "owner", "note": "fix round next"}); code != 200 {
		t.Fatalf("decide: HTTP %d %v", code, out)
	}
	_, out := taskCall(t, a, "GET", path+"?exported=false", projectKey, nil)
	ts := out["tasks"].([]any)
	if len(ts) != 1 || ts[0].(map[string]any)["id"] != id {
		t.Fatalf("waiting for export: %v", ts)
	}
	if code, out := taskCall(t, a, "POST", path+"/"+id+"/exported", projectKey, map[string]any{"seen": ts[0].(map[string]any)["updated_at"]}); code != 200 || out["exported_at"] == nil {
		t.Fatalf("exported: HTTP %d %v", code, out)
	}

	admin, _ := a.signIn(t, 1)
	user, _ := a.signIn(t, 2)
	resp, page := a.get(t, "/account/ops", admin)
	if resp.StatusCode != 200 {
		t.Fatalf("ops: HTTP %d", resp.StatusCode)
	}
	for _, want := range []string{`id="tasks"`, `data-task="T5-B.a-F1"`, "red (owner-codex)", "rejected", "exported",
		`data-task="T5-A.a-A"`, "<code>runner-b</code>", "1 leased, 1 done"} {
		if !strings.Contains(page, want) {
			t.Errorf("ops page lacks %q", want)
		}
	}
	if strings.Contains(page, "+++ b/TauCeti/B.lean") || strings.Contains(page, lease) {
		t.Error("the ops page shows a diff or a lease id")
	}
	if _, page := a.get(t, "/account/ops", user); strings.Contains(page, "T5-B") {
		t.Error("a normal account sees the queue")
	}
}

// Interruptions do not use up tries (owner 2026-10-29): a runner on a spot instance that is
// stopped every hour gives its task back and takes it back by id, more often than max_tries,
// and the task stays on its first try; a lease that expires is taken back by the same runner
// within the window without a new try, by another runner after it with one; a real failure ends
// the task. max_tries of an open task can be raised with the project key only.
func TestTaskInterruptionsKeepTries(t *testing.T) {
	clk := &fakeClock{t: time.Date(2026, 10, 29, 20, 0, 0, 0, time.UTC)}
	a := newAcctRigWith(t, 100, nil, func(c *gateway.Config) {
		withProjects(c)
		c.TasksNow = clk.Now
	})
	const path = "/v1/projects/tauceti/tasks"
	for _, id := range []string{"T5-NS-2.a-A", "T5-NS-3.a-A"} {
		if code, out := taskCall(t, a, "POST", path, projectKey, map[string]any{"id": id, "kind": "attempt", "created_by": "migration",
			"max_tries": 3, "spec": map[string]any{"attempt": id}}); code != 201 {
			t.Fatalf("create %s: HTTP %d %v", id, code, out)
		}
	}
	claim := func(worker, task string) (int, map[string]any, string) {
		t.Helper()
		code, out := taskCall(t, a, "POST", path+"/claim", projectKey, map[string]any{"worker": worker, "task": task})
		if code != 200 {
			return code, out, ""
		}
		return code, out["task"].(map[string]any), out["lease"].(map[string]any)["id"].(string)
	}
	get := func(id string) map[string]any {
		t.Helper()
		_, out := taskCall(t, a, "GET", path+"/"+id, projectKey, nil)
		return out["task"].(map[string]any)
	}

	// Five spot stops in five hours: given back on SIGTERM, taken back by id after the restart.
	code, task, lease := claim("ip-10-0-1-5", "T5-NS-2.a-A")
	if code != 200 || task["tries"] != 1.0 {
		t.Fatalf("first claim: HTTP %d %v", code, task)
	}
	for i := 1; i <= 5; i++ {
		for range 10 { // 50 minutes of work, a heartbeat every 5
			clk.Add(5 * time.Minute)
			if code, out := taskCall(t, a, "POST", path+"/T5-NS-2.a-A/heartbeat", projectKey, map[string]any{"worker": "ip-10-0-1-5", "lease": lease}); code != 200 {
				t.Fatalf("heartbeat: HTTP %d %v", code, out)
			}
		}
		code, out := taskCall(t, a, "POST", path+"/T5-NS-2.a-A/fail", projectKey, map[string]any{"worker": "ip-10-0-1-5", "lease": lease,
			"requeue": true, "reason": "runner stop: service stop"})
		if code != 200 || out["state"] != "queued" || out["tries"] != 1.0 {
			t.Fatalf("give-back %d: HTTP %d %v", i, code, out)
		}
		clk.Add(3 * time.Minute)
		if code, task, lease = claim("ip-10-0-1-5", "T5-NS-2.a-A"); code != 200 {
			t.Fatalf("take-back %d: HTTP %d %v", i, code, task)
		}
		if task["tries"] != 1.0 || task["interruptions"] != float64(i) || task["state"] != "leased" {
			t.Fatalf("after spot stop %d: tries %v, interruptions %v, %v", i, task["tries"], task["interruptions"], task["state"])
		}
	}

	// A stop without SIGTERM: the lease expires (10 minutes) and the runner takes the task back
	// within the window: the same try.
	clk.Add(11 * time.Minute)
	if code, _, _ := claim("runner-b", "T5-NS-2.a-A"); code != 409 { // the claim sweeps: held for the runner
		t.Fatalf("another runner takes the held task: HTTP %d", code)
	}
	clk.Add(5 * time.Minute)
	if code, task, lease = claim("ip-10-0-1-5", "T5-NS-2.a-A"); code != 200 || task["tries"] != 1.0 || task["interruptions"] != 6.0 {
		t.Fatalf("taken back after an expiry: HTTP %d %v", code, task)
	}
	// It expires again and the runner does not come back: another runner after the window
	// starts try 2.
	clk.Add(11 * time.Minute)
	if code, _, _ := claim("runner-b", "T5-NS-2.a-A"); code != 409 {
		t.Fatalf("runner-b inside the window: HTTP %d", code)
	}
	clk.Add(10 * time.Minute)
	code, task, lease = claim("runner-b", "T5-NS-2.a-A")
	if code != 200 || task["tries"] != 2.0 || task["interruptions"] != 6.0 {
		t.Fatalf("runner-b after the window: HTTP %d %v", code, task)
	}

	// max_tries: the project key raises it; another project's key, no key and an admin's
	// session cannot; out of range and below the tries started are refused.
	if code, out := taskCall(t, a, "POST", path+"/T5-NS-2.a-A/max_tries", projectKey, map[string]any{"max_tries": 6}); code != 200 || out["max_tries"] != 6.0 || out["tries"] != 2.0 {
		t.Fatalf("raise max_tries: HTTP %d %v", code, out)
	}
	for _, c := range []struct {
		key  string
		body map[string]any
		want int
	}{
		{unlimitedKey, map[string]any{"max_tries": 10}, 403},
		{"", map[string]any{"max_tries": 10}, 401},
		{projectKey, map[string]any{"max_tries": 11}, 400},
		{projectKey, map[string]any{"max_tries": 0}, 400},
		{projectKey, map[string]any{"max_tries": 1}, 409},
	} {
		if code, out := taskCall(t, a, "POST", path+"/T5-NS-2.a-A/max_tries", c.key, c.body); code != c.want {
			t.Errorf("max_tries %v (with a key: %v): HTTP %d %v, want %d", c.body, c.key != "", code, out, c.want)
		}
	}
	admin, _ := a.signIn(t, 1) // gh:1 is in accounts.admins
	req, _ := http.NewRequest("POST", a.gw.URL+path+"/T5-NS-2.a-A/max_tries", strings.NewReader(`{"max_tries": 10}`))
	req.Header.Set("Cookie", admin)
	req.Header.Set("Content-Type", "application/json")
	if resp, err := http.DefaultClient.Do(req); err != nil || resp.StatusCode != 401 {
		t.Fatalf("an admin session sets max_tries: %v %v", err, resp.StatusCode)
	}
	if got := get("T5-NS-2.a-A"); got["max_tries"] != 6.0 {
		t.Fatalf("max_tries after the refused calls: %v", got["max_tries"])
	}

	// A real failure ends the task, with tries left.
	if code, out := taskCall(t, a, "POST", path+"/T5-NS-2.a-A/fail", projectKey, map[string]any{"worker": "runner-b", "lease": lease,
		"reason": "exit 1 after 3 tries on runner-b"}); code != 200 || out["state"] != "failed" || out["tries"] != 2.0 {
		t.Fatalf("fail: HTTP %d %v", code, out)
	}
	if code, _, _ := claim("runner-b", "T5-NS-2.a-A"); code != 409 {
		t.Fatalf("a failed task was claimed: HTTP %d", code)
	}
	if code, out := taskCall(t, a, "POST", path+"/T5-NS-2.a-A/max_tries", projectKey, map[string]any{"max_tries": 10}); code != 409 {
		t.Fatalf("max_tries on a failed task: HTTP %d %v", code, out)
	}
	if got := get("T5-NS-3.a-A"); got["tries"] != 0.0 || got["state"] != "queued" {
		t.Fatalf("the other task: %v", got)
	}
}
