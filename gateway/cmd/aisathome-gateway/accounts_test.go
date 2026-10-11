// SPDX-License-Identifier: Apache-2.0
package main

import (
	"bytes"
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
	out := run("nodes")
	for _, want := range []string{id + " gh:1 rig", "download-and-execute", "1 of 5 node tokens", "new nodes are closed", "routing trusted-only"} {
		if !strings.Contains(out, want) {
			t.Fatalf("nodes lacks %q:\n%s", want, out)
		}
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
	for _, bad := range [][]string{{"routing", "maybe"}, {"nodes", "limit", "0"}, {"node-clear", id}} {
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
