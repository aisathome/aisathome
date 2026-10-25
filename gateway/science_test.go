// SPDX-License-Identifier: Apache-2.0
package gateway

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// minimalScience is a valid result set generated at now: one merged PR with two review rounds
// and one task whose attempts failed before a PR.
func minimalScience(now time.Time) map[string]any {
	ts := func(d time.Duration) string { return now.Add(d).UTC().Format(time.RFC3339) }
	return map[string]any{
		"project": "Tau Ceti", "project_url": "https://taucetiproject.org/",
		"repository": "https://github.com/TauCetiProject/TauCeti", "model": "Qwen3.8 27B",
		"generated_at": ts(-5 * time.Hour), "states_as_of": now.UTC().Format("2006-01-02"),
		"methodology": "Every pull request counts.",
		"type_labels": map[string]any{"NS": "Namespace move", "NEW": "New lemma"},
		"totals": map[string]any{
			"prs_opened": 1, "prs_merged": 1, "prs_closed": 0, "prs_open": 0, "first_review_approved": 0,
			"first_review_approval_rate": 0, "attempts": 3, "attempts_passing_local_checks": 1,
			"local_check_pass_rate": 0.333, "gpu_hours": 1.2, "attempts_without_gpu_time": 0,
			"qwen_tokens": 3000000, "qwen_output_tokens": 90000, "qwen_tokens_per_merged_pr": 1000000,
			"attempts_without_token_count": 0,
		},
		"prs": []any{map[string]any{
			"number": 19779, "url": "https://github.com/TauCetiProject/TauCeti/pull/19779",
			"title": "refactor: move subScalar to LinearPMap", "type": "Namespace move", "size": "easy",
			"state": "merged", "opened": ts(-72 * time.Hour), "merged": ts(-24 * time.Hour), "closed": nil,
			"fix_rounds": 1, "first_review_approved": false,
			"review_url": "https://github.com/TauCetiProject/TauCeti/pull/19779#issuecomment-6330287596",
			"rounds": []any{
				map[string]any{"round": 1, "commit": "1ca00527cf49", "at": ts(-70 * time.Hour), "result": "changes requested",
					"changes": []any{map[string]any{"rubric": "naming", "verdict": "request_changes",
						"reason": "`vadd_subScalar` still sits in `TauCeti.LinearPMap`.",
						"url":    "https://github.com/TauCetiProject/TauCeti/pull/19779#discussion_r4343613000"}}},
				map[string]any{"round": 2, "commit": "a884bd77bf5d", "at": ts(-30 * time.Hour), "result": "approved", "changes": []any{}},
			},
			"attempts": 2, "qwen_tokens": 2000000, "qwen_output_tokens": 60000, "gpu_minutes": 50.5,
			"attempts_without_tokens": 0, "attempts_without_gpu_time": 0,
		}},
		"tasks_without_pr": []any{map[string]any{
			"target": "The multigraph isomorphism interface Graph.Iso", "type": "New lemma", "size": "hard",
			"state": "failed", "last_attempt": ts(-10 * time.Hour), "attempts": 1, "qwen_tokens": 1000000,
			"qwen_output_tokens": 30000, "gpu_minutes": 21.5, "attempts_without_tokens": 0, "attempts_without_gpu_time": 0,
		}},
	}
}

func postScience(t *testing.T, g *Gateway, project, key string, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest("POST", "/v1/projects/"+project+"/science", bytes.NewReader(body))
	if key != "" {
		r.Header.Set("Authorization", "Bearer "+key)
	}
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	g.Handler().ServeHTTP(w, r)
	return w
}

func getScience(t *testing.T, g *Gateway, project string) (int, map[string]any, string) {
	t.Helper()
	w := httptest.NewRecorder()
	g.Handler().ServeHTTP(w, httptest.NewRequest("GET", "/v1/projects/"+project+"/science", nil))
	var got map[string]any
	json.Unmarshal(w.Body.Bytes(), &got)
	return w.Code, got, w.Body.String()
}

