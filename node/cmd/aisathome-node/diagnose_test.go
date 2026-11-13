// SPDX-License-Identifier: Apache-2.0
package main

import (
	"archive/zip"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aisathome/aisathome/node"
)

type diagSMI struct{}

func (diagSMI) Query(_ context.Context, args ...string) (string, error) {
	return "NVIDIA GeForce RTX 4090, 24564 MiB, driver 566.36 " + strings.Join(args, " "), nil
}

// The diagnostics zip holds what a failed test session needs, and neither the token nor the
// local password, wherever they appear (node.json, logs, command output).
func TestDiagnose(t *testing.T) {
	dir := t.TempDir()
	const token, password = "nt-n-0000000a.Sup3rSecretTokenPart", "local-password-0123456789"
	other := "nt-n-0000000b.AnotherSecret" // an older token in an old log
	l := node.Layout{Config: filepath.Join(dir, "node.json"), StateFile: filepath.Join(dir, "state.json"),
		PasswordFile: filepath.Join(dir, "ui-password"), LogFile: filepath.Join(dir, "logs", "node.log"),
		EngineLogFile: filepath.Join(dir, "logs", "engine.log"), EngineProgram: filepath.Join("engine", "llama-server"),
		Model: filepath.Join(dir, "models", "model.gguf"), SetupStateFile: filepath.Join(dir, "setup-state.json")}
	gw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			fmt.Fprintln(w, "ok")
		}
	}))
	defer gw.Close()
	os.MkdirAll(filepath.Join(dir, "logs"), 0o700)
	os.MkdirAll(filepath.Join(dir, "models"), 0o700)
	os.WriteFile(l.Model, []byte("gguf"), 0o600)
	os.WriteFile(l.Config, []byte(fmt.Sprintf(`{"node_id":"n-0000000a","token":%q,"gateway_url":%q,"engine":{"tier":"big"}}`,
		token, "ws"+strings.TrimPrefix(gw.URL, "http")+"/v1/node/connect")), 0o600)
	os.WriteFile(l.PasswordFile, []byte(password+"\n"), 0o600)
	os.WriteFile(l.LogFile, []byte("msg=starting\nleaked "+token+" and "+password+"\n"), 0o600)
	os.WriteFile(l.LogFile+".1", []byte("older: "+other+"\n"), 0o600)
	os.WriteFile(l.EngineLogFile, []byte("llama-server: model loaded\n"), 0o600)
	trayLog := filepath.Join(dir, "tray.log")
	os.WriteFile(trayLog, []byte("msg=state state=problem\n"), 0o600)
	os.WriteFile(l.SetupStateFile, []byte(`{"mode":"setup","card":{"name":"RTX 4090"},"speed":{"error":"slow"}}`), 0o600)
	os.WriteFile(l.StateFile, []byte(`{"events":[{"kind":"rejected","text":"HTTP 401"}]}`), 0o600)
	ui, _ := net.Listen("tcp", "127.0.0.1:0")
	go http.Serve(ui, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"state":"disconnected","rejected":"HTTP 401: unknown node token"}`)
	}))
	defer ui.Close()

	var ran []string
	d := diagnosis{layout: l, cfgPath: l.Config, exeDir: dir, uiPort: ui.Addr().(*net.TCPAddr).Port, smi: diagSMI{},
		trayLog: trayLog, now: func() time.Time { return time.Date(2026, 11, 12, 9, 0, 0, 0, time.UTC) },
		run: func(_ context.Context, name string, args ...string) (string, error) {
			ran = append(ran, name)
			return "output of " + name + " mentioning " + password, nil
		}}
	out := filepath.Join(dir, "diag.zip")
	if _, err := d.write(context.Background(), out); err != nil {
		t.Fatal(err)
	}
	zr, err := zip.OpenReader(out)
	if err != nil {
		t.Fatal(err)
	}
	defer zr.Close()
	files := map[string]string{}
	for _, f := range zr.File {
		rc, _ := f.Open()
		b, _ := io.ReadAll(rc)
		rc.Close()
		files[f.Name] = string(b)
		for _, secret := range []string{"Sup3rSecretTokenPart", password, "AnotherSecret"} {
			if strings.Contains(string(b), secret) {
				t.Errorf("%s holds a secret (%s):\n%s", f.Name, secret, b)
			}
		}
	}
	for name, want := range map[string]string{
		"node.json":        `"token": "<redacted: 34 characters, node id \"n-0000000a\">"`,
		"version.txt":      "agent: aisathome-node/",
		"status.json":      "unknown node token",
		"setup-state.json": "RTX 4090",
		"state.json":       "rejected",
		"nvidia-smi.txt":   "RTX 4090",
		"logs/node.log":    "leaked <redacted> and <redacted>",
		"logs/node.log.1":  "nt-n-0000000b.<redacted>",
		"logs/engine.log":  "model loaded",
		"logs/tray.log":    "state=problem",
		"files.txt":        filepath.Join("models", "model.gguf"), // the folder's own separator, as Explorer shows it
		"gateway.txt":      `/healthz: 200 "ok"`,
		"errors.txt":       "",
		"service/" + serviceCommands(l, dir)[0].name + ".txt": "output of",
	} {
		got, ok := files[name]
		if !ok {
			t.Errorf("no %s in the zip (has %v)", name, keys(files))
			continue
		}
		if !strings.Contains(got, want) {
			t.Errorf("%s lacks %q:\n%s", name, want, got)
		}
	}
	if strings.TrimSpace(files["errors.txt"]) != "" {
		t.Errorf("errors: %s", files["errors.txt"])
	}
	if len(ran) == 0 {
		t.Error("no service commands ran")
	}
	// An existing zip is never overwritten.
	if _, err := d.write(context.Background(), out); err == nil {
		t.Error("overwrote an existing file")
	}
}

func keys(m map[string]string) []string {
	var k []string
	for n := range m {
		k = append(k, n)
	}
	return k
}

// A node.json that is not valid JSON is left out, and its token (any form, also an operator's)
// is still redacted from the logs (Codex review).
func TestDiagnoseBrokenConfig(t *testing.T) {
	dir := t.TempDir()
	const token = "operator-token-Zq81xx"
	l := node.Layout{Config: filepath.Join(dir, "node.json"), LogFile: filepath.Join(dir, "node.log")}
	os.WriteFile(l.Config, []byte(`{"token": "`+token+`", "upstream": }`), 0o600)
	os.WriteFile(l.LogFile, []byte("using "+token+"\n"), 0o600)
	d := diagnosis{layout: l, cfgPath: l.Config, exeDir: dir, uiPort: 1, smi: diagSMI{}, now: time.Now,
		run: func(context.Context, string, ...string) (string, error) { return "", nil }}
	out := filepath.Join(dir, "diag.zip")
	if _, err := d.write(context.Background(), out); err != nil {
		t.Fatal(err)
	}
	zr, err := zip.OpenReader(out)
	if err != nil {
		t.Fatal(err)
	}
	defer zr.Close()
	for _, f := range zr.File {
		rc, _ := f.Open()
		b, _ := io.ReadAll(rc)
		rc.Close()
		if strings.Contains(string(b), token) {
			t.Errorf("%s holds the token:\n%s", f.Name, b)
		}
		if f.Name == "node.json" && !strings.Contains(string(b), "not included") {
			t.Errorf("node.json: %s", b)
		}
	}
}
