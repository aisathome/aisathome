// Accounts (decision 0006): GitHub sign-in, waitlist and cap, self-service keys, the chat's
// forward_auth check, and one budget for an account's chat and API use.
//
// SPDX-License-Identifier: Apache-2.0
package e2e

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aisathome/aisathome/gateway"
)

// fakeGitHub accepts code "c<id>" and answers /user with that numeric id.
func fakeGitHub(t *testing.T) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/token":
			r.ParseForm()
			if r.Form.Get("client_secret") != "gh-secret" || !strings.HasPrefix(r.Form.Get("code"), "c") {
				json.NewEncoder(w).Encode(map[string]string{"error": "bad_verification_code"})
				return
			}
			json.NewEncoder(w).Encode(map[string]string{"access_token": "tok-" + r.Form.Get("code")[1:]})
		case "/user":
			id := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer tok-")
			fmt.Fprintf(w, `{"id": %s, "login": "someone", "email": "someone@example.com"}`, id)
		}
	}))
}

type acctRig struct {
	*rig
	store *gateway.AccountStore
	file  string
}

func newAcctRig(t *testing.T, maxActive int, mod func(*gateway.AccountsConfig)) *acctRig {
	return newAcctRigWith(t, maxActive, mod, nil)
}

// newAcctRigWith also lets a test adjust the rest of the gateway config.
func newAcctRigWith(t *testing.T, maxActive int, mod func(*gateway.AccountsConfig), extra func(*gateway.Config)) *acctRig {
	return newAcctRigNodes(t, maxActive, mod, extra, mockEngine(t, false))
}

// newAcctRigNodes starts one config-token node (operator op1, untrusted) per engine.
func newAcctRigNodes(t *testing.T, maxActive int, mod func(*gateway.AccountsConfig), extra func(*gateway.Config), engines ...*httptest.Server) *acctRig {
	gh := fakeGitHub(t)
	t.Cleanup(gh.Close)
	file := filepath.Join(t.TempDir(), "accounts.json")
	store, err := gateway.OpenAccountStore(file)
	if err != nil {
		t.Fatal(err)
	}
	store.Update(func(f *gateway.AccountsFile) error { f.Settings.MaxActive = maxActive; return nil })
	r := newRigCfg(t, func(c *gateway.Config) {
		c.Accounts = &gateway.AccountsConfig{File: file, PublicURL: "https://site.test", CookieDomain: "site.test",
			SessionSecret: strings.Repeat("s", 32), GitHubClientID: "gh-id", GitHubClientSecret: "gh-secret",
			ChatURL: "https://chat.site.test", Limits: gateway.UserLimits{TokensPerDay: 1000}, MinTrust: "any",
			Admins:        []string{"gh:1"},
			GitHubAuthURL: gh.URL + "/authorize", GitHubTokenURL: gh.URL + "/token", GitHubUserURL: gh.URL + "/user"}
		c.Keys["owui-key"] = gateway.KeyInfo{User: "owui", ForwardUsers: true}
		if mod != nil {
			mod(c.Accounts)
		}
		if extra != nil {
			extra(c)
		}
	}, engines...)
	return &acctRig{rig: r, store: store, file: file}
}

var noRedirect = &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

// cookieValue returns "name=value" of the cookie whose name starts with prefix (cookie names
// carry a per-environment suffix).
func cookieValue(resp *http.Response, prefix string) string {
	for _, c := range resp.Cookies() {
		if strings.HasPrefix(c.Name, prefix+"_") && c.MaxAge >= 0 {
			return c.Name + "=" + c.Value
		}
	}
	return ""
}

// signIn runs the GitHub flow for numeric id and returns the session cookie value.
func (a *acctRig) signIn(t *testing.T, id int) (string, *http.Response) {
	t.Helper()
	resp, err := noRedirect.Get(a.gw.URL + "/account/login")
	if err != nil {
		t.Fatal(err)
	}
	loc, _ := url.Parse(resp.Header.Get("Location"))
	state := loc.Query().Get("state")
	oauth := cookieValue(resp, "aisathome_oauth")
	req, _ := http.NewRequest("GET", fmt.Sprintf("%s/account/callback?code=c%d&state=%s", a.gw.URL, id, state), nil)
	req.Header.Set("Cookie", oauth)
	resp, err = noRedirect.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return cookieValue(resp, "aisathome_session"), resp
}

