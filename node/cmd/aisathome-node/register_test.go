// SPDX-License-Identifier: Apache-2.0
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/aisathome/aisathome/node"
	"github.com/aisathome/aisathome/protocol"
)

const anonTestToken = "nt-a-0a0b0c0d.BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB"

// fakeRegisterGateway answers POST /v1/node/register with anonTestToken (or refuses), and the link
// calls as fakeLinkGateway does, recording the Authorization of the code request.
type fakeRegisterGateway struct {
	*fakeLinkGateway
	refuse string
	mu     sync.Mutex
	regs   []protocol.RegisterRequest
	auth   string
}

func newFakeRegisterGateway(t *testing.T, refuse string) *fakeRegisterGateway {
	f := &fakeRegisterGateway{fakeLinkGateway: newFakeLinkGateway(t, protocol.LinkApproved), refuse: refuse}
	link := f.srv.Config.Handler
	f.srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case protocol.RegisterPath:
			var b protocol.RegisterRequest
			json.NewDecoder(r.Body).Decode(&b)
			f.mu.Lock()
			f.regs = append(f.regs, b)
			f.mu.Unlock()
			if f.refuse != "" {
				w.WriteHeader(http.StatusTooManyRequests)
				json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"message": f.refuse}})
				return
			}
			json.NewEncoder(w).Encode(protocol.Register{Token: anonTestToken, NodeID: "a-0a0b0c0d"})
		case protocol.LinkPath:
			f.mu.Lock()
			f.auth = r.Header.Get("Authorization")
			f.mu.Unlock()
			link.ServeHTTP(w, r)
		default:
			link.ServeHTTP(w, r)
		}
	})
	return f
}

// register on the Linux install: it needs -accept-terms, writes the token to token.env (0600) and the
// node id to node.json, and starts the node; a node with a token keeps it.
func TestRegister(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("file modes")
	}
	f := newFakeRegisterGateway(t, "")
	dir := t.TempDir()
	cfg, env := filepath.Join(dir, "node.json"), filepath.Join(dir, "token.env")
	os.WriteFile(cfg, []byte(`{"node_id":"box","gateway_url":"`+f.wsURL()+`"}`), 0o640)
	var restarts []restartCall
	run := func(args ...string) (int, string, string) {
		var out, errb bytes.Buffer
		code := testLinkEnv(f.fakeLinkGateway, node.LinuxLayout, &restarts).register(context.Background(), append([]string{"-config", cfg, "-token-env", env}, args...), &out, &errb)
		return code, out.String(), errb.String()
	}
	code, _, errs := run()
	if code != 2 || len(f.regs) != 0 {
		t.Fatalf("without -accept-terms: exit %d, %d registrations", code, len(f.regs))
	}
	for _, want := range []string{"science work only", "earns no credit", "18 or older", "https://aisathome.org/about/terms/", "-accept-terms"} {
		if !strings.Contains(errs, want) {
			t.Errorf("without -accept-terms, the notice lacks %q: %s", want, errs)
		}
	}
	code, out, errs := run("-accept-terms")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errs)
	}
	if len(f.regs) != 1 || !f.regs[0].AcceptTerms || !strings.HasPrefix(f.regs[0].Agent, "aisathome-node/") {
		t.Fatalf("registrations %+v", f.regs)
	}
	if b, _ := os.ReadFile(env); string(b) != "AISATHOME_NODE_TOKEN="+anonTestToken+"\n" || mode(t, env) != 0o600 {
		t.Fatalf("token.env %q mode %v", b, mode(t, env))
	}
	var c map[string]any
	b, _ := os.ReadFile(cfg)
	json.Unmarshal(b, &c)
	if c["node_id"] != "a-0a0b0c0d" || c["token"] != nil {
		t.Fatalf("node.json %s", b)
	}
	if len(restarts) != 1 || !strings.Contains(out, "a-0a0b0c0d, without an account (science only)") || strings.Contains(out, anonTestToken) {
		t.Fatalf("restarts %v, output %s", restarts, out)
	}
	if code, _, errs := run("-accept-terms"); code != 1 || len(f.regs) != 1 || !strings.Contains(errs, "already runs without an account") {
		t.Fatalf("again: exit %d, %d registrations: %s", code, len(f.regs), errs)
	}
	// A refusal (a limit) is said as the gateway says it, and nothing is written.
	g := newFakeRegisterGateway(t, "too many nodes registered from this address today")
	dir2 := t.TempDir()
	cfg2 := filepath.Join(dir2, "node.json")
	os.WriteFile(cfg2, []byte(`{"gateway_url":"`+g.wsURL()+`"}`), 0o600)
	var errb bytes.Buffer
	if code := testLinkEnv(g.fakeLinkGateway, node.LinuxLayout, &restarts).register(context.Background(), []string{"-config", cfg2, "-accept-terms"}, &bytes.Buffer{}, &errb); code != 1 ||
		!strings.Contains(errb.String(), "too many nodes registered") {
		t.Fatalf("refused: exit %d: %s", code, errb.String())
	}
	if b, _ := os.ReadFile(cfg2); strings.Contains(string(b), "token") {
		t.Fatalf("a refused registration wrote %s", b)
	}
}

// link on a node without an account sends its token with the code request (so the gateway can delete its
// identity without an account once linked) and replaces it with the account's; an account's node sends none.
func TestLinkAnonymousSendsItsToken(t *testing.T) {
	for _, tc := range []struct {
		token, auth string
	}{{anonTestToken, "Bearer " + anonTestToken}, {"nt-n-00000000.old", ""}} {
		f := newFakeRegisterGateway(t, "")
		dir := t.TempDir()
		cfg, env := filepath.Join(dir, "node.json"), filepath.Join(dir, "token.env")
		os.WriteFile(cfg, []byte(`{"gateway_url":"`+f.wsURL()+`"}`), 0o640)
		os.WriteFile(env, []byte("AISATHOME_NODE_TOKEN="+tc.token+"\n"), 0o600)
		var restarts []restartCall
		var out, errb bytes.Buffer
		if code := testLinkEnv(f.fakeLinkGateway, node.LinuxLayout, &restarts).main(context.Background(), []string{"-config", cfg, "-no-start"}, &out, &errb); code != 0 {
			t.Fatalf("exit %d: %s", code, errb.String())
		}
		if f.auth != tc.auth {
			t.Errorf("token %s: Authorization %q, want %q", tc.token[:8], f.auth, tc.auth)
		}
		if b, _ := os.ReadFile(env); !strings.Contains(string(b), linkTestToken) {
			t.Errorf("token.env after the link: %q", b)
		}
		if anon := strings.Contains(out.String(), "runs without an account"); anon != (tc.auth != "") {
			t.Errorf("output %q", out.String())
		}
	}
}

func TestTokenKinds(t *testing.T) {
	for tok, want := range map[string]bool{anonTestToken: true, linkTestToken: false, "nt-a-0A0B0C0D.x": false, "nt-a-0a0b0c0.x": false, "pc-0123456789ab.x": false} {
		if got := node.Anonymous(tok); got != want {
			t.Errorf("Anonymous(%q) = %v", tok, got)
		}
	}
	if id, ok := node.TokenNodeID(anonTestToken); !ok || id != "a-0a0b0c0d" {
		t.Errorf("TokenNodeID: %q %v", id, ok)
	}
}
