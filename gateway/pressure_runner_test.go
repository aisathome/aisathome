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

const (
	runnerA = "0123456789abcdef0123456789abcdef"
	runnerB = "fedcba9876543210"
)

// runnerGateway: lab publishes its queue; other does not.
func runnerGateway(t *testing.T, dir string) *Gateway {
	t.Helper()
	return newTestGateway(t, Config{StateDir: dir, Projects: []Project{
		{ID: "lab", KeySHA256: []string{keyHash(testProjectKey)}, PublicQueue: true},
		{ID: "other", KeySHA256: []string{keyHash(otherProjectKey)}},
	}})
}

// report is a valid runner report: a 7.6 GB runner with one Lean slot, at now.
func report(runner string, now time.Time, more map[string]any) map[string]any {
	r := map[string]any{
		"schema": 1, "runner": runner, "at": now.UTC().Format(time.RFC3339), "period_s": 60, "cpus": 2,
		"cpu_busy": 0.5, "cpu_iowait": 0.02, "cpu_steal": 0, "mem_total_gb": 7.6, "mem_private_gb": 1.2,
		"mem_private_peak_gb": 1.9, "page_cache_gb": 5.6, "page_cache_low_gb": 5.1, "refaults_s": 300,
		"psi":            map[string]any{"cpu": 0.1, "memory": 0.01, "io": 0.03},
		"disk_read_mb_s": 2.5, "disk_write_mb_s": 0.5, "free_gb": 30,
		"lean_slots": 1, "lean_busy": 0.4, "lean_busy_peak": 1, "lean_waiting": 0.2, "lean_waiting_peak": 2,
		"lean_runs_s": []float64{8, 9, 40}, "lean_waits_s": []float64{3}, "sessions": 2, "sessions_peak": 3, "spot_notice": false,
	}
	for k, v := range more {
		if v == nil {
			delete(r, k)
		} else {
			r[k] = v
		}
	}
	return r
}

func postRunner(t *testing.T, g *Gateway, project, key string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var raw []byte
	switch b := body.(type) {
	case []byte:
		raw = b
	default:
		raw, _ = json.Marshal(b)
	}
	r := httptest.NewRequest("POST", "/v1/projects/"+project+"/runner-metrics", bytes.NewReader(raw))
	if key != "" {
		r.Header.Set("Authorization", "Bearer "+key)
	}
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	g.Handler().ServeHTTP(w, r)
	return w
}

func runnerReports(g *Gateway) int {
	g.press.mu.Lock()
	defer g.press.mu.Unlock()
	n := 0
	for _, b := range g.press.hours {
		if b.Runner != nil {
			n += b.Runner.Reports
		}
	}
	return n
}

