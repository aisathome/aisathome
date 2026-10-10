package e2e

// Fourth review (security, 2026-10-10): account forms, front-end key scope and policy,
// request bodies, node reconnects and restoration buffers.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aisathome/aisathome/gateway"
	"github.com/aisathome/aisathome/protocol"
	"github.com/coder/websocket"
)

// Account forms take only short URL-encoded bodies: no multipart (which may spill to disk)
// and nothing large, whether or not the session is valid.
func TestAccountFormsRejectMultipartAndLargeBodies(t *testing.T) {
	a := newAcctRig(t, 100, nil)
	s, _ := a.signIn(t, 7)
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, _ := mw.CreateFormFile("upload", "big.bin")
	fw.Write(bytes.Repeat([]byte("x"), 64<<10))
	mw.Close()
	for _, path := range []string{"/account/keys", "/account/keys/revoke", "/account/logout"} {
		req, _ := http.NewRequest("POST", a.gw.URL+path, bytes.NewReader(buf.Bytes()))
		req.Header.Set("Content-Type", mw.FormDataContentType())
		req.Header.Set("Cookie", s)
		req.Header.Set("Origin", "https://site.test")
		resp, err := noRedirect.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnsupportedMediaType {
			t.Fatalf("%s multipart: %d", path, resp.StatusCode)
		}
	}
	if resp, _ := a.post(t, "/account/keys", s, "once="+strings.Repeat("a", 20<<10)); resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("20 KB form: %d", resp.StatusCode)
	}
	if resp, page := a.createKey(t, s); resp.StatusCode != 200 || !keyRe.MatchString(page) { // the real form still works
		t.Fatalf("normal key form: %d", resp.StatusCode)
	}
}

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

// slowUpload starts a request that declares a body of size bytes and sends only part of it.
func slowUpload(t *testing.T, base, key string, size int) net.Conn {
	t.Helper()
	c, err := net.Dial("tcp", strings.TrimPrefix(base, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	fmt.Fprintf(c, "POST /v1/chat/completions HTTP/1.1\r\nHost: x\r\nAuthorization: Bearer %s\r\nContent-Type: application/json\r\nContent-Length: %d\r\n\r\n{\"messages\":", key, size)
	return c
}

// A trickled upload holds no concurrency slot: the slot is taken after the body is read.
func TestSlowUploadHoldsNoSlot(t *testing.T) {
	r := newRigCfg(t, func(c *gateway.Config) {
		c.Keys["one-key"] = gateway.KeyInfo{User: "dave", MaxConcurrent: 1}
	}, mockEngine(t, false))
	slowUpload(t, r.gw.URL, "one-key", 1000)
	time.Sleep(200 * time.Millisecond)
	resp := r.do(t, "POST", "/v1/chat/completions", "one-key", chatBody)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("second request while an upload trickles: %d", resp.StatusCode)
	}
}

// Request bodies share a memory budget: uploads in progress reserve their declared size,
// and once the budget is reserved further requests are turned away until they finish.
func TestRequestBodyBudget(t *testing.T) {
	r := newRigCfg(t, func(c *gateway.Config) {
		for i := 0; i < 8; i++ {
			c.Keys[fmt.Sprintf("up-%d", i)] = gateway.KeyInfo{User: fmt.Sprintf("up%d", i)}
		}
	}, mockEngine(t, false))
	conns := []net.Conn{}
	for i := 0; i < 8; i++ { // 8 users × 8 MiB fill the 64 MiB budget
		conns = append(conns, slowUpload(t, r.gw.URL, fmt.Sprintf("up-%d", i), 8<<20))
	}
	time.Sleep(300 * time.Millisecond)
	resp := r.do(t, "POST", "/v1/chat/completions", "user-key", chatBody)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("with the body budget reserved: %d, want 503", resp.StatusCode)
	}
	for _, c := range conns {
		c.Close()
	}
	waitFor(t, func() bool {
		resp := r.do(t, "POST", "/v1/chat/completions", "user-key", chatBody)
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		return resp.StatusCode == 200
	})
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

// With secrets being restored, only choice 0 is passed on: a node inventing choice indexes
// gets neither extra held-back buffers nor extra answers to the user.
func TestRestorerIgnoresInventedChoices(t *testing.T) {
	r := newRigCfg(t, nil)
	rawNodeWith(t, r, "inventive", "node-token", func(write func(protocol.Msg), _ func([]byte), m protocol.Msg) {
		for i := 0; i < 50; i++ {
			b, _ := json.Marshal(map[string]any{"choices": []any{
				map[string]any{"index": 0, "delta": map[string]any{"content": "a"}},
				map[string]any{"index": i + 1, "delta": map[string]any{"content": "junk"}}}})
			write(protocol.Msg{Type: protocol.TypeChunk, ReqID: m.ReqID, Data: b})
		}
		write(protocol.Msg{Type: protocol.TypeDone, ReqID: m.ReqID, Usage: &protocol.Usage{PromptTokens: 3, CompletionTokens: 1}})
	})
	r.waitNodes(t, 1)
	body, _ := json.Marshal(map[string]any{"model": "deep", "stream": true,
		"messages": []any{map[string]any{"role": "user", "content": "use " + secret}}})
	resp := r.do(t, "POST", "/v1/chat/completions", "restore-key", string(body))
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || strings.Contains(string(raw), "junk") || !strings.Contains(string(raw), `"a"`) {
		t.Fatalf("status %d; invented choices must be dropped: %.300s", resp.StatusCode, raw)
	}
}

// One user's stalled uploads cannot use up the shared budget: each user may have at most one
// maximum-size body in flight, so other users are still served (fifth review).
func TestOneUserCannotFillBodyBudget(t *testing.T) {
	r := newRigCfg(t, func(c *gateway.Config) {
		c.Keys["tight-key"] = gateway.KeyInfo{User: "mallory", RPM: 1, MaxConcurrent: 1}
	}, mockEngine(t, false))
	for i := 0; i < 8; i++ {
		slowUpload(t, r.gw.URL, "tight-key", 8<<20)
	}
	time.Sleep(300 * time.Millisecond)
	resp := r.do(t, "POST", "/v1/chat/completions", "user-key", chatBody)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("another user while one user stalls 8 uploads: %d", resp.StatusCode)
	}
	resp = r.do(t, "POST", "/v1/chat/completions", "tight-key", chatBody)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("the stalling user's next request: %d, want 429", resp.StatusCode)
	}
}
