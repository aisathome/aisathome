// SPDX-License-Identifier: Apache-2.0
package gateway

import (
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aisathome/aisathome/node/modelcheck"
	"github.com/aisathome/aisathome/protocol"
)

// testMCRef writes a synthetic secret reference of n prompts (32 positions each, 20 alternatives,
// a confident top token at every position) and returns its config entry.
func testMCRef(t *testing.T, n int) (ModelCheckReference, *modelcheck.Reference) {
	t.Helper()
	ref := &modelcheck.Reference{Format: modelcheck.Format, Kind: modelcheck.KindReference, Name: "test-secret",
		Created: "2026-11-20", Engine: modelcheck.Engine{Name: "llama.cpp", Build: "b12342"},
		Model: modelcheck.Model{File: "Qwen3.8-27B-UD-IQ4_XS.gguf"}, GPU: modelcheck.GPU{Name: "test"},
		Request: modelcheck.DefaultRequest}
	for i := 0; i < n; i++ {
		ref.Results = append(ref.Results, modelcheck.Result{ID: fmt.Sprintf("s%03d", i), Text: fmt.Sprintf("secret prompt %d", i),
			Tokens: testMCTokens(i, 32)})
	}
	b, err := ref.Encode(true)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "secret.json.gz")
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	return ModelCheckReference{Model: "qwen3.8-27b", File: path, SHA256: modelcheck.SHA256(b)}, ref
}

// testMCTokens is prompt i's answer: at each position the top token has probability 0.7 and 19
// others share the rest.
func testMCTokens(i, n int) []modelcheck.Token {
	var toks []modelcheck.Token
	for p := 0; p < n; p++ {
		var top []modelcheck.Alt
		for k := 0; k < 20; k++ {
			lp := math.Log(0.3 / 19)
			if k == 0 {
				lp = math.Log(0.7)
			}
			top = append(top, modelcheck.Alt{Key: hex.EncodeToString(fmt.Appendf(nil, "t%d-%d-%d", i, p, k)), LP: lp})
		}
		toks = append(toks, modelcheck.Token{Key: top[0].Key, Top: top})
	}
	return toks
}

// skewed is what a different model file says: the same top token, far less sure of it.
func skewed(toks []modelcheck.Token) []modelcheck.Token {
	out := make([]modelcheck.Token, len(toks))
	for p, t := range toks {
		top := make([]modelcheck.Alt, len(t.Top))
		copy(top, t.Top)
		top[0].LP = math.Log(0.4)
		for k := 1; k < len(top); k++ {
			top[k].LP = math.Log(0.6 / 19)
		}
		out[p] = modelcheck.Token{Key: t.Key, Top: top}
	}
	return out
}

func quietLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func testChecker(t *testing.T, cfg ModelCheckConfig, dir string) *modelChecker {
	t.Helper()
	mc, err := newModelChecker(cfg, dir, quietLog())
	if err != nil {
		t.Fatal(err)
	}
	return mc
}

