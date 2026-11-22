// SPDX-License-Identifier: Apache-2.0
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aisathome/aisathome/node"
	"github.com/aisathome/aisathome/node/accountlink"
	"github.com/aisathome/aisathome/protocol"
)

const linkTestToken = "nt-n-1a2b3c4d.AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"

// fakeLinkGateway answers the link calls: pending twice, then outcome ("approved" with linkTestToken,
// "denied", or "expired").
type fakeLinkGateway struct {
	srv     *httptest.Server
	outcome string
	token   string // the approved token; "" = linkTestToken
	nodeID  string // the approved node id; "" = n-1a2b3c4d
	mu      sync.Mutex
	starts  int
	polls   int
	info    protocol.LinkInfo
}

func newFakeLinkGateway(t *testing.T, outcome string) *fakeLinkGateway {
	f := &fakeLinkGateway{outcome: outcome}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case protocol.LinkPath:
			f.starts++
			json.NewDecoder(r.Body).Decode(&f.info)
			json.NewEncoder(w).Encode(protocol.LinkStart{DeviceCode: "dev-secret", UserCode: "KXQT-4821",
				VerificationURI: "https://site.test/link", VerificationURIComplete: "https://site.test/link?code=KXQT-4821",
				ExpiresIn: 600, Interval: 5})
		case protocol.LinkPollPath:
			var p protocol.LinkPollRequest
			json.NewDecoder(r.Body).Decode(&p)
			f.polls++
			out := protocol.LinkPoll{Status: protocol.LinkPending, Interval: 5}
			switch {
			case p.DeviceCode != "dev-secret":
				out = protocol.LinkPoll{Status: protocol.LinkExpired}
			case f.polls <= 2:
			case f.outcome == protocol.LinkApproved:
				out = protocol.LinkPoll{Status: protocol.LinkApproved, Token: linkTestToken, NodeID: "n-1a2b3c4d"}
				if f.token != "" {
					out.Token, out.NodeID = f.token, f.nodeID
				}
			default:
				out = protocol.LinkPoll{Status: f.outcome}
			}
			json.NewEncoder(w).Encode(out)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeLinkGateway) wsURL() string {
	return "ws" + strings.TrimPrefix(f.srv.URL, "http") + "/v1/node/connect"
}

type restartCall struct{ tokenEnv bool }

func testLinkEnv(f *fakeLinkGateway, layout node.Layout, restarts *[]restartCall) linkEnv {
	return linkEnv{layout: layout, exists: fileExists, smi: nil,
		client: func(base string) *accountlink.Client {
			return &accountlink.Client{Base: base, Sleep: func(ctx context.Context, d time.Duration) error { return ctx.Err() }}
		},
		restart: func(tokenEnv bool, out io.Writer) error {
			*restarts = append(*restarts, restartCall{tokenEnv})
			return nil
		}}
}

func mode(t *testing.T, p string) os.FileMode {
	t.Helper()
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	return fi.Mode().Perm()
}

// The Linux install: the token goes to token.env (0600), node.json gets the node id and keeps its mode,
// and the node restarts.
func TestLinkSavesToTokenEnv(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("file modes")
	}
	f := newFakeLinkGateway(t, protocol.LinkApproved)
	dir := t.TempDir()
	cfg, env := filepath.Join(dir, "node.json"), filepath.Join(dir, "token.env")
	os.WriteFile(cfg, []byte(`{"node_id":"box","gateway_url":"`+f.wsURL()+`","upstream":"http://127.0.0.1:8000"}`), 0o640)
	os.Chmod(cfg, 0o640)
	os.WriteFile(env, []byte("AISATHOME_NODE_TOKEN=nt-n-00000000.old\n"), 0o600)
	var restarts []restartCall
	var out, errb bytes.Buffer
	code := testLinkEnv(f, node.LinuxLayout, &restarts).main(context.Background(), []string{"-config", cfg, "-name", "my 4090"}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errb.String())
	}
	for _, want := range []string{"https://site.test/link?code=KXQT-4821", "KXQT-4821", "Linked: this is node n-1a2b3c4d", env} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out.String())
		}
	}
	if strings.Contains(out.String(), linkTestToken) || strings.Contains(out.String(), "dev-secret") {
		t.Error("the output shows the token or the device code")
	}
	b, _ := os.ReadFile(env)
	if string(b) != "AISATHOME_NODE_TOKEN="+linkTestToken+"\n" || mode(t, env) != 0o600 {
		t.Errorf("token.env %q, mode %o", b, mode(t, env))
	}
	var c map[string]any
	b, _ = os.ReadFile(cfg)
	json.Unmarshal(b, &c)
	if c["node_id"] != "n-1a2b3c4d" || c["token"] != nil || c["upstream"] != "http://127.0.0.1:8000" || c["gateway_url"] != f.wsURL() || mode(t, cfg) != 0o640 {
		t.Errorf("node.json %s, mode %o", b, mode(t, cfg))
	}
	if len(restarts) != 1 || !restarts[0].tokenEnv {
		t.Errorf("restarts %v", restarts)
	}
	if f.info.Name != "my 4090" || !strings.HasPrefix(f.info.Agent, "aisathome-node/") || f.info.OS == "" {
		t.Errorf("reported %+v", f.info)
	}
	if left, _ := filepath.Glob(filepath.Join(dir, ".*")); len(left) != 0 {
		t.Errorf("left behind: %v", left)
	}
}

