// SPDX-License-Identifier: Apache-2.0
package gateway

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/aisathome/aisathome/protocol"
)

var (
	modelIQ4 = protocol.ModelInfo{Name: "qwen3.8-27b", Roles: []string{"deep", "agent"}, Slots: 1, Ctx: 196608}
	modelQ3  = protocol.ModelInfo{Name: "qwen3.8-27b-q3", Roles: []string{"deep", "agent"}, Slots: 1, Ctx: 81920}
)

// A conversation that continues an answered one is held to the model that answered it; other
// conversations, other requesters and requests that name a model are not.
func TestSessionBinds(t *testing.T) {
	s := newSessionBinds()
	now := time.Now()
	// served: turn k of a conversation was answered by m; the next prompt starts with turn k+1's
	// text minus its last exchange's user message (turn() renders whole exchanges).
	served := func(who, topic string, k int, m protocol.ModelInfo) { s.bind(who, []string{turn(topic, k)}, m, now) }

	served("alice", "proofs", 1, modelQ3)
	if got := s.model("alice", turn("proofs", 2), "deep", now); got != modelQ3.Name {
		t.Fatalf("the next turn is held to %q, want %q", got, modelQ3.Name)
	}
	if got := s.model("alice", turn("proofs", 2), "agent", now); got != modelQ3.Name {
		t.Fatalf("a request for the model's other role is held to %q", got)
	}
	for _, tc := range []struct{ name, who, want, prompt string }{
		{"another conversation of the requester", "alice", "deep", turn("parsers", 2)},
		{"another requester's copy", "bob", "deep", turn("proofs", 2)},
		{"a role the model does not serve", "alice", "fast", turn("proofs", 2)},
		{"a request that names another model", "alice", modelIQ4.Name, turn("proofs", 2)},
		{"the conversation's start only", "alice", "deep", turn("proofs", 0)},
		{"the conversation itself, nothing added", "alice", "deep", turn("proofs", 1)},
		{"no requester", "", "deep", turn("proofs", 2)},
	} {
		if got := s.model(tc.who, tc.prompt, tc.want, now); got != "" {
			t.Errorf("%s: held to %q", tc.name, got)
		}
	}
	// Naming the session's own model is held to it as well (nothing changes for it).
	if got := s.model("alice", turn("proofs", 2), modelQ3.Name, now); got != modelQ3.Name {
		t.Fatalf("naming the model: %q", got)
	}
	// After a day without an answer the session is routed as a new one.
	if got := s.model("alice", turn("proofs", 2), "deep", now.Add(bindTTL+time.Minute)); got != "" {
		t.Fatalf("a day later the session is still held to %q", got)
	}

	// The longest continued conversation decides: a client that switched the session to another
	// model by name keeps it there when it asks for the role again.
	served("alice", "proofs", 2, modelIQ4)
	if got := s.model("alice", turn("proofs", 3), "deep", now); got != modelIQ4.Name {
		t.Fatalf("after the switch by name the session is held to %q", got)
	}
	// An earlier turn (an answer regenerated from there) still finds its own model.
	if got := s.model("alice", turn("proofs", 1)+"<|im_start|>user\nagain", "deep", now); got != modelQ3.Name {
		t.Fatalf("a regenerated turn is held to %q", got)
	}
	// Any of the forms an answer reappears in.
	s.bind("erin", []string{"form one: hi, with reasoning", "form two: hi"}, modelQ3, now)
	if got := s.model("erin", "form two: hi<|im_start|>user\nmore", "deep", now); got != modelQ3.Name {
		t.Fatalf("the second form: %q", got)
	}
}

