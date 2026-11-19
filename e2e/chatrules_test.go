// The chat rules for volunteers' nodes (gateway/chatrules.go): the operator terms (GDPR decision D3).
//
// SPDX-License-Identifier: Apache-2.0
package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"html"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/aisathome/aisathome/gateway"
	"github.com/aisathome/aisathome/protocol"
	"github.com/coder/websocket"
)

const rulesSciKey = "ak-e2e-rules-science-key-0123456789abcdefgh"

// rulesRig: accounts, the chat rules on (as by default), a science project, a "boinc" connector with
// chat on pilots, and no nodes. It returns the config for a restart.
func rulesRig(t *testing.T) (*acctRig, *gateway.Config) {
	t.Helper()
	var cfg *gateway.Config
	a := newAcctRigNodes(t, 100, nil, func(c *gateway.Config) {
		cfg = c
		c.ChatRulesOff = false
		c.Projects = []gateway.Project{{ID: "lab", KeySHA256: []string{sha(rulesSciKey)}}}
		c.Connectors = []gateway.Connector{{ID: "boinc", KeySHA256: []string{sha(connectorKey)}, MaxPilots: 5, PilotsChatEnabled: true}}
		c.QueueTimeoutSec = 2
	})
	return a, cfg
}

// termsNode connects a hand-written node to the gateway at gwURL; it answers every request with
// "from-<id>".
func termsNode(t *testing.T, gwURL, id, token string) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	h := http.Header{}
	h.Set("Authorization", "Bearer "+token)
	c, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(gwURL, "http")+"/v1/node/connect", &websocket.DialOptions{HTTPHeader: h})
	if err != nil {
		t.Fatal(err)
	}
	write := func(m protocol.Msg) { b, _ := json.Marshal(m); c.Write(ctx, websocket.MessageText, b) }
	write(protocol.Msg{Type: protocol.TypeHello, NodeID: id, Protocol: protocol.LegacyVersion,
		Models: []protocol.ModelInfo{{Name: "qwen3.8-27b-q4", Roles: []string{"deep"}, Slots: 4}}})
	go func() {
		for {
			_, b, err := c.Read(ctx)
			if err != nil {
				return
			}
			var m protocol.Msg
			json.Unmarshal(b, &m)
			if m.Type == protocol.TypeRequest {
				write(chunkMsg(m.ReqID, "from-"+id))
				write(protocol.Msg{Type: protocol.TypeDone, ReqID: m.ReqID, Usage: &protocol.Usage{PromptTokens: 5, CompletionTokens: 1}})
			}
		}
	}()
}

// snap is the accounts file as the gateway wrote it.
func snap(a *acctRig) *gateway.AccountsFile {
	a.store.Refresh()
	f := a.store.Snapshot()
	return &f
}

// operator signs in GitHub user n and adds a node; it returns the session, the node token and id.
func (a *acctRig) operator(t *testing.T, n int) (session, token, id string) {
	t.Helper()
	session, _ = a.signIn(t, n)
	token, id, page := a.addNode(t, session, fmt.Sprintf("node%d", n))
	if token == "" {
		t.Fatalf("no node token for user %d: %s", n, page)
	}
	return session, token, id
}

// apiUser signs in GitHub user n and creates an API key; it returns the session and the key.
func (a *acctRig) apiUser(t *testing.T, n int) (session, key string) {
	t.Helper()
	session, _ = a.signIn(t, n)
	_, page := a.createKey(t, session)
	if key = keyRe.FindString(page); key == "" {
		t.Fatalf("no key for user %d", n)
	}
	return session, key
}

func (a *acctRig) acceptTerms(t *testing.T, session string) {
	t.Helper()
	resp := a.postForm(t, "/account/operator", "action=accept&accept=1&version="+gateway.OperatorTermsVersion, session)
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("accept the operator terms: %d", resp.StatusCode)
	}
}

var fromRe = regexp.MustCompile(`from-[A-Za-z0-9-]+`)

// servedBy asks n times with key and returns which nodes answered (by "from-<id>"), and the last
// refusal's status and body.
func servedBy(t *testing.T, r *rig, key string, n int) (map[string]int, int, string) {
	t.Helper()
	got := map[string]int{}
	code, last := 0, ""
	for i := 0; i < n; i++ {
		c, body := ask(t, r, key, fmt.Sprintf(`{"model":"deep","messages":[{"role":"user","content":"question %d"}]}`, i))
		if c != 200 {
			code, last = c, body
			continue
		}
		m := fromRe.FindString(body)
		if m == "" {
			t.Fatalf("answer from no known node: %s", body)
		}
		got[m]++
	}
	return got, code, last
}

