// SPDX-License-Identifier: Apache-2.0
package gateway

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// syncBuf is a log sink the gateway's goroutines may write to while the test reads it.
type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) { s.mu.Lock(); defer s.mu.Unlock(); return s.b.Write(p) }
func (s *syncBuf) String() string              { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

// statsServe is /v1/stats' serve.
func statsServe(t *testing.T, g *Gateway) serveCapacity {
	t.Helper()
	rec := httptest.NewRecorder()
	g.handleStats(rec, httptest.NewRequest("GET", "/v1/stats", nil))
	var out struct {
		Nodes map[string]int `json:"nodes"` // still numbers only
		Serve *serveCapacity `json:"serve"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil || out.Serve == nil {
		t.Fatalf("/v1/stats without serve: %v %s", err, rec.Body)
	}
	return *out.Serve
}

// The incident of 2026-11-20: nodes online, none of their operators accepted the operator terms. A
// stranger's chat is refused ("operator_terms"), and every signal says so: /v1/stats' serve.chat is 0, the
// operations page's health warning, and one WARN line for any number of refusals (rate-limited). Once a
// node's operator accepts, the chat is placed and the signal says 1.
func TestServeCapacityOperatorTerms(t *testing.T) {
	finalTerms(t)
	logs := &syncBuf{}
	g, err := New(Config{StateDir: t.TempDir()}, slog.New(slog.NewTextHandler(logs, nil)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { g.Close() })
	s := &stickyRig{t, g}
	a, b := s.node("a", "op-a", 0), s.node("b", "op-b", 0) // connected, operator terms not accepted
	held := s.node("h", "op-h", 0)
	g.mu.Lock()
	held.held = true // a held node is not online and counts nowhere
	g.mu.Unlock()

	if got := statsServe(t, g); got != (serveCapacity{Chat: 0, Tools: 0, Science: 2}) {
		t.Fatalf("serve before acceptance: %+v", got)
	}
	_, _, why := g.serveCapacity(time.Now())
	if why[whyTerms] != 2 || len(why) != 1 {
		t.Fatalf("why: %v", why)
	}
	ws := g.healthOf("lab", time.Now())
	if len(ws) == 0 || ws[0].Rule != ruleNoChat || !strings.Contains(ws[0].Text, "2 operator_terms") {
		t.Fatalf("health warnings: %+v", ws)
	}

	chat := func() *route {
		return &route{want: "deep", class: ClassChat, rules: g.chatRulesOn(), exclude: map[*nodeConn]bool{}}
	}
	for i := 0; i < 3; i++ {
		rec := httptest.NewRecorder()
		g.unavailable(rec, chat(), timedOut)
		if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "operator terms") {
			t.Fatalf("refusal %d: %d %s", i, rec.Code, rec.Body)
		}
	}
	out := logs.String()
	if strings.Count(out, "reason=operator_terms") != 3 {
		t.Fatalf("refusals logged: %s", out)
	}
	if strings.Count(out, "level=WARN") != 1 || !strings.Contains(out, `msg="no online node may serve people's requests"`) ||
		!strings.Contains(out, "online=2") || !strings.Contains(out, "serve_chat=0") || !strings.Contains(out, "serve_science=2") ||
		!strings.Contains(out, `why="2 operator_terms"`) {
		t.Fatalf("one WARN line for three refusals expected: %s", out)
	}
	// Science is not refused into a WARN: it has nodes.
	g.unavailable(httptest.NewRecorder(), &route{want: "deep", class: ClassScience, rules: true, exclude: map[*nodeConn]bool{}}, timedOut)
	// After the rate limit the next line counts the refusals since the last one (3 people's, 1 more now).
	g.noChat.mu.Lock()
	g.noChat.last = time.Now().Add(-noChatWarnEvery)
	g.noChat.mu.Unlock()
	g.unavailable(httptest.NewRecorder(), chat(), timedOut)
	if out = logs.String(); strings.Count(out, "level=WARN") != 2 || !strings.Contains(out, "refused=3") {
		t.Fatalf("second WARN after the interval, counting 3 refusals: %s", out)
	}

	// a's operator accepts (the sweep sets this from the accounts file).
	g.mu.Lock()
	a.rules = nodeRules{terms: true}
	g.mu.Unlock()
	if got := statsServe(t, g); got != (serveCapacity{Chat: 1, Tools: 1, Science: 2}) {
		t.Fatalf("serve after acceptance: %+v", got)
	}
	if n := s.serve(chat(), "hello"); n != "a" {
		t.Fatalf("chat after acceptance placed on %q, want a", n)
	}
	for _, w := range g.healthOf("lab", time.Now()) {
		if w.Rule == ruleNoChat {
			t.Fatalf("no-chat warning after acceptance: %+v", w)
		}
	}
	before := strings.Count(logs.String(), "level=WARN")
	g.noChat.mu.Lock()
	g.noChat.last = time.Time{}
	g.noChat.mu.Unlock()
	g.unavailable(httptest.NewRecorder(), chat(), timedOut) // refused for another reason: no WARN, chat has a node
	if strings.Count(logs.String(), "level=WARN") != before {
		t.Fatalf("WARN while a node may serve chat: %s", logs.String())
	}
	_ = b
}

// Every rule that keeps a node from people's requests shows in the counts and names its reason: the
// own-only switch, a draft of the operator terms, a pilot's science-only switch, a pilot not ready.
func TestServeCapacityReasons(t *testing.T) {
	finalTerms(t)
	s := newStickyRig(t)
	g := s.g
	n := s.node("n", "op-n", 0)
	g.mu.Lock()
	n.rules = nodeRules{terms: true}
	g.mu.Unlock()
	check := func(name string, chat, science int, reason string) {
		t.Helper()
		c, online, why := g.serveCapacity(time.Now())
		if online != 1 || c.Chat != chat || c.Science != science || (reason != "" && why[reason] != 1) {
			t.Errorf("%s: %+v online %d why %v; want chat %d science %d why %s", name, c, online, why, chat, science, reason)
		}
	}
	check("accepted", 1, 1, "")

	old := OperatorTermsVersion
	OperatorTermsVersion = "draft-x"
	g.mu.Lock()
	n.rules = nodeRules{terms: false} // a draft is accepted by nobody (rulesFor)
	g.mu.Unlock()
	check("draft terms", 0, 1, whyTermsDraft)
	OperatorTermsVersion = old

	g.mu.Lock()
	n.rules = nodeRules{terms: true}
	n.pilot, n.deadline, n.scienceOnly = "p-1", time.Now().Add(time.Hour), true
	g.mu.Unlock()
	check("pilot science only", 0, 1, whyPilotScience)
	g.mu.Lock()
	n.scienceOnly, n.registering = false, true
	g.mu.Unlock()
	check("pilot registering", 0, 0, whyPilotNotYet)
	g.mu.Lock()
	n.pilot, n.registering = "", false
	g.mu.Unlock()

	g.cfg.ChatRulesOff = true
	g.mu.Lock()
	n.rules = nodeRules{}
	g.mu.Unlock()
	check("rules off", 1, 1, "")
	g.cfg.ChatRulesOff = false
}
