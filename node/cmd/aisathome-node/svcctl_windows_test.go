// SPDX-License-Identifier: Apache-2.0
package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/windows"

	"github.com/aisathome/aisathome/node"
)

// testServiceSID stands in for the virtual account (no service is installed by this test),
// testUserSID for the UI user. Neither needs to exist, and neither has an SDDL alias.
const (
	testServiceSID = "S-1-5-80-3-1-4-1-5"
	testUserSID    = "S-1-5-21-1-2-3-1001"
)

func elevated(t *testing.T) {
	t.Helper()
	if !windows.GetCurrentProcessToken().IsElevated() {
		t.Skip("needs an elevated administrator (the GitHub runner is one)")
	}
}

func sddlOf(t *testing.T, path string) string {
	t.Helper()
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT,
		windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatal(err)
	}
	return sd.String()
}

func testLayout(t *testing.T) node.Layout {
	base := t.TempDir()
	return node.WindowsLayout(func(k string) string {
		if k == "ProgramData" {
			return base
		}
		return ""
	})
}

// trusted is what serviceInstall trusts as owner: the service and the account running it
// (here the test, which made the temporary folders).
func trusted(t *testing.T) []string { return []string{testServiceSID, currentUser(t)} }

func currentUser(t *testing.T) string {
	t.Helper()
	u, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	return u.User.Sid.String()
}

// The data folder: owned by Administrators, nothing inherited, entries for SYSTEM,
// Administrators and the service only; a config file made earlier with its own entry for
// Everyone loses it; the password file is new, usable and readable by the UI user.
func TestPrepareDataDir(t *testing.T) {
	elevated(t)
	l := testLayout(t)
	dir := filepath.Dir(l.Config)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(l.Config, []byte(`{"token":"secret"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := applySDDL(l.Config, "D:(A;;FR;;;WD)", windows.DACL_SECURITY_INFORMATION|windows.UNPROTECTED_DACL_SECURITY_INFORMATION); err != nil {
		t.Fatal(err)
	}
	user := testUserSID
	if err := prepareDataDir(l, trusted(t), testServiceSID, user); err != nil {
		t.Fatal(err)
	}
	d := sddlOf(t, dir)
	if !strings.HasPrefix(d, "O:BAD:P") {
		t.Errorf("folder %s: want owner BA and a protected DACL", d)
	}
	for _, want := range []string{";;;SY)", ";;;BA)", ";;;" + testServiceSID + ")"} {
		if !strings.Contains(d, want) {
			t.Errorf("folder %s: no entry %s", d, want)
		}
	}
	if n := strings.Count(d, "(A;"); n != 3 {
		t.Errorf("folder %s: %d entries, want 3", d, n)
	}
	c := sddlOf(t, l.Config)
	if strings.Contains(c, ";;;WD)") || !strings.Contains(c, ";;;"+testServiceSID+")") || strings.Contains(c, ";;;"+user+")") {
		t.Errorf("config %s: want only the folder's entries (no Everyone, no user)", c)
	}
	pw, err := os.ReadFile(l.PasswordFile)
	if err != nil || !usablePassword(pw) {
		t.Fatalf("password file: %q, %v", pw, err)
	}
	if p := sddlOf(t, l.PasswordFile); !strings.Contains(p, "(A;;FR;;;"+user+")") || !strings.Contains(p, ";;;"+testServiceSID+")") {
		t.Errorf("password file %s: want read for %s on top of the folder's entries", p, user)
	}

	// Again (an update): the password stays.
	if err := prepareDataDir(l, trusted(t), testServiceSID, ""); err != nil {
		t.Fatal(err)
	}
	if again, _ := os.ReadFile(l.PasswordFile); string(again) != string(pw) {
		t.Errorf("an update changed the password")
	}
	if p := sddlOf(t, l.PasswordFile); strings.Contains(p, ";;;"+user+")") {
		t.Errorf("password file %s: no UI user given, yet %s keeps read access", p, user)
	}
}

// A too-short password file is replaced; a folder owned by an ordinary account is refused.
func TestPrepareDataDirRefuses(t *testing.T) {
	elevated(t)
	l := testLayout(t)
	dir := filepath.Dir(l.Config)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(l.PasswordFile, []byte("short\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := prepareDataDir(l, trusted(t), testServiceSID, ""); err != nil {
		t.Fatal(err)
	}
	if pw, _ := os.ReadFile(l.PasswordFile); !usablePassword(pw) {
		t.Errorf("short password kept: %q", pw)
	}

	l2 := testLayout(t)
	dir2 := filepath.Dir(l2.Config)
	if err := os.MkdirAll(dir2, 0o700); err != nil {
		t.Fatal(err)
	}
	// The user's own SID as owner, as when an ordinary user made the folder first.
	if err := applySDDL(dir2, "O:"+currentUser(t)+"D:(A;OICI;FA;;;WD)", windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION); err != nil {
		t.Fatal(err)
	}
	// Only the service is trusted here, so the test's own account stands in for an ordinary
	// user who made the folder first.
	err := prepareDataDir(l2, []string{testServiceSID}, testServiceSID, "")
	if err == nil || !strings.Contains(err.Error(), "belongs to") {
		t.Fatalf("a folder owned by a user: err %v, want a refusal", err)
	}
	if _, err := os.Stat(l2.PasswordFile); err == nil {
		t.Error("a password was written into a refused folder")
	}
}
