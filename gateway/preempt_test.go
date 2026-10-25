// SPDX-License-Identifier: Apache-2.0
package gateway

import (
	"context"
	"testing"
	"time"
)

// runScience marks one of n's slots as held by a science request that started at start, as
// pick and run do.
func (s *stickyRig) runScience(n *nodeConn, start time.Time) *sciRun {
	n.mu.Lock()
	n.inflight++
	n.mu.Unlock()
	sr := s.g.addScience(n)
	sr.start = start
	return sr
}

// endScience frees the slot of a science request that was told to stop, as relay.free does.
func (s *stickyRig) endScience(sr *sciRun) {
	sr.n.mu.Lock()
	sr.n.inflight--
	sr.n.mu.Unlock()
	s.g.dropScience(sr)
	s.g.signal()
}

func stopped(sr *sciRun) bool {
	select {
	case <-sr.stop:
		return true
	default:
		return false
	}
}

// waitStopped waits until one of srs is told to stop, and fails if another one is too.
func (s *stickyRig) waitStopped(srs ...*sciRun) *sciRun {
	s.t.Helper()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		var got []*sciRun
		for _, sr := range srs {
			if stopped(sr) {
				got = append(got, sr)
			}
		}
		if len(got) > 1 {
			s.t.Fatalf("%d science requests preempted for one waiting request", len(got))
		}
		if len(got) == 1 {
			return got[0]
		}
	}
	s.t.Fatal("no science request was preempted")
	return nil
}

// A waiting chat preempts after the threshold, not before; of the science requests on nodes it
// could use it stops the one that started last (least work lost), one at a time; once that
// slot is free the chat gets it. If someone else takes the slot, it preempts the next one.
func TestPreemptLatestScienceOneAtATime(t *testing.T) {
	s := newStickyRig(t)
	s.g.cfg.PreemptAfterSec = 1
	a := s.node("a", "op", true, 0)
	a.models[0].Slots = 3
	now := time.Now()
	old := s.runScience(a, now.Add(-time.Hour))
	newest := s.runScience(a, now.Add(-time.Minute))
	mid := s.runScience(a, now.Add(-10*time.Minute))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	t0 := time.Now()
	got := s.queue(ctx, classRoute(ClassChat, "", 0))
	if sr := s.waitStopped(old, newest, mid); sr != newest {
		t.Fatalf("preempted the request from %v ago, want the newest", now.Sub(sr.start))
	}
	if took := time.Since(t0); took < 900*time.Millisecond {
		t.Fatalf("preempted after %v, before the 1 s threshold", took)
	}
	// Someone else (an agent that looked first) takes the freed slot: the chat preempts the next.
	newest.n.mu.Lock()
	newest.n.inflight-- // the slot is free, and taken at once by another request
	newest.n.inflight++
	newest.n.mu.Unlock()
	s.g.dropScience(newest)
	s.g.signal()
	if sr := s.waitStopped(old, mid); sr != mid {
		t.Fatal("the second preemption took the oldest request")
	}
	s.endScience(mid)
	if p := s.next(got); p.n != a {
		t.Fatalf("the chat got %v", p.n)
	}
	if stopped(old) {
		t.Fatal("the oldest science request was preempted although the chat was served")
	}
}

// A waiting request preempts only on nodes it could use, at its own placement threshold: not
// on another operator's node it may not use, and not on a small node when it waits for a large
// one that fits its whole conversation. Science does not preempt science.
func TestPreemptOnlyUsableNodes(t *testing.T) {
	s := newStickyRig(t)
	s.g.cfg.PreemptAfterSec = 1
	other := s.node("other", "someone", true, 196608)
	small := s.node("small", "op", true, 81920)
	big := s.node("big", "op", true, 196608)
	onOther := s.runScience(other, time.Now())
	onSmall := s.runScience(small, time.Now().Add(-time.Second))
	onBig := s.runScience(big, time.Now().Add(-time.Hour))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	agent := classRoute(ClassAgent, "", 0)
	agent.operator = "op"
	agent.need, agent.needMin = 90000, 74000
	got := s.queue(ctx, agent)
	if sr := s.waitStopped(onOther, onSmall, onBig); sr != onBig {
		t.Fatalf("preempted on %s, want big: the only node the request may use that fits it", sr.n.id)
	}
	s.endScience(onBig)
	if p := s.next(got); p.n != big {
		t.Fatalf("the agent got %v", p.n)
	}
	// Science waiting on science never preempts.
	sci := classRoute(ClassScience, "tauceti", 0)
	done := make(chan struct{})
	go func() {
		defer close(done)
		if n, _, why := s.g.pickWait(ctx, sci, time.Now().Add(1500*time.Millisecond)); n != nil || why != timedOut {
			t.Errorf("science: %v %s", n, why)
		}
	}()
	<-done
	if stopped(onOther) || stopped(onSmall) {
		t.Fatal("a science request preempted science")
	}
}

// A person waits at most a second before a science request gives up its node (2026-10-25: the
// default was 8 s, longer than a person should wait for the first token).
func TestPreemptAfterDefaultsToOneSecond(t *testing.T) {
	g := newTestGateway(t, Config{})
	if g.cfg.PreemptAfterSec != 1 {
		t.Fatalf("default preempt_after_seconds = %d, want 1", g.cfg.PreemptAfterSec)
	}
	g = newTestGateway(t, Config{PreemptAfterSec: -1})
	if g.cfg.PreemptAfterSec != -1 {
		t.Fatalf("-1 (off) became %d", g.cfg.PreemptAfterSec)
	}
}
