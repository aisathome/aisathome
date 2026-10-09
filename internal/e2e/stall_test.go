// Tests for hung engines: health checks, node and gateway stall timeouts.
//
// SPDX-License-Identifier: Apache-2.0
package e2e

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"aisathome/internal/gateway"
	"aisathome/internal/node"
	"aisathome/internal/protocol"
)

// switchEngine is healthy while ok is true; /health fails otherwise (like a hung llama-server).
func switchEngine(t *testing.T, ok *atomic.Bool) *httptest.Server {
	inner := mockEngine(t, false)
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !ok.Load() {
			time.Sleep(4 * time.Second) // hangs past the agent's 3 s health timeout
			return
		}
		resp, err := http.Post(inner.URL+r.URL.Path, "application/json", r.Body)
		if err != nil {
			return
		}
		defer resp.Body.Close()
		w.Header().Set("Content-Type", resp.Header.Get("Content-Type"))
		w.WriteHeader(resp.StatusCode)
		io.Copy(w, resp.Body)
	}))
}

// A node whose engine stops answering /health leaves the gateway, and returns once healthy.
func TestNodeLeavesWhileEngineUnhealthy(t *testing.T) {
	var ok atomic.Bool
	ok.Store(true)
	r := newRigCfg(t, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r.startNodeCfg(ctx, node.Config{NodeID: "flaky-engine", Token: "node-token", Upstream: switchEngine(t, &ok).URL,
		HealthInterval: 50 * time.Millisecond,
		Models:         []protocol.ModelInfo{{Name: "qwen3.8-27b-q4", Roles: []string{"deep"}, Slots: 2}}})
	r.waitNodes(t, 1)
	ok.Store(false)
	deadline := time.Now().Add(20 * time.Second)
	for r.g.NodeCount() != 0 && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if r.g.NodeCount() != 0 {
		t.Fatal("node stayed connected although its engine is unhealthy")
	}
	ok.Store(true)
	r.waitNodes(t, 1)
}

// hangEngine accepts requests and never answers (health stays fine).
var hangHits atomic.Int32

func hangEngine(t *testing.T) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if health(w, r) {
			return
		}
		hangHits.Add(1)
		<-r.Context().Done()
	}))
}

// The gateway gives up on a silent node and retries the request on another one.
func TestGatewayFailsOverFromStalledNode(t *testing.T) {
	hangHits.Store(0)
	r := newRigCfg(t, func(c *gateway.Config) { c.FirstChunkTimeoutSec = 1 }, hangEngine(t), mockEngine(t, false))
	for i := 0; i < 2; i++ {
		start := time.Now()
		resp := r.do(t, "POST", "/v1/chat/completions", "user-key", chatBody)
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 || !strings.Contains(string(b), "Hello") {
			t.Fatalf("request %d: %d %s", i, resp.StatusCode, b)
		}
		if time.Since(start) > 5*time.Second {
			t.Fatalf("request %d took %v", i, time.Since(start))
		}
	}
	if hangHits.Load() == 0 {
		t.Fatal("the hung node was never tried, so the stall timeout was not exercised")
	}
}

// The node agent itself ends a stalled engine request and reports an error.
func TestNodeReportsStalledEngine(t *testing.T) {
	r := newRigCfg(t, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r.startNodeCfg(ctx, node.Config{NodeID: "stuck", Token: "node-token", Upstream: hangEngine(t).URL,
		FirstChunkTimeout: 500 * time.Millisecond,
		Models:            []protocol.ModelInfo{{Name: "qwen3.8-27b-q4", Roles: []string{"deep"}, Slots: 2}}})
	r.waitNodes(t, 1)
	start := time.Now()
	resp := r.do(t, "POST", "/v1/chat/completions", "user-key", chatBody)
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 502 || time.Since(start) > 5*time.Second {
		t.Fatalf("got %d after %v (%s), want a quick 502 from the node's stall report", resp.StatusCode, time.Since(start), b)
	}
}