// A short chat is compared to its end: a new conversation that shares the system prompt (and
// even the first question) with an earlier short one is not held to its model, however long
// the system prompt, while that chat's own next turn is (Codex review, 2026-11-09).
func TestSessionBindsShortChatSharedSystemPrompt(t *testing.T) {
	s := newSessionBinds()
	now := time.Now()
	sys := "<|im_start|>system\n" + strings.Repeat("You are a coding agent with these tools. ", 300) + "<|im_end|>\n"
	q := func(text string) string { return "<|im_start|>user\n" + text + "<|im_end|>\n<|im_start|>assistant\n" }
	earlier := sys + q("hi") + "Hello!<|im_end|>\n"
	s.bind("dave", []string{earlier}, modelQ3, now)
	for _, tc := range []struct{ name, prompt string }{
		{"another first question", sys + q("fix the failing test in parser.go")},
		{"the same first question", sys + q("hi")},
		{"the same question, another answer", sys + q("hi") + "Hi there!<|im_end|>\n" + q("go on")},
		{"the system prompt and some padding", sys + strings.Repeat(" ", len(earlier))},
	} {
		if got := s.model("dave", tc.prompt, "deep", now); got != "" {
			t.Errorf("%s: held to %q", tc.name, got)
		}
	}
	if got := s.model("dave", earlier+q("and now?"), "deep", now); got != modelQ3.Name {
		t.Fatalf("the short chat's own next turn is held to %q", got)
	}
	// Also a conversation under one block is recognised.
	s.bind("carol", []string{q("hi") + "Hello!<|im_end|>\n"}, modelQ3, now)
	if got := s.model("carol", q("hi")+"Hello!<|im_end|>\n"+q("thanks"), "deep", now); got != modelQ3.Name {
		t.Fatalf("a 60-byte chat's next turn is held to %q", got)
	}
}

// A secret placeholder's nonce changes with every request; the session still holds, and other
// placeholder text does not make a conversation match (Codex review, 2026-11-09).
func TestSessionBindsPlaceholderNonce(t *testing.T) {
	s := newSessionBinds()
	now := time.Now()
	conv := func(ph string) string {
		return "<|im_start|>user\nmy key is " + ph + "<|im_end|>\n<|im_start|>assistant\nGot it.<|im_end|>\n"
	}
	s.bind("alice", []string{conv("sk-proj-AISX3f0a01XXXXXXXX")}, modelQ3, now)
	if got := s.model("alice", conv("sk-proj-AISXb71c01XXXXXXXX")+"<|im_start|>user\nuse it", "deep", now); got != modelQ3.Name {
		t.Fatalf("with a new nonce the next turn is held to %q", got)
	}
	if got := s.model("alice", conv("ghp_AISXb71c01XXXXXXXXXXXX")+"<|im_start|>user\nuse it", "deep", now); got != "" {
		t.Fatalf("another kind of secret matched: %q", got)
	}
}

// A placeholder's padding follows its number: the same secret as number 99 in one request and
// number 100 in the next (99 other credentials before it) still continues the session (Codex
// review round 2, 2026-11-09).
func TestSessionBindsPlaceholderPadding(t *testing.T) {
	ph := func(others int) string { // the placeholder of fakeKey after others earlier secrets
		r := newRedactor()
		for i := 0; i < others; i++ {
			r.byPH[fmt.Sprintf("ph-%d", i)] = "secret"
		}
		return r.placeholder(fakeKey)
	}
	p99, p100 := ph(98), ph(99)
	if !strings.Contains(p99, "99X") || !strings.Contains(p100, "100X") || len(p99) != len(p100) {
		t.Fatalf("the test needs numbers 99 and 100 of equal length: %q %q", p99, p100)
	}
	s := newSessionBinds()
	now := time.Now()
	conv := func(ph string) string {
		return "<|im_start|>user\nkey " + ph + " here<|im_end|>\n<|im_start|>assistant\nNoted.<|im_end|>\n"
	}
	s.bind("alice", []string{conv(p99)}, modelQ3, now)
	if got := s.model("alice", conv(p100)+"<|im_start|>user\nuse it", "deep", now); got != modelQ3.Name {
		t.Fatalf("with the placeholder renumbered 99 -> 100 the next turn is held to %q", got)
	}
}

// The memory stays bounded: per requester, and in the number of requesters.
func TestSessionBindsBounded(t *testing.T) {
	s := newSessionBinds()
	now := time.Now()
	for i := 0; i < bindPerWho+10; i++ {
		s.bind("alice", []string{turn("t", 1) + strings.Repeat("y", 256*(i+1))}, modelQ3, now)
	}
	if n := len(s.by["alice"].list); n != bindPerWho {
		t.Fatalf("%d conversations kept, want %d", n, bindPerWho)
	}
	for i := 0; i < bindWhos+5; i++ {
		s.bind(fmt.Sprintf("who-%d", i), []string{turn("t", 1)}, modelQ3, now.Add(time.Duration(i)))
	}
	if n := len(s.by); n != bindWhos {
		t.Fatalf("%d requesters kept, want %d", n, bindWhos)
	}
}

