// Served-by for operator keys: only keys marked served_by learn which node answered.
//
// SPDX-License-Identifier: Apache-2.0
package e2e

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aisathome/aisathome/gateway"
)

const (
	servedByHeader  = "X-AIsAtHome-Served-By"
	requestIDHeader = "X-AIsAtHome-Request-Id"
)

func servedByRig(t *testing.T, engines ...*httptest.Server) *rig {
	return newRigCfg(t, func(c *gateway.Config) {
		c.Keys["operator-key"] = gateway.KeyInfo{User: "runner", ServedBy: true}
		c.Keys["front-key"] = gateway.KeyInfo{User: "owui", ServedBy: true, ForwardUsers: true}
	}, engines...)
}

func chatHeaders(t *testing.T, r *rig, key string, stream bool, content string) http.Header {
	t.Helper()
	resp := r.do(t, "POST", "/v1/chat/completions", key,
		fmt.Sprintf(`{"model":"deep","stream":%t,"messages":[{"role":"user","content":%q}]}`, stream, content))
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !strings.Contains(string(b), "Hello") {
		t.Fatalf("%s stream=%t: status %d body %s", key, stream, resp.StatusCode, b)
	}
	return resp.Header
}

// An operator key gets the node and the request id, streamed or not; every attempt is logged
// with the same request id and node; ordinary keys get neither header.
func TestServedByOnlyForOperatorKeys(t *testing.T) {
	logs := &syncBuf{}
	logSink = logs
	defer func() { logSink = io.Discard }()
	r := servedByRig(t, mockEngine(t, false))
	for _, stream := range []bool{false, true} {
		h := chatHeaders(t, r, "operator-key", stream, "hi")
		if got := h.Get(servedByHeader); got != "node-0" {
			t.Fatalf("operator key, stream=%t: %s = %q, want node-0", stream, servedByHeader, got)
		}
		id := h.Get(requestIDHeader)
		if id == "" {
			t.Fatalf("operator key, stream=%t: no %s", stream, requestIDHeader)
		}
		waitFor(t, func() bool {
			return strings.Contains(logs.String(), `msg="chat request" req=`+id+` node=node-0 ok=true`)
		})
		for _, key := range []string{"user-key", "limited-key", "restore-key"} {
			h := chatHeaders(t, r, key, stream, "hi")
			if h.Get(servedByHeader) != "" || h.Get(requestIDHeader) != "" {
				t.Fatalf("ordinary key %s, stream=%t, learned the node: %v", key, stream, h)
			}
		}
	}
	// Every request is logged with its node, also those of ordinary keys.
	if n := strings.Count(logs.String(), `msg="chat request"`); n < 8 {
		t.Fatalf("want a log line per request (8), got %d", n)
	}
}

// A front end's key never passes the node on to its end users, even if marked served_by.
func TestServedByNotForFrontEndUsers(t *testing.T) {
	r := servedByRig(t, mockEngine(t, false))
	for _, stream := range []bool{false, true} {
		req, _ := http.NewRequest("POST", r.front.URL+"/v1/chat/completions", strings.NewReader(
			fmt.Sprintf(`{"model":"deep","stream":%t,"messages":[{"role":"user","content":"hi"}]}`, stream)))
		req.Header.Set("Authorization", "Bearer front-key")
		req.Header.Set("X-OpenWebUI-User-Id", "u1")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatalf("front end, stream=%t: status %d", stream, resp.StatusCode)
		}
		if resp.Header.Get(servedByHeader) != "" || resp.Header.Get(requestIDHeader) != "" {
			t.Fatalf("front-end user learned the node, stream=%t: %v", stream, resp.Header)
		}
	}
}

// After a failover the header names the node that answered, not the one that failed.
func TestServedByAfterFailover(t *testing.T) {
	r := servedByRig(t, mockEngine(t, true), mockEngine(t, false))
	failHits.Store(0)
	tried := 0 // operator requests that met the broken node first
	for i := 0; i < 6; i++ {
		stream := i%2 == 1
		before := failHits.Load()
		h := chatHeaders(t, r, "operator-key", stream, fmt.Sprintf("question %d", i))
		if failHits.Load() > before {
			tried++
		}
		if got := h.Get(servedByHeader); got != "node-1" {
			t.Fatalf("request %d, stream=%t: %s = %q, want the healthy node-1", i, stream, servedByHeader, got)
		}
		h = chatHeaders(t, r, "user-key", stream, fmt.Sprintf("other %d", i))
		if h.Get(servedByHeader) != "" {
			t.Fatalf("ordinary key learned the node after failover: %v", h)
		}
	}
	if tried == 0 {
		t.Fatal("no operator request met the broken node, so failover was not exercised")
	}
}

// Accounts' own keys (ordinary users) never learn the node; admin accounts' keys (the
// operator's runner and dogfooding) do.
func TestServedByForAdminAccountKeysOnly(t *testing.T) {
	a := newAcctRig(t, 100, nil) // gh:1 is the admin
	for _, c := range []struct {
		id    int
		admin bool
	}{{1, true}, {7, false}} {
		s, _ := a.signIn(t, c.id)
		_, page := a.createKey(t, s)
		key := keyRe.FindString(page)
		if key == "" {
			t.Fatal("no key created")
		}
		for _, stream := range []bool{false, true} {
			h := chatHeaders(t, a.rig, key, stream, "hi")
			if got := h.Get(servedByHeader); c.admin && (got != "node-0" || h.Get(requestIDHeader) == "") {
				t.Fatalf("admin account key, stream=%t: %s = %q", stream, servedByHeader, got)
			} else if !c.admin && (got != "" || h.Get(requestIDHeader) != "") {
				t.Fatalf("account key learned the node, stream=%t: %v", stream, h)
			}
		}
	}
}
