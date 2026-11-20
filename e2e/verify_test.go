// Answer checks and per-node signals (node verification, BACKLOG 11c/11d; gateway/verify.go,
// signals.go, discard.go): a node that pads its answers faster than any GPU, thinks past its budget
// or writes past max_tokens is flagged in observe mode and earns nothing in enforce mode; the usage
// record carries the signals, the task, and the answers a request discarded.
//
// SPDX-License-Identifier: Apache-2.0
package e2e

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aisathome/aisathome/gateway"
	"github.com/aisathome/aisathome/protocol"
)

// deltaChunk is one streamed chunk with delta d and, if set, a finish_reason.
func deltaChunk(id string, d map[string]any, finish string) protocol.Msg {
	ch := map[string]any{"index": 0, "delta": d}
	if finish != "" {
		ch["finish_reason"] = finish
	}
	b, _ := json.Marshal(map[string]any{"choices": []any{ch}})
	return protocol.Msg{Type: protocol.TypeChunk, ReqID: id, Data: b}
}

// paddingNode answers every request at once with n chunks of text (reasoning first when reason > 0
// chunks), as a node that never runs the model would, ending with finish.
func paddingNode(t *testing.T, r *rig, reason, n int, piece, finish string) {
	rawNodeWith(t, r, "pad-node", "node-token", func(write func(protocol.Msg), _ func([]byte), m protocol.Msg) {
		for i := 0; i < reason; i++ {
			write(deltaChunk(m.ReqID, map[string]any{"reasoning_content": piece}, ""))
		}
		for i := 0; i < n; i++ {
			write(deltaChunk(m.ReqID, map[string]any{"content": piece}, ""))
		}
		write(deltaChunk(m.ReqID, map[string]any{}, finish))
		write(protocol.Msg{Type: protocol.TypeDone, ReqID: m.ReqID, Usage: &protocol.Usage{CompletionTokens: n}})
	})
	r.waitNodes(t, 1)
}

func verifyRig(t *testing.T, v gateway.VerifyConfig) *rig {
	return newRigCfg(t, func(c *gateway.Config) { c.TokenizerFile = miniTokenizer; c.Verify = v })
}

func checksOf(u map[string]any) []string {
	var out []string
	cs, _ := u["checks"].([]any)
	for _, c := range cs {
		out = append(out, c.(string))
	}
	return out
}

func signalsOf(t *testing.T, u map[string]any) map[string]any {
	t.Helper()
	s, ok := u["signals"].(map[string]any)
	if !ok {
		t.Fatalf("no signals in %v", u)
	}
	return s
}

// lastRecord is the newest of at least n usage records.
func lastRecord(t *testing.T, r *rig, n int) map[string]any {
	t.Helper()
	u := waitUsage(t, r, n)
	return u[len(u)-1]
}

// Observe (the default): a padded answer, 2,000 chunks at once, is flagged "speed" and keeps its
// credit; its signals say what the node did, numbers only.
func TestAnswerChecksObserve(t *testing.T) {
	r := verifyRig(t, gateway.VerifyConfig{})
	paddingNode(t, r, 0, 2000, "hello hello ", "stop")
	if code, _ := ask(t, r, "user-key", chatBody); code != 200 {
		t.Fatalf("HTTP %d", code)
	}
	u := lastRecord(t, r, 1)
	if got := checksOf(u); len(got) != 1 || got[0] != "speed" {
		t.Fatalf("checks %v, want [speed]: %v", got, u)
	}
	if creditOf(u) <= 0 || u["voided"] != nil {
		t.Fatalf("observe mode changed the credit: credit %v voided %v", u["credit"], u["voided"])
	}
	s := signalsOf(t, u)
	if s["finish"] != "stop" || s["tps"].(float64) < 1000 || s["degenerate"] != true {
		t.Fatalf("signals %v", s)
	}
	if b, _ := json.Marshal(u); strings.Contains(string(b), "hello hello") {
		t.Fatalf("the record holds answer text: %s", b)
	}
}

// Enforce: the same answer earns nothing, the record says what it would have earned, and the
// node's ledger gets nothing; an honest-speed answer is untouched.
func TestAnswerChecksEnforce(t *testing.T) {
	r := verifyRig(t, gateway.VerifyConfig{Mode: gateway.VerifyEnforce})
	paddingNode(t, r, 0, 2000, "hello hello ", "stop")
	ask(t, r, "user-key", chatBody)
	u := lastRecord(t, r, 1)
	if got := checksOf(u); len(got) != 1 || got[0] != "speed" {
		t.Fatalf("checks %v", got)
	}
	if creditOf(u) != 0 || u["voided"] == nil || u["voided"].(float64) <= 0 {
		t.Fatalf("credit %v voided %v: want 0 and the voided amount", u["credit"], u["voided"])
	}
	if c := totalCredit(t, r); c != 0 {
		t.Fatalf("the ledger credited a voided answer: %v", c)
	}
}

