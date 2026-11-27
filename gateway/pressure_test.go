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

// A state file from before private projects were left out (version 1): its open tasks, demand and node
// time may hold private projects' numbers and are dropped; the rest is kept.
func TestPressureDropsVersion1PrivateNumbers(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	start := now.UTC().Truncate(time.Hour).Format(time.RFC3339)
	b := &pressBucket{Start: start, Samples: 1, Node: &nodeAgg{}}
	b.Slots.add(4)
	b.OpenTasks.add(7)
	b.Demand.add(3)
	raw, _ := json.Marshal(pressureState{Version: 1, Hours: []*pressBucket{b}})
	os.WriteFile(filepath.Join(dir, pressureFile), raw, 0o600)
	g := newTestGateway(t, Config{StateDir: dir, HistorySample: time.Hour})
	defer g.Close()
	g.press.mu.Lock()
	got := g.press.hours[start]
	g.press.mu.Unlock()
	if got == nil || got.Slots != b.Slots || got.OpenTasks != (gauge{}) || got.Demand != (gauge{}) || got.Node != nil {
		t.Fatalf("version 1 bucket after load: %+v", got)
	}
}

// The sample: slots of nodes that serve (not held, not paused), the largest model's slots per node;
// science's slots within its agent rules and share; demand = min(open tasks, science slots).
func TestPressureNowDemand(t *testing.T) {
	g := newTestGateway(t, Config{StateDir: t.TempDir(), Projects: []Project{
		{ID: "lab", KeySHA256: []string{keyHash(testProjectKey)}, AgentOK: true, MaxNodeShare: 0.5, PublicQueue: true},
		{ID: "quiet", KeySHA256: []string{keyHash(otherProjectKey)}, AgentOK: true}, // keeps its queue private
	}})
	defer g.Close()
	add := func(id string, probation bool, slots, inflight int) *nodeConn {
		n := &nodeConn{id: id, operator: "op-" + id, pending: map[string]*pendingReq{}, closed: make(chan struct{}),
			inflight: inflight, models: []protocol.ModelInfo{{Name: "m", Roles: []string{"deep"}, Slots: slots}, {Name: "small", Slots: 1}}}
		if probation {
			n.probEnd = time.Now().Add(time.Hour)
		}
		g.mu.Lock()
		g.node[id] = n
		g.mu.Unlock()
		return n
	}
	add("t1", false, 2, 2)
	add("t2", false, 3, 1)
	add("t3", false, 1, 0)
	add("v1", true, 4, 4) // in probation: no agents yet
	add("p1", false, 8, 0).paused = true
	add("h1", false, 8, 0).held = true

	s := g.pressureNow()
	if s.Slots != 10 || s.Busy != 7 || s.Held != 1 {
		t.Fatalf("slots %d busy %d held %d", s.Slots, s.Busy, s.Held)
	}
	// Three nodes serve the project's agents; its share is one of them, the largest (3 slots).
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
	// A project that keeps its queue private is in no public count: neither its tasks nor its slots.
	for _, id := range []string{"q1", "q2", "q3"} {
		if _, _, err := g.tasks.create("quiet", &taskNew{ID: id, Kind: kindOther, Title: id, CreatedBy: "owner", Spec: json.RawMessage(`{}`)}); err != nil {
			t.Fatal(err)
		}
	}
	if s = g.pressureNow(); s.OpenTasks != 5 || s.Demand != 3 || s.ScienceSlots != 3 {
		t.Fatalf("with a private project's tasks: %+v", s)
	}
	// A restarted gateway reads the queue for the sample, before any worker calls.
	g.tasks.mu.Lock()
	delete(g.tasks.byPr, "lab")
	g.tasks.mu.Unlock()
	if s = g.pressureNow(); s.OpenTasks != 5 {
		t.Fatalf("unloaded queue: %+v", s)
	}
}