// Only the project's own key may post; anyone may read, without a key; the results survive a
// restart; a second post within 10 s is refused.
func TestSciencePostKeysAndPublicGet(t *testing.T) {
	dir := t.TempDir()
	g := statusGateway(t, dir)
	body := statusJSON(minimalScience(time.Now()))
	if code, _, _ := getScience(t, g, "tauceti"); code != http.StatusNotFound {
		t.Fatalf("GET before any post: HTTP %d, want 404", code)
	}
	for _, c := range []struct {
		name, key string
		want      int
	}{
		{"no key", "", http.StatusUnauthorized},
		{"wrong key", "ak-not-a-project-key-at-all-000", http.StatusUnauthorized},
		{"other project's key", otherProjectKey, http.StatusForbidden},
	} {
		if w := postScience(t, g, "tauceti", c.key, body); w.Code != c.want {
			t.Errorf("%s: HTTP %d, want %d: %s", c.name, w.Code, c.want, w.Body)
		}
	}
	if w := postScience(t, g, "tauceti", testProjectKey, body); w.Code != http.StatusOK {
		t.Fatalf("own key: HTTP %d: %s", w.Code, w.Body)
	}
	if w := postScience(t, g, "tauceti", testProjectKey, body); w.Code != http.StatusTooManyRequests {
		t.Fatalf("second post at once: HTTP %d", w.Code)
	}
	g.Close()
	if _, err := os.Stat(filepath.Join(dir, "science-tauceti.json")); err != nil {
		t.Fatal("results not kept in state_dir")
	}
	g = statusGateway(t, dir)
	defer g.Close()
	code, got, raw := getScience(t, g, "tauceti")
	if code != 200 || got["received_at"] == nil {
		t.Fatalf("public GET after a restart: HTTP %d %s", code, raw)
	}
	prs := got["prs"].([]any)
	pr := prs[0].(map[string]any)
	rounds := pr["rounds"].([]any)
	if len(prs) != 1 || pr["number"].(float64) != 19779 || len(rounds) != 2 || pr["size"] != "easy" ||
		rounds[0].(map[string]any)["changes"].([]any)[0].(map[string]any)["rubric"] != "naming" {
		t.Fatalf("results changed on the way: %s", raw)
	}
	if tw := got["tasks_without_pr"].([]any); len(tw) != 1 || tw[0].(map[string]any)["state"] != "failed" {
		t.Fatalf("tasks_without_pr: %s", raw)
	}
	if got["now"] != nil {
		t.Fatalf("now without any runner status: %v", got["now"])
	}
	// Unknown projects are 404, also for a project that exists but posted nothing.
	for _, id := range []string{"other", "nope", "..%2f"} {
		if code, _, _ := getScience(t, g, id); code != http.StatusNotFound {
			t.Errorf("GET %s: HTTP %d, want 404", id, code)
		}
	}
}

