// SPDX-License-Identifier: Apache-2.0
package bridge

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeProject is a project directory whose bin/ programs are shell scripts that log their
// arguments, one per line, to args.log.
func fakeProject(t *testing.T, dirHier string) Config {
	t.Helper()
	cfg := testConfig(t, "https://gw.example")
	dir := cfg.ProjectDir
	os.MkdirAll(filepath.Join(dir, "bin"), 0o755)
	logArgs := `for a in "$@"; do printf '%s\n' "$a"; done >> "` + filepath.Join(dir, "args.log") + `"; echo -- >> "` + filepath.Join(dir, "args.log") + `"`
	if dirHier == "" {
		dirHier = `mkdir -p "` + dir + `/download/1f" && echo "` + dir + `/download/1f/$1"`
	}
	scripts := map[string]string{
		"create_work":   logArgs,
		"cancel_jobs":   logArgs,
		"dir_hier_path": dirHier,
	}
	for name, body := range scripts {
		if err := os.WriteFile(filepath.Join(dir, "bin", name), []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return cfg
}

func argsLog(t *testing.T, cfg Config) string {
	b, _ := os.ReadFile(filepath.Join(cfg.ProjectDir, "args.log"))
	return string(b)
}

func TestExecToolsCreateWork(t *testing.T) {
	cfg := fakeProject(t, "")
	cfg.Batch, cfg.TargetHost = 2, 17
	tl := NewExecTools(cfg)
	if err := tl.Stage(ctx, "pc-p-0123456789ab-ff", []byte("pc-0123456789ab.secret\n")); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(cfg.ProjectDir, "download/1f/pc-p-0123456789ab-ff")
	info, err := os.Stat(p)
	if err != nil || info.Mode().Perm()&0o007 != 0 {
		t.Fatalf("staged file: %v %v (others must not read it)", info, err)
	}
	if err := tl.Stage(ctx, "pc-p-0123456789ab-ff", []byte("x")); err == nil {
		t.Error("staged over an existing file")
	}
	if err := tl.CreateWork(ctx, "p-0123456789ab", "pc-p-0123456789ab-ff"); err != nil {
		t.Fatal(err)
	}
	want := strings.Join([]string{"--appname", "pilot", "--wu_name", "p-0123456789ab", "--batch", "2",
		"--wu_template", "templates/pilot_in", "--result_template", "templates/pilot_out",
		"--command_line", "--passthrough_child --gateway wss://gw.example/v1/node/connect",
		"--target_host", "17",
		"--remote_file", "https://example.org/model.gguf", "1000", strings.Repeat("a", 32), "pc-p-0123456789ab-ff", "--"}, "\n") + "\n"
	if got := argsLog(t, cfg); got != want {
		t.Errorf("create_work args:\n%s\nwant:\n%s", got, want)
	}
	if err := tl.Cancel(ctx, "p-0123456789ab"); err != nil {
		t.Fatal(err)
	}
	if got := argsLog(t, cfg); !strings.HasSuffix(got, "--name\np-0123456789ab\n--\n") {
		t.Errorf("cancel_jobs args: %q", got)
	}
	if err := tl.Unstage(ctx, "pc-p-0123456789ab-ff"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Error("not unstaged")
	}
}

func TestExecToolsRefusesOddPaths(t *testing.T) {
	cfg := fakeProject(t, `echo /etc/$1`)
	if err := NewExecTools(cfg).Stage(ctx, "pc-x", []byte("x")); err == nil {
		t.Error("staged outside the download directory")
	}
	cfg = fakeProject(t, "")
	for _, name := range []string{"../x", "-x", ".x", "a/b", ""} {
		if err := NewExecTools(cfg).Stage(ctx, name, []byte("x")); err == nil {
			t.Errorf("staged %q", name)
		}
	}
	// A failing program: its output is in the error, shortened.
	os.WriteFile(filepath.Join(cfg.ProjectDir, "bin", "create_work"), []byte("#!/bin/sh\necho 'no such template' >&2\nexit 1\n"), 0o755)
	err := NewExecTools(cfg).CreateWork(ctx, "p-0123456789ab", "f")
	if err == nil || !strings.Contains(err.Error(), "no such template") {
		t.Errorf("create_work failure: %v", err)
	}
}

func TestCheckNotListed(t *testing.T) {
	var asked []string
	listed := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked = append(asked, r.URL.Path)
		if listed && strings.HasSuffix(r.URL.Path, "/1f/") {
			w.Write([]byte("<html><title>Index of /project/download/1f</title><a href=\"pc-p-0123456789ab-ff\">"))
			return
		}
		http.Error(w, "Forbidden", http.StatusForbidden)
	}))
	defer srv.Close()
	cfg := fakeProject(t, "")
	cfg.WebURL = srv.URL + "/project/"
	if err := NewExecTools(cfg).CheckNotListed(ctx); err != nil {
		t.Fatal(err)
	}
	if strings.Join(asked, " ") != "/project/download/ /project/download/1f/" {
		t.Errorf("asked %v", asked)
	}
	listed = true
	if err := NewExecTools(cfg).CheckNotListed(ctx); err == nil || !strings.Contains(err.Error(), "must not be listed") {
		t.Errorf("a listed fan-out directory: %v", err)
	}
	srv.Close()
	if err := NewExecTools(cfg).CheckNotListed(ctx); err == nil {
		t.Error("no error when the web server cannot be asked")
	}
}
