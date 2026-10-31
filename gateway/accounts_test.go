// SPDX-License-Identifier: Apache-2.0
package gateway

import (
	"crypto/sha256"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func addAccount(s *AccountStore, id string) error {
	return s.Update(func(f *AccountsFile) error {
		f.Accounts = append(f.Accounts, &Account{ID: id})
		return nil
	})
}

// An update writes the file whole and private, whatever its mode was, and leaves nothing beside it;
// the hash it applies is the file's, so the next Refresh has nothing to do.
func TestAccountUpdateWritesPrivately(t *testing.T) {
	file := filepath.Join(t.TempDir(), "accounts.json")
	s, err := OpenAccountStore(file)
	if err != nil {
		t.Fatal(err)
	}
	if err := addAccount(s, "gh-1"); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(file, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := addAccount(s, "gh-2"); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(file)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("mode %o", st.Mode().Perm())
	}
	raw, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(string(raw), "}\n") {
		t.Fatalf("file ends %q", raw[len(raw)-3:])
	}
	if s.sum != sha256.Sum256(raw) {
		t.Fatal("the applied hash is not the file's")
	}
	ents, _ := filepath.Glob(file + "*")
	hidden, _ := filepath.Glob(filepath.Join(filepath.Dir(file), ".*"))
	for _, f := range append(ents, hidden...) {
		if f != file && f != file+".lock" {
			t.Fatalf("left behind: %s", f)
		}
	}
}

// A fixed temporary name can be in the way (left by a dead process, or owned by another user); the
// update must not depend on it.
func TestAccountUpdateIgnoresAStaleTemporary(t *testing.T) {
	file := filepath.Join(t.TempDir(), "accounts.json")
	s, err := OpenAccountStore(file)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(file+".tmp", 0o700); err != nil {
		t.Fatal(err)
	}
	if err := addAccount(s, "gh-1"); err != nil {
		t.Fatal(err)
	}
	if _, _, ok := s.Get("gh-1"); !ok {
		t.Fatal("the account is not in")
	}
}

// A write that fails changes neither the file nor what the gateway holds, and leaves nothing behind.
func TestAccountUpdateFailedWrite(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory modes")
	}
	dir := t.TempDir()
	file := filepath.Join(dir, "accounts.json")
	s, err := OpenAccountStore(file)
	if err != nil {
		t.Fatal(err)
	}
	if err := addAccount(s, "gh-1"); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(file)
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0o700) })
	if err := addAccount(s, "gh-2"); !errors.Is(err, os.ErrPermission) {
		t.Fatalf("update on a read-only directory: %v", err)
	}
	if _, _, ok := s.Get("gh-2"); ok {
		t.Fatal("memory ahead of the file")
	}
	after, _ := os.ReadFile(file)
	if string(after) != string(before) {
		t.Fatal("the file changed")
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := addAccount(s, "gh-2"); err != nil {
		t.Fatal(err)
	}
}