// tierRig: one node per model, the 24 GB file on "big" and the 16 GB file on "std", both
// serving the role.
func tierRig(t *testing.T) (*stickyRig, *nodeConn, *nodeConn) {
	s := newStickyRig(t)
	s.g.start = time.Now().Add(-2 * startupGrace)
	add := func(id string, m protocol.ModelInfo) *nodeConn {
		n := &nodeConn{id: id, operator: "op", trusted: true, pending: map[string]*pendingReq{},
			closed: make(chan struct{}), models: []protocol.ModelInfo{m}}
		s.g.mu.Lock()
		s.g.node[id] = n
		s.g.rememberLocked(n.models, time.Now())
		s.g.mu.Unlock()
		return n
	}
	return s, add("big", modelIQ4), add("std", modelQ3)
}

func free(n *nodeConn) {
	n.mu.Lock()
	n.inflight--
	n.mu.Unlock()
}

// A request held to a model goes only to nodes serving it: also when a node of the other model
// is free and its own is busy. A request with no session takes any node serving the role.
func TestPickHeldToSessionModel(t *testing.T) {
	s, big, std := tierRig(t)
	rt := &route{want: "deep", model: modelQ3.Name, exclude: map[*nodeConn]bool{}}
	for i := 0; i < 3; i++ { // round-robin would alternate
		n, model := s.g.pick(rt)
		if n != std || model != modelQ3.Name {
			t.Fatalf("pick %d: %v %q, want the 16 GB node's model", i, n, model)
		}
		free(n)
	}
	std.inflight = 1 // busy: the request waits rather than move to the other model
	if n, _ := s.g.pick(rt); n != nil {
		t.Fatalf("with its model's node busy the session went to %s", n.id)
	}
	std.inflight = 0
	seen := map[*nodeConn]bool{}
	for i := 0; i < 4; i++ {
		n, _ := s.g.pick(&route{want: "deep", exclude: map[*nodeConn]bool{}})
		seen[n] = true
		free(n)
	}
	if !seen[big] || !seen[std] {
		t.Fatalf("requests without a session did not use both nodes: %v", seen)
	}
}

// When no node serving the session's model is online, the request waits like any request for a
// recently served model (a node restarting comes back within that time), then fails with a
// reason that names the model, never moving to the other model. A node of its model that
// comes back during the wait gets it.
func TestSessionModelOffline(t *testing.T) {
	t.Run("fails after the wait", func(t *testing.T) {
		s, big, std := tierRig(t)
		s.g.mu.Lock()
		delete(s.g.node, std.id) // the 16 GB node left
		s.g.mu.Unlock()
		rt := &route{want: "deep", model: modelQ3.Name, exclude: map[*nodeConn]bool{}}
		start := time.Now()
		n, _, why := s.g.pickWait(context.Background(), rt, start.Add(200*time.Millisecond))
		if n != nil || why != timedOut || time.Since(start) < 200*time.Millisecond {
			t.Fatalf("node %v, %q after %v; want the whole wait and a time-out", n, why, time.Since(start))
		}
		if big.inflight != 0 {
			t.Fatal("the session moved to the other model's node")
		}
		var logs strings.Builder
		s.g.log = slog.New(slog.NewTextHandler(&logs, nil))
		w := httptest.NewRecorder()
		s.g.unavailable(w, rt, why)
		if w.Code != http.StatusServiceUnavailable || w.Header().Get("Retry-After") != "60" ||
			!strings.Contains(w.Body.String(), `this conversation started on \"qwen3.8-27b-q3\", and no node serving that model is online`) {
			t.Fatalf("HTTP %d: %s", w.Code, w.Body)
		}
		if l := logs.String(); !strings.Contains(l, "reason=session_model_offline") || !strings.Contains(l, "session_model=qwen3.8-27b-q3") {
			t.Fatalf("refusal log: %q", l)
		}
		// Its node online but busy: the busy answer names the model it waits for.
		s.g.mu.Lock()
		s.g.node[std.id] = std
		s.g.mu.Unlock()
		std.inflight = 1
		w = httptest.NewRecorder()
		s.g.unavailable(w, rt, timedOut)
		if !strings.Contains(w.Body.String(), `all nodes serving \"qwen3.8-27b-q3\" are busy`) {
			t.Fatalf("busy: %s", w.Body)
		}
	})
	t.Run("its node comes back", func(t *testing.T) {
		s, big, std := tierRig(t)
		s.g.mu.Lock()
		delete(s.g.node, std.id)
		s.g.mu.Unlock()
		got := make(chan *nodeConn, 1)
		go func() {
			n, _, _ := s.g.pickWait(context.Background(), &route{want: "deep", model: modelQ3.Name, exclude: map[*nodeConn]bool{}},
				time.Now().Add(10*time.Second))
			got <- n
		}()
		time.Sleep(100 * time.Millisecond)
		s.g.mu.Lock()
		s.g.node[std.id] = std
		s.g.mu.Unlock()
		s.g.signal()
		if n := <-got; n != std {
			t.Fatalf("got %v, want the 16 GB node that came back", n)
		}
		if big.inflight != 0 {
			t.Fatal("the session moved to the other model's node")
		}
	})
}