func (a *acctRig) get(t *testing.T, path, session string) (*http.Response, string) {
	t.Helper()
	req, _ := http.NewRequest("GET", a.gw.URL+path, nil)
	if session != "" {
		req.Header.Set("Cookie", session)
	}
	resp, err := noRedirect.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp, string(b)
}

func (a *acctRig) post(t *testing.T, path, session, form string) (*http.Response, string) {
	t.Helper()
	req, _ := http.NewRequest("POST", a.gw.URL+path, strings.NewReader(form))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Cookie", session)
	req.Header.Set("Origin", "https://site.test")
	resp, err := noRedirect.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp, string(b)
}

var keyRe = regexp.MustCompile(`ak-[A-Za-z0-9_-]{40,}`)
var onceRe = regexp.MustCompile(`name="once" value="([0-9a-f]+)"`)

// createKey creates a key the way the page does (with the form's one-time token).
func (a *acctRig) createKey(t *testing.T, session string) (*http.Response, string) {
	t.Helper()
	_, page := a.get(t, "/account", session)
	once := ""
	if m := onceRe.FindStringSubmatch(page); m != nil {
		once = m[1]
	}
	return a.post(t, "/account/keys", session, "once="+once)
}

func TestAccountSignInStoresOnlyTheID(t *testing.T) {
	a := newAcctRig(t, 100, nil)
	session, resp := a.signIn(t, 1)
	if session == "" || resp.StatusCode != http.StatusFound {
		t.Fatalf("sign-in: %d, session %q", resp.StatusCode, session)
	}
	a.store.Refresh() // the gateway wrote the file; this is the test's own view of it
	f := a.store.Snapshot()
	if len(f.Accounts) != 1 || f.Accounts[0].ID != "gh:1" || f.Accounts[0].Status != gateway.StatusActive {
		t.Fatalf("accounts: %+v", f.Accounts)
	}
	raw, _ := json.Marshal(f)
	for _, leak := range []string{"someone", "example.com", "tok-"} {
		if strings.Contains(string(raw), leak) {
			t.Fatalf("accounts file holds %q", leak)
		}
	}
	if _, page := a.get(t, "/account", session); !strings.Contains(page, "Your account") {
		t.Fatalf("account page: %s", page)
	}
}

func TestBadStateAndForgedSessionRejected(t *testing.T) {
	a := newAcctRig(t, 100, nil)
	resp, _ := a.get(t, "/account/callback?code=c1&state=nope", "")
	if resp.StatusCode != http.StatusBadRequest || len(a.store.Snapshot().Accounts) != 0 {
		t.Fatalf("callback without matching state: %d", resp.StatusCode)
	}
	session, _ := a.signIn(t, 1)
	// Change a character in the middle of the signature (the last base64 character carries
	// unused bits, so changing it may not change the signature at all).
	i := len(session) - 10
	flip := map[bool]string{true: "A", false: "Q"}[session[i:i+1] != "A"]
	forged := session[:i] + flip + session[i+1:]
	for _, s := range []string{forged, "", "gh:1"} {
		if resp, _ := a.get(t, "/account/auth", s); resp.StatusCode == http.StatusOK {
			t.Fatalf("forward_auth accepted session %q", s)
		}
	}
}

func TestWaitlistAndCap(t *testing.T) {
	a := newAcctRig(t, 1, nil)
	s1, _ := a.signIn(t, 1)
	s2, _ := a.signIn(t, 2)
	s3, _ := a.signIn(t, 3)
	if _, page := a.get(t, "/account", s3); !strings.Contains(page, "waitlist") || !strings.Contains(page, "<strong>2</strong>") {
		t.Fatalf("third account should be second in line: %s", page)
	}
	if resp, _ := a.get(t, "/account/auth", s2); resp.StatusCode == http.StatusOK {
		t.Fatal("a waitlisted account reached the chat")
	}
	// The operator raises the limit (CLI edits the file); the oldest waiting account gets in.
	a.store.Update(func(f *gateway.AccountsFile) error { f.Settings.MaxActive = 2; return nil })
	waitFor(t, func() bool { r, _ := a.get(t, "/account/auth", s2); return r.StatusCode == http.StatusOK })
	if r, _ := a.get(t, "/account/auth", s3); r.StatusCode == http.StatusOK {
		t.Fatal("account 3 got in although the limit is 2")
	}
	// Closed sign-up: no new accounts, existing ones keep working.
	a.store.Update(func(f *gateway.AccountsFile) error { f.Settings.Closed = true; return nil })
	waitFor(t, func() bool { _, r := a.signIn(t, 4); return r.StatusCode == http.StatusForbidden })
	if r, _ := a.get(t, "/account/auth", s1); r.StatusCode != http.StatusOK {
		t.Fatal("closing sign-up locked out an existing account")
	}
}

