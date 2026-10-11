// SPDX-License-Identifier: Apache-2.0
package localui

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aisathome/aisathome/node"
	"github.com/aisathome/aisathome/protocol"
)

const testPassword = "correct-horse-battery-staple"

type fakeAgent struct {
	mu        sync.Mutex
	paused    bool
	connected bool
	restarts  int
	canRestar bool
}

func (f *fakeAgent) Status() node.Status {
	f.mu.Lock()
	defer f.mu.Unlock()
	st := node.Status{NodeID: "my-4090", Gateway: "api.aisathome.org", Connected: f.connected, Paused: f.paused,
		Engine: node.EngineHealthy, Agent: node.AgentVersion, SlotsTotal: 2, CanRestart: f.canRestar,
		Models: []protocol.ModelInfo{{Name: "qwen3.8-27b", Slots: 2, Ctx: 32768}},
		Today:  node.Counters{Requests: 3, PromptTokens: 1200, CompletionTokens: 345}}
	st.State = node.StateConnected
	if !f.connected {
		st.State = node.StateDisconnected
	}
	if f.paused {
		st.State = node.StatePaused
	}
	return st
}
func (f *fakeAgent) Pause()  { f.mu.Lock(); f.paused = true; f.mu.Unlock() }
func (f *fakeAgent) Resume() { f.mu.Lock(); f.paused = false; f.mu.Unlock() }
func (f *fakeAgent) RestartEngine() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.canRestar {
		return node.ErrRestartUnavailable
	}
	f.restarts++
	return nil
}
func (f *fakeAgent) isPaused() bool { f.mu.Lock(); defer f.mu.Unlock(); return f.paused }

// fakeSMI is a stand-in nvidia-smi printing one RTX 4090.
func fakeSMI(t *testing.T) string {
	p := filepath.Join(t.TempDir(), "nvidia-smi")
	os.WriteFile(p, []byte("#!/bin/sh\necho '0, NVIDIA GeForce RTX 4090, 23545, 24564, 39, 9, 56.75, 450.00'\n"), 0o755)
	return p
}

type env struct {
	srv   *httptest.Server
	agent *fakeAgent
	ui    *Server
	pw    string // password file
}