func TestModelCheckEvaluate(t *testing.T) {
	entry, ref := testMCRef(t, 20)
	mc := testChecker(t, ModelCheckConfig{Enabled: true, References: []ModelCheckReference{entry}}, "")
	r := mc.refs["qwen3.8-27b"]
	group := func(f func(i int, toks []modelcheck.Token) mcAnswer) []mcAnswer {
		var g []mcAnswer
		for i := 0; i < 4; i++ {
			g = append(g, f(i, ref.Results[i].Tokens))
		}
		return g
	}
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	// The same numbers pass; an answer that goes on past the reference's max_tokens is cut.
	honest := group(func(i int, toks []modelcheck.Token) mcAnswer {
		long := append(append([]modelcheck.Token(nil), toks...), testMCTokens(99, 10)...)
		return mcAnswer{id: ref.Results[i].ID, toks: long}
	})
	if c := mc.evaluate(r, honest, now); c.Verdict != modelcheck.Pass || c.KL > 1e-9 || c.Flips != 0 || c.Positions != 4*32 || c.Diverged != 0 || len(c.Prompts) != 4 {
		t.Errorf("honest: %+v", c)
	}
	// Another model file: the same tokens, other probabilities.
	wrong := group(func(i int, toks []modelcheck.Token) mcAnswer {
		return mcAnswer{id: ref.Results[i].ID, toks: skewed(toks)}
	})
	if c := mc.evaluate(r, wrong, now); c.Verdict != modelcheck.Fail || c.KL < mc.th.MaxCheckKL || len(c.Reasons) == 0 {
		t.Errorf("skewed: %+v", c)
	}
	// Confident flips: the answer takes the second token where the reference was sure.
	flipped := group(func(i int, toks []modelcheck.Token) mcAnswer {
		f := append([]modelcheck.Token(nil), toks...)
		f[3] = modelcheck.Token{Key: f[3].Top[1].Key, Top: f[3].Top}
		return mcAnswer{id: ref.Results[i].ID, toks: f}
	})
	if c := mc.evaluate(r, flipped, now); c.Verdict != modelcheck.Fail || c.Flips != 4 {
		t.Errorf("flips: %+v", c)
	}
	// An answer without logprobs fails the check (honest llama-server always sends them).
	nolp := group(func(i int, toks []modelcheck.Token) mcAnswer {
		if i == 2 {
			return mcAnswer{id: ref.Results[i].ID, bad: "an answer without logprobs"}
		}
		return mcAnswer{id: ref.Results[i].ID, toks: toks}
	})
	if c := mc.evaluate(r, nolp, now); c.Verdict != modelcheck.Fail || len(c.Reasons) != 1 || !strings.Contains(c.Reasons[0], "1 of 4 answers unusable: an answer without logprobs") {
		t.Errorf("no logprobs: %+v", c)
	}
	// Positions that are not log-probabilities cannot pass.
	broken := group(func(i int, toks []modelcheck.Token) mcAnswer {
		f := append([]modelcheck.Token(nil), toks...)
		f[0] = modelcheck.Token{Key: f[0].Key, Top: []modelcheck.Alt{{Key: f[0].Key, LP: 0.5}}}
		return mcAnswer{id: ref.Results[i].ID, toks: f}
	})
	if c := mc.evaluate(r, broken, now); c.Verdict != modelcheck.Fail {
		t.Errorf("broken logprobs: %+v", c)
	}
	// A shorter reference (it ended by itself): an answer that goes on differs there, not confidently.
	short := *r
	short.byID = map[string]*modelcheck.Result{}
	for id, res := range r.byID {
		c := *res
		c.Tokens = c.Tokens[:20]
		short.byID[id] = &c
	}
	sref := *r.ref
	short.ref = &sref
	if c := mc.evaluate(&short, honest, now); c.Verdict != modelcheck.Pass || c.Diverged != 4 || c.Flips != 0 {
		t.Errorf("went on past a short reference: %+v", c)
	}
}

func TestModelCheckStatus(t *testing.T) {
	th := modelcheck.DefaultThresholds
	pass := ModelCheckResult{Verdict: modelcheck.Pass, KL: 0.002}
	fail := ModelCheckResult{Verdict: modelcheck.Fail, KL: 0.03}
	for _, c := range []struct {
		checks []ModelCheckResult
		want   string
	}{
		{nil, MCUnknown},
		{[]ModelCheckResult{pass, pass}, MCPass},
		{[]ModelCheckResult{pass, fail, pass}, MCSuspect},
		{[]ModelCheckResult{fail, pass, fail}, MCFail},
		{[]ModelCheckResult{pass, {Verdict: modelcheck.Pass, Flips: 2}, {Verdict: modelcheck.Pass, Flips: 1}}, MCFail}, // 3 > 2 over the window
	} {
		n := &ModelCheckNode{Checks: c.checks}
		if got, _, _ := n.Status(th); got != c.want {
			t.Errorf("%+v: %s, want %s", c.checks, got, c.want)
		}
	}
	n := &ModelCheckNode{Checks: []ModelCheckResult{pass, fail}}
	if _, worst, _ := n.Status(th); worst != 0.03 {
		t.Errorf("worst KL %v", worst)
	}
}

