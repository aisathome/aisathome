// BOINC's own programs for work units (tools/ in BOINC's source): create_work, cancel_jobs and
// dir_hier_path, run in the project directory as the project's user. Not stage_file: server
// 1.6.3's copy calls a PHP function that release removed.
//
// SPDX-License-Identifier: Apache-2.0
package bridge

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// ExecTools runs the project's programs.
type ExecTools struct {
	cfg Config
}

// NewExecTools returns Tools for the project in cfg.
func NewExecTools(cfg Config) *ExecTools { return &ExecTools{cfg: cfg} }

func (t *ExecTools) Stage(ctx context.Context, name string, content []byte) error {
	p, err := t.downloadPath(ctx, name)
	if err != nil {
		return err
	}
	// Group-readable: the web server serves it to the client the work unit goes to.
	f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o640)
	if err != nil {
		return fmt.Errorf("staging an input file: %w", err)
	}
	if _, err = f.Write(content); err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(p)
		return fmt.Errorf("staging an input file: %w", err)
	}
	return nil
}

func (t *ExecTools) Unstage(ctx context.Context, name string) error {
	p, err := t.downloadPath(ctx, name)
	if err != nil {
		return err
	}
	if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func (t *ExecTools) CreateWork(ctx context.Context, name, credentialFile string, tier Tier) error {
	if !appNameRe.MatchString(tier.App) {
		return fmt.Errorf("create_work: app %q", tier.App)
	}
	args := []string{"--appname", tier.App, "--wu_name", name, "--batch", strconv.Itoa(t.cfg.Batch),
		"--wu_template", t.cfg.WUTemplate, "--result_template", t.cfg.ResultTemplate,
		// The wrapper refuses options it does not know and hands the task what follows
		// --passthrough_child (the pilot app's job.xml).
		"--command_line", "--passthrough_child --gateway " + t.cfg.PilotGateway}
	if t.cfg.TargetHost > 0 {
		args = append(args, "--target_host", strconv.FormatInt(t.cfg.TargetHost, 10))
	}
	// Input files in the template's order: 0 the model (remote, sticky), 1 the credential.
	args = append(args, "--remote_file", tier.Model.URL, strconv.FormatInt(tier.Model.NBytes, 10), tier.Model.MD5,
		credentialFile)
	_, err := t.run(ctx, "bin/create_work", args...)
	return err
}

// Validate runs the validator on one work unit and ends (boinc/validator: --one_pass_N_WU 1 and
// the id range keep it to that work unit, and it leaves one without its credit file as it was).
func (t *ExecTools) Validate(ctx context.Context, workunitID int64, app string) (string, error) {
	if t.cfg.Validator == "" {
		return "", errors.New("no validator configured")
	}
	if !appNameRe.MatchString(app) {
		return "", fmt.Errorf("validator: app %q", app)
	}
	id := strconv.FormatInt(workunitID, 10)
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, filepath.Join(t.cfg.ProjectDir, t.cfg.Validator), "--app", app,
		"--one_pass", "--one_pass_N_WU", "1", "--min_wu_id", id, "--max_wu_id", id, "--post_assigned_credit",
		"--max_granted_credit", strconv.FormatFloat(t.cfg.MaxGrantedCredit, 'f', -1, 64),
		"--credit_dir", t.cfg.CreditDir)
	cmd.Dir = t.cfg.ProjectDir
	out, err := cmd.CombinedOutput() // BOINC's daemons log to stderr
	tail := strings.TrimSpace(string(out))
	if len(tail) > 600 {
		tail = "..." + tail[len(tail)-600:]
	}
	if err != nil {
		return tail, fmt.Errorf("%s: %w", t.cfg.Validator, err)
	}
	return tail, nil
}

func (t *ExecTools) Cancel(ctx context.Context, name string) error {
	_, err := t.run(ctx, "bin/cancel_jobs", "--name", name)
	return err
}

// downloadPath is where an input file goes (dir_hier_path also makes its directory).
func (t *ExecTools) downloadPath(ctx context.Context, name string) (string, error) {
	if !safeFileName(name) {
		return "", errors.New("bad input file name")
	}
	out, err := t.run(ctx, "bin/dir_hier_path", name)
	if err != nil {
		return "", err
	}
	p := strings.TrimSpace(string(out))
	if !filepath.IsAbs(p) {
		p = filepath.Join(t.cfg.ProjectDir, p)
	}
	p = filepath.Clean(p)
	dl := filepath.Join(t.cfg.ProjectDir, "download") + string(filepath.Separator)
	if filepath.Base(p) != name || !strings.HasPrefix(p, dl) {
		return "", fmt.Errorf("dir_hier_path: unexpected path for an input file")
	}
	return p, nil
}

// run runs a project program with a time limit; its output goes into the error, shortened.
func (t *ExecTools) run(ctx context.Context, prog string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, filepath.Join(t.cfg.ProjectDir, prog), args...)
	cmd.Dir = t.cfg.ProjectDir
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String() + " " + stdout.String())
		if len(msg) > 300 {
			msg = msg[:300] + "..."
		}
		return nil, fmt.Errorf("%s: %w: %s", prog, err, msg)
	}
	return stdout.Bytes(), nil
}

// safeFileName: letters, digits, '-', '_' and '.', not starting with '-' or '.'.
func safeFileName(s string) bool {
	if s == "" || len(s) > 128 || s[0] == '-' || s[0] == '.' {
		return false
	}
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' || r == '.') {
			return false
		}
	}
	return true
}

// CheckNotListed fails unless the project's web server refuses to list the download directory
// and one of its fan-out directories. A pilot's credential lies there as an input file that any
// client may fetch by name, so its unguessable name is what protects it; a listing would give it
// away. Checked on the project's own web server (web_url), which serves the download directory
// as BOINC's stock configuration does (<project>/download).
func (t *ExecTools) CheckNotListed(ctx context.Context) error {
	p, err := t.downloadPath(ctx, "listing-check") // makes its fan-out directory, writes nothing
	if err != nil {
		return err
	}
	rel, err := filepath.Rel(filepath.Join(t.cfg.ProjectDir, "download"), filepath.Dir(p))
	if err != nil {
		return err
	}
	client := &http.Client{Timeout: 15 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	for _, u := range []string{t.cfg.WebURL + "download/", t.cfg.WebURL + "download/" + filepath.ToSlash(rel) + "/"} {
		req, err := http.NewRequestWithContext(ctx, "GET", u, nil)
		if err != nil {
			return err
		}
		resp, err := client.Do(req)
		if err != nil {
			return fmt.Errorf("download directory: %w", err)
		}
		io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		if resp.StatusCode/100 == 2 {
			return fmt.Errorf("the web server answers %s with HTTP %d: the download directory must not be listed (Options -Indexes)", u, resp.StatusCode)
		}
	}
	return nil
}