// The schema is strict and public-only: unknown fields (a spec, a prompt, an operator note),
// links that are not GitHub PRs or their comments, reviewer model names, keys, bad states and
// numbers are refused, and nothing is stored.
func TestSciencePostSchema(t *testing.T) {
	g := statusGateway(t, "")
	defer g.Close()
	now := time.Now()
	mod := func(f func(m map[string]any)) []byte {
		m := minimalScience(now)
		f(m)
		return statusJSON(m)
	}
	pr := func(m map[string]any) map[string]any { return m["prs"].([]any)[0].(map[string]any) }
	round := func(m map[string]any, i int) map[string]any { return pr(m)["rounds"].([]any)[i].(map[string]any) }
	change := func(m map[string]any) map[string]any {
		return round(m, 0)["changes"].([]any)[0].(map[string]any)
	}
	task := func(m map[string]any) map[string]any { return m["tasks_without_pr"].([]any)[0].(map[string]any) }
	cases := map[string][]byte{
		"unknown top-level field":           mod(func(m map[string]any) { m["notes"] = "operator notes" }),
		"a spec on a PR":                    mod(func(m map[string]any) { pr(m)["spec"] = "goal: move the lemmas" }),
		"a prompt on a task":                mod(func(m map[string]any) { task(m)["prompt"] = "You are ..." }),
		"an attempt id on a task":           mod(func(m map[string]any) { task(m)["attempt"] = "T2-NEW-1.a-A" }),
		"the judge on a change":             mod(func(m map[string]any) { change(m)["judge"] = "x/y" }),
		"a reviewer model in reason":        mod(func(m map[string]any) { change(m)["reason"] = "### naming `claude/claude-opus-5-5` ..." }),
		"a reviewer model, codex":           mod(func(m map[string]any) { change(m)["reason"] = "per codex/gpt-6.1-sol" }),
		"a reviewer model in a title":       mod(func(m map[string]any) { pr(m)["title"] = "Written with Claude" }),
		"a key in a target":                 mod(func(m map[string]any) { task(m)["target"] = "ghp_abcdefghijklmnop" }),
		"a change link off GitHub":          mod(func(m map[string]any) { change(m)["url"] = "https://evil.example/pull/1#discussion_r1" }),
		"a javascript link":                 mod(func(m map[string]any) { pr(m)["review_url"] = "javascript:alert(1)" }),
		"a PR link of another PR":           mod(func(m map[string]any) { pr(m)["url"] = "https://github.com/TauCetiProject/TauCeti/pull/1" }),
		"bad PR state":                      mod(func(m map[string]any) { pr(m)["state"] = "exploded" }),
		"merged without a date":             mod(func(m map[string]any) { pr(m)["merged"] = nil }),
		"bad size":                          mod(func(m map[string]any) { pr(m)["size"] = "huge" }),
		"bad task state":                    mod(func(m map[string]any) { task(m)["state"] = "running" }),
		"rounds out of order":               mod(func(m map[string]any) { round(m, 1)["round"] = 5 }),
		"bad commit":                        mod(func(m map[string]any) { round(m, 0)["commit"] = "HEAD; rm" }),
		"control characters":                mod(func(m map[string]any) { change(m)["reason"] = "a\nb" }),
		"negative tokens":                   mod(func(m map[string]any) { task(m)["qwen_tokens"] = -1 }),
		"more without tokens than attempts": mod(func(m map[string]any) { task(m)["attempts_without_tokens"] = 9 }),
		"wrong type":                        mod(func(m map[string]any) { pr(m)["fix_rounds"] = "one" }),
		"future":                            mod(func(m map[string]any) { m["generated_at"] = now.Add(time.Hour).UTC().Format(time.RFC3339) }),
		"bad day":                           mod(func(m map[string]any) { m["states_as_of"] = "yesterday" }),
		"trailing data":                     append(statusJSON(minimalScience(now)), []byte(`{}`)...),
	}
	for name, body := range cases {
		if w := postScience(t, g, "tauceti", testProjectKey, body); w.Code != http.StatusBadRequest {
			t.Errorf("%s: HTTP %d, want 400: %s", name, w.Code, w.Body)
		}
	}
	if g.psci.get("tauceti") != nil {
		t.Fatal("a refused result set was stored")
	}
	// Long Lean names and paths in a reason are fine (the status's long-run rule does not apply).
	ok := mod(func(m map[string]any) {
		change(m)["reason"] = "`TauCeti/NumberTheory/Multiquadratic/CMField/Basic.lean:163`: `card_aut_adjoin_I_sqrt_prime_of_something_long`"
	})
	if w := postScience(t, g, "tauceti", testProjectKey, ok); w.Code != http.StatusOK {
		t.Fatalf("a reason with paths: HTTP %d: %s", w.Code, w.Body)
	}
}

