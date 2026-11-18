// SPDX-License-Identifier: Apache-2.0
package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/aisathome/aisathome/protocol"
)

// Durations fall into log-spaced bins; a percentile is the upper edge of its bin, never more than
// the longest seen, and 0 in the first bin (no wait).
func TestPressureHistogramPercentiles(t *testing.T) {
	h := &durHist{}
	if h.quantile(0.95) != 0 {
		t.Fatal("an empty histogram has a percentile")
	}
	for i := 0; i < 50; i++ {
		h.add(2 * time.Millisecond) // no wait
	}
	for i := 0; i < 45; i++ {
		h.add(2500 * time.Millisecond) // the 3 s bin
	}
	for i := 0; i < 5; i++ {
		h.add(200 * time.Second) // the 240 s bin, longest 200 s
	}
	if p50, p95, p99 := h.quantile(0.5), h.quantile(0.95), h.quantile(0.99); p50 != 0 || p95 != 3 || p99 != 200 {
		t.Fatalf("p50 %v p95 %v p99 %v, want 0, 3 and 200", p50, p95, p99)
	}
	if h.count() != 100 || h.Max != 200_000 {
		t.Fatalf("count %d, max %d ms", h.count(), h.Max)
	}
	// Longer than the last edge: the overflow bin, reported as the longest seen.
	long := &durHist{}
	long.add(2 * time.Hour)
	if long.quantile(0.5) != 7200 || len(long.C) != len(durBins)+1 {
		t.Fatalf("overflow: %v in %d bins", long.quantile(0.5), len(long.C))
	}
	// Merging adds bin by bin: the sum of two hours is one histogram.
	m := &durHist{}
	m.merge(h)
	m.merge(long)
	if m.count() != 101 || m.Max != 7_200_000 || m.quantile(0.9) != 3 || m.quantile(0.96) != 240 {
		t.Fatalf("merged: count %d, max %d, p90 %v", m.count(), m.Max, m.quantile(0.9))
	}
}

// Samples and events go to their UTC hour and day; averages, peaks and utilisation come from the
// samples; a range sums its buckets; a waiting burst between samples still sets the peak.
func TestPressureBuckets(t *testing.T) {
	p := newPressure()
	at := time.Date(2026, 11, 18, 13, 5, 0, 0, time.UTC)
	p.sample(pressSample{Slots: 4, Busy: 4, QueueScience: 2, ScienceSlots: 3, OpenTasks: 10, Demand: 3}, at)
	p.sample(pressSample{Slots: 4, Busy: 2, ScienceSlots: 3, OpenTasks: 1, Demand: 1}, at.Add(time.Minute))
	p.queuePeak(5, 0, at.Add(2*time.Minute))
	p.sample(pressSample{Slots: 2, Busy: 0}, at.Add(time.Hour)) // the next hour
	p.wait(ClassScience, 150*time.Second, at)
	p.firstToken(ClassChat, 900*time.Millisecond, at)
	p.outcome(pressOK, at)
	p.outcome(pressOK, at)
	p.outcome(pressRefused, at)
	p.hold(at)

	v := p.view("24h", at.Add(time.Hour))
	bs := v["buckets"].([]map[string]any)
	if len(bs) != 2 || bs[0]["start"] != "2026-11-18T13:00Z" || bs[1]["start"] != "2026-11-18T14:00Z" {
		t.Fatalf("buckets: %v", bs)
	}
	h := bs[0]
	if h["utilisation"] != 0.75 || h["samples"] != 2 || h["holds"] != 1 {
		t.Fatalf("hour: %v", h)
	}
	if s := h["slots"].(map[string]any); s["avg"] != 4.0 || s["peak"] != 4 {
		t.Fatalf("slots %v", s)
	}
	if q := h["queue"].(map[string]any); q["people"].(map[string]any)["peak"] != 5 || q["science"].(map[string]any)["avg"] != 1.0 {
		t.Fatalf("queue %v", q)
	}
	if d := h["demand"].(map[string]any); d["avg"] != 2.0 || d["peak"] != 3 {
		t.Fatalf("demand %v", d)
	}
	if w := h["wait_s"].(map[string]any)[ClassScience].(map[string]any); w["p95"] != 150.0 || w["count"] != int64(1) {
		t.Fatalf("wait %v", w)
	}
	if o := h["outcomes"].(map[string]int); o[pressOK] != 2 || o[pressRefused] != 1 || len(o) != 2 {
		t.Fatalf("outcomes %v", o)
	}
	if _, ok := bs[1]["utilisation"]; !ok || bs[1]["utilisation"] != 0.0 {
		t.Fatalf("an idle hour's utilisation: %v", bs[1])
	}
	// The range and the last 24 hours sum both hours: 6 busy slot-samples of 10.
	for _, k := range []string{"total", "last_24h"} {
		if s := v[k].(map[string]any); s["utilisation"] != 0.6 || s["samples"] != 3 {
			t.Fatalf("%s: %v", k, s)
		}
	}
	// The day holds the same counts.
	d := p.view("30d", at.Add(time.Hour))["buckets"].([]map[string]any)
	if len(d) != 1 || d[0]["start"] != "2026-11-18" || d[0]["samples"] != 3 || d[0]["utilisation"] != 0.6 {
		t.Fatalf("days: %v", d)
	}
	// No slots online: no utilisation rather than 0%.
	empty := newPressure()
	empty.sample(pressSample{}, at)
	if _, ok := empty.view("24h", at)["buckets"].([]map[string]any)[0]["utilisation"]; ok {
		t.Fatal("utilisation without slots")
	}
}

