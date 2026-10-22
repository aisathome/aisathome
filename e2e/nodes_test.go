// Self-registered nodes (decision 0007, backlog 15): node tokens on the account page, the
// cut-off (revoke, ban), the per-address cap, the share cap, node preferences and the
// trusted-only switch; and the node connection itself: bound ids, revocation while
// connecting, frame limits, stalled reconnects.
//
// SPDX-License-Identifier: Apache-2.0
package e2e

import (
	"context"
	"encoding/json"
	"errors"
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
	if token == "" || !strings.Contains(page, "You will not see this again") {
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
	// At most max_nodes_per_account (3 here): the form gives way to a message that says why,
	// and the store refuses a fourth.
	a.store.Update(func(f *gateway.AccountsFile) error { f.Settings.MaxNodesPerAccount = 3; return nil })
	a.addNode(t, s, "")
	a.addNode(t, s, "")
	waitFor(t, func() bool {
		_, page := a.get(t, "/account", s)
		return strings.Contains(page, "You have 3 of 3 nodes. Revoke one to add another.") &&
			!strings.Contains(page, `action="/account/nodes"`)
	})
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
// are free. These are short one-off requests, so each is a conversation of its own
// (TestStickySessionShareCap covers a session's many turns).
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

// A token revoked between the WebSocket handshake and hello must not register the node.
func TestRevokedWhileConnecting(t *testing.T) {
	r := newRigCfg(t, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h := http.Header{}
	h.Set("Authorization", "Bearer node-token")
	c, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(r.gw.URL, "http")+"/v1/node/connect", &websocket.DialOptions{HTTPHeader: h})
	if err != nil {
		t.Fatal(err)
	}
	defer c.CloseNow()
	time.Sleep(100 * time.Millisecond) // the gateway has read the token and waits for hello
	r.g.Reload(gateway.Config{APIKeys: map[string]string{"user-key": "alice"}, NodeTokens: map[string]gateway.NodeToken{}})
	b, _ := json.Marshal(protocol.Msg{Type: protocol.TypeHello, NodeID: "late", Protocol: protocol.Version,
		Models: []protocol.ModelInfo{{Name: "qwen3.8-27b-q4", Roles: []string{"deep"}, Slots: 2}}})
	c.Write(ctx, websocket.MessageText, b)
	time.Sleep(300 * time.Millisecond)
	if n := r.g.NodeCount(); n != 0 {
		t.Fatalf("a node with a revoked token registered (%d nodes)", n)
	}
}

// A node that reconnects while its old connection ignores the close handshake does not stall
// the gateway: the old connection is closed outside the shared lock.
func TestStalledReconnectDoesNotBlockGateway(t *testing.T) {
	r := newRigCfg(t, nil)
	hello := func() (*websocket.Conn, context.CancelFunc) {
		ctx, cancel := context.WithCancel(context.Background())
		h := http.Header{}
		h.Set("Authorization", "Bearer node-token")
		c, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(r.gw.URL, "http")+"/v1/node/connect", &websocket.DialOptions{HTTPHeader: h})
		if err != nil {
			t.Fatal(err)
		}
		b, _ := json.Marshal(protocol.Msg{Type: protocol.TypeHello, NodeID: "same", Protocol: protocol.Version,
			Models: []protocol.ModelInfo{{Name: "qwen3.8-27b-q4", Roles: []string{"deep"}, Slots: 2}}})
		c.Write(ctx, websocket.MessageText, b)
		return c, cancel
	}
	_, cancel1 := hello() // never reads again: its close handshake stalls
	defer cancel1()
	r.waitNodes(t, 1)
	_, cancel2 := hello()
	defer cancel2()
	time.Sleep(300 * time.Millisecond)
	start := time.Now()
	resp := r.do(t, "GET", "/v1/models", "user-key", "")
	resp.Body.Close()
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("the gateway was blocked for %s while a replaced node stalled", d)
	}
}

func TestOversizedChunkDropsNode(t *testing.T) {
	r := newRigCfg(t, nil)
	rawNodeWith(t, r, "big", "node-token", func(write func(protocol.Msg), _ func([]byte), m protocol.Msg) {
		write(chunkMsg(m.ReqID, strings.Repeat("x", 200<<10)))
	})
	r.waitNodes(t, 1)
	start := time.Now()
	resp := r.do(t, "POST", "/v1/chat/completions", "user-key", chatBody)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode == 200 && time.Since(start) < time.Second {
		t.Fatalf("a 200 KB chunk was accepted")
	}
	r.waitNodes(t, 0)
}

