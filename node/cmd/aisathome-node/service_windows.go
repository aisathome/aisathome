// SPDX-License-Identifier: Apache-2.0
package main

import (
	"context"
	"sync"
	"time"

	"golang.org/x/sys/windows/svc"

	"github.com/aisathome/aisathome/node"
)

// runningAsService reports whether the Service Control Manager started this process.
func runningAsService() (bool, error) { return svc.IsWindowsService() }

// runService runs the agent as a Windows service until the Service Control Manager stops it
// (Stop, or Shutdown when Windows shuts down). It returns the process's exit code.
func runService(run func(context.Context) int) int {
	h := &handler{run: run, wait: stopWait}
	if err := svc.Run(node.ServiceName, h); err != nil {
		return 1
	}
	return h.code
}

type handler struct {
	run  func(context.Context) int
	wait time.Duration
	code int
}

// Execute implements svc.Handler. An agent that fails, or does not stop within the stop wait,
// reports its exit code as a service-specific error (serviceExit), so the failure is visible
// and the service's recovery actions (restart on failure) can apply.
func (h *handler) Execute(_ []string, req <-chan svc.ChangeRequest, status chan<- svc.Status) (bool, uint32) {
	const accepts = svc.AcceptStop | svc.AcceptShutdown
	status <- svc.Status{State: svc.StartPending}
	stop := make(chan struct{})
	var once sync.Once
	type result struct{ code int }
	res := make(chan result, 1)
	go func() {
		code, _ := runUntil(h.run, stop, h.wait)
		res <- result{code}
	}()
	status <- svc.Status{State: svc.Running, Accepts: accepts}
	for {
		select {
		case r := <-res:
			status <- svc.Status{State: svc.StopPending}
			h.code = r.code
			return serviceExit(r.code)
		case c := <-req:
			switch c.Cmd {
			case svc.Interrogate:
				status <- c.CurrentStatus
			case svc.Stop, svc.Shutdown:
				status <- svc.Status{State: svc.StopPending, WaitHint: uint32(h.wait / time.Millisecond)}
				once.Do(func() { close(stop) })
			}
		}
	}
}
