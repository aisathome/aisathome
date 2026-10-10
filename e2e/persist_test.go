// Tests for state that must survive a gateway restart, and for per-user API keys managed in
// a keys file.
//
// SPDX-License-Identifier: Apache-2.0
package e2e

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aisathome/aisathome/gateway"
)

// A restart must not hand a key a fresh daily budget.
func TestBudgetSurvivesRestart(t *testing.T) {
	var cfg gateway.Config
	r := newRigCfg(t, func(c *gateway.Config) {
		c.StateDir = t.TempDir()
		c.Keys["budget-key"] = gateway.KeyInfo{User: "dave", TokensPerDay: 10}
		cfg = *c
	}, mockEngine(t, false))
	ask := func(url string) int {
		req, _ := http.NewRequest("POST", url+"/v1/chat/completions", strings.NewReader(chatBody))
		req.Header.Set("Authorization", "Bearer budget-key")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		return resp.StatusCode
	}
	// mockEngine's usage weighs 7 tokens: two requests use up a budget of 10.
	for i := 0; i < 2; i++ {
		if c := ask(r.gw.URL); c != 200 {
			t.Fatalf("request %d: %d", i, c)
		}
	}
	if c := ask(r.gw.URL); c != 429 {
		t.Fatalf("budget not enforced before restart: %d", c)
	}
	r.gw.Close()
	if err := r.g.Close(); err != nil {
		t.Fatal(err)
	}

	cfg.QueueTimeoutSec = 1 // without the saved budget, fail fast rather than wait for a node
	g2, err := gateway.New(cfg, quiet())
	if err != nil {
		t.Fatal(err)
	}
	defer g2.Close()
	gw2 := httptest.NewServer(g2.Handler())
	defer gw2.Close()
	if c := ask(gw2.URL); c != 429 {
		t.Fatalf("restart reset the daily budget: got %d, want 429", c)
	}
}

// eventually polls until the key gets want from /v1/models (the gateway checks the keys
// file every few seconds).
func eventually(t *testing.T, r *rig, key string, want int) {
	t.Helper()
	got := 0
	for deadline := time.Now().Add(6 * time.Second); time.Now().Before(deadline); time.Sleep(100 * time.Millisecond) {
		resp := r.do(t, "GET", "/v1/models", key, "")
		resp.Body.Close()
		if got = resp.StatusCode; got == want {
			return
		}
	}
	t.Fatalf("key got %d, want %d", got, want)
}

// Keys added to and revoked from the keys file apply without a restart or reload.
func TestKeysFileAddAndRevoke(t *testing.T) {
	path := filepath.Join(t.TempDir(), "keys.json")
	r := newRigCfg(t, func(c *gateway.Config) { c.KeysFile = path }, mockEngine(t, false))
	var kf gateway.KeysFile
	key, err := kf.Add(gateway.KeyInfo{User: "erin"}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := kf.Save(path); err != nil {
		t.Fatal(err)
	}
	eventually(t, r, key, 200)
	resp := r.do(t, "POST", "/v1/chat/completions", key, chatBody)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || waitUsage(t, r, 1)[0]["user"] != "erin" {
		t.Fatalf("chat with a file key: %d", resp.StatusCode)
	}
	if resp := r.do(t, "GET", "/v1/models", kf.Keys[0].Hash, ""); resp.StatusCode != 401 {
		t.Fatalf("the stored hash works as a key: %d", resp.StatusCode)
	}
	if _, err := kf.Revoke(key[:11]); err != nil {
		t.Fatal(err)
	}
	if err := kf.Save(path); err != nil {
		t.Fatal(err)
	}
	eventually(t, r, key, 401)
	if resp := r.do(t, "POST", "/v1/chat/completions", key, chatBody); resp.StatusCode != 401 {
		t.Fatalf("revoked key can still chat: %d", resp.StatusCode)
	}
	if resp := r.do(t, "GET", "/v1/models", "user-key", ""); resp.StatusCode != 200 {
		t.Fatalf("config keys must keep working: %d", resp.StatusCode)
	}
}

// A broken keys file never locks users out: the keys in use stay until it is fixed.
func TestBrokenKeysFileKeepsKeys(t *testing.T) {
	path := filepath.Join(t.TempDir(), "keys.json")
	var kf gateway.KeysFile
	key, _ := kf.Add(gateway.KeyInfo{User: "erin"}, time.Now())
	kf.Save(path)
	var cfg gateway.Config
	r := newRigCfg(t, func(c *gateway.Config) { c.KeysFile = path; cfg = *c }, mockEngine(t, false))
	eventually(t, r, key, 200)
	os.WriteFile(path, []byte(`{"keys": [`), 0o600)
	if err := r.g.Reload(cfg); err == nil {
		t.Fatal("reload with a broken keys file must fail")
	}
	time.Sleep(2500 * time.Millisecond) // one watcher round
	eventually(t, r, key, 200)
}