// Only the project's own key may report: no key or an unknown key gets 401, another project's 403,
// and nothing is counted.
func TestRunnerMetricsKeys(t *testing.T) {
	g := runnerGateway(t, t.TempDir())
	body := report(runnerA, time.Now(), nil)
	for _, c := range []struct {
		name, key string
		want      int
	}{
		{"no key", "", http.StatusUnauthorized},
		{"unknown key", "ak-not-a-project-key-at-all-000", http.StatusUnauthorized},
		{"other project's key", otherProjectKey, http.StatusForbidden},
	} {
		if w := postRunner(t, g, "lab", c.key, body); w.Code != c.want {
			t.Errorf("%s: HTTP %d, want %d: %s", c.name, w.Code, c.want, w.Body)
		}
	}
	if n := runnerReports(g); n != 0 {
		t.Fatalf("refused posts counted: %d", n)
	}
	if w := postRunner(t, g, "lab", testProjectKey, body); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"counted":true`) {
		t.Fatalf("own key: HTTP %d %s", w.Code, w.Body)
	}
	if n := runnerReports(g); n != 1 {
		t.Fatalf("counted %d", n)
	}
}

// The schema is strict: unknown fields, an instance id or a host name as the runner, a stale time,
// values out of range and too many durations are refused; the body is JSON.
func TestRunnerMetricsSchema(t *testing.T) {
	g := runnerGateway(t, t.TempDir())
	now := time.Now()
	for name, more := range map[string]map[string]any{
		"unknown field":       {"instance": "m7a.large"},
		"instance id":         {"runner": "i-0123456789abcdef0"},
		"host name":           {"runner": "ip-10-0-1-23.eu-north-1"},
		"short id":            {"runner": "abc123"},
		"upper-case id":       {"runner": "0123456789ABCDEF"},
		"stale":               {"at": now.Add(-time.Hour).UTC().Format(time.RFC3339)},
		"future":              {"at": now.Add(time.Hour).UTC().Format(time.RFC3339)},
		"schema 2":            {"schema": 2},
		"cpu over 1":          {"cpu_busy": 1.5},
		"negative memory":     {"mem_private_gb": -1},
		"no cpus":             {"cpus": 0},
		"short period":        {"period_s": 2},
		"psi over 1":          {"psi": map[string]any{"cpu": 2, "memory": 0, "io": 0}},
		"negative duration":   {"lean_runs_s": []float64{-1}},
		"too many durations":  {"lean_waits_s": make([]float64, runnerMaxDurations+1)},
		"unknown psi field":   {"psi": map[string]any{"cpu": 0, "memory": 0, "io": 0, "irq": 0}},
		"day-long duration+1": {"lean_runs_s": []float64{86401}},
	} {
		if w := postRunner(t, g, "lab", testProjectKey, report(runnerA, now, more)); w.Code != http.StatusBadRequest {
			t.Errorf("%s: HTTP %d, want 400: %s", name, w.Code, w.Body)
		}
	}
	if w := postRunner(t, g, "lab", testProjectKey, []byte(`{"schema":1}{"schema":1}`)); w.Code != http.StatusBadRequest {
		t.Errorf("two objects: HTTP %d", w.Code)
	}
	r := httptest.NewRequest("POST", "/v1/projects/lab/runner-metrics", strings.NewReader(`{}`))
	r.Header.Set("Authorization", "Bearer "+testProjectKey)
	w := httptest.NewRecorder()
	g.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusUnsupportedMediaType {
		t.Errorf("no content type: HTTP %d", w.Code)
	}
	big := report(runnerA, now, map[string]any{"lean_runs_s": make([]float64, 0)})
	big["pad"] = strings.Repeat("x", maxRunnerBody)
	if w := postRunner(t, g, "lab", testProjectKey, big); w.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("too large: HTTP %d", w.Code)
	}
	if n := runnerReports(g); n != 0 {
		t.Fatalf("refused reports counted: %d", n)
	}
	// Without PSI (a kernel without it) the report is fine.
	if w := postRunner(t, g, "lab", testProjectKey, report(runnerA, now, map[string]any{"psi": nil})); w.Code != http.StatusOK {
		t.Fatalf("without psi: HTTP %d %s", w.Code, w.Body)
	}
}

// One report a minute per runner: a second within runnerMinInterval is 429, another runner's is
// counted; a project that keeps its queue private is answered but not counted.
func TestRunnerMetricsRateAndPrivateProjects(t *testing.T) {
	g := runnerGateway(t, t.TempDir())
	now := time.Now()
	if w := postRunner(t, g, "lab", testProjectKey, report(runnerA, now, nil)); w.Code != http.StatusOK {
		t.Fatalf("first: HTTP %d", w.Code)
	}
	if w := postRunner(t, g, "lab", testProjectKey, report(runnerA, now, nil)); w.Code != http.StatusTooManyRequests {
		t.Fatalf("again at once: HTTP %d", w.Code)
	}
	if w := postRunner(t, g, "lab", testProjectKey, report(runnerB, now, nil)); w.Code != http.StatusOK {
		t.Fatalf("another runner: HTTP %d", w.Code)
	}
	if n := runnerReports(g); n != 2 {
		t.Fatalf("counted %d, want 2", n)
	}
	if n := g.press.runnersFresh(time.Now()); n != 2 {
		t.Fatalf("%d runners fresh", n)
	}
	if n := g.press.runnersFresh(time.Now().Add(runnerFresh + time.Second)); n != 0 {
		t.Fatalf("%d runners fresh after %v", n, runnerFresh)
	}
	w := postRunner(t, g, "other", otherProjectKey, report("aaaaaaaaaaaaaaaa", now, nil))
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"counted":false`) {
		t.Fatalf("private project: HTTP %d %s", w.Code, w.Body)
	}
	if n := runnerReports(g); n != 2 {
		t.Fatalf("a private project's runner was counted: %d", n)
	}
	if s := g.pressureNow(); s.Runners != 2 {
		t.Fatalf("sample: %d runners", s.Runners)
	}
}

