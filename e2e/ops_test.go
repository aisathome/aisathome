// The operations page (theme A2): a fake runner posts its status with the project key; admins
// see it at /account/ops with the gateway's own data and the health warnings; everyone else
// gets 403.
//
// SPDX-License-Identifier: Apache-2.0
package e2e

import (
	"encoding/json"
	"io"
	"strings"
	"testing"
	"time"
)

func TestOpsPageShowsAFakeRunnersStatus(t *testing.T) {
	a := newAcctRigWith(t, 100, nil, withProjects)
	admin, _ := a.signIn(t, 1) // gh:1 is in accounts.admins
	user, _ := a.signIn(t, 2)

	// One science request, so the gateway can name the node of the attempt's last step.
	resp := a.do(t, "POST", "/v1/chat/completions", projectKey, chatBody)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	rec := waitUsage(t, a.rig, 1)[0]
	u := rec["usage"].(map[string]any)
	now := time.Now().UTC()
	status := map[string]any{
		"schema": 1, "generated_at": now.Format(time.RFC3339),
		"runner": map[string]any{"paused": false, "service": "active", "free_gb": 18.2, "instance": "m7i.large spot eu-north-1c"},
		"attempts": []any{
			map[string]any{"attempt": "T2-NEW-9.a-A", "task_type": "NEW", "size": "hard", "state": "running", "steps": 12, "step_cap": 80,
				"used_min": 50, "budget_min": 60, "budget_kind": "model", "awake_min": 61, "cache_hit": 0.91, "interruptions": 1,
				"started_at": now.Add(-time.Hour).Format(time.RFC3339), "last_activity": now.Add(-time.Minute).Format(time.RFC3339),
				"last_step": map[string]any{"prompt_tokens": u["prompt_tokens"], "completion_tokens": u["completion_tokens"], "ended_at": rec["time"]}},
			map[string]any{"attempt": "T2-DOC-9.a-A", "state": "queued", "steps": 0, "used_min": 0, "awake_min": 0, "interruptions": 0,
				"added_at": now.Add(-2 * time.Hour).Format(time.RFC3339)},
		},
		"candidates":    []any{map[string]any{"attempt": "T2-NS-9.a-A", "since": now.Add(-30 * time.Hour).Format(time.RFC3339), "outcome": "passes every local CI check"}},
		"interruptions": []any{},
		"ci":            map[string]any{"pause": true, "reasons": []any{"pr-build queue: median wait 95 min"}, "since": now.Add(-4 * time.Hour).Format(time.RFC3339), "checked_at": now.Format(time.RFC3339)},
		"baselines":     map[string]any{"NEW": map[string]any{"min_per_step": 0.344, "attempts": 8}},
		"nodes":         []any{map[string]any{"node": "node-0", "contract_ends": now.Add(30 * time.Hour).Format(time.RFC3339), "usd_per_hour": 0.217}},
		"cost_today":    map[string]any{"usd": 3.21, "note": "estimate from configured rates"},
	}
	body, _ := json.Marshal(status)
	// Another project's key cannot post for tauceti; tauceti's can.
	if resp := a.do(t, "POST", "/v1/projects/tauceti/status", unlimitedKey, string(body)); resp.StatusCode != 403 {
		t.Fatalf("other project's key: HTTP %d", resp.StatusCode)
	}
	if resp := a.do(t, "POST", "/v1/projects/tauceti/status", "user-key", string(body)); resp.StatusCode != 401 {
		t.Fatalf("a user's key: HTTP %d", resp.StatusCode)
	}
	if resp := a.do(t, "POST", "/v1/projects/tauceti/status", projectKey, string(body)); resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("project key: HTTP %d %s", resp.StatusCode, b)
	}

	for name, s := range map[string]string{"signed out": "", "not an admin": user} {
		resp, page := a.get(t, "/account/ops", s)
		if resp.StatusCode != 403 || strings.Contains(page, "T2-NEW-9") || strings.Contains(page, "node-0") {
			t.Fatalf("%s: HTTP %d, page shows the status: %v", name, resp.StatusCode, strings.Contains(page, "T2-NEW-9"))
		}
	}
	resp, page := a.get(t, "/account/ops", admin)
	if resp.StatusCode != 200 {
		t.Fatalf("admin: HTTP %d", resp.StatusCode)
	}
	if cc := resp.Header.Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control %q", cc)
	}
	warnAt := strings.Index(page, `id="warnings"`)
	for _, want := range []string{
		`<meta http-equiv="refresh" content="60">`,
		`data-rule="budget"`, `data-rule="slow"`, `data-rule="ci-backoff"`, `data-rule="contract"`, `data-rule="candidate-waiting"`,
		`data-attempt="T2-NEW-9.a-A"`, "12 of 80", "50 of 60 (model)", "4.17", "91%", // min/step = 50/12
		"<code>node-0</code></td><td data-label=\"Interruptions\"", // the node of the last step
		"T2-DOC-9.a-A", "T2-NS-9.a-A", "pr-build queue: median wait 95 min", "$3.21",
		"Nodes online (1)", `data-node="node-0"`, "m7i.large spot eu-north-1c",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("page lacks %q", want)
		}
	}
	if warnAt < 0 || warnAt > strings.Index(page, `id="attempts"`) {
		t.Error("the warnings are not at the top")
	}
	// The warnings, for the runner's summary.
	resp = a.do(t, "GET", "/v1/projects/tauceti/status", projectKey, "")
	var got struct {
		Warnings []struct{ Rule string } `json:"warnings"`
	}
	json.NewDecoder(resp.Body).Decode(&got)
	resp.Body.Close()
	if len(got.Warnings) < 5 {
		t.Fatalf("GET warnings: %+v", got)
	}
}