// D3: a node whose operator has not accepted the operator terms serves science, never people's
// requests; accepting on the account page lets it serve chat, withdrawing stops it again. The page shows
// the approved text.
func TestChatRulesOperatorTerms(t *testing.T) {
	a, _ := rulesRig(t)
	sA, tokA, idA := a.operator(t, 1)
	_, keyB := a.apiUser(t, 2)
	termsNode(t, a.gw.URL, idA, tokA)
	a.waitNodes(t, 1)

	if got, code, body := servedBy(t, a.rig, keyB, 1); len(got) != 0 || code != 503 || !strings.Contains(body, "operator terms") {
		t.Fatalf("chat on a node without the operator terms: %v %d %s", got, code, body)
	}
	if got, _, body := servedBy(t, a.rig, rulesSciKey, 1); got["from-"+idA] != 1 {
		t.Fatalf("science on a node without the operator terms: %v %s", got, body)
	}
	_, page := a.get(t, "/account", sA)
	for _, want := range []string{"science only: accept the operator terms", `name="accept"`, "2026-11-19",
		html.EscapeString("Users may not put personal data into their chats, and we send your node no account, name, email address or IP address."),
		html.EscapeString("Not let anyone else run your node's work for you or pass requests on."),
		html.EscapeString("Your node answers other people's chats only after you have accepted these terms; until then it does science work only."),
		html.EscapeString(gateway.UserRule)} {
		if !strings.Contains(page, want) {
			t.Errorf("account page before accepting lacks %q", want)
		}
	}
	if strings.Contains(page, "These terms are a draft") {
		t.Error("the final terms are shown as a draft")
	}
	// The tick is required.
	if resp := a.postForm(t, "/account/operator", "action=accept&version="+gateway.OperatorTermsVersion, sA); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("accepting without the tick: %d", resp.StatusCode)
	}

	a.acceptTerms(t, sA)
	if got, _, body := servedBy(t, a.rig, keyB, 3); got["from-"+idA] != 3 {
		t.Fatalf("chat after accepting: %v %s", got, body)
	}
	_, page = a.get(t, "/account", sA)
	if !strings.Contains(page, "chat and science") || !strings.Contains(page, "Accepted") {
		t.Errorf("account page after accepting: no chat state")
	}
	acct := snap(a).Find("gh:1")
	if acct.OperatorTerms == nil || acct.OperatorTerms.Version != gateway.OperatorTermsVersion || acct.OperatorTerms.At.IsZero() {
		t.Fatalf("recorded acceptance: %+v", acct.OperatorTerms)
	}

	if resp := a.postForm(t, "/account/operator", "action=withdraw", sA); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("withdraw: %d", resp.StatusCode)
	}
	if got, code, _ := servedBy(t, a.rig, keyB, 1); len(got) != 0 || code != 503 {
		t.Fatalf("chat after withdrawing: %v %d", got, code)
	}
	if got, _, _ := servedBy(t, a.rig, rulesSciKey, 1); got["from-"+idA] != 1 {
		t.Fatalf("science after withdrawing: %v", got)
	}
}

// While the operator terms are a draft, accepting them lets no node serve people's requests.
func TestChatRulesDraftTerms(t *testing.T) {
	old := gateway.OperatorTermsVersion
	gateway.OperatorTermsVersion = "draft-2026-12-01"
	t.Cleanup(func() { gateway.OperatorTermsVersion = old }) // registered first: runs after the gateway closed
	a, _ := rulesRig(t)
	sA, tokA, idA := a.operator(t, 1)
	_, keyB := a.apiUser(t, 2)
	a.acceptTerms(t, sA)
	termsNode(t, a.gw.URL, idA, tokA)
	a.waitNodes(t, 1)
	if got, code, _ := servedBy(t, a.rig, keyB, 1); len(got) != 0 || code != 503 {
		t.Fatalf("chat on a node whose operator accepted a draft: %v %d", got, code)
	}
	if got, _, _ := servedBy(t, a.rig, rulesSciKey, 1); got["from-"+idA] != 1 {
		t.Fatalf("science: %v", got)
	}
	_, page := a.get(t, "/account", sA)
	if !strings.Contains(page, "These terms are a draft") || !strings.Contains(page, "the operator terms are not final yet") {
		t.Error("the account page does not say the terms are a draft")
	}
}

// The sign-in page states the age limit and the rule for people who chat.
func TestChatRulesSignInPage(t *testing.T) {
	a, _ := rulesRig(t)
	_, page := a.get(t, "/account", "")
	for _, want := range []string{"18 or older", html.EscapeString(gateway.UserRule)} {
		if !strings.Contains(page, want) {
			t.Errorf("sign-in page lacks %q", want)
		}
	}
	if strings.Contains(page, `name="transfer"`) {
		t.Error("the sign-in page still has the transfer box")
	}
}

// The operator's acceptance survives a restart: a new gateway on the same accounts file serves the same
// chat on the same node.
func TestChatRulesSurviveRestart(t *testing.T) {
	a, cfg := rulesRig(t)
	sA, tokA, idA := a.operator(t, 1)
	_, keyB := a.apiUser(t, 2)
	a.acceptTerms(t, sA)
	a.g.Close()

	g2, err := gateway.New(*cfg, quiet())
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(g2.Handler())
	t.Cleanup(func() { srv.Close(); g2.Close() })
	termsNode(t, srv.URL, idA, tokA)
	for deadline := time.Now().Add(5 * time.Second); g2.NodeCount() != 1; time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("node did not connect to the restarted gateway")
		}
	}
	r2 := &rig{g: g2, gw: srv}
	if got, _, body := servedBy(t, r2, keyB, 2); got["from-"+idA] != 2 {
		t.Fatalf("after a restart: %v %s", got, body)
	}
}