// Without token.env the token goes into the config, which then is 0600; -no-start leaves the node alone.
func TestLinkSavesToConfig(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("file modes")
	}
	f := newFakeLinkGateway(t, protocol.LinkApproved)
	cfg := filepath.Join(t.TempDir(), "node.json")
	os.WriteFile(cfg, []byte(`{"gateway_url":"`+f.wsURL()+`"}`), 0o644)
	os.Chmod(cfg, 0o644)
	var restarts []restartCall
	var out, errb bytes.Buffer
	if code := testLinkEnv(f, node.LinuxLayout, &restarts).main(context.Background(), []string{"-config", cfg, "-no-start"}, &out, &errb); code != 0 {
		t.Fatalf("exit %d: %s", code, errb.String())
	}
	var c map[string]any
	b, _ := os.ReadFile(cfg)
	json.Unmarshal(b, &c)
	if c["token"] != linkTestToken || c["node_id"] != "n-1a2b3c4d" || mode(t, cfg) != 0o600 {
		t.Errorf("node.json %s, mode %o", b, mode(t, cfg))
	}
	if len(restarts) != 0 {
		t.Errorf("-no-start restarted: %v", restarts)
	}

	// -gateway names another gateway: the token is that gateway's, so the config gets its address too.
	f2 := newFakeLinkGateway(t, protocol.LinkApproved)
	if code := testLinkEnv(f2, node.LinuxLayout, &restarts).main(context.Background(), []string{"-config", cfg, "-gateway", f2.wsURL()}, &out, &errb); code != 0 {
		t.Fatalf("exit %d: %s", code, errb.String())
	}
	b, _ = os.ReadFile(cfg)
	json.Unmarshal(b, &c)
	if c["gateway_url"] != f2.wsURL() || f2.starts != 1 || len(restarts) != 1 || restarts[0].tokenEnv {
		t.Errorf("node.json %s, starts %d, restarts %v", b, f2.starts, restarts)
	}
}

// Denied or expired: exit 1, nothing saved, nothing restarted.
func TestLinkDeniedOrExpired(t *testing.T) {
	for _, outcome := range []string{protocol.LinkDenied, protocol.LinkExpired} {
		f := newFakeLinkGateway(t, outcome)
		cfg := filepath.Join(t.TempDir(), "node.json")
		orig := `{"gateway_url":"` + f.wsURL() + `"}`
		os.WriteFile(cfg, []byte(orig), 0o600)
		var restarts []restartCall
		var out, errb bytes.Buffer
		code := testLinkEnv(f, node.LinuxLayout, &restarts).main(context.Background(), []string{"-config", cfg}, &out, &errb)
		b, _ := os.ReadFile(cfg)
		if code != 1 || string(b) != orig || len(restarts) != 0 {
			t.Errorf("%s: exit %d, config %s, restarts %v", outcome, code, b, restarts)
		}
		if !strings.Contains(errb.String(), outcome[:4]) { // "deni", "expi"
			t.Errorf("%s: message %q", outcome, errb.String())
		}
	}
}