// Reports add up per hour and day: averages over runner-minutes, peaks, lows, Lean run and wait
// percentiles, PSI over the reports that had it, and one spot notice per runner however often it
// is reported.
func TestRunnerMetricsAggregate(t *testing.T) {
	p := newPressure()
	at := time.Date(2026, 11, 19, 10, 0, 30, 0, time.UTC)
	add := func(runner string, dt time.Duration, more map[string]any) {
		t.Helper()
		raw, _ := json.Marshal(report(runner, at.Add(dt), more))
		r, err := decodeRunnerReport(raw, at.Add(dt))
		if err != nil {
			t.Fatal(err)
		}
		if !p.runner("lab", r, at.Add(dt)) {
			t.Fatal("not counted")
		}
	}
	add(runnerA, 0, nil)
	add(runnerA, time.Minute, map[string]any{"cpu_busy": 0.9, "page_cache_gb": 3.6, "page_cache_low_gb": 2.9, "refaults_s": 4100,
		"free_gb": 12, "psi": nil, "spot_notice": true, "lean_runs_s": []float64{130}, "lean_waits_s": []float64{45, 60}})
	add(runnerA, 2*time.Minute, map[string]any{"spot_notice": true, "lean_runs_s": []float64{}, "lean_waits_s": []float64{}})
	add(runnerB, 0, map[string]any{"cpu_busy": 0.1, "mem_private_peak_gb": 3.3, "free_gb": 40})

	v := p.view("24h", at.Add(3*time.Minute))
	h := v["buckets"].([]map[string]any)[0]["runner"].(map[string]any)
	if h["reports"] != 4 || h["spot_notices"] != 1 {
		t.Fatalf("reports %v, spot notices %v", h["reports"], h["spot_notices"])
	}
	if c := h["cpu"].(map[string]any); c["avg"] != 0.5 || c["peak"] != 0.9 {
		t.Fatalf("cpu %v", c)
	}
	if pc := h["page_cache_gb"].(map[string]any); pc["avg"] != 5.1 || pc["low"] != 2.9 {
		t.Fatalf("page cache %v", pc)
	}
	if m := h["mem_private_gb"].(map[string]any); m["avg"] != 1.2 || m["peak"] != 3.3 {
		t.Fatalf("private memory %v", m)
	}
	if r := h["refaults_s"].(map[string]any); r["avg"] != 1250.0 || r["peak"] != 4100.0 {
		t.Fatalf("refaults %v", r)
	}
	if h["free_gb_low"] != 12.0 || h["mem_total_gb"] != 7.6 {
		t.Fatalf("free %v, total %v", h["free_gb_low"], h["mem_total_gb"])
	}
	if psi := h["psi"].(map[string]any)["cpu"].(map[string]any); psi["avg"] != 0.1 {
		t.Fatalf("psi over the three reports with it: %v", psi)
	}
	lean := h["lean"].(map[string]any)
	if run := lean["run_s"].(map[string]any); run["count"] != int64(7) || run["p95"] != 130.0 {
		t.Fatalf("lean runs %v", run)
	}
	if w := lean["wait_s"].(map[string]any); w["count"] != int64(4) || w["p50"] != 3.0 || w["p95"] != 60.0 {
		t.Fatalf("lean waits %v", w)
	}
	if b := lean["busy"].(map[string]any); b["avg"] != 0.4 || b["peak"] != 1.0 || lean["slots"] != 1.0 {
		t.Fatalf("lean busy %v, slots %v", b, lean["slots"])
	}
	// The day and the range hold the same counts.
	day := p.view("30d", at.Add(3*time.Minute))["buckets"].([]map[string]any)[0]["runner"].(map[string]any)
	tot := v["total"].(map[string]any)["runner"].(map[string]any)
	for _, x := range []map[string]any{day, tot} {
		if x["reports"] != 4 || x["page_cache_gb"].(map[string]any)["low"] != 2.9 || x["free_gb_low"] != 12.0 {
			t.Fatalf("sum: %v", x)
		}
	}
	// An hour without reports has no runner part.
	p.sample(pressSample{Slots: 1}, at.Add(time.Hour))
	if _, ok := p.view("24h", at.Add(time.Hour))["buckets"].([]map[string]any)[1]["runner"]; ok {
		t.Fatal("an hour without reports has runner numbers")
	}
}

