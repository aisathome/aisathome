// The trusted front end (Open WebUI): its key works only on the private listener, its users
// and signed-in accounts keep its restrictions, and only the end user is charged.
//
// SPDX-License-Identifier: Apache-2.0
package e2e

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/aisathome/aisathome/gateway"
)

// A forward_users key works only on the private front-end listener: leaked, it can neither
// speak for an account nor be used as an ordinary key on the public API.
func TestFrontEndKeyOnlyOnPrivateListener(t *testing.T) {
	a := newAcctRig(t, 100, nil)
	a.signIn(t, 7)
	for _, c := range []struct {
		base, method, path string
		want               int
	}{
		{a.gw.URL, "POST", "/v1/chat/completions", 401},
		{a.gw.URL, "GET", "/v1/models", 401},
		{a.front.URL, "POST", "/v1/chat/completions", 200},
		{a.front.URL, "GET", "/v1/models", 200},
	} {
		req, _ := http.NewRequest(c.method, c.base+c.path, strings.NewReader(chatBody))
		req.Header.Set("Authorization", "Bearer owui-key")
		req.Header.Set("X-OpenWebUI-User-Email", "gh-7@users.invalid")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode != c.want {
			t.Fatalf("%s %s on %s: %d, want %d", c.method, c.path, map[bool]string{true: "public", false: "front end"}[c.base == a.gw.URL], resp.StatusCode, c.want)
		}
	}
}

// An account in the chat keeps the front-end key's routing restrictions: a front end that
// may use only trusted nodes does not reach volunteers through its users' accounts.
func TestFrontEndRestrictionsKeptForAccounts(t *testing.T) {
	a := newAcctRigWith(t, 100, nil, func(c *gateway.Config) {
		c.NodeTokens["home-token"] = gateway.NodeToken{Operator: "owner", Trust: "trusted"}
		c.Keys["owui-key"] = gateway.KeyInfo{User: "owui", ForwardUsers: true, MinTrust: "trusted"}
	})
	var home atomic.Int32
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a.startNode(ctx, "home", "home-token", countEngine(t, &home), 4)
	a.waitNodes(t, 2) // plus the rig's untrusted node
	a.signIn(t, 7)
	for i := 0; i < 6; i++ {
		req, _ := http.NewRequest("POST", a.front.URL+"/v1/chat/completions", strings.NewReader(chatBody))
		req.Header.Set("Authorization", "Bearer owui-key")
		req.Header.Set("X-OpenWebUI-User-Email", "gh-7@users.invalid")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatalf("request %d: %d", i, resp.StatusCode)
		}
	}
	if n := home.Load(); n != 6 {
		t.Fatalf("only %d of 6 account requests stayed on the trusted node", n)
	}
}

// A front-end key restricted to one operator keeps that restriction for signed-in accounts,
// also when an account uses only its own nodes: both apply, so a conflict leaves no node.
func TestFrontEndOperatorAndOwnNodesOnly(t *testing.T) {
	var own, home atomic.Int32
	a := newAcctRigNodes(t, 100, nil, func(c *gateway.Config) {
		c.NodeTokens["home-token"] = gateway.NodeToken{Operator: "owner", Trust: "trusted"}
		c.Keys["owui-owner"] = gateway.KeyInfo{User: "owui-owner", ForwardUsers: true, OnlyOperator: "owner"}
		c.QueueTimeoutSec = 2
	})
	s, _ := a.signIn(t, 1)
	token, _, _ := a.addNode(t, s, "")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a.startNode(ctx, "home", "home-token", countEngine(t, &home), 4)
	a.startTokenNode(ctx, token, countEngine(t, &own), 4, 32768)
	a.waitNodes(t, 2)
	chat := func() int {
		req, _ := http.NewRequest("POST", a.front.URL+"/v1/chat/completions", strings.NewReader(chatBody))
		req.Header.Set("Authorization", "Bearer owui-owner")
		req.Header.Set("X-OpenWebUI-User-Email", "gh-1@users.invalid")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if c := chat(); c != 200 || home.Load() != 1 || own.Load() != 0 {
		t.Fatalf("prefer own (default): %d, home %d, own %d", c, home.Load(), own.Load())
	}
	a.post(t, "/account/nodes/pref", s, "pref=only")
	if c := chat(); c != 503 || own.Load() != 0 || home.Load() != 1 {
		t.Fatalf("only own nodes behind a front end for operator owner: %d, home %d, own %d", c, home.Load(), own.Load())
	}
}

// Only the end user is charged behind a front end: a node inflating usage for one user must
// not use up the shared front-end key for everyone.
func TestFrontEndKeyNotCharged(t *testing.T) {
	r := newRigCfg(t, func(c *gateway.Config) {
		c.Keys["owui-key"] = gateway.KeyInfo{User: "owui", ForwardUsers: true, TokensPerDay: 50,
			UserLimits: &gateway.UserLimits{TokensPerDay: 100000}}
	}, liarEngine())
	for i, uid := range []string{"u1", "u2", "u3"} {
		req, _ := http.NewRequest("POST", r.front.URL+"/v1/chat/completions", strings.NewReader(chatBody))
		req.Header.Set("Authorization", "Bearer owui-key")
		req.Header.Set("X-OpenWebUI-User-Id", uid)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatalf("user %d: %d; the shared front-end key was drained", i, resp.StatusCode)
		}
		waitUsage(t, r, i+1)
	}
}