func newEnv(t *testing.T) *env {
	t.Helper()
	pw := filepath.Join(t.TempDir(), "ui-password")
	if err := os.WriteFile(pw, []byte(testPassword+"\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	a := &fakeAgent{connected: true}
	ui := New(a, Options{PasswordFile: pw, NvidiaSMI: fakeSMI(t)})
	srv := httptest.NewServer(ui.Handler()) // listens on 127.0.0.1
	t.Cleanup(srv.Close)
	return &env{srv: srv, agent: a, ui: ui, pw: pw}
}

// req sends a request as a same-origin page would, unless hdr overrides headers ("" deletes).
func (e *env) req(t *testing.T, method, path, body string, hdr map[string]string, cookies ...*http.Cookie) *http.Response {
	t.Helper()
	r, _ := http.NewRequest(method, e.srv.URL+path, strings.NewReader(body))
	if method == http.MethodPost {
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Origin", e.srv.URL)
		r.Header.Set("Sec-Fetch-Site", "same-origin")
	}
	for k, v := range hdr {
		if k == "Host" {
			r.Host = v
		} else if v == "" {
			r.Header.Del(k)
		} else {
			r.Header.Set(k, v)
		}
	}
	for _, c := range cookies {
		r.AddCookie(c)
	}
	resp, err := http.DefaultTransport.RoundTrip(r)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func (e *env) login(t *testing.T) *http.Cookie {
	t.Helper()
	resp := e.req(t, "POST", "/api/login", `{"password":"`+testPassword+`"}`, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("login: %d", resp.StatusCode)
	}
	for _, c := range resp.Cookies() {
		if c.Name == cookieName {
			return c
		}
	}
	t.Fatal("no session cookie")
	return nil
}

func body(resp *http.Response) string { b, _ := io.ReadAll(resp.Body); return string(b) }

// The UI listens on the loopback interface only: another address of this machine gets nothing.
func TestListensOnLoopbackOnly(t *testing.T) {
	if Addr(7381) != "127.0.0.1:7381" {
		t.Fatalf("Addr = %s", Addr(7381))
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0") // find a free port
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	ui := New(&fakeAgent{}, Options{Port: port})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go ui.ListenAndServe(ctx)
	var ok bool
	for i := 0; i < 100 && !ok; i++ {
		c, err := net.Dial("tcp", Addr(port))
		if ok = err == nil; ok {
			c.Close()
		} else {
			time.Sleep(10 * time.Millisecond)
		}
	}
	if !ok {
		t.Fatal("UI not reachable on 127.0.0.1")
	}
	addrs, _ := net.InterfaceAddrs()
	tried := 0
	for _, a := range addrs {
		ip, _, _ := net.ParseCIDR(a.String())
		if ip == nil || ip.IsLoopback() || ip.IsLinkLocalUnicast() {
			continue
		}
		tried++
		if c, err := net.DialTimeout("tcp", net.JoinHostPort(ip.String(), itoa(port)), time.Second); err == nil {
			c.Close()
			t.Fatalf("the UI answers on %s, not only on the loopback", ip)
		}
	}
	if tried == 0 {
		t.Log("no non-loopback address to check against")
	}
}

func itoa(i int) string { b, _ := json.Marshal(i); return string(b) }

// DNS rebinding: a page on evil.example that resolves to 127.0.0.1 sends Host: evil.example.
func TestRejectsForeignHost(t *testing.T) {
	e := newEnv(t)
	for _, path := range []string{"/", "/api/status", "/app.js"} {
		if resp := e.req(t, "GET", path, "", map[string]string{"Host": "evil.example:7381"}); resp.StatusCode != http.StatusMisdirectedRequest {
			t.Fatalf("GET %s with a foreign Host: %d", path, resp.StatusCode)
		}
	}
	resp := e.req(t, "POST", "/api/login", `{"password":"`+testPassword+`"}`,
		map[string]string{"Host": "evil.example:7381", "Origin": "http://evil.example:7381"})
	if resp.StatusCode != http.StatusMisdirectedRequest || len(resp.Cookies()) != 0 {
		t.Fatalf("login with a foreign Host: %d %v", resp.StatusCode, resp.Cookies())
	}
	// Any loopback name and port passes (an SSH tunnel may use another local port).
	for _, h := range []string{"127.0.0.1:7381", "localhost:7381", "localhost:8000", "[::1]:7381", "LOCALHOST:7381", "localhost"} {
		if resp := e.req(t, "GET", "/api/status", "", map[string]string{"Host": h}); resp.StatusCode != 200 {
			t.Fatalf("Host %s: %d", h, resp.StatusCode)
		}
	}
	for _, h := range []string{"127.0.0.2:7381", "localhost.evil.example:7381", "0.0.0.0:7381", "192.168.1.5:7381"} {
		if resp := e.req(t, "GET", "/api/status", "", map[string]string{"Host": h}); resp.StatusCode != http.StatusMisdirectedRequest {
			t.Fatalf("Host %s accepted: %d", h, resp.StatusCode)
		}
	}
}

// Cross-site requests cannot use the controls, even with a valid session cookie (which a
// browser would not send cross-site anyway: SameSite=Strict).
func TestRejectsCrossSiteRequests(t *testing.T) {
	e := newEnv(t)
	cookie := e.login(t)
	cases := map[string]map[string]string{
		"foreign Origin":            {"Origin": "http://evil.example"},
		"other local port (Origin)": {"Origin": "http://localhost:3000"},
		"Sec-Fetch-Site cross-site": {"Sec-Fetch-Site": "cross-site", "Origin": ""},
		"Sec-Fetch-Site same-site":  {"Sec-Fetch-Site": "same-site", "Origin": ""},
		"form post":                 {"Content-Type": "application/x-www-form-urlencoded"},
		"text/plain":                {"Content-Type": "text/plain"},
		"no Content-Type":           {"Content-Type": ""},
	}
	for name, hdr := range cases {
		for _, path := range []string{"/api/pause", "/api/login", "/api/restart-engine"} {
			resp := e.req(t, "POST", path, `{"password":"`+testPassword+`"}`, hdr, cookie)
			if resp.StatusCode != http.StatusForbidden {
				t.Fatalf("%s to %s: %d, want 403", name, path, resp.StatusCode)
			}
			if len(resp.Cookies()) != 0 {
				t.Fatalf("%s to %s set a cookie", name, path)
			}
		}
	}
	if e.agent.isPaused() || e.agent.restarts != 0 {
		t.Fatal("a cross-site request changed the node")
	}
	// A command-line client (no Origin, no Sec-Fetch-Site) with a session works.
	if resp := e.req(t, "POST", "/api/pause", "{}", map[string]string{"Origin": "", "Sec-Fetch-Site": ""}, cookie); resp.StatusCode != 200 || !e.agent.isPaused() {
		t.Fatalf("same-origin pause: %d", resp.StatusCode)
	}
}

// Controls need a session cookie issued for the password; forged and stale cookies fail.
func TestControlsNeedSession(t *testing.T) {
	e := newEnv(t)
	for _, path := range []string{"/api/pause", "/api/resume", "/api/restart-engine"} {
		if resp := e.req(t, "POST", path, "{}", nil); resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("%s without session: %d", path, resp.StatusCode)
		}
	}
	exp := time.Now().Add(time.Hour).Unix()
	forged := []*http.Cookie{
		{Name: cookieName, Value: "1"},
		{Name: cookieName, Value: itoa(int(exp)) + "." + strings.Repeat("0", 64)},
		{Name: cookieName, Value: itoa(int(exp)) + "." + sign("another-password-123", exp)},
		{Name: cookieName, Value: itoa(int(exp)) + "." + sign(testPassword, exp+1)}, // MAC of another expiry
	}
	for _, c := range forged {
		if resp := e.req(t, "POST", "/api/pause", "{}", nil, c); resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("forged cookie %q: %d", c.Value, resp.StatusCode)
		}
	}
	if e.agent.isPaused() {
		t.Fatal("paused without a session")
	}
	cookie := e.login(t)
	if resp := e.req(t, "POST", "/api/pause", "{}", nil, cookie); resp.StatusCode != 200 || !e.agent.isPaused() {
		t.Fatalf("pause with session: %d", resp.StatusCode)
	}
	if resp := e.req(t, "POST", "/api/resume", "{}", nil, cookie); resp.StatusCode != 200 || e.agent.isPaused() {
		t.Fatalf("resume with session: %d", resp.StatusCode)
	}
	var st StatusResponse
	json.NewDecoder(e.req(t, "GET", "/api/status", "", nil, cookie).Body).Decode(&st)
	if !st.Controls.Enabled || !st.Controls.SignedIn {
		t.Fatalf("status with session: %+v", st.Controls)
	}
	// A new password ends all sessions.
	os.WriteFile(e.pw, []byte("a-brand-new-password-456\n"), 0o640)
	if resp := e.req(t, "POST", "/api/pause", "{}", nil, cookie); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("old session after password change: %d", resp.StatusCode)
	}
	// An expired session fails.
	e.ui.auth.now = func() time.Time { return time.Now().Add(-sessionTTL - time.Hour) }
	old, _, err := e.ui.auth.login("a-brand-new-password-456")
	e.ui.auth.now = time.Now
	if err != nil {
		t.Fatal(err)
	}
	if resp := e.req(t, "POST", "/api/pause", "{}", nil, old); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expired session: %d", resp.StatusCode)
	}
}

