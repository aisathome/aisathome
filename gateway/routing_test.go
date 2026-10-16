// SPDX-License-Identifier: Apache-2.0
package gateway

import (
	"fmt"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/aisathome/aisathome/protocol"
)

// stickyRig is a gateway with hand-made node connections, to test pick without a network.
type stickyRig struct {
	t *testing.T
	g *Gateway
}

func newStickyRig(t *testing.T) *stickyRig {
	g, err := New(Config{StateDir: t.TempDir()}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { g.Close() })
	return &stickyRig{t, g}
}

func (s *stickyRig) node(id, operator string, trusted bool, ctx int) *nodeConn {
	n := &nodeConn{id: id, operator: operator, trusted: trusted, pending: map[string]*pendingReq{},
		closed: make(chan struct{}), models: []protocol.ModelInfo{{Name: "m", Roles: []string{"deep"}, Slots: 1, Ctx: ctx}}}
	s.g.mu.Lock()
	s.g.node[id] = n
	s.g.mu.Unlock()
	return n
}

// turn is the rendered prompt of turn k of a conversation: a long system prompt, then k
// exchanges. Each turn extends the previous one.
func turn(topic string, k int) string {
	var b strings.Builder
	b.WriteString("<|im_start|>system\n" + strings.Repeat("You are an agent working on "+topic+". ", 200) + "<|im_end|>\n")
	for i := 0; i < k; i++ {
		b.WriteString("<|im_start|>user\nstep " + strings.Repeat("x", 3000) + "<|im_end|>\n<|im_start|>assistant\ndone<|im_end|>\n")
	}
	return b.String()
}

func (s *stickyRig) route(who, prompt string) *route {
	return &route{want: "deep", who: who, conv: s.g.pcache.blockHashes(prompt), exclude: map[*nodeConn]bool{}}
}

// serve picks a node for rt, has it "serve" prompt (remembered as the credit path does) and
// frees the slot again. It returns the node's id ("" = none free).
func (s *stickyRig) serve(rt *route, prompt string) string {
	n, _ := s.g.pick(rt)
	if n == nil {
		return ""
	}
	s.g.pcache.servedHashed(n.id, rt.who, rt.conv, prompt, "", time.Now(), true)
	n.mu.Lock()
	n.inflight--
	n.mu.Unlock()
	return n.id
}

// A conversation returns to the node that served it while that node is free; the same
// requester's other conversation and another requester's copy of it do not follow it there.
func TestPickSticky(t *testing.T) {
	s := newStickyRig(t)
	s.node("a", "op", true, 0)
	s.node("b", "op", true, 0)
	first := s.serve(s.route("alice", turn("proofs", 0)), turn("proofs", 0))
	for k := 1; k <= 5; k++ {
		if got := s.serve(s.route("alice", turn("proofs", k)), turn("proofs", k)); got != first {
			t.Fatalf("turn %d went to %s, the conversation is on %s", k, got, first)
		}
	}
	// Round-robin points at the other node now (sticky picks do not move it).
	other := s.serve(s.route("alice", turn("tactics", 0)), turn("tactics", 0))
	if other == first {
		t.Fatalf("a new conversation of alice's went to %s by her other conversation", other)
	}
	for k := 1; k <= 3; k++ { // interleaved, each stays where it is
		if got := s.serve(s.route("alice", turn("proofs", 5+k)), turn("proofs", 5+k)); got != first {
			t.Fatalf("proofs turn %d: %s, want %s", 5+k, got, first)
		}
		if got := s.serve(s.route("alice", turn("tactics", k)), turn("tactics", k)); got != other {
			t.Fatalf("tactics turn %d: %s, want %s", k, got, other)
		}
	}
	// Round-robin points at first again: bob's copy of the tactics conversation goes there.
	if got := s.serve(s.route("bob", turn("tactics", 3)), turn("tactics", 3)); got == other {
		t.Fatalf("bob's request followed alice's conversation to %s", got)
	}
}

// When the node holding the conversation is busy, the request takes another free node at
// once instead of waiting; afterwards the conversation follows its longest copy.
func TestPickStickyBusyGoesElsewhere(t *testing.T) {
	s := newStickyRig(t)
	s.node("a", "op", true, 0)
	s.node("b", "op", true, 0)
	home := s.serve(s.route("alice", turn("p", 1)), turn("p", 1))
	busy := s.g.node[home]
	busy.inflight = 1 // its one slot is taken
	away := s.serve(s.route("alice", turn("p", 2)), turn("p", 2))
	if away == "" || away == home {
		t.Fatalf("with %s busy the request went to %q", home, away)
	}
	busy.inflight = 0
	if got := s.serve(s.route("alice", turn("p", 3)), turn("p", 3)); got != away {
		t.Fatalf("turn 3 went to %s; %s holds the longer prefix", got, away)
	}
}