func TestForwardAuthHeaders(t *testing.T) {
	a := newAcctRig(t, 100, nil)
	s1, _ := a.signIn(t, 1)
	s2, _ := a.signIn(t, 2)
	r1, _ := a.get(t, "/account/auth", s1)
	r2, _ := a.get(t, "/account/auth", s2)
	if r1.Header.Get("X-Auth-Email") != "gh-1@users.invalid" || r1.Header.Get("X-Auth-Role") != "admin" ||
		r2.Header.Get("X-Auth-Email") != "gh-2@users.invalid" || r2.Header.Get("X-Auth-Role") != "user" {
		t.Fatalf("headers: %v / %v", r1.Header, r2.Header)
	}
	req, _ := http.NewRequest("GET", a.gw.URL+"/account/auth", nil)
	req.Header.Set("Sec-Fetch-Mode", "navigate") // a page load is sent to sign in
	r, err := noRedirect.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	r.Body.Close()
	if r.StatusCode != http.StatusFound || !strings.Contains(r.Header.Get("Location"), "/account?next=chat") {
		t.Fatalf("no session: %d %s", r.StatusCode, r.Header.Get("Location"))
	}
}

func TestSelfServiceKeys(t *testing.T) {
	a := newAcctRig(t, 100, nil)
	s, _ := a.signIn(t, 7)
	var keys []string
	for i := 0; i < 3; i++ {
		_, page := a.createKey(t, s)
		k := keyRe.FindString(page)
		if k == "" {
			t.Fatalf("key %d not shown", i)
		}
		keys = append(keys, k)
	}
	if _, page := a.createKey(t, s); keyRe.MatchString(page) {
		t.Fatal("a fourth key was created")
	}
	a.store.Refresh()
	raw, _ := json.Marshal(a.store.Snapshot())
	if strings.Contains(string(raw), keys[0]) {
		t.Fatal("the accounts file holds a key in clear")
	}
	if _, page := a.get(t, "/account", s); keyRe.MatchString(page) {
		t.Fatal("a key was shown again")
	}
	ask := func(key string) int {
		resp := a.do(t, "POST", "/v1/chat/completions", key, chatBody)
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		return resp.StatusCode
	}
	if c := ask(keys[0]); c != 200 {
		t.Fatalf("account key: %d", c)
	}
	// Cross-site form posts are refused.
	req, _ := http.NewRequest("POST", a.gw.URL+"/account/keys/revoke", strings.NewReader("prefix="+keys[0][:11]))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Cookie", s)
	req.Header.Set("Origin", "https://evil.test")
	noRedirect.Do(req)
	if c := ask(keys[0]); c != 200 {
		t.Fatal("a cross-site post revoked a key")
	}
	a.post(t, "/account/keys/revoke", s, "prefix="+keys[0][:11])
	if c := ask(keys[0]); c != 401 {
		t.Fatalf("revoked key: %d", c)
	}
	// A banned account's keys stop working.
	a.store.SetStatus("gh:7", gateway.StatusBanned)
	waitFor(t, func() bool { return ask(keys[1]) == 401 }) // within the 2 s reread
}

// Chat (through Open WebUI) and API share one budget per account; only a trusted front end
// can name an account.
func TestOneBudgetPerAccount(t *testing.T) {
	a := newAcctRig(t, 100, func(c *gateway.AccountsConfig) { c.Limits.TokensPerDay = 40 })
	s, _ := a.signIn(t, 9)
	oneBudget(t, a, s, "gh-9@users.invalid")
}

