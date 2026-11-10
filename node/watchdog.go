// SPDX-License-Identifier: Apache-2.0
package node

import (
	"context"
	"fmt"
	"time"
)

// DefaultWatchdog is how long the gateway link may stay dead before the agent exits. It is
// well above a normal reconnect (backoff at most 15 s, a handshake at most 30 s, a missing
// pong found within 45 s), so the watchdog only ends an agent that no longer recovers on its
// own.
const DefaultWatchdog = 5 * time.Minute

// LinkDeadError is Run's error when the watchdog found the gateway link dead for too long.
type LinkDeadError struct{ For time.Duration }

func (e *LinkDeadError) Error() string {
	return fmt.Sprintf("watchdog: no word from the gateway for %s; exiting so that the service manager restarts the agent",
		e.For.Round(time.Second))
}

// linkAlive records that the gateway link works: a welcome, a message or a pong arrived.
func (a *Agent) linkAlive() {
	a.mu.Lock()
	a.linkAt = a.now()
	a.mu.Unlock()
}

// setAway records whether the node stays away from the gateway on purpose (engine unhealthy,
// or waiting after a refusal). The watchdog does not count that time.
func (a *Agent) setAway(away bool) {
	a.mu.Lock()
	if a.away != away {
		a.away, a.linkAt = away, a.now()
	}
	a.mu.Unlock()
}

// watch runs serve under the watchdog (Config.Watchdog > 0): if the gateway link stays dead
// for longer than that while the node is not paused or away on purpose, watch returns a
// *LinkDeadError without waiting for serve, which may be stuck (2026-11-10: 16 hours in one
// read). The caller exits non-zero; systemd, the Windows service or a start script restarts
// the agent.
func (a *Agent) watch(ctx context.Context, serve func(context.Context) error) error {
	a.mu.Lock()
	a.linkAt = a.now()
	a.mu.Unlock()
	if a.cfg.Watchdog <= 0 {
		return serve(ctx)
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- serve(ctx) }()
	t := time.NewTicker(max(min(a.cfg.Watchdog/10, 10*time.Second), time.Millisecond))
	defer t.Stop()
	for {
		select {
		case err := <-done:
			return err
		case <-t.C:
		}
		a.mu.Lock()
		dead := a.now().Sub(a.linkAt)
		exempt := a.ps.Paused || a.away
		a.mu.Unlock()
		if exempt || dead < a.cfg.Watchdog {
			continue
		}
		err := &LinkDeadError{For: dead}
		a.log.Error("watchdog: the gateway link is dead; exiting for a restart", "for", dead.Round(time.Second),
			"limit", a.cfg.Watchdog)
		a.event("watchdog", fmt.Sprintf("no word from the gateway for %s; exiting for a restart", dead.Round(time.Second)))
		cancel() // let a session that still responds close its connection
		select {
		case <-done:
		case <-time.After(2 * time.Second): // stuck: leave it, the process exits
		}
		return err
	}
}