// "Working on now" copies only the running tasks' target, type label, size and state and the
// queued count from the runner's status: no attempt ids, outcomes, budgets, instance or cost.
// A status older than an hour is not shown.
func TestScienceNowIsAWhitelist(t *testing.T) {
	g := statusGateway(t, "")
	defer g.Close()
	now := time.Now()
	if w := postScience(t, g, "tauceti", testProjectKey, statusJSON(minimalScience(now))); w.Code != 200 {
		t.Fatalf("science: HTTP %d %s", w.Code, w.Body)
	}
	st := minimalStatus(now)
	st["attempts"] = []any{
		map[string]any{"attempt": "T2-NS-1.a-A", "title": "Move the MonoidAlgebra trace lemmas to Mathlib's MonoidAlgebra",
			"task_type": "NS", "size": "easy", "state": "running", "steps": 12, "step_cap": 80, "used_min": 9.5,
			"budget_min": 60, "budget_kind": "model", "awake_min": 14, "interruptions": 1, "outcome": "secret-ish outcome text"},
		map[string]any{"attempt": "T2-NEW-2.a-A2", "title": "Markov-kernel reachability and communication", "task_type": "NEW",
			"size": "hard", "state": "interrupted", "steps": 3, "used_min": 2, "awake_min": 2, "interruptions": 2},
		map[string]any{"attempt": "T2-GEN-2.a-A", "task_type": "GEN", "state": "queued", "steps": 0, "used_min": 0, "awake_min": 0, "interruptions": 0},
		map[string]any{"attempt": "T2-GEN-3.a-A", "task_type": "GEN", "state": "queued", "steps": 0, "used_min": 0, "awake_min": 0, "interruptions": 0},
		map[string]any{"attempt": "T2-DOC-1.a-A", "task_type": "DOC", "state": "done", "steps": 40, "used_min": 30, "awake_min": 31, "interruptions": 0, "outcome": "passes every local CI check"},
	}
	st["cost_today"] = map[string]any{"usd": 4.2, "note": "estimate"}
	if w := postStatus(t, g, "tauceti", testProjectKey, statusJSON(st)); w.Code != 200 {
		t.Fatalf("status: HTTP %d %s", w.Code, w.Body)
	}
	_, got, raw := getScience(t, g, "tauceti")
	n, _ := got["now"].(map[string]any)
	if n == nil || n["queued"].(float64) != 2 || n["reported_at"] == nil {
		t.Fatalf("now: %s", raw)
	}
	run := n["running"].([]any)
	want := []map[string]any{
		{"target": "Move the MonoidAlgebra trace lemmas to Mathlib's MonoidAlgebra", "type": "Namespace move", "size": "easy", "state": "running"},
		{"target": "Markov-kernel reachability and communication", "type": "New lemma", "size": "hard", "state": "interrupted"},
	}
	if len(run) != len(want) {
		t.Fatalf("running: %v", run)
	}
	for i, w := range want {
		r := run[i].(map[string]any)
		if len(r) != len(w) {
			t.Errorf("running[%d] has fields %v, want exactly %v", i, r, w)
		}
		for k, v := range w {
			if r[k] != v {
				t.Errorf("running[%d].%s = %v, want %v", i, k, r[k], v)
			}
		}
	}
	for _, leak := range []string{"T2-", "outcome", "secret-ish", "m7i", "cost", "budget", "steps", "interruptions", "candidates"} {
		if strings.Contains(raw, leak) {
			t.Errorf("the public JSON contains %q: %s", leak, raw)
		}
	}
	// A title that names a reviewer model is refused with the status.
	st["generated_at"] = now.Add(time.Second).UTC().Format(time.RFC3339)
	st["attempts"].([]any)[0].(map[string]any)["title"] = "Ask Claude"
	g.pstatus.byID["tauceti"].ReceivedAt = now.Add(-time.Minute)
	if w := postStatus(t, g, "tauceti", testProjectKey, statusJSON(st)); w.Code != http.StatusBadRequest {
		t.Errorf("a title naming a model: HTTP %d", w.Code)
	}
	// An hour-old status is not "now".
	if publicNow(g.pstatus.get("tauceti"), nil, now.Add(2*time.Hour)) != nil {
		t.Error("a two-hour-old status is shown as now")
	}
}

// Public GETs for ids that are not projects are refused before any lookup and never cached: many
// distinct unknown ids leave the results cache as it was (finding 2026-10-25: every miss was
// kept, so anyone could grow the gateway's memory without bound).
func TestScienceGetUnknownIDsDoNotGrowTheCache(t *testing.T) {
	g := statusGateway(t, t.TempDir())
	defer g.Close()
	if w := postScience(t, g, "tauceti", testProjectKey, statusJSON(minimalScience(time.Now()))); w.Code != 200 {
		t.Fatalf("science: HTTP %d %s", w.Code, w.Body)
	}
	size := func() int {
		g.psci.mu.Lock()
		defer g.psci.mu.Unlock()
		return len(g.psci.byID)
	}
	before := size()
	for i := 0; i < 500; i++ {
		if code, _, _ := getScience(t, g, fmt.Sprintf("nope-%d", i)); code != http.StatusNotFound {
			t.Fatalf("GET nope-%d: HTTP %d, want 404", i, code)
		}
	}
	// A configured project without results is a miss too, and not kept either.
	for i := 0; i < 3; i++ {
		if code, _, _ := getScience(t, g, "other"); code != http.StatusNotFound {
			t.Fatalf("GET other: HTTP %d, want 404", code)
		}
	}
	if after := size(); after != before {
		t.Fatalf("the results cache grew from %d to %d entries on 404s", before, after)
	}
	if g.psci.get("never-configured") != nil || size() != before {
		t.Fatal("a miss of the store itself was cached")
	}
}

