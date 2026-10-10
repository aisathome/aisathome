// HTTP side of accounts: GitHub and Google sign-in, the account page, and the check Caddy's
// forward_auth calls before the chat (Open WebUI) sees a request.
//
// SPDX-License-Identifier: Apache-2.0
package gateway

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"
)

const (
	oauthCookie = "aisathome_oauth"
	// authEmailHeader carries the signed-in account to Open WebUI (its
	// WEBUI_AUTH_TRUSTED_EMAIL_HEADER). Caddy deletes any copy a client sends.
	authEmailHeader = "X-Auth-Email"
	authRoleHeader  = "X-Auth-Role" // WEBUI_AUTH_TRUSTED_ROLE_HEADER: admin or user
	// accountEmailSuffix: the chat knows accounts only by this made-up address; .invalid
	// can never be a real mailbox.
	accountEmailSuffix = "@users.invalid"
)

func (g *Gateway) accountRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /account", g.handleAccount)
	mux.HandleFunc("GET /account/login", g.handleLogin)
	if g.acfg.githubOn() {
		mux.HandleFunc("GET /account/callback", g.handleCallback)
	}
	if g.acfg.googleOn() {
		mux.HandleFunc("GET /account/callback/google", g.handleGoogleCallback)
	}
	mux.HandleFunc("POST /account/keys", g.handleKeyCreate)
	mux.HandleFunc("POST /account/keys/revoke", g.handleKeyRevoke)
	mux.HandleFunc("POST /account/logout", g.handleLogout)
	mux.HandleFunc("GET /account/auth", g.handleForwardAuth)
}

// accountEmail is the chat's address for account id ("gh:123" → "gh-123@users.invalid",
// "google:1045" → "google-1045@users.invalid").
func accountEmail(id string) string { return strings.ReplaceAll(id, ":", "-") + accountEmailSuffix }

// accountFromEmail reverses accountEmail; ok only for well-formed GitHub and Google accounts
// (digits only, so no address can name anything else).
func accountFromEmail(email string) (string, bool) {
	local, ok := strings.CutSuffix(strings.ToLower(email), accountEmailSuffix)
	if !ok {
		return "", false
	}
	if digits, gh := strings.CutPrefix(local, "gh-"); gh && digits != "" && strings.Trim(digits, "0123456789") == "" {
		return "gh:" + digits, true
	}
	if sub, google := strings.CutPrefix(local, "google-"); google && validGoogleSub(sub) {
		return "google:" + sub, true
	}
	return "", false
}

// session returns the signed-in account id, if any.
func (g *Gateway) session(r *http.Request) (string, bool) {
	c, err := r.Cookie(g.cookieName(sessionCookie))
	if err != nil {
		return "", false
	}
	return verifySession(g.acfg.SessionSecret, c.Value, time.Now())
}

// cookieName makes cookie names differ per environment: prod's cookies (Domain=aisathome.org)
// also reach staging.aisathome.org, and with one name the browser's first cookie wins.
func (g *Gateway) cookieName(base string) string {
	h := sha256.Sum256([]byte(g.acfg.CookieDomain + "|" + g.acfg.PublicURL))
	return base + "_" + hex.EncodeToString(h[:3])
}

func (g *Gateway) setCookie(w http.ResponseWriter, name, value, path string, maxAge time.Duration) {
	http.SetCookie(w, &http.Cookie{Name: g.cookieName(name), Value: value, Path: path, Domain: g.acfg.CookieDomain,
		MaxAge: int(maxAge.Seconds()), Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode})
}

// sameOrigin rejects cross-site form posts (on top of SameSite=Lax cookies).
// Browsers send Sec-Fetch-Site on every request; it says "same-origin" for our own forms even
// though Referrer-Policy: no-referrer makes them send "Origin: null". Without it (older
// browsers, scripts) the Origin header decides.
func (g *Gateway) sameOrigin(r *http.Request) bool {
	if s := r.Header.Get("Sec-Fetch-Site"); s != "" {
		return s == "same-origin"
	}
	o := r.Header.Get("Origin")
	return o == "" || o == g.acfg.PublicURL
}

