//go:build windows

// SPDX-License-Identifier: Apache-2.0
package localui

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

func testPipeName(t *testing.T) string {
	return fmt.Sprintf(`\\.\pipe\aisathome-node-test-%d-%d`, os.Getpid(), time.Now().UnixNano())
}

// The pipe end to end, as the tray uses it: this test runs as an administrator (the CI
// runner's account), so the agent hands out a link, and the link signs in once.
func TestPipeLoginLink(t *testing.T) {
	pw := filepath.Join(t.TempDir(), "ui-password")
	if err := os.WriteFile(pw, []byte(testPassword+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ui := New(&fakeAgent{connected: true}, Options{PasswordFile: pw, NvidiaSMI: fakeSMI(t, "4090")})
	name := testPipeName(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- ui.servePipe(ctx, name) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("servePipe: %v", err)
		}
	})
	var link string
	var err error
	for i := 0; i < 50; i++ { // until the pipe exists
		if link, err = AskLoginLink(name, DefaultPort, 2*time.Second); err == nil || !strings.Contains(err.Error(), "not running") {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("AskLoginLink: %v", err)
	}
	// Twice more: the server serves one client after another.
	for i := 0; i < 2; i++ {
		if _, err := AskLoginLink(name, DefaultPort, 2*time.Second); err != nil {
			t.Fatalf("second client: %v", err)
		}
	}
	srv := httptest.NewServer(ui.Handler())
	t.Cleanup(srv.Close)
	u, _ := url.Parse(link)
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Get(srv.URL + u.RequestURI())
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if sessionCookie(resp) == nil {
		t.Fatalf("the pipe's link did not sign in: %d", resp.StatusCode)
	}
	// A second server on the same name fails instead of serving beside the first.
	if err := ui.servePipe(ctx, name); err == nil {
		t.Fatal("a second pipe server on the same name started")
	}
	if _, err := AskLoginLink(name+"-none", DefaultPort, time.Second); err == nil || !strings.Contains(err.Error(), "not running") {
		t.Fatalf("no pipe: %v", err)
	}
}

// fileReaders reads the password file's permissions as the installer sets them.
func TestFileReaders(t *testing.T) {
	tok := windows.GetCurrentProcessToken()
	u, err := tok.GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	me := u.User.Sid.String()
	f := filepath.Join(t.TempDir(), "ui-password")
	if err := os.WriteFile(f, []byte(testPassword), 0o600); err != nil {
		t.Fatal(err)
	}
	set := func(sddl string) {
		t.Helper()
		sd, err := windows.SecurityDescriptorFromString(sddl)
		if err != nil {
			t.Fatal(err)
		}
		dacl, _, err := sd.DACL()
		if err != nil {
			t.Fatal(err)
		}
		if err := windows.SetNamedSecurityInfo(f, windows.SE_FILE_OBJECT,
			windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, dacl, nil); err != nil {
			t.Fatal(err)
		}
	}
	set("D:P(A;;FA;;;SY)(A;;FA;;;BA)(A;;FR;;;" + me + ")(A;;0x100;;;BU)")
	allow, deny, err := fileReaders(f)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"S-1-5-18", sidAdministrators, me} {
		if !slices.Contains(allow, want) {
			t.Errorf("readers %v lack %s", allow, want)
		}
	}
	if slices.Contains(allow, "S-1-5-32-545") || len(deny) != 0 {
		t.Errorf("Users (no read right) counted, or deny entries invented: %v %v", allow, deny)
	}
	set("D:P(D;;FR;;;" + me + ")(A;;FA;;;SY)(A;;FA;;;BA)")
	if _, deny, err = fileReaders(f); err != nil || !slices.Contains(deny, me) {
		t.Errorf("deny entry not read: %v %v", deny, err)
	}
}