// The session cookie is HttpOnly and SameSite=Strict; wrong passwords get no cookie and slow
// down the next attempt.
func TestLogin(t *testing.T) {
	e := newEnv(t)
	resp := e.req(t, "POST", "/api/login", `{"password":"`+testPassword+`"}`, nil)
	sc := resp.Header.Get("Set-Cookie")
	if resp.StatusCode != 200 || !strings.Contains(sc, "HttpOnly") || !strings.Contains(sc, "SameSite=Strict") ||
		!strings.Contains(sc, "Path=/") || !strings.Contains(sc, "Max-Age=") {
		t.Fatalf("login: %d, Set-Cookie %q", resp.StatusCode, sc)
	}
	resp = e.req(t, "POST", "/api/login", `{"password":"wrong-password-000"}`, nil)
	if resp.StatusCode != http.StatusUnauthorized || len(resp.Cookies()) != 0 {
		t.Fatalf("wrong password: %d %v", resp.StatusCode, resp.Cookies())
	}
	resp = e.req(t, "POST", "/api/login", `{"password":"`+testPassword+`"}`, nil)
	if resp.StatusCode != http.StatusTooManyRequests || len(resp.Cookies()) != 0 {
		t.Fatalf("attempt right after a failure: %d", resp.StatusCode)
	}
	time.Sleep(failDelay)
	if resp := e.req(t, "POST", "/api/login", `{"password":" `+testPassword+` "}`, nil); resp.StatusCode != 200 {
		t.Fatalf("after the delay (spaces trimmed): %d", resp.StatusCode)
	}
	if resp := e.req(t, "POST", "/api/logout", `{}`, nil); !strings.Contains(resp.Header.Get("Set-Cookie"), "Max-Age=0") {
		t.Fatalf("logout must clear the cookie: %q", resp.Header.Get("Set-Cookie"))
	}
}

