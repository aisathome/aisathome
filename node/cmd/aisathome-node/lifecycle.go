// SPDX-License-Identifier: Apache-2.0
package main

import (
	"context"
	"time"
)

// exitStopTimeout is the exit code when the agent did not stop within the stop wait.
const exitStopTimeout = 3

// stopWait bounds how long a stop request waits for the agent. Windows waits for a service
// at shutdown only a few seconds; the agent needs well under one to close its connection.
const stopWait = 10 * time.Second

// runUntil runs fn until it returns or stop is closed. On stop it cancels fn's context and
// waits up to wait for fn to return. It returns fn's exit code (exitStopTimeout when fn did
// not return in time) and whether a stop ended it.
func runUntil(fn func(context.Context) int, stop <-chan struct{}, wait time.Duration) (code int, stopped bool) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan int, 1)
	go func() { done <- fn(ctx) }()
	select {
	case code := <-done:
		return code, false
	case <-stop:
	}
	cancel()
	t := time.NewTimer(wait)
	defer t.Stop()
	select {
	case code := <-done:
		return code, true
	case <-t.C:
		return exitStopTimeout, true
	}
}

// serviceExit is what a Windows service reports when the agent has ended: a clean end is
// success; any other code, including exitStopTimeout after a stop the agent did not finish in
// time, is a service-specific error, so it shows in the service's state and event log.
func serviceExit(code int) (svcSpecific bool, exitCode uint32) {
	if code == 0 {
		return false, 0
	}
	return true, uint32(code)
}
