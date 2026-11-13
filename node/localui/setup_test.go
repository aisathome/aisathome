// SPDX-License-Identifier: Apache-2.0
package localui

import (
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aisathome/aisathome/node"
	"github.com/aisathome/aisathome/node/engine"
	"github.com/aisathome/aisathome/node/setupflow"
)

// fakeSetup records the setup actions the page asks for.
type fakeSetup struct {
	mu    sync.Mutex
	st    setupflow.State
	calls []string
}

func (f *fakeSetup) State() setupflow.State { f.mu.Lock(); defer f.mu.Unlock(); return f.st }
func (f *fakeSetup) do(c string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, c)
	return nil
}
func (f *fakeSetup) CheckCard() error { return f.do("check") }
func (f *fakeSetup) Download() error  { return f.do("download") }
func (f *fakeSetup) SpeedTest() error { return setupflow.ErrBusy }
func (f *fakeSetup) Cancel()          { f.do("cancel") }
func (f *fakeSetup) Start(token, gw string) error {
	if _, err := setupflow.CheckToken(token); err != nil {
		return err
	}
	return f.do("start " + token + " " + gw)
}
func (f *fakeSetup) ChangeToken(token, gw string) error { return f.do("token " + token) }
func (f *fakeSetup) Enter() error                       { return f.do("enter") }
func (f *fakeSetup) Leave() error                       { return f.do("leave") }

// engineAgent runs its engine (Windows): stop and start work.
type engineAgent struct {
	fakeAgent
	stops, starts int
}

func (a *engineAgent) Status() node.Status {
	st := a.fakeAgent.Status()
	st.EngineProcess = &engine.Status{State: engine.StateRunning}
	st.CanRestart = true
	return st
}
func (a *engineAgent) StopEngine() error  { a.stops++; return nil }
func (a *engineAgent) StartEngine() error { a.starts++; return nil }
func (a *engineAgent) RestartEngine() error {
	return nil
}

func newSetupEnv(t *testing.T, st setupflow.State) (*env, *fakeSetup) {
	e := newEnv(t)
	fs := &fakeSetup{st: st}
	e.ui = New(e.agent, Options{PasswordFile: e.pw, NvidiaSMI: fakeSMI(t, "4090"), Setup: fs})
	e.srv.Config.Handler = e.ui.Handler()
	return e, fs
}

// The installer's shortcut opens "/": the setup page while the node is not set up or the
// volunteer runs the setup again; the status page otherwise. Without a setup (Linux) there is
// no setup page.
func TestSetupRedirect(t *testing.T) {
	noFollow := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	get := func(e *env, path string) *http.Response {
		resp, err := noFollow.Get(e.srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { resp.Body.Close() })
		return resp
	}
	for _, c := range []struct {
		st       setupflow.State
		redirect bool
	}{
		{setupflow.State{Mode: "setup"}, true},
		{setupflow.State{Mode: "run", Configured: false}, true},
		{setupflow.State{Mode: "setup", Configured: true}, true},
		{setupflow.State{Mode: "run", Configured: true}, false},
	} {
		e, _ := newSetupEnv(t, c.st)
		resp := get(e, "/")
		if got := resp.StatusCode == http.StatusSeeOther && resp.Header.Get("Location") == "/setup"; got != c.redirect {
			t.Errorf("%+v: / answered %d %q", c.st, resp.StatusCode, resp.Header.Get("Location"))
		}
		resp = get(e, "/setup")
		if b := body(resp); resp.StatusCode != 200 || !strings.Contains(b, `src="/setup.js"`) {
			t.Errorf("/setup: %d", resp.StatusCode)
		}
		if csp := resp.Header.Get("Content-Security-Policy"); !strings.Contains(csp, "default-src 'self'") {
			t.Errorf("/setup without the CSP: %q", csp)
		}
	}
	e := newEnv(t)
	if resp := get(e, "/"); resp.StatusCode != 200 {
		t.Errorf("no setup: / answered %d", resp.StatusCode)
	}
	for _, p := range []string{"/setup", "/setup.html", "/setup.js", "/api/setup"} {
		if resp := get(e, p); resp.StatusCode != http.StatusNotFound {
			t.Errorf("no setup: %s answered %d", p, resp.StatusCode)
		}
	}
}