// chatPrepare holds a request to its session's model, from the conversation it continues.
func TestChatPrepareSessionModel(t *testing.T) {
	g := newTestGateway(t, Config{})
	defer g.Close()
	c := &chatCaller{acct: KeyInfo{User: "alice"}, userKey: "alice", class: ClassAgent, charge: []string{"alice"}}
	long := strings.Repeat("the proof of the lemma goes by induction on n. ", 100)
	first := prepared(t, g, c, long, nil)
	if first.rt.model != "" {
		t.Fatalf("the first request of a session is held to %q", first.rt.model)
	}
	// The node answered with the 16 GB model, remembered as meterRecord does (bindSession).
	reasoning, answer := "Induction on n.", strings.Repeat("Here is the step. ", 40)
	mt := newMeter(forNode(first.req.body), first.req.conv, nil)
	<-mt.ready
	mt.chunk([]byte(fmt.Sprintf(`{"choices":[{"delta":{"reasoning_content":%q,"content":%q}}]}`, reasoning, answer)))
	_, tail := mt.output()
	g.bindSession(keyHash("alice"), mt, tail, modelQ3, time.Now())
	// The next request carries the answer and a new user message (the template drops the
	// answer's reasoning then), or a tool result (it keeps it).
	// A client may also leave the reasoning out of what it sends back (the template then renders
	// an empty thinking part before a tool result).
	for _, tc := range []struct {
		after     map[string]any
		reasoning bool
	}{
		{map[string]any{"role": "user", "content": "go on"}, true},
		{map[string]any{"role": "tool", "content": "ok", "tool_call_id": "c1"}, true},
		{map[string]any{"role": "tool", "content": "ok", "tool_call_id": "c1"}, false},
	} {
		asst := map[string]any{"role": "assistant", "content": answer}
		if tc.reasoning {
			asst["reasoning_content"] = reasoning
		}
		body := map[string]any{"model": "deep", "messages": []any{map[string]any{"role": "user", "content": long}, asst, tc.after}}
		next, ok := g.chatPrepare(httptest.NewRecorder(), c, body)
		if !ok || next.rt.model != modelQ3.Name {
			t.Fatalf("after a %s message (reasoning sent back: %v) the next turn is held to %q (ok %v), want %q",
				tc.after["role"], tc.reasoning, next.rt.model, ok, modelQ3.Name)
		}
	}
	// A credential in the conversation: its placeholder's nonce is new in every request.
	withKey := long + " The key is " + fakeKey + "."
	keyed := prepared(t, g, c, withKey, nil)
	if !strings.Contains(keyed.req.conv.prompt, "AISX") || strings.Contains(keyed.req.conv.prompt, fakeKey) {
		t.Fatalf("the test needs a redacted prompt: %q", keyed.req.conv.prompt)
	}
	mt = newMeter(forNode(keyed.req.body), keyed.req.conv, nil)
	<-mt.ready
	mt.chunk([]byte(fmt.Sprintf(`{"choices":[{"delta":{"content":%q}}]}`, answer)))
	_, tail = mt.output()
	g.bindSession(keyHash("alice"), mt, tail, modelQ3, time.Now())
	body := map[string]any{"model": "deep", "messages": []any{
		map[string]any{"role": "user", "content": withKey},
		map[string]any{"role": "assistant", "content": answer},
		map[string]any{"role": "user", "content": "use it"}}}
	next, ok := g.chatPrepare(httptest.NewRecorder(), c, body)
	if !ok || next.rt.model != modelQ3.Name {
		t.Fatalf("a conversation with a hidden key is held to %q (ok %v), want %q", next.rt.model, ok, modelQ3.Name)
	}
	if other := prepared(t, g, c, strings.Repeat("a different question. ", 100), nil); other.rt.model != "" {
		t.Fatalf("another conversation is held to %q", other.rt.model)
	}
}