// Hours older than two weeks and days older than 90 days are dropped, counted back from now.
func TestPressureRetention(t *testing.T) {
	p := newPressure()
	start := time.Date(2026, 1, 1, 0, 30, 0, 0, time.UTC)
	for i := 0; i < 100*24; i++ {
		p.sample(pressSample{Slots: 1}, start.Add(time.Duration(i)*time.Hour))
	}
	p.mu.Lock()
	hours, days := len(p.hours), len(p.days)
	_, oldHour := p.hours[start.Add(time.Duration(100*24-pressureHours-1)*time.Hour).Format(hourFormat)]
	_, firstHour := p.hours[start.Add(time.Duration(100*24-pressureHours)*time.Hour).Format(hourFormat)]
	p.mu.Unlock()
	if hours != pressureHours || days != pressureDays || oldHour || !firstHour {
		t.Fatalf("%d hours (oldest kept %v, one older %v), %d days", hours, firstHour, oldHour, days)
	}
	end := start.Add(100*24*time.Hour - time.Hour)
	if n := len(p.view("14d", end)["buckets"].([]map[string]any)); n != pressureHours {
		t.Fatalf("14d: %d hours", n)
	}
	if n := len(p.view("7d", end)["buckets"].([]map[string]any)); n != 7*24 {
		t.Fatalf("7d: %d hours", n)
	}
	if n := len(p.view("90d", end)["buckets"].([]map[string]any)); n != pressureDays {
		t.Fatalf("90d: %d days", n)
	}
	// A restart long after: a saved file's old buckets are dropped on load.
	dir := t.TempDir()
	p.path = filepath.Join(dir, pressureFile)
	if err := p.save(); err != nil {
		t.Fatal(err)
	}
	q := newPressure()
	q.path = p.path
	q.load(nil, end.AddDate(0, 0, 30))
	if len(q.hours) != 0 || len(q.days) != pressureDays-30 {
		t.Fatalf("after a month: %d hours, %d days", len(q.hours), len(q.days))
	}
}

// The buckets survive a restart: saved at Close, read at New, and counting goes on in them; a
// corrupt file is set aside, not a reason to refuse to start.
func TestPressureSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{StateDir: dir, HistorySample: time.Hour}
	g := newTestGateway(t, cfg)
	now := time.Now()
	g.press.sample(pressSample{Slots: 2, Busy: 1}, now)
	g.press.wait(ClassChat, 4*time.Second, now)
	g.press.outcome(pressOK, now)
	if err := g.Close(); err != nil {
		t.Fatal(err)
	}
	g = newTestGateway(t, cfg)
	g.press.sample(pressSample{Slots: 2, Busy: 2}, now)
	g.press.outcome(pressFailed, now)
	h := g.press.view("24h", now)["last_24h"].(map[string]any)
	if h["samples"] != 2 || h["utilisation"] != 0.75 || h["outcomes"].(map[string]int)[pressOK] != 1 ||
		h["wait_s"].(map[string]any)[ClassChat].(map[string]any)["p50"] != 4.0 {
		t.Fatalf("after a restart: %v", h)
	}
	g.Close()

	os.WriteFile(filepath.Join(dir, pressureFile), []byte("{not json"), 0o600)
	g = newTestGateway(t, cfg)
	defer g.Close()
	if n := len(g.press.view("24h", now)["buckets"].([]map[string]any)); n != 0 {
		t.Fatalf("a corrupt file gave %d buckets", n)
	}
	if _, err := os.Stat(filepath.Join(dir, pressureFile+".bad")); err != nil {
		t.Fatal("the corrupt file was not set aside")
	}
}

