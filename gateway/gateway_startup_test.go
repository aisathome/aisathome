// SPDX-License-Identifier: Apache-2.0
package gateway

import (
	"database/sql"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// openUnder lists the files this process has open under dir (Linux; nil elsewhere).
func openUnder(t *testing.T, dir string) []string {
	t.Helper()
	ents, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range ents {
		if p, err := os.Readlink(filepath.Join("/proc/self/fd", e.Name())); err == nil && strings.HasPrefix(p, dir) {
			out = append(out, p)
		}
	}
	return out
}

// settled waits until no more goroutines run than before (background work that was started and
// stopped needs a moment to return) and reports the count.
func settled(before int) int {
	n := runtime.NumGoroutine()
	for deadline := time.Now().Add(3 * time.Second); n > before && time.Now().Before(deadline); n = runtime.NumGoroutine() {
		time.Sleep(10 * time.Millisecond)
	}
	return n
}

// startupConfig has every part that starts background work: accounts with email sign-in (the mail
// worker), a keys file, a state dir (saves, the science copy), a usage log.
func startupConfig(dir string) Config {
	return Config{StateDir: filepath.Join(dir, "state"), UsageLog: filepath.Join(dir, "usage.jsonl"), KeysFile: filepath.Join(dir, "keys.json"),
		Accounts: &AccountsConfig{File: filepath.Join(dir, "accounts.json"), PublicURL: "https://site.test", SessionSecret: strings.Repeat("s", 32),
			Email: &EmailConfig{SMTPHost: "127.0.0.1", SMTPPort: 2525, SMTPUser: "u", SMTPPassword: "p", From: "Test <noreply@site.test>",
				Keys: []EmailKey{{ID: "k1", Secret: strings.Repeat("e", 32)}}}},
		Projects: []Project{{ID: "lab", KeySHA256: []string{keyHash(testProjectKey)}}}}
}

// A gateway that fails at any step of its start leaves no goroutine and no open file behind; one
// that started and closed neither (refactor review 2026-11-17, finding 4).
func TestGatewayStartupCleanup(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	for _, tc := range []struct {
		name  string
		setup func(dir string, cfg *Config)
		want  string // in the error; "" = starts
	}{
		{"projects", func(_ string, c *Config) { c.Projects[0].ID = "Bad Id" }, "project"},
		{"keys file", func(dir string, _ *Config) { os.WriteFile(filepath.Join(dir, "keys.json"), []byte("{broken"), 0o600) }, "keys"},
		{"state dir", func(dir string, _ *Config) { os.WriteFile(filepath.Join(dir, "state"), []byte("a file"), 0o600) }, "state dir"},
		{"usage log", func(dir string, c *Config) { c.UsageLog = filepath.Join(dir, "missing", "usage.jsonl") }, "usage log"},
		{"science db", func(dir string, _ *Config) {
			os.MkdirAll(filepath.Join(dir, "state"), 0o700)
			db, _ := sql.Open("sqlite", filepath.Join(dir, "state", ScienceDBFile))
			db.Exec("PRAGMA user_version = 999")
			db.Close()
		}, "newer"},
		{"starts", func(string, *Config) {}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			cfg := startupConfig(dir)
			tc.setup(dir, &cfg)
			before := runtime.NumGoroutine()
			g, err := New(cfg, log)
			if tc.want == "" {
				if err != nil {
					t.Fatal(err)
				}
				if open := openUnder(t, dir); len(open) == 0 && runtime.GOOS == "linux" {
					t.Fatal("a started gateway holds no files: the check below would prove nothing")
				}
				if runtime.NumGoroutine() <= before {
					t.Fatal("a started gateway runs no background work: the check below would prove nothing")
				}
				if err := g.Close(); err != nil {
					t.Fatal(err)
				}
			} else if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("New: %v, want an error about %q", err, tc.want)
			}
			if n := settled(before); n > before {
				buf := make([]byte, 1<<16)
				t.Fatalf("%d goroutines left running (had %d):\n%s", n-before, before, buf[:runtime.Stack(buf, true)])
			}
			if open := openUnder(t, dir); len(open) > 0 {
				t.Fatalf("files left open: %v", open)
			}
		})
	}
}