// A switch of model by name in a tool loop holds: once a new user message follows, the template
// drops the reasoning of every answer since the last user message, and the switched-to model's
// answer must still be recognised in that form, or the older binding would route the session
// silently back to the first model (Codex review, 2026-11-10).
func TestSessionSwitchInToolLoopHolds(t *testing.T) {
	g := newTestGateway(t, Config{})
	defer g.Close()
	c := &chatCaller{acct: KeyInfo{User: "alice"}, userKey: "alice", class: ClassAgent, charge: []string{"alice"}}
	long := strings.Repeat("prove that the sum of the first n odd numbers is n squared. ", 60)
	user := map[string]any{"role": "user", "content": long}
	answer := func(job *chatJob, chunk string, m protocol.ModelInfo) {
		t.Helper()
		mt := newMeter(forNode(job.req.body), job.req.conv, nil)
		<-mt.ready
		mt.chunk([]byte(chunk))
		_, tail := mt.output()
		g.bindSession(keyHash("alice"), mt, tail, m, time.Now())
	}
	prep := func(model string, msgs ...any) *chatJob {
		t.Helper()
		job, ok := g.chatPrepare(httptest.NewRecorder(), c, map[string]any{"model": model, "messages": msgs})
		if !ok {
			t.Fatal("prepare failed")
		}
		return job
	}
	// Turn 1, for the role: the 24 GB model answers with a tool call.
	first := prep("deep", user)
	r1, r2 := "Try induction.", "The base case holds; now the step."
	answer(first, fmt.Sprintf(`{"choices":[{"delta":{"reasoning_content":%q,"content":"","tool_calls":[{"index":0,"function":{"name":"bash","arguments":"{\"cmd\":\"ls\"}"}}]}}]}`, r1), modelIQ4)
	asst1 := map[string]any{"role": "assistant", "content": "", "reasoning_content": r1,
		"tool_calls": []any{map[string]any{"id": "c1", "type": "function", "function": map[string]any{"name": "bash", "arguments": `{"cmd":"ls"}`}}}}
	tool := map[string]any{"role": "tool", "content": "lemma.lean", "tool_call_id": "c1"}
	// Turn 2, after the tool result: the client switches to the 16 GB model by name.
	second := prep(modelQ3.Name, user, asst1, tool)
	if second.rt.model != "" {
		t.Fatalf("a request naming another model is held to %q", second.rt.model)
	}
	text := strings.Repeat("By induction on n, the sum is n squared. ", 20)
	answer(second, fmt.Sprintf(`{"choices":[{"delta":{"reasoning_content":%q,"content":%q}}]}`, r2, text), modelQ3)
	asst2 := map[string]any{"role": "assistant", "content": text, "reasoning_content": r2}
	// Turn 3, a new user message, for the role again: it stays on the model it switched to.
	third := prep("deep", user, asst1, tool, asst2, map[string]any{"role": "user", "content": "now in Lean"})
	if third.rt.model != modelQ3.Name {
		t.Fatalf("after the switch by name the next user turn is held to %q, want %q", third.rt.model, modelQ3.Name)
	}
}

// Streamed chunks name the model that answered.
func TestWithModel(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{`{"model":"qwen3.8-27b-q3","choices":[]}`, `{"model":"qwen3.8-27b-q3","choices":[]}`}, // unchanged, byte for byte
		{`{"model":"deep","created":1730000000123,"choices":[]}`, `{"choices":[],"created":1730000000123,"model":"qwen3.8-27b-q3"}`},
		{`{"choices":[{"delta":{"content":"\"model\":\"qwen3.8-27b-q3\""}}]}`, `{"choices":[{"delta":{"content":"\"model\":\"qwen3.8-27b-q3\""}}],"model":"qwen3.8-27b-q3"}`},
		{`not json`, `not json`},
	} {
		if got := string(withModel([]byte(tc.in), "qwen3.8-27b-q3")); got != tc.want {
			t.Errorf("withModel(%s) = %s, want %s", tc.in, got, tc.want)
		}
	}
}
