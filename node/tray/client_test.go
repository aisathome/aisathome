// SPDX-License-Identifier: Apache-2.0
package tray

import (
	"context"
	"errors"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/aisathome/aisathome/node"
	"github.com/aisathome/aisathome/node/engine"
	"github.com/aisathome/aisathome/node/localui"
)

// engineAgent is an agent that runs its engine (Windows): Stop and Start engine work.
type engineAgent struct {
	mu      sync.Mutex
	stopped bool
}

func (a *engineAgent) Status() node.Status {
	a.mu.Lock()
	defer a.mu.Unlock()
	st := node.Status{State: node.StateConnected, Connected: true, Engine: node.EngineHealthy, NodeID: "n1",
		EngineProcess: &engine.Status{State: engine.StateRunning}}
	if a.stopped {
		st.State, st.Connected, st.Engine = node.StateDisconnected, false, node.EngineStopped
		st.EngineProcess.State = engine.StateStopped
	}
	return st
}
func (a *engineAgent) Pause()               {}
func (a *engineAgent) Resume()              {}
func (a *engineAgent) RestartEngine() error { return nil }
func (a *engineAgent) StopEngine() error    { a.mu.Lock(); a.stopped = true; a.mu.Unlock(); return nil }
func (a *engineAgent) StartEngine() error   { a.mu.Lock(); a.stopped = false; a.mu.Unlock(); return nil }

type env struct {
	agent *engineAgent
	ui    *localui.Server
	pw    string
	links int
	c     *Client
}

// newEnv runs the real local page with a password file, and a client whose sign-in links come
// straight from the page's server (on Windows they come over the agent's pipe).
func newEnv(t *testing.T) *env {
	pw := filepath.Join(t.TempDir(), "ui-password")
	if err := os.WriteFile(pw, []byte("tray-test-password-123\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	e := &env{agent: &engineAgent{}, pw: pw}
	e.ui = localui.New(e.agent, localui.Options{PasswordFile: pw, NvidiaSMI: "/nonexistent/nvidia-smi"})
	srv := httptest.NewServer(e.ui.Handler())
	t.Cleanup(srv.Close)
	e.c = NewClient(localui.DefaultPort, func() (string, error) { e.links++; return e.ui.NewLoginLink() })
	e.c.Base = srv.URL
	return e
}

// Stop and Start engine from the tray: the first control signs in with one link, later ones
// reuse the session; the tray's state follows.
func TestClientControls(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	if v := FromStatus(e.c.Status(ctx)); v.State != Connected || !v.CanStop {
		t.Fatalf("before: %+v", v)
	}
	msg, err := e.c.Control(ctx, "stop-engine")
	if err != nil || !strings.Contains(msg, "GPU is free") {
		t.Fatalf("stop-engine: %q %v", msg, err)
	}
	if v := FromStatus(e.c.Status(ctx)); v.State != Stopped || !v.CanStart || v.CanStop {
		t.Fatalf("after stop: %+v", v)
	}
	if _, err := e.c.Control(ctx, "start-engine"); err != nil {
		t.Fatalf("start-engine: %v", err)
	}
	if e.links != 1 {
		t.Fatalf("%d sign-in links for two controls, want 1 (the session is kept)", e.links)
	}
	if v := FromStatus(e.c.Status(ctx)); v.State != Connected {
		t.Fatalf("after start: %+v", v)
	}
}

// A new password ends the tray's session: the next control signs in again, once.
func TestClientSessionEnds(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	if _, err := e.c.Control(ctx, "stop-engine"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(e.pw, []byte("another-password-4567\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := e.c.Control(ctx, "start-engine"); err != nil {
		t.Fatalf("after a new password: %v", err)
	}
	if e.links != 2 {
		t.Fatalf("%d links, want 2", e.links)
	}
}

// Without a link (the pipe refuses this account, or the agent is down) the controls fail with
// the reason, and the page opens without signing in.
func TestClientNoLink(t *testing.T) {
	e := newEnv(t)
	e.c.Link = func() (string, error) { return "", errors.New("this account may not sign in") }
	if _, err := e.c.Control(context.Background(), "stop-engine"); err == nil || !strings.Contains(err.Error(), "may not sign in") {
		t.Fatalf("control without a link: %v", err)
	}
	u, err := e.c.PageURL()
	if err == nil || u != e.c.Base+"/" {
		t.Fatalf("page without a link: %q %v", u, err)
	}
	e.c.Link = func() (string, error) {
		return "http://" + localui.Addr(localui.DefaultPort) + "/login-link?t=" + strings.Repeat("0", 64), nil
	}
	if err := e.c.SignIn(context.Background()); err == nil {
		t.Fatal("an unknown link gave a session")
	}
}

// The agent not running is the tray's Down state.
func TestClientAgentDown(t *testing.T) {
	c := NewClient(1, nil)
	if v := FromStatus(c.Status(context.Background())); v.State != Down {
		t.Fatalf("no agent: %+v", v)
	}
}

// The tray's self-test passes against the real page, and fails when links work twice.
func TestCheckSignIn(t *testing.T) {
	e := newEnv(t)
	if err := e.c.CheckSignIn(context.Background()); err != nil {
		t.Fatal(err)
	}
	e.c.Link = func() (string, error) { return "", errors.New("refused") }
	if err := e.c.CheckSignIn(context.Background()); err == nil {
		t.Fatal("passed without a link")
	}
}

func TestLogFile(t *testing.T) {
	env := map[string]string{"LOCALAPPDATA": `C:\Users\jo\AppData\Local\`}
	if got := LogFile(func(k string) string { return env[k] }); got != `C:\Users\jo\AppData\Local\AIsAtHome\tray.log` {
		t.Fatal(got)
	}
	if got := LogFile(func(string) string { return "" }); got != "" {
		t.Fatal(got)
	}
}