// Lows merge across buckets only from buckets with reports: an empty one's zero is no low.
func TestRunnerAggMergeLows(t *testing.T) {
	a, b := &runnerAgg{}, &runnerAgg{}
	b.Free.add(20, 20, 20, 0)
	b.Reports = 1
	a.merge(&runnerAgg{}) // nothing yet
	a.merge(b)
	a.merge(&runnerAgg{})
	if a.Free.Low != 20 || a.Reports != 1 {
		t.Fatalf("low %v reports %d", a.Free.Low, a.Reports)
	}
	c := &runnerAgg{Reports: 1}
	c.Free.add(5, 5, 5, 0)
	a.merge(c)
	if a.Free.Low != 5 {
		t.Fatalf("low %v", a.Free.Low)
	}
}

// The runners' numbers survive a restart in pressure.json; the runner ids never reach the file or
// the public answer: no id, no project, no instance.
func TestRunnerMetricsPrivacyAndRestart(t *testing.T) {
	dir := t.TempDir()
	g := runnerGateway(t, dir)
	now := time.Now()
	for _, id := range []string{runnerA, runnerB} {
		if w := postRunner(t, g, "lab", testProjectKey, report(id, now, nil)); w.Code != http.StatusOK {
			t.Fatalf("HTTP %d %s", w.Code, w.Body)
		}
	}
	g.press.sample(g.pressureNow(), now)
	if err := g.Close(); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, pressureFile))
	if err != nil {
		t.Fatal(err)
	}
	for _, leak := range []string{runnerA, runnerB, "lab", "i-"} {
		if strings.Contains(string(raw), leak) {
			t.Fatalf("pressure.json holds %q", leak)
		}
	}
	g = runnerGateway(t, dir)
	defer g.Close()
	w := httptest.NewRecorder()
	g.Handler().ServeHTTP(w, httptest.NewRequest("GET", "/v1/stats/pressure?range=24h", nil))
	body := w.Body.String()
	for _, leak := range []string{runnerA, runnerB, "lab", "other", "project", `"i-`, testProjectKey} {
		if strings.Contains(body, leak) {
			t.Fatalf("the public answer holds %q: %s", leak, body)
		}
	}
	var out struct {
		Last struct {
			Runners map[string]any `json:"runners"`
			Runner  map[string]any `json:"runner"`
		} `json:"last_24h"`
	}
	json.Unmarshal(w.Body.Bytes(), &out)
	if out.Last.Runner["reports"] != 2.0 || out.Last.Runners["peak"] != 2.0 {
		t.Fatalf("after a restart: %s", body)
	}
	// The runners themselves are not remembered across a restart: the same runner may report at once.
	if w := postRunner(t, g, "lab", testProjectKey, report(runnerA, time.Now(), nil)); w.Code != http.StatusOK {
		t.Fatalf("after a restart: HTTP %d", w.Code)
	}
}

// At most runnerMaxTracked runners are kept at once; a runner silent for runnerForget is forgotten.
func TestRunnerMetricsBounded(t *testing.T) {
	p := newPressure()
	now := time.Now()
	raw, _ := json.Marshal(report(runnerA, now, nil))
	r, _ := decodeRunnerReport(raw, now)
	for i := 0; i < runnerMaxTracked; i++ {
		x := *r
		x.Runner = fmt.Sprintf("%016x", i)
		if !p.runner("lab", &x, now) {
			t.Fatalf("runner %d refused", i)
		}
	}
	x := *r
	x.Runner = "ffffffffffffffffffff"
	if p.runner("lab", &x, now) {
		t.Fatal("one runner more than runnerMaxTracked was counted")
	}
	if !p.runner("lab", &x, now.Add(runnerForget+time.Minute)) {
		t.Fatal("silent runners were not forgotten")
	}
}