// A token that could not be saved would be lost: the folder must take a new file before the gateway is
// asked; and a Windows node links only once its setup is done.
func TestLinkChecksBeforeAsking(t *testing.T) {
	f := newFakeLinkGateway(t, protocol.LinkApproved)
	var restarts []restartCall
	var out, errb bytes.Buffer
	if runtime.GOOS != "windows" && os.Geteuid() != 0 {
		dir := t.TempDir()
		cfg := filepath.Join(dir, "node.json")
		os.WriteFile(cfg, []byte(`{"gateway_url":"`+f.wsURL()+`"}`), 0o600)
		os.Chmod(dir, 0o500)
		t.Cleanup(func() { os.Chmod(dir, 0o700) })
		if code := testLinkEnv(f, node.LinuxLayout, &restarts).main(context.Background(), []string{"-config", cfg}, &out, &errb); code != 1 || f.starts != 0 {
			t.Errorf("read-only folder: exit %d, gateway asked %d times: %s", code, f.starts, errb.String())
		}
	}
	dir := t.TempDir()
	win := node.Layout{Config: filepath.Join(dir, "node.json"), Model: filepath.Join(dir, "model.gguf"), Setup: true}
	os.WriteFile(win.Config, []byte(`{"gateway_url":"`+f.wsURL()+`"}`), 0o600)
	errb.Reset()
	if code := testLinkEnv(f, win, &restarts).main(context.Background(), nil, &out, &errb); code != 1 || f.starts != 0 ||
		!strings.Contains(errb.String(), "Link to my account") {
		t.Errorf("unconfigured Windows node: exit %d, gateway asked %d times: %s", code, f.starts, errb.String())
	}
}

func TestLinkTarget(t *testing.T) {
	dir := t.TempDir()
	e := linkEnv{layout: node.LinuxLayout, exists: func(p string) bool { return p == linkInstalledConfig }}
	if got := e.target("", ""); runtime.GOOS == "linux" && (got.config != linkInstalledConfig || got.tokenEnv != "/etc/aisathome-node/token.env") {
		t.Errorf("installed: %+v", got)
	}
	e.exists = fileExists
	cfg := filepath.Join(dir, "node.json")
	if got := e.target(cfg, ""); got.tokenEnv != "" {
		t.Errorf("no token.env beside: %+v", got)
	}
	os.WriteFile(filepath.Join(dir, "token.env"), nil, 0o600)
	if got := e.target(cfg, ""); got.tokenEnv != filepath.Join(dir, "token.env") {
		t.Errorf("token.env beside: %+v", got)
	}
}

// injectedTokens are approvals a malicious or broken gateway could send: each would add a line or a
// setting to token.env (a systemd EnvironmentFile), or is not the form the gateway issues.
var injectedTokens = []string{
	linkTestToken + "\nLD_PRELOAD=/tmp/x.so",
	"nt-n-1a2b3c4d.AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA\n",
	"nt-n-1a2b3c4d.AAAAAAAAAAAAAAAAAAAA=AAAAAAAAAAAAAAAAAAAAAA",
	"nt-n-1a2b3c4d.AAAAAAAAAAAAAAAAAAAA AAAAAAAAAAAAAAAAAAAAAA",
	"nt-n-1a2b3c4d.AAAAAAAAAAAAAAAAAAAA\"AAAAAAAAAAAAAAAAAAAAAA",
	"nt-n-1a2b3c4d.AAAAAAAAAAAAAAAAAAAA#AAAAAAAAAAAAAAAAAAAAAA",
	"nt-n-1a2b3c4d.AAAAAAAAAAAAAAAAAAAA\rAAAAAAAAAAAAAAAAAAAAAA",
	linkTestToken + "A",                  // 44 characters
	linkTestToken[:len(linkTestToken)-1], // 42
	"nt-n-1a2b3c4d.secret",
}

// A gateway that approves with a token that is not exactly the form it issues: nothing is written,
// token.env and node.json stay as they were, and the node is not restarted.
func TestLinkRefusesInjectedToken(t *testing.T) {
	for _, tok := range injectedTokens {
		f := newFakeLinkGateway(t, protocol.LinkApproved)
		f.token, f.nodeID = tok, "n-1a2b3c4d"
		dir := t.TempDir()
		cfg, env := filepath.Join(dir, "node.json"), filepath.Join(dir, "token.env")
		origCfg := `{"node_id":"box","gateway_url":"` + f.wsURL() + `"}`
		os.WriteFile(cfg, []byte(origCfg), 0o640)
		os.WriteFile(env, []byte("AISATHOME_NODE_TOKEN=nt-n-00000000.old\n"), 0o600)
		var restarts []restartCall
		var out, errb bytes.Buffer
		code := testLinkEnv(f, node.LinuxLayout, &restarts).main(context.Background(), []string{"-config", cfg}, &out, &errb)
		b, _ := os.ReadFile(env)
		c, _ := os.ReadFile(cfg)
		if code != 1 || string(b) != "AISATHOME_NODE_TOKEN=nt-n-00000000.old\n" || string(c) != origCfg || len(restarts) != 0 {
			t.Errorf("%q: exit %d, token.env %q, node.json %s, restarts %v", tok, code, b, c, restarts)
		}
		if !strings.Contains(errb.String(), "no valid node token") || strings.Contains(errb.String()+out.String(), "LD_PRELOAD") {
			t.Errorf("%q: message %q", tok, errb.String())
		}
	}
	// A valid token for another node id than the answer names is refused too.
	f := newFakeLinkGateway(t, protocol.LinkApproved)
	f.token, f.nodeID = linkTestToken, "n-ffffffff"
	cfg := filepath.Join(t.TempDir(), "node.json")
	os.WriteFile(cfg, []byte(`{"gateway_url":"`+f.wsURL()+`"}`), 0o600)
	var restarts []restartCall
	var out, errb bytes.Buffer
	if code := testLinkEnv(f, node.LinuxLayout, &restarts).main(context.Background(), []string{"-config", cfg}, &out, &errb); code != 1 {
		t.Errorf("mismatched node id: exit %d", code)
	}
}

