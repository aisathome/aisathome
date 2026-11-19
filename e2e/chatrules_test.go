// The chat rules for volunteers' nodes (gateway/chatrules.go): the operator terms (GDPR decision D3)
// and the country rule with the opt-in to volunteers outside Europe (D4 option B).
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
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/aisathome/aisathome/gateway"
	"github.com/aisathome/aisathome/protocol"
	"github.com/coder/websocket"
)

const rulesSciKey = "ak-e2e-rules-science-key-0123456789abcdefgh"

// The rules' GeoIP table: loopback is Norway, 203.0.113.0/24 the US, 198.51.100.0/24 Sweden.
const rulesGeo = "127.0.0.0,127.255.255.255,NO\n203.0.113.0,203.0.113.255,US\n198.51.100.0,198.51.100.255,SE\n"

// rulesRig: accounts, the chat rules on (as by default) with a GeoIP table, a science project, a
// "boinc" connector with chat on pilots, and no nodes. It returns the config for a restart.
func rulesRig(t *testing.T) (*acctRig, *gateway.Config) {
	t.Helper()
	geo := filepath.Join(t.TempDir(), "dbip-country-lite.csv")
	if err := os.WriteFile(geo, []byte(rulesGeo), 0o600); err != nil {
		t.Fatal(err)
	}
	var cfg *gateway.Config
	a := newAcctRigNodes(t, 100, nil, func(c *gateway.Config) {
		cfg = c
		c.ChatRulesOff = false
		c.GeoIPCountryFile = geo
		c.Projects = []gateway.Project{{ID: "lab", KeySHA256: []string{sha(rulesSciKey)}}}
		c.Connectors = []gateway.Connector{{ID: "boinc", KeySHA256: []string{sha(connectorKey)}, MaxPilots: 5, PilotsChatEnabled: true}}
		c.QueueTimeoutSec = 2
	})
	return a, cfg
}

// geoNode connects a hand-written node to the gateway at gwURL from address xff (behind a local
// proxy, as Caddy is); it answers every request with "from-<id>".
func geoNode(t *testing.T, gwURL, id, token, xff string) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	h := http.Header{}
	h.Set("Authorization", "Bearer "+token)
	h.Set("X-Forwarded-For", xff)
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

func (a *acctRig) acceptTerms(t *testing.T, session, country string) {
	t.Helper()
	resp := a.postForm(t, "/account/operator", "action=accept&accept=1&version="+gateway.OperatorTermsVersion+"&country="+country, session)
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("accept the operator terms: %d", resp.StatusCode)
	}
}

