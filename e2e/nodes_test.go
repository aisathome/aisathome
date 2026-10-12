// Self-registered nodes (decision 0007, backlog 15): node tokens on the account page, the
// cut-off (revoke, ban), the per-address cap, the share cap, node preferences and the
// trusted-only switch.
//
// SPDX-License-Identifier: Apache-2.0
package e2e

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/aisathome/aisathome/gateway"
	"github.com/aisathome/aisathome/node"
	"github.com/aisathome/aisathome/protocol"
)

var (
	nodeTokenRe = regexp.MustCompile(`nt-(n-[0-9a-f]{8})\.[A-Za-z0-9_-]{43}`)
	nodeOnceRe  = regexp.MustCompile(`action="/account/nodes"><input type="hidden" name="once" value="([0-9a-f]+)"`)
)

// addNode adds a node on the account page the way a browser does; it returns the token and
// node id ("" if none was shown) and the page.
func (a *acctRig) addNode(t *testing.T, session, name string) (token, id, page string) {
	t.Helper()
	_, page = a.get(t, "/account", session)
	once := ""
	if m := nodeOnceRe.FindStringSubmatch(page); m != nil {
		once = m[1]
	}
	_, page = a.post(t, "/account/nodes", session, "once="+once+"&name="+name)
	if m := nodeTokenRe.FindStringSubmatch(page); m != nil {
		return m[0], m[1], page
	}
	return "", "", page
}

// startTokenNode runs a node agent configured with only a token (no node_id), as the
// installer sets it up for a token from the account page.
func (r *rig) startTokenNode(ctx context.Context, token string, engine *httptest.Server, slots, ctxLen int) {
	r.startNodeCfg(ctx, node.Config{Token: token, Upstream: engine.URL,
		Models: []protocol.ModelInfo{{Name: "qwen3.8-27b-q4", Roles: []string{"deep"}, Slots: slots, Ctx: ctxLen}}})
}

// nodeRig is an accounts gateway with a trusted config token ("home-token") and no nodes yet.
func nodeRig(t *testing.T, mod func(*gateway.AccountsConfig)) *acctRig {
	return newAcctRigNodes(t, 100, mod, func(c *gateway.Config) {
		c.NodeTokens["home-token"] = gateway.NodeToken{Operator: "owner", Trust: "trusted"}
		c.QueueTimeoutSec = 3 // requests that may only go to an offline node fail sooner
	})
}

func ask(t *testing.T, r *rig, key, body string) (int, string) {
	t.Helper()
	resp := r.do(t, "POST", "/v1/chat/completions", key, body)
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp.StatusCode, string(b)
}

func TestNodeTokensOnAccountPage(t *testing.T) {
	a := nodeRig(t, nil)
	s, _ := a.signIn(t, 1)
	token, id, page := a.addNode(t, s, "my 3090")
	if token == "" || !strings.Contains(page, "shown only once") {
		t.Fatalf("no node token shown: %s", page)
	}
	a.store.Refresh()
	raw, _ := os.ReadFile(a.file)
	if strings.Contains(string(raw), token) || !strings.Contains(string(raw), id) {
		t.Fatal("the accounts file must hold the node id and only a hash of the token")
	}
	if _, page := a.get(t, "/account", s); nodeTokenRe.MatchString(page) || !strings.Contains(page, id) ||
		!strings.Contains(page, `badge offline`) || !strings.Contains(page, "my 3090") {
		t.Fatalf("page must list the node (offline) and never show the token again: %s", page)
	}
	// The node agent needs only the token: it takes its id from it.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a.startTokenNode(ctx, token, mockEngine(t, false), 2, 32768)
	a.waitNodes(t, 1)
	if _, page := a.get(t, "/account", s); !strings.Contains(page, `badge online`) || strings.Contains(page, "after a probation") || strings.Contains(page, "Probation (") {
		t.Fatalf("a new node serves at once, with no probation (the default): %s", page)
	}
	// SIGHUP-style reloads of the config keep account nodes (their tokens are not in it).
	a.g.Reload(gateway.Config{NodeTokens: map[string]gateway.NodeToken{"node-token": {Operator: "op1"}}})
	time.Sleep(100 * time.Millisecond)
	if a.g.NodeCount() != 1 {
		t.Fatal("a config reload dropped an account node")
	}
	// At most 3 per account: the form disappears, and the store refuses a fourth.
	a.addNode(t, s, "")
	a.addNode(t, s, "")
	if _, page := a.get(t, "/account", s); strings.Contains(page, `action="/account/nodes"`) {
		t.Fatal("the add form is still shown with 3 nodes")
	}
	if _, _, err := a.store.AddNode("gh:1", "", time.Now(), nil); err == nil {
		t.Fatal("a fourth node token was created")
	}
	// The network-wide limit: no waitlist, a clear message.
	s2, _ := a.signIn(t, 2)
	_, page = a.get(t, "/account", s2)
	once := nodeOnceRe.FindStringSubmatch(page)[1]
	a.store.Update(func(f *gateway.AccountsFile) error { f.Settings.MaxNodes = 3; return nil })
	if _, page := a.post(t, "/account/nodes", s2, "once="+once); !strings.Contains(page, "The network is full for now") ||
		nodeTokenRe.MatchString(page) {
		t.Fatalf("over max_nodes: %s", page)
	}
	waitFor(t, func() bool {
		_, page := a.get(t, "/account", s2)
		return strings.Contains(page, "network is full for now")
	})
	a.store.Update(func(f *gateway.AccountsFile) error {
		f.Settings.MaxNodes = 10
		f.Settings.NodesClosed = true
		return nil
	})
	waitFor(t, func() bool { _, page := a.get(t, "/account", s2); return strings.Contains(page, "not being accepted") })
	if _, _, err := a.store.AddNode("gh:2", "", time.Now(), nil); err == nil {
		t.Fatal("a node token was created while nodes are closed")
	}
	// Revoking on the page drops the node at once, and its token no longer connects.
	a.post(t, "/account/nodes/revoke", s, "id="+id)
	a.waitNodes(t, 0)
	time.Sleep(1500 * time.Millisecond) // the agent keeps retrying
	if a.g.NodeCount() != 0 {
		t.Fatal("a revoked token connected again")
	}
}