// oneBudget checks that the account signed in as session (chat address email) is one budget
// across its API key and the chat.
func oneBudget(t *testing.T, a *acctRig, s, email string) {
	t.Helper()
	_, page := a.createKey(t, s)
	key := keyRe.FindString(page)
	chat := func(apiKey, email string) int {
		base := a.gw.URL
		if apiKey == "owui-key" {
			base = a.front.URL
		}
		req, _ := http.NewRequest("POST", base+"/v1/chat/completions", strings.NewReader(chatBody))
		req.Header.Set("Authorization", "Bearer "+apiKey)
		if email != "" {
			req.Header.Set("X-OpenWebUI-User-Email", email)
			req.Header.Set("X-OpenWebUI-User-Id", "owui-uuid-9")
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		return resp.StatusCode
	}
	// Use up the budget through the API key ...
	for i := 0; i < 20 && chat(key, "") == 200; i++ {
		waitUsage(t, a.rig, i+1)
	}
	if c := chat(key, ""); c != 429 {
		t.Fatalf("API key over budget: %d", c)
	}
	// ... and the same account is over budget in the chat too.
	if c := chat("owui-key", email); c != 429 {
		t.Fatalf("chat for the same account: %d", c)
	}
	// A plain API key cannot charge someone else's account by sending the header.
	if c := chat("user-key", email); c != 200 {
		t.Fatalf("header from an untrusted key changed the budget: %d", c)
	}
}

// waitFor polls cond for a few seconds (the gateway rereads the accounts file every 2 s).
func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	for i := 0; i < 40; i++ {
		if cond() {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatal("condition not reached")
}

// Our own form posts carry "Origin: null" (Referrer-Policy: no-referrer) but Sec-Fetch-Site:
// same-origin; cross-site posts are refused; re-posting a form creates no second key.
func TestKeyFormInRealBrowsers(t *testing.T) {
	a := newAcctRig(t, 100, nil)
	s, _ := a.signIn(t, 5)
	_, page := a.get(t, "/account", s)
	once := onceRe.FindStringSubmatch(page)[1]
	post := func(site, origin string) string {
		req, _ := http.NewRequest("POST", a.gw.URL+"/account/keys", strings.NewReader("once="+once))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Cookie", s)
		req.Header.Set("Sec-Fetch-Site", site)
		req.Header.Set("Origin", origin)
		resp, err := noRedirect.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return keyRe.FindString(string(b))
	}
	if post("cross-site", "https://site.test") != "" {
		t.Fatal("a cross-site post created a key")
	}
	if post("same-origin", "null") == "" {
		t.Fatal("our own form (Origin: null) did not create a key")
	}
	if post("same-origin", "null") != "" {
		t.Fatal("re-posting the same form created a second key")
	}
}

// Review fixes (2026-10-10): banned accounts are refused through the chat too; the operator
// can hold an account back on the waitlist; the usage log names no GitHub ids.
func TestBanHoldAndPseudonyms(t *testing.T) {
	a := newAcctRig(t, 100, nil)
	a.signIn(t, 11)
	a.signIn(t, 12)
	chat := func(email string) int {
		req, _ := http.NewRequest("POST", a.front.URL+"/v1/chat/completions", strings.NewReader(chatBody))
		req.Header.Set("Authorization", "Bearer owui-key")
		req.Header.Set("X-OpenWebUI-User-Email", email)
		req.Header.Set("X-OpenWebUI-User-Id", "some-uuid")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		return resp.StatusCode
	}
	if c := chat("gh-11@users.invalid"); c != 200 {
		t.Fatalf("active account through the chat: %d", c)
	}
	waitUsage(t, a.rig, 1)
	if raw, _ := os.ReadFile(a.usageLog); strings.Contains(string(raw), "gh:11") || !strings.Contains(string(raw), "acct-") {
		t.Fatalf("usage log should name a pseudonym, not the GitHub id: %s", raw)
	}
	a.store.SetStatus("gh:11", gateway.StatusBanned)
	waitFor(t, func() bool { return chat("gh-11@users.invalid") == 403 })
	// "waitlist" holds an account back even though there is room.
	a.store.SetStatus("gh:12", gateway.StatusWaitlisted)
	a.store.Refresh()
	if acct, _, _ := a.store.Get("gh:12"); acct.Status != gateway.StatusWaitlisted {
		t.Fatalf("waitlist did not hold the account: %s", acct.Status)
	}
}

// Review 2026-10-10: account keys work on every endpoint, and one form creates at most one key,
// even when posted concurrently or again after a revoke.
func TestAccountKeyEndpointsAndFormRaces(t *testing.T) {
	a := newAcctRig(t, 100, nil)
	s, _ := a.signIn(t, 21)
	_, page := a.get(t, "/account", s)
	once := onceRe.FindStringSubmatch(page)[1]
	post := func() string {
		req, _ := http.NewRequest("POST", a.gw.URL+"/account/keys", strings.NewReader("once="+once))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Cookie", s)
		req.Header.Set("Sec-Fetch-Site", "same-origin")
		resp, err := noRedirect.Do(req)
		if err != nil {
			t.Error(err)
			return ""
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return keyRe.FindString(string(b))
	}
	keys := make(chan string, 10)
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); keys <- post() }()
	}
	wg.Wait()
	close(keys)
	var got []string
	for k := range keys {
		if k != "" {
			got = append(got, k)
		}
	}
	if len(got) != 1 {
		t.Fatalf("10 concurrent posts of one form created %d keys", len(got))
	}
	// The key works on /v1/models too.
	resp := a.do(t, "GET", "/v1/models", got[0], "")
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("/v1/models with an account key: %d", resp.StatusCode)
	}
	// Revoking the key does not make the old form valid again.
	a.post(t, "/account/keys/revoke", s, "prefix="+got[0][:11])
	if post() != "" {
		t.Fatal("an old form created a key after a revoke")
	}
}