func TestMalformedFrameDoesNotDropNode(t *testing.T) {
	r := newRigCfg(t, nil)
	rawNodeWith(t, r, "messy", "node-token", func(write func(protocol.Msg), raw func([]byte), m protocol.Msg) {
		raw(nil)                 // empty frame
		raw([]byte("{not json")) // garbage
		write(chunkMsg(m.ReqID, "ok"))
		write(protocol.Msg{Type: protocol.TypeDone, ReqID: m.ReqID, Usage: &protocol.Usage{PromptTokens: 3, CompletionTokens: 1}})
	})
	r.waitNodes(t, 1)
	resp := r.do(t, "POST", "/v1/chat/completions", "user-key", chatBody)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || r.g.NodeCount() != 1 {
		t.Fatalf("status %d, nodes %d: a bad frame must be skipped, not drop the node", resp.StatusCode, r.g.NodeCount())
	}
}

func TestBoundIDCannotBeSquatted(t *testing.T) {
	r := newRigCfg(t, func(c *gateway.Config) {
		c.NodeTokens["home-token"] = gateway.NodeToken{Operator: "owner", NodeID: "home-1", Trust: "trusted"}
		c.NodeTokens["other-token"] = gateway.NodeToken{Operator: "mallory"}
	})
	rawNodeWith(t, r, "home-1", "other-token", func(func(protocol.Msg), func([]byte), protocol.Msg) {})
	time.Sleep(300 * time.Millisecond)
	if r.g.NodeCount() != 0 {
		t.Fatal("another token squatted a bound node id")
	}
	rawNodeWith(t, r, "home-1", "home-token", func(func(protocol.Msg), func([]byte), protocol.Msg) {})
	r.waitNodes(t, 1)
}

