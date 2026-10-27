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
