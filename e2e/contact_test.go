// The contact address (opt-in "Email me about important changes and outages"): the box starts
// unticked; only a ticked sign-in asks GitHub (user:email) or Google (email) for an address or
// keeps an email sign-in's typed address; only verified addresses are kept; turning it off
// deletes it; the address never reaches the log. And the operator's notice: escaped, dismissible,
// reloaded from the accounts file, and offered to the chat as its banner.
//
// SPDX-License-Identifier: Apache-2.0
package e2e

import (
	"crypto/rsa"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aisathome/aisathome/gateway"
)

// fakeGitHubEmails answers GET /user/emails for the token of user id with emails[id], and
// counts the calls.
type fakeGitHubEmails struct {
	*httptest.Server
	mu     sync.Mutex
	emails map[string]string // numeric id → JSON list
	calls  int
}

func newFakeGitHubEmails(t *testing.T) *fakeGitHubEmails {
	f := &fakeGitHubEmails{emails: map[string]string{}}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer tok-")
		f.mu.Lock()
		f.calls++
		list, ok := f.emails[id]
		f.mu.Unlock()
		if !ok {
			http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound) // as without scope user:email
			return
		}
		io.WriteString(w, list)
	}))
	t.Cleanup(f.Close)
	return f
}

func (f *fakeGitHubEmails) set(id int, list string) {
	f.mu.Lock()
	f.emails[fmt.Sprint(id)] = list
	f.mu.Unlock()
}
func (f *fakeGitHubEmails) count() int { f.mu.Lock(); defer f.mu.Unlock(); return f.calls }

// postForm posts a form as the account page's own forms do (same origin), with session.
func (a *acctRig) postForm(t *testing.T, path, form, session string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest("POST", a.gw.URL+path, strings.NewReader(form))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	if session != "" {
		req.Header.Set("Cookie", session)
	}
	resp, err := noRedirect.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	return resp
}

// startForm posts the sign-in form (or another form that starts a provider's flow) and returns
// the redirect to the provider and the flow cookie.
func (a *acctRig) startForm(t *testing.T, path, form, session string) (*url.URL, string) {
	t.Helper()
	resp := a.postForm(t, path, form, session)
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("%s %s: %d", path, form, resp.StatusCode)
	}
	loc, _ := url.Parse(resp.Header.Get("Location"))
	return loc, cookieValue(resp, "aisathome_oauth")
}

// joinCookies joins "name=value" pairs, skipping empty ones.
func joinCookies(c ...string) string {
	var out []string
	for _, v := range c {
		if v != "" {
			out = append(out, v)
		}
	}
	return strings.Join(out, "; ")
}

// githubBack finishes a GitHub flow as user id (the fake's code "c<id>").
func (a *acctRig) githubBack(t *testing.T, loc *url.URL, oauth string, id int, session string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest("GET", fmt.Sprintf("%s/account/callback?code=c%d&state=%s", a.gw.URL, id, loc.Query().Get("state")), nil)
	req.Header.Set("Cookie", joinCookies(oauth, session))
	resp, err := noRedirect.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	return resp
}

// googleBack finishes a Google flow as sub through the fake's consent.
func (a *acctRig) googleBack(t *testing.T, loc *url.URL, oauth, sub, session string) *http.Response {
	t.Helper()
	resp, err := noRedirect.Get(loc.String() + "&as=" + url.QueryEscape(sub))
	if err != nil || resp.StatusCode != http.StatusFound {
		t.Fatalf("fake authorize: %v %v", err, resp)
	}
	back, _ := url.Parse(resp.Header.Get("Location"))
	req, _ := http.NewRequest("GET", a.gw.URL+back.Path+"?"+back.RawQuery, nil)
	req.Header.Set("Cookie", joinCookies(oauth, session))
	if resp, err = noRedirect.Do(req); err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	return resp
}

// contactOf is the account's stored contact fields, from the file as the gateway wrote it.
func (a *acctRig) contactOf(t *testing.T, id string) (addr, source string, at *time.Time) {
	t.Helper()
	a.store.Refresh()
	f := a.store.Snapshot()
	x := f.Find(id)
	if x == nil {
		t.Fatalf("no account %s", id)
	}
	return x.ContactEmail, x.ContactSource, x.ContactConsentAt
}

