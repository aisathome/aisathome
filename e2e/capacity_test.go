// Tests for waiting on capacity, metering of unfinished requests, node-bound tokens and
// config reload.
//
// SPDX-License-Identifier: Apache-2.0
package e2e

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aisathome/aisathome/gateway"
)

const chatBody = `{"model":"deep","stream":true,"messages":[{"role":"user","content":"hi"}]}`

// More requests than slots: the extra ones wait for a free slot instead of failing.
func TestQueueWaitsForSlot(t *testing.T) {
	r := newRigCfg(t, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	engineMax.Store(0)
	r.startNode(ctx, "one-slot", "node-token", slowEngine(t, 3, 50*time.Millisecond), 1)
	r.waitNodes(t, 1)

	var wg sync.WaitGroup
	codes := make([]int, 3)
	for i := range codes {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			resp := r.do(t, "POST", "/v1/chat/completions", "user-key", chatBody)
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			codes[i] = resp.StatusCode
		}(i)
	}
	wg.Wait()
	for i, c := range codes {
		if c != 200 {
			t.Fatalf("request %d: got %d, want 200 (all should queue for the single slot): %v", i, c, codes)
		}
	}
	if m := engineMax.Load(); m != 1 {
		t.Fatalf("the 1-slot node served %d requests at once; the gateway oversubscribed it", m)
	}
}

// A known model whose node is briefly gone (e.g. reconnecting): the request waits for it.
func TestQueueWaitsForNodeToReconnect(t *testing.T) {
	r := newRigCfg(t, nil)
	engine := mockEngine(t, false)
	ctx1, stop1 := context.WithCancel(context.Background())
	r.startNode(ctx1, "flaky", "node-token", engine, 2)
	r.waitNodes(t, 1)
	stop1()
	r.waitNodes(t, 0)

	go func() {
		time.Sleep(300 * time.Millisecond)
		ctx2, stop2 := context.WithCancel(context.Background())
		t.Cleanup(stop2)
		r.startNode(ctx2, "flaky", "node-token", engine, 2)
	}()
	resp := r.do(t, "POST", "/v1/chat/completions", "user-key", chatBody)
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !strings.Contains(string(b), "Hello") {
		t.Fatalf("got %d %s, want the request to wait for the reconnecting node", resp.StatusCode, b)
	}
}

// No capacity within the queue timeout: an honest 503 with Retry-After.
func TestQueueTimeout(t *testing.T) {
	r := newRigCfg(t, func(c *gateway.Config) { c.QueueTimeoutSec = 1 })
	ctx, stop := context.WithCancel(context.Background())
	r.startNode(ctx, "gone", "node-token", mockEngine(t, false), 2)
	r.waitNodes(t, 1)
	stop()
	r.waitNodes(t, 0)

	start := time.Now()
	resp := r.do(t, "POST", "/v1/chat/completions", "user-key", chatBody)
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 503 || resp.Header.Get("Retry-After") == "" || !strings.Contains(string(b), "online") {
		t.Fatalf("got %d (Retry-After %q) %s", resp.StatusCode, resp.Header.Get("Retry-After"), b)
	}
	if d := time.Since(start); d < 900*time.Millisecond || d > 5*time.Second {
		t.Fatalf("waited %v, want about the 1 s queue timeout", d)
	}
}

// A client that leaves mid-stream is still metered, with an estimate from relayed chunks.
func TestCancelledRequestIsMetered(t *testing.T) {
	r := newRig(t, slowEngine(t, 200, 20*time.Millisecond))
	req, _ := http.NewRequest("POST", r.gw.URL+"/v1/chat/completions", strings.NewReader(chatBody))
	req.Header.Set("Authorization", "Bearer user-key")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 600) // a few chunks, then hang up
	io.ReadAtLeast(resp.Body, buf, len(buf))
	resp.Body.Close()

	u := waitUsage(t, r, 1)[0]
	usage := u["usage"].(map[string]any)
	if u["outcome"] != "cancelled" || u["estimated"] != true || u["relayed_chunks"].(float64) < 3 ||
		usage["completion_tokens"] != u["relayed_chunks"] {
		t.Fatalf("bad record for a cancelled request: %v", u)
	}
}

// Completed requests record outcome ok and the gateway's own chunk count next to the
// node-reported tokens.
func TestUsageCrossCheck(t *testing.T) {
	r := newRig(t, mockEngine(t, false))
	resp := r.do(t, "POST", "/v1/chat/completions", "user-key", chatBody)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	u := waitUsage(t, r, 1)[0]
	if u["outcome"] != "ok" || u["relayed_chunks"] != 3.0 || u["estimated"] != nil {
		t.Fatalf("bad record: %v", u)
	}
}

// A token bound to one node id cannot be used by another node.
func TestNodeTokenBoundToNodeID(t *testing.T) {
	r := newRigCfg(t, func(c *gateway.Config) {
		c.NodeTokens["bound-token"] = gateway.NodeToken{Operator: "op2", NodeID: "home-1"}
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r.startNode(ctx, "impostor", "bound-token", mockEngine(t, false), 2)
	time.Sleep(300 * time.Millisecond)
	if n := r.g.NodeCount(); n != 0 {
		t.Fatalf("node with a foreign id was accepted (%d connected)", n)
	}
	r.startNode(ctx, "home-1", "bound-token", mockEngine(t, false), 2)
	r.waitNodes(t, 1)
}

// Reload revokes a node token without a restart and disconnects the node; new API keys work.
func TestReloadRevokesTokenAndAddsKeys(t *testing.T) {
	r := newRig(t, mockEngine(t, false))
	r.g.Reload(gateway.Config{
		APIKeys:    map[string]string{"new-key": "carol"},
		NodeTokens: map[string]gateway.NodeToken{},
	})
	r.waitNodes(t, 0)
	time.Sleep(1500 * time.Millisecond) // the agent retries with backoff; it must stay out
	if n := r.g.NodeCount(); n != 0 {
		t.Fatalf("revoked node reconnected (%d connected)", n)
	}
	if resp := r.do(t, "GET", "/v1/models", "user-key", ""); resp.StatusCode != 401 {
		t.Fatalf("old key still accepted after reload: %d", resp.StatusCode)
	}
	if resp := r.do(t, "GET", "/v1/models", "new-key", ""); resp.StatusCode != 200 {
		t.Fatalf("new key rejected after reload: %d", resp.StatusCode)
	}
}

// Right after a gateway (re)start no node is connected and no model is known yet; a request
// must wait for the nodes to reconnect instead of failing as "not served".
func TestFreshGatewayWaitsForFirstNode(t *testing.T) {
	r := newRigCfg(t, nil)
	go func() {
		time.Sleep(300 * time.Millisecond)
		ctx, stop := context.WithCancel(context.Background())
		t.Cleanup(stop)
		r.startNode(ctx, "late", "node-token", mockEngine(t, false), 2)
	}()
	resp := r.do(t, "POST", "/v1/chat/completions", "user-key", chatBody)
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !strings.Contains(string(b), "Hello") {
		t.Fatalf("got %d %s, want the request to wait for the first node", resp.StatusCode, b)
	}
}
