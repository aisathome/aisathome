// SPDX-License-Identifier: Apache-2.0
package gateway

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const (
	testProjectKey  = "ak-tauceti-test-key-0123456789"
	otherProjectKey = "ak-other-project-key-987654321"
)

func statusGateway(t *testing.T, dir string) *Gateway {
	t.Helper()
	return newTestGateway(t, Config{StateDir: dir, Projects: []Project{
		{ID: "tauceti", KeySHA256: []string{keyHash(testProjectKey)}},
		{ID: "other", KeySHA256: []string{keyHash(otherProjectKey)}},
	}})
}

// minimalStatus is a valid status generated at now.
func minimalStatus(now time.Time) map[string]any {
	return map[string]any{
		"schema": 1, "generated_at": now.UTC().Format(time.RFC3339),
		"runner":     map[string]any{"paused": false, "service": "active", "free_gb": 21.5, "instance": "m7i.large spot eu-north-1c"},
		"attempts":   []any{map[string]any{"attempt": "T2-NS-2.a-A", "task_type": "NS", "size": "easy", "state": "running", "steps": 12, "step_cap": 80, "used_min": 9.5, "budget_min": 60, "budget_kind": "model", "awake_min": 14, "interruptions": 1}},
		"candidates": []any{}, "interruptions": []any{},
	}
}

func postStatus(t *testing.T, g *Gateway, project, key string, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest("POST", "/v1/projects/"+project+"/status", bytes.NewReader(body))
	if key != "" {
		r.Header.Set("Authorization", "Bearer "+key)
	}
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	g.Handler().ServeHTTP(w, r)
	return w
}

func statusJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

// Only the project's own key may post its status: no key or an account-style key gets 401,
// another project's key 403. A valid status is stored and survives a restart.
func TestStatusPostKeys(t *testing.T) {
	dir := t.TempDir()
	g := statusGateway(t, dir)
	body := statusJSON(minimalStatus(time.Now()))
	for _, c := range []struct {
		name, key string
		want      int
	}{
		{"no key", "", http.StatusUnauthorized},
		{"wrong key", "ak-not-a-project-key-at-all-000", http.StatusUnauthorized},
		{"other project's key", otherProjectKey, http.StatusForbidden},
	} {
		if w := postStatus(t, g, "tauceti", c.key, body); w.Code != c.want {
			t.Errorf("%s: HTTP %d, want %d: %s", c.name, w.Code, c.want, w.Body)
		}
	}
	if g.pstatus.get("tauceti") != nil {
		t.Fatal("a refused post was stored")
	}
	if w := postStatus(t, g, "tauceti", testProjectKey, body); w.Code != http.StatusOK {
		t.Fatalf("own key: HTTP %d: %s", w.Code, w.Body)
	}
	// A second post within 10 s is refused (the runner posts every minute).
	if w := postStatus(t, g, "tauceti", testProjectKey, body); w.Code != http.StatusTooManyRequests {
		t.Fatalf("second post at once: HTTP %d", w.Code)
	}
	g.Close()
	if _, err := os.Stat(filepath.Join(dir, "status-tauceti.json")); err != nil {
		t.Fatal("status not kept in state_dir")
	}
	g = statusGateway(t, dir)
	defer g.Close()
	s := g.pstatus.get("tauceti")
	if s == nil || len(s.Status.Attempts) != 1 || s.Status.Attempts[0].Attempt != "T2-NS-2.a-A" || s.Status.Runner.Instance == "" {
		t.Fatalf("after a restart: %+v", s)
	}
	// GET with the project's key: the warnings (for the runner's summary), nothing else.
	r := httptest.NewRequest("GET", "/v1/projects/tauceti/status", nil)
	r.Header.Set("Authorization", "Bearer "+testProjectKey)
	w := httptest.NewRecorder()
	g.Handler().ServeHTTP(w, r)
	var got map[string]any
	if json.Unmarshal(w.Body.Bytes(), &got); w.Code != 200 || got["warnings"] == nil || got["received_at"] == nil || got["status"] != nil {
		t.Fatalf("GET: HTTP %d %s", w.Code, w.Body)
	}
	r.Header.Set("Authorization", "Bearer "+otherProjectKey)
	w = httptest.NewRecorder()
	g.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusForbidden {
		t.Fatalf("GET with another project's key: HTTP %d", w.Code)
	}
}