// The write itself checks the token again (save and writeTokenEnv): even a caller that skipped the
// client's check cannot put another line into token.env. A valid token is written atomically, 0600.
func TestWriteTokenEnvChecksToken(t *testing.T) {
	dir := t.TempDir()
	env, cfg := filepath.Join(dir, "token.env"), filepath.Join(dir, "node.json")
	os.WriteFile(env, []byte("AISATHOME_NODE_TOKEN=nt-n-00000000.old\n"), 0o644)
	os.WriteFile(cfg, []byte(`{"node_id":"box"}`), 0o640)
	tgt := linkTarget{config: cfg, tokenEnv: env}
	for _, tok := range injectedTokens {
		if err := writeTokenEnv(env, tok); err == nil {
			t.Errorf("writeTokenEnv took %q", tok)
		}
		if err := tgt.save(tok, "n-1a2b3c4d", false, ""); err == nil {
			t.Errorf("save took %q", tok)
		}
		if err := (linkTarget{config: cfg}).save(tok, "n-1a2b3c4d", false, ""); err == nil {
			t.Errorf("save into the config took %q", tok)
		}
	}
	if err := tgt.save(linkTestToken, "n-ffffffff", false, ""); err == nil {
		t.Error("save took a token for another node id")
	}
	if b, _ := os.ReadFile(env); string(b) != "AISATHOME_NODE_TOKEN=nt-n-00000000.old\n" {
		t.Fatalf("token.env changed: %q", b)
	}
	if b, _ := os.ReadFile(cfg); string(b) != `{"node_id":"box"}` {
		t.Fatalf("node.json changed: %s", b)
	}
	if err := tgt.save(linkTestToken, "n-1a2b3c4d", false, ""); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(env); string(b) != "AISATHOME_NODE_TOKEN="+linkTestToken+"\n" {
		t.Errorf("token.env %q", b)
	}
	if runtime.GOOS != "windows" && mode(t, env) != 0o600 {
		t.Errorf("token.env mode %o, want 600", mode(t, env))
	}
	if left, _ := filepath.Glob(filepath.Join(dir, ".*")); len(left) != 0 {
		t.Errorf("left behind: %v", left)
	}
}

