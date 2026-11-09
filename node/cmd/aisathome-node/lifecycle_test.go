// SPDX-License-Identifier: Apache-2.0
package main

import (
	"context"
	"testing"
	"time"
)

// An agent that ends on its own reports its code; a stop is not involved.
func TestRunUntilAgentEnds(t *testing.T) {
	code, stopped := runUntil(func(context.Context) int { return 7 }, make(chan struct{}), time.Second)
	if code != 7 || stopped {
		t.Fatalf("code %d, stopped %v", code, stopped)
	}
}

// A stop cancels the agent's context and waits for it to return.
func TestRunUntilStop(t *testing.T) {
	stop := make(chan struct{})
	started := make(chan struct{})
	go func() { <-started; close(stop) }()
	code, stopped := runUntil(func(ctx context.Context) int {
		close(started)
		<-ctx.Done()
		return 0
	}, stop, 5*time.Second)
	if code != 0 || !stopped {
		t.Fatalf("code %d, stopped %v", code, stopped)
	}
}

// An agent that ignores the stop is given up on after the wait.
func TestRunUntilStopTimeout(t *testing.T) {
	stop := make(chan struct{})
	close(stop)
	release := make(chan struct{})
	defer close(release)
	start := time.Now()
	code, stopped := runUntil(func(context.Context) int { <-release; return 0 }, stop, 50*time.Millisecond)
	if code != exitStopTimeout || !stopped {
		t.Fatalf("code %d, stopped %v", code, stopped)
	}
	if d := time.Since(start); d < 50*time.Millisecond || d > 5*time.Second {
		t.Fatalf("gave up after %v, want about 50ms", d)
	}
}

// A service reports success only for a clean end: a failure or a stop timeout is an error.
func TestServiceExit(t *testing.T) {
	for _, c := range []struct {
		code     int
		specific bool
		exit     uint32
	}{{0, false, 0}, {1, true, 1}, {exitStopTimeout, true, exitStopTimeout}} {
		if specific, exit := serviceExit(c.code); specific != c.specific || exit != c.exit {
			t.Errorf("code %d: (%v, %d), want (%v, %d)", c.code, specific, exit, c.specific, c.exit)
		}
	}
}