// Science's slots are those real science requests may use: a pilot that serves science only and a node
// without an account that the model check admitted count; one it has not admitted does not.
func TestPressureNowScienceNodes(t *testing.T) {
	g := newTestGateway(t, Config{StateDir: t.TempDir(), Projects: []Project{
		{ID: "lab", KeySHA256: []string{keyHash(testProjectKey)}, AgentOK: true, PublicQueue: true},
	}})
	defer g.Close()
	add := func(id string, slots int, mod func(*nodeConn)) {
		n := &nodeConn{id: id, operator: "op-" + id, pending: map[string]*pendingReq{}, closed: make(chan struct{}),
			models: []protocol.ModelInfo{{Name: "m", Slots: slots}}}
		mod(n)
		g.mu.Lock()
		g.node[id] = n
		g.mu.Unlock()
	}
	add("n1", 1, func(*nodeConn) {})
	add("p-000000000001", 2, func(n *nodeConn) {
		n.pilot, n.connector, n.deadline, n.scienceOnly = "p-000000000001", "boinc", time.Now().Add(time.Hour), true
	})
	add("a1", 4, func(n *nodeConn) { n.anon, n.admitted = true, true })
	add("a2", 8, func(n *nodeConn) { n.anon = true }) // not admitted yet
	if s := g.pressureNow(); s.Slots != 15 || s.ScienceSlots != 7 {
		t.Fatalf("slots %d, science slots %d, want 15 and 7 (the node, the pilot and the admitted node)", s.Slots, s.ScienceSlots)
	}
}

// The endpoint: public, cached, the ranges it names, and aggregates only (no node, operator,
// project or user).
func TestPressureEndpoint(t *testing.T) {
	s := newStickyRig(t)
	s.node("node-secret", "op-secret", 0)
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
	g := newTestGateway(t, Config{Keys: map[string]KeyInfo{"user-key": {User: "alice"}},
		NodeTokens: map[string]NodeToken{"node-token": {Operator: "op1", OperatorTerms: OperatorTermsVersion}}, QueueTimeoutSec: 1,
		Projects: []Project{{ID: "quiet", KeySHA256: []string{keyHash(otherProjectKey)}}}}) // keeps its queue private
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
				// llama-server's last chunk: its own timings (shorter than what the gateway measures)
				write(protocol.Msg{Type: protocol.TypeChunk, ReqID: m.ReqID, Data: []byte(`{"choices":[],"timings":{"cache_n":3,"prompt_n":2,"prompt_ms":20,"predicted_n":1,"predicted_ms":30}}`)})
				write(protocol.Msg{Type: protocol.TypeDone, ReqID: m.ReqID, Usage: &protocol.Usage{PromptTokens: 5, CachedTokens: 3, CompletionTokens: 1}})
			}
		}
	}()
	for deadline := time.Now().Add(5 * time.Second); g.NodeCount() != 1; time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the node did not connect")
		}
	}
	chatAs := func(key, model string) int {
		req, _ := http.NewRequest("POST", srv.URL+"/v1/chat/completions", strings.NewReader(`{"model":"`+model+`","messages":[{"role":"user","content":"hi"}]}`))
		req.Header.Set("Authorization", "Bearer "+key)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	chat := func(model string) int { return chatAs("user-key", model) }
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
	// A private project's answer adds nothing to the public node time.
	if code := chatAs(otherProjectKey, "deep"); code != http.StatusOK {
		t.Fatalf("private project: HTTP %d", code)
	}
	v = g.press.view("24h", time.Now())["last_24h"].(map[string]any)
	// The answer's node time, from the engine's timings: 20 ms prefill, 30 ms decode; 3 of 5 prompt
	// tokens from the cache.
	nd := v["node"].(map[string]any)
	if nd["requests"] != 1 || nd["engine_timed"] != 1 || nd["prefill_share"] != 0.4 || nd["cache_hit"] != 0.6 ||
		nd["prefill_tok_s"] != 100.0 || nd["decode_tok_s"] != 33.3 {
		t.Fatalf("node %v", nd)
	}
}