// The sample: slots of nodes that serve (not held, not paused), the largest model's slots per node;
// science's slots within its trust and share; demand = min(open tasks, science slots).
func TestPressureNowDemand(t *testing.T) {
	g := newTestGateway(t, Config{StateDir: t.TempDir(), Projects: []Project{
		{ID: "lab", KeySHA256: []string{keyHash(testProjectKey)}, MinTrust: "trusted", MaxNodeShare: 0.5},
	}})
	defer g.Close()
	add := func(id string, trusted bool, slots, inflight int) *nodeConn {
		n := &nodeConn{id: id, operator: "op-" + id, trusted: trusted, pending: map[string]*pendingReq{}, closed: make(chan struct{}),
			inflight: inflight, models: []protocol.ModelInfo{{Name: "m", Roles: []string{"deep"}, Slots: slots}, {Name: "small", Slots: 1}}}
		g.mu.Lock()
		g.node[id] = n
		g.mu.Unlock()
		return n
	}
	add("t1", true, 2, 2)
	add("t2", true, 3, 1)
	add("t3", true, 1, 0)
	add("v1", false, 4, 4) // a volunteer: not for a trusted-only project
	add("p1", true, 8, 0).paused = true
	add("h1", true, 8, 0).held = true

	s := g.pressureNow()
	if s.Slots != 10 || s.Busy != 7 || s.Held != 1 {
		t.Fatalf("slots %d busy %d held %d", s.Slots, s.Busy, s.Held)
	}
	// Three trusted nodes serve the project; its share is one of them, the largest (3 slots).
	if s.ScienceSlots != 3 || s.OpenTasks != 0 || s.Demand != 0 {
		t.Fatalf("no tasks: %+v", s)
	}
	for _, id := range []string{"a", "b"} {
		if _, _, err := g.tasks.create("lab", &taskNew{ID: id, Kind: kindOther, Title: id, CreatedBy: "owner", Spec: json.RawMessage(`{}`)}); err != nil {
			t.Fatal(err)
		}
	}
	if s = g.pressureNow(); s.OpenTasks != 2 || s.Demand != 2 {
		t.Fatalf("two tasks: %+v", s)
	}
	for _, id := range []string{"c", "d", "e"} {
		g.tasks.create("lab", &taskNew{ID: id, Kind: kindOther, Title: id, CreatedBy: "owner", Spec: json.RawMessage(`{}`)})
	}
	if _, err := g.tasks.claim("lab", "w1", 600, nil, "", nil); err != nil { // a running task is still open
		t.Fatal(err)
	}
	if s = g.pressureNow(); s.OpenTasks != 5 || s.Demand != 3 {
		t.Fatalf("five tasks: %+v", s)
	}
	// A restarted gateway reads the queue for the sample, before any worker calls.
	g.tasks.mu.Lock()
	delete(g.tasks.byPr, "lab")
	g.tasks.mu.Unlock()
	if s = g.pressureNow(); s.OpenTasks != 5 {
		t.Fatalf("unloaded queue: %+v", s)
	}
}