// The schema is strict: unknown fields, wrong types, bad values, secrets, oversized bodies and
// stale statuses are refused, and nothing is stored.
func TestStatusPostSchema(t *testing.T) {
	g := statusGateway(t, "")
	defer g.Close()
	now := time.Now()
	mod := func(f func(m map[string]any)) []byte {
		m := minimalStatus(now)
		f(m)
		return statusJSON(m)
	}
	att := func(m map[string]any) map[string]any { return m["attempts"].([]any)[0].(map[string]any) }
	cases := map[string][]byte{
		"unknown top-level field": mod(func(m map[string]any) { m["prompt"] = "the task text" }),
		"unknown attempt field":   mod(func(m map[string]any) { att(m)["session"] = "ses_x" }),
		"wrong schema":            mod(func(m map[string]any) { m["schema"] = 2 }),
		"wrong type":              mod(func(m map[string]any) { att(m)["steps"] = "twelve" }),
		"bad state":               mod(func(m map[string]any) { att(m)["state"] = "exploded" }),
		"bad attempt id":          mod(func(m map[string]any) { att(m)["attempt"] = "../../etc/passwd" }),
		"negative minutes":        mod(func(m map[string]any) { att(m)["used_min"] = -1 }),
		"cache hit above 1":       mod(func(m map[string]any) { att(m)["cache_hit"] = 1.5 }),
		"an API key in a reason": mod(func(m map[string]any) {
			m["ci"] = map[string]any{"pause": true, "reasons": []any{"key ak-Zm9vYmFyYmF6cXV4cXV1eHh5eXp6MTIzNDU2Nzg5MA"}, "checked_at": now.UTC().Format(time.RFC3339)}
		}),
		"a GitHub token in an outcome": mod(func(m map[string]any) { att(m)["outcome"] = "token ghp_abcdefghijklmnop" }),
		"a long hash in the instance":  mod(func(m map[string]any) { m["runner"].(map[string]any)["instance"] = strings.Repeat("ab12", 12) }),
		"stale":                        mod(func(m map[string]any) { m["generated_at"] = now.Add(-2 * time.Hour).UTC().Format(time.RFC3339) }),
		"two objects":                  append(statusJSON(minimalStatus(now)), statusJSON(minimalStatus(now))...),
		// over 64 KiB (valid JSON padded with spaces): 413 before it is even parsed
		"too large": append(statusJSON(minimalStatus(now)), bytes.Repeat([]byte(" "), maxStatusBody)...),
	}
	for name, body := range cases {
		w := postStatus(t, g, "tauceti", testProjectKey, body)
		if w.Code != http.StatusBadRequest && w.Code != http.StatusRequestEntityTooLarge {
			t.Errorf("%s: HTTP %d, want 400 or 413: %s", name, w.Code, w.Body)
		}
	}
	if g.pstatus.get("tauceti") != nil {
		t.Fatal("a refused status was stored")
	}
	// The T2-NEW-1 fixtures (made by the runner's reporter) are valid at their own time.
	for _, f := range []string{"status-t2-new-1-1810.json", "status-t2-new-1-1931.json", "status-t2-new-1-1944.json"} {
		st := loadStatusFixture(t, f)
		if _, err := decodeStatus(statusJSON(st), st.GeneratedAt); err != nil {
			t.Errorf("%s: %v", f, err)
		}
	}
}

func loadStatusFixture(t *testing.T, name string) *runnerStatus {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	var st runnerStatus
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&st); err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return &st
}
