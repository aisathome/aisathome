// The order of a chat request's stages, seen from outside (api.go, handleChat): which refusal comes
// first decides what a request costs the gateway and the user.
//
// SPDX-License-Identifier: Apache-2.0
package e2e

import (
	"io"
	"net/http"
	"testing"

	"github.com/aisathome/aisathome/gateway"
)

func chatStatus(t *testing.T, r *rig, key, body string) (int, http.Header) {
	t.Helper()
	resp := r.do(t, "POST", "/v1/chat/completions", key, body)
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	return resp.StatusCode, resp.Header
}

// A request is refused by who it is before anyone reads what it says: a wrong key and a used-up
// budget both come before a body that does not parse.
func TestChatIdentityComesBeforeTheBody(t *testing.T) {
	r := newRigCfg(t, func(c *gateway.Config) {
		c.Keys["budget-key"] = gateway.KeyInfo{User: "dave", TokensPerDay: 10}
	}, mockEngine(t, false))
	if code, _ := chatStatus(t, r, "wrong-key", "not json"); code != 401 {
		t.Fatalf("wrong key: %d", code)
	}
	if code, _ := chatStatus(t, r, "budget-key", "not json"); code != 400 {
		t.Fatalf("within its budget: %d", code)
	}
	for i := 0; i < 2; i++ { // mockEngine's usage weighs 7 tokens
		if code, _ := chatStatus(t, r, "budget-key", chatBody); code != 200 {
			t.Fatalf("request %d: %d", i, code)
		}
	}
	code, h := chatStatus(t, r, "budget-key", "not json")
	if code != 429 || h.Get("Retry-After") == "" {
		t.Fatalf("over its budget: %d %v", code, h)
	}
}

// The request limits are taken after the body is read, so a body that does not parse uses none of
// them, and before the body is judged, so a well-formed but refused request does.
func TestChatLimitsSitBetweenBodyAndChecks(t *testing.T) {
	r := newRig(t, mockEngine(t, false)) // limited-key: 2 requests a minute
	for i := 0; i < 3; i++ {
		if code, _ := chatStatus(t, r, "limited-key", "not json"); code != 400 {
			t.Fatalf("bad JSON %d: %d", i, code)
		}
	}
	if code, _ := chatStatus(t, r, "limited-key", `{"model":"deep"}`); code != 400 {
		t.Fatalf("no messages: %d", code)
	}
	if code, _ := chatStatus(t, r, "limited-key", chatBody); code != 200 {
		t.Fatalf("the second request of the minute: %d", code)
	}
	if code, _ := chatStatus(t, r, "limited-key", `{"model":"deep"}`); code != 429 {
		t.Fatalf("past the limit, the limit answers first: %d", code)
	}
}

// The concurrency slot comes back at the end of a request, answered or refused.
func TestChatConcurrencySlotComesBack(t *testing.T) {
	r := newRigCfg(t, func(c *gateway.Config) {
		c.Keys["one-key"] = gateway.KeyInfo{User: "dave", MaxConcurrent: 1}
	}, mockEngine(t, false))
	for i, c := range []struct {
		body string
		want int
	}{{chatBody, 200}, {`{"model":"deep"}`, 400}, {chatBody, 200}, {chatBody, 200}} {
		if code, _ := chatStatus(t, r, "one-key", c.body); code != c.want {
			t.Fatalf("request %d: %d, want %d", i, code, c.want)
		}
	}
}