// Banning an account drops its nodes within seconds, and a request in flight on one fails
// over to another node.
func TestBanDropsNodesAndFailsOver(t *testing.T) {
	a := nodeRig(t, nil)
	s, _ := a.signIn(t, 1)
	token, id, _ := a.addNode(t, s, "")
	got := make(chan struct{}, 4)
	rawNodeWith(t, a.rig, id, token, func(func(protocol.Msg), func([]byte), protocol.Msg) { got <- struct{}{} }) // never answers
	a.waitNodes(t, 1)
	type result struct {
		code int
		body string
	}
	done := make(chan result, 1)
	go func() {
		req, _ := http.NewRequest("POST", a.gw.URL+"/v1/chat/completions", strings.NewReader(chatBody))
		req.Header.Set("Authorization", "Bearer user-key")
		resp, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
		if err != nil {
			done <- result{0, err.Error()}
			return
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		done <- result{resp.StatusCode, string(b)}
	}()
	select {
	case <-got:
	case <-time.After(5 * time.Second):
		t.Fatal("the request never reached the account's node")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a.startNode(ctx, "home", "home-token", mockEngine(t, false), 2)
	a.waitNodes(t, 2)
	start := time.Now()
	if err := a.store.SetStatus("gh:1", gateway.StatusBanned); err != nil { // as `accounts ban` does
		t.Fatal(err)
	}
	res := <-done
	if res.code != 200 || !strings.Contains(res.body, "Hello") {
		t.Fatalf("the request did not fail over: %d %s", res.code, res.body)
	}
	if d := time.Since(start); d > 6*time.Second {
		t.Fatalf("the cut-off took %v", d)
	}
	a.waitNodes(t, 1)
	if _, page := a.get(t, "/account", s); !strings.Contains(page, "Account closed") {
		t.Fatal("banned account page")
	}
}

// dialNode connects a protocol-level node from address xff (the gateway trusts
// X-Forwarded-For from loopback, as from Caddy).
func dialNode(t *testing.T, r *rig, id, token, xff string) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	h := http.Header{}
	h.Set("Authorization", "Bearer "+token)
	h.Set("X-Forwarded-For", xff)
	c, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(r.gw.URL, "http")+"/v1/node/connect", &websocket.DialOptions{HTTPHeader: h})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(protocol.Msg{Type: protocol.TypeHello, NodeID: id, Protocol: protocol.Version,
		Models: []protocol.ModelInfo{{Name: "qwen3.8-27b-q4", Roles: []string{"deep"}, Slots: 1}}})
	c.Write(ctx, websocket.MessageText, b)
	go func() {
		for {
			if _, _, err := c.Read(ctx); err != nil {
				return
			}
		}
	}()
}

func TestNodesPerAddress(t *testing.T) {
	a := nodeRig(t, nil)
	var tokens, ids []string
	for _, acct := range []int{1, 2} {
		s, _ := a.signIn(t, acct)
		for i := 0; i < 2; i++ {
			tok, id, _ := a.addNode(t, s, "")
			tokens, ids = append(tokens, tok), append(ids, id)
		}
	}
	for i := 0; i < 3; i++ {
		dialNode(t, a.rig, ids[i], tokens[i], "203.0.113.7")
	}
	a.waitNodes(t, 3)
	dialNode(t, a.rig, ids[3], tokens[3], "203.0.113.7") // a fourth from the same address
	time.Sleep(300 * time.Millisecond)
	if n := a.g.NodeCount(); n != 3 {
		t.Fatalf("a fourth node from one address was accepted (%d nodes)", n)
	}
	dialNode(t, a.rig, ids[3], tokens[3], "198.51.100.9")
	a.waitNodes(t, 4)
}