// Without a readable password file the controls are off, and say why.
func TestNoPasswordFile(t *testing.T) {
	for name, pw := range map[string]string{"missing": "/nonexistent/ui-password", "unset": ""} {
		a := &fakeAgent{}
		srv := httptest.NewServer(New(a, Options{PasswordFile: pw}).Handler())
		defer srv.Close()
		e := &env{srv: srv, agent: a}
		var st StatusResponse
		json.NewDecoder(e.req(t, "GET", "/api/status", "", nil).Body).Decode(&st)
		if st.Controls.Enabled || st.Controls.Note == "" {
			t.Fatalf("%s: controls %+v", name, st.Controls)
		}
		if resp := e.req(t, "POST", "/api/login", `{"password":""}`, nil); resp.StatusCode != http.StatusServiceUnavailable {
			t.Fatalf("%s: login %d", name, resp.StatusCode)
		}
	}
	// A short password counts as none.
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "pw"), []byte("short\n"), 0o640)
	if _, err := newAuth(filepath.Join(dir, "pw")).password(); err == nil {
		t.Fatal("short password accepted")
	}
}

// The page loads nothing from elsewhere, sets data as text only, and is not framable.
func TestPageIsSelfContained(t *testing.T) {
	e := newEnv(t)
	resp := e.req(t, "GET", "/", "", nil)
	csp := resp.Header.Get("Content-Security-Policy")
	if resp.StatusCode != 200 || !strings.Contains(csp, "default-src 'self'") || !strings.Contains(csp, "frame-ancestors 'none'") ||
		resp.Header.Get("X-Frame-Options") != "DENY" {
		t.Fatalf("headers: %d %v", resp.StatusCode, resp.Header)
	}
	ext := regexp.MustCompile(`(?i)(https?:)?//[a-z0-9.-]+\.[a-z]{2,}`)
	fs.WalkDir(assets, "assets", func(p string, d fs.DirEntry, err error) error {
		if d.IsDir() || !strings.HasSuffix(p, ".html") && !strings.HasSuffix(p, ".css") && !strings.HasSuffix(p, ".js") {
			return nil
		}
		b, _ := assets.ReadFile(p)
		for _, m := range ext.FindAllString(string(b), -1) {
			if m != "https://aisathome.org" && m != "http://www.w3.org" { // the account link (a link, not a load)
				t.Errorf("%s references %s", p, m)
			}
		}
		if strings.HasSuffix(p, ".js") && regexp.MustCompile(`innerHTML|outerHTML|insertAdjacentHTML|document\.write|eval\(`).Match(b) {
			t.Errorf("%s writes HTML; set text only", p)
		}
		return nil
	})
	for _, f := range []string{"/app.css", "/app.js", "/favicon.svg", "/fonts/public-sans-latin-wght-normal.woff2",
		"/fonts/big-shoulders-display-latin-wght-normal.woff2", "/fonts/jetbrains-mono-latin-wght-normal.woff2"} {
		if resp := e.req(t, "GET", f, "", nil); resp.StatusCode != 200 {
			t.Errorf("%s: %d", f, resp.StatusCode)
		}
	}
	if b := body(e.req(t, "GET", "/", "", nil)); !strings.Contains(b, `href="https://aisathome.org/account"`) {
		t.Error("no link to the account page")
	}
}