// GET follows the live project table: a project added by a reload can post and be read; a
// project removed by a reload is 404 although its results are still stored.
func TestScienceGetFollowsReload(t *testing.T) {
	const newKey = "ak-newproj-test-key-0123456789"
	g := statusGateway(t, "")
	defer g.Close()
	body := statusJSON(minimalScience(time.Now()))
	if w := postScience(t, g, "tauceti", testProjectKey, body); w.Code != 200 {
		t.Fatalf("tauceti: HTTP %d %s", w.Code, w.Body)
	}
	if err := g.Reload(Config{Projects: []Project{
		{ID: "other", KeySHA256: []string{keyHash(otherProjectKey)}},
		{ID: "newproj", KeySHA256: []string{keyHash(newKey)}},
	}}); err != nil {
		t.Fatal(err)
	}
	if w := postScience(t, g, "newproj", newKey, body); w.Code != 200 {
		t.Fatalf("a project added by a reload: POST HTTP %d %s", w.Code, w.Body)
	}
	if code, got, raw := getScience(t, g, "newproj"); code != 200 || got["prs"] == nil {
		t.Fatalf("a project added by a reload: GET HTTP %d %s", code, raw)
	}
	if code, _, raw := getScience(t, g, "tauceti"); code != http.StatusNotFound {
		t.Fatalf("a project removed by a reload: GET HTTP %d %s, want 404", code, raw)
	}
}

// A PR without a review yet has first_review_approved null (not false: no "changes asked"
// before any review); true and false still round-trip, other values are refused.
func TestScienceFirstReviewIsTriState(t *testing.T) {
	g := statusGateway(t, "")
	defer g.Close()
	now := time.Now()
	m := minimalScience(now)
	open := map[string]any{
		"number": 21144, "url": "https://github.com/TauCetiProject/TauCeti/pull/21144",
		"title": "refactor: prove the generator lemma once", "type": "Generalisation", "size": "medium",
		"state": "open", "opened": now.Add(-48 * time.Hour).UTC().Format(time.RFC3339), "merged": nil, "closed": nil,
		"fix_rounds": 0, "first_review_approved": nil, "review_url": nil, "rounds": []any{},
		"attempts": 1, "qwen_tokens": nil, "qwen_output_tokens": nil, "gpu_minutes": nil,
		"attempts_without_tokens": 1, "attempts_without_gpu_time": 1,
	}
	m["prs"] = append(m["prs"].([]any), open)
	bad := minimalScience(now)
	bad["prs"].([]any)[0].(map[string]any)["first_review_approved"] = "pending"
	if w := postScience(t, g, "tauceti", testProjectKey, statusJSON(bad)); w.Code != http.StatusBadRequest {
		t.Fatalf("a string first_review_approved: HTTP %d", w.Code)
	}
	if w := postScience(t, g, "tauceti", testProjectKey, statusJSON(m)); w.Code != 200 {
		t.Fatalf("HTTP %d %s", w.Code, w.Body)
	}
	_, got, raw := getScience(t, g, "tauceti")
	prs := got["prs"].([]any)
	if v, ok := prs[1].(map[string]any)["first_review_approved"]; !ok || v != nil {
		t.Fatalf("no review yet: first_review_approved = %v (present %v), want null: %s", v, ok, raw)
	}
	if v := prs[0].(map[string]any)["first_review_approved"]; v != false {
		t.Fatalf("changes asked at the first review: %v, want false", v)
	}
}

