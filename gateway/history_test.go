// SPDX-License-Identifier: Apache-2.0
package gateway

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aisathome/aisathome/protocol"
)

func rec(day string, class, outcome string, prompt, completion int) usageRecord {
	t, _ := time.Parse(time.DateOnly, day)
	return usageRecord{Time: t.Add(12 * time.Hour), User: "acct-secret-user", Node: "node-secret", Operator: "op-secret",
		Outcome: outcome, Class: class, Usage: protocol.Usage{PromptTokens: prompt, CompletionTokens: completion}}
}

func newTestGateway(t *testing.T, cfg Config) *Gateway {
	t.Helper()
	g, err := New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	return g
}

func historyOf(t *testing.T, g *Gateway) []map[string]any {
	t.Helper()
	w := httptest.NewRecorder()
	g.handleHistory(w, httptest.NewRequest("GET", "/v1/stats/history", nil))
	var out struct {
		Days []map[string]any `json:"days"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{"secret", "acct-", "node"} { // no users, nodes or operators, only aggregates
		if strings.Contains(strings.ReplaceAll(w.Body.String(), "nodes_", ""), s) {
			t.Fatalf("history names %q: %s", s, w.Body.String())
		}
	}
	return out.Days
}

// Days are counted per class, with node samples as peak and average; they survive a restart;
// days older than the usage log keep their saved numbers, and days the log still covers are
// rebuilt from it.
func TestHistoryDaysSurviveRestart(t *testing.T) {
	dir := t.TempDir()
	usage := filepath.Join(dir, "usage.jsonl")
	cfg := Config{StateDir: dir, UsageLog: usage, HistorySample: time.Hour}
	g := newTestGateway(t, cfg)
	g.hist.add(rec("2026-10-01", ClassChat, "ok", 100, 10))
	g.hist.add(rec("2026-10-01", ClassScience, "ok", 1000, 50))
	g.hist.add(rec("2026-10-01", ClassAgent, "failed", 7, 0))
	day1, _ := time.Parse(time.DateOnly, "2026-10-01")
	for _, n := range []int{2, 4, 3} {
		g.hist.sample(n, day1.Add(time.Hour))
	}
	g.Close()

	// The usage log now covers 2026-10-02 only (the older part was rotated away).
	f, _ := os.Create(usage)
	for _, r := range []usageRecord{rec("2026-10-02", ClassAgent, "ok", 5, 5), rec("2026-10-02", "", "ok", 1, 1)} {
		b, _ := json.Marshal(r)
		f.Write(append(b, '\n'))
	}
	f.Close()
	g = newTestGateway(t, cfg)
	defer g.Close()
	days := historyOf(t, g)
	if len(days) != 2 {
		t.Fatalf("days: %v", days)
	}
	d1, d2 := days[0], days[1]
	if d1["date"] != "2026-10-01" || d1["requests"] != 3.0 || d1["completed"] != 2.0 || d1["prompt_tokens"] != 1107.0 ||
		d1["completion_tokens"] != 60.0 || d1["nodes_peak"] != 4.0 || d1["nodes_avg"] != 3.0 {
		t.Fatalf("day 1: %v", d1)
	}
	cls := d1["classes"].(map[string]any)
	if sci := cls[ClassScience].(map[string]any); sci["requests"] != 1.0 || sci["prompt_tokens"] != 1000.0 {
		t.Fatalf("day 1 science: %v", sci)
	}
	if d2["date"] != "2026-10-02" || d2["requests"] != 2.0 || d2["classes"].(map[string]any)[ClassAgent] == nil {
		t.Fatalf("day 2 (from the usage log): %v", d2)
	}
	if _, ok := d2["nodes_peak"]; ok {
		t.Fatalf("day 2 has node numbers without samples: %v", d2)
	}
	// A third start counts the log's day once, not twice.
	g.Close()
	g = newTestGateway(t, cfg)
	defer g.Close()
	if d := historyOf(t, g)[1]; d["requests"] != 2.0 {
		t.Fatalf("day 2 after another restart: %v", d)
	}
}

// The history keeps at most historyDays days, dropping the oldest.
func TestHistoryBounded(t *testing.T) {
	h := newHistory()
	start, _ := time.Parse(time.DateOnly, "2025-01-01")
	for i := 0; i < historyDays+30; i++ {
		h.sample(1, start.AddDate(0, 0, i))
	}
	days := h.public()
	if len(days) != historyDays {
		t.Fatalf("%d days kept", len(days))
	}
	if days[0]["date"] != start.AddDate(0, 0, 30).Format(time.DateOnly) {
		t.Fatalf("oldest kept: %v", days[0]["date"])
	}
}

// Nodes online are sampled while the gateway runs.
func TestHistorySamplesNodes(t *testing.T) {
	g := newTestGateway(t, Config{HistorySample: 10 * time.Millisecond})
	defer g.Close()
	g.mu.Lock()
	g.node["x"] = &nodeConn{id: "x", pending: map[string]*pendingReq{}, closed: make(chan struct{})}
	g.mu.Unlock()
	for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if d := g.hist.public(); len(d) > 0 && d[len(d)-1]["nodes_peak"] == 1 {
			return
		}
	}
	t.Fatalf("no node sample: %v", g.hist.public())
}

// A corrupt history file is set aside, not fatal.
func TestHistoryCorruptFile(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "history.json"), []byte("{not json"), 0o600)
	g := newTestGateway(t, Config{StateDir: dir})
	g.Close()
	if _, err := os.Stat(filepath.Join(dir, "history.json.bad")); err != nil {
		t.Fatal("corrupt history not set aside")
	}
}

func recAt(t time.Time, class, outcome string, prompt, completion int) usageRecord {
	r := rec("2026-10-01", class, outcome, prompt, completion)
	r.Time = t
	return r
}

func hoursOf(t *testing.T, g *Gateway) []map[string]any {
	t.Helper()
	historyOf(t, g) // the privacy check on the whole answer
	w := httptest.NewRecorder()
	g.handleHistory(w, httptest.NewRequest("GET", "/v1/stats/history", nil))
	var out struct {
		Hours []map[string]any `json:"hours"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	return out.Hours
}

// Requests and tokens are counted per UTC hour and class in /v1/stats/history; hours without
// requests are left out; the hours survive a restart, and hours the usage log still covers are
// rebuilt from it without counting them twice.
func TestHistoryHours(t *testing.T) {
	dir := t.TempDir()
	usage := filepath.Join(dir, "usage.jsonl")
	cfg := Config{StateDir: dir, UsageLog: usage, HistorySample: time.Hour}
	g := newTestGateway(t, cfg)
	h13, _ := time.Parse(time.RFC3339, "2026-10-24T13:05:00Z")
	g.hist.add(recAt(h13, ClassChat, "ok", 100, 10))
	g.hist.add(recAt(h13.Add(50*time.Minute), ClassScience, "ok", 1000, 50)) // 13:55, same hour
	g.hist.add(recAt(h13.Add(2*time.Hour), ClassAgent, "failed", 7, 0))      // 15:05; 14:00 had none
	g.Close()

	// The usage log now holds one more record in 16:00 only.
	f, _ := os.Create(usage)
	b, _ := json.Marshal(recAt(h13.Add(3*time.Hour), ClassChat, "ok", 5, 5))
	f.Write(append(b, '\n'))
	f.Close()
	g = newTestGateway(t, cfg)
	defer g.Close()
	hours := hoursOf(t, g)
	if len(hours) != 3 {
		t.Fatalf("hours: %v", hours)
	}
	a, c, d := hours[0], hours[1], hours[2]
	if a["hour"] != "2026-10-24T13:00Z" || a["requests"] != 2.0 || a["completed"] != 2.0 || a["prompt_tokens"] != 1100.0 ||
		a["completion_tokens"] != 60.0 {
		t.Fatalf("13:00: %v", a)
	}
	cls := a["classes"].(map[string]any)
	if cls[ClassChat].(map[string]any)["requests"] != 1.0 || cls[ClassScience].(map[string]any)["prompt_tokens"] != 1000.0 {
		t.Fatalf("13:00 classes: %v", cls)
	}
	if c["hour"] != "2026-10-24T15:00Z" || c["requests"] != 1.0 || c["completed"] != 0.0 {
		t.Fatalf("15:00: %v", c)
	}
	if d["hour"] != "2026-10-24T16:00Z" || d["requests"] != 1.0 {
		t.Fatalf("16:00 (from the usage log): %v", d)
	}
	g.Close()
	g = newTestGateway(t, cfg)
	defer g.Close()
	if h := hoursOf(t, g); len(h) != 3 || h[2]["requests"] != 1.0 || h[0]["requests"] != 2.0 {
		t.Fatalf("after another restart: %v", h)
	}
}

// The hourly part keeps at most historyHours hours, dropping the oldest.
func TestHistoryHoursBounded(t *testing.T) {
	h := newHistory()
	start, _ := time.Parse(time.RFC3339, "2026-01-01T00:30:00Z")
	for i := 0; i < historyHours+5; i++ {
		h.add(recAt(start.Add(time.Duration(i)*time.Hour), ClassChat, "ok", 1, 1))
	}
	hours := h.publicHours()
	if len(hours) != historyHours {
		t.Fatalf("%d hours kept", len(hours))
	}
	if hours[0]["hour"] != start.Add(5*time.Hour).Format(hourFormat) {
		t.Fatalf("oldest kept: %v", hours[0]["hour"])
	}
}