func (a *acctRig) setTransfer(t *testing.T, session string, on bool) {
	t.Helper()
	form := "action=off"
	if on {
		form = "action=on&transfer=1&version=" + url.QueryEscape(gateway.TransferConsentVersion)
	}
	if resp := a.postForm(t, "/account/transfer", form, session); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("transfer %v: %d", on, resp.StatusCode)
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
// requests; accepting on the account page lets it serve chat, withdrawing stops it again.
func TestChatRulesOperatorTerms(t *testing.T) {
	a, _ := rulesRig(t)
	sA, tokA, idA := a.operator(t, 1)
	_, keyB := a.apiUser(t, 2)
	geoNode(t, a.gw.URL, idA, tokA, "198.51.100.7") // Sweden
	a.waitNodes(t, 1)

	if got, code, body := servedBy(t, a.rig, keyB, 1); len(got) != 0 || code != 503 || !strings.Contains(body, "operator terms") {
		t.Fatalf("chat on a node without the operator terms: %v %d %s", got, code, body)
	}
	if got, _, body := servedBy(t, a.rig, rulesSciKey, 1); got["from-"+idA] != 1 {
		t.Fatalf("science on a node without the operator terms: %v %s", got, body)
	}
	_, page := a.get(t, "/account", sA)
	for _, want := range []string{"science only: accept the operator terms", `name="accept"`, gateway.OperatorTermsVersion, "IP Geolocation by DB-IP"} {
		if !strings.Contains(page, want) {
			t.Errorf("account page before accepting lacks %q", want)
		}
	}
	// The tick is required.
	if resp := a.postForm(t, "/account/operator", "action=accept&version="+gateway.OperatorTermsVersion+"&country=SE", sA); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("accepting without the tick: %d", resp.StatusCode)
	}

	a.acceptTerms(t, sA, "SE")
	if got, _, body := servedBy(t, a.rig, keyB, 3); got["from-"+idA] != 3 {
		t.Fatalf("chat after accepting: %v %s", got, body)
	}
	_, page = a.get(t, "/account", sA)
	if !strings.Contains(page, "chat and science (connects from Sweden)") || !strings.Contains(page, "Accepted") {
		t.Errorf("account page after accepting: no chat state")
	}
	a.store.Refresh()
	acct := snap(a).Find("gh:1")
	if acct.OperatorTerms == nil || acct.OperatorTerms.Version != gateway.OperatorTermsVersion || acct.OperatorTerms.At.IsZero() || acct.OperatorCountry != "SE" {
		t.Fatalf("recorded acceptance: %+v %q", acct.OperatorTerms, acct.OperatorCountry)
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

// D4: without the opt-in, people's requests go only to nodes in the EEA or an adequate country whose
// address matches the operator's stated country; with it, to any node with the operator terms. The
// account's chat (through the front end) follows the same setting; anonymous front-end users have none.
func TestChatRulesCountry(t *testing.T) {
	a, _ := rulesRig(t)
	sA, tokA, idA := a.operator(t, 1) // in the US
	sC, tokC, idC := a.operator(t, 3) // in Sweden, but states Norway
	sB, keyB := a.apiUser(t, 2)
	a.acceptTerms(t, sA, "other")
	a.acceptTerms(t, sC, "NO")
	geoNode(t, a.gw.URL, idA, tokA, "203.0.113.9")
	geoNode(t, a.gw.URL, idC, tokC, "198.51.100.9")
	a.waitNodes(t, 2)

	got, code, body := servedBy(t, a.rig, keyB, 1)
	if len(got) != 0 || code != 503 || !strings.Contains(body, "allow volunteers outside Europe") {
		t.Fatalf("no opt-in, only nodes abroad or mismatched: %v %d %s", got, code, body)
	}
	_, page := a.get(t, "/account", sC)
	if !strings.Contains(page, "connects from Sweden, not the country you state") {
		t.Errorf("account page does not explain the mismatch")
	}

	a.setTransfer(t, sB, true)
	if got, _, body := servedBy(t, a.rig, keyB, 8); got["from-"+idA]+got["from-"+idC] != 8 || got["from-"+idA] == 0 || got["from-"+idC] == 0 {
		t.Fatalf("with the opt-in: %v %s", got, body)
	}
	_, page = a.get(t, "/account", sB)
	if !strings.Contains(page, "Stop: Europe only") {
		t.Errorf("account page does not show the opt-in as on")
	}

	// An EEA node whose address matches; the opt-in withdrawn: only it serves.
	sD, tokD, idD := a.operator(t, 4)
	a.acceptTerms(t, sD, "SE")
	geoNode(t, a.gw.URL, idD, tokD, "198.51.100.10")
	a.waitNodes(t, 3)
	a.setTransfer(t, sB, false)
	if got, _, body := servedBy(t, a.rig, keyB, 10); got["from-"+idD] != 10 {
		t.Fatalf("no opt-in reached a node abroad or mismatched: %v %s", got, body)
	}
	// The front end: the account's chat follows its setting; an anonymous user has no opt-in.
	front := func(email, uid string) map[string]int {
		got := map[string]int{}
		for i := 0; i < 6; i++ {
			req, _ := http.NewRequest("POST", a.front.URL+"/v1/chat/completions",
				strings.NewReader(fmt.Sprintf(`{"model":"deep","messages":[{"role":"user","content":"front %d"}]}`, i)))
			req.Header.Set("Authorization", "Bearer owui-key")
			req.Header.Set("Content-Type", "application/json")
			if email != "" {
				req.Header.Set("X-OpenWebUI-User-Email", email)
			}
			req.Header.Set("X-OpenWebUI-User-Id", uid)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			var b strings.Builder
			buf := make([]byte, 4096)
			for {
				n, err := resp.Body.Read(buf)
				b.Write(buf[:n])
				if err != nil {
					break
				}
			}
			resp.Body.Close()
			for _, id := range []string{idA, idC, idD} {
				if strings.Contains(b.String(), "from-"+id) {
					got[id]++
				}
			}
		}
		return got
	}
	if got := front("", "anon-1"); got[idA]+got[idC] != 0 || got[idD] != 6 {
		t.Fatalf("anonymous front-end user: %v", got)
	}
	a.setTransfer(t, sB, true)
	if got := front("gh-2@users.invalid", "u-2"); got[idA]+got[idC] == 0 {
		t.Fatalf("the account's chat ignores its opt-in: %v", got)
	}
}

// The opt-in ticked on the sign-in form is recorded; unticked, it is not, and a later sign-in
// without the tick withdraws nothing. The form states the age limit.
func TestChatRulesSignInTick(t *testing.T) {
	a, _ := rulesRig(t)
	_, page := a.get(t, "/account", "")
	for _, want := range []string{`name="transfer" value="1"`, html.EscapeString(gateway.TransferConsentText), "18 or older"} {
		if !strings.Contains(page, want) {
			t.Errorf("sign-in page lacks %q", want)
		}
	}
	if strings.Contains(page, `name="transfer" value="1" checked`) {
		t.Error("the opt-in is ticked by default")
	}
	signIn := func(id int, form string) {
		loc, oauth := a.startForm(t, "/account/login", form, "")
		if resp := a.githubBack(t, loc, oauth, id, ""); resp.StatusCode != http.StatusFound {
			t.Fatalf("sign-in %d: %d", id, resp.StatusCode)
		}
	}
	signIn(5, "provider=github")
	signIn(6, "provider=github&transfer=1")
	a.store.Refresh()
	if x := snap(a).Find("gh:5"); x.TransferConsent != nil {
		t.Fatalf("unticked sign-in recorded an opt-in: %+v", x.TransferConsent)
	}
	x := snap(a).Find("gh:6")
	if x.TransferConsent == nil || x.TransferConsent.Version != gateway.TransferConsentVersion || x.TransferConsent.Via != gateway.ViaSignIn {
		t.Fatalf("ticked sign-in: %+v", x.TransferConsent)
	}
	signIn(6, "provider=github")
	a.store.Refresh()
	if snap(a).Find("gh:6").TransferConsent == nil {
		t.Fatal("a sign-in without the tick withdrew the opt-in")
	}
}

// The operator's acceptance and the user's opt-in survive a restart: a new gateway on the same
// accounts file serves the same chat on the same node.
func TestChatRulesSurviveRestart(t *testing.T) {
	a, cfg := rulesRig(t)
	sA, tokA, idA := a.operator(t, 1)
	sB, keyB := a.apiUser(t, 2)
	a.acceptTerms(t, sA, "other")
	a.setTransfer(t, sB, true)
	a.g.Close()

	g2, err := gateway.New(*cfg, quiet())
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(g2.Handler())
	t.Cleanup(func() { srv.Close(); g2.Close() })
	geoNode(t, srv.URL, idA, tokA, "203.0.113.20") // the US: needs both decisions
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

// BOINC-first operators: the connector records its user's acceptance (the bridge, when BOINC records
// the ENROLL consent); that user's pilot then serves chat. A mismatched country keeps it to users
// with the opt-in.
func TestChatRulesConnectorTerms(t *testing.T) {
	a, _ := rulesRig(t)
	_, keyB := a.apiUser(t, 2)
	engine := mockEngine(t, false)
	t.Cleanup(engine.Close)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pid, cred := a.newPilot(t)
	a.connector(t, "POST", "/pilots/"+pid+"/bind", map[string]any{"user": "42", "host": "11"})
	a.runPilot(ctx, cred, engine.URL) // from loopback: Norway
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
		{"version": gateway.OperatorTermsVersion, "accepted_at": time.Now(), "country": "Norway"},
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

	out := a.connector(t, "POST", "/users/42/operator-terms", map[string]any{"version": gateway.OperatorTermsVersion,
		"accepted_at": time.Now().Add(-time.Hour), "country": "SE"})
	if out["recorded"] != true || out["current"] != true || out["account_kind"] != "unclaimed" {
		t.Fatalf("connector terms answer: %v", out)
	}
	// Accepted, but it connects from Norway and its user states Sweden: only users with the opt-in.
	if code, body := ask(t, a.rig, keyB, deepBody); code != 503 || !strings.Contains(body, "allow volunteers outside Europe") {
		t.Fatalf("mismatched pilot served a user without the opt-in: %d %s", code, body)
	}
	a.connector(t, "POST", "/users/42/operator-terms", map[string]any{"version": gateway.OperatorTermsVersion,
		"accepted_at": time.Now(), "country": "no"})
	if code, body := ask(t, a.rig, keyB, deepBody); code != 200 || !strings.Contains(body, "Hello") {
		t.Fatalf("chat on the accepted pilot in Norway: %d %s", code, body)
	}
	// An older terms version is recorded but lets nothing serve chat.
	out = a.connector(t, "POST", "/users/42/operator-terms", map[string]any{"version": "old-1", "accepted_at": time.Now()})
	if out["current"] != false {
		t.Fatalf("old version: %v", out)
	}
	if code, _ := ask(t, a.rig, keyB, deepBody); code != 503 {
		t.Fatalf("chat on a pilot whose user accepted only an old version: %d", code)
	}
}

// Trusted nodes need neither the operator terms nor a country, and a config token can carry both.
func TestChatRulesConfigTokens(t *testing.T) {
	a, saved := rulesRig(t)
	_, keyB := a.apiUser(t, 2)
	cfg := *saved
	cfg.NodeTokens = map[string]gateway.NodeToken{
		"home-token":  {Operator: "owner", Trust: "trusted"},
		"vol-token":   {Operator: "vol", Country: "NO"},
		"volok-token": {Operator: "volok", Country: "SE", OperatorTerms: gateway.OperatorTermsVersion},
	}
	if err := a.g.Reload(cfg); err != nil {
		t.Fatal(err)
	}
	geoNode(t, a.gw.URL, "home", "home-token", "203.0.113.1") // trusted, in the US
	a.waitNodes(t, 1)
	if got, _, body := servedBy(t, a.rig, keyB, 2); got["from-home"] != 2 {
		t.Fatalf("trusted node: %v %s", got, body)
	}
	geoNode(t, a.gw.URL, "vol", "vol-token", "127.0.0.2")        // Norway, no terms
	geoNode(t, a.gw.URL, "volok", "volok-token", "198.51.100.2") // Sweden, accepted
	a.waitNodes(t, 3)
	got, _, _ := servedBy(t, a.rig, keyB, 12)
	if got["from-vol"] != 0 || got["from-volok"] == 0 {
		t.Fatalf("config tokens: %v", got)
	}
}
