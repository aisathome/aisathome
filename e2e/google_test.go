// Google sign-in (OpenID Connect, scope "openid" only): the ID token's signature and claims
// are checked, the flow uses state, a nonce and PKCE, and only "google:<sub>" is stored.
//
// SPDX-License-Identifier: Apache-2.0
package e2e

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aisathome/aisathome/gateway"
)

// fakeGoogle plays Google's authorization, token and key endpoints. /authorize stands in for
// the user's consent: it binds a code to the sub in "as", the nonce and the PKCE challenge.
// /token answers with an RS256-signed ID token that carries an email, a name and a picture
// too, so tests can show the gateway drops them.
type fakeGoogle struct {
	*httptest.Server
	key *rsa.PrivateKey

	mu     sync.Mutex
	grants map[string]url.Values                            // code → the authorization request
	mutate func(hdr, claims map[string]any) *rsa.PrivateKey // per case; may return another signing key
}

const (
	googleClientID = "google-id.apps.example"
	googleSecret   = "google-secret"
)

var testRSAKey, otherRSAKey = mustRSA(), mustRSA()

func mustRSA() *rsa.PrivateKey {
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		panic(err)
	}
	return k
}

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

func signJWT(k *rsa.PrivateKey, hdr, claims map[string]any) string {
	h, _ := json.Marshal(hdr)
	c, _ := json.Marshal(claims)
	in := b64(h) + "." + b64(c)
	sum := sha256.Sum256([]byte(in))
	sig, err := rsa.SignPKCS1v15(rand.Reader, k, crypto.SHA256, sum[:])
	if err != nil {
		panic(err)
	}
	return in + "." + b64(sig)
}

func newFakeGoogle(t *testing.T) *fakeGoogle {
	fg := &fakeGoogle{key: testRSAKey, grants: map[string]url.Values{}}
	fg.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/authorize":
			q := r.URL.Query()
			if q.Get("response_type") != "code" || q.Get("scope") != "openid" || q.Get("client_id") != googleClientID ||
				q.Get("code_challenge_method") != "S256" || q.Get("code_challenge") == "" {
				http.Error(w, "bad authorization request", http.StatusBadRequest)
				return
			}
			code := "code-" + randomString()
			fg.mu.Lock()
			fg.grants[code] = q
			fg.mu.Unlock()
			back := url.Values{"code": {code}, "state": {q.Get("state")}}
			http.Redirect(w, r, q.Get("redirect_uri")+"?"+back.Encode(), http.StatusFound)
		case "/token":
			r.ParseForm()
			fg.mu.Lock()
			grant, ok := fg.grants[r.Form.Get("code")]
			delete(fg.grants, r.Form.Get("code")) // codes are single-use
			mutate := fg.mutate
			fg.mu.Unlock()
			sum := sha256.Sum256([]byte(r.Form.Get("code_verifier")))
			if !ok || r.Form.Get("client_id") != googleClientID || r.Form.Get("client_secret") != googleSecret ||
				r.Form.Get("grant_type") != "authorization_code" || r.Form.Get("redirect_uri") != grant.Get("redirect_uri") ||
				r.Form.Get("code_verifier") == "" || b64(sum[:]) != grant.Get("code_challenge") {
				w.WriteHeader(http.StatusBadRequest)
				json.NewEncoder(w).Encode(map[string]string{"error": "invalid_grant"})
				return
			}
			now := time.Now()
			hdr := map[string]any{"alg": "RS256", "kid": "g1", "typ": "JWT"}
			claims := map[string]any{"iss": "https://accounts.google.com", "aud": googleClientID, "azp": googleClientID,
				"sub": grant.Get("as"), "iat": now.Unix(), "exp": now.Add(time.Hour).Unix(), "nonce": grant.Get("nonce"),
				"email": "someone@gmail.example", "name": "Some One", "picture": "https://pics.example/someone"}
			key := fg.key
			if mutate != nil {
				if k := mutate(hdr, claims); k != nil {
					key = k
				}
			}
			json.NewEncoder(w).Encode(map[string]any{"access_token": "ya29.access-secret", "expires_in": 3599,
				"scope": "openid", "token_type": "Bearer", "id_token": signJWT(key, hdr, claims)})
		case "/certs":
			pub := fg.key.PublicKey
			json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]string{{"kty": "RSA", "alg": "RS256",
				"use": "sig", "kid": "g1", "n": b64(pub.N.Bytes()), "e": b64(big.NewInt(int64(pub.E)).Bytes())}}})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(fg.Close)
	return fg
}

func randomString() string {
	b := make([]byte, 12)
	rand.Read(b)
	return b64(b)
}

func (fg *fakeGoogle) setMutate(m func(hdr, claims map[string]any) *rsa.PrivateKey) {
	fg.mu.Lock()
	fg.mutate = m
	fg.mu.Unlock()
}

