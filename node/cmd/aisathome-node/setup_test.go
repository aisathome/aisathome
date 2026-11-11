// SPDX-License-Identifier: Apache-2.0
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/cookiejar"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/aisathome/aisathome/node"
	"github.com/aisathome/aisathome/node/localui"
)

// page is a browser on the agent's local UI: signed in, same-origin requests.
type page struct {
	t    *testing.T
	base string
	c    *http.Client
}

func newPage(t *testing.T, port int, password string) *page {
	jar, _ := cookiejar.New(nil)
	p := &page{t: t, base: fmt.Sprintf("http://127.0.0.1:%d", port), c: &http.Client{Jar: jar, Timeout: 5 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
	deadline := time.Now().Add(10 * time.Second)
	for {
		resp, err := p.c.Get(p.base + "/api/status")
		if err == nil {
			resp.Body.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the local UI does not answer")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if code, b := p.post("/api/login", fmt.Sprintf(`{"password":%q}`, password)); code != 200 {
		t.Fatalf("login: %d %s", code, b)
	}
	return p
}

func (p *page) post(path, body string) (int, string) {
	req, _ := http.NewRequest("POST", p.base+path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", p.base)
	resp, err := p.c.Do(req)
	if err != nil {
		p.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func (p *page) status() localui.StatusResponse {
	resp, err := p.c.Get(p.base + "/api/status")
	if err != nil {
		p.t.Fatal(err)
	}
	defer resp.Body.Close()
	var st localui.StatusResponse
	if err := json.NewDecoder(resp.Body).Decode(&st); err != nil {
		p.t.Fatal(err)
	}
	return st
}

func (p *page) waitFor(what string, ok func(localui.StatusResponse) bool) localui.StatusResponse {
	p.t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		st := p.status()
		if ok(st) {
			return st
		}
		if time.Now().After(deadline) {
			p.t.Fatalf("waiting for %s; status %+v, setup %+v", what, st.Status, st.Setup)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// pidOf waits for the fake engine's pid file (or returns 0 after d).
func pidOf(fake string, d time.Duration) int {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		b, _ := os.ReadFile(filepath.Join(fake, "pid"))
		if pid, _ := strconv.Atoi(string(b)); pid != 0 {
			return pid
		}
		time.Sleep(10 * time.Millisecond)
	}
	return 0
}

// A fresh install (no node.json) runs: the local UI answers, "/" is the setup page, the engine
// stays stopped. Once node.json is complete the node starts in the same process, with its
// engine; "Run the setup again" stops the engine and the node; a node.json the node cannot use
// leaves it in setup mode with the reason, instead of exiting.
func TestRunWithSetup(t *testing.T) {
	dir := t.TempDir()
	fake := filepath.Join(dir, "fake")
	os.Mkdir(fake, 0o700)
	t.Setenv("FAKE_LLAMA_SERVER", fake)
	uiPort := freePort(t)
	t.Setenv("AISATHOME_UI_PORT", strconv.Itoa(uiPort))
	const password = "a-local-password-for-tests"
	layout := node.Layout{Config: filepath.Join(dir, "node.json"), StateFile: filepath.Join(dir, "state.json"),
		PasswordFile: filepath.Join(dir, "ui-password"), EngineProgram: "llama-server", Model: filepath.Join(dir, "model.gguf"),
		Setup: true, SetupStateFile: filepath.Join(dir, "setup-state.json")}
	os.WriteFile(layout.PasswordFile, []byte(password), 0o600)

	ctx, cancel := context.WithCancel(context.Background())
	var logw syncBuffer
	code := make(chan int, 1)
	go func() { code <- run(ctx, layout.Config, layout, &logw, "") }()
	defer func() {
		cancel()
		select {
		case c := <-code:
			if c != 0 {
				t.Errorf("exit code %d", c)
			}
		case <-time.After(30 * time.Second):
			t.Error("the agent did not stop")
		}
		if t.Failed() {
			t.Logf("agent log:\n%s", logw.String())
		}
	}()
	p := newPage(t, uiPort, password)
	resp, err := p.c.Get(p.base + "/")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/setup" {
		t.Fatalf("/ on a fresh install: %d %q", resp.StatusCode, resp.Header.Get("Location"))
	}
	st := p.status()
	if st.Setup == nil || st.Setup.Mode != "setup" || st.Setup.Configured || st.Connected || st.EngineProcess != nil {
		t.Fatalf("fresh install: %+v %+v", st.Status, st.Setup)
	}
	if pidOf(fake, 300*time.Millisecond) != 0 {
		t.Fatal("the engine runs before the setup")
	}
	if code, _ := p.post("/api/setup/leave", `{}`); code != http.StatusBadRequest {
		t.Fatalf("leave without a config: %d", code)
	}

	// node.json as the setup writes it, and the model in place.
	exe, _ := os.Executable()
	enginePort := freePort(t)
	os.WriteFile(layout.Model, []byte("gguf"), 0o600)
	os.WriteFile(layout.Config, []byte(fmt.Sprintf(`{"node_id":"n-0000000a","token":"nt-n-0000000a.s","gateway_url":"ws://127.0.0.1:1/v1/node/connect",
		"engine":{"tier":"standard","llama_server":%q,"port":%d}}`, exe, enginePort)), 0o600)
	if code, b := p.post("/api/setup/leave", `{}`); code != 200 {
		t.Fatalf("leave: %d %s", code, b)
	}
	pid := pidOf(fake, 10*time.Second)
	if pid == 0 {
		t.Fatal("the engine did not start after the setup")
	}
	st = p.waitFor("the node with its engine", func(s localui.StatusResponse) bool {
		return s.EngineProcess != nil && s.Setup.Mode == "run" && s.Setup.Configured && s.NodeID == "n-0000000a"
	})
	resp, _ = p.c.Get(p.base + "/")
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("/ after the setup: %d", resp.StatusCode)
	}

	// Run the setup again: the node and its engine stop.
	os.Remove(filepath.Join(fake, "pid"))
	if code, b := p.post("/api/setup/enter", `{}`); code != 200 {
		t.Fatalf("enter: %d %s", code, b)
	}
	p.waitFor("setup mode", func(s localui.StatusResponse) bool { return s.Setup.Mode == "setup" && s.EngineProcess == nil })
	waitGone(t, pid)

	// A node.json the node cannot use: the setup page says why; the agent keeps running.
	os.WriteFile(layout.Config, []byte(`{"token":"nt-n-0000000a.s","engine":{"tier":"huge"}}`), 0o600)
	if code, b := p.post("/api/setup/leave", `{}`); code != 200 {
		t.Fatalf("leave: %d %s", code, b)
	}
	st = p.waitFor("held in setup", func(s localui.StatusResponse) bool {
		return s.Setup.Mode == "setup" && strings.Contains(s.Setup.StartError, "engine.tier")
	})
	if pidOf(fake, 300*time.Millisecond) != 0 {
		t.Fatal("the engine started with a broken config")
	}
}

func waitGone(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		p, err := os.FindProcess(pid)
		if err != nil || !processAlive(p) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the engine still runs")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// A program already on the engine's port: the agent does not start the engine (the node would
// otherwise send its requests to that program) and says why.
func TestRunRefusesTakenEnginePort(t *testing.T) {
	dir := t.TempDir()
	fake := filepath.Join(dir, "fake")
	os.Mkdir(fake, 0o700)
	t.Setenv("FAKE_LLAMA_SERVER", fake)
	t.Setenv("AISATHOME_UI_PORT", "off")
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	port := ln.Addr().(*net.TCPAddr).Port
	exe, _ := os.Executable()
	cfgPath := writeConfig(t, fmt.Sprintf(`{"node_id":"n1","gateway_url":"ws://127.0.0.1:1/v1/node/connect","token":"t",
		"engine":{"tier":"standard","llama_server":%q,"model":%q,"port":%d}}`, exe, filepath.Join(dir, "m.gguf"), port))
	ctx, cancel := context.WithCancel(context.Background())
	var logw syncBuffer
	code := make(chan int, 1)
	go func() { code <- run(ctx, cfgPath, node.LinuxLayout, &logw, "") }()
	deadline := time.Now().Add(10 * time.Second)
	for !strings.Contains(logw.String(), "already in use") && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	<-code
	if !strings.Contains(logw.String(), fmt.Sprintf("port %d", port)) {
		t.Errorf("no word of the taken port; log:\n%s", logw.String())
	}
	if pidOf(fake, 0) != 0 {
		t.Error("the engine started on a taken port")
	}
}

// When the node fails (the watchdog), the agent must exit so the service's recovery restarts
// it: the setup's local UI must not keep it alive (Codex review).
func TestWithSetupExitsWhenTheNodeFails(t *testing.T) {
	dir := t.TempDir()
	uiPort := freePort(t)
	layout := node.Layout{Config: filepath.Join(dir, "node.json"), PasswordFile: filepath.Join(dir, "ui-password"),
		EngineProgram: "llama-server", Model: filepath.Join(dir, "model.gguf"), Setup: true}
	os.WriteFile(layout.Config, []byte(`{"token":"t","upstream":"http://127.0.0.1:1"}`), 0o600)
	var logw syncBuffer
	r := &runner{cfgPath: layout.Config, layout: layout, log: slog.New(slog.NewTextHandler(&logw, nil)), uiPort: uiPort}
	r.runNode = func(context.Context, *localui.Server) (int, error) {
		time.Sleep(200 * time.Millisecond) // the UI is up
		return 1, nil
	}
	code := make(chan int, 1)
	go func() { code <- r.withSetup(context.Background()) }()
	select {
	case c := <-code:
		if c != 1 {
			t.Fatalf("exit code %d, want 1", c)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the agent did not exit after the node failed")
	}
}