// BOINC-first operators: the connector records its user's acceptance (the bridge, when BOINC records the
// ENROLL consent); that user's pilot then serves chat.
func TestChatRulesConnectorTerms(t *testing.T) {
	a, _ := rulesRig(t)
	_, keyB := a.apiUser(t, 2)
	engine := mockEngine(t, false)
	t.Cleanup(engine.Close)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pid, cred := a.newPilot(t)
	a.connector(t, "POST", "/pilots/"+pid+"/bind", map[string]any{"user": "42", "host": "11"})
	a.runPilot(ctx, cred, engine.URL)
	waitUntil(t, "the pilot to connect", func() bool { return a.g.NodeCount() == 1 })

	if code, body := ask(t, a.rig, keyB, deepBody); code != 503 || !strings.Contains(body, "operator terms") {
		t.Fatalf("chat on a pilot without the operator terms: %d %s", code, body)
	}
	if code, _ := ask(t, a.rig, rulesSciKey, deepBody); code != 200 {
		t.Fatalf("science on the pilot: %d", code)
	}
	for _, bad := range []map[string]any{
		{"version": "", "accepted_at": time.Now()},
		{"version": gateway.OperatorTermsVersion},
		{"version": gateway.OperatorTermsVersion, "accepted_at": time.Now().Add(time.Hour)},
	} {
		b, _ := json.Marshal(bad)
		resp := a.do(t, "POST", "/v1/connectors/boinc/users/42/operator-terms", connectorKey, string(b))
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("bad body %v: %d", bad, resp.StatusCode)
		}
	}
	if resp := a.do(t, "POST", "/v1/connectors/boinc/users/42/operator-terms", "user-key", "{}"); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("another key: %d", resp.StatusCode)
	}

	// A connector clock a little ahead: accepted, recorded as now.
	out := a.connector(t, "POST", "/users/42/operator-terms", map[string]any{"version": gateway.OperatorTermsVersion,
		"accepted_at": time.Now().Add(2 * time.Minute)})
	if out["recorded"] != true || out["current"] != true || out["account_kind"] != "unclaimed" {
		t.Fatalf("connector terms answer: %v", out)
	}
	if code, body := ask(t, a.rig, keyB, deepBody); code != 200 || !strings.Contains(body, "Hello") {
		t.Fatalf("chat on the accepted pilot: %d %s", code, body)
	}
	for _, x := range snap(a).Accounts {
		if x.Status == gateway.StatusUnclaimed && x.OperatorTerms != nil && x.OperatorTerms.At.After(time.Now()) {
			t.Fatalf("an acceptance from the future was stored: %v", x.OperatorTerms.At)
		}
	}
	// An older record changes nothing and says so.
	out = a.connector(t, "POST", "/users/42/operator-terms", map[string]any{"version": "old-1", "accepted_at": time.Now().Add(-time.Hour)})
	if out["recorded"] != false || out["current"] != true {
		t.Fatalf("older record: %v", out)
	}
	// A newer record of an old version is recorded but lets the pilot serve no chat.
	out = a.connector(t, "POST", "/users/42/operator-terms", map[string]any{"version": "old-1", "accepted_at": time.Now()})
	if out["recorded"] != true || out["current"] != false {
		t.Fatalf("old version: %v", out)
	}
	if code, _ := ask(t, a.rig, keyB, deepBody); code != 503 {
		t.Fatalf("chat on a pilot whose user accepted only an old version: %d", code)
	}
}

// Trusted nodes need no operator terms, and a config token can name the version accepted.
func TestChatRulesConfigTokens(t *testing.T) {
	a, saved := rulesRig(t)
	_, keyB := a.apiUser(t, 2)
	cfg := *saved
	cfg.NodeTokens = map[string]gateway.NodeToken{
		"home-token":  {Operator: "owner", Trust: "trusted"},
		"vol-token":   {Operator: "vol"},
		"volok-token": {Operator: "volok", OperatorTerms: gateway.OperatorTermsVersion},
	}
	if err := a.g.Reload(cfg); err != nil {
		t.Fatal(err)
	}
	termsNode(t, a.gw.URL, "home", "home-token")
	a.waitNodes(t, 1)
	if got, _, body := servedBy(t, a.rig, keyB, 2); got["from-home"] != 2 {
		t.Fatalf("trusted node: %v %s", got, body)
	}
	termsNode(t, a.gw.URL, "vol", "vol-token")
	termsNode(t, a.gw.URL, "volok", "volok-token")
	a.waitNodes(t, 3)
	got, _, _ := servedBy(t, a.rig, keyB, 12)
	if got["from-vol"] != 0 || got["from-volok"] == 0 {
		t.Fatalf("config tokens: %v", got)
	}
}
