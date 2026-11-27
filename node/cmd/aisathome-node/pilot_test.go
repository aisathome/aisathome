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

// The pilot as BOINC's wrapper runs it, from boinc/job.xml and the work unit's command line in
// boinc/README.md: in a slot folder whose files are BOINC soft links into the project folder,
// with only job.xml's environment, and with the engine unpacked from the bundle (a llama-server
// script that runs this test binary as a fake). The slot also holds init_data.xml with the
// volunteer's account key. The pilot connects, serves a request that never ends, and on SIGTERM
// leaves with "pilot: sigterm" and exits 0 within the wrapper's 10 s, taking the engine with it.
// The account key appears nowhere it could leave the machine: the gateway's frames and headers,
// the wrapper's and the pilot's log (stderr, which BOINC uploads and shows on the result page) or
// the output file; the credential appears only in the Authorization header, and its physical
// file name (its download URL) nowhere.
func TestPilotProcessSigterm(t *testing.T) {
	const key = "fedcba9876543210fedcba9876543210"
	const cred = "nt-n-0000cafe.pilotsecretvalue"
	const credPhys = "pc-p-test-20261117-0123456789abcdef0123456789abcdef"
	base, fake := t.TempDir(), t.TempDir()
	slot := filepath.Join(base, "slots", "0")
	projectDir := filepath.Join(base, "projects", "example.org_aisathome")
	os.MkdirAll(slot, 0o755)
	os.MkdirAll(projectDir, 0o755)
	bundle := "aisathome-node-bundle-linux-amd64_0123456789ab.tar.xz"
	writeBundle(t, filepath.Join(projectDir, bundle), "#!/bin/sh\nFAKE_LLAMA_SERVER='"+fake+"' exec '"+os.Args[0]+"' \"$@\"\n")
	for logical, phys := range map[string]string{"model.gguf": "js_0123456789abcdef", "credential": credPhys,
		"engine.tar.xz": bundle, "summary.json": "p-test-20261117_0_r0_0"} {
		os.WriteFile(filepath.Join(slot, logical), []byte("<soft_link>../../projects/example.org_aisathome/"+phys+"</soft_link>\n"), 0o644)
	}
	for name, content := range map[string]string{
		filepath.Join(slot, "init_data.xml"):             "<app_init_data>\n<authenticator>" + key + "</authenticator>\n</app_init_data>\n",
		filepath.Join(projectDir, credPhys):              cred + "\n",
		filepath.Join(projectDir, "js_0123456789abcdef"): "GGUF",
	} {
		if err := os.WriteFile(name, []byte(content), 0o600); err != nil {
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

	gateway := "ws" + strings.TrimPrefix(gw.URL, "http") + "/v1/node/connect"
	wuArgs := strings.Fields(wuCommandLine(t, gateway))
	child, err := wrapperChildArgs(wuArgs)
	if err != nil {
		t.Fatalf("the wrapper refuses the documented work-unit command line %q: %v", wuArgs, err)
	}
	logged, args, env := taskCommand(t, child, projectDir, func(name string) string { return boincResolve(slot, name) })
	if strings.Contains(logged, credPhys) {
		t.Errorf("the wrapper's log line names the credential's physical file: %s", logged)
	}
	var stderr bytes.Buffer
	cmd := exec.Command(os.Args[0], args...)
	cmd.Dir = slot
	cmd.Env = append(env, "AISATHOME_TEST_PILOT=1")
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
	if _, err := os.Stat(filepath.Join(projectDir, "engine", strings.TrimSuffix(bundle, ".tar.xz"),
		"aisathome-node-bundle-linux-amd64", "bin", "llama-server")); err != nil {
		t.Errorf("the engine was not unpacked into the project folder: %v", err)
	}
	// The card was read as the engine sees it: the GPU the client assigned (job.xml), the bundle's libraries.
	if env := readFile(t, filepath.Join(fake, "list-devices-env")); !strings.Contains(env, "CUDA_VISIBLE_DEVICES=0\n") ||
		!strings.Contains(env, "LD_LIBRARY_PATH="+filepath.Join(projectDir, "engine")) {
		t.Errorf("--list-devices ran with %q", env)
	}
	if _, err := os.Stat(filepath.Join(projectDir, "engine", "speed-checks.json")); err != nil {
		t.Errorf("the passed speed test was not remembered in the project folder: %v", err)
	}
	model := filepath.Join(projectDir, "js_0123456789abcdef")
	engineArgs := strings.Split(readFile(t, filepath.Join(fake, "args")), "\n")
	if i := slices.Index(engineArgs, "-m"); i < 0 || i+1 >= len(engineArgs) || engineArgs[i+1] != model {
		t.Errorf("engine arguments %q: want -m %s", engineArgs, model)
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
	if log := stderr.String(); !strings.Contains(log, "chosen_by=card") || !strings.Contains(log, "tps=40") {
		t.Errorf("no tier or speed test in the log:\n%s", log)
	}
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

	summary := readFile(t, filepath.Join(projectDir, "p-test-20261117_0_r0_0"))
	if !strings.Contains(summary, `"reason":"sigterm"`) {
		t.Errorf("summary %s", summary)
	}
	for what, s := range map[string]string{"gateway": gotWire, "log": logged + "\n" + stderr.String(), "summary": summary} {
		if strings.Contains(s, key) {
			t.Errorf("the account key appears in the %s", what)
		}
		if what != "gateway" && (strings.Contains(s, "pilotsecretvalue") || strings.Contains(s, credPhys)) {
			t.Errorf("the credential or its file name appears in the %s", what)
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
	if code := runPilot([]string{"--gateway", "wss://x/v1/node/connect", "--min-tps", "-1"}, &out); code != 2 {
		t.Errorf("negative speed floor: %d", code)
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