// "Working on now" names a fix round's pull request (number, a GitHub link into the results'
// repository, its title from the results) and lists the first ten queued tasks (target, type
// label, size, and the PR of a fix round) with the full count; a status's pr is a number only,
// and a fix round needs one.
func TestScienceNowFixRoundsAndQueue(t *testing.T) {
	g := statusGateway(t, "")
	defer g.Close()
	now := time.Now()
	if w := postScience(t, g, "tauceti", testProjectKey, statusJSON(minimalScience(now))); w.Code != 200 {
		t.Fatalf("science: HTTP %d %s", w.Code, w.Body)
	}
	att := func(id, state string, kv ...any) map[string]any {
		a := map[string]any{"attempt": id, "state": state, "steps": 0, "used_min": 0, "awake_min": 0, "interruptions": 0}
		for i := 0; i < len(kv); i += 2 {
			a[kv[i].(string)] = kv[i+1]
		}
		return a
	}
	atts := []any{att("G3b.a-F2", "running", "title", "Move subScalar to LinearPMap", "task_type", "NS", "size", "easy", "pr", 19779, "fix_round", 2)}
	atts = append(atts, att("G1a.a-F5", "queued", "title", "Class number of isomorphic fields", "task_type", "NEW", "size", "hard", "pr", 19781, "fix_round", 5))
	for i := 0; i < 11; i++ {
		atts = append(atts, att(fmt.Sprintf("T2-GEN-%d.a-A", i), "queued", "title", fmt.Sprintf("Generalise lemma %d", i), "task_type", "GEN", "size", "medium"))
	}
	st := minimalStatus(now)
	st["attempts"] = atts
	if w := postStatus(t, g, "tauceti", testProjectKey, statusJSON(st)); w.Code != 200 {
		t.Fatalf("status: HTTP %d %s", w.Code, w.Body)
	}
	_, got, raw := getScience(t, g, "tauceti")
	n := got["now"].(map[string]any)
	run := n["running"].([]any)[0].(map[string]any)
	pr, _ := run["pr"].(map[string]any)
	if run["fix_round"] != float64(2) || pr == nil || pr["number"] != float64(19779) ||
		pr["url"] != "https://github.com/TauCetiProject/TauCeti/pull/19779" || pr["title"] != "refactor: move subScalar to LinearPMap" {
		t.Fatalf("running fix round: %s", raw)
	}
	queue := n["queue"].([]any)
	if n["queued"] != float64(12) || len(queue) != nowQueueMax {
		t.Fatalf("queued %v, queue of %d, want 12 and %d: %s", n["queued"], len(queue), nowQueueMax, raw)
	}
	q0 := queue[0].(map[string]any)
	qpr, _ := q0["pr"].(map[string]any)
	if q0["target"] != "Class number of isomorphic fields" || q0["type"] != "New lemma" || q0["size"] != "hard" ||
		q0["fix_round"] != float64(5) || qpr == nil || qpr["url"] != "https://github.com/TauCetiProject/TauCeti/pull/19781" {
		t.Fatalf("queued fix round: %v", q0)
	}
	if _, ok := qpr["title"]; ok {
		t.Errorf("a PR the results do not list got a title: %v", qpr)
	}
	if q1 := queue[1].(map[string]any); q1["target"] != "Generalise lemma 0" || q1["pr"] != nil || q1["state"] != nil || len(q1) != 3 {
		t.Errorf("queued task: %v, want exactly target, type and size", q1)
	}
	for _, leak := range []string{"G3b", "G1a", "T2-", "steps", "used_min"} {
		if strings.Contains(raw, leak) {
			t.Errorf("the public JSON contains %q", leak)
		}
	}
	// pr is a number in range; a fix round needs one; a URL is not a number.
	for name, f := range map[string]func(a map[string]any){
		"negative pr":            func(a map[string]any) { a["pr"] = -1 },
		"pr out of range":        func(a map[string]any) { a["pr"] = 10000000000 },
		"fix round without pr":   func(a map[string]any) { delete(a, "pr") },
		"a URL as pr":            func(a map[string]any) { a["pr"] = "https://evil.example/pull/1" },
		"fix round out of range": func(a map[string]any) { a["fix_round"] = 101 },
	} {
		b := minimalStatus(now)
		a := att("G3b.a-F2", "running", "pr", 19779, "fix_round", 2)
		f(a)
		b["attempts"] = []any{a}
		g.pstatus.byID["tauceti"].ReceivedAt = now.Add(-time.Minute)
		if w := postStatus(t, g, "tauceti", testProjectKey, statusJSON(b)); w.Code != http.StatusBadRequest {
			t.Errorf("%s: HTTP %d, want 400", name, w.Code)
		}
	}
}
