// SPDX-License-Identifier: Apache-2.0
package engine

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// The test binary stands in for the engine (and, for the kill test, for the agent): with
// ENGINE_TEST_MODE set it runs as one of these instead of the tests.
//
//	serve <pidfile>    write its pid, then run until stopped
//	stubborn <pidfile> the same, but ignore SIGTERM (a hung engine)
//	crash <logfile>    append the time to logfile and exit with status 3
//	agent <pidfile>    run a Supervisor whose engine is "serve <pidfile>", until killed
func TestMain(m *testing.M) {
	mode := os.Getenv("ENGINE_TEST_MODE")
	if mode == "" {
		os.Exit(m.Run())
	}
	arg := os.Getenv("ENGINE_TEST_ARG")
	switch mode {
	case "serve", "stubborn":
		if mode == "stubborn" {
			signal.Ignore(syscall.SIGTERM)
		}
		writePID(arg)
		time.Sleep(time.Hour)
	case "crash":
		f, err := os.OpenFile(arg, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
		if err == nil {
			fmt.Fprintln(f, time.Now().UnixNano())
			f.Close()
		}
		os.Exit(3)
	case "agent":
		s := New(helper("serve", arg), Options{})
		s.Run(context.Background())
	}
	os.Exit(2)
}

func writePID(path string) {
	tmp := path + ".tmp"
	os.WriteFile(tmp, []byte(strconv.Itoa(os.Getpid())), 0o600)
	os.Rename(tmp, path) // whole or not at all
}

// helper is the test binary as an engine in mode with arg.
func helper(mode, arg string) Command {
	exe, err := os.Executable()
	if err != nil {
		panic(err)
	}
	return Command{Path: exe, Args: []string{"-test.run=^$"}, Env: []string{"ENGINE_TEST_MODE=" + mode, "ENGINE_TEST_ARG=" + arg}}
}

// readPID waits for a helper's pid file and removes it, so the next start writes a new one.
func readPID(t *testing.T, path string) int {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if b, err := os.ReadFile(path); err == nil {
			os.Remove(path)
			pid, err := strconv.Atoi(string(b))
			if err != nil {
				t.Fatal(err)
			}
			return pid
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("no pid file %s", path)
	return 0
}

func waitState(t *testing.T, s *Supervisor, state string) Status {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if st := s.Status(); st.State == state {
			return st
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("state %q, want %q", s.Status().State, state)
	return Status{}
}

// run starts s.Run in the background; the returned func stops it and returns Run's error.
func run(t *testing.T, s *Supervisor) func() error {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()
	var once sync.Once
	var err error
	stop := func() error {
		once.Do(func() {
			cancel()
			select {
			case err = <-done:
			case <-time.After(30 * time.Second):
				t.Error("Run did not return")
			}
		})
		return err
	}
	t.Cleanup(func() { stop() })
	return stop
}

// Stop frees the engine and keeps it stopped; Start runs it again; Restart replaces it at
// once; the end of Run stops it.
func TestStopStartRestart(t *testing.T) {
	pidf := filepath.Join(t.TempDir(), "pid")
	s := New(helper("serve", pidf), Options{MinBackoff: 10 * time.Millisecond})
	stop := run(t, s)
	pid1 := readPID(t, pidf)
	if st := waitState(t, s, StateRunning); st.PID != pid1 {
		t.Fatalf("status pid %d, engine %d", st.PID, pid1)
	}

	s.Stop()
	waitState(t, s, StateStopped)
	if !processGone(pid1, 10*time.Second) {
		t.Fatal("a stopped engine still runs")
	}
	time.Sleep(100 * time.Millisecond) // ten back-offs: it must stay stopped
	if st := s.Status(); st.State != StateStopped || st.Restarts != 0 {
		t.Fatalf("after a stop: %+v", st)
	}

	s.Start()
	pid2 := readPID(t, pidf)
	waitState(t, s, StateRunning)
	s.Start() // already running: nothing happens
	s.Restart()
	pid3 := readPID(t, pidf)
	if pid3 == pid2 || !processGone(pid2, 10*time.Second) {
		t.Fatalf("restart: old %d, new %d", pid2, pid3)
	}
	if st := waitState(t, s, StateRunning); st.PID != pid3 || st.Restarts != 0 || st.LastExit != "" {
		t.Fatalf("after a restart on request: %+v", st)
	}

	if err := stop(); err != context.Canceled {
		t.Fatalf("Run: %v", err)
	}
	if !processGone(pid3, 10*time.Second) {
		t.Fatal("the engine outlived Run")
	}
	if st := s.Status(); st.State != StateEnded {
		t.Fatalf("after Run: %+v", st)
	}
}

// An engine that keeps crashing is started again with a doubling wait, up to the maximum.
func TestRestartBackoff(t *testing.T) {
	logf := filepath.Join(t.TempDir(), "starts")
	var exits []time.Duration
	exited := make(chan struct{}, 100)
	s := New(helper("crash", logf), Options{MinBackoff: 40 * time.Millisecond, MaxBackoff: 160 * time.Millisecond,
		OnExit: func(err error, in time.Duration) {
			if !strings.Contains(err.Error(), "exit status 3") {
				t.Errorf("exit error %v", err)
			}
			exits = append(exits, in)
			exited <- struct{}{}
		}})
	stop := run(t, s)
	for range 5 {
		select {
		case <-exited:
		case <-time.After(20 * time.Second):
			t.Fatal("the engine was not started again")
		}
	}
	stop()
	want := []time.Duration{40, 80, 160, 160, 160}
	for i, w := range want {
		if exits[i] != w*time.Millisecond {
			t.Fatalf("back-offs %v, want %v ms", exits, want)
		}
	}
	// The starts really were that far apart (the waits are lower bounds).
	b, _ := os.ReadFile(logf)
	var starts []int64
	for _, l := range strings.Fields(string(b)) {
		n, _ := strconv.ParseInt(l, 10, 64)
		starts = append(starts, n)
	}
	for i := 1; i < len(starts) && i < len(want); i++ {
		if gap := time.Duration(starts[i] - starts[i-1]); gap < want[i-1]*time.Millisecond {
			t.Errorf("start %d came %v after the one before, want at least %v ms", i, gap, want[i-1])
		}
	}
	if st := s.Status(); st.Restarts < 4 || !strings.Contains(st.LastExit, "exit status 3") {
		t.Errorf("status %+v", st)
	}
}

// A run of StableAfter or longer starts the back-off over; a fresh supervisor starts at the
// minimum.
func TestNextBackoff(t *testing.T) {
	o := Options{MinBackoff: time.Second, MaxBackoff: 8 * time.Second, StableAfter: time.Minute}
	for _, c := range []struct{ cur, ran, want time.Duration }{
		{0, 0, time.Second},
		{time.Second, time.Second, 2 * time.Second},
		{4 * time.Second, time.Second, 8 * time.Second},
		{8 * time.Second, time.Second, 8 * time.Second},
		{8 * time.Second, time.Minute, time.Second},
	} {
		if got := o.nextBackoff(c.cur, c.ran); got != c.want {
			t.Errorf("nextBackoff(%v, %v) = %v, want %v", c.cur, c.ran, got, c.want)
		}
	}
	var d Options
	d.defaults()
	if d.MinBackoff != 2*time.Second || d.MaxBackoff != 2*time.Minute || d.StableAfter != time.Minute || d.StopWait != 20*time.Second {
		t.Errorf("defaults %+v", d)
	}
}

// A program that does not start is retried like a crash, and Stop during the wait holds it.
func TestStartFailure(t *testing.T) {
	exited := make(chan struct{}, 100)
	s := New(Command{Path: filepath.Join(t.TempDir(), "no-such-engine")}, Options{MinBackoff: 10 * time.Millisecond,
		OnExit: func(error, time.Duration) { exited <- struct{}{} }})
	run(t, s)
	<-exited
	<-exited
	if st := s.Status(); st.LastExit == "" || st.State == StateRunning {
		t.Fatalf("status %+v", st)
	}
	s.Stop()
	waitState(t, s, StateStopped)
}

// A failing PreStart (the engine's port is taken) is a failed start: the engine is not started,
// and the reason shows as its last exit.
func TestPreStartFailure(t *testing.T) {
	exited := make(chan error, 100)
	pid := filepath.Join(t.TempDir(), "pid")
	s := New(helper("serve", pid), Options{MinBackoff: 10 * time.Millisecond,
		PreStart: func() error { return fmt.Errorf("port 8000 is in use") },
		OnExit:   func(err error, _ time.Duration) { exited <- err }})
	run(t, s)
	if err := <-exited; err == nil || err.Error() != "port 8000 is in use" {
		t.Fatalf("exit %v", err)
	}
	<-exited
	if _, err := os.Stat(pid); err == nil {
		t.Fatal("the engine started although PreStart failed")
	}
	if st := s.Status(); st.LastExit != "port 8000 is in use" || st.State == StateRunning {
		t.Fatalf("status %+v", st)
	}
}

// An engine that ignores the stop request is killed after StopWait.
func TestStopKillsStubbornEngine(t *testing.T) {
	pidf := filepath.Join(t.TempDir(), "pid")
	s := New(helper("stubborn", pidf), Options{StopWait: 200 * time.Millisecond})
	run(t, s)
	pid := readPID(t, pidf)
	waitState(t, s, StateRunning)
	start := time.Now()
	s.Stop()
	waitState(t, s, StateStopped)
	if !processGone(pid, 10*time.Second) {
		t.Fatal("a stubborn engine survived the stop")
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("the stop took %v", d)
	}
}

// The engine dies with the agent, even when the agent is killed without a chance to stop it
// (Windows: TerminateProcess, ending the agent's handle to the engine's job; Linux: SIGKILL,
// and the engine's parent-death signal).
func TestEngineDiesWithAgent(t *testing.T) {
	pidf := filepath.Join(t.TempDir(), "pid")
	c := helper("agent", pidf)
	agent := exec.Command(c.Path, c.Args...)
	agent.Env = append(os.Environ(), c.Env...)
	if err := agent.Start(); err != nil {
		t.Fatal(err)
	}
	enginePID := readPID(t, pidf)
	if err := agent.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	agent.Wait()
	if !processGone(enginePID, 10*time.Second) {
		killPID(enginePID)
		t.Fatal("the engine outlived its agent")
	}
}
