// SPDX-License-Identifier: Apache-2.0
package engine

import (
	"io"
	"os"
	"os/exec"
	"sync"
	"time"
)

// proc is one running engine process.
type proc struct {
	cmd  *exec.Cmd
	done chan error // the process's exit, once
	sys  sysProc    // the platform's part (proc_*.go)
	once sync.Once
}

// start starts c with its output to out.
func start(c Command, out io.Writer) (*proc, error) {
	cmd := exec.Command(c.Path, c.Args...)
	// Later entries win, so c.Env overrides the agent's own environment.
	cmd.Env = append(os.Environ(), c.Env...)
	cmd.Dir = c.Dir
	cmd.Stdout, cmd.Stderr = out, out
	// The output is copied through a pipe; do not wait for it long after the process ended
	// (a process the engine started could hold the pipe open).
	cmd.WaitDelay = 5 * time.Second
	p := &proc{cmd: cmd, done: make(chan error, 1)}
	if err := p.startSys(); err != nil {
		return nil, err
	}
	go func() { p.done <- cmd.Wait() }()
	return p, nil
}

func (p *proc) pid() int { return p.cmd.Process.Pid }

// wait waits up to d for the process to end; it reports whether it did.
func (p *proc) wait(d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case err := <-p.done:
		p.done <- err // keep it for a later wait
		return true
	case <-t.C:
		return false
	}
}

// release frees what the platform holds for the process, once it has ended or been stopped.
func (p *proc) release() { p.once.Do(p.releaseSys) }