// randomToken returns 32 random bytes in base64url: 43 characters, also a valid PKCE
// verifier.
func randomToken() string {
	b := make([]byte, 32)
	rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

// oauthFlow is what the browser keeps between the redirect to a provider and the callback,
// in a short-lived cookie: the provider, the state, where to go next and, for Google, the
// OpenID Connect nonce and the PKCE verifier.
type oauthFlow struct{ provider, state, next, nonce, verifier string }

func (f oauthFlow) String() string {
	return strings.Join([]string{f.provider, f.state, f.next, f.nonce, f.verifier}, "|")
}

// takeFlow reads and clears the flow cookie; ok only if it was started for provider and the
// callback carries its state.
func (g *Gateway) takeFlow(w http.ResponseWriter, r *http.Request, provider string) (oauthFlow, bool) {
	var f oauthFlow
	if c, err := r.Cookie(g.cookieName(oauthCookie)); err == nil {
		if p := strings.Split(c.Value, "|"); len(p) == 5 {
			f = oauthFlow{p[0], p[1], p[2], p[3], p[4]}
		}
	}
	g.setCookie(w, oauthCookie, "", "/account", -time.Second)
	got := r.URL.Query().Get("state")
	return f, f.provider == provider && f.state != "" && subtle.ConstantTimeCompare([]byte(got), []byte(f.state)) == 1
}

// handleLogin starts sign-in with ?provider=github (the default, if enabled) or google.
// GitHub gets no scopes: /user without a scope gives the public profile, of which only the
// numeric id is kept. Google gets scope "openid" only (google.go).
func (g *Gateway) handleLogin(w http.ResponseWriter, r *http.Request) {
	provider := r.URL.Query().Get("provider")
	if provider == "" {
		provider = "github"
		if !g.acfg.githubOn() {
			provider = "google"
		}
	}
	f := oauthFlow{provider: provider, state: randomToken()}
	if r.URL.Query().Get("next") == "chat" {
		f.next = "chat"
	}
	var target string
	switch {
	case provider == "github" && g.acfg.githubOn():
		q := url.Values{"client_id": {g.acfg.GitHubClientID}, "state": {f.state},
			"redirect_uri": {g.acfg.PublicURL + "/account/callback"}, "allow_signup": {"true"}}
		target = g.acfg.GitHubAuthURL + "?" + q.Encode()
	case provider == "google" && g.acfg.googleOn():
		f.nonce, f.verifier = randomToken(), randomToken()
		target = g.googleAuthURL(f)
	default:
		g.accountPage(w, http.StatusNotFound, pageData{Error: "There is no such way to sign in."})
		return
	}
	g.setCookie(w, oauthCookie, f.String(), "/account", 10*time.Minute)
	http.Redirect(w, r, target, http.StatusFound)
}

// handleCallback finishes GitHub sign-in: code → token → numeric id. The token is used once
// and dropped.
func (g *Gateway) handleCallback(w http.ResponseWriter, r *http.Request) {
	f, ok := g.takeFlow(w, r, "github")
	if !ok {
		g.accountPage(w, http.StatusBadRequest, pageData{Error: "The sign-in expired or came from elsewhere. Please try again."})
		return
	}
	id, err := g.githubID(r, r.URL.Query().Get("code"))
	if err != nil {
		g.log.Warn("github sign-in failed", "err", err)
		g.accountPage(w, http.StatusBadGateway, pageData{Error: "GitHub sign-in failed. Please try again."})
		return
	}
	g.finishSignIn(w, r, id, f.next)
}

// finishSignIn creates or finds the account for a verified provider id and starts a session.
func (g *Gateway) finishSignIn(w http.ResponseWriter, r *http.Request, id, next string) {
	a, err := g.accounts.SignIn(id, time.Now())
	if errors.Is(err, errSignupClosed) {
		g.accountPage(w, http.StatusForbidden, pageData{Error: "Sign-up is closed for now. Please try again later."})
		return
	} else if err != nil {
		g.log.Error("account sign-in", "err", err)
		g.accountPage(w, http.StatusInternalServerError, pageData{Error: "Something went wrong. Please try again."})
		return
	}
	g.setCookie(w, sessionCookie, signSession(g.acfg.SessionSecret, id, time.Now().Add(sessionTTL)), "/", sessionTTL)
	if next == "chat" && a.Status == StatusActive && g.acfg.ChatURL != "" {
		http.Redirect(w, r, g.acfg.ChatURL, http.StatusFound)
		return
	}
	http.Redirect(w, r, g.acfg.PublicURL+"/account", http.StatusFound)
}

// githubID exchanges an OAuth code for the user's numeric GitHub id ("gh:<id>").
func (g *Gateway) githubID(r *http.Request, code string) (string, error) {
	if code == "" {
		return "", errors.New("no code")
	}
	form := url.Values{"client_id": {g.acfg.GitHubClientID}, "client_secret": {g.acfg.GitHubClientSecret},
		"code": {code}, "redirect_uri": {g.acfg.PublicURL + "/account/callback"}}
	req, _ := http.NewRequestWithContext(r.Context(), "POST", g.acfg.GitHubTokenURL, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	var tok struct {
		AccessToken string `json:"access_token"`
		Error       string `json:"error"`
	}
	if err := g.getJSON(req, &tok); err != nil {
		return "", err
	}
	if tok.AccessToken == "" {
		return "", fmt.Errorf("token exchange: %s", tok.Error)
	}
	req, _ = http.NewRequestWithContext(r.Context(), "GET", g.acfg.GitHubUserURL, nil)
	req.Header.Set("Authorization", "Bearer "+tok.AccessToken)
	req.Header.Set("Accept", "application/vnd.github+json")
	var user struct {
		ID int64 `json:"id"`
	}
	if err := g.getJSON(req, &user); err != nil {
		return "", err
	}
	if user.ID <= 0 {
		return "", errors.New("no user id")
	}
	return fmt.Sprintf("gh:%d", user.ID), nil
}

func (g *Gateway) getJSON(req *http.Request, v any) error {
	req.Header.Set("User-Agent", "aisathome-gateway")
	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s: HTTP %d", req.URL.Host, resp.StatusCode)
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(v)
}

// handleForwardAuth answers Caddy's forward_auth for the chat: 200 with the account's chat
// address for active accounts, otherwise a redirect to sign-in.
func (g *Gateway) handleForwardAuth(w http.ResponseWriter, r *http.Request) {
	id, ok := g.session(r)
	if !ok || !g.accounts.active(id) {
		http.Redirect(w, r, g.acfg.PublicURL+"/account?next=chat", http.StatusFound)
		return
	}
	role := "user"
	if slices.Contains(g.acfg.Admins, id) {
		role = "admin"
	}
	w.Header().Set(authEmailHeader, accountEmail(id))
	w.Header().Set(authRoleHeader, role)
	w.WriteHeader(http.StatusOK)
}

func (g *Gateway) handleAccount(w http.ResponseWriter, r *http.Request) {
	id, ok := g.session(r)
	if !ok {
		g.accountPage(w, http.StatusOK, pageData{Next: r.URL.Query().Get("next") == "chat"})
		return
	}
	g.accountPage(w, http.StatusOK, g.accountData(id))
}

func (g *Gateway) accountData(id string) pageData {
	a, pos, ok := g.accounts.Get(id)
	if !ok {
		return pageData{}
	}
	used := g.budget.used1("acct:"+id, time.Now())
	return pageData{SignedIn: true, Account: a, Position: pos, ChatURL: g.acfg.ChatURL, API: g.acfg.PublicURL,
		Used: used, Budget: g.acfg.Limits.TokensPerDay, MaxKeys: g.acfg.MaxKeys, Once: g.createToken(id, a.KeySeq)}
}

func (g *Gateway) handleKeyCreate(w http.ResponseWriter, r *http.Request) {
	id, ok := g.session(r)
	if !ok || !g.sameOrigin(r) {
		http.Redirect(w, r, g.acfg.PublicURL+"/account", http.StatusSeeOther)
		return
	}
	d := g.accountData(id)
	once := r.FormValue("once")
	key, err := g.accounts.AddKey(id, g.acfg.MaxKeys, time.Now(), func(seq int) bool {
		return hmac.Equal([]byte(once), []byte(g.createToken(id, seq)))
	})
	if errors.Is(err, errStaleForm) { // a reloaded, replayed or concurrent post of one form
		http.Redirect(w, r, g.acfg.PublicURL+"/account", http.StatusSeeOther)
		return
	}
	if err != nil {
		d.Error = err.Error()
		g.accountPage(w, http.StatusBadRequest, d)
		return
	}
	d = g.accountData(id)
	d.NewKey = key
	g.accountPage(w, http.StatusOK, d) // shown once: rendered here, never stored
}

func (g *Gateway) handleKeyRevoke(w http.ResponseWriter, r *http.Request) {
	id, ok := g.session(r)
	if ok && g.sameOrigin(r) {
		if err := g.accounts.RevokeKey(id, r.FormValue("prefix")); err != nil {
			d := g.accountData(id)
			d.Error = err.Error()
			g.accountPage(w, http.StatusBadRequest, d)
			return
		}
	}
	http.Redirect(w, r, g.acfg.PublicURL+"/account", http.StatusSeeOther)
}

func (g *Gateway) handleLogout(w http.ResponseWriter, r *http.Request) {
	if g.sameOrigin(r) {
		g.setCookie(w, sessionCookie, "", "/", -time.Second)
	}
	http.Redirect(w, r, g.acfg.PublicURL+"/", http.StatusSeeOther)
}

// ---------------------------------------------------------------- page

type pageData struct {
	GitHub   bool // sign-in providers on offer
	Google   bool
	SignedIn bool
	Next     bool
	Account  Account
	Position int
	NewKey   string
	Error    string
	ChatURL  string
	API      string
	Used     int64
	Budget   int
	MaxKeys  int
	Once     string // one-time token for the "create a key" form
}

// createToken ties a "create a key" form to the account's KeySeq (see AccountStore.AddKey).
func (g *Gateway) createToken(id string, seq int) string {
	m := hmac.New(sha256.New, []byte(g.acfg.SessionSecret))
	fmt.Fprintf(m, "create|%s|%d", id, seq)
	return hex.EncodeToString(m.Sum(nil)[:12])
}

func (g *Gateway) accountPage(w http.ResponseWriter, status int, d pageData) {
	d.GitHub, d.Google = g.acfg.githubOn(), g.acfg.googleOn()
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store") // a new key must not end up in a cache
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; form-action 'self' https://github.com; frame-ancestors 'none'")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.WriteHeader(status)
	if err := accountTmpl.Execute(w, d); err != nil {
		g.log.Error("account page", "err", err)
	}
}

var accountTmpl = template.Must(template.New("account").Funcs(template.FuncMap{
	"day": func(t time.Time) string { return t.Format("2006-01-02") },
}).Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<meta name="robots" content="noindex"><title>Your account · AIs@home</title>
<style>
:root{color-scheme:dark light;--bg:#111417;--fg:#e6edf0;--mut:#9aa7ad;--acc:#14b8a6;--card:#1a1f23;--line:#2a3237}
@media (prefers-color-scheme:light){:root{--bg:#f7f9f9;--fg:#132024;--mut:#4d5b60;--acc:#0f766e;--card:#fff;--line:#d6dee0}}
body{margin:0;background:var(--bg);color:var(--fg);font:16px/1.6 system-ui,sans-serif}
main{max-width:40rem;margin:0 auto;padding:2.5rem 1rem}
a{color:var(--acc)}h1{font-size:1.6rem;margin:0 0 1rem}
.card{background:var(--card);border:1px solid var(--line);border-radius:.6rem;padding:1.1rem 1.2rem;margin:1rem 0}
.btn{display:inline-block;background:var(--acc);color:#fff;border:0;border-radius:2rem;padding:.55rem 1.2rem;font:inherit;cursor:pointer;text-decoration:none}
.btn.ghost{background:transparent;color:var(--fg);border:1px solid var(--line)}
code,.key{font-family:ui-monospace,Menlo,monospace}.key{display:block;word-break:break-all;background:var(--bg);padding:.6rem;border-radius:.4rem;border:1px solid var(--acc)}
.mut{color:var(--mut)}.err{border-color:#e5484d}table{width:100%;border-collapse:collapse}td{padding:.35rem 0;border-bottom:1px solid var(--line)}
form{display:inline}
</style></head><body><main>
<p><a href="/">← AIs@home</a></p>
{{if .Error}}<div class="card err">{{.Error}}</div>{{end}}
{{if not .SignedIn}}
<h1>Sign in</h1>
<div class="card"><p>AIs@home uses your {{if .GitHub}}GitHub{{end}}{{if and .GitHub .Google}} or {{end}}{{if .Google}}Google{{end}}
account only to tell accounts apart. We keep only its user id ({{if .GitHub}}GitHub's numeric
id{{end}}{{if and .GitHub .Google}}, {{end}}{{if .Google}}Google's account identifier{{end}}): no email, no
name, no password.</p>
<p>{{if .GitHub}}<a class="btn" href="/account/login?provider=github{{if .Next}}&amp;next=chat{{end}}">Sign in with GitHub</a>{{end}}
{{if .Google}}<a class="btn" href="/account/login?provider=google{{if .Next}}&amp;next=chat{{end}}">Sign in with Google</a>{{end}}</p>
<p class="mut">New accounts are let in in order while there is room; otherwise you get a place in the
waitlist.</p></div>
{{else if eq .Account.Status "waitlisted"}}
<h1>You are on the waitlist</h1>
<div class="card"><p>Place <strong>{{.Position}}</strong>. Accounts are let in in order as room
appears; come back to this page to check.</p></div>
{{else if eq .Account.Status "banned"}}
<h1>Account closed</h1>
<div class="card"><p>This account can no longer use AIs@home.</p></div>
{{else}}
<h1>Your account</h1>
<div class="card"><p><a class="btn" href="{{.ChatURL}}">Open the chat</a></p>
<p class="mut">Today: {{.Used}}{{if .Budget}} of {{.Budget}}{{end}} tokens (resets 00:00 UTC; chat and API share it).</p></div>
{{if .NewKey}}<div class="card"><p><strong>Your new API key.</strong> Copy it now: it is shown only once.</p>
<code class="key">{{.NewKey}}</code>
<p class="mut">Use it with any OpenAI-compatible client at <code>{{.API}}/v1</code> (Authorization: Bearer).</p></div>{{end}}
<div class="card"><h2 style="font-size:1.1rem;margin-top:0">API keys</h2>
{{if .Account.Keys}}<table>{{range .Account.Keys}}<tr><td><code>{{.Prefix}}…</code></td><td class="mut">{{day .Created}}</td>
<td style="text-align:right"><form method="post" action="/account/keys/revoke"><input type="hidden" name="prefix" value="{{.Prefix}}">
<button class="btn ghost">Revoke</button></form></td></tr>{{end}}</table>{{else}}<p class="mut">No keys yet.</p>{{end}}
{{if lt (len .Account.Keys) .MaxKeys}}<form method="post" action="/account/keys"><input type="hidden" name="once" value="{{.Once}}"><p><button class="btn">Create a key</button></p></form>{{end}}
<p class="mut">See <a href="/use/api/">API and opencode</a>.</p></div>
{{end}}
{{if .SignedIn}}<form method="post" action="/account/logout"><button class="btn ghost">Sign out</button></form>{{end}}
</main></body></html>`))
