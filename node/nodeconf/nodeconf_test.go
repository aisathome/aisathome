// SPDX-License-Identifier: Apache-2.0
package nodeconf

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestCheckKeys(t *testing.T) {
	for body, ok := range map[string]bool{
		`{"gateway_url":"a","node_id":"b"}`:       true,
		`{"Gateway_URL":"a"}`:                     true, // one spelling: every reader agrees
		`{"models":[{"name":"a"},{"name":"b"}]}`:  true,
		`{"a":{"x":1},"b":{"x":2}}`:               true,
		`{"gateway_url":"a","gateway_url":"b"}`:   false,
		`{"gateway_url":"a","GATEWAY_URL":"b"}`:   false,
		`{"engine":{"tier":"a","Tier":"b"}}`:      false,
		`{"models":[{"name":"a","NAME":"b"}]}`:    false,
		`{"gateway_url":"a"} {"gateway_url":"b"}`: false,
		`{"gateway_url":`:                         false,
	} {
		if err := CheckKeys([]byte(body)); (err == nil) != ok {
			t.Errorf("%s: %v, want ok %v", body, err, ok)
		}
	}
}

func TestReadObjectAndEncode(t *testing.T) {
	p := filepath.Join(t.TempDir(), "node.json")
	os.WriteFile(p, []byte(`{"Gateway_URL":"wss://a/v1/node/connect","node_id":"x"}`), 0o600)
	m, exists, err := ReadObject(p)
	if err != nil || !exists || String(m, "gateway_url") != "wss://a/v1/node/connect" {
		t.Fatalf("%v %v %v", m, exists, err)
	}
	Set(m, "Token_Gateway", "wss://a/v1/node/connect")
	b, _ := Encode(m)
	if strings.Contains(string(b), "Gateway") || !strings.Contains(string(b), `"token_gateway"`) {
		t.Errorf("not canonical: %s", b)
	}
	os.WriteFile(p, []byte(`{"gateway_url":"a","GATEWAY_URL":"b"}`), 0o600)
	if _, _, err := ReadObject(p); err == nil || !strings.Contains(err.Error(), "more than once") {
		t.Errorf("case variants read: %v", err)
	}
	if m, exists, err := ReadObject(filepath.Join(t.TempDir(), "none.json")); err != nil || exists || len(m) != 0 {
		t.Errorf("missing file: %v %v %v", m, exists, err)
	}
}

func TestTokenEnv(t *testing.T) {
	yes := func(string) bool { return true }
	const gw = "wss://api.example.org/v1/node/connect"
	b, err := TokenEnv("nt-n-1.x", gw, yes)
	if err != nil || string(b) != "AISATHOME_NODE_TOKEN=nt-n-1.x\nAISATHOME_TOKEN_GATEWAY="+gw+"\n" {
		t.Fatalf("%q %v", b, err)
	}
	if tok, rec := ParseTokenEnv(b); tok != "nt-n-1.x" || rec != gw {
		t.Errorf("parsed %q %q", tok, rec)
	}
	if tok, rec := ParseTokenEnv([]byte("AISATHOME_NODE_TOKEN=nt-n-1.x\n")); tok != "nt-n-1.x" || rec != "" {
		t.Errorf("no record: %q %q", tok, rec)
	}
	if _, err := TokenEnv("bad", gw, func(string) bool { return false }); err == nil {
		t.Error("took a token the check refuses")
	}
	for _, bad := range []string{"", gw + "\nLD_PRELOAD=/x", "https://api.example.org/", "wss://a b/", "wss://a/#x", "wss://a/$HOME"} {
		if _, err := TokenEnv("nt-n-1.x", bad, yes); err == nil {
			t.Errorf("took the gateway %q", bad)
		}
	}
}

func TestCheckTokenGateway(t *testing.T) {
	if err := CheckTokenGateway("wss://a/v1/node/connect", "wss://a/v1/node/connect"); err != nil {
		t.Error(err)
	}
	if err := CheckTokenGateway("", "wss://a/v1/node/connect"); err != nil {
		t.Error(err)
	}
	if err := CheckTokenGateway("wss://prod/v1/node/connect", "wss://staging/v1/node/connect"); !errors.Is(err, ErrTokenGateway) {
		t.Errorf("another gateway's token: %v", err)
	}
}

// Lock is the installer's lock too: while held, flock(1) on the same file (as install.sh takes it) cannot
// take it, and can once it is released.
func TestLockIsTheInstallersLock(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("flock(1)")
	}
	if _, err := exec.LookPath("flock"); err != nil {
		t.Skip("no flock(1)")
	}
	dir := t.TempDir()
	unlock, err := Lock(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := exec.Command("flock", "-n", filepath.Join(dir, LockFile), "true").Run(); err == nil {
		unlock()
		t.Fatal("flock took the lock while Lock held it")
	}
	unlock()
	if err := exec.Command("flock", "-n", filepath.Join(dir, LockFile), "true").Run(); err != nil {
		t.Fatalf("flock after the release: %v", err)
	}
}