// The node time of an answer: the engine's timings when they fit within the gateway's measure, else
// the gateway's split at the first text; engine timings that are odd are not read.
func TestPressureNodeTime(t *testing.T) {
	ms := time.Millisecond
	et := &engineTimings{PromptMs: 300, PredictedMs: 1200}
	if p, d, e := nodeTime(2000*ms, 900*ms, et); p != 300*ms || d != 1200*ms || !e {
		t.Fatalf("engine: %v %v %v", p, d, e)
	}
	// Longer than the gateway saw: a node cannot claim more time than it took.
	if p, d, e := nodeTime(1000*ms, 400*ms, et); p != 400*ms || d != 600*ms || e {
		t.Fatalf("too long: %v %v %v", p, d, e)
	}
	if p, d, e := nodeTime(1000*ms, 250*ms, nil); p != 250*ms || d != 750*ms || e {
		t.Fatalf("no timings: %v %v %v", p, d, e)
	}
	if p, d, _ := nodeTime(1000*ms, 0, nil); p != 0 || d != 0 {
		t.Fatalf("no text: %v %v", p, d)
	}
	for _, bad := range []string{`{"timings":{"prompt_ms":-1,"predicted_ms":5}}`, `{"timings":{}}`, `{"timings":"x"}`, `{"choices":[]}`, `not json "timings"`} {
		if chunkTimings([]byte(bad)) != nil {
			t.Errorf("%s: read as timings", bad)
		}
	}
	if et := chunkTimings([]byte(`{"timings":{"prompt_ms":12.5,"predicted_ms":40}}`)); et == nil || et.PromptMs != 12.5 {
		t.Fatalf("timings: %+v", et)
	}
	// Buckets: counted per hour and day; a range sums them; an answer with no text is not counted.
	p := newPressure()
	at := time.Date(2026, 11, 19, 9, 0, 0, 0, time.UTC)
	p.node(2000*ms, 500*ms, nil, protocol.Usage{PromptTokens: 1000, CachedTokens: 900, CompletionTokens: 30}, at)
	p.node(2000*ms, 500*ms, et, protocol.Usage{PromptTokens: 1000, CachedTokens: 0, CompletionTokens: 30}, at.Add(time.Hour))
	p.node(2000*ms, 0, nil, protocol.Usage{PromptTokens: 9999}, at)
	tot := p.view("24h", at.Add(time.Hour))["total"].(map[string]any)["node"].(map[string]any)
	if tot["requests"] != 2 || tot["engine_timed"] != 1 || tot["cache_hit"] != 0.45 || tot["prefill_share"] != 0.229 {
		t.Fatalf("total %v", tot)
	}
}

// Projects that share nodes: demand is capped by the slots of the nodes the projects with work may
// use, each node once; a project without work adds no slot to that cap (part 1 counted the slots
// of every project, so two projects sharing one slot could show a demand of two).
func TestPressureDemandSharedSlots(t *testing.T) {
	keys := []string{"ak-a-test-key-0123456789abcdef", "ak-b-test-key-0123456789abcdef", "ak-c-test-key-0123456789abcdef"}
	g := newTestGateway(t, Config{StateDir: t.TempDir(), Projects: []Project{
		{ID: "a", KeySHA256: []string{keyHash(keys[0])}, AgentOK: true, PublicQueue: true},
		{ID: "b", KeySHA256: []string{keyHash(keys[1])}, AgentOK: true, PublicQueue: true},
		{ID: "c", KeySHA256: []string{keyHash(keys[2])}, AgentOK: true, PilotProbationOK: true, PublicQueue: true}, // pilots in probation too
	}})
	defer g.Close()
	add := func(id string, pilotInProbation bool, slots int) {
		n := &nodeConn{id: id, operator: "op-" + id, pending: map[string]*pendingReq{}, closed: make(chan struct{}),
			models: []protocol.ModelInfo{{Name: "m", Roles: []string{"deep"}, Slots: slots}}}
		if pilotInProbation {
			n.pilot, n.deadline, n.probEnd = "p-"+id, time.Now().Add(time.Hour), time.Now().Add(time.Hour)
		}
		g.mu.Lock()
		g.node[id] = n
		g.mu.Unlock()
	}
	add("shared", false, 1) // a, b and c may use it
	add("vol", true, 1)     // only c may
	task := func(project, id string) {
		t.Helper()
		if _, _, err := g.tasks.create(project, &taskNew{ID: id, Kind: kindOther, Title: id, CreatedBy: "owner", Spec: json.RawMessage(`{}`)}); err != nil {
			t.Fatal(err)
		}
	}
	task("a", "a1")
	task("b", "b1")
	s := g.pressureNow()
	if s.OpenTasks != 2 || s.ScienceSlots != 2 || s.Demand != 1 {
		t.Fatalf("a and b share one slot, c has none of their work: %+v", s)
	}
	task("c", "c1")
	if s = g.pressureNow(); s.OpenTasks != 3 || s.Demand != 2 {
		t.Fatalf("c's work may use the volunteer's slot too: %+v", s)
	}
}

// The waiting peak of people and science together is its own gauge: two peaks at different moments
// do not add up.
func TestPressureQueueAllPeak(t *testing.T) {
	p := newPressure()
	at := time.Date(2026, 11, 19, 9, 0, 0, 0, time.UTC)
	p.queuePeak(3, 0, at)
	p.queuePeak(0, 4, at.Add(time.Minute))
	p.sample(pressSample{QueuePeople: 1, QueueScience: 1}, at.Add(2*time.Minute))
	q := p.view("24h", at)["buckets"].([]map[string]any)[0]["queue"].(map[string]any)
	if q["all"].(map[string]any)["peak"] != 4 || q["people"].(map[string]any)["peak"] != 3 || q["science"].(map[string]any)["peak"] != 4 {
		t.Fatalf("queue %v", q)
	}
}