// newGoogleRig is an accounts rig with Google sign-in (and GitHub unless mod clears it).
func newGoogleRig(t *testing.T, maxActive int, mod func(*gateway.AccountsConfig)) (*acctRig, *fakeGoogle) {
	fg := newFakeGoogle(t)
	a := newAcctRig(t, maxActive, func(c *gateway.AccountsConfig) {
		c.GoogleClientID, c.GoogleClientSecret = googleClientID, googleSecret
		c.GoogleAuthURL, c.GoogleTokenURL, c.GoogleCertsURL = fg.URL+"/authorize", fg.URL+"/token", fg.URL+"/certs"
		if mod != nil {
			mod(c)
		}
	})
	return a, fg
}

// signInGoogle runs the Google flow for sub and returns the session cookie value. cookie, if
// set, changes the flow cookie before the callback.
func (a *acctRig) signInGoogle(t *testing.T, sub string, cookie func(string) string) (string, *http.Response) {
	t.Helper()
	resp, err := noRedirect.Get(a.gw.URL + "/account/login?provider=google")
	if err != nil {
		t.Fatal(err)
	}
	oauth := cookieValue(resp, "aisathome_oauth")
	if cookie != nil {
		oauth = cookie(oauth)
	}
	resp, err = noRedirect.Get(resp.Header.Get("Location") + "&as=" + url.QueryEscape(sub)) // the user consents
	if err != nil || resp.StatusCode != http.StatusFound {
		t.Fatalf("fake authorize: %v %v", err, resp)
	}
	back, _ := url.Parse(resp.Header.Get("Location"))
	if back.Path != "/account/callback/google" {
		t.Fatalf("redirect_uri: %s", back)
	}
	req, _ := http.NewRequest("GET", a.gw.URL+back.Path+"?"+back.RawQuery, nil)
	req.Header.Set("Cookie", oauth)
	if resp, err = noRedirect.Do(req); err != nil {
		t.Fatal(err)
	}
	return cookieValue(resp, "aisathome_session"), resp
}

const googleSub = "104578901234567890123"

func TestGoogleSignInStoresOnlyTheSub(t *testing.T) {
	logs := &syncBuf{}
	logSink = logs
	defer func() { logSink = io.Discard }()
	a, _ := newGoogleRig(t, 100, nil)

	// The request to Google asks for "openid" and nothing else, with a nonce and PKCE.
	resp, _ := a.get(t, "/account/login?provider=google", "")
	loc, _ := url.Parse(resp.Header.Get("Location"))
	q := loc.Query()
	if q.Get("scope") != "openid" || q.Get("code_challenge_method") != "S256" || len(q.Get("code_challenge")) != 43 ||
		q.Get("nonce") == "" || q.Get("state") == "" || q.Get("redirect_uri") != "https://site.test/account/callback/google" ||
		strings.Contains(loc.RawQuery, "email") || strings.Contains(loc.RawQuery, "profile") {
		t.Fatalf("authorization request: %s", loc.RawQuery)
	}

	session, resp := a.signInGoogle(t, googleSub, nil)
	if session == "" || resp.StatusCode != http.StatusFound {
		t.Fatalf("sign-in: %d, session %q", resp.StatusCode, session)
	}
	a.store.Refresh()
	f := a.store.Snapshot()
	if len(f.Accounts) != 1 || f.Accounts[0].ID != "google:"+googleSub || f.Accounts[0].Status != gateway.StatusActive {
		t.Fatalf("accounts: %+v", f.Accounts)
	}
	raw, _ := json.Marshal(f)
	for _, leak := range []string{"someone", "Some One", "gmail", "pics.example", "ya29", "eyJ"} {
		if strings.Contains(string(raw), leak) || strings.Contains(logs.String(), leak) {
			t.Fatalf("accounts file or log holds %q", leak)
		}
	}
	r, _ := a.get(t, "/account/auth", session)
	if r.StatusCode != http.StatusOK || r.Header.Get("X-Auth-Email") != "google-"+googleSub+"@users.invalid" ||
		r.Header.Get("X-Auth-Role") != "user" {
		t.Fatalf("forward_auth: %d %v", r.StatusCode, r.Header)
	}
	// GitHub still works next to Google, and its account is a different one.
	if _, page := a.get(t, "/account", ""); !strings.Contains(page, "Sign in with GitHub") || !strings.Contains(page, "Sign in with Google") {
		t.Fatalf("sign-in page: %s", page)
	}
	if s, _ := a.signIn(t, 1); s == "" {
		t.Fatal("GitHub sign-in next to Google failed")
	}
	a.store.Refresh()
	if n := len(a.store.Snapshot().Accounts); n != 2 {
		t.Fatalf("%d accounts, want 2", n)
	}
}

