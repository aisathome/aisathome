// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// project-key writes the key to a new file only (mode 600) and prints its hash, never the key.
func TestProjectKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tauceti.key")
	var out bytes.Buffer
	if err := runProjectKey([]string{path}, &out); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	key := strings.TrimSpace(string(raw))
	if !strings.HasPrefix(key, "ak-") || len(key) != 46 {
		t.Fatalf("key form: %d characters", len(key))
	}
	sum := sha256.Sum256([]byte(key))
	if got := strings.TrimSpace(out.String()); got != hex.EncodeToString(sum[:]) {
		t.Fatalf("printed %q, want the key's SHA-256", got)
	}
	if strings.Contains(out.String(), key) {
		t.Fatal("the key was printed")
	}
	if st, _ := os.Stat(path); st.Mode().Perm() != 0o600 {
		t.Fatalf("mode %o, want 600", st.Mode().Perm())
	}
	if err := runProjectKey([]string{path}, &out); err == nil {
		t.Fatal("an existing key file was overwritten")
	}
	if raw2, _ := os.ReadFile(path); !bytes.Equal(raw, raw2) {
		t.Fatal("the existing key changed")
	}
}
