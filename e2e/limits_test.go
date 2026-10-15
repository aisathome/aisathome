// Limits on what one request may cost or hold: daily budgets (tool-call output counts too),
// the max_tokens cap, and request bodies (size, the shared upload budget, slow uploads).
//
// SPDX-License-Identifier: Apache-2.0
package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aisathome/aisathome/gateway"
)

// longToolEngine answers with one long tool call only, and reports that output honestly.
func longToolEngine() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if health(w, r) {
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		args := `{"path":"notes.txt","text":"` + strings.Repeat("lorem ipsum ", 300) + `"}`
		first, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"index": 0, "delta": map[string]any{
			"tool_calls": []any{map[string]any{"index": 0, "id": "c1", "type": "function", "function": map[string]any{"name": "write", "arguments": ""}}}}}}})
		fmt.Fprintf(w, "data: %s\n\n", first)
		for i := 0; i < len(args); i += 40 {
			b, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"index": 0, "delta": map[string]any{
				"tool_calls": []any{map[string]any{"index": 0, "function": map[string]any{"arguments": args[i:min(i+40, len(args))]}}}}}}})
			fmt.Fprintf(w, "data: %s\n\n", b)
		}
		fmt.Fprint(w, "data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\n")
		fmt.Fprint(w, "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":900}}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
}

// Answers from the account's own nodes cost nothing from its daily budget, and an account over
// its budget can still use its own nodes, and only those.
func TestOwnNodesOutsideTheBudget(t *testing.T) {
	a := nodeRig(t, func(c *gateway.AccountsConfig) { c.Limits.TokensPerDay = 40 })
	s, _ := a.signIn(t, 1)
	token, nodeID, _ := a.addNode(t, s, "my 3090")
	_, page := a.createKey(t, s)
	key := keyRe.FindString(page)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Only its own node: far more requests than a budget of 40 would allow.
	ownCtx, stopOwn := context.WithCancel(ctx)
	a.startTokenNode(ownCtx, token, mockEngine(t, false), 2, 32768)
	a.waitNodes(t, 1)
	for i := 0; i < 15; i++ {
		if c, b := ask(t, a.rig, key, chatBody); c != 200 {
			t.Fatalf("request %d on its own node: %d %s", i, c, b)
		}
	}
	served := len(waitUsage(t, a.rig, 15))

	// Only a stranger's node (the maintainer's): the budget runs out.
	stopOwn()
	a.waitNodes(t, 0)
	a.startNode(ctx, "home", "home-token", mockEngine(t, false), 2)
	a.waitNodes(t, 1)
	charged := 0
	for ; charged < 20; charged++ {
		if c, _ := ask(t, a.rig, key, chatBody); c != 200 {
			break
		}
		waitUsage(t, a.rig, served+charged+1)
	}
	if c, _ := ask(t, a.rig, key, chatBody); c != 429 || charged == 0 || charged == 20 {
		t.Fatalf("stranger's node: %d answered before the budget ran out, then %d; want some, then 429", charged, c)
	}

	// Its own node is back: over budget, it is served again, by its own node only.
	a.startTokenNode(ctx, token, mockEngine(t, false), 2, 32768)
	a.waitNodes(t, 2)
	for i := 0; i < 4; i++ {
		if c, b := ask(t, a.rig, key, chatBody); c != 200 {
			t.Fatalf("over budget with its own node online: %d %s", c, b)
		}
	}
	for _, u := range waitUsage(t, a.rig, served+charged+4)[served+charged:] {
		if u["node"] != nodeID || u["self_served"] != true {
			t.Fatalf("over-budget request not served by its own node: %v", u)
		}
	}
}

// Tool-call arguments are generated output and count against the daily budget.
func TestToolCallsChargeTheBudget(t *testing.T) {
	r := newRigCfg(t, func(c *gateway.Config) {
		c.Keys["tool-key"] = gateway.KeyInfo{User: "dev", TokensPerDay: 500}
	}, longToolEngine())
	ask := func() int {
		resp := r.do(t, "POST", "/v1/chat/completions", "tool-key", chatBody)
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		return resp.StatusCode
	}
	if c := ask(); c != 200 {
		t.Fatalf("first request: %d", c)
	}
	waitUsage(t, r, 1)
	if c := ask(); c != 429 {
		t.Fatalf("a 900-token tool call should use up a 500-token budget; second request got %d", c)
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

func TestMaxTokensCapped(t *testing.T) {
	var last atomic.Value
	r := newRig(t, captureEngine(t, &last))
	resp := r.do(t, "POST", "/v1/chat/completions", "user-key",
		`{"model":"deep","max_tokens":1000000,"messages":[{"role":"user","content":"hi"}]}`)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if got := last.Load().(map[string]any)["max_tokens"].(float64); got > 32768 {
		t.Fatalf("node was asked for %v tokens", got)
	}
}