func TestModelCheckPromptChoice(t *testing.T) {
	entry, _ := testMCRef(t, 16)
	mc := testChecker(t, ModelCheckConfig{Enabled: true, References: []ModelCheckReference{entry}}, "")
	r := mc.refs["qwen3.8-27b"]
	a, b := &ModelCheckNode{Node: "a"}, &ModelCheckNode{Node: "b"}
	seen := map[string]bool{}
	for i := 0; i < 16; i++ {
		id := mc.pickPromptLocked(a, r)
		if seen[id] {
			t.Fatalf("prompt %s sent twice to one node before the pool was used up", id)
		}
		seen[id] = true
	}
	// Node b gets the least used prompts first: here every prompt has been sent once, so any; after
	// b's first, that one is used twice and b never gets it again in this round.
	first := mc.pickPromptLocked(b, r)
	if mc.st.Uses[first] != 2 {
		t.Errorf("uses of %s: %d", first, mc.st.Uses[first])
	}
	// a has had them all: its list starts again.
	id := mc.pickPromptLocked(a, r)
	if len(a.Used) != 1 || a.Used[0] != id {
		t.Errorf("after the pool: used %v", a.Used)
	}
	// Least used first: a's new pick is not b's (used twice already), unless all are equal.
	if id == first {
		t.Errorf("picked %s, used %d times, while others were used once", id, mc.st.Uses[id]-1)
	}
}