// The status carries the node, GPU and counters; without nvidia-smi the GPU panel degrades.
func TestStatusAndGPU(t *testing.T) {
	e := newEnv(t)
	var st StatusResponse
	json.NewDecoder(e.req(t, "GET", "/api/status", "", nil).Body).Decode(&st)
	if st.NodeID != "my-4090" || st.State != "connected" || st.AccountURL != AccountURL || len(st.GPU.GPUs) != 1 ||
		st.GPU.GPUs[0].Name != "NVIDIA GeForce RTX 4090" || *st.GPU.GPUs[0].TempC != 39 || st.Controls.SignedIn ||
		st.Controls.RestartCommand != node.RestartCommand {
		t.Fatalf("status: %+v", st)
	}
	g := newGPUProbe(filepath.Join(t.TempDir(), "no-such-nvidia-smi")).get(context.Background())
	if len(g.GPUs) != 0 || g.Note == "" {
		t.Fatalf("missing nvidia-smi: %+v", g)
	}
	broken := filepath.Join(t.TempDir(), "nvidia-smi")
	os.WriteFile(broken, []byte("#!/bin/sh\necho 'Failed to initialize NVML' >&2; exit 9\n"), 0o755)
	if g := newGPUProbe(broken).get(context.Background()); len(g.GPUs) != 0 || !strings.Contains(g.Note, "failed") {
		t.Fatalf("failing nvidia-smi: %+v", g)
	}
	gs := parseGPUs("0, NVIDIA GeForce RTX 3090, 100, 24576, 45, [N/A], [N/A], 350.00\n1, RTX A4000, 5, 16376, 30, 0, 14.1, 140.00\ngarbage\n")
	if len(gs) != 2 || gs[0].UtilPct != nil || gs[0].PowerW != nil || *gs[1].PowerW != 14.1 || gs[1].Index != 1 {
		t.Fatalf("parse: %+v", gs)
	}
}

// Restart: when the agent may not, the answer carries the command to run by hand.
func TestRestartEngine(t *testing.T) {
	e := newEnv(t)
	cookie := e.login(t)
	resp := e.req(t, "POST", "/api/restart-engine", "{}", nil, cookie)
	if b := body(resp); resp.StatusCode != http.StatusConflict || !strings.Contains(b, node.RestartCommand) {
		t.Fatalf("unavailable restart: %d %s", resp.StatusCode, b)
	}
	e.agent.mu.Lock()
	e.agent.canRestar = true
	e.agent.mu.Unlock()
	if resp := e.req(t, "POST", "/api/restart-engine", "{}", nil, cookie); resp.StatusCode != http.StatusAccepted || e.agent.restarts != 1 {
		t.Fatalf("restart: %d", resp.StatusCode)
	}
}

// `aisathome-node status` prints the status and exits 0 only when connected.
func TestStatusCommand(t *testing.T) {
	e := newEnv(t)
	port := e.srv.Listener.Addr().(*net.TCPAddr).Port
	var out, errb bytes.Buffer
	if code := RunStatus([]string{"-port", itoa(port)}, &out, &errb); code != ExitConnected {
		t.Fatalf("connected: exit %d, %s", code, errb.String())
	}
	for _, want := range []string{"my-4090: CONNECTED", "api.aisathome.org", "qwen3.8-27b, 2 slot(s) x 32768 tokens",
		"NVIDIA GeForce RTX 4090", "39 °C", "3 requests, 1200 tokens in, 345 out", "http://127.0.0.1:"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out.String())
		}
	}
	e.agent.mu.Lock()
	e.agent.connected = false
	e.agent.mu.Unlock()
	out.Reset()
	if code := RunStatus([]string{"-port", itoa(port)}, &out, &errb); code != ExitNotConnected || !strings.Contains(out.String(), "not connected") {
		t.Fatalf("disconnected: exit %d\n%s", code, out.String())
	}
	out.Reset()
	if code := RunStatus([]string{"-port", itoa(port), "-json"}, &out, &errb); code != ExitNotConnected || !json.Valid(out.Bytes()) {
		t.Fatalf("json: exit %d %s", code, out.String())
	}
	e.srv.Close()
	if code := RunStatus([]string{"-port", itoa(port)}, &out, &errb); code != ExitUnreachable {
		t.Fatalf("no agent: exit %d", code)
	}
	if code := RunStatus([]string{"-bogus"}, &out, &errb); code != ExitUsage {
		t.Fatalf("bad flag: exit %d", code)
	}
}

func TestPortFromEnv(t *testing.T) {
	for v, want := range map[string]int{"": DefaultPort, "8123": 8123, "off": 0, "0": 0} {
		t.Setenv("AISATHOME_UI_PORT", v)
		if p, err := PortFromEnv(DefaultPort); err != nil || p != want {
			t.Fatalf("%q: %d %v", v, p, err)
		}
	}
	for _, v := range []string{"x", "70000", "-1"} {
		t.Setenv("AISATHOME_UI_PORT", v)
		if _, err := PortFromEnv(DefaultPort); err == nil {
			t.Fatalf("%q accepted", v)
		}
	}
}