// A signature that differs only in the unused bits of its last base64 character is the same
// signature in another spelling; strict decoding refuses it, so each session has one spelling.
func TestSessionNonCanonicalSpellingRejected(t *testing.T) {
	a := newAcctRig(t, 100, nil)
	s, _ := a.signIn(t, 31)
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"
	last := strings.IndexByte(alphabet, s[len(s)-1])
	variant := s[:len(s)-1] + string(alphabet[last^1]) // flips only an unused bit
	if r, _ := a.get(t, "/account/auth", variant); r.StatusCode == http.StatusOK {
		t.Fatal("a non-canonical spelling of the session was accepted")
	}
	if r, _ := a.get(t, "/account/auth", s); r.StatusCode != http.StatusOK {
		t.Fatal("the real session was refused")
	}
}

// Without a session, page loads go to sign-in but the chat app's own calls get 401: a redirect
// there made Open WebUI reload itself in a loop (staging, 2026-10-10).
func TestForwardAuthNavigationVersusFetch(t *testing.T) {
	a := newAcctRig(t, 100, nil)
	ask := func(mode, accept string) *http.Response {
		req, _ := http.NewRequest("GET", a.gw.URL+"/account/auth", nil)
		if mode != "" {
			req.Header.Set("Sec-Fetch-Mode", mode)
		}
		if accept != "" {
			req.Header.Set("Accept", accept)
		}
		resp, err := noRedirect.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp
	}
	if r := ask("navigate", "text/html"); r.StatusCode != http.StatusFound {
		t.Fatalf("page load: %d, want a redirect to sign-in", r.StatusCode)
	}
	if r := ask("", "text/html,application/xhtml+xml"); r.StatusCode != http.StatusFound {
		t.Fatalf("page load without Sec-Fetch headers: %d", r.StatusCode)
	}
	for _, mode := range []string{"cors", "same-origin", "websocket"} {
		if r := ask(mode, "application/json"); r.StatusCode != http.StatusUnauthorized {
			t.Fatalf("fetch (%s): %d, want 401", mode, r.StatusCode)
		}
	}
	if r := ask("", "application/json"); r.StatusCode != http.StatusUnauthorized {
		t.Fatalf("fetch without Sec-Fetch headers: %d, want 401", r.StatusCode)
	}
}

// A place on the waitlist matches the order accounts are let in: accounts the operator
// holds back are not counted ahead of others, and see that they are on hold.
func TestWaitlistPlaceSkipsHeldAccounts(t *testing.T) {
	a := newAcctRig(t, 1, nil)
	a.signIn(t, 1) // active
	s2, _ := a.signIn(t, 2)
	s3, _ := a.signIn(t, 3)
	a.store.Update(func(f *gateway.AccountsFile) error { f.Find("gh:2").Held = true; return nil })
	// gh:3 is next to be let in, so place 1 (once the gateway has reread the file).
	waitFor(t, func() bool { _, p := a.get(t, "/account", s3); return strings.Contains(p, "Place <strong>1</strong>") })
	if _, page := a.get(t, "/account", s2); !strings.Contains(page, "on hold") || strings.Contains(page, "Place <strong>") {
		t.Fatalf("a held account should see that it is on hold: %s", page)
	}
	a.store.Update(func(f *gateway.AccountsFile) error { f.Settings.MaxActive = 2; return nil })
	waitFor(t, func() bool { r, _ := a.get(t, "/account/auth", s3); return r.StatusCode == http.StatusOK })
	if r, _ := a.get(t, "/account/auth", s2); r.StatusCode == http.StatusOK {
		t.Fatal("a held account was let in")
	}
}