// No untrusted operator gets more than about a third of the requests while other nodes
// are free.
func TestShareCap(t *testing.T) {
	var home, stranger atomic.Int32
	r := trustRig(t, &home, &stranger)
	for i := 0; i < 30; i++ {
		if code, _ := ask(t, r, "user-key", chatBody); code != 200 {
			t.Fatalf("request %d: %d", i, code)
		}
	}
	if s := stranger.Load(); s < 8 || s > 11 {
		t.Fatalf("the volunteer served %d of 30 requests; want about a third", s)
	}
}

// An account's own requests prefer its own nodes (default), may use only them, or any node;
// the usage log marks answers an account served itself.
func TestNodePreference(t *testing.T) {
	var own, home atomic.Int32
	a := nodeRig(t, nil)
	s, _ := a.signIn(t, 1)
	token, _, _ := a.addNode(t, s, "")
	_, page := a.createKey(t, s)
	key := keyRe.FindString(page)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a.startNode(ctx, "home", "home-token", countEngine(t, &home), 4)
	ownCtx, stopOwn := context.WithCancel(ctx)
	a.startTokenNode(ownCtx, token, countEngine(t, &own), 4, 32768)
	a.waitNodes(t, 2)
	for i := 0; i < 6; i++ {
		ask(t, a.rig, key, chatBody)
	}
	chat := func() int {
		req, _ := http.NewRequest("POST", a.front.URL+"/v1/chat/completions", strings.NewReader(chatBody))
		req.Header.Set("Authorization", "Bearer owui-key")
		req.Header.Set("X-OpenWebUI-User-Email", "gh-1@users.invalid")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		return resp.StatusCode
	}
	chat()
	if own.Load() != 7 || home.Load() != 0 {
		t.Fatalf("prefer own nodes: own %d, home %d", own.Load(), home.Load())
	}
	self := 0
	for _, u := range waitUsage(t, a.rig, 7) {
		if u["self_served"] == true {
			self++
		}
	}
	if self != 7 {
		t.Fatalf("usage log: %d of 7 marked self_served", self)
	}
	a.post(t, "/account/nodes/pref", s, "pref=any")
	for i := 0; i < 6; i++ {
		ask(t, a.rig, key, chatBody)
	}
	if home.Load() == 0 {
		t.Fatal("pref any: the account's own node took everything")
	}
	// With its own node gone: "prefer" falls back to others, "only" does not.
	stopOwn()
	a.waitNodes(t, 1)
	a.post(t, "/account/nodes/pref", s, "pref=")
	if code, _ := ask(t, a.rig, key, chatBody); code != 200 {
		t.Fatalf("prefer own, own offline: %d", code)
	}
	a.post(t, "/account/nodes/pref", s, "pref=only")
	if code, body := ask(t, a.rig, key, chatBody); code != 503 || !strings.Contains(body, "only your own nodes") {
		t.Fatalf("only own, own offline: %d %s", code, body)
	}
	if c := chat(); c != 503 {
		t.Fatalf("the preference applies to the chat too: %d", c)
	}
}

// The operator's emergency switch sends everything to trusted nodes.
func TestRoutingTrustedOnly(t *testing.T) {
	var home, stranger atomic.Int32
	a := nodeRig(t, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a.startNode(ctx, "home", "home-token", countEngine(t, &home), 4)
	a.startNode(ctx, "stranger", "node-token", countEngine(t, &stranger), 4)
	a.waitNodes(t, 2)
	a.store.Update(func(f *gateway.AccountsFile) error { f.Settings.Routing = gateway.RoutingTrustedOnly; return nil })
	time.Sleep(2500 * time.Millisecond) // the gateway rereads the file every 2 s
	for i := 0; i < 6; i++ {
		ask(t, a.rig, "user-key", chatBody)
	}
	if stranger.Load() != 0 {
		t.Fatalf("trusted-only: %d requests reached an untrusted node", stranger.Load())
	}
	a.store.Update(func(f *gateway.AccountsFile) error { f.Settings.Routing = ""; return nil })
	waitFor(t, func() bool { ask(t, a.rig, "user-key", chatBody); return stranger.Load() > 0 })
}

// The account page tells a volunteer's own pause (local node UI) from a hold by a guard.
func TestNodeStatusPausedByVolunteer(t *testing.T) {
	a := nodeRig(t, nil)
	s, _ := a.signIn(t, 1)
	token, id, _ := a.addNode(t, s, "")
	c := rawNodeWith(t, a.rig, id, token, func(func(protocol.Msg), func([]byte), protocol.Msg) {})
	a.waitNodes(t, 1)
	b, _ := json.Marshal(protocol.Msg{Type: protocol.TypeHeartbeat, Paused: true})
	c.Write(context.Background(), websocket.MessageText, b)
	waitFor(t, func() bool { _, page := a.get(t, "/account", s); return strings.Contains(page, "badge paused") })
}
