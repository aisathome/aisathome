// SPDX-License-Identifier: Apache-2.0
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aisathome/aisathome/gateway"
)

// node-checks sums the usage log per node over the last days, one node if asked; it needs the
// config's usage_log and no accounts file.
func TestAccountsNodeChecks(t *testing.T) {
	dir := t.TempDir()
	log := filepath.Join(dir, "usage.jsonl")
	now := time.Now().UTC()
	at := func(d time.Duration) string { return now.Add(-d).Format(time.RFC3339Nano) }
	lines := []string{
		fmt.Sprintf(`{"time":%q,"node":"n-bad","operator":"op","outcome":"ok","credit":0,"voided":250,"checks":["speed"],"task":"t1","signals":{"tps":3000,"finish":"stop","tool_calls":2,"bad_calls":1,"degenerate":true}}`, at(time.Hour)),
		fmt.Sprintf(`{"time":%q,"node":"n-bad","outcome":"ok","credit":10,"signals":{"tps":40,"finish":"length"}}`, at(2*time.Hour)),
		fmt.Sprintf(`{"time":%q,"node":"n-good","outcome":"ok","credit":20,"signals":{"tps":42},"discarded":[{"req_id":"r1","node":"n-bad"}]}`, at(time.Hour)),
		fmt.Sprintf(`{"time":%q,"node":"n-good","outcome":"failed"}`, at(time.Hour)),
		fmt.Sprintf(`{"time":%q,"node":"n-old","outcome":"ok","credit":5}`, at(10*24*time.Hour)),
		`not json`,
	}
	os.WriteFile(log, []byte(strings.Join(lines, "\n")+"\n"), 0o600)
	cfgFile := filepath.Join(dir, "gateway.json")
	raw, _ := json.Marshal(gateway.Config{UsageLog: log})
	os.WriteFile(cfgFile, raw, 0o600)
	run := func(args ...string) (string, error) {
		var out, msg bytes.Buffer
		err := runAccounts(append([]string{"-config", cfgFile, "node-checks"}, args...), &out, &msg)
		return out.String(), err
	}
	out, err := run()
	if err != nil {
		t.Fatal(err)
	}
	bad, good := -1, -1
	for i, l := range strings.Split(out, "\n") {
		f := strings.Fields(l)
		if len(f) == 0 {
			continue
		}
		switch f[0] {
		case "n-bad":
			bad = i
			for _, want := range []string{"2", "1 (50.0%)", "speed 1", "250.0", "10.0", "40.0/3000.0/3000.0", "1 (50.0%)", "1 (50.0%)"} {
				if !strings.Contains(l, want) {
					t.Errorf("n-bad lacks %q: %s", want, l)
				}
			}
		case "n-good":
			good = i
		case "n-old":
			t.Errorf("a record older than 7 days counted: %s", l)
		}
	}
	if bad < 0 || good < 0 || bad > good {
		t.Fatalf("want n-bad (failed checks) before n-good:\n%s", out)
	}
	if out, _ := run("30", "n-old"); !strings.Contains(out, "n-old") || strings.Contains(out, "n-bad") {
		t.Fatalf("30 days, one node:\n%s", out)
	}
	if _, err := run("0"); err == nil {
		t.Fatal("0 days accepted")
	}
	raw, _ = json.Marshal(gateway.Config{})
	os.WriteFile(cfgFile, raw, 0o600)
	if _, err := run(); err == nil || !strings.Contains(err.Error(), "usage_log") {
		t.Fatalf("no usage_log: %v", err)
	}
}

// accounts nodes lists held accounts; account-clear lifts a hold and refuses an account that has none.
func TestAccountsAccountClear(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "accounts.json")
	store, err := gateway.OpenAccountStore(file)
	if err != nil {
		t.Fatal(err)
	}
	store.HoldAccount(gateway.AccountHold{Account: "gh:5", Rule: "answer-checks", Reason: "2 of its nodes were held by the answer checks",
		At: time.Date(2026, 11, 20, 9, 30, 0, 0, time.UTC)})
	run := func(args ...string) (string, error) {
		var out, msg bytes.Buffer
		err := runAccounts(append([]string{"-file", file}, args...), &out, &msg)
		return out.String(), err
	}
	out, err := run("nodes")
	if err != nil || !strings.Contains(out, "gh:5  answer-checks since 2026-11-20 09:30 UTC: 2 of its nodes were held by the answer checks") {
		t.Fatalf("nodes: %v\n%s", err, out)
	}
	if _, err := run("account-clear", "gh:5"); err != nil {
		t.Fatal(err)
	}
	if out, _ := run("nodes"); strings.Contains(out, "gh:5") {
		t.Fatalf("still listed:\n%s", out)
	}
	if _, err := run("account-clear", "gh:5"); err == nil {
		t.Fatal("cleared an account that is not held")
	}
}
