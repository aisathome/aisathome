// SPDX-License-Identifier: Apache-2.0
package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aisathome/aisathome/gateway"
)

// node-failures sums the saved failure counts per node and model and per model (the tier) over the
// last UTC days, one node if asked, and lists failure categories and refusals; it needs state_dir.
func TestAccountsNodeFailures(t *testing.T) {
	dir := t.TempDir()
	now := time.Now().UTC()
	day := func(d int) string { return now.AddDate(0, 0, -d).Format(time.DateOnly) }
	st := map[string]any{"version": 1, "days": []any{
		map[string]any{"day": day(20), "rows": []any{
			map[string]any{"node": "n-old", "model": "qwen3.8-27b", "ctx": 196608, "kinds": map[string]int{"ok": 50}}}},
		map[string]any{"day": day(1), "rows": []any{
			map[string]any{"node": "n-big", "model": "qwen3.8-27b", "ctx": 196608, "kinds": map[string]int{"ok": 9, "length_max_tokens": 1}},
			map[string]any{"node": "n-small", "model": "qwen3.8-27b-q3", "ctx": 81920, "kinds": map[string]int{
				"ok": 4, "length_context": 2, "bad_tool_calls": 1, "failed:context_length_exceeded": 2,
				"failed:tool_call_parse": 1, "failed:node stalled": 1}},
			map[string]any{"node": "n-small2", "model": "qwen3.8-27b-q3", "ctx": 81920, "kinds": map[string]int{"ok": 3, "blocked": 1}},
			map[string]any{"node": "n-\x1b[2Jevil", "model": "m\nNODE forged", "kinds": map[string]int{"ok": 1}},
			map[string]any{"node": "-", "model": "qwen3.8-27b-q3", "kinds": map[string]int{"refused:too_long": 3}}}},
		map[string]any{"day": day(0), "rows": []any{
			map[string]any{"node": "n-small", "model": "qwen3.8-27b-q3", "ctx": 81920, "kinds": map[string]int{"ok": 0, "cancelled": 1}}}},
	}}
	raw, _ := json.Marshal(st)
	os.WriteFile(filepath.Join(dir, gateway.FailuresFile), raw, 0o600)
	cfgFile := filepath.Join(dir, "gateway.json")
	cfg, _ := json.Marshal(gateway.Config{StateDir: dir})
	os.WriteFile(cfgFile, cfg, 0o600)
	run := func(args ...string) (string, error) {
		var out, msg bytes.Buffer
		err := runAccounts(append([]string{"-config", cfgFile, "node-failures"}, args...), &out, &msg)
		return out.String(), err
	}
	out, err := run()
	if err != nil {
		t.Fatal(err)
	}
	line := func(prefix string) []string {
		t.Helper()
		for _, l := range strings.Split(out, "\n") {
			if strings.HasPrefix(l, prefix) {
				return strings.Fields(l)
			}
		}
		t.Fatalf("no line %q in:\n%s", prefix, out)
		return nil
	}
	// n-small: 11 attempts (bad_tool_calls marks an answer), 6 answers, 2 cut at the context, 2 context
	// exceeded, 1 parse error, 1 bad call, 1 other failure, 1 cancelled.
	small := strings.Join(line("n-small "), " ")
	if !strings.Contains(small, "n-small qwen3.8-27b-q3 81920 11 6 0 2 (18.2%) 2 (18.2%) 1 (9.1%) 1 (9.1%) 1 (9.1%) 1/0/0/0") {
		t.Fatalf("n-small: %s\n%s", small, out)
	}
	if f := line("n-big "); f[3] != "10" || f[4] != "10" || f[5] != "1" {
		t.Fatalf("n-big: %v", f)
	}
	if strings.ContainsAny(out, "\x1b") || strings.Contains(out, "\nNODE forged") || !strings.Contains(out, "n-?[2Jevil") {
		t.Fatalf("control characters reach the terminal:\n%q", out)
	}
	if strings.Contains(out, "n-old") {
		t.Fatalf("a day older than 7 is counted:\n%s", out)
	}
	perModel := false
	for _, l := range strings.Split(out, "\n") {
		// summed over n-small and n-small2: 11 + 4 attempts, 6 + 3 answers, one blocked
		if f := strings.Fields(l); len(f) > 4 && f[0] == "all" && f[1] == "qwen3.8-27b-q3" && f[3] == "15" && f[4] == "9" &&
			f[len(f)-1] == "1/0/1/0" {
			perModel = true
		}
	}
	if !perModel {
		t.Fatalf("no per-model row of the 16 GB tier:\n%s", out)
	}
	if f := line("node stalled"); f[2] != "qwen3.8-27b-q3" || f[3] != "1" {
		t.Fatalf("category: %v", f)
	}
	if f := line("too_long"); f[1] != "qwen3.8-27b-q3" || f[2] != "3" {
		t.Fatalf("refusal: %v", f)
	}

	// Thirty days take the old day in; one node only.
	if out, err = run("30", "n-old"); err != nil || !strings.Contains(out, "n-old") || strings.Contains(out, "n-small") {
		t.Fatalf("%v\n%s", err, out)
	}
	if _, err := run("0"); err == nil {
		t.Fatal("0 days accepted")
	}
	os.Remove(filepath.Join(dir, gateway.FailuresFile))
	if _, err := run(); err == nil || !strings.Contains(err.Error(), "no failure counts yet") {
		t.Fatalf("no file: %v", err)
	}
}