// Of two nodes holding a prefix, the longer prefix wins; at equal length, the more recent.
// Short prompts (under stickyMin) are not sticky at all.
func TestPickStickyLongestAndShort(t *testing.T) {
	s := newStickyRig(t)
	a, b := s.node("a", "op", true, 0), s.node("b", "op", true, 0)
	now := time.Now()
	s.g.pcache.served("a", "alice", turn("p", 3), "", now, true)
	s.g.pcache.served("b", "alice", turn("p", 1), "", now.Add(time.Second), true)
	if n, _ := s.g.pick(s.route("alice", turn("p", 4))); n != a {
		t.Fatalf("picked %s, a holds the longer prefix", n.id)
	}
	a.inflight = 0
	s.g.pcache.served("b", "alice", turn("q", 1), "", now, true)
	s.g.pcache.served("a", "alice", turn("q", 1), "", now.Add(time.Second), true)
	if n, _ := s.g.pick(s.route("alice", turn("q", 2))); n != a {
		t.Fatalf("picked %s, a served the same prefix more recently", n.id)
	}
	a.inflight = 0
	short := strings.Repeat("s", stickyMin*cacheBlock-1)
	s.g.pcache.served("b", "alice", short, "", now, true)
	s.g.rr = 0 // round-robin starts at a
	if n, _ := s.g.pick(s.route("alice", short+"more")); n != a {
		t.Fatalf("a %d-byte prefix made the pick sticky (%s)", len(short), n.id)
	}
	_ = b
}

// Placement policy comes first: stickiness only orders the nodes the request may use
// anyway.
func TestPickStickyPolicyFirst(t *testing.T) {
	prompt := turn("p", 3)
	cases := []struct {
		name string
		set  func(s *stickyRig, rt *route, holder *nodeConn)
	}{
		{"trusted-only switch", func(s *stickyRig, rt *route, h *nodeConn) { rt.trustedOnly = true }},
		{"trusted (personal data)", func(s *stickyRig, rt *route, h *nodeConn) { rt.trusted = true }},
		{"own nodes only", func(s *stickyRig, rt *route, h *nodeConn) { rt.onlySelf = true }},
		{"prefers own nodes", func(s *stickyRig, rt *route, h *nodeConn) { rt.preferSelf = true }},
		{"only operator", func(s *stickyRig, rt *route, h *nodeConn) { rt.operator = "me" }},
		{"excluded after a failure", func(s *stickyRig, rt *route, h *nodeConn) { rt.exclude[h] = true }},
		{"held by a guard", func(s *stickyRig, rt *route, h *nodeConn) { h.held = true }},
		{"agent without consent", func(s *stickyRig, rt *route, h *nodeConn) { rt.tools = true }},
		{"context too small", func(s *stickyRig, rt *route, h *nodeConn) { rt.need, rt.needMin = 30000, 9000 }},
		{"paused", func(s *stickyRig, rt *route, h *nodeConn) { h.paused = true }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := newStickyRig(t)
			vol := s.node("vol", "volunteer", false, 16384) // holds the conversation
			own := s.node("own", "me", true, 32768)         // the requester's own node, trusted, free
			for i := 0; i < 6; i++ {                        // others served lately: vol is under its share
				s.g.shares = append(s.g.shares, shareEntry{time.Now(), "someone", 0})
			}
			s.g.pcache.served("vol", "alice", prompt, "", time.Now(), true)
			rt := s.route("alice", prompt+"next")
			rt.self = "me"
			if n, _ := s.g.pick(rt); n != vol { // the control: without the restriction it sticks
				t.Fatalf("control: picked %v", n)
			}
			vol.inflight = 0
			s.g.rr = 0 // round-robin starts at own: only a sticky pick that ignores the policy picks vol
			c.set(s, rt, vol)
			if n, _ := s.g.pick(rt); n != own {
				t.Fatalf("picked %v, want the requester's own node", n)
			}
		})
	}
}

// The operator share cap applies before stickiness: a volunteer over its share serves the
// conversation only when no node under it is free.
func TestPickStickyShareCap(t *testing.T) {
	s := newStickyRig(t)
	full := s.node("full", "busyop", false, 0)
	free := s.node("free", "otherop", false, 0)
	now := time.Now()
	for i := 0; i < 5; i++ {
		s.g.shares = append(s.g.shares, shareEntry{now, "busyop", 0})
	}
	s.g.pcache.served("full", "alice", turn("p", 2), "", now, true)
	if n, _ := s.g.pick(s.route("alice", turn("p", 3))); n != free {
		t.Fatalf("picked %v; the holder is over its share", n)
	}
	if n, _ := s.g.pick(s.route("alice", turn("p", 3))); n != full { // free is busy now: the holder serves
		t.Fatalf("picked %v with only the holder free", n)
	}
}