// A short answer at once (within the burst) passes in enforce mode and keeps its credit.
func TestAnswerChecksShortAnswerPasses(t *testing.T) {
	r := verifyRig(t, gateway.VerifyConfig{Mode: gateway.VerifyEnforce})
	paddingNode(t, r, 0, 20, "hello ", "stop")
	ask(t, r, "user-key", chatBody)
	u := lastRecord(t, r, 1)
	if len(checksOf(u)) != 0 || creditOf(u) <= 0 {
		t.Fatalf("a short honest answer failed: checks %v credit %v", checksOf(u), u["credit"])
	}
}

// totalCredit is the ledger's total as GET /v1/stats/credit shows it.
func totalCredit(t *testing.T, r *rig) float64 {
	t.Helper()
	resp, err := http.Get(r.gw.URL + "/v1/stats/credit")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var b struct {
		Total *struct {
			Credit *float64 `json:"credit"`
		} `json:"total"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&b); err != nil || resp.StatusCode != 200 || b.Total == nil || b.Total.Credit == nil {
		t.Fatalf("GET /v1/stats/credit: HTTP %d, %v, %+v", resp.StatusCode, err, b)
	}
	return *b.Total.Credit
}

func bodyWith(extra map[string]any) string {
	b := map[string]any{"model": "deep", "stream": true, "messages": []any{map[string]any{"role": "user", "content": "hi"}}}
	for k, v := range extra {
		b[k] = v
	}
	s, _ := json.Marshal(b)
	return string(s)
}

// Reasoning past the request's budget fails "reasoning-budget"; the same reasoning within a larger
// budget passes. The speed rule is out of the way (rate 1e9).
func TestAnswerChecksReasoningBudget(t *testing.T) {
	r := verifyRig(t, gateway.VerifyConfig{Mode: gateway.VerifyEnforce, SpeedRate: 1e9})
	paddingNode(t, r, 300, 2, "think ", "stop")
	ask(t, r, "user-key", bodyWith(map[string]any{"thinking_budget_tokens": 16}))
	u := lastRecord(t, r, 1)
	if got := checksOf(u); len(got) != 1 || got[0] != "reasoning-budget" || creditOf(u) != 0 {
		t.Fatalf("over budget: checks %v credit %v", got, u["credit"])
	}
	if s := signalsOf(t, u); s["reasoning"].(float64) < 300 {
		t.Fatalf("reasoning tokens %v", s["reasoning"])
	}
	ask(t, r, "user-key", bodyWith(map[string]any{"thinking_budget_tokens": 4096}))
	if u := lastRecord(t, r, 2); len(checksOf(u)) != 0 || creditOf(u) <= 0 {
		t.Fatalf("within budget: checks %v credit %v", checksOf(u), u["credit"])
	}
	// No budget in the request: the nodes' default (node_reasoning_budget) applies.
	r2 := verifyRig(t, gateway.VerifyConfig{Mode: gateway.VerifyEnforce, SpeedRate: 1e9, NodeReasoningBudget: 64})
	paddingNode(t, r2, 300, 2, "think ", "stop")
	ask(t, r2, "user-key", chatBody)
	if got := checksOf(lastRecord(t, r2, 1)); len(got) != 1 || got[0] != "reasoning-budget" {
		t.Fatalf("node default budget: checks %v", got)
	}
}

// Output past max_tokens fails "max-tokens" (many tokens per chunk, so the chunk bound does not
// stop it first); an answer cut at max_tokens by the engine is only a signal (finish "length").
func TestAnswerChecksMaxTokens(t *testing.T) {
	r := verifyRig(t, gateway.VerifyConfig{Mode: gateway.VerifyEnforce, SpeedRate: 1e9})
	paddingNode(t, r, 0, 40, strings.Repeat("word ", 40), "length")
	ask(t, r, "user-key", bodyWith(map[string]any{"max_tokens": 100}))
	u := lastRecord(t, r, 1)
	if got := checksOf(u); len(got) != 1 || got[0] != "max-tokens" || creditOf(u) != 0 {
		t.Fatalf("checks %v credit %v", got, u["credit"])
	}
	if s := signalsOf(t, u); s["finish"] != "length" {
		t.Fatalf("finish %v", s["finish"])
	}
}

// Mode off checks nothing.
func TestAnswerChecksOff(t *testing.T) {
	r := verifyRig(t, gateway.VerifyConfig{Mode: gateway.VerifyOff})
	paddingNode(t, r, 0, 2000, "hello hello ", "stop")
	ask(t, r, "user-key", chatBody)
	if u := lastRecord(t, r, 1); len(checksOf(u)) != 0 || creditOf(u) <= 0 {
		t.Fatalf("mode off: checks %v credit %v", checksOf(u), u["credit"])
	}
}

// A tool call whose arguments are not JSON is counted as a bad call; a good one is not.
func TestAnswerSignalsToolCalls(t *testing.T) {
	r := verifyRig(t, gateway.VerifyConfig{})
	rawNodeWith(t, r, "tool-node", "node-token", func(write func(protocol.Msg), _ func([]byte), m protocol.Msg) {
		call := func(i int, name, args string) map[string]any {
			return map[string]any{"tool_calls": []any{map[string]any{"index": i, "id": "c" + name, "type": "function",
				"function": map[string]any{"name": name, "arguments": args}}}}
		}
		write(deltaChunk(m.ReqID, call(0, "read", `{"path":"a.lean"}`), ""))
		write(deltaChunk(m.ReqID, call(1, "edit", `{"path": "a.lean", "old"`), "")) // never closed
		write(deltaChunk(m.ReqID, map[string]any{}, "tool_calls"))
		write(protocol.Msg{Type: protocol.TypeDone, ReqID: m.ReqID, Usage: &protocol.Usage{CompletionTokens: 9}})
	})
	r.waitNodes(t, 1)
	body := `{"model":"deep","stream":true,"messages":[{"role":"user","content":"go"}],"tools":[{"type":"function","function":{"name":"read","parameters":{"type":"object"}}},{"type":"function","function":{"name":"edit","parameters":{"type":"object"}}}]}`
	ask(t, r, "agent-key", body)
	s := signalsOf(t, lastRecord(t, r, 1))
	if s["tool_calls"] != 2.0 || s["bad_calls"] != 1.0 || s["finish"] != "tool_calls" {
		t.Fatalf("signals %v", s)
	}
}

// A requester that sends the same prompt again threw the earlier answer away: the record of the
// request that did names it and the node that served it. One that continues the conversation with
// the answer kept it.
func TestAnswerDiscarded(t *testing.T) {
	r := verifyRig(t, gateway.VerifyConfig{})
	paddingNode(t, r, 0, 3, "Hello ", "stop")
	sys := strings.Repeat("You are a careful assistant who answers briefly. ", 20)
	ask(t, r, "user-key", convBody(sys, "first question"))
	ask(t, r, "user-key", convBody(sys, "first question", "Hello Hello Hello", "second question")) // kept the first
	ask(t, r, "user-key", convBody(sys, "first question", "Hello Hello Hello", "second question")) // discards the second
	u := waitUsage(t, r, 3)
	if len(u) != 3 || u[0]["discarded"] != nil || u[1]["discarded"] != nil {
		t.Fatalf("records %v", u)
	}
	d, _ := u[2]["discarded"].([]any)
	if len(d) != 1 || d[0].(map[string]any)["req_id"] != u[1]["req_id"] || d[0].(map[string]any)["node"] != "pad-node" {
		t.Fatalf("discarded %v, want the second answer (%v) of pad-node", u[2]["discarded"], u[1]["req_id"])
	}
}

// The usage record names the task a project's request named (a well-formed name; the project's key
// vouches for it).
func TestUsageRecordNamesTask(t *testing.T) {
	l := &queueGateway{}
	r := newRigCfg(t, func(c *gateway.Config) {
		c.StateDir = t.TempDir()
		withV7(c)
		l.cfg = *c
	}, mockEngine(t, false))
	l.g, l.srv, l.url = r.g, r.gw, r.gw.URL
	if code, out := l.p2m(t, "POST", "", map[string]any{"id": "t7-a", "kind": "p2m-v7", "created_by": "owner", "role": "hole",
		"spec": map[string]any{"prompt": "private"}}); code != http.StatusCreated {
		t.Fatalf("create: HTTP %d %v", code, out)
	}
	if code, out := l.p2m(t, "POST", "/claim", map[string]any{"worker": "runner-1", "task": "t7-a"}); code != 200 {
		t.Fatalf("claim: HTTP %d %v", code, out)
	}
	askTask(t, l.url, p2mKey, "t7-a", true)
	askTask(t, l.url, p2mKey, "no spaces allowed", true) // not a task name
	askTask(t, l.url, p2mKey, "", true)
	u := waitUsage(t, r, 3)
	if u[0]["task"] != "t7-a" || u[1]["task"] != nil || u[2]["task"] != nil {
		t.Fatalf("tasks %v, %v, %v: want t7-a, none, none", u[0]["task"], u[1]["task"], u[2]["task"])
	}
}

// `accounts node-checks` sums the log per node.
func TestNodeChecksReport(t *testing.T) {
	r := verifyRig(t, gateway.VerifyConfig{Mode: gateway.VerifyEnforce})
	paddingNode(t, r, 0, 2000, "hello hello ", "stop")
	ask(t, r, "user-key", chatBody)
	waitUsage(t, r, 1)
	f, err := os.Open(filepath.Clean(r.usageLog))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	rows, err := gateway.ReadNodeChecks(f, time.Now().Add(-time.Hour))
	if err != nil || len(rows) != 1 {
		t.Fatalf("%v %v", rows, err)
	}
	if row := rows[0]; row.Node != "pad-node" || row.Answers != 1 || row.Failed != 1 || row.ByRule["speed"] != 1 || row.Voided <= 0 ||
		row.Degenerate != 1 || row.Credit != 0 {
		t.Fatalf("row %+v", row)
	}
}

// The operations page shows the answer checks per node since the start, with the mode.
func TestOpsPageShowsAnswerChecks(t *testing.T) {
	a := newAcctRigWith(t, 100, nil, func(c *gateway.Config) {
		withProjects(c)
		// mockEngine writes 3 tokens at once: past a burst of 1 at a crawl of a rate.
		c.Verify = gateway.VerifyConfig{Mode: gateway.VerifyEnforce, SpeedBurst: 1, SpeedRate: 0.001}
	})
	admin, _ := a.signIn(t, 1)
	_, page := a.get(t, "/account/ops", admin)
	if !strings.Contains(page, `id="checks"`) || !strings.Contains(page, "No answers since the gateway started.") ||
		!strings.Contains(page, `<strong data-mode>enforce</strong>`) {
		t.Fatalf("before any answer: %s", page)
	}
	if code, _ := ask(t, a.rig, "user-key", chatBody); code != 200 {
		t.Fatalf("HTTP %d", code)
	}
	waitFor(t, func() bool {
		_, page = a.get(t, "/account/ops", admin)
		return strings.Contains(page, `data-check="node-0"`)
	})
	for _, want := range []string{`<tr data-check="node-0" class="warn">`, `data-failed>1<`, `data-rules>speed 1<`} {
		if !strings.Contains(page, want) {
			t.Fatalf("the page lacks %q: %s", want, page)
		}
	}
}

// A pilot's answer that fails a check earns nothing in its record in enforce mode, while the record
// still counts the request.
func TestPilotAnswerCheckVoidsCredit(t *testing.T) {
	a := newAcctRigNodes(t, 100, nil, func(c *gateway.Config) {
		c.Projects = []gateway.Project{{ID: "lab", KeySHA256: []string{sha(pilotSciKey)}, AgentOK: true, PilotProbationOK: true}}
		c.Connectors = []gateway.Connector{{ID: "boinc", KeySHA256: []string{sha(connectorKey)}, MaxPilots: 5}}
		c.QueueTimeoutSec = 2
		// mockEngine writes 3 tokens at once: past a burst of 1 at a crawl of a rate.
		c.Verify = gateway.VerifyConfig{Mode: gateway.VerifyEnforce, SpeedBurst: 1, SpeedRate: 0.001}
	})
	engine := mockEngine(t, false)
	t.Cleanup(engine.Close)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	id, cred := a.newPilot(t)
	a.runPilot(ctx, cred, engine.URL)
	a.connector(t, "POST", "/pilots/"+id+"/bind", map[string]any{"user": "5", "host": "11"})
	waitUntil(t, "the bound pilot to connect", func() bool { return a.g.NodeCount() == 1 })
	if code, body := ask(t, a.rig, pilotSciKey, deepBody); code != 200 {
		t.Fatalf("science on the pilot: %d %s", code, body)
	}
	u := waitUsage(t, a.rig, 1)[0]
	if u["node"] != id || len(checksOf(u)) != 1 || u["voided"] == nil {
		t.Fatalf("usage %v", u)
	}
	p := a.pilotRecord(t, id)
	if p["requests"] != 1.0 || p["credit"] != 0.0 || p["metered_credit"] != 0.0 {
		t.Fatalf("pilot record after a voided answer: %v", p)
	}
}