// link -gateway with token.env: the new token never sits beside the old gateway's address, nor the old
// token beside the new one. The old token.env goes aside first; if the config cannot be written, it
// comes back beside its own config and the new token is not written.
func TestLinkOtherGatewaySavesInOrder(t *testing.T) {
	const old, oldGW, newGW = "AISATHOME_NODE_TOKEN=nt-n-00000000.old\n", "wss://old.example/v1/node/connect", "wss://new.example/v1/node/connect"
	dir := t.TempDir()
	cfg, env := filepath.Join(dir, "node.json"), filepath.Join(dir, "token.env")
	tgt := linkTarget{config: cfg, tokenEnv: env}

	os.WriteFile(cfg, []byte(`{"gateway_url":"`+oldGW+`",`), 0o640) // the config write fails: not JSON
	os.WriteFile(env, []byte(old), 0o600)
	if err := tgt.save(linkTestToken, "n-1a2b3c4d", true, newGW); err == nil {
		t.Fatal("saved over a config that cannot be written")
	}
	if b, _ := os.ReadFile(env); string(b) != old {
		t.Errorf("config write failed: token.env %q, want the old token back", b)
	}
	if _, err := os.Stat(env + ".unlinked"); err == nil {
		t.Error("config write failed: the old token is still aside")
	}

	// The node is stopped once the old token is aside and before the config names the new gateway.
	os.WriteFile(cfg, []byte(`{"gateway_url":"`+oldGW+`"}`), 0o640)
	stops := 0
	tgt.stopNode = func() (bool, error) {
		stops++
		if _, err := os.Stat(env); err == nil {
			t.Error("stopping the node: the old token is still in token.env")
		}
		if gw := configGateway(cfg); gw != oldGW {
			t.Errorf("stopping the node: the config names %s already", gw)
		}
		return true, nil
	}
	if err := tgt.save(linkTestToken, "n-1a2b3c4d", true, newGW); err != nil {
		t.Fatal(err)
	}
	if stops != 1 {
		t.Errorf("the node was stopped %d times, want 1", stops)
	}
	if b, _ := os.ReadFile(env); string(b) != "AISATHOME_NODE_TOKEN="+linkTestToken+"\n" {
		t.Errorf("token.env %q", b)
	}
	if b, _ := os.ReadFile(env + ".unlinked"); string(b) != old {
		t.Errorf("token.env.unlinked %q, want the old token", b)
	}
	if configGateway(cfg) != newGW {
		t.Errorf("config gateway %q", configGateway(cfg))
	}

	// The same gateway given again: nothing goes aside, and the node is not stopped.
	os.Remove(env + ".unlinked")
	stops = 0
	if err := tgt.save(linkTestToken, "n-1a2b3c4d", true, newGW); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(env + ".unlinked"); err == nil {
		t.Error("same gateway: the token was moved aside")
	}
	if stops != 0 {
		t.Errorf("same gateway: the node was stopped %d times", stops)
	}
}

// otherGatewayDir is a node linked to oldGW: node.json (with a key in another case) and token.env.
func otherGatewayDir(t *testing.T) (tgt linkTarget, cfgBody string) {
	dir := t.TempDir()
	tgt = linkTarget{config: filepath.Join(dir, "node.json"), tokenEnv: filepath.Join(dir, "token.env")}
	cfgBody = `{"Gateway_URL":"wss://old.example/v1/node/connect","upstream":"http://127.0.0.1:8000"}`
	os.WriteFile(tgt.config, []byte(cfgBody), 0o640)
	os.Chmod(tgt.config, 0o640)
	os.WriteFile(tgt.tokenEnv, []byte("AISATHOME_NODE_TOKEN=nt-n-00000000.old\n"), 0o600)
	return tgt, cfgBody
}

// checkUnchanged fails unless the node's config and token are the ones otherGatewayDir made.
func checkUnchanged(t *testing.T, what string, tgt linkTarget, cfgBody string) {
	t.Helper()
	if b, _ := os.ReadFile(tgt.config); string(b) != cfgBody {
		t.Errorf("%s: node.json %s", what, b)
	}
	if b, _ := os.ReadFile(tgt.tokenEnv); string(b) != "AISATHOME_NODE_TOKEN=nt-n-00000000.old\n" {
		t.Errorf("%s: token.env %q, want the old token", what, b)
	}
	if _, err := os.Stat(tgt.tokenEnv + ".unlinked"); err == nil {
		t.Errorf("%s: the old token is still aside", what)
	}
}

// link -gateway on a node that does not stop changes nothing: the old token stays with its config.
func TestLinkOtherGatewayNodeDoesNotStop(t *testing.T) {
	tgt, cfgBody := otherGatewayDir(t)
	starts := 0
	tgt.stopNode = func() (bool, error) { return true, errors.New("job canceled") }
	tgt.startNode = func() error { starts++; return nil }
	err := tgt.save(linkTestToken, "n-1a2b3c4d", true, "wss://new.example/v1/node/connect")
	if err == nil || !strings.Contains(err.Error(), "did not stop") {
		t.Fatalf("save: %v", err)
	}
	checkUnchanged(t, "the node did not stop", tgt, cfgBody)
	if starts != 0 {
		t.Errorf("started %d times", starts)
	}
}