// The share cap counts conversations, not requests: an agent session of many turns stays on
// the volunteer node holding its cache, and that node still gets its share of other people's
// new conversations, which respect the cap.
func TestShareCapCountsConversations(t *testing.T) {
	s := newStickyRig(t)
	for _, op := range []string{"a", "b", "c"} {
		s.node(op, op, false, 0)
	}
	home := s.serve(s.route("alice", turn("session", 0)), turn("session", 0))
	got := map[string]int{home: 1} // conversations per node (and operator)
	for k := 1; k <= 60; k++ {
		if n := s.serve(s.route("alice", turn("session", k)), turn("session", k)); n != home {
			t.Fatalf("turn %d of the session went to %s; the session is on %s", k, n, home)
		}
		if k%3 != 0 {
			continue
		}
		for _, who := range []string{"bob", "carol"} {
			topic := fmt.Sprintf("%s-%d", who, k)
			s.g.mu.Lock()
			over, _ := s.g.overShareLocked(time.Now(), 0)
			under, any := map[string]bool{}, false
			for _, op := range []string{"a", "b", "c"} {
				under[op] = !over(op)
				any = any || under[op]
			}
			s.g.mu.Unlock()
			n := s.serve(s.route(who, turn(topic, 0)), turn(topic, 0))
			if any && !under[n] {
				t.Fatalf("%s's new conversation went to %s, over its share (under: %v)", who, n, under)
			}
			got[n]++
		}
	}
	// 41 conversations: about 14 each. Counting every request would put home far over its
	// share and leave it none of the new conversations.
	for op, c := range got {
		if c < 12 || c > 15 {
			t.Fatalf("conversations per operator %v; %s has %d", got, op, c)
		}
	}
}

// A requester starting many new conversations is still capped, even when they share a long
// system prompt that makes each new one stick to the node that served the last.
func TestShareCapNewConversations(t *testing.T) {
	s := newStickyRig(t)
	for _, op := range []string{"a", "b", "c"} {
		s.node(op, op, false, 0)
	}
	got := map[string]int{}
	for i := 0; i < 30; i++ {
		p := turn("shared", 0) + fmt.Sprintf("<|im_start|>user\ntask %d %s<|im_end|>\n", i, strings.Repeat("y", 3000))
		got[s.serve(s.route("alice", p), p)]++
	}
	for op, c := range got {
		if c < 9 || c > 11 {
			t.Fatalf("conversations per operator %v; %s has %d", got, op, c)
		}
	}
}

// A session longer than the share window counts again once per window, so its operator's
// share stays a count of the conversations it saw lately; short prompts count every time.
func TestShareCapOncePerWindow(t *testing.T) {
	s := newStickyRig(t)
	s.node("a", "a", false, 0)
	shares := func() int { s.g.mu.Lock(); defer s.g.mu.Unlock(); return len(s.g.shares) }
	for k := 0; k <= 3; k++ {
		s.serve(s.route("alice", turn("session", k)), turn("session", k))
	}
	if n := shares(); n != 1 {
		t.Fatalf("4 turns of one conversation counted %d times", n)
	}
	s.g.mu.Lock()
	s.g.shares[0].at = time.Now().Add(-shareWindow - time.Second)
	s.g.mu.Unlock()
	for k := 4; k <= 5; k++ {
		s.serve(s.route("alice", turn("session", k)), turn("session", k))
	}
	if n := shares(); n != 1 {
		t.Fatalf("after the window the conversation counted %d times, want once", n)
	}
	for i := 0; i < 3; i++ {
		s.serve(s.route("alice", "hi"), "hi")
	}
	if n := shares(); n != 4 {
		t.Fatalf("3 short requests: %d share entries, want 4", n)
	}
}

// Hashing once: the hashes of prompt+tail continue those of the prompt.
func TestBlockHashesFrom(t *testing.T) {
	c := newPrefixCache()
	p := strings.Repeat("abcdefg", 300)
	full := p + strings.Repeat("tail", 200)
	want, got := c.blockHashes(full), c.blockHashesFrom(c.blockHashes(p), full)
	if len(want) != len(got) || commonPrefix(want, got) != len(want) {
		t.Fatalf("continued hashes differ: %d of %d blocks equal", commonPrefix(want, got), len(want))
	}
}

// The conversation is rendered and hashed before routing, on the request's path: about
// 100k tokens of agent conversation.
func BenchmarkConvPrint(b *testing.B) {
	c := newPrefixCache()
	msgs := []any{map[string]any{"role": "system", "content": strings.Repeat("You are a coding agent. ", 1000)}}
	for i := 0; i < 100; i++ {
		msgs = append(msgs, map[string]any{"role": "user", "content": strings.Repeat("tool output line\n", 220)},
			map[string]any{"role": "assistant", "content": "ok"})
	}
	body := map[string]any{"messages": msgs}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if p := c.print(forNode(body)); len(p.hashes) < 1000 {
			b.Fatal(len(p.prompt))
		}
	}
}

// Looking up which node holds the conversation, with 32 remembered conversations on each of
// 64 nodes, 8 of them the requester's.
func BenchmarkHeld(b *testing.B) {
	c := newPrefixCache()
	now := time.Now()
	for n := 0; n < 64; n++ {
		for k := 0; k < 32; k++ {
			who := "other"
			if k < 8 {
				who = "alice"
			}
			c.served(string(rune('A'+n)), who, turn(string(rune('a'+k)), 10), "", now, true)
		}
	}
	ph := c.blockHashes(turn("a", 11))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		c.held("alice", ph, now)
	}
}
