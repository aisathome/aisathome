//go:build !windows

// SPDX-License-Identifier: Apache-2.0
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/aisathome/aisathome/protocol"
)

// The pilot as BOINC's wrapper runs it: a process in a task folder that also holds
// init_data.xml with the volunteer's account key. It starts its engine (this test binary as a
// fake llama-server), connects, serves a request that never ends, and on SIGTERM leaves with
// "pilot: sigterm" and exits 0 within the wrapper's 10 s, taking the engine with it. The account
// key appears nowhere it could leave the machine: the gateway's frames and headers, the log
// (stderr, which BOINC uploads and shows on the result page) or the output file; the credential
// appears only in the Authorization header.
func TestPilotProcessSigterm(t *testing.T) {
	const key = "fedcba9876543210fedcba9876543210"
	const cred = "nt-n-0000cafe.pilotsecretvalue"
	slot, fake := t.TempDir(), t.TempDir()
	for name, content := range map[string]string{
		"init_data.xml": "<app_init_data>\n<authenticator>" + key + "</authenticator>\n</app_init_data>\n",
		"credential":    cred + "\n",
		"model.gguf":    "GGUF",
	} {
		if err := os.WriteFile(filepath.Join(slot, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	var mu sync.Mutex
	var wire bytes.Buffer
	reason := ""
	streaming := make(chan struct{})
	var once sync.Once
	closed := make(chan struct{})
	gw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		for k, v := range r.Header {
			wire.WriteString(k + ": " + strings.Join(v, ",") + "\n")
		}
		mu.Unlock()
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		ctx := r.Context()
		if _, b, err := c.Read(ctx); err != nil {
			return
		} else {
			mu.Lock()
			wire.Write(b)
			mu.Unlock()
		}
		send := func(m protocol.Msg) { b, _ := json.Marshal(m); c.Write(ctx, websocket.MessageText, b) }
		send(protocol.Msg{Type: protocol.TypeWelcome, GatewayID: "gw", ProtocolVersion: protocol.Version})
		go func() {
			time.Sleep(500 * time.Millisecond) // the node waits for a healthy engine first
			send(protocol.Msg{Type: protocol.TypeRequest, ReqID: "r1",
				Body: json.RawMessage(`{"model":"m","stream":true,"messages":[{"role":"user","content":"hi"}]}`)})
		}()
		for {
			_, b, err := c.Read(ctx)
			if err != nil {
				var ce websocket.CloseError
				mu.Lock()
				if errors.As(err, &ce) {
					reason = ce.Reason
				}
				mu.Unlock()
				close(closed)
				return
			}
			mu.Lock()
			wire.Write(b)
			mu.Unlock()
			if bytes.Contains(b, []byte(`"type":"chunk"`)) {
				once.Do(func() { close(streaming) })
			}
		}
	}))
	defer gw.Close()

	var stderr bytes.Buffer
	cmd := exec.Command(os.Args[0], "--gateway", "ws"+strings.TrimPrefix(gw.URL, "http")+"/v1/node/connect",
		"--credential-file", "credential", "--model", "model.gguf", "--llama-server", os.Args[0],
		"--summary", "summary.json", "--checkpoint", "pilot_checkpoint", "--fraction-done", "pilot_fraction")
	cmd.Dir = slot
	cmd.Env = append(os.Environ(), "AISATHOME_TEST_PILOT=1", "FAKE_LLAMA_SERVER="+fake)
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer cmd.Process.Kill()
	select {
	case <-streaming:
	case <-time.After(30 * time.Second):
		t.Fatalf("no streamed answer reached the gateway; pilot log:\n%s", stderr.String())
	}
	enginePID, _ := strconv.Atoi(readFile(t, filepath.Join(fake, "pid")))
	// BOINC's paths are relative to the task folder, and the engine runs in its own folder: it
	// must get the model as an absolute path.
	args := strings.Split(readFile(t, filepath.Join(fake, "args")), "\n")
	if i := slices.Index(args, "-m"); i < 0 || i+1 >= len(args) || args[i+1] != filepath.Join(slot, "model.gguf") {
		t.Errorf("engine arguments %q: want -m %s", args, filepath.Join(slot, "model.gguf"))
	}

	start := time.Now()
	cmd.Process.Signal(syscall.SIGTERM)
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("exit: %v (want 0); log:\n%s", err, stderr.String())
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("still running 10 s after SIGTERM (the wrapper would kill it); log:\n%s", stderr.String())
	}
	t.Logf("stop took %s", time.Since(start))
	if took := time.Since(start); took > 9*time.Second {
		t.Errorf("stop took %s", took)
	}
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("the gateway connection did not end")
	}
	mu.Lock()
	gotReason, gotWire := reason, wire.String()
	mu.Unlock()
	if gotReason != "pilot: sigterm" {
		t.Errorf("close reason %q, want %q", gotReason, "pilot: sigterm")
	}
	if enginePID > 0 {
		if err := syscall.Kill(enginePID, 0); err == nil {
			t.Errorf("the engine (pid %d) outlived the pilot", enginePID)
			syscall.Kill(enginePID, syscall.SIGKILL)
		}
	}

	summary := readFile(t, filepath.Join(slot, "summary.json"))
	if !strings.Contains(summary, `"reason":"sigterm"`) {
		t.Errorf("summary %s", summary)
	}
	for what, s := range map[string]string{"gateway": gotWire, "log": stderr.String(), "summary": summary} {
		if strings.Contains(s, key) {
			t.Errorf("the account key appears in the %s", what)
		}
		if what != "gateway" && strings.Contains(s, "pilotsecretvalue") {
			t.Errorf("the credential appears in the %s", what)
		}
	}
	if !strings.Contains(gotWire, "Authorization: Bearer "+cred) {
		t.Error("the pilot did not authenticate with its credential")
	}
	if !strings.Contains(readFile(t, filepath.Join(slot, "engine.log")), "fake llama-server") {
		t.Error("the engine's output did not go to engine.log")
	}
}

// Wrong use stops before anything starts.
func TestPilotUsage(t *testing.T) {
	var out bytes.Buffer
	if code := runPilot([]string{"--nope"}, &out); code != 2 {
		t.Errorf("unknown flag: %d", code)
	}
	if code := runPilot([]string{"--gateway", "wss://x/v1/node/connect", "extra"}, &out); code != 2 {
		t.Errorf("extra argument: %d", code)
	}
	dir := t.TempDir()
	if code := runPilot([]string{"--gateway", "wss://x/v1/node/connect", "--credential-file", filepath.Join(dir, "none"),
		"--engine-log", ""}, &out); code != 1 {
		t.Errorf("missing credential: %d", code)
	}
}

func readFile(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("%s: %v", p, err)
	}
	return string(b)
}