// Every setup action needs the local password, and cross-site requests are refused; the token
// goes to the setup and never comes back in an answer.
func TestSetupActions(t *testing.T) {
	e, fs := newSetupEnv(t, setupflow.State{Mode: "setup", Gateway: setupflow.DefaultGatewayURL})
	for _, p := range []string{"/api/setup/check", "/api/setup/download", "/api/setup/start", "/api/setup/enter", "/api/stop-engine"} {
		if resp := e.req(t, "POST", p, `{}`, nil); resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s without a session: %d", p, resp.StatusCode)
		}
	}
	c := e.login(t)
	if resp := e.req(t, "POST", "/api/setup/check", `{}`, map[string]string{"Origin": "https://evil.example", "Sec-Fetch-Site": "cross-site"}, c); resp.StatusCode != http.StatusForbidden {
		t.Errorf("cross-site setup action: %d", resp.StatusCode)
	}
	if resp := e.req(t, "POST", "/api/setup/check", `{}`, nil, c); resp.StatusCode != 200 {
		t.Errorf("check: %d %s", resp.StatusCode, body(resp))
	}
	if resp := e.req(t, "POST", "/api/setup/speed", `{}`, nil, c); resp.StatusCode != http.StatusConflict {
		t.Errorf("busy: %d", resp.StatusCode)
	}
	resp := e.req(t, "POST", "/api/setup/start", `{"token":"nope"}`, nil, c)
	if b := body(resp); resp.StatusCode != http.StatusBadRequest || !strings.Contains(b, "not a node token") {
		t.Errorf("bad token: %d %s", resp.StatusCode, b)
	}
	resp = e.req(t, "POST", "/api/setup/start", `{"token":"nt-n-0000000a.s3cret","gateway":"wss://gw.example/v1/node/connect"}`, nil, c)
	if b := body(resp); resp.StatusCode != 200 || strings.Contains(b, "s3cret") {
		t.Errorf("start: %d %s", resp.StatusCode, b)
	}
	if got := fs.calls[len(fs.calls)-1]; got != "start nt-n-0000000a.s3cret wss://gw.example/v1/node/connect" {
		t.Errorf("start call %q", got)
	}
	resp = e.req(t, "GET", "/api/status", "", nil, c)
	var st StatusResponse
	if err := json.NewDecoder(resp.Body).Decode(&st); err != nil || st.Setup == nil || st.Setup.Mode != "setup" ||
		st.Controls.PasswordHint == "" || st.Controls.StatusCommand == "" {
		t.Errorf("status: %v %+v", err, st)
	}
}

// Stop engine and Start engine work on an agent that runs its engine; on one that does not
// (Linux) they answer with the command to use instead. The restart message does not claim the
// agent restarts when only the engine does.
func TestEngineButtons(t *testing.T) {
	e := newEnv(t)
	c := e.login(t)
	if resp := e.req(t, "POST", "/api/stop-engine", `{}`, nil, c); resp.StatusCode != http.StatusConflict {
		t.Errorf("Linux stop: %d", resp.StatusCode)
	}
	a := &engineAgent{}
	e.ui.SetAgent(a)
	for _, p := range []string{"/api/stop-engine", "/api/start-engine"} {
		if resp := e.req(t, "POST", p, `{}`, nil, c); resp.StatusCode != 200 {
			t.Errorf("%s: %d %s", p, resp.StatusCode, body(resp))
		}
	}
	if a.stops != 1 || a.starts != 1 {
		t.Errorf("stops %d, starts %d", a.stops, a.starts)
	}
	resp := e.req(t, "POST", "/api/restart-engine", `{}`, nil, c)
	if b := body(resp); resp.StatusCode != http.StatusAccepted || strings.Contains(b, "agent restarts") {
		t.Errorf("restart: %d %s", resp.StatusCode, b)
	}
	var st StatusResponse
	json.NewDecoder(e.req(t, "GET", "/api/status", "", nil, c).Body).Decode(&st)
	if !st.Controls.EngineControl || st.EngineProcess == nil {
		t.Errorf("controls %+v", st.Controls)
	}
}

// `aisathome-node status` on a Windows node: the engine process (a stopped engine is
// "stopped", not unhealthy), the gateway's rejection, and the setup when it is not done.
func TestPrintStatusEngineProcess(t *testing.T) {
	now := time.Date(2026, 11, 11, 20, 0, 0, 0, time.UTC)
	st := StatusResponse{Status: node.Status{NodeID: "n-0000000a", State: node.StateDisconnected, Engine: node.EngineStopped,
		Rejected: "HTTP 401: unknown node token", EngineProcess: &engine.Status{State: engine.StateStopped}},
		Setup: &setupflow.State{Mode: "run", Configured: true}}
	var b strings.Builder
	PrintStatus(&b, st, 7381, now)
	for _, want := range []string{"rejected   HTTP 401: unknown node token", "engine     stopped (Start engine on the local page)"} {
		if !strings.Contains(b.String(), want) {
			t.Errorf("lacks %q:\n%s", want, b.String())
		}
	}
	if strings.Contains(b.String(), "setup") {
		t.Errorf("a set-up node shows the setup:\n%s", b.String())
	}
	st.Engine = node.EngineHealthy
	st.EngineProcess = &engine.Status{State: engine.StateRunning, PID: 4242, Since: now.Add(-90 * time.Second), Restarts: 2}
	st.Setup.Mode = "setup"
	b.Reset()
	PrintStatus(&b, st, 7381, now)
	for _, want := range []string{"healthy, process 4242 running for 1m, 2 restart(s)", "setup      not set up yet: open http://127.0.0.1:7381/setup"} {
		if !strings.Contains(b.String(), want) {
			t.Errorf("lacks %q:\n%s", want, b.String())
		}
	}
}
