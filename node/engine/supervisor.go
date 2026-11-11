// Package engine runs llama-server as a child of the node agent and keeps it running: start,
// restart with back-off when it exits, stop on request (to free the GPU) and start again. The
// Windows node uses it; on Linux systemd runs the engine as its own unit, and this package is
// only tested there. On Windows the child runs in a Job object that kills it when the agent's
// process ends, however it ends; on Linux it gets SIGKILL when the agent dies (proc_unix.go).
//
// SPDX-License-Identifier: Apache-2.0
package engine

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"time"
)

// Command is the engine's program, arguments and the environment it adds to the agent's.
type Command struct {
	Path string
	Args []string
	Env  []string // "KEY=value" entries added to (and overriding) the agent's environment
	Dir  string   // working directory; "" = the agent's
}

// Options tunes a Supervisor; the zero value is the real setting.
type Options struct {
	Output io.Writer    // the engine's stdout and stderr; nil discards them
	Log    *slog.Logger // nil discards
	// MinBackoff is the wait before the first restart after an exit; each further exit in a row
	// doubles it up to MaxBackoff. A run of at least StableAfter starts over at MinBackoff.
	// Defaults 2 s, 2 min, 1 min.
	MinBackoff  time.Duration
	MaxBackoff  time.Duration
	StableAfter time.Duration
	// StopWait is how long a stop waits after asking the engine to end before it kills it
	// (Linux: SIGTERM, then SIGKILL; Windows has no such request and kills at once). Default
	// 20 s, as the Linux unit's TimeoutStopSec.
	StopWait time.Duration
	// OnExit is called when the engine ended without being asked to (a crash or a failed
	// start), with the error and the wait before the next start.
	OnExit func(err error, restartIn time.Duration)
	// PreStart, when set, runs before every start; an error is a failed start (retried with the
	// back-off), e.g. the engine's port is taken by another program.
	PreStart func() error
}

func (o *Options) defaults() {
	if o.Output == nil {
		o.Output = io.Discard
	}
	if o.Log == nil {
		o.Log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if o.MinBackoff <= 0 {
		o.MinBackoff = 2 * time.Second
	}
	if o.MaxBackoff < o.MinBackoff {
		o.MaxBackoff = max(2*time.Minute, o.MinBackoff)
	}
	if o.StableAfter <= 0 {
		o.StableAfter = time.Minute
	}
	if o.StopWait <= 0 {
		o.StopWait = 20 * time.Second
	}
}

// The engine process's states (Status.State).
const (
	StateStarting   = "starting"   // Run has not started it yet
	StateRunning    = "running"    // the process runs (it may still be loading the model)
	StateRestarting = "restarting" // it ended on its own; waiting to start it again
	StateStopped    = "stopped"    // stopped on request; stays stopped until Start
	StateEnded      = "ended"      // Run has returned
)

// Status is a snapshot of the engine process. It never holds the engine's output.
type Status struct {
	State     string     `json:"state"`
	PID       int        `json:"pid,omitempty"`
	Since     time.Time  `json:"since"`               // when State last changed
	Restarts  int        `json:"restarts"`            // starts after it ended on its own
	LastExit  string     `json:"last_exit,omitempty"` // how it last ended on its own
	NextStart *time.Time `json:"next_start,omitempty"`
}

type request int

const (
	reqStop request = iota
	reqStart
	reqRestart
)

// Supervisor runs one engine process at a time.
type Supervisor struct {
	cmd Command
	o   Options
	req chan request

	mu sync.Mutex
	st Status
}

// New returns a supervisor for cmd; Run starts the engine.
func New(cmd Command, o Options) *Supervisor {
	o.defaults()
	return &Supervisor{cmd: cmd, o: o, req: make(chan request, 8),
		st: Status{State: StateStarting, Since: time.Now()}}
}

// Status returns the engine process's state.
func (s *Supervisor) Status() Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.st
	if st.NextStart != nil {
		t := *st.NextStart
		st.NextStart = &t
	}
	return st
}

