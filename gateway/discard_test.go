// SPDX-License-Identifier: Apache-2.0
package gateway

import (
	"strings"
	"testing"
	"time"
)

func TestDiscardTracker(t *testing.T) {
	d := newDiscardTracker()
	now := time.Now()
	sys := strings.Repeat("system prompt ", 50) // longer than a hash block
	p1 := sys + "<|im_start|>user\nq1<|im_end|>\n<|im_start|>assistant\n"
	ans := "A1<|im_end|>\n"
	d.answered("alice", p1, []string{p1 + ans}, "n-1", "op", "r1", now)

	if got := d.next("bob", p1, now); got != nil {
		t.Fatalf("another requester's prompt closed alice's answer: %v", got)
	}
	if got := d.next("alice", sys+"<|im_start|>user\nanother conversation", now); got != nil {
		t.Fatalf("another conversation discarded it: %v", got)
	}
	got := d.next("alice", p1, now) // the same prompt again: thrown away
	if len(got) != 1 || got[0].reqID != "r1" || got[0].node != "n-1" || got[0].op != "op" {
		t.Fatalf("regenerate: %v", got)
	}
	if got := d.next("alice", p1, now); got != nil {
		t.Fatalf("counted twice: %v", got)
	}

	// Kept: the next prompt continues one of its forms; later prompts change nothing.
	d.answered("alice", p1, []string{p1 + ans, "", p1 + "\n</think>\n\n" + ans}, "n-1", "op", "r2", now)
	if got := d.next("alice", p1+"\n</think>\n\n"+ans+"<|im_start|>user\nq2", now); got != nil {
		t.Fatalf("a continued answer was discarded: %v", got)
	}
	if got := d.next("alice", p1, now); got != nil {
		t.Fatalf("a kept answer was discarded later: %v", got)
	}

	// An edited earlier turn (the prompt starts with neither) leaves it open.
	d.answered("alice", p1, []string{p1 + ans}, "n-1", "op", "r3", now)
	if got := d.next("alice", sys+"<|im_start|>user\nq1 edited", now); got != nil {
		t.Fatalf("an edit counted: %v", got)
	}
	// Forgotten after discardTTL.
	if got := d.next("alice", p1, now.Add(discardTTL+time.Second)); got != nil {
		t.Fatalf("an old answer counted: %v", got)
	}

	// Secret placeholders change their nonce per request: the same prompt with a new nonce is the same.
	ps := sys + "key AISXab12" + "01XXXX here<|im_start|>assistant\n"
	d.answered("carol", ps, []string{ps + ans}, "n-2", "", "r4", now)
	if got := d.next("carol", sys+"key AISXcd34"+"02XXXX here<|im_start|>assistant\n", now); len(got) != 1 {
		t.Fatalf("a new nonce hid the discard: %v", got)
	}

	// Bounds: per requester and requesters.
	for i := 0; i < discardPerWho+5; i++ {
		d.answered("dave", p1, []string{p1 + ans}, "n-1", "", "x", now)
	}
	if n := len(d.by["dave"].list); n != discardPerWho {
		t.Fatalf("%d open answers kept", n)
	}
	var nilT *discardTracker
	nilT.answered("x", "p", nil, "n", "", "r", now)
	if nilT.next("x", "p", now) != nil {
		t.Fatal("nil tracker")
	}
}
