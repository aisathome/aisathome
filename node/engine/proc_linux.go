// SPDX-License-Identifier: Apache-2.0
package engine

import (
	"syscall"
	"time"
)

type sysProc struct{}

// startSys starts the engine in its own process group, so a stop reaches anything it started,
// and with SIGKILL as its parent-death signal, so it does not outlive the agent. (The signal
// follows the thread that started it; Go ends a thread only when a goroutine locked to it
// returns, which this package never does.)
func (p *proc) startSys() error {
	p.cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGKILL}
	return p.cmd.Start()
}

// stop asks the engine's process group to end (SIGTERM), kills it after wait, and waits for
// the process to be gone (at most wait again: a process stuck in the GPU driver may not go).
func (p *proc) stop(wait time.Duration) {
	pgid := p.pid()
	syscall.Kill(-pgid, syscall.SIGTERM)
	if p.wait(wait) {
		return
	}
	syscall.Kill(-pgid, syscall.SIGKILL)
	p.wait(wait)
}

func (p *proc) releaseSys() {}
