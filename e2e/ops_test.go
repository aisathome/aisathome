// The operations page (theme A2): a fake runner posts its status with the project key; admins
// see it at /account/ops with the gateway's own data and the health warnings; everyone else
// gets 403.
//
// SPDX-License-Identifier: Apache-2.0
package e2e

import (
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/aisathome/aisathome/gateway"
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
	// The account page links to the operations page for admins only (owner 2026-10-24: "how do I
	// navigate").
	if _, acc := a.get(t, "/account", admin); !strings.Contains(acc, `href="/account/ops"`) {
		t.Errorf("the admin's account page has no link to /account/ops")
	}
	if _, acc := a.get(t, "/account", user); strings.Contains(acc, "/account/ops") {
		t.Errorf("a normal account's page links to /account/ops")
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

// A hold shows on the page with its rule and time, the node connected or gone, and in the
// warnings; the node leaves the count online. Clearing it takes all of that away.
func TestOpsPageShowsHolds(t *testing.T) {
	a := newAcctRigWith(t, 100, nil, withProjects)
	admin, _ := a.signIn(t, 1)
	at := time.Date(2026, 10, 31, 22, 13, 37, 0, time.UTC)
	for _, id := range []string{"node-0", "n-0ff11ce0"} {
		if err := a.store.HoldNode(gateway.NodeHold{Node: id, Operator: "op", Rule: "file-upload", At: at}); err != nil {
			t.Fatal(err)
		}
	}
	var page string
	waitFor(t, func() bool {
		_, page = a.get(t, "/account/ops", admin)
		return strings.Contains(page, "Nodes online (0) · 1 held")
	})
	for _, want := range []string{`id="holds"`, "Held nodes (2)", `data-hold="node-0"`, `data-hold="n-0ff11ce0"`, `data-rule="node-held"`,
		"since 10-31 22:13 UTC (rule file-upload)", `<span class="badge offline">offline</span>`, "file-upload", "10-31 22:13 UTC"} {
		if !strings.Contains(page, want) {
			t.Fatalf("the page lacks %q: %s", want, page)
		}
	}
	if strings.Count(page, `<span class="badge offline">offline</span>`) != 1 {
		t.Fatalf("only the node that left is offline: %s", page)
	}
	if !strings.Contains(page, `data-rule="no-nodes"`) { // the only node is held
		t.Fatalf("every node is held and no warning says so: %s", page)
	}
	var st struct {
		Nodes map[string]int `json:"nodes"`
	}
	resp := a.do(t, "GET", "/v1/stats", "", "")
	json.NewDecoder(resp.Body).Decode(&st)
	resp.Body.Close()
	if st.Nodes["online"] != 0 || st.Nodes["held"] != 1 || st.Nodes["slots"] != 0 {
		t.Fatalf("stats %v", st.Nodes)
	}
	for _, id := range []string{"node-0", "n-0ff11ce0"} {
		if err := a.store.ClearNode(id); err != nil {
			t.Fatal(err)
		}
	}
	waitFor(t, func() bool {
		_, page = a.get(t, "/account/ops", admin)
		return !strings.Contains(page, `id="holds"`) && !strings.Contains(page, "node-held") && strings.Contains(page, "Nodes online (1)")
	})
}

// The page's "Clear hold" button: an admin sees the rule and reason, posts the form from the
// page, and the node serves again, as with `accounts node-clear`. Nobody else can, no other
// site can make the admin's browser do it, and a hold newer than the page stays. The clear is
// logged with node, rule and the admin's pseudonym only.
func TestOpsPageClearsAHold(t *testing.T) {
	logs := &syncBuf{}
	logSink = logs
	defer func() { logSink = io.Discard }()
	a := newAcctRigWith(t, 100, nil, withProjects)
	admin, _ := a.signIn(t, 1)
	user, _ := a.signIn(t, 2)
	at := time.Date(2026, 11, 10, 9, 41, 7, 123456789, time.UTC)
	if err := a.store.HoldNode(gateway.NodeHold{Node: "node-0", Operator: "op", Rule: "file-upload",
		Reason: "netcat sending a file to a host and port", At: at}); err != nil {
		t.Fatal(err)
	}
	var page string
	waitFor(t, func() bool {
		_, page = a.get(t, "/account/ops", admin)
		return strings.Contains(page, "Nodes online (0) · 1 held")
	})
	for _, want := range []string{`action="/account/ops/clear"`, `name="node" value="node-0"`,
		`name="at" value="` + at.Format(time.RFC3339Nano) + `"`, "Clear hold", "file-upload", "netcat sending a file to a host and port"} {
		if !strings.Contains(page, want) {
			t.Fatalf("the page lacks %q: %s", want, page)
		}
	}
	held := func() bool {
		a.store.Refresh()
		return len(a.store.Snapshot().NodeHolds) == 1
	}
	post := func(session, form, site, origin string) (int, string) {
		req, _ := http.NewRequest("POST", a.gw.URL+"/account/ops/clear", strings.NewReader(form))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		if session != "" {
			req.Header.Set("Cookie", session)
		}
		if site != "" {
			req.Header.Set("Sec-Fetch-Site", site)
		}
		req.Header.Set("Origin", origin)
		resp, err := noRedirect.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return resp.StatusCode, string(b)
	}
	form := "node=node-0&at=" + url.QueryEscape(at.Format(time.RFC3339Nano))
	for _, c := range []struct {
		name, session, form, site, origin string
		code                              int
	}{
		{"signed out", "", form, "same-origin", "null", 403},
		{"not an admin", user, form, "same-origin", "null", 403},
		{"cross-site, Sec-Fetch-Site", admin, form, "cross-site", "https://evil.test", 403},
		{"cross-site, Origin only", admin, form, "", "https://evil.test", 403},
		{"a newer hold than the page showed", admin, "node=node-0&at=" + url.QueryEscape(at.Add(time.Second).Format(time.RFC3339Nano)), "same-origin", "null", 409},
		{"no time", admin, "node=node-0", "same-origin", "null", 400},
	} {
		if code, body := post(c.session, c.form, c.site, c.origin); code != c.code || !held() {
			t.Fatalf("%s: HTTP %d (want %d), held %v: %s", c.name, code, c.code, held(), body)
		}
	}
	if strings.Contains(logs.String(), "node hold cleared") {
		t.Fatal("a refused clear was logged as done")
	}
	// The admin's own form (a real browser sends Origin: null with Sec-Fetch-Site).
	if code, body := post(admin, form, "same-origin", "null"); code != 303 || held() {
		t.Fatalf("the admin's clear: HTTP %d, held %v: %s", code, held(), body)
	}
	waitFor(t, func() bool {
		_, page = a.get(t, "/account/ops", admin)
		return !strings.Contains(page, `id="holds"`) && strings.Contains(page, "Nodes online (1)")
	})
	l := logs.String()
	if !strings.Contains(l, "node hold cleared on the ops page") || !strings.Contains(l, "node=node-0") ||
		!strings.Contains(l, "rule=file-upload") || !strings.Contains(l, "by=acct-") || strings.Contains(l, "gh:1") {
		t.Fatalf("the clear's log line: %s", l)
	}
	// Cleared twice: the page says there is nothing to clear.
	if code, body := post(admin, form, "same-origin", "null"); code != 409 || !strings.Contains(body, "not held") {
		t.Fatalf("clearing again: HTTP %d %s", code, body)
	}
}
