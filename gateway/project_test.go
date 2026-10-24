// SPDX-License-Identifier: Apache-2.0
package gateway

import (
	"context"
	"strings"
	"testing"
	"time"
)

func classRoute(class, project string, share float64) *route {
	return &route{want: "deep", exclude: map[*nodeConn]bool{}, class: class, project: project, share: share}
}

// release frees a slot that pick reserved for rt, as relay.free does.
func (s *stickyRig) release(n *nodeConn, rt *route) {
	n.mu.Lock()
	n.inflight--
	if rt.project != "" {
		n.projects[rt.project]--
	}
	n.mu.Unlock()
	s.g.signal()
}

func (s *stickyRig) waiters() int {
	s.g.mu.Lock()
	defer s.g.mu.Unlock()
	return len(s.g.waiting)
}

func (s *stickyRig) waitWaiters(n int) {
	s.t.Helper()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		if s.waiters() == n {
			return
		}
	}
	s.t.Fatalf("%d requests waiting, want %d", s.waiters(), n)
}

type placed struct {
	rt *route
	n  *nodeConn
}

// queue starts a request of each route, one after the other, each waiting for a node.
func (s *stickyRig) queue(ctx context.Context, rts ...*route) chan placed {
	got := make(chan placed, len(rts))
	for i, rt := range rts {
		go func() {
			n, _, why := s.g.pickWait(ctx, rt, time.Now().Add(10*time.Second))
			if n == nil {
				s.t.Errorf("%s: no node (%s)", rt.class, why)
			}
			got <- placed{rt, n}
		}()
		s.waitWaiters(i + 1)
	}
	return got
}

func (s *stickyRig) next(got chan placed) placed {
	s.t.Helper()
	select {
	case p := <-got:
		return p
	case <-time.After(5 * time.Second):
		s.t.Fatal("no request got the node")
	}
	return placed{}
}

// When the one node frees up, the waiting requests get it in the order chat, agent, science,
// although they arrived in the opposite order.
func TestPriorityOrder(t *testing.T) {
	s := newStickyRig(t)
	a := s.node("a", "op", true, 0)
	a.inflight = 1 // busy with someone's request
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	got := s.queue(ctx, classRoute(ClassScience, "tauceti", 0), classRoute(ClassAgent, "", 0), classRoute(ClassChat, "", 0))
	a.mu.Lock()
	a.inflight--
	a.mu.Unlock()
	s.g.signal()
	for _, want := range []string{ClassChat, ClassAgent, ClassScience} {
		p := s.next(got)
		if p.rt.class != want {
			t.Fatalf("%s got the node, want %s", p.rt.class, want)
		}
		select {
		case p2 := <-got:
			t.Fatalf("%s got a node while %s holds the only one", p2.rt.class, want)
		case <-time.After(50 * time.Millisecond):
		}
		s.release(p.n, p.rt)
	}
}

// A science request never takes a node that a waiting user request could use, even when it
// looks before the user request wakes up; a node the user request cannot use stays open to it.
func TestScienceNeverDelaysWaitingUser(t *testing.T) {
	s := newStickyRig(t)
	a := s.node("a", "op", true, 0)
	a.inflight = 1
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	user := classRoute(ClassAgent, "", 0)
	user.operator = "op" // only this operator's nodes
	got := s.queue(ctx, user)
	// The slot frees without waking anyone yet: science arrives first.
	a.mu.Lock()
	a.inflight--
	a.mu.Unlock()
	sci := classRoute(ClassScience, "tauceti", 0)
	if n, _ := s.g.pick(sci); n != nil {
		t.Fatalf("science took %s while a user request waits for it", n.id)
	}
	// Another operator's node, which the waiting user request may not use: science may.
	s.node("b", "other", true, 0)
	if n, _ := s.g.pick(sci); n == nil || n.id != "b" {
		t.Fatalf("science on a node no waiting user can use: %v", n)
	}
	s.g.signal()
	if p := s.next(got); p.n != a {
		t.Fatalf("the user request got %v, want a", p.n)
	}
	// Agents also leave nodes to waiting chats.
	chat := classRoute(ClassChat, "", 0)
	got = s.queue(ctx, chat)
	s.release(a, user)
	if p := s.next(got); p.n != a || p.rt != chat {
		t.Fatalf("chat did not get the node: %v", p.n)
	}
	// With nobody waiting, science takes a free node at once.
	s.release(a, chat)
	if n, _ := s.g.pick(classRoute(ClassScience, "tauceti", 0)); n != a {
		t.Fatalf("science with no one waiting: %v", n)
	}
}

