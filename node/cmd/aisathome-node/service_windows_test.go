// SPDX-License-Identifier: Apache-2.0
package main

import (
	"context"
	"testing"
	"time"

	"golang.org/x/sys/windows/svc"
)

// execute runs the handler as the Service Control Manager would and returns its result and
// the states it reported.
func execute(h *handler, req chan svc.ChangeRequest) (chan svc.Status, chan [2]any) {
	status := make(chan svc.Status, 16)
	done := make(chan [2]any, 1)
	go func() {
		ec, code := h.Execute(nil, req, status)
		done <- [2]any{ec, code}
	}()
	return status, done
}

func wantState(t *testing.T, status chan svc.Status, want svc.State) svc.Status {
	t.Helper()
	select {
	case s := <-status:
		if s.State != want {
			t.Fatalf("state %d, want %d", s.State, want)
		}
		return s
	case <-time.After(5 * time.Second):
		t.Fatalf("no state reported, want %d", want)
	}
	return svc.Status{}
}

// Stop: the agent's context ends, the service stops cleanly.
func TestServiceStop(t *testing.T) {
	ctxDone := make(chan struct{})
	h := &handler{wait: 5 * time.Second, run: func(ctx context.Context) int { <-ctx.Done(); close(ctxDone); return 0 }}
	req := make(chan svc.ChangeRequest)
	status, done := execute(h, req)
	wantState(t, status, svc.StartPending)
	if s := wantState(t, status, svc.Running); s.Accepts&svc.AcceptStop == 0 || s.Accepts&svc.AcceptShutdown == 0 {
		t.Fatalf("accepts %d: want stop and shutdown", s.Accepts)
	}
	req <- svc.ChangeRequest{Cmd: svc.Interrogate, CurrentStatus: svc.Status{State: svc.Running}}
	wantState(t, status, svc.Running)
	req <- svc.ChangeRequest{Cmd: svc.Shutdown}
	wantState(t, status, svc.StopPending)
	select {
	case r := <-done:
		if r[0] != false || r[1] != uint32(0) {
			t.Fatalf("result %v, want a clean stop", r)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the service did not stop")
	}
	select {
	case <-ctxDone:
	default:
		t.Fatal("the agent's context was not cancelled")
	}
}

// An agent that fails reports a service-specific error, so recovery restarts the service.
func TestServiceAgentFails(t *testing.T) {
	h := &handler{wait: time.Second, run: func(context.Context) int { return 1 }}
	status, done := execute(h, make(chan svc.ChangeRequest))
	wantState(t, status, svc.StartPending)
	select {
	case r := <-done:
		if r[0] != true || r[1] != uint32(1) || h.code != 1 {
			t.Fatalf("result %v (code %d), want a service-specific error 1", r, h.code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the service did not end")
	}
}

// An agent that does not stop within the wait: the service still ends, with an error.
func TestServiceStopTimeout(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	h := &handler{wait: 50 * time.Millisecond, run: func(context.Context) int { <-release; return 0 }}
	req := make(chan svc.ChangeRequest)
	status, done := execute(h, req)
	wantState(t, status, svc.StartPending)
	wantState(t, status, svc.Running)
	req <- svc.ChangeRequest{Cmd: svc.Stop}
	wantState(t, status, svc.StopPending)
	select {
	case r := <-done:
		if r[0] != true || r[1] != uint32(exitStopTimeout) || h.code != exitStopTimeout {
			t.Fatalf("result %v (code %d), want a service-specific error %d", r, h.code, exitStopTimeout)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the service did not end after the stop wait")
	}
}
