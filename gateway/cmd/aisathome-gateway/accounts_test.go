// SPDX-License-Identifier: Apache-2.0
package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aisathome/aisathome/gateway"
)

func TestAccountsCommand(t *testing.T) {
	file := filepath.Join(t.TempDir(), "accounts.json")
	store, _ := gateway.OpenAccountStore(file)
	for _, id := range []string{"gh:1", "gh:2", "gh:3"} {
		store.SignIn(id, time.Now())
	}
	run := func(args ...string) string {
		var out, msg bytes.Buffer
		if err := runAccounts(append([]string{"-file", file}, args...), &out, &msg); err != nil {
			t.Fatalf("%v: %v %s", args, err, msg.String())
		}
		return out.String()
	}
	run("add", "test:e2e-1")
	run("limit", "1") // already active accounts stay active
	run("ban", "gh:2")
	run("close")
	out := run("list")
	for _, want := range []string{"test:e2e-1 active", "gh:2  banned", "of 1 active", "sign-up is closed"} {
		if !strings.Contains(strings.Join(strings.Fields(out), " "), strings.Join(strings.Fields(want), " ")) {
			t.Fatalf("list lacks %q:\n%s", want, out)
		}
	}
	var msg bytes.Buffer
	if err := runAccounts([]string{"-file", file, "limit", "x"}, &bytes.Buffer{}, &msg); err == nil {
		t.Fatal("limit x accepted")
	}
}

// signup goes through the same path as a first sign-in: the waitlist when the network is
// full, active when there is room; only test ids.
func TestAccountsSignup(t *testing.T) {
	file := filepath.Join(t.TempDir(), "accounts.json")
	run := func(args ...string) (string, error) {
		var out, msg bytes.Buffer
		err := runAccounts(append([]string{"-file", file}, args...), &out, &msg)
		return strings.TrimSpace(out.String()), err
	}
	run("limit", "1")
	if s, err := run("signup", "test:a"); err != nil || s != "active" {
		t.Fatalf("first signup: %q %v", s, err)
	}
	if s, err := run("signup", "test:b"); err != nil || s != "waitlisted" {
		t.Fatalf("signup over the limit: %q %v", s, err)
	}
	run("limit", "2")
	if out, _ := run("list"); !strings.Contains(strings.Join(strings.Fields(out), " "), "test:b active") {
		t.Fatalf("raising the limit did not let test:b in:\n%s", out)
	}
	if _, err := run("signup", "gh:99"); err == nil {
		t.Fatal("signup accepted a real provider id")
	}
}

func TestAccountsNodeCommands(t *testing.T) {
	file := filepath.Join(t.TempDir(), "accounts.json")
	store, _ := gateway.OpenAccountStore(file)
	store.SignIn("gh:1", time.Now())
	_, id, err := store.AddNode("gh:1", "rig", time.Now(), nil)
	if err != nil {
		t.Fatal(err)
	}
	store.HoldNode(gateway.NodeHold{Node: id, Operator: "acct-x", Rule: "download-and-execute", At: time.Now()})
	run := func(args ...string) string {
		var out, msg bytes.Buffer
		if err := runAccounts(append([]string{"-file", file}, args...), &out, &msg); err != nil {
			t.Fatalf("%v: %v %s", args, err, msg.String())
		}
		return strings.Join(strings.Fields(out.String()), " ")
	}
	run("nodes", "close")
	run("nodes", "limit", "5")
	run("routing", "trusted-only")
	if out := run("nodes"); !strings.Contains(out, "up to 10 (default) per account") {
		t.Fatalf("nodes lacks the default per-account limit:\n%s", out)
	}
	run("nodes", "per-account", "2")
	out := run("nodes")
	for _, want := range []string{id + " gh:1 rig", "download-and-execute", "1 of 5 node tokens", "up to 2 per account", "new nodes are closed", "routing trusted-only"} {
		if !strings.Contains(out, want) {
			t.Fatalf("nodes lacks %q:\n%s", want, out)
		}
	}
	store.Refresh()
	if store.Settings().MaxNodesPerAccountOrDefault() != 2 {
		t.Fatal("nodes per-account 2 was not saved")
	}
	run("nodes", "per-account", "default")
	if out := run("nodes"); !strings.Contains(out, "up to 10 (default) per account") {
		t.Fatalf("per-account default did not reset the limit:\n%s", out)
	}
	run("node-clear", id)
	run("routing", "any")
	run("nodes", "open")
	if out := run("nodes"); strings.Contains(out, "download-and-execute") || !strings.Contains(out, "routing any") || !strings.Contains(out, "are open") {
		t.Fatalf("after clear/any/open:\n%s", out)
	}
	run("node-revoke", id)
	if out := run("nodes"); strings.Contains(out, id) {
		t.Fatalf("revoked node still listed:\n%s", out)
	}
	for _, bad := range [][]string{{"routing", "maybe"}, {"nodes", "limit", "0"}, {"nodes", "per-account", "0"},
		{"nodes", "per-account", "x"}, {"nodes", "per-account"}, {"node-clear", id}} {
		if err := runAccounts(append([]string{"-file", file}, bad...), &bytes.Buffer{}, &bytes.Buffer{}); err == nil {
			t.Fatalf("%v accepted", bad)
		}
	}
	// ban deletes the account's node tokens
	store.Refresh()
	store.AddNode("gh:1", "", time.Now(), nil)
	run("ban", "gh:1")
	if out := run("nodes"); !strings.Contains(out, "0 of 5") {
		t.Fatalf("ban kept node tokens:\n%s", out)
	}
}

