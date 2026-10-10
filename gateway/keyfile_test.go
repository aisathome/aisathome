// SPDX-License-Identifier: Apache-2.0

package gateway

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestKeysFileStoresOnlyHashes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "keys.json")
	var f KeysFile
	key, err := f.Add(KeyInfo{User: "bob", RPM: 5}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(key, "ak-") || len(key) < 40 {
		t.Fatalf("unexpected key form %q", key)
	}
	if err := f.Save(path); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(path)
	if strings.Contains(string(raw), key[keyPrefixLen:]) {
		t.Fatal("keys file holds the key itself")
	}
	g, err := LoadKeysFile(path)
	if err != nil || len(g.Keys) != 1 || g.Keys[0].User != "bob" || g.Keys[0].RPM != 5 {
		t.Fatalf("round trip: %+v %v", g, err)
	}
	ks := &configKeys{hashed: map[string]KeyInfo{g.Keys[0].Hash: g.Keys[0].KeyInfo}}
	if k, ok := ks.Lookup(context.Background(), key); !ok || k.User != "bob" {
		t.Fatal("key not found by hash")
	}
	if _, ok := ks.Lookup(context.Background(), g.Keys[0].Hash); ok {
		t.Fatal("the stored hash must not work as a key")
	}
}

func TestKeysFileRevoke(t *testing.T) {
	var f KeysFile
	a, _ := f.Add(KeyInfo{User: "a"}, time.Now())
	b, _ := f.Add(KeyInfo{User: "b"}, time.Now())
	if r, err := f.Revoke(a[:keyPrefixLen]); err != nil || r.User != "a" {
		t.Fatalf("revoke by prefix: %v %v", r, err)
	}
	if r, err := f.Revoke(b); err != nil || r.User != "b" { // a leaked key, pasted in full
		t.Fatalf("revoke by key: %v %v", r, err)
	}
	if _, err := f.Revoke("ak-"); err == nil || len(f.Keys) != 0 {
		t.Fatal("revoking an unknown prefix must fail")
	}
}

func TestKeysFileRejectsBadEntries(t *testing.T) {
	for _, raw := range []string{`{"keys": [`, `{"keys": [{"sha256": "abc", "user": "x"}]}`,
		`{"keys": [{"sha256": "` + strings.Repeat("a", 64) + `"}]}`} {
		if _, err := parseKeysFile([]byte(raw)); err == nil {
			t.Fatalf("accepted %s", raw)
		}
	}
}
