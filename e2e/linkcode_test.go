// Linking a node to an account without pasting a token (gateway/nodelink.go, node/accountlink): the node
// asks for a code, a signed-in person approves it on /link, the node polls and gets its token once, and
// connects with it.
//
// SPDX-License-Identifier: Apache-2.0
package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"html"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aisathome/aisathome/gateway"
	"github.com/aisathome/aisathome/node/accountlink"
	"github.com/aisathome/aisathome/protocol"
)

var (
	linkIDRe   = regexp.MustCompile(`name="link" value="([A-Za-z0-9_-]+)"`)
	linkOnceRe = regexp.MustCompile(`name="once" value="([0-9a-f]+)"`)
)

// linkClock is the gateway's clock for links, moved by the test.
type linkClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *linkClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *linkClock) add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// codeRig is an accounts gateway with the chat rules on and a clock for links.
func codeRig(t *testing.T, maxActive int) (*acctRig, *linkClock) {
	clk := &linkClock{t: time.Now()}
	a := newAcctRigNodes(t, maxActive, nil, func(c *gateway.Config) {
		c.ChatRulesOff = false
		c.LinkNow = clk.now
	}, mockEngine(t, false))
	return a, clk
}

// linkPost calls a link endpoint as a node does.
func linkPost(t *testing.T, a *acctRig, path string, body any, out any) int {
	t.Helper()
	b, _ := json.Marshal(body)
	resp, err := http.Post(a.gw.URL+path, "application/json", bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if out != nil {
		json.NewDecoder(resp.Body).Decode(out)
	}
	return resp.StatusCode
}

func linkStart(t *testing.T, a *acctRig, info protocol.LinkInfo) protocol.LinkStart {
	t.Helper()
	var s protocol.LinkStart
	if code := linkPost(t, a, protocol.LinkPath, info, &s); code != 200 {
		t.Fatalf("link start: HTTP %d", code)
	}
	return s
}

func linkPoll(t *testing.T, a *acctRig, device string) protocol.LinkPoll {
	t.Helper()
	var p protocol.LinkPoll
	if code := linkPost(t, a, protocol.LinkPollPath, protocol.LinkPollRequest{DeviceCode: device}, &p); code != 200 {
		t.Fatalf("link poll: HTTP %d", code)
	}
	return p
}

// waiting: the link is neither approved nor denied (a test polls faster than the interval: slow_down).
func waiting(p protocol.LinkPoll) bool {
	return p.Status == protocol.LinkPending || p.Status == protocol.LinkSlowDown
}

// linkForm reads the approval form's fields from the page of code.
func linkForm(t *testing.T, page string) url.Values {
	t.Helper()
	f := url.Values{}
	if m := linkIDRe.FindStringSubmatch(page); m != nil {
		f.Set("link", m[1])
	}
	if m := linkOnceRe.FindStringSubmatch(page); m != nil {
		f.Set("once", m[1])
	}
	if m := nodeTermsRe.FindStringSubmatch(page); m != nil {
		f.Set("terms", html.UnescapeString(m[1]))
	}
	return f
}

// approveLink opens /link?code= as the signed-in browser does and presses action (approve or deny).
func approveLink(t *testing.T, a *acctRig, session, code, action string) (*http.Response, string) {
	t.Helper()
	_, page := a.get(t, "/link?code="+url.QueryEscape(code), session)
	f := linkForm(t, page)
	f.Set("code", code)
	f.Set("action", action)
	return a.post(t, "/link", session, f.Encode())
}

// The whole flow with the node's own client: the code leads to sign-in and back, the page shows what the
// node reported and the operator terms line, approving creates the node like "Add a node" (recorded as a
// link, with the acceptance), the node gets its token once and connects with it.
func TestLinkNodeToAccount(t *testing.T) {
	a, _ := codeRig(t, 100)
	base := a.gw.URL
	c := &accountlink.Client{Base: base, Sleep: func(ctx context.Context, d time.Duration) error {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(50 * time.Millisecond):
			return nil
		}
	}}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	st, err := c.Start(ctx, protocol.LinkInfo{Name: "den-<b>pc</b>", GPU: "NVIDIA GeForce RTX 4090", VRAMMiB: 24564,
		OS: "Ubuntu 24.04.1 LTS", Agent: "aisathome-node/9.9.9"})
	if err != nil {
		t.Fatal(err)
	}
	if st.VerificationURI != "https://site.test/link" || st.VerificationURIComplete != "https://site.test/link?code="+st.UserCode ||
		st.ExpiresIn != 600 || st.Interval != 5 || len(st.DeviceCode) < 40 {
		t.Fatalf("start: %+v", st)
	}
	type result struct {
		p   protocol.LinkPoll
		err error
	}
	done := make(chan result, 1)
	go func() { p, err := c.Wait(ctx, st); done <- result{p, err} }()

	// Signed out, the link sends the browser to sign in, and sign-in back to the link with its code.
	resp, _ := a.get(t, "/link?code="+strings.ToLower(st.UserCode), "")
	if loc := resp.Header.Get("Location"); resp.StatusCode != http.StatusSeeOther || loc != "https://site.test/account?next="+url.QueryEscape("link:"+st.UserCode) {
		t.Fatalf("signed out: %d %q", resp.StatusCode, loc)
	}
	_, page := a.get(t, "/account?next="+url.QueryEscape("link:"+st.UserCode), "")
	if !strings.Contains(page, `name="next" value="link:`+st.UserCode+`"`) || !strings.Contains(page, "Sign in to link your node") {
		t.Fatal("the sign-in page does not carry the link back")
	}
	resp, err = noRedirect.Get(base + "/account/login?next=" + url.QueryEscape("link:"+st.UserCode))
	if err != nil {
		t.Fatal(err)
	}
	loc, _ := url.Parse(resp.Header.Get("Location"))
	req, _ := http.NewRequest("GET", base+"/account/callback?code=c42&state="+loc.Query().Get("state"), nil)
	req.Header.Set("Cookie", cookieValue(resp, "aisathome_oauth"))
	if resp, err = noRedirect.Do(req); err != nil {
		t.Fatal(err)
	}
	session := cookieValue(resp, "aisathome_session")
	if got := resp.Header.Get("Location"); got != "https://site.test/link?code="+st.UserCode || session == "" {
		t.Fatalf("after sign-in: %q", got)
	}

	resp, page = a.get(t, "/link?code="+st.UserCode, session)
	for _, want := range []string{st.UserCode, "den-&lt;b&gt;pc&lt;/b&gt;", "NVIDIA GeForce RTX 4090", "(24 GB)", "Ubuntu 24.04.1 LTS",
		"aisathome-node/9.9.9", "nobody has checked them", "Only approve a code that a machine you run shows you",
		"By creating this node and running it you accept the", gateway.OperatorTermsURL, ">Approve<", ">Deny<"} {
		if !strings.Contains(page, want) {
			t.Errorf("approval page lacks %q", want)
		}
	}
	if csp := resp.Header.Get("Content-Security-Policy"); !strings.Contains(csp, "frame-ancestors 'none'") || strings.Contains(csp, "script-src") {
		t.Errorf("CSP %q", csp)
	}
	if strings.Contains(page, st.DeviceCode) {
		t.Fatal("the page shows the device code")
	}
	select {
	case r := <-done:
		t.Fatalf("the node got an answer before the approval: %+v %v", r.p, r.err)
	default:
	}
	f := linkForm(t, page)
	f.Set("code", st.UserCode)
	f.Set("action", "approve")
	f.Set("name", "den pc")
	_, page = a.post(t, "/link", session, f.Encode())
	if !strings.Contains(page, "Approved.") || !strings.Contains(page, "Running this node means you accept") {
		t.Fatalf("after approving: %s", page)
	}
	r := <-done
	if r.err != nil {
		t.Fatal(r.err)
	}
	id, ok := strings.CutPrefix(r.p.Token, "nt-")
	if !ok || !strings.HasPrefix(id, r.p.NodeID+".") || !strings.Contains(page, r.p.NodeID) {
		t.Fatalf("token %q for node %q", r.p.Token, r.p.NodeID)
	}
	if strings.Contains(page, r.p.Token) {
		t.Fatal("the approval page shows the token")
	}
	// Once: the next poll, and the code, are spent.
	if p := linkPoll(t, a, st.DeviceCode); p.Status != protocol.LinkExpired || p.Token != "" {
		t.Fatalf("second poll: %+v", p)
	}
	if _, page := a.get(t, "/link?code="+st.UserCode, session); !strings.Contains(page, "unknown or has expired") {
		t.Fatal("the used code still shows its link")
	}

	// The accounts file: the node, as a link, and the acceptance it recorded.
	a.store.Refresh()
	acct, _, _ := a.store.Get("gh:42")
	if len(acct.Nodes) != 1 || acct.Nodes[0].ID != r.p.NodeID || acct.Nodes[0].Via != gateway.ViaNodeLink || acct.Nodes[0].Name != "den pc" {
		t.Fatalf("nodes %+v", acct.Nodes)
	}
	if acct.OperatorTerms == nil || acct.OperatorTerms.Via != gateway.ViaNodeLinked || acct.OperatorTerms.Version != gateway.OperatorTermsVersion {
		t.Fatalf("operator terms %+v", acct.OperatorTerms)
	}

	// The token works: the node connects and is listed on the account page.
	a.startTokenNode(ctx, r.p.Token, mockEngine(t, false), 1, 8192)
	a.waitNodes(t, 2)
	if _, page := a.get(t, "/account", session); !strings.Contains(page, r.p.NodeID) || !strings.Contains(page, "den pc") {
		t.Fatal("the account page does not list the linked node")
	}
}

