// SPDX-License-Identifier: Apache-2.0
package main

import (
	"bytes"
	"runtime"
	"strings"
	"testing"
)

func TestServiceCommandUsage(t *testing.T) {
	for _, args := range [][]string{nil, {"bogus"}, {"install", "extra"}, {"uninstall", "-start"}, {"install", "-nope"}} {
		var out, errw bytes.Buffer
		if code := runServiceCommand(args, &out, &errw); code != 2 {
			t.Errorf("%q: exit %d, want 2 (stderr %q)", args, code, errw.String())
		}
		if !strings.Contains(errw.String(), "usage: aisathome-node service") && !strings.Contains(errw.String(), "unexpected arguments") {
			t.Errorf("%q: no usage on stderr: %q", args, errw.String())
		}
	}
}

func TestServiceCommandOnlyOnWindows(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the real commands need the Service Control Manager; the Windows CI job runs them")
	}
	for _, cmd := range []string{"install", "uninstall", "stop"} {
		var out, errw bytes.Buffer
		if code := runServiceCommand([]string{cmd}, &out, &errw); code != 1 || !strings.Contains(errw.String(), "only on Windows") {
			t.Errorf("%s: exit %d, stderr %q; want 1 and \"only on Windows\"", cmd, code, errw.String())
		}
	}
}

func TestDataDirSDDL(t *testing.T) {
	got := dataDirSDDL("S-1-5-80-1-2-3-4-5")
	// Owned by Administrators, protected (P: nothing inherited from %ProgramData%), and exactly
	// three entries, all inherited by files and folders inside (OICI).
	want := "O:BAD:P(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)(A;OICI;0x1301bf;;;S-1-5-80-1-2-3-4-5)"
	if got != want {
		t.Fatalf("dataDirSDDL = %s, want %s", got, want)
	}
	if got := passwordFileSDDL("S-1-5-21-1-2-3-1001"); got != "D:(A;;FR;;;S-1-5-21-1-2-3-1001)" {
		t.Errorf("passwordFileSDDL = %s", got)
	}
	if got := passwordFileSDDL(""); got != inheritOnlySDDL {
		t.Errorf("passwordFileSDDL(\"\") = %s, want only inherited entries", got)
	}
}

func TestTrustedOwner(t *testing.T) {
	svc := "S-1-5-80-1-2-3-4-5"
	for sid, want := range map[string]bool{
		sidSystem: true, sidAdministrators: true, sidTrustedInstall: true, svc: true,
		"S-1-5-21-1-2-3-1001": false, // a user, who may create folders under %ProgramData%
		"S-1-5-32-545":        false, // Users
		"S-1-1-0":             false, // Everyone
		"":                    false,
	} {
		if got := trustedOwner(sid, svc, ""); got != want {
			t.Errorf("trustedOwner(%q) = %v, want %v", sid, got, want)
		}
	}
	if trustedOwner("", "", "") {
		t.Error("empty SIDs must not trust an empty owner")
	}
	if !trustedOwner("S-1-5-21-1-2-3-500", svc, "S-1-5-21-1-2-3-500") {
		t.Error("the administrator running the install is not trusted")
	}
}

func TestUIUserFromToken(t *testing.T) {
	for sid, want := range map[string]string{
		sidSystem: "", sidLocalService: "", sidNetworkService: "",
		"S-1-5-21-1-2-3-1001": "S-1-5-21-1-2-3-1001",
	} {
		if got := uiUserFromToken(sid); got != want {
			t.Errorf("uiUserFromToken(%q) = %q, want %q", sid, got, want)
		}
	}
}

func TestNewPassword(t *testing.T) {
	a, err := newPassword()
	if err != nil {
		t.Fatal(err)
	}
	b, _ := newPassword()
	if len(a) != 32 || a == b || !usablePassword([]byte(a)) {
		t.Fatalf("newPassword: %q, %q", a, b)
	}
	for in, want := range map[string]bool{"": false, "short\n": false, "  eleven-char  \n": false, "twelve-chars\r\n": true} {
		if got := usablePassword([]byte(in)); got != want {
			t.Errorf("usablePassword(%q) = %v, want %v", in, got, want)
		}
	}
}
