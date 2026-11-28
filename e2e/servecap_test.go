// What may serve whom (gateway/servecap.go): /v1/stats' serve, the signal that would have shown
// the incident of 2026-11-20 (nodes online, no operator had accepted the operator terms, every chat and
// API request refused for 8 hours while science and every check stayed green).
//
// SPDX-License-Identifier: Apache-2.0
package e2e

import (
	"encoding/json"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/aisathome/aisathome/gateway"
)

type serveCounts struct {
	Chat    int `json:"chat"`
	Tools   int `json:"tools"`
	Science int `json:"science"`
}

// statsNodes reads /v1/stats: nodes online, and serve (which must be there).
func statsNodes(t *testing.T, r *rig) (online int, serve serveCounts) {
	t.Helper()
	resp := r.do(t, "GET", "/v1/stats", "", "")
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	var out struct {
		Nodes struct {
			Online int `json:"online"`
		} `json:"nodes"`
		Serve *serveCounts `json:"serve"`
	}
	if err := json.Unmarshal(b, &out); err != nil || out.Serve == nil {
		t.Fatalf("/v1/stats without serve: %v %s", err, b)
	}
	if strings.Contains(string(b), "gh:") || strings.Contains(string(b), "node1") {
		t.Fatalf("/v1/stats names an account or node: %s", b)
	}
	return out.Nodes.Online, *out.Serve
}

// Nodes online whose operators have not accepted the operator terms (tokens made before creating a node
// accepted them, as on 2026-11-20): people's requests (chat, and agents with tools) are refused, science
// is served, and /v1/stats says 2 online, 0 may serve chat, 2 may serve science. Once one operator
// accepts, chat is served on that node and the signal says 1. The own-only switch takes it to 0 again.
func TestServeCapacityNoAcceptedNode(t *testing.T) {
	a, _ := rulesRig(t)
	s1, _ := a.signIn(t, 1)
	a.signIn(t, 3)
	tok1, id1, err := a.store.AddNode("gh:1", "node1", time.Now(), nil) // no acceptance on record
	if err != nil {
		t.Fatal(err)
	}
	tok3, id3, err := a.store.AddNode("gh:3", "node3", time.Now(), nil)
	if err != nil {
		t.Fatal(err)
	}
	_, keyB := a.apiUser(t, 2)
	waitUntil(t, "the gateway to read the new tokens", func() bool { // it rereads the accounts file within seconds
		_, page := a.get(t, "/account", s1)
		return strings.Contains(page, "terms-banner")
	})
	termsNode(t, a.gw.URL, id1, tok1)
	termsNode(t, a.gw.URL, id3, tok3)
	a.waitNodes(t, 2)

	if online, serve := statsNodes(t, a.rig); online != 2 || serve != (serveCounts{Chat: 0, Tools: 0, Science: 2}) {
		t.Fatalf("before acceptance: online %d serve %+v, want 2 online, 0 chat, 2 science", online, serve)
	}
	if got, code, body := servedBy(t, a.rig, keyB, 2); len(got) != 0 || code != 503 || !strings.Contains(body, "operator terms") {
		t.Fatalf("chat with no accepted node: %v %d %s", got, code, body)
	}
	tools := `{"model":"deep","messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"f","parameters":{"type":"object"}}}]}`
	if code, body := ask(t, a.rig, keyB, tools); code != 503 {
		t.Fatalf("agent with no accepted node: %d %s", code, body)
	}
	if got, _, body := servedBy(t, a.rig, rulesSciKey, 2); got["from-"+id1]+got["from-"+id3] != 2 {
		t.Fatalf("science with no accepted node: %v %s", got, body)
	}

	a.acceptTerms(t, s1) // the account page's button: its nodes serve chat at once (sweep)
	if online, serve := statsNodes(t, a.rig); online != 2 || serve.Chat != 1 || serve.Science != 2 {
		t.Fatalf("after one acceptance: online %d serve %+v, want 1 chat, 2 science", online, serve)
	}
	if got, _, body := servedBy(t, a.rig, keyB, 3); got["from-"+id1] != 3 {
		t.Fatalf("chat after acceptance: %v %s", got, body)
	}

	// The emergency switch: no stranger's node serves people's requests, whatever their terms.
	a.store.SetRouting(gateway.RoutingOwnOnly)
	waitUntil(t, "own-only in /v1/stats", func() bool { _, s := statsNodes(t, a.rig); return s.Chat == 0 })
	if got, code, _ := servedBy(t, a.rig, keyB, 1); len(got) != 0 || code != 503 {
		t.Fatalf("chat under own-only: %v %d", got, code)
	}
}