func TestModelCheckConfig(t *testing.T) {
	entry, ref := testMCRef(t, 20)
	ok := func(mod func(*ModelCheckConfig)) error {
		c := ModelCheckConfig{Enabled: true, References: []ModelCheckReference{entry}}
		mod(&c)
		_, err := newModelChecker(c, "", quietLog())
		return err
	}
	if err := ok(func(*ModelCheckConfig) {}); err != nil {
		t.Fatal(err)
	}
	if mc := testChecker(t, ModelCheckConfig{Enabled: true, References: []ModelCheckReference{entry}}, ""); mc.pilotIdleFor() != 10*time.Minute {
		t.Errorf("default pilot_idle_seconds: %v", mc.pilotIdleFor())
	}
	bad := map[string]func(*ModelCheckConfig){
		"no references": func(c *ModelCheckConfig) { c.References = nil },
		"no pin":        func(c *ModelCheckConfig) { c.References[0].SHA256 = "" },
		"wrong pin": func(c *ModelCheckConfig) {
			c.References[0].SHA256 = "0000000000000000000000000000000000000000000000000000000000000000"
		},
		"missing file": func(c *ModelCheckConfig) { c.References[0].File = filepath.Join(t.TempDir(), "none") },
		"twice":        func(c *ModelCheckConfig) { c.References = append(c.References, c.References[0]) },
		"cover share":  func(c *ModelCheckConfig) { v := 1.5; c.CoverShare = &v },
		"interval":     func(c *ModelCheckConfig) { c.IntervalSec = 5 },
		"pilot idle":   func(c *ModelCheckConfig) { c.PilotIdleSec = 30 },
	}
	for name, mod := range bad {
		c := ModelCheckConfig{Enabled: true, References: []ModelCheckReference{entry}}
		c.References = append([]ModelCheckReference(nil), c.References...)
		mod(&c)
		if _, err := newModelChecker(c, "", quietLog()); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	// A reference recorded with thinking on, or too small a pool, is refused.
	ref.Request.Thinking = true
	b, _ := ref.Encode(true)
	path := filepath.Join(t.TempDir(), "thinking.json.gz")
	os.WriteFile(path, b, 0o600)
	if err := ok(func(c *ModelCheckConfig) {
		c.References = []ModelCheckReference{{Model: "qwen3.8-27b", File: path, SHA256: modelcheck.SHA256(b)}}
	}); err == nil {
		t.Error("a thinking reference was accepted")
	}
	small, _ := testMCRef(t, 8)
	if err := ok(func(c *ModelCheckConfig) { c.References = []ModelCheckReference{small} }); err == nil {
		t.Error("a pool of 8 prompts was accepted")
	}
}

func TestModelCheckWithholdWaits(t *testing.T) {
	entry, _ := testMCRef(t, 20)
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	mc := testChecker(t, ModelCheckConfig{Enabled: true, Withhold: true, References: []ModelCheckReference{entry}, Now: clock}, "")
	if mc.withholding() {
		t.Error("withholding from the start")
	}
	now = now.Add(ModelCheckObserveFirst - time.Minute)
	if mc.withholding() {
		t.Error("withholding before 3 days of observing")
	}
	now = now.Add(2 * time.Minute)
	if !mc.withholding() {
		t.Error("not withholding after 3 days")
	}
	off := testChecker(t, ModelCheckConfig{Enabled: true, References: []ModelCheckReference{entry}, Now: clock}, "")
	if off.withholding() {
		t.Error("withholding with the switch off")
	}
}

func TestModelCheckSaved(t *testing.T) {
	entry, _ := testMCRef(t, 20)
	dir := t.TempDir()
	start := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	now := start
	cfg := ModelCheckConfig{Enabled: true, References: []ModelCheckReference{entry}, Now: func() time.Time { return now }}
	mc := testChecker(t, cfg, dir)
	mc.st.Nodes["n-1"] = &ModelCheckNode{Node: "n-1", Operator: "op", Model: "qwen3.8-27b", Pass: 3, Used: []string{"s001"},
		Checks: []ModelCheckResult{{At: start, Verdict: modelcheck.Pass, KL: 0.001, Prompts: []string{"s001"}}}, LastSeen: start}
	mc.st.Nodes["old"] = &ModelCheckNode{Node: "old", LastSeen: start.Add(-mcKeepNodes - time.Hour)}
	mc.dirty = true
	if err := mc.save(); err != nil {
		t.Fatal(err)
	}
	st, err := ReadModelCheckState(dir)
	if err != nil {
		t.Fatal(err)
	}
	if st.Nodes["old"] != nil {
		t.Error("a node not seen for 30 days was kept")
	}
	if n := st.Nodes["n-1"]; n == nil || n.Pass != 3 || len(n.Checks) != 1 || n.Used[0] != "s001" || !st.ObservingSince.Equal(start) {
		t.Errorf("saved %+v", st)
	}
	// A restart keeps the observing start: the 3 days are not counted again.
	now = start.Add(2 * time.Hour)
	again := testChecker(t, cfg, dir)
	if !again.st.ObservingSince.Equal(start) || again.st.Nodes["n-1"] == nil {
		t.Errorf("after a restart: since %v, nodes %v", again.st.ObservingSince, again.st.Nodes)
	}
	// A check that was off for more than a day starts observing again.
	now = start.Add(48 * time.Hour)
	later := testChecker(t, cfg, dir)
	if !later.st.ObservingSince.Equal(now) {
		t.Errorf("after two days off: observing since %v, want %v", later.st.ObservingSince, now)
	}
	// The file holds prompt ids, never their text.
	b, _ := os.ReadFile(filepath.Join(dir, ModelCheckFile))
	if strings.Contains(string(b), "secret prompt") || len(b) == 0 {
		t.Errorf("result file holds prompt text or is empty")
	}
}

// A probe takes only a free slot of a node that may get requests, and never one a waiting request
// could have.
func TestModelCheckReserve(t *testing.T) {
	g := &Gateway{node: map[string]*nodeConn{}, waiting: map[*route]struct{}{}}
	n := &nodeConn{id: "n-1", models: []protocol.ModelInfo{{Name: "qwen3.8-27b", Slots: 1}}}
	g.node[n.id] = n
	if !g.mcReserve(n, "qwen3.8-27b") || n.inflight != 1 {
		t.Fatalf("a free node: inflight %d", n.inflight)
	}
	if g.mcReserve(n, "qwen3.8-27b") {
		t.Error("a full node gave a slot")
	}
	n.inflight = 0
	for name, mod := range map[string]func(){
		"paused":      func() { n.paused = true },
		"held":        func() { n.held = true },
		"registering": func() { n.registering = true },
		"waiting":     func() { g.waiting[&route{}] = struct{}{} },
		"replaced":    func() { g.node[n.id] = &nodeConn{id: n.id} },
		"past its deadline": func() {
			n.pilot, n.deadline = "p-1", time.Now().Add(-time.Minute)
		},
	} {
		mod()
		if g.mcReserve(n, "qwen3.8-27b") {
			t.Errorf("%s: a slot was taken", name)
		}
		n.paused, n.held, n.registering, n.pilot, n.deadline, n.inflight = false, false, false, "", time.Time{}, 0
		g.waiting = map[*route]struct{}{}
		g.node[n.id] = n
	}
}

// Cover logprobs go only on requests without tools (llama-server refuses logprobs with tools
// when streaming) to nodes the check is for.
func TestModelCheckCover(t *testing.T) {
	entry, _ := testMCRef(t, 20)
	one := 1.0
	mc := testChecker(t, ModelCheckConfig{Enabled: true, References: []ModelCheckReference{entry}, CoverShare: &one}, "")
	checked := &nodeConn{id: "a", models: []protocol.ModelInfo{{Name: "qwen3.8-27b", Slots: 1}}}
	other := &nodeConn{id: "c", models: []protocol.ModelInfo{{Name: "qwen3.8-27b-q3", Slots: 1}}}
	chat := map[string]any{"messages": []any{}}
	tools := map[string]any{"messages": []any{}, "tools": []any{map[string]any{"type": "function"}}}
	if mc.coverFor(checked, chat) {
		t.Error("cover on a node that has not passed a check (its engine may refuse logprobs)")
	}
	mc.st.Nodes["a"] = &ModelCheckNode{Node: "a", Pass: 1}
	if !mc.coverFor(checked, chat) {
		t.Error("no cover on a chat to a checked node at share 1")
	}
	for name, c := range map[string]struct {
		n    *nodeConn
		body map[string]any
	}{"tools": {checked, tools}, "model without a reference": {other, chat}} {
		if mc.coverFor(c.n, c.body) {
			t.Errorf("%s: cover", name)
		}
	}
	// Every node is checked, a config token's (the maintainer's, once "trusted") as any other.
	if !mc.checks(&nodeConn{id: "home", operator: "owner", models: checked.models}) {
		t.Error("a config token's node is not checked")
	}
	zero := 0.0
	off := testChecker(t, ModelCheckConfig{Enabled: true, References: []ModelCheckReference{entry}, CoverShare: &zero}, "")
	if off.coverFor(checked, chat) {
		t.Error("cover at share 0")
	}
	var none *modelChecker
	if none.coverFor(checked, chat) || none.checks(checked) {
		t.Error("cover or checks with the check off")
	}
	// The default share is a fraction: about defaultCoverShare of 2000 requests.
	def := testChecker(t, ModelCheckConfig{Enabled: true, References: []ModelCheckReference{entry}}, "")
	def.st.Nodes["a"] = &ModelCheckNode{Node: "a", Pass: 1}
	n := 0
	for i := 0; i < 2000; i++ {
		if def.coverFor(checked, chat) {
			n++
		}
	}
	if n < 300 || n > 500 {
		t.Errorf("default cover on %d of 2000", n)
	}
}

// A probe the node answers with an error counts as a bad answer; one it never answered (it left,
// stalled, a person took the slot) does not count. A group never mixes two models' prompts: a node
// that came back with another model starts its group again (code review, 2026-11-20: mixing them
// crashed the comparison).
func TestModelCheckAnswers(t *testing.T) {
	big, ref := testMCRef(t, 20)
	small, _ := testMCRef(t, 20)
	small.Model = "qwen3.8-27b-q3"
	mc := testChecker(t, ModelCheckConfig{Enabled: true, References: []ModelCheckReference{big, small}}, "")
	g := &Gateway{mc: mc, log: quietLog()}
	n := &nodeConn{id: "n-1", operator: "op"}
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	s := &ModelCheckNode{Node: "n-1"}
	rb, rs := mc.refs["qwen3.8-27b"], mc.refs["qwen3.8-27b-q3"]
	ans := func(i int, model string) mcAnswer {
		return mcAnswer{id: ref.Results[i].ID, model: model, toks: ref.Results[i].Tokens}
	}
	mc.mu.Lock()
	defer mc.mu.Unlock()
	g.mcAnswerLocked(n, s, rb, ans(0, "qwen3.8-27b"), true, "", now)
	g.mcAnswerLocked(n, s, rb, ans(1, "qwen3.8-27b"), false, "node left", now)
	if len(mc.pending["n-1"]) != 1 || s.Dropped != 1 {
		t.Fatalf("pending %d, dropped %d", len(mc.pending["n-1"]), s.Dropped)
	}
	// Back with the standard tier's model: the big tier's answer is dropped from the group.
	g.mcAnswerLocked(n, s, rs, ans(2, "qwen3.8-27b-q3"), true, "", now)
	if p := mc.pending["n-1"]; len(p) != 1 || p[0].model != "qwen3.8-27b-q3" {
		t.Fatalf("mixed group: %+v", p)
	}
	g.mcAnswerLocked(n, s, rs, ans(3, "qwen3.8-27b-q3"), false, mcNodeError+"engine returned 400", now)
	g.mcAnswerLocked(n, s, rs, ans(4, "qwen3.8-27b-q3"), true, "", now)
	g.mcAnswerLocked(n, s, rs, ans(5, "qwen3.8-27b-q3"), true, "", now)
	if len(s.Checks) != 1 || s.Checks[0].Verdict != modelcheck.Fail || !strings.Contains(s.Checks[0].Reasons[0], "answered with an error") {
		t.Fatalf("a group with an error answer: %+v", s.Checks)
	}
	// An answer whose prompt the reference lacks is never compared (no crash).
	c := mc.evaluate(rb, []mcAnswer{{id: "not-in-the-pool", model: "qwen3.8-27b"}, ans(0, "qwen3.8-27b"), ans(1, "qwen3.8-27b"), ans(2, "qwen3.8-27b")}, now)
	if c.Verdict != modelcheck.Fail || len(c.Reasons) == 0 {
		t.Errorf("a group of 3 comparable answers: %+v", c)
	}
}

// When a node's list starts again, the check in progress still gets no prompt twice.
func TestModelCheckNoRepeatInAGroup(t *testing.T) {
	entry, _ := testMCRef(t, 16)
	mc := testChecker(t, ModelCheckConfig{Enabled: true, References: []ModelCheckReference{entry}}, "")
	r := mc.refs["qwen3.8-27b"]
	s := &ModelCheckNode{Node: "n-1"}
	for i := 0; i < 15; i++ {
		mc.pickPromptLocked(s, r)
	}
	last := mc.pickPromptLocked(s, r) // the 16th: the pool is used up
	mc.pending["n-1"] = []mcAnswer{{id: last}}
	for i := 0; i < 40; i++ {
		s.Used = append([]string(nil), r.ids...) // used up again
		if id := mc.pickPromptLocked(s, r); id == last {
			t.Fatalf("prompt %s twice in one group after the list started again", id)
		}
	}
}

// A request waiting for a node that could use this one stops a probe on it.
func TestModelCheckYields(t *testing.T) {
	n := &nodeConn{id: "n-1", models: []protocol.ModelInfo{{Name: "qwen3.8-27b", Roles: []string{"deep"}, Slots: 1, Ctx: 8192}}}
	g := &Gateway{node: map[string]*nodeConn{"n-1": n}, waiting: map[*route]struct{}{}}
	if g.mcWanted(n) {
		t.Error("wanted with nothing waiting")
	}
	g.waiting[&route{want: "deep", class: ClassScience}] = struct{}{}
	if !g.mcWanted(n) {
		t.Error("a waiting science request that could use the node does not stop the probe")
	}
	g.waiting = map[*route]struct{}{{want: "other-model", class: ClassChat}: {}}
	if g.mcWanted(n) {
		t.Error("a request for another model stops the probe")
	}
}