// A node id bound to another token is refused before the node is registered, with the reason.
func TestSquatterIsToldIDIsReserved(t *testing.T) {
	r := newRigCfg(t, func(c *gateway.Config) {
		c.NodeTokens["home-token"] = gateway.NodeToken{Operator: "owner", NodeID: "home-1"}
		c.NodeTokens["other-token"] = gateway.NodeToken{Operator: "mallory"}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	h := http.Header{}
	h.Set("Authorization", "Bearer other-token")
	c, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(r.gw.URL, "http")+"/v1/node/connect", &websocket.DialOptions{HTTPHeader: h})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(protocol.Msg{Type: protocol.TypeHello, NodeID: "home-1", Protocol: protocol.Version,
		Models: []protocol.ModelInfo{{Name: "qwen3.8-27b-q4", Slots: 1}}})
	c.Write(ctx, websocket.MessageText, b)
	_, _, err = c.Read(ctx)
	var ce websocket.CloseError
	if !errors.As(err, &ce) || ce.Code != websocket.StatusPolicyViolation || ce.Reason != "node id is reserved" {
		t.Fatalf("got %v, want a close with \"node id is reserved\"", err)
	}
}

// The gateway records the version a node reports: it logs it on connect and shows it on the
// account page while the node is connected. A version that is not a plain name is not kept.
func TestNodeVersionLoggedAndShown(t *testing.T) {
	logs := &syncBuf{}
	logSink = logs
	defer func() { logSink = io.Discard }()
	a := nodeRig(t, nil)
	s, _ := a.signIn(t, 1)
	token, id, _ := a.addNode(t, s, "")
	if _, page := a.get(t, "/account", s); !strings.Contains(page, "<th>Version</th>") || strings.Contains(page, "<code>dev</code>") {
		t.Fatalf("an offline node shows no version: %s", page)
	}
	ctx, cancel := context.WithCancel(context.Background())
	a.startTokenNode(ctx, token, mockEngine(t, false), 2, 32768) // a test build reports aisathome-node/dev
	a.waitNodes(t, 1)
	if _, page := a.get(t, "/account", s); !strings.Contains(page, "<code>dev</code>") {
		t.Fatalf("the connected node's version is not shown: %s", page)
	}
	if !regexp.MustCompile(`msg="node connected" node=` + id + ` .*version=aisathome-node/dev`).MatchString(logs.String()) {
		t.Fatalf("version not logged:\n%s", logs.String())
	}
	cancel()
	a.waitNodes(t, 0)

	// A raw node announcing markup as its version: logged as unknown, not shown.
	h := http.Header{}
	h.Set("Authorization", "Bearer "+token)
	c, _, err := websocket.Dial(context.Background(), "ws"+strings.TrimPrefix(a.gw.URL, "http")+"/v1/node/connect",
		&websocket.DialOptions{HTTPHeader: h})
	if err != nil {
		t.Fatal(err)
	}
	defer c.CloseNow()
	b, _ := json.Marshal(protocol.Msg{Type: protocol.TypeHello, NodeID: id, Protocol: protocol.Version, Agent: "<b>x</b>",
		Models: []protocol.ModelInfo{{Name: "qwen3.8-27b-q4", Roles: []string{"deep"}, Slots: 1}}})
	c.Write(context.Background(), websocket.MessageText, b)
	a.waitNodes(t, 1)
	if _, page := a.get(t, "/account", s); strings.Contains(page, "<b>x</b>") || strings.Contains(page, "&lt;b&gt;") {
		t.Fatalf("an invalid version was shown: %s", page)
	}
	waitFor(t, func() bool { return strings.Contains(logs.String(), "version=unknown") })
}

// Two clients running with one node token and id replace each other's connection in turn.
// The gateway warns once it has happened a few times in a row; an ordinary reconnect does not.
func TestReplacingConnectionsWarn(t *testing.T) {
	logs := &syncBuf{}
	logSink = logs
	defer func() { logSink = io.Discard }()
	r := newRig(t)
	const warning = "node connections keep replacing each other"
	connect := func(n int) {
		rawNodeAll(t, r, "twice", "node-token", func(func(protocol.Msg), func([]byte), protocol.Msg) {})
		waitFor(t, func() bool { return strings.Count(logs.String(), `msg="node connected" node=twice `) == n })
	}
	connect(1)
	connect(2) // one replacement: an ordinary reconnect
	connect(3)
	if strings.Contains(logs.String(), warning) {
		t.Fatalf("warned after 2 replacements:\n%s", logs.String())
	}
	connect(4)
	if strings.Count(logs.String(), warning) != 1 || !strings.Contains(logs.String(), "replaced=3 same_address=true") {
		t.Fatalf("want one warning after 3 quick replacements:\n%s", logs.String())
	}
	connect(5)
	connect(6)
	connect(7)
	if strings.Count(logs.String(), warning) != 1 {
		t.Fatalf("the warning is not rate-limited:\n%s", logs.String())
	}
}

// Without settings in the accounts file: 10 node tokens per account, 25 over all accounts
// (owner, 2026-10-22); the page counts against the actual limit.
func TestNodeLimitDefaults(t *testing.T) {
	a := nodeRig(t, nil)
	s, _ := a.signIn(t, 1)
	for i := 0; i < 9; i++ {
		if _, _, err := a.store.AddNode("gh:1", "", time.Now(), nil); err != nil {
			t.Fatalf("node %d of an account: %v", i+1, err)
		}
	}
	waitFor(t, func() bool { _, page := a.get(t, "/account", s); return strings.Count(page, "badge offline") == 9 })
	if _, page := a.get(t, "/account", s); !strings.Contains(page, `action="/account/nodes"`) || strings.Contains(page, "You have") {
		t.Fatalf("the add form must stay with 9 of 10 nodes: %s", page)
	}
	if token, _, _ := a.addNode(t, s, ""); token == "" {
		t.Fatal("the tenth node of an account was refused")
	}
	waitFor(t, func() bool {
		_, page := a.get(t, "/account", s)
		return strings.Contains(page, "You have 10 of 10 nodes. Revoke one to add another.")
	})
	if _, _, err := a.store.AddNode("gh:1", "", time.Now(), nil); err == nil {
		t.Fatal("an eleventh node token was created")
	}
	for id, n := range map[string]int{"gh:2": 10, "gh:3": 5} {
		a.store.SignIn(id, time.Now())
		for i := 0; i < n; i++ {
			if _, _, err := a.store.AddNode(id, "", time.Now(), nil); err != nil {
				t.Fatalf("%s node %d (network total under 25): %v", id, i+1, err)
			}
		}
	}
	a.store.SignIn("gh:4", time.Now())
	if _, _, err := a.store.AddNode("gh:4", "", time.Now(), nil); err == nil || !strings.Contains(err.Error(), "network is full") {
		t.Fatalf("a 26th node token over all accounts: %v", err)
	}
}
