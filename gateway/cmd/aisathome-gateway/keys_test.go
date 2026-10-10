// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestKeysCommand(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "gateway.json")
	os.WriteFile(cfg, []byte(`{"keys_file": "`+filepath.Join(dir, "keys.json")+`"}`), 0o600)
	run := func(args ...string) (string, error) {
		var out, msg bytes.Buffer
		err := runKeys(args, &out, &msg)
		return out.String(), err
	}
	key, err := run("add", "bob", "-config", cfg, "--tokens-per-day", "1000", "-rpm", "7")
	key = strings.TrimSpace(key)
	if err != nil || !strings.HasPrefix(key, "ak-") {
		t.Fatalf("add: %q %v", key, err)
	}
	list, err := run("list", "-config", cfg)
	if err != nil || strings.Contains(list, key) || !strings.Contains(list, key[:11]) ||
		!strings.Contains(list, "1000") || !strings.Contains(list, "trusted") {
		t.Fatalf("list must show the prefix and limits, never the key: %q %v", list, err)
	}
	raw, _ := os.ReadFile(filepath.Join(dir, "keys.json"))
	if strings.Contains(string(raw), `"restore_secrets"`) {
		t.Fatal("new keys must not restore secrets")
	}
	if _, err := run("revoke", key[:11], "-config", cfg); err != nil {
		t.Fatal(err)
	}
	if list, _ := run("list", "-config", cfg); strings.Contains(list, key[:11]) {
		t.Fatalf("revoked key still listed: %q", list)
	}
	if _, err := run("add", "bob", "-min-trust", "maybe", "-config", cfg); err == nil {
		t.Fatal("invalid -min-trust accepted")
	}
	if _, err := run("list", "-config", filepath.Join(dir, "missing.json")); err == nil {
		t.Fatal("a missing config must be an error")
	}
}