// Deny, a second approval, forms from elsewhere and stale forms change nothing; the node learns it was
// denied.
func TestLinkDenyAndForms(t *testing.T) {
	a, _ := codeRig(t, 100)
	s, _ := a.signIn(t, 7)
	st := linkStart(t, a, protocol.LinkInfo{Name: "x"})

	// A cross-site post (another origin), and a form whose token is not this account's.
	_, page := a.get(t, "/link?code="+st.UserCode, s)
	f := linkForm(t, page)
	f.Set("code", st.UserCode)
	f.Set("action", "approve")
	req, _ := http.NewRequest("POST", a.gw.URL+"/link", strings.NewReader(f.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Cookie", s)
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	if resp, err := noRedirect.Do(req); err != nil || resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("cross-site post: %v %v", resp, err)
	}
	s2, _ := a.signIn(t, 8)
	if _, page := a.post(t, "/link", s2, f.Encode()); !strings.Contains(page, "out of date") {
		t.Fatal("another account's form token was taken")
	}
	if p := linkPoll(t, a, st.DeviceCode); !waiting(p) {
		t.Fatalf("after refused forms: %+v", p)
	}

	if _, page := approveLink(t, a, s, st.UserCode, "deny"); !strings.Contains(page, "Denied.") {
		t.Fatalf("deny: %s", page)
	}
	if _, page := a.post(t, "/link", s, f.Encode()); strings.Contains(page, "Approved.") {
		t.Fatal("approved after deny")
	}
	if p := linkPoll(t, a, st.DeviceCode); p.Status != protocol.LinkDenied || p.Token != "" {
		t.Fatalf("poll after deny: %+v", p)
	}
	a.store.Refresh()
	if acct, _, _ := a.store.Get("gh:7"); len(acct.Nodes) != 0 {
		t.Fatalf("a denied link made a node: %+v", acct.Nodes)
	}

	// Approved twice: the second is refused, and only one node exists. The form here lacks the operator
	// terms line (as one opened before it existed, or crafted): the node is made, the terms not accepted.
	st = linkStart(t, a, protocol.LinkInfo{})
	_, page = a.get(t, "/link?code="+st.UserCode, s)
	f = linkForm(t, page)
	if f.Get("terms") == "" {
		t.Fatal("the approval form has no operator terms line")
	}
	f.Del("terms")
	f.Set("code", st.UserCode)
	f.Set("action", "approve")
	if _, page := a.post(t, "/link", s, f.Encode()); !strings.Contains(page, "Approved.") {
		t.Fatal("first approval")
	}
	if _, page := a.post(t, "/link", s, f.Encode()); !strings.Contains(page, "already approved or denied") {
		t.Fatalf("second approval: %s", page)
	}
	a.store.Refresh()
	if acct, _, _ := a.store.Get("gh:7"); len(acct.Nodes) != 1 || acct.OperatorTerms != nil {
		t.Fatalf("after a repeated approval without the terms line: %d nodes, terms %+v", len(acct.Nodes), acct.OperatorTerms)
	}
}

// After 10 minutes a code is gone: the page says so, the node's poll says expired; a node approved but
// never picked up is revoked.
func TestLinkExpiry(t *testing.T) {
	a, clk := codeRig(t, 100)
	s, _ := a.signIn(t, 7)
	st := linkStart(t, a, protocol.LinkInfo{})
	clk.add(10 * time.Minute)
	if _, page := a.get(t, "/link?code="+st.UserCode, s); !strings.Contains(page, "unknown or has expired") || strings.Contains(page, ">Approve<") {
		t.Fatal("an expired code still shows its link")
	}
	if p := linkPoll(t, a, st.DeviceCode); p.Status != protocol.LinkExpired {
		t.Fatalf("poll after expiry: %+v", p)
	}

	st = linkStart(t, a, protocol.LinkInfo{})
	if _, page := approveLink(t, a, s, st.UserCode, "approve"); !strings.Contains(page, "Approved.") {
		t.Fatal("approve")
	}
	a.store.Refresh()
	if acct, _, _ := a.store.Get("gh:7"); len(acct.Nodes) != 1 {
		t.Fatal("no node after approval")
	}
	clk.add(11 * time.Minute) // the node did not poll in time
	if p := linkPoll(t, a, st.DeviceCode); p.Status != protocol.LinkExpired || p.Token != "" {
		t.Fatalf("late poll: %+v", p)
	}
	a.store.Refresh()
	if acct, _, _ := a.store.Get("gh:7"); len(acct.Nodes) != 0 {
		t.Fatalf("the node nobody picked up is still there: %+v", acct.Nodes)
	}
}

// Codes per address are limited (in memory); the answer says so and when to try again.
func TestLinkRateLimit(t *testing.T) {
	a, clk := codeRig(t, 100)
	for i := 0; i < 20; i++ { // gateway/nodelink.go linkPerClient: 20 in 10 minutes
		linkStart(t, a, protocol.LinkInfo{})
	}
	resp, err := http.Post(a.gw.URL+protocol.LinkPath, "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusTooManyRequests || resp.Header.Get("Retry-After") == "" {
		t.Fatalf("21st code: %d", resp.StatusCode)
	}
	clk.add(10 * time.Minute)
	linkStart(t, a, protocol.LinkInfo{})
	// Not JSON: refused (a web page cannot ask for codes with a simple form post).
	resp, _ = http.Post(a.gw.URL+protocol.LinkPath, "text/plain", strings.NewReader("{}"))
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnsupportedMediaType {
		t.Fatalf("text/plain: %d", resp.StatusCode)
	}
}

// The limits of "Add a node" apply: a waitlisted account cannot link (and cannot look codes up), an
// account at its node limit is told why and its approval fails with the node waiting, and an account that
// chose science only stays so.
func TestLinkLimitsAndWaitlist(t *testing.T) {
	a, _ := codeRig(t, 1)
	s1, _ := a.signIn(t, 1) // active (the one place)
	s2, _ := a.signIn(t, 2) // waitlisted
	st := linkStart(t, a, protocol.LinkInfo{GPU: "RTX 3090"})
	_, page := a.get(t, "/link?code="+st.UserCode, s2)
	if !strings.Contains(page, "on the waitlist") || strings.Contains(page, "RTX 3090") || strings.Contains(page, ">Approve<") {
		t.Fatalf("waitlisted: %s", page)
	}
	_, page1 := a.get(t, "/link?code="+st.UserCode, s1) // a form of an active account, posted by the waitlisted one
	f := linkForm(t, page1)
	f.Set("code", st.UserCode)
	f.Set("action", "approve")
	if resp, _ := a.post(t, "/link", s2, f.Encode()); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("waitlisted approval: %d", resp.StatusCode)
	}
	if p := linkPoll(t, a, st.DeviceCode); !waiting(p) {
		t.Fatalf("after the waitlisted approval: %+v", p)
	}

	a.store.Update(func(f *gateway.AccountsFile) error { f.Settings.MaxNodesPerAccount = 1; return nil })
	if tok, _, _ := a.addNode(t, s1, "first"); tok == "" {
		t.Fatal("first node")
	}
	waitFor(t, func() bool {
		_, p := a.get(t, "/link?code="+st.UserCode, s1)
		return strings.Contains(p, "You have 1 of 1 nodes")
	})
	_, page = a.get(t, "/link?code="+st.UserCode, s1)
	if strings.Contains(page, ">Approve<") || !strings.Contains(page, ">Deny<") {
		t.Fatal("at the node limit the page offers Approve, or no Deny")
	}
	f.Set("once", linkOnceRe.FindStringSubmatch(page)[1])
	if _, page := a.post(t, "/link", s1, f.Encode()); !strings.Contains(page, "at most 1 nodes per account") {
		t.Fatalf("approval at the limit: %s", page)
	}
	if p := linkPoll(t, a, st.DeviceCode); !waiting(p) {
		t.Fatalf("after the refused approval: %+v", p)
	}
	a.store.Update(func(f *gateway.AccountsFile) error { f.Settings.MaxNodesPerAccount = 0; return nil })

	// Science only, as chosen: approving does not accept the terms.
	if resp, _ := a.post(t, "/account/operator", s1, "action=withdraw"); resp.StatusCode != http.StatusSeeOther {
		t.Fatal("withdraw")
	}
	waitFor(t, func() bool { _, p := a.get(t, "/link?code="+st.UserCode, s1); return strings.Contains(p, ">Approve<") })
	_, page = a.get(t, "/link?code="+st.UserCode, s1)
	if !strings.Contains(page, "Your nodes do science only, as you chose; a new node does too.") || strings.Contains(page, `name="terms"`) {
		t.Fatal("science only: the page offers the terms")
	}
	if _, page := approveLink(t, a, s1, st.UserCode, "approve"); !strings.Contains(page, "Your nodes do science only, as you chose; this one too.") {
		t.Fatalf("approve, science only: %s", page)
	}
	a.store.Refresh()
	if acct, _, _ := a.store.Get("gh:1"); acct.OperatorTerms != nil || len(acct.Nodes) != 2 {
		t.Fatalf("science only: terms %+v, %d nodes", acct.OperatorTerms, len(acct.Nodes))
	}
}

// Wrong codes are counted per account: after ten in an hour even the right one is refused.
func TestLinkWrongCodes(t *testing.T) {
	a, clk := codeRig(t, 100)
	s, _ := a.signIn(t, 7)
	st := linkStart(t, a, protocol.LinkInfo{})
	for i := 0; i < 10; i++ {
		if _, page := a.get(t, "/link?code=BCDF-000"+string(rune('0'+i)), s); !strings.Contains(page, "unknown or has expired") {
			t.Fatalf("wrong code %d", i)
		}
	}
	if _, page := a.get(t, "/link?code="+st.UserCode, s); !strings.Contains(page, "Too many wrong codes") {
		t.Fatal("the right code after ten wrong ones")
	}
	clk.add(time.Hour)
	st = linkStart(t, a, protocol.LinkInfo{})
	if _, page := a.get(t, "/link?code="+st.UserCode, s); !strings.Contains(page, ">Approve<") {
		t.Fatal("an hour later")
	}
}