// Every check on the ID token and the flow refuses sign-in on its own.
func TestGoogleIDTokenChecks(t *testing.T) {
	logs := &syncBuf{}
	logSink = logs
	defer func() { logSink = io.Discard }()
	a, fg := newGoogleRig(t, 100, nil)
	claim := func(k string, v any) func(_, c map[string]any) *rsa.PrivateKey {
		return func(_, c map[string]any) *rsa.PrivateKey { c[k] = v; return nil }
	}
	now := time.Now()
	field := func(i int, v string) func(string) string { // replace one field of the flow cookie
		return func(c string) string { p := strings.Split(c, "|"); p[i] = v; return strings.Join(p, "|") }
	}
	cases := []struct {
		name   string
		mutate func(hdr, claims map[string]any) *rsa.PrivateKey
		cookie func(string) string
		want   int // 400: the gateway refused the flow; 502: it refused Google's answer
	}{
		{"wrong aud", claim("aud", "someone-else.apps.example"), nil, 502},
		{"aud list with another client", claim("aud", []string{googleClientID, "other"}), nil, 502},
		{"wrong iss", claim("iss", "https://accounts.evil.example"), nil, 502},
		{"expired", func(_, c map[string]any) *rsa.PrivateKey {
			c["exp"], c["iat"] = now.Add(-5*time.Minute).Unix(), now.Add(-65*time.Minute).Unix()
			return nil
		}, nil, 502},
		{"issued in the future", claim("iat", now.Add(10*time.Minute).Unix()), nil, 502},
		{"no exp", func(_, c map[string]any) *rsa.PrivateKey { delete(c, "exp"); return nil }, nil, 502},
		{"bad signature", func(_, _ map[string]any) *rsa.PrivateKey { return otherRSAKey }, nil, 502},
		{"alg none", func(h, _ map[string]any) *rsa.PrivateKey { h["alg"] = "none"; return nil }, nil, 502},
		{"alg HS256", func(h, _ map[string]any) *rsa.PrivateKey { h["alg"] = "HS256"; return nil }, nil, 502},
		{"unknown kid", func(h, _ map[string]any) *rsa.PrivateKey { h["kid"] = "g9"; return nil }, nil, 502},
		{"wrong nonce", claim("nonce", "not-the-nonce"), nil, 502},
		{"no nonce", func(_, c map[string]any) *rsa.PrivateKey { delete(c, "nonce"); return nil }, nil, 502},
		{"sub with letters", func(_, c map[string]any) *rsa.PrivateKey { c["sub"] = "1045abc"; return nil }, nil, 502},
		{"missing PKCE verifier", nil, field(4, ""), 400},
		{"wrong PKCE verifier", nil, field(4, strings.Repeat("v", 43)), 502},
		{"missing nonce in the flow", nil, field(3, ""), 400},
		{"flow started for GitHub", nil, field(0, "github"), 400},
		{"other state", nil, field(1, "forged-state"), 400},
	}
	for _, c := range cases {
		fg.setMutate(c.mutate)
		session, resp := a.signInGoogle(t, "2", c.cookie)
		if session != "" || resp.StatusCode != c.want {
			t.Errorf("%s: %d, want %d", c.name, resp.StatusCode, c.want)
		}
	}
	a.store.Refresh()
	if n := len(a.store.Snapshot().Accounts); n != 0 {
		t.Fatalf("rejected sign-ins created %d accounts", n)
	}
	// The same rig signs in fine once the token is right, also with the short issuer form.
	fg.setMutate(claim("iss", "accounts.google.com"))
	if s, resp := a.signInGoogle(t, "2", nil); s == "" {
		t.Fatalf("valid token refused: %d", resp.StatusCode)
	}
	for _, leak := range []string{"someone", "ya29", "eyJ"} {
		if strings.Contains(logs.String(), leak) {
			t.Fatalf("log holds %q", leak)
		}
	}
}

// With only Google configured, sign-in defaults to it and GitHub is not on offer.
func TestGoogleOnly(t *testing.T) {
	a, fg := newGoogleRig(t, 100, func(c *gateway.AccountsConfig) { c.GitHubClientID, c.GitHubClientSecret = "", "" })
	if _, page := a.get(t, "/account?next=chat", ""); strings.Contains(page, "GitHub") ||
		!strings.Contains(page, `href="/account/login?provider=google&amp;next=chat"`) {
		t.Fatalf("sign-in page: %s", page)
	}
	if r, _ := a.get(t, "/account/login", ""); !strings.HasPrefix(r.Header.Get("Location"), fg.URL+"/authorize?") {
		t.Fatalf("default provider: %s", r.Header.Get("Location"))
	}
	for _, p := range []string{"/account/login?provider=github", "/account/login?provider=evil", "/account/callback?code=c1&state=x"} {
		if r, _ := a.get(t, p, ""); r.StatusCode != http.StatusNotFound {
			t.Fatalf("%s: %d", p, r.StatusCode)
		}
	}
}

// Chat and API share one budget for a Google account too (google-<sub>@users.invalid).
func TestOneBudgetPerGoogleAccount(t *testing.T) {
	a, _ := newGoogleRig(t, 100, func(c *gateway.AccountsConfig) { c.Limits.TokensPerDay = 40 })
	s, _ := a.signInGoogle(t, googleSub, nil)
	oneBudget(t, a, s, "google-"+googleSub+"@users.invalid")
}
