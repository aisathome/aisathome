// The science task coordinator (decision 0015, phase 1): a project's tools submit tasks to the
// gateway, workers claim them with leases and report results, and the admins can read the queue.
//
// SPDX-License-Identifier: Apache-2.0
package e2e

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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

// The review loop through the real server: a passing candidate gets its review from the gateway,
// behind fixes and before first attempts; two runners working the queue never hold one task; a spot
// stop keeps the review's try; red schedules a fix round at class 3, the limit stops at the
// operator, and the ops card shows it. Nothing here opens a PR: there is no way to.
func TestTaskReviewLoopFlow(t *testing.T) {
	a := newAcctRigWith(t, 100, nil, func(c *gateway.Config) {
		withProjects(c)
		c.Projects[0].ReviewLoop, c.Projects[0].MaxReviewRounds = true, 1
	})
	const path = "/v1/projects/tauceti/tasks"
	create := func(task map[string]any) {
		t.Helper()
		task["created_by"] = "owner"
		if task["spec"] == nil {
			task["spec"] = map[string]any{"attempt": task["id"]}
		}
		if code, out := taskCall(t, a, "POST", path, projectKey, task); code != 201 {
			t.Fatalf("create %v: HTTP %d %v", task["id"], code, out)
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
	complete := func(worker, id, lease string, result map[string]any) {
		t.Helper()
		if code, out := taskCall(t, a, "POST", path+"/"+id+"/complete", projectKey, map[string]any{"worker": worker, "lease": lease, "result": result}); code != 200 {
			t.Fatalf("complete %s: HTTP %d %v", id, code, out)
		}
	}
	get := func(id string) (map[string]any, map[string]any) {
		t.Helper()
		_, out := taskCall(t, a, "GET", path+"/"+id, projectKey, nil)
		task, _ := out["task"].(map[string]any)
		ch, _ := out["chain"].(map[string]any)
		return task, ch
	}
	passed := map[string]any{"outcome": "passes every local CI check", "checks_passed": true, "diff": "--- a/X.lean\n+++ b/X.lean\n", "model_min": 20}
	create(map[string]any{"id": "T6-C.a-A", "kind": "attempt"})
	_, id, lease := claim("runner-a", "")
	complete("runner-a", id, lease, passed)

	create(map[string]any{"id": "T6-D.a-A", "kind": "attempt"})
	create(map[string]any{"id": "T6-E.a-P1", "kind": "fix_round", "findings_source": "prereview-qwen"})
	create(map[string]any{"id": "T6-F.a-P1", "kind": "fix_round", "findings_source": "prereview"})
	create(map[string]any{"id": "T6-G.a-F1", "kind": "fix_round", "findings_source": "review", "pr": 22600})

	// Two runners at once: each task goes to one of them.
	var mu sync.Mutex
	var wg sync.WaitGroup
	claimed, leases := map[string]string{}, map[string]string{}
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func(w string) {
			defer wg.Done()
			_, got, l := claim(w, "")
			if got == "" {
				return
			}
			mu.Lock()
			defer mu.Unlock()
			if prev, dup := claimed[got]; dup {
				t.Errorf("%s claimed by %s and %s", got, prev, w)
			}
			claimed[got], leases[got] = w, l
		}([]string{"runner-a", "runner-b"}[i%2])
	}
	wg.Wait()
	const rv = "T6-C.a-A.review"
	if len(claimed) != 5 || claimed[rv] == "" {
		t.Fatalf("claimed %v", claimed)
	}
	// A spot stop: the review is given back and taken back on the same try.
	w := claimed[rv]
	if code, out := taskCall(t, a, "POST", path+"/"+rv+"/fail", projectKey, map[string]any{"worker": w, "lease": leases[rv], "requeue": true, "reason": "runner stop: spot"}); code != 200 || out["tries"] != float64(1) {
		t.Fatalf("give back: HTTP %d %v", code, out)
	}
	code, _, lease := claim(w, rv)
	if code != 200 {
		t.Fatalf("take back: HTTP %d", code)
	}
	if tk, _ := get(rv); tk["tries"] != float64(1) || tk["interruptions"] != float64(1) || tk["class"] != float64(4) || tk["created_by"] != "gateway" {
		t.Fatalf("after the take-back: %v", tk)
	}
	red := map[string]any{"verdict": "red", "by": "qwen-judge", "judge": "qwen", "findings": 1, "summary": "naming"}
	fix := func(id string) map[string]any {
		return map[string]any{"id": id, "kind": "fix_round", "findings_source": "prereview-qwen", "created_by": "runner", "spec": map[string]any{"attempt": id}}
	}
	complete(w, rv, lease, map[string]any{"outcome": "qwen review: findings", "model_min": 22.5, "completion_tokens": 4000, "review": red, "fix": fix("T6-C.a-P1")})
	parent, _ := get("T6-C.a-A")
	if parent["stage"] != "fix-scheduled" || parent["qwen_review"].(map[string]any)["verdict"] != "red" || parent["review"] != nil {
		t.Fatalf("parent: %v", parent)
	}
	round, ch := get("T6-C.a-P1")
	if round["class"] != float64(3) || round["created_by"] != "gateway" || ch["auto_rounds"] != float64(1) {
		t.Fatalf("the round: %v, chain %v", round, ch)
	}
	// The round passes, and red again is the limit (1 here): the operator's.
	_, _, lease = claim("runner-b", "T6-C.a-P1")
	complete("runner-b", "T6-C.a-P1", lease, passed)
	_, _, lease = claim("runner-b", "T6-C.a-P1.review")
	complete("runner-b", "T6-C.a-P1.review", lease, map[string]any{"outcome": "qwen review: findings", "review": red, "fix": fix("T6-C.a-P2")})
	if tk, _ := get("T6-C.a-P2"); tk != nil {
		t.Fatalf("a round past the limit: %v", tk)
	}
	cand, ch := get("T6-C.a-P1")
	if cand["stage"] != "needs-operator" || ch["reviews"] != float64(2) || ch["auto_rounds"] != float64(1) {
		t.Fatalf("at the limit: %v, chain %v", cand, ch)
	}
	admin, _ := a.signIn(t, 1)
	_, page := a.get(t, "/account/ops", admin)
	for _, want := range []string{"qwen red (qwen-judge)", `data-task="T6-C.a-P1.review"`, "needs-operator: 2 reviews, 1 of 1 rounds", "limit of 1"} {
		if !strings.Contains(page, want) {
			t.Errorf("ops page lacks %q", want)
		}
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

// loopGateway is a gateway with the review loop on and a state dir, which a test can restart.
type loopGateway struct {
	cfg gateway.Config
	g   *gateway.Gateway
	url string
	srv *httptest.Server
}

func newLoopGateway(t *testing.T, rounds int) *loopGateway {
	t.Helper()
	l := &loopGateway{}
	r := newRigCfg(t, func(c *gateway.Config) {
		c.StateDir = t.TempDir()
		withProjects(c)
		c.Projects[0].ReviewLoop, c.Projects[0].MaxReviewRounds = true, rounds
		l.cfg = *c
	}, mockEngine(t, false))
	l.g, l.srv, l.url = r.g, r.gw, r.gw.URL
	return l
}

func (l *loopGateway) restart(t *testing.T) {
	t.Helper()
	l.srv.Close()
	if err := l.g.Close(); err != nil {
		t.Fatal(err)
	}
	g, err := gateway.New(l.cfg, quiet())
	if err != nil {
		t.Fatal(err)
	}
	l.g, l.srv = g, httptest.NewServer(g.Handler())
	l.url = l.srv.URL
	t.Cleanup(func() { l.srv.Close(); l.g.Close() })
}

func (l *loopGateway) call(t *testing.T, method, path string, body any) (int, map[string]any) {
	t.Helper()
	raw, _ := json.Marshal(body)
	req, _ := http.NewRequest(method, l.url+"/v1/projects/tauceti/tasks"+path, bytes.NewReader(raw))
	req.Header.Set("Authorization", "Bearer "+projectKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func (l *loopGateway) create(t *testing.T, id, kind string) {
	t.Helper()
	if code, out := l.call(t, "POST", "", map[string]any{"id": id, "kind": kind, "created_by": "owner", "spec": map[string]any{"attempt": id}}); code != 201 {
		t.Fatalf("create %s: HTTP %d %v", id, code, out)
	}
}

func (l *loopGateway) claim(t *testing.T, id string) string {
	t.Helper()
	code, out := l.call(t, "POST", "/claim", map[string]any{"worker": "runner-a", "task": id})
	if code != 200 {
		t.Fatalf("claim %s: HTTP %d %v", id, code, out)
	}
	return out["lease"].(map[string]any)["id"].(string)
}

func (l *loopGateway) complete(t *testing.T, id, lease string, result map[string]any) {
	t.Helper()
	if code, out := l.call(t, "POST", "/"+id+"/complete", map[string]any{"worker": "runner-a", "lease": lease, "result": result}); code != 200 {
		t.Fatalf("complete %s: HTTP %d %v", id, code, out)
	}
}

func (l *loopGateway) get(t *testing.T, id string) (map[string]any, map[string]any) {
	t.Helper()
	code, out := l.call(t, "GET", "/"+id, nil)
	if code != 200 {
		return nil, nil
	}
	ch, _ := out["chain"].(map[string]any)
	return out["task"].(map[string]any), ch
}

var (
	loopPassed = map[string]any{"outcome": "passes every local CI check", "checks_passed": true, "diff": "--- a/X.lean\n+++ b/X.lean\n", "model_min": 20}
	loopRed    = map[string]any{"verdict": "red", "by": "qwen-judge", "judge": "qwen", "findings": 1, "summary": "naming"}
)

func loopFix(id string, spec map[string]any) map[string]any {
	return map[string]any{"id": id, "kind": "fix_round", "findings_source": "prereview-qwen", "created_by": "runner", "spec": spec}
}

// A gateway that stopped between saving a finished review and scheduling its round: the file it left
// has the review done and the round owed. The restart makes the round once, and the worker's retry
// of the report, which never got an answer, changes nothing.
func TestTaskLoopCrashBetweenSaveAndRound(t *testing.T) {
	l := newLoopGateway(t, 3)
	const cand, rv, round = "T7-C.a-A", "T7-C.a-A.review", "T7-C.a-P1"
	l.create(t, cand, "attempt")
	l.complete(t, cand, l.claim(t, cand), loopPassed)
	lease := l.claim(t, rv)
	fix := loopFix(round, map[string]any{"attempt": round})
	l.srv.Close()
	if err := l.g.Close(); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(l.cfg.StateDir, "tasks-tauceti", rv+".json")
	raw, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	var task map[string]any
	if err := json.Unmarshal(raw, &task); err != nil {
		t.Fatal(err)
	}
	task["state"] = "done"
	task["result"] = map[string]any{"outcome": "qwen review: findings", "model_min": 22.5, "steps": 0, "prompt_tokens": 0, "completion_tokens": 0, "awake_min": 0,
		"finished_at": time.Now().UTC()}
	task["review"] = map[string]any{"verdict": "red", "by": "qwen-judge", "judge": "qwen", "at": time.Now().UTC()}
	task["loop_pending"] = map[string]any{"step": "done", "fix": fix}
	raw, _ = json.Marshal(task)
	if err := os.WriteFile(file, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	g, err := gateway.New(l.cfg, quiet())
	if err != nil {
		t.Fatal(err)
	}
	l.g, l.srv = g, httptest.NewServer(g.Handler())
	l.url = l.srv.URL
	t.Cleanup(func() { l.srv.Close(); l.g.Close() })

	check := func(when string) {
		t.Helper()
		n, ch := l.get(t, round)
		if n == nil || n["created_by"] != "gateway" || n["round_kind"] != "qwen-review" || ch["auto_rounds"] != float64(1) || ch["reviews"] != float64(1) {
			t.Fatalf("%s: round %v, chain %v", when, n, ch)
		}
		if p, _ := l.get(t, cand); p["stage"] != "fix-scheduled" || p["qwen_review"] == nil {
			t.Fatalf("%s: candidate %v", when, p)
		}
		if _, out := l.call(t, "GET", "?state=queued", nil); len(out["tasks"].([]any)) != 1 {
			t.Fatalf("%s: queue %v", when, out["tasks"])
		}
	}
	check("after the restart")
	if code, out := l.call(t, "POST", "/"+rv+"/complete", map[string]any{"worker": "runner-a", "lease": lease,
		"result": map[string]any{"outcome": "qwen review: findings", "review": loopRed, "fix": fix}}); code != 200 {
		t.Fatalf("the retry: HTTP %d %v", code, out)
	}
	check("after the retry")
	l.restart(t)
	check("after another restart")
}

// The pruning leaves a candidate whose review is still queued, however many tasks finish after it.
func TestTaskPruneKeepsTheReviewedCandidate(t *testing.T) {
	l := newLoopGateway(t, 3)
	const cand, rv = "T7-P.a-A", "T7-P.a-A.review"
	l.create(t, cand, "attempt")
	l.complete(t, cand, l.claim(t, cand), loopPassed)
	exported := func(id string) {
		t.Helper()
		tk, _ := l.get(t, id)
		if code, out := l.call(t, "POST", "/"+id+"/exported", map[string]any{"seen": tk["updated_at"]}); code != 200 {
			t.Fatalf("exported %s: HTTP %d %v", id, code, out)
		}
	}
	exported(cand)
	for i := 0; i < 205; i++ {
		id := fmt.Sprintf("O-%d", i)
		l.create(t, id, "other")
		if code, _ := l.call(t, "POST", "/"+id+"/cancel", map[string]any{}); code != 200 {
			t.Fatalf("cancel: HTTP %d", code)
		}
		exported(id)
	}
	if tk, _ := l.get(t, "O-0"); tk != nil {
		t.Fatalf("nothing was pruned: %v", tk)
	}
	l.complete(t, rv, l.claim(t, rv), map[string]any{"outcome": "qwen review: green", "review": map[string]any{"verdict": "green", "by": "qwen-judge", "judge": "qwen"}})
	if p, ch := l.get(t, cand); p == nil || p["stage"] != "ready-for-operator" || ch["status"] != "ready-for-operator" {
		t.Fatalf("candidate %v, chain %v", p, ch)
	}
}

// A round past the limit that a create cannot carry: it waits in the gateway, the operator continues
// it with a note, and it counts as the operator's, once.
func TestTaskContinueBigWaitingRound(t *testing.T) {
	l := newLoopGateway(t, 1)
	const cand, round, next = "T7-W.a-A", "T7-W.a-P1", "T7-W.a-P2"
	l.create(t, cand, "attempt")
	l.complete(t, cand, l.claim(t, cand), loopPassed)
	l.complete(t, cand+".review", l.claim(t, cand+".review"), map[string]any{"outcome": "findings", "review": loopRed, "fix": loopFix(round, map[string]any{"attempt": round})})
	l.complete(t, round, l.claim(t, round), loopPassed)
	big := map[string]any{"attempt": next, "files": map[string]any{"tasks/tree_patches/x.diff": strings.Repeat("x", 300<<10)},
		"ledger": map[string]any{"subtask": map[string]any{"fix_rounds": map[string]any{"P2": map[string]any{"findings": "verbatim"}}}}}
	const rv = round + ".review"
	l.complete(t, rv, l.claim(t, rv), map[string]any{"outcome": "findings", "review": loopRed, "fix": loopFix(next, big)})
	if p, ch := l.get(t, round); p["stage"] != "needs-operator" || ch["auto_rounds"] != float64(1) {
		t.Fatalf("at the limit: %v, chain %v", p, ch)
	}
	waiting, _ := l.get(t, rv)
	create := waiting["next_fix"].(map[string]any)
	create["created_by"] = "owner"
	if code, _ := l.call(t, "POST", "", create); code != http.StatusRequestEntityTooLarge {
		t.Fatalf("a create of the recipe: HTTP %d", code)
	}
	body := map[string]any{"created_by": "owner", "note": "`A.lean` imports `B`."}
	if code, out := l.call(t, "POST", "/"+rv+"/continue", body); code != 201 || out["created"] != true {
		t.Fatalf("continue: HTTP %d %v", code, out)
	}
	if code, out := l.call(t, "POST", "/"+rv+"/continue", body); code != 200 || out["created"] != false {
		t.Fatalf("again: HTTP %d %v", code, out)
	}
	l.restart(t)
	n, ch := l.get(t, next)
	if n == nil || n["created_by"] != "owner" || n["class"] != float64(3) || ch["auto_rounds"] != float64(1) || ch["operator_rounds"] != float64(1) || ch["status"] != "operator-round" {
		t.Fatalf("the round: %v, chain %v", n, ch)
	}
	fx := n["spec"].(map[string]any)["ledger"].(map[string]any)["subtask"].(map[string]any)["fix_rounds"].(map[string]any)["P2"].(map[string]any)
	if fx["operator_note"] != "`A.lean` imports `B`." || fx["findings"] != "verbatim" {
		t.Fatalf("the fix-round entry: %v", fx)
	}
	code, out := l.call(t, "POST", "/claim", map[string]any{"worker": "runner-a"})
	if got := out["task"].(map[string]any); code != 200 || got["id"] != next || len(got["spec"].(map[string]any)["files"].(map[string]any)["tasks/tree_patches/x.diff"].(string)) != 300<<10 {
		t.Fatalf("claim: HTTP %d", code)
	}
}

// A task id without a hyphen: its review stays in its chain across a restart, so the counts stay one.
func TestTaskChainOfAPlainIDSurvivesRestart(t *testing.T) {
	l := newLoopGateway(t, 3)
	l.create(t, "plain", "attempt")
	l.complete(t, "plain", l.claim(t, "plain"), loopPassed)
	l.restart(t)
	if rv, _ := l.get(t, "plain.review"); rv["chain"] != "plain" {
		t.Fatalf("the review: %v", rv)
	}
	l.complete(t, "plain.review", l.claim(t, "plain.review"), map[string]any{"outcome": "findings", "model_min": 5, "review": loopRed,
		"fix": loopFix("plain-P1", map[string]any{"attempt": "plain-P1"})})
	rv, ch := l.get(t, "plain.review")
	if ch == nil || ch["id"] != "plain" || ch["reviews"] != float64(1) || ch["auto_rounds"] != float64(1) || rv["chain"] != "plain" {
		t.Fatalf("chain %v, review %v", ch, rv)
	}
	if n, _ := l.get(t, "plain-P1"); n == nil {
		t.Fatal("no round: the chain split")
	}
	ents, _ := os.ReadDir(filepath.Join(l.cfg.StateDir, "task-chains-tauceti"))
	if len(ents) != 1 {
		t.Fatalf("%d chain files", len(ents))
	}
}

// A real read-only directory, through the HTTP API and a restart: with its chain unwritable a red
// verdict is saved and answered, but no round exists and no work goes out; with the task directory
// unwritable too, a create is refused. When the directories are writable again the round comes by
// itself, counted once, and the limit of 1 holds.
func TestTaskQueueOnAReadOnlyDisk(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory modes")
	}
	l := newLoopGateway(t, 1)
	const cand, rv, round, next = "T7-R.a-A", "T7-R.a-A.review", "T7-R.a-P1", "T7-R.a-P2"
	l.create(t, cand, "attempt")
	l.complete(t, cand, l.claim(t, cand), loopPassed)
	lease := l.claim(t, rv)
	mode := func(dir string, m os.FileMode) {
		t.Helper()
		if err := os.Chmod(filepath.Join(l.cfg.StateDir, dir), m); err != nil {
			t.Fatal(err)
		}
	}
	const chains, tasks = "task-chains-tauceti", "tasks-tauceti"
	t.Cleanup(func() {
		os.Chmod(filepath.Join(l.cfg.StateDir, chains), 0o700)
		os.Chmod(filepath.Join(l.cfg.StateDir, tasks), 0o700)
	})
	held := func(when string) {
		t.Helper()
		if n, _ := l.get(t, round); n != nil {
			t.Fatalf("%s: a round without its count: %v", when, n)
		}
		if code, _ := l.call(t, "POST", "/claim", map[string]any{"worker": "runner-b"}); code != 503 {
			t.Fatalf("%s: claim: HTTP %d", when, code)
		}
	}
	mode(chains, 0o500)
	l.complete(t, rv, lease, map[string]any{"outcome": "qwen review: findings", "review": loopRed,
		"fix": loopFix(round, map[string]any{"attempt": round})})
	held("after the verdict")
	mode(tasks, 0o500)
	if code, out := l.call(t, "POST", "", map[string]any{"id": "T8.a-A", "kind": "attempt", "created_by": "owner", "spec": map[string]any{}}); code != 503 {
		t.Fatalf("create on a read-only queue: HTTP %d %v", code, out)
	}
	mode(tasks, 0o700)
	l.restart(t)
	held("after a restart")

	mode(chains, 0o700)
	n, ch := l.get(t, round)
	if n == nil || ch["auto_rounds"] != float64(1) || ch["reviews"] != float64(1) {
		t.Fatalf("after the disk came back: round %v, chain %v", n, ch)
	}
	l.restart(t)
	if _, ch := l.get(t, round); ch["auto_rounds"] != float64(1) {
		t.Fatalf("after another restart: chain %v", ch)
	}
	l.complete(t, round, l.claim(t, round), map[string]any{"outcome": "fails", "checks_passed": false, "model_min": 5,
		"fix": loopFix(next, map[string]any{"attempt": next})})
	if n, _ := l.get(t, next); n != nil {
		t.Fatalf("a second round past the limit of 1: %v", n)
	}
}