func TestUsernameClearCommand(t *testing.T) {
	file := filepath.Join(t.TempDir(), "accounts.json")
	store, _ := gateway.OpenAccountStore(file)
	store.SignIn("gh:1", time.Now())
	if err := store.SetUsername("gh:1", "Crunch Queen"); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	runAccounts([]string{"-file", file, "list"}, &out, &bytes.Buffer{})
	if !strings.Contains(out.String(), `"Crunch Queen"`) {
		t.Fatalf("list lacks the username:\n%s", out.String())
	}
	if err := runAccounts([]string{"-file", file, "username-clear", "gh:1"}, &bytes.Buffer{}, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	store.Refresh()
	if a, _, _ := store.Get("gh:1"); a.Username != "" {
		t.Fatalf("username kept: %q", a.Username)
	}
	if err := runAccounts([]string{"-file", file, "username-clear", "gh:9"}, &bytes.Buffer{}, &bytes.Buffer{}); err == nil {
		t.Fatal("clearing an unknown account succeeded")
	}
}

// budgetsFixture writes a gateway budget state file: weighted tokens per account and UTC day,
// keyed by the hash of the account's budget key, as the running gateway saves it.
func budgetsFixture(t *testing.T, dir string, use map[string][2]any) {
	t.Helper()
	used := map[string]any{}
	for id, u := range use {
		h := sha256.Sum256([]byte("acct:" + id))
		used[hex.EncodeToString(h[:])] = map[string]any{"day": u[0], "n": u[1]}
	}
	h := sha256.Sum256([]byte("ak-some-plain-key")) // a key's own budget, not an account's
	used[hex.EncodeToString(h[:])] = map[string]any{"day": "2026-10-28", "n": 999}
	raw, _ := json.Marshal(map[string]any{"version": 1, "used": used})
	if err := os.WriteFile(filepath.Join(dir, gateway.BudgetsFile), raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

// list shows the limits every account has on its own, and each account's weighted tokens of
// the current UTC day from the gateway's saved budget state (the account page's number).
func TestAccountsListLimitsAndUsedToday(t *testing.T) {
	dir := t.TempDir()
	file, state := filepath.Join(dir, "accounts.json"), filepath.Join(dir, "state")
	os.Mkdir(state, 0o700)
	store, _ := gateway.OpenAccountStore(file)
	for _, id := range []string{"gh:1", "gh:2", "gh:3"} {
		store.SignIn(id, time.Now())
	}
	cfg := gateway.Config{StateDir: state, Accounts: &gateway.AccountsConfig{File: file,
		Limits: gateway.UserLimits{TokensPerDay: 5000000, RPM: 30, MaxConcurrent: 2}}}
	cfgFile := filepath.Join(dir, "gateway.json")
	raw, _ := json.Marshal(cfg)
	os.WriteFile(cfgFile, raw, 0o600)
	budgetsFixture(t, state, map[string][2]any{
		"gh:1": {"2026-10-28", 1234567}, // today
		"gh:2": {"2026-10-27", 4000},    // yesterday: counts nothing today
	})
	list := func(now time.Time) map[string]string {
		var out bytes.Buffer
		if err := listAccounts(&out, store.Snapshot(), &cfg, now); err != nil {
			t.Fatal(err)
		}
		rows := map[string]string{}
		for _, line := range strings.Split(out.String(), "\n") {
			if f := strings.Fields(line); len(f) > 3 && strings.HasPrefix(f[0], "gh:") {
				rows[f[0]] = f[3] // ID STATE KEYS USED-TODAY ...
			}
		}
		rows["out"] = out.String()
		return rows
	}
	// Just after midnight UTC, on a clock two hours ahead: the UTC day counts, not the local one.
	r := list(time.Date(2026, 10, 28, 2, 0, 30, 0, time.FixedZone("CEST", 2*3600)))
	if r["gh:1"] != "1,234,567" || r["gh:2"] != "0" || r["gh:3"] != "0" {
		t.Fatalf("used today after midnight UTC: gh:1 %q gh:2 %q gh:3 %q\n%s", r["gh:1"], r["gh:2"], r["gh:3"], r["out"])
	}
	// One second before midnight UTC (still 27 October), on a clock five hours behind.
	r = list(time.Date(2026, 10, 27, 18, 59, 59, 0, time.FixedZone("EST", -5*3600)))
	if r["gh:1"] != "0" || r["gh:2"] != "4,000" {
		t.Fatalf("used today before midnight UTC: gh:1 %q gh:2 %q\n%s", r["gh:1"], r["gh:2"], r["out"])
	}
	// Through the command, with -config: the header, the limits line and the column's meaning.
	var out bytes.Buffer
	if err := runAccounts([]string{"-config", cfgFile, "list"}, &out, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	flat := strings.Join(strings.Fields(out.String()), " ")
	for _, want := range []string{"ID STATE KEYS USED TODAY CREATED USERNAME",
		"limits per account, each account separately: 5,000,000 tokens/day (weighted), 30 requests/min, 2 concurrent; the day resets 00:00 UTC",
		"USED TODAY: weighted tokens since 00:00 UTC, chat and API together"} {
		if !strings.Contains(flat, want) {
			t.Fatalf("list lacks %q:\n%s", want, out.String())
		}
	}
	// -file alone: no config, so neither limits nor use (unchanged output).
	out.Reset()
	runAccounts([]string{"-file", file, "list"}, &out, &bytes.Buffer{})
	if strings.Contains(out.String(), "USED TODAY") || strings.Contains(out.String(), "limits per account") {
		t.Fatalf("-file alone shows limits or use:\n%s", out.String())
	}
	// No saved state yet: everyone at 0. A corrupt state file is an error, not zeros.
	os.Remove(filepath.Join(state, gateway.BudgetsFile))
	if r := list(time.Now()); r["gh:1"] != "0" {
		t.Fatalf("without a state file: %q\n%s", r["gh:1"], r["out"])
	}
	os.WriteFile(filepath.Join(state, gateway.BudgetsFile), []byte(`{"used": {`), 0o600)
	if err := listAccounts(&bytes.Buffer{}, store.Snapshot(), &cfg, time.Now()); err == nil {
		t.Fatal("a corrupt state file listed as zeros")
	}
	// Without a state_dir the use is unknown and the list says so.
	noState := cfg
	noState.StateDir = ""
	out.Reset()
	listAccounts(&out, store.Snapshot(), &noState, time.Now())
	if strings.Contains(out.String(), "USED TODAY") || !strings.Contains(out.String(), "no state_dir") {
		t.Fatalf("without state_dir:\n%s", out.String())
	}
}

func TestLimitsLineAndThousands(t *testing.T) {
	if got := limitsLine(gateway.UserLimits{}); got != "limits per account, each account separately: unlimited tokens/day (weighted), "+
		"unlimited requests/min, unlimited concurrent; the day resets 00:00 UTC" {
		t.Fatalf("all unlimited: %q", got)
	}
	for n, want := range map[int64]string{0: "0", 999: "999", 1000: "1,000", 123456: "123,456", 1234567: "1,234,567", -4000: "-4,000"} {
		if got := thousands(n); got != want {
			t.Fatalf("thousands(%d) = %q, want %q", n, got, want)
		}
	}
}