func fileHas(t *testing.T, path, s string) bool {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return strings.Contains(string(raw), s)
}

const contactBox = `<input type="checkbox" name="contact" value="1" id="contact">`

func TestContactGitHub(t *testing.T) {
	logs := &syncBuf{}
	logSink = logs
	defer func() { logSink = io.Discard }()
	ghe := newFakeGitHubEmails(t)
	a := newAcctRig(t, 100, func(c *gateway.AccountsConfig) { c.GitHubEmailsURL = ghe.URL + "/user/emails" })
	for id := 1; id <= 4; id++ {
		ghe.set(id, fmt.Sprintf(`[{"email":"other%d@example.com","primary":false,"verified":true},
			{"email":"primary%d@example.com","primary":true,"verified":true,"visibility":"private"}]`, id, id))
	}

	// The box is there and unticked.
	if _, page := a.get(t, "/account?next=chat", ""); !strings.Contains(page, contactBox) ||
		!strings.Contains(page, "Email me about important changes and outages") {
		t.Fatalf("sign-in page lacks the unticked box: %s", page)
	}

	// Unticked: no scope, no address asked for, none stored.
	loc, oauth := a.startForm(t, "/account/login", "provider=github&next=chat&email=", "")
	if loc.Query().Has("scope") {
		t.Fatalf("unticked sign-in asks for scope %q", loc.Query().Get("scope"))
	}
	resp := a.githubBack(t, loc, oauth, 1, "")
	s1 := cookieValue(resp, "aisathome_session")
	if s1 == "" || resp.Header.Get("Location") != "https://chat.site.test" {
		t.Fatalf("unticked sign-in: %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
	if addr, _, _ := a.contactOf(t, "gh:1"); addr != "" || ghe.count() != 0 {
		t.Fatalf("unticked sign-in stored %q (%d address lookups)", addr, ghe.count())
	}

	// A link (GET) cannot tick the box for someone, and neither can another site's form.
	r, _ := a.get(t, "/account/login?provider=github&contact=1", "")
	if l, _ := url.Parse(r.Header.Get("Location")); l.Query().Has("scope") {
		t.Fatalf("a link asked for scope: %s", l)
	}
	req, _ := http.NewRequest("POST", a.gw.URL+"/account/login", strings.NewReader("provider=github&contact=1"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	if r, _ := noRedirect.Do(req); r.Header.Get("Location") != "https://site.test/account" || cookieValue(r, "aisathome_oauth") != "" {
		t.Fatalf("cross-site sign-in form: %d %s", r.StatusCode, r.Header.Get("Location"))
	}

	// Ticked: scope user:email, the primary verified address is kept, with the time and source.
	loc, oauth = a.startForm(t, "/account/login", "provider=github&next=chat&contact=1", "")
	if loc.Query().Get("scope") != "user:email" {
		t.Fatalf("ticked sign-in: scope %q", loc.Query().Get("scope"))
	}
	before := time.Now().Add(-time.Second)
	resp = a.githubBack(t, loc, oauth, 2, "")
	s2 := cookieValue(resp, "aisathome_session")
	if s2 == "" || resp.Header.Get("Location") != "https://chat.site.test" {
		t.Fatalf("ticked sign-in: %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
	if addr, src, at := a.contactOf(t, "gh:2"); addr != "primary2@example.com" || src != "github" || at == nil || at.Before(before) {
		t.Fatalf("stored %q %q %v", addr, src, at)
	}
	_, page := a.get(t, "/account", s2)
	if !strings.Contains(page, "pr•••@example.com") || strings.Contains(page, "primary2@") ||
		!strings.Contains(page, `<span class="badge on">On</span>`) || !strings.Contains(page, "the address is from GitHub") {
		t.Fatalf("account page: %s", page)
	}

	// Ticked, but GitHub has no verified primary address: sign-in works, nothing is stored, and
	// the account page says why.
	ghe.set(3, `[{"email":"primary3@example.com","primary":true,"verified":false},{"email":"other3@example.com","primary":false,"verified":true}]`)
	loc, oauth = a.startForm(t, "/account/login", "provider=github&next=chat&contact=1", "")
	resp = a.githubBack(t, loc, oauth, 3, "")
	s3 := cookieValue(resp, "aisathome_session")
	if s3 == "" || resp.Header.Get("Location") != "https://site.test/account?contact=none#contact" {
		t.Fatalf("no verified address: %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
	if addr, _, _ := a.contactOf(t, "gh:3"); addr != "" {
		t.Fatalf("an unverified address was stored: %q", addr)
	}
	if _, page := a.get(t, "/account?contact=none", s3); !strings.Contains(page, "GitHub gave no verified email address, so none is stored") {
		t.Fatalf("account page does not say why: %s", page)
	}
	// GitHub refusing the list (no scope granted) is the same: no address, sign-in works.
	loc, oauth = a.startForm(t, "/account/login", "provider=github&contact=1", "")
	if resp := a.githubBack(t, loc, oauth, 7, ""); cookieValue(resp, "aisathome_session") == "" {
		t.Fatalf("sign-in failed when GitHub refused the address list: %d", resp.StatusCode)
	}

	// Turning it off deletes the address at once, from the file too.
	if r := a.postForm(t, "/account/contact/off", "", s2); r.Header.Get("Location") != "https://site.test/account#contact" {
		t.Fatalf("off: %d %s", r.StatusCode, r.Header.Get("Location"))
	}
	if addr, src, at := a.contactOf(t, "gh:2"); addr != "" || src != "" || at != nil || fileHas(t, a.file, "primary2@") {
		t.Fatalf("after off: %q %q %v", addr, src, at)
	}
	if _, page := a.get(t, "/account", s2); !strings.Contains(page, `<span class="badge off">Off</span>`) ||
		!strings.Contains(page, `action="/account/contact/connect"`) {
		t.Fatalf("account page after off: %s", page)
	}

	// "Connect email" on the account page: GitHub again with the scope, for this account only.
	loc, oauth = a.startForm(t, "/account/contact/connect", "", s1)
	if loc.Query().Get("scope") != "user:email" {
		t.Fatalf("connect: scope %q", loc.Query().Get("scope"))
	}
	if r := a.githubBack(t, loc, oauth, 4, s1); r.StatusCode != http.StatusBadRequest || cookieValue(r, "aisathome_session") != "" {
		t.Fatalf("connect as another GitHub user: %d", r.StatusCode)
	}
	if a.store.Refresh(); func() bool { f := a.store.Snapshot(); return f.Find("gh:4") != nil }() {
		t.Fatal("connect as another user made an account")
	}
	if addr, _, _ := a.contactOf(t, "gh:1"); addr != "" {
		t.Fatalf("connect as another user stored %q", addr)
	}
	loc, oauth = a.startForm(t, "/account/contact/connect", "", s1)
	if r := a.githubBack(t, loc, oauth, 1, s1); r.Header.Get("Location") != "https://site.test/account#contact" {
		t.Fatalf("connect: %d %s", r.StatusCode, r.Header.Get("Location"))
	}
	if addr, src, _ := a.contactOf(t, "gh:1"); addr != "primary1@example.com" || src != "github" {
		t.Fatalf("connect stored %q %q", addr, src)
	}
	// Signing in again without the box keeps the choice (only the account page turns it off).
	if s, _ := a.signIn(t, 1); s == "" {
		t.Fatal("sign-in again")
	}
	if addr, _, _ := a.contactOf(t, "gh:1"); addr != "primary1@example.com" {
		t.Fatalf("an unticked sign-in changed the address: %q", addr)
	}
	// A ban deletes it.
	if err := a.store.SetStatus("gh:1", gateway.StatusBanned); err != nil {
		t.Fatal(err)
	}
	if addr, _, _ := a.contactOf(t, "gh:1"); addr != "" || fileHas(t, a.file, "primary1@") {
		t.Fatalf("banned account keeps %q", addr)
	}

	for _, leak := range []string{"@example.com", "tok-"} {
		if strings.Contains(logs.String(), leak) {
			t.Fatalf("log holds %q:\n%s", leak, logs.String())
		}
	}
}

func TestContactGoogle(t *testing.T) {
	logs := &syncBuf{}
	logSink = logs
	defer func() { logSink = io.Discard }()
	a, fg := newGoogleRig(t, 100, nil)

	// Unticked: scope "openid"; the token's (verified) address is not kept.
	loc, oauth := a.startForm(t, "/account/login", "provider=google", "")
	if loc.Query().Get("scope") != "openid" {
		t.Fatalf("unticked: scope %q", loc.Query().Get("scope"))
	}
	if r := a.googleBack(t, loc, oauth, "111", ""); cookieValue(r, "aisathome_session") == "" {
		t.Fatalf("unticked sign-in: %d", r.StatusCode)
	}
	if addr, _, _ := a.contactOf(t, "google:111"); addr != "" || fileHas(t, a.file, "gmail.example") {
		t.Fatalf("unticked sign-in stored %q", addr)
	}

	// Ticked: scope "openid email"; a verified address is kept.
	loc, oauth = a.startForm(t, "/account/login", "provider=google&contact=1", "")
	if loc.Query().Get("scope") != "openid email" {
		t.Fatalf("ticked: scope %q", loc.Query().Get("scope"))
	}
	if r := a.googleBack(t, loc, oauth, "222", ""); r.Header.Get("Location") != "https://site.test/account" {
		t.Fatalf("ticked sign-in: %d %s", r.StatusCode, r.Header.Get("Location"))
	}
	if addr, src, at := a.contactOf(t, "google:222"); addr != "someone@gmail.example" || src != "google" || at == nil {
		t.Fatalf("ticked stored %q %q %v", addr, src, at)
	}

	// Ticked, but Google does not call it verified (false, missing, or anything but true): none.
	for i, v := range []any{false, nil, "yes"} {
		fg.setMutate(func(_, claims map[string]any) *rsa.PrivateKey {
			if v == nil {
				delete(claims, "email_verified")
			} else {
				claims["email_verified"] = v
			}
			return nil
		})
		sub := fmt.Sprintf("33%d", i)
		loc, oauth = a.startForm(t, "/account/login", "provider=google&contact=1", "")
		r := a.googleBack(t, loc, oauth, sub, "")
		s := cookieValue(r, "aisathome_session")
		if s == "" || r.Header.Get("Location") != "https://site.test/account?contact=none#contact" {
			t.Fatalf("email_verified %v: %d %s", v, r.StatusCode, r.Header.Get("Location"))
		}
		if addr, _, _ := a.contactOf(t, "google:"+sub); addr != "" {
			t.Fatalf("email_verified %v: stored %q", v, addr)
		}
		if _, page := a.get(t, "/account?contact=none", s); !strings.Contains(page, "Google gave no verified email address") {
			t.Fatalf("account page does not say why: %s", page)
		}
	}
	// Some tokens carry "true" as a string; that is verified too.
	fg.setMutate(func(_, claims map[string]any) *rsa.PrivateKey { claims["email_verified"] = "true"; return nil })
	loc, oauth = a.startForm(t, "/account/login", "provider=google&contact=1", "")
	a.googleBack(t, loc, oauth, "444", "")
	if addr, _, _ := a.contactOf(t, "google:444"); addr != "someone@gmail.example" {
		t.Fatalf(`email_verified "true": stored %q`, addr)
	}
	fg.setMutate(nil)

	// Connect from the account page of the unticked account.
	s1, _ := a.signInGoogle(t, "111", nil)
	loc, oauth = a.startForm(t, "/account/contact/connect", "", s1)
	if loc.Query().Get("scope") != "openid email" {
		t.Fatalf("connect: scope %q", loc.Query().Get("scope"))
	}
	if r := a.googleBack(t, loc, oauth, "111", s1); r.Header.Get("Location") != "https://site.test/account#contact" {
		t.Fatalf("connect: %d %s", r.StatusCode, r.Header.Get("Location"))
	}
	if addr, src, _ := a.contactOf(t, "google:111"); addr != "someone@gmail.example" || src != "google" {
		t.Fatalf("connect stored %q %q", addr, src)
	}
	if strings.Contains(logs.String(), "gmail.example") {
		t.Fatalf("log holds the address:\n%s", logs.String())
	}
}

func TestContactEmailSignIn(t *testing.T) {
	logs := &syncBuf{}
	logSink = logs
	defer func() { logSink = io.Discard }()
	e := newEmailRig(t, 100, nil)
	ask := func(addr string, contact bool, n int) string {
		t.Helper()
		e.clock.Add(61 * time.Second)
		form := "email=" + urlEscape(addr)
		if contact {
			form += "&contact=1"
		}
		req, _ := http.NewRequest("POST", e.gw.URL+"/account/email", strings.NewReader(form))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Sec-Fetch-Site", "same-origin")
		req.Header.Set("X-Forwarded-For", e.ip)
		resp, err := noRedirect.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		m := e.smtp.wait(t, n)
		s, _, _ := e.verify(t, cookieValue(resp, "aisathome_pending"), "token="+m.token) // the link
		if s == "" {
			t.Fatal("no session")
		}
		return s
	}

	// Unticked: only the keyed hash, as before.
	s1 := ask("first@example.org", false, 1)
	id1 := e.accountOf(t, s1)
	if addr, _, _ := e.contactOf(t, id1); addr != "" || fileHas(t, e.file, "first@") {
		t.Fatalf("unticked stored %q", addr)
	}
	// Ticked: the address as typed, after the link proves it.
	s2 := ask("Second@Example.org", true, 2)
	id2 := e.accountOf(t, s2)
	if addr, src, at := e.contactOf(t, id2); addr != "Second@Example.org" || src != "email" || at == nil {
		t.Fatalf("ticked stored %q %q %v", addr, src, at)
	}
	if _, page := e.get(t, "/account", s2); !strings.Contains(page, "Se•••@Example.org") || !strings.Contains(page, "the address is from email sign-in") {
		t.Fatalf("account page: %s", page)
	}
	// Off deletes it.
	e.postForm(t, "/account/contact/off", "", s2)
	if addr, _, _ := e.contactOf(t, id2); addr != "" || fileHas(t, e.file, "Second@") {
		t.Fatalf("after off: %q", addr)
	}
	// On from the account page: the address must be the one the account signs in with.
	if _, page := e.get(t, "/account", s1); !strings.Contains(page, `action="/account/contact/email"`) {
		t.Fatalf("no way to turn it on: %s", page)
	}
	req, _ := http.NewRequest("POST", e.gw.URL+"/account/contact/email", strings.NewReader("email=second%40example.org"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	req.Header.Set("Cookie", s1)
	r, _ := noRedirect.Do(req)
	body, _ := io.ReadAll(r.Body)
	r.Body.Close()
	if r.StatusCode != http.StatusBadRequest || !strings.Contains(string(body), "That is not the address you sign in with") {
		t.Fatalf("another account's address: %d", r.StatusCode)
	}
	if addr, _, _ := e.contactOf(t, id1); addr != "" {
		t.Fatalf("a wrong address was stored: %q", addr)
	}
	e.postForm(t, "/account/contact/email", "email=First%40example.org", s1)
	if addr, src, _ := e.contactOf(t, id1); addr != "First@example.org" || src != "email" {
		t.Fatalf("turned on: %q %q", addr, src)
	}
	for _, leak := range []string{"irst@example", "econd@example", "Second@"} {
		if strings.Contains(logs.String(), leak) {
			t.Fatalf("log holds %q:\n%s", leak, logs.String())
		}
	}
}

var noticeRe = regexp.MustCompile(`<div class="card notice" role="status" data-notice="([0-9a-f]{12})"><p>(.*?)</p>`)

func TestNotice(t *testing.T) {
	e := newEmailRig(t, 100, nil)
	s, _ := e.signIn(t, 1)
	chat := func() []map[string]any {
		t.Helper()
		r, body := e.get(t, "/account/notice/chat", "")
		var out []map[string]any
		if r.StatusCode != http.StatusOK || r.Header.Get("Cache-Control") != "no-store" || json.Unmarshal([]byte(body), &out) != nil {
			t.Fatalf("chat banners: %d %q", r.StatusCode, body)
		}
		return out
	}
	if _, page := e.get(t, "/account", s); strings.Contains(page, `class="card notice"`) || len(chat()) != 0 {
		t.Fatal("a notice without one being set")
	}

	// The operator sets one with the CLI (another process changing the file): the gateway
	// picks it up within seconds, without a restart.
	cli, err := gateway.OpenAccountStore(e.file)
	if err != nil {
		t.Fatal(err)
	}
	const text = `Maintenance <script>alert(1)</script> *today* at https://example.com/x & "more" 'later'`
	if err := cli.SetNotice(text, time.Now()); err != nil {
		t.Fatal(err)
	}
	var id string
	waitFor(t, func() bool {
		_, page := e.get(t, "/account", s)
		m := noticeRe.FindStringSubmatch(page)
		if m != nil {
			id = m[1]
		}
		return m != nil
	})
	for _, p := range []struct{ path, session string }{{"/account", s}, {"/account?next=chat", ""}, {"/account/email", ""}} {
		_, page := e.get(t, p.path, p.session)
		m := noticeRe.FindStringSubmatch(page)
		if m == nil || m[1] != id || strings.Contains(page, "<script>alert") ||
			m[2] != `Maintenance &lt;script&gt;alert(1)&lt;/script&gt; *today* at https://example.com/x &amp; &#34;more&#34; &#39;later&#39;` {
			t.Fatalf("%s: notice %q", p.path, m)
		}
	}
	b := chat()
	if len(b) != 1 || b[0]["id"] != "aisathome-"+id || b[0]["type"] != "info" || b[0]["dismissible"] != true {
		t.Fatalf("chat banner: %v", b)
	}
	content, _ := b[0]["content"].(string)
	if content != `Maintenance &lt;script&gt;alert\(1\)&lt;\/script&gt; \*today\* at https\:\/\/example\.com\/x &amp; &quot;more&quot; &#39;later&#39;` {
		t.Fatalf("chat banner content: %q", content)
	}

	// Dismissing hides it in this browser (a cookie with its id); the next page load has none.
	r := e.postForm(t, "/account/notice/dismiss", "id="+id+"&back=%2Faccount%3Fnext%3Dchat", "")
	dismissed := cookieValue(r, "aisathome_notice")
	if r.Header.Get("Location") != "https://site.test/account?next=chat" || !strings.HasSuffix(dismissed, "="+id) {
		t.Fatalf("dismiss: %d %s %q", r.StatusCode, r.Header.Get("Location"), dismissed)
	}
	if _, page := e.get(t, "/account", joinCookies(s, dismissed)); strings.Contains(page, `class="card notice"`) {
		t.Fatal("dismissed notice still shown")
	}
	if r := e.postForm(t, "/account/notice/dismiss", "id=000000000000&back=https%3A%2F%2Fevil.test", ""); cookieValue(r, "aisathome_notice") != "" ||
		r.Header.Get("Location") != "https://site.test/account" {
		t.Fatalf("dismissing another id: %s %s", cookieValue(r, "aisathome_notice"), r.Header.Get("Location"))
	}
	// A new notice shows again, even to that browser.
	cli.SetNotice("Back to normal.", time.Now())
	waitFor(t, func() bool {
		_, page := e.get(t, "/account", joinCookies(s, dismissed))
		m := noticeRe.FindStringSubmatch(page)
		return m != nil && m[2] == "Back to normal." && m[1] != id
	})
	// Plain text on one line, at most 300 characters.
	for _, bad := range []string{"", "   ", "two\nlines", "tab\there", strings.Repeat("x", 301), "bidi \u202e"} {
		if err := cli.SetNotice(bad, time.Now()); err == nil {
			t.Fatalf("notice %q accepted", bad)
		}
	}
	if err := cli.SetNotice(strings.Repeat("é", 300), time.Now()); err != nil {
		t.Fatalf("300 characters: %v", err)
	}
	cli.ClearNotice()
	waitFor(t, func() bool { _, page := e.get(t, "/account", s); return !strings.Contains(page, `class="card notice"`) })
	if len(chat()) != 0 {
		t.Fatal("chat banner after clear")
	}
}