// Stop stops the engine (frees the GPU) and keeps it stopped until Start or Restart.
func (s *Supervisor) Stop() { s.send(reqStop) }

// Start starts a stopped engine, or one waiting to restart, at once. It does nothing to a
// running engine.
func (s *Supervisor) Start() { s.send(reqStart) }

// Restart stops the engine and starts it again at once.
func (s *Supervisor) Restart() { s.send(reqRestart) }

func (s *Supervisor) send(r request) {
	select {
	case s.req <- r:
	default: // a full queue: Run is behind on requests that are already pending
	}
}

func (s *Supervisor) set(state string, pid int, next *time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.st.State != state {
		s.st.Since = time.Now()
	}
	s.st.State, s.st.PID, s.st.NextStart = state, pid, next
}

// nextBackoff is the wait after an engine that ran for ran ended on its own, when the last
// wait was cur (0: none yet).
func (o *Options) nextBackoff(cur, ran time.Duration) time.Duration {
	if cur == 0 || ran >= o.StableAfter {
		return o.MinBackoff
	}
	return min(2*cur, o.MaxBackoff)
}

// Run starts the engine and keeps it running until ctx ends; then it stops the engine and
// returns ctx's error once the process is gone.
func (s *Supervisor) Run(ctx context.Context) error {
	defer s.set(StateEnded, 0, nil)
	var backoff time.Duration
	stopped := false
	for {
		if stopped {
			s.set(StateStopped, 0, nil)
			s.o.Log.Info("engine stopped on request")
			for stopped {
				select {
				case <-ctx.Done():
					return ctx.Err()
				case r := <-s.req:
					stopped = r == reqStop
				}
			}
			backoff = 0
		}
		started := time.Now()
		var p *proc
		var err error
		if s.o.PreStart != nil {
			err = s.o.PreStart()
		}
		if err == nil {
			p, err = start(s.cmd, s.o.Output)
		}
		var exitErr error
		if err != nil {
			exitErr = err
		} else {
			s.set(StateRunning, p.pid(), nil)
			s.o.Log.Info("engine started", "pid", p.pid())
			exitErr, stopped = s.watch(ctx, p)
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if exitErr == nil { // stopped or restarted on request
				backoff = 0
				continue
			}
		}
		// It ended on its own, or did not start: wait, then start it again.
		backoff = s.o.nextBackoff(backoff, time.Since(started))
		next := time.Now().Add(backoff)
		s.mu.Lock()
		s.st.LastExit = exitErr.Error()
		s.mu.Unlock()
		s.set(StateRestarting, 0, &next)
		s.o.Log.Warn("engine ended; starting it again", "err", exitErr, "in", backoff)
		if s.o.OnExit != nil {
			s.o.OnExit(exitErr, backoff)
		}
		t := time.NewTimer(backoff)
		select {
		case <-ctx.Done():
			t.Stop()
			return ctx.Err()
		case r := <-s.req:
			t.Stop()
			stopped = r == reqStop
			if !stopped {
				backoff = 0 // started by hand: a fresh start
			}
		case <-t.C:
		}
		if !stopped {
			s.mu.Lock()
			s.st.Restarts++
			s.mu.Unlock()
		}
	}
}

// watch waits for the running process p to end or to be stopped. It returns the exit error
// when p ended on its own (nil when it was stopped on request or ctx ended) and whether it
// must stay stopped.
func (s *Supervisor) watch(ctx context.Context, p *proc) (exitErr error, stopped bool) {
	for {
		select {
		case <-ctx.Done():
			s.stopProc(p)
			return nil, false
		case err := <-p.done:
			p.release()
			if err == nil {
				err = errors.New("exited with status 0")
			}
			return err, false
		case r := <-s.req:
			if r == reqStart {
				continue // already running
			}
			s.stopProc(p)
			return nil, r == reqStop
		}
	}
}

// stopProc ends p and waits until it is gone.
func (s *Supervisor) stopProc(p *proc) {
	s.o.Log.Info("stopping the engine", "pid", p.pid())
	p.stop(s.o.StopWait)
	p.release()
}