// A waiting chat that needs more context than a free node has, and that a busy larger node
// fits, waits for the larger node (pick's threshold is its full need): lower classes must not
// leave the smaller node idle for it. The larger node still goes to the chat when it frees.
func TestYieldOnlyNodesTheWaiterWouldTake(t *testing.T) {
	s := newStickyRig(t)
	small := s.node("small", "op", true, 81920)
	big := s.node("big", "op", true, 196608)
	big.inflight = 1
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	chat := classRoute(ClassChat, "", 0)
	chat.need, chat.needMin = 90000, 74000
	got := s.queue(ctx, chat)
	for _, lower := range []*route{classRoute(ClassAgent, "", 0), classRoute(ClassScience, "tauceti", 0)} {
		n, _ := s.g.pick(lower)
		if n != small {
			t.Fatalf("%s got %v while the waiting chat would not take the 80k node, want small", lower.class, n)
		}
		s.release(n, lower)
	}
	// With the big node busy, a lower class gets nothing that the chat would take.
	select {
	case p := <-got:
		t.Fatalf("the chat got %s, want it to wait for the 192k node", p.n.id)
	case <-time.After(50 * time.Millisecond):
	}
	s.release(big, classRoute(ClassChat, "", 0))
	if p := s.next(got); p.n != big {
		t.Fatalf("the chat got %v, want big", p.n)
	}
}

// A project occupies at most its share of the nodes that may serve it (rounded down, at
// least one); people's requests still get the rest; freeing a node lets the project back in.
func TestProjectShareCap(t *testing.T) {
	s := newStickyRig(t)
	for _, id := range []string{"a", "b", "c"} {
		s.node(id, "op", true, 0)
	}
	sci := func() *route { return classRoute(ClassScience, "tauceti", 0.7) }
	n1, _ := s.g.pick(sci())
	n2, _ := s.g.pick(sci())
	if n1 == nil || n2 == nil || n1 == n2 {
		t.Fatalf("two science requests on two nodes: %v %v", n1, n2)
	}
	if n, _ := s.g.pick(sci()); n != nil {
		t.Fatalf("a third node for a project capped at 0.7 of 3: %s", n.id)
	}
	// Another project has its own share.
	if n, _ := s.g.pick(classRoute(ClassScience, "other", 0.7)); n == nil {
		t.Fatal("another project was held to tauceti's share")
	}
	s.release(n1, sci())
	if n, _ := s.g.pick(classRoute(ClassAgent, "", 0)); n == nil || n != n1 {
		t.Fatalf("a user request did not get the free node: %v", n)
	}
	s.release(n1, classRoute(ClassAgent, "", 0))
	if n, _ := s.g.pick(sci()); n != n1 {
		t.Fatalf("the project did not get back under its share: %v", n)
	}

	// One node and a share below 1: the project may still use it when people do not.
	s1 := newStickyRig(t)
	s1.node("solo", "op", true, 0)
	if n, _ := s1.g.pick(classRoute(ClassScience, "tauceti", 0.5)); n == nil {
		t.Fatal("a capped project never runs on a one-node network")
	}
}

func TestShareCap(t *testing.T) {
	for _, c := range []struct {
		share      float64
		pool, want int
	}{{0, 5, 0}, {1, 5, 0}, {0.7, 3, 2}, {0.7, 1, 1}, {0.5, 4, 2}, {0.7, 10, 7}, {0.3, 0, 1}} {
		if got := shareCap(c.share, c.pool); got != c.want {
			t.Errorf("shareCap(%v, %d) = %d, want %d", c.share, c.pool, got, c.want)
		}
	}
}

func TestProjectConfigChecked(t *testing.T) {
	h := strings.Repeat("ab", 32)
	ok := Project{ID: "tauceti", KeySHA256: []string{h}, MaxNodeShare: 0.7, DailyTokens: 1000}
	if _, err := newProjectTable([]Project{ok}); err != nil {
		t.Fatal(err)
	}
	for name, p := range map[string]Project{
		"bad id":    {ID: "Tau Ceti", KeySHA256: []string{h}},
		"no key":    {ID: "x"},
		"short key": {ID: "x", KeySHA256: []string{"abc"}},
		"share":     {ID: "x", KeySHA256: []string{h}, MaxNodeShare: 1.5},
		"negative":  {ID: "x", KeySHA256: []string{h}, DailyTokens: -1},
		"trust":     {ID: "x", KeySHA256: []string{h}, MinTrust: "some"},
	} {
		if _, err := newProjectTable([]Project{p}); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, err := newProjectTable([]Project{ok, ok}); err == nil {
		t.Error("a project defined twice was accepted")
	}
	other := Project{ID: "other", KeySHA256: []string{strings.ToUpper(h)}}
	if _, err := newProjectTable([]Project{ok, other}); err == nil {
		t.Error("one key for two projects was accepted")
	}
}
