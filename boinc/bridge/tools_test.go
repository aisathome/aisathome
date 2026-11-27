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
		"create_work":     logArgs,
		"cancel_jobs":     logArgs,
		"pilot_validator": logArgs,
		"dir_hier_path":   dirHier,
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
	if err := tl.CreateWork(ctx, "p-0123456789ab", "pc-p-0123456789ab-ff", cfg.tiers()[0]); err != nil {
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

func TestExecToolsCreateWorkInTheTiersApp(t *testing.T) {
	cfg := fakeProject(t, "")
	tier := Tier{Name: "standard", App: "pilot_std", Model: RemoteFile{URL: "https://example.org/q3.gguf", NBytes: 13146393504, MD5: strings.Repeat("b", 32)}}
	if err := NewExecTools(cfg).CreateWork(ctx, "p-0123456789ab", "pc-p-0123456789ab-ff", tier); err != nil {
		t.Fatal(err)
	}
	got := argsLog(t, cfg)
	if !strings.HasPrefix(got, "--appname\npilot_std\n") ||
		!strings.Contains(got, "--remote_file\nhttps://example.org/q3.gguf\n13146393504\n"+strings.Repeat("b", 32)+"\npc-p-0123456789ab-ff\n") {
		t.Errorf("create_work args for the standard tier:\n%s", got)
	}
	if err := NewExecTools(cfg).CreateWork(ctx, "p-0123456789ab", "f", Tier{App: "-x"}); err == nil {
		t.Error("create_work with an odd app name")
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
	err := NewExecTools(cfg).CreateWork(ctx, "p-0123456789ab", "f", cfg.tiers()[0])
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

func TestExecToolsValidate(t *testing.T) {
	cfg := fakeProject(t, "")
	cfg.Validator, cfg.MaxGrantedCredit = "bin/pilot_validator", 150000
	if _, err := NewExecTools(cfg).Validate(ctx, 4321, "pilot"); err != nil {
		t.Fatal(err)
	}
	want := strings.Join([]string{"--app", "pilot", "--one_pass", "--one_pass_N_WU", "1", "--min_wu_id", "4321",
		"--max_wu_id", "4321", "--post_assigned_credit", "--max_granted_credit", "150000",
		"--credit_dir", cfg.CreditDir, "--"}, "\n") + "\n"
	if got := argsLog(t, cfg); got != want {
		t.Errorf("validator args:\n%s\nwant:\n%s", got, want)
	}
	// Another tier's app: its validator run names that app.
	if _, err := NewExecTools(cfg).Validate(ctx, 7, "pilot_std"); err != nil {
		t.Fatal(err)
	}
	if got := argsLog(t, cfg); !strings.Contains(got, "--\n--app\npilot_std\n--one_pass\n") {
		t.Errorf("validator args for pilot_std: %q", got)
	}
	if _, err := NewExecTools(cfg).Validate(ctx, 7, "-x"); err == nil {
		t.Error("ran the validator with an odd app name")
	}
	cfg.Validator = ""
	if _, err := NewExecTools(cfg).Validate(ctx, 1, "pilot"); err == nil {
		t.Error("ran without a validator")
	}
}