// The token write fails after the config named the new gateway: the old config comes back, then the old
// token beside it, and a node that was on starts again, with both back in place.
func TestLinkOtherGatewayRollsBack(t *testing.T) {
	defer func(w func(string, string) error) { saveTokenEnv = w }(saveTokenEnv)
	saveTokenEnv = func(string, string) error { return errors.New("disk full") }
	for _, wasOn := range []bool{true, false} {
		tgt, cfgBody := otherGatewayDir(t)
		starts := 0
		tgt.stopNode = func() (bool, error) { return wasOn, nil }
		tgt.startNode = func() error {
			starts++
			checkUnchanged(t, "starting the node again", tgt, cfgBody)
			return nil
		}
		err := tgt.save(linkTestToken, "n-1a2b3c4d", true, "wss://new.example/v1/node/connect")
		if err == nil || !strings.Contains(err.Error(), "disk full") {
			t.Fatalf("save: %v", err)
		}
		checkUnchanged(t, "token write failed", tgt, cfgBody)
		if want := map[bool]int{true: 1, false: 0}[wasOn]; starts != want {
			t.Errorf("node on %v: started %d times, want %d", wasOn, starts, want)
		}
		if runtime.GOOS != "windows" && mode(t, tgt.config) != 0o640 {
			t.Errorf("node.json mode %o after the rollback", mode(t, tgt.config))
		}
	}
}

// The config's gateway key in another case is the agent's gateway too: link reads it, and link -gateway
// replaces it rather than adding a second key the agent might read instead.
func TestLinkGatewayKeyAnyCase(t *testing.T) {
	tgt, _ := otherGatewayDir(t)
	if err := tgt.save(linkTestToken, "n-1a2b3c4d", true, "wss://new.example/v1/node/connect"); err != nil {
		t.Fatal(err)
	}
	var c map[string]any
	b, _ := os.ReadFile(tgt.config)
	json.Unmarshal(b, &c)
	if c["gateway_url"] != "wss://new.example/v1/node/connect" || c["Gateway_URL"] != nil || c["upstream"] != "http://127.0.0.1:8000" {
		t.Errorf("node.json %s", b)
	}

	f := newFakeLinkGateway(t, protocol.LinkApproved)
	cfg := filepath.Join(t.TempDir(), "node.json")
	os.WriteFile(cfg, []byte(`{"Gateway_URL":"`+f.wsURL()+`"}`), 0o600)
	var restarts []restartCall
	var out, errb bytes.Buffer
	if code := testLinkEnv(f, node.LinuxLayout, &restarts).main(context.Background(), []string{"-config", cfg, "-no-start"}, &out, &errb); code != 0 || f.starts != 1 {
		t.Errorf("exit %d, gateway asked %d times: %s", code, f.starts, errb.String())
	}
}

// link (without -gateway) does not save a token beside a gateway the config was changed to meanwhile.
func TestLinkConfigGatewayChangedMeanwhile(t *testing.T) {
	tgt, cfgBody := otherGatewayDir(t)
	if err := tgt.save(linkTestToken, "n-1a2b3c4d", false, "wss://new.example/v1/node/connect"); err == nil {
		t.Fatal("saved a token for another gateway than the config names")
	}
	checkUnchanged(t, "gateway changed meanwhile", tgt, cfgBody)
}

// Two saves at once go one after the other: a save waits while another holds the config folder's lock.
func TestLinkSaveWaitsForLock(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no lock: the token goes into node.json in one write")
	}
	tgt, _ := otherGatewayDir(t)
	unlock, err := lockDir(filepath.Dir(tgt.config))
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- tgt.save(linkTestToken, "n-1a2b3c4d", true, "wss://new.example/v1/node/connect") }()
	select {
	case err := <-done:
		unlock()
		t.Fatalf("saved while another held the lock: %v", err)
	case <-time.After(200 * time.Millisecond):
	}
	unlock()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

// link -gateway on the Linux install stops the node through linkEnv.stop before it saves.
func TestLinkOtherGatewayStopsNode(t *testing.T) {
	f := newFakeLinkGateway(t, protocol.LinkApproved)
	tgt, _ := otherGatewayDir(t)
	var restarts []restartCall
	var out, errb bytes.Buffer
	e := testLinkEnv(f, node.LinuxLayout, &restarts)
	stops := 0
	e.stop = func() (bool, error) { stops++; return true, nil }
	code := e.main(context.Background(), []string{"-config", tgt.config, "-gateway", f.wsURL()}, &out, &errb)
	if code != 0 || stops != 1 || len(restarts) != 1 || configGateway(tgt.config) != f.wsURL() {
		t.Errorf("exit %d, stops %d, restarts %v, gateway %s: %s", code, stops, restarts, configGateway(tgt.config), errb.String())
	}
	if b, _ := os.ReadFile(tgt.tokenEnv); string(b) != "AISATHOME_NODE_TOKEN="+linkTestToken+"\n" {
		t.Errorf("token.env %q", b)
	}
}