// The endpoint: public, cached, the ranges it names, and aggregates only (no node, operator,
// project or user).
func TestPressureEndpoint(t *testing.T) {
	s := newStickyRig(t)
	s.node("node-secret", "op-secret", true, 0)
	now := time.Now()
	s.g.press.sample(s.g.pressureNow(), now)
	s.g.press.wait(ClassAgent, time.Second, now)
	s.g.press.outcome(pressOK, now)

	get := func(q string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		s.g.Handler().ServeHTTP(w, httptest.NewRequest("GET", "/v1/stats/pressure"+q, nil))
		return w
	}
	w := get("")
	if w.Code != http.StatusOK || !strings.Contains(w.Header().Get("Cache-Control"), "public") {
		t.Fatalf("HTTP %d %v", w.Code, w.Header())
	}
	for _, bad := range []string{"secret", "op-", "node-", "user", "operator", "project"} {
		if strings.Contains(w.Body.String(), bad) {
			t.Fatalf("the answer names %q: %s", bad, w.Body)
		}
	}
	var out struct {
		Range   string           `json:"range"`
		Bucket  string           `json:"bucket"`
		Buckets []map[string]any `json:"buckets"`
		Now     pressSample      `json:"now"`
		Last    map[string]any   `json:"last_24h"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Range != "7d" || out.Bucket != "hour" || len(out.Buckets) != 1 || out.Now.Slots != 1 || out.Last["utilisation"] != 0.0 {
		t.Fatalf("answer: %s", w.Body)
	}
	if d := get("?range=90d"); d.Code != http.StatusOK || !strings.Contains(d.Body.String(), `"bucket":"day"`) {
		t.Fatalf("90d: HTTP %d %s", d.Code, d.Body)
	}
	if b := get("?range=1y"); b.Code != http.StatusBadRequest {
		t.Fatalf("an unknown range: HTTP %d", b.Code)
	}
	// Built once per pressureCacheFor: a new request within it gets the same answer.
	s.g.press.outcome(pressFailed, now)
	if again := get(""); again.Body.String() != w.Body.String() {
		t.Fatal("not cached")
	}
}

// A request through the gateway counts once: its wait for a slot, the time to its first token
// and its outcome; one that finds no node counts as refused.
func TestPressureCountsRequests(t *testing.T) {
	g := newTestGateway(t, Config{Keys: map[string]KeyInfo{"user-key": {User: "alice", MinTrust: "any"}},
		NodeTokens: map[string]NodeToken{"node-token": {Operator: "op1", Trust: "trusted"}}, QueueTimeoutSec: 1})
	defer g.Close()
	srv := httptest.NewServer(g.Handler())
	defer srv.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h := http.Header{}
	h.Set("Authorization", "Bearer node-token")
	c, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http")+"/v1/node/connect", &websocket.DialOptions{HTTPHeader: h})
	if err != nil {
		t.Fatal(err)
	}
	write := func(m protocol.Msg) { b, _ := json.Marshal(m); c.Write(ctx, websocket.MessageText, b) }
	write(protocol.Msg{Type: protocol.TypeHello, NodeID: "n1", Protocol: protocol.LegacyVersion,
		Models: []protocol.ModelInfo{{Name: "qwen3.8-27b-q4", Roles: []string{"deep"}, Slots: 1}}})
	go func() {
		for {
			_, b, err := c.Read(ctx)
			if err != nil {
				return
			}
			var m protocol.Msg
			json.Unmarshal(b, &m)
			if m.Type == protocol.TypeRequest {
				time.Sleep(60 * time.Millisecond) // the first token takes a moment
				write(protocol.Msg{Type: protocol.TypeChunk, ReqID: m.ReqID, Data: []byte(`{"choices":[{"index":0,"delta":{"content":"x"}}]}`)})
				write(protocol.Msg{Type: protocol.TypeDone, ReqID: m.ReqID, Usage: &protocol.Usage{PromptTokens: 5, CompletionTokens: 1}})
			}
		}
	}()
	for deadline := time.Now().Add(5 * time.Second); g.NodeCount() != 1; time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the node did not connect")
		}
	}
	chat := func(model string) int {
		req, _ := http.NewRequest("POST", srv.URL+"/v1/chat/completions", strings.NewReader(`{"model":"`+model+`","messages":[{"role":"user","content":"hi"}]}`))
		req.Header.Set("Authorization", "Bearer user-key")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if code := chat("deep"); code != http.StatusOK {
		t.Fatalf("chat: HTTP %d", code)
	}
	if code := chat("nobody-serves-this"); code != http.StatusNotFound {
		t.Fatalf("unknown model: HTTP %d", code)
	}
	v := g.press.view("24h", time.Now())["last_24h"].(map[string]any)
	if o := v["outcomes"].(map[string]int); o[pressOK] != 1 || o[pressRefused] != 1 || len(o) != 2 {
		t.Fatalf("outcomes %v", o)
	}
	if w := v["wait_s"].(map[string]any)[ClassAgent].(map[string]any); w["count"] != int64(2) {
		t.Fatalf("waits %v", w)
	}
	ft := v["first_token_s"].(map[string]any)[ClassAgent].(map[string]any)
	if ft["count"] != int64(1) || ft["p50"].(float64) < 0.1 {
		t.Fatalf("first token %v", ft)
	}
}
