// HTTP side of accounts: GitHub and Google sign-in (email sign-in: email.go), the account page, and the check Caddy's
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
	"mime"
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
	mux.HandleFunc("POST /account/login", smallForm(g.handleLoginForm))
	if g.acfg.githubOn() {
		mux.HandleFunc("GET /account/callback", g.handleCallback)
	}
	if g.acfg.googleOn() {
		mux.HandleFunc("GET /account/callback/google", g.handleGoogleCallback)
	}
	if g.email != nil {
		g.emailRoutes(mux)
	}
	mux.HandleFunc("POST /account/keys", smallForm(g.handleKeyCreate))
	mux.HandleFunc("POST /account/keys/revoke", smallForm(g.handleKeyRevoke))
	mux.HandleFunc("POST /account/nodes", smallForm(g.handleNodeCreate))
	mux.HandleFunc("POST /account/nodes/revoke", smallForm(g.handleNodeRevoke))
	mux.HandleFunc("POST /account/nodes/pref", smallForm(g.handleNodePref))
	mux.HandleFunc("POST /account/username", smallForm(g.handleUsername))
	mux.HandleFunc("POST /account/logout", smallForm(g.handleLogout))
	mux.HandleFunc("POST /account/theme", smallForm(g.handleTheme))
	mux.HandleFunc("POST /account/contact/connect", smallForm(g.handleContactConnect))
	mux.HandleFunc("POST /account/contact/email", smallForm(g.handleContactEmail))
	mux.HandleFunc("POST /account/contact/off", smallForm(g.handleContactOff))
	mux.HandleFunc("POST /account/operator", smallForm(g.handleOperatorTerms))
	mux.HandleFunc("POST /account/transfer", smallForm(g.handleTransferConsent))
	mux.HandleFunc("POST /account/connectors/{id}/run", smallForm(g.handleConnectorRun))
	mux.HandleFunc("POST /account/connectors/{id}/claim", smallForm(g.handleConnectorClaim))
	mux.HandleFunc("POST /account/connectors/{id}/email", smallForm(g.handleConnectorEmail))
	mux.HandleFunc("POST /account/pilots/chat", smallForm(g.handlePilotChatForm))
	mux.HandleFunc("POST /account/notice/dismiss", smallForm(g.handleNoticeDismiss))
	mux.HandleFunc("GET /account/notice/chat", g.handleNoticeChat)
	mux.HandleFunc("GET /account/auth", g.handleForwardAuth)
	mux.HandleFunc("GET /account/ops", g.handleOps)
	mux.HandleFunc("POST /account/ops/clear", smallForm(g.handleOpsClear))
	mux.HandleFunc("GET /account/assets/{name}", handleAsset)
}

// smallForm accepts only short URL-encoded forms. The account pages upload nothing, and
// multipart parsing could spill parts to disk before the session is even checked.
func smallForm(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if ct := r.Header.Get("Content-Type"); ct != "" {
			if mt, _, err := mime.ParseMediaType(ct); err != nil || mt != "application/x-www-form-urlencoded" {
				http.Error(w, "unsupported form encoding", http.StatusUnsupportedMediaType)
				return
			}
		}
		r.Body = http.MaxBytesReader(w, r.Body, 8<<10)
		if err := r.ParseForm(); err != nil {
			http.Error(w, "form too large or malformed", http.StatusRequestEntityTooLarge)
			return
		}
		h(w, r)
	}
}

// accountEmail is the chat's address for account id ("gh:123" → "gh-123@users.invalid",
// "google:1045" → "google-1045@users.invalid", "em:<hex>" → "em-<hex>@users.invalid").
func accountEmail(id string) string { return strings.ReplaceAll(id, ":", "-") + accountEmailSuffix }

// accountFromEmail reverses accountEmail; ok only for well-formed GitHub, Google and email
// accounts (digits, or exactly 32 lowercase hex digits, so no address can name anything
// else).
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
	if h, em := strings.CutPrefix(local, "em-"); em && len(h) == 32 && strings.Trim(h, "0123456789abcdef") == "" {
		return "em:" + h, true
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
// in a short-lived cookie that the callback clears: the provider, the state, where to go next,
// for Google the OpenID Connect nonce and the PKCE verifier, and whether the user ticked "Email
// me about important changes and outages" for this sign-in (contact "1") and whether it was
// started from the account page to add the address to the signed-in account (connect "1"), and whether
// they ticked the opt-in to volunteers outside Europe (transfer "1", chatrules.go).
type oauthFlow struct{ provider, state, next, nonce, verifier, contact, connect, transfer string }

func (f oauthFlow) String() string {
	return strings.Join([]string{f.provider, f.state, f.next, f.nonce, f.verifier, f.contact, f.connect, f.transfer}, "|")
}

// takeFlow reads and clears the flow cookie; ok only if it was started for provider and the
// callback carries its state.
func (g *Gateway) takeFlow(w http.ResponseWriter, r *http.Request, provider string) (oauthFlow, bool) {
	var f oauthFlow
	if c, err := r.Cookie(g.cookieName(oauthCookie)); err == nil {
		switch p := strings.Split(c.Value, "|"); len(p) {
		case 8:
			f = oauthFlow{p[0], p[1], p[2], p[3], p[4], p[5], p[6], p[7]}
		case 7: // a flow started before the transfer opt-in existed
			f = oauthFlow{p[0], p[1], p[2], p[3], p[4], p[5], p[6], ""}
		case 5: // a flow started before the contact fields existed
			f = oauthFlow{provider: p[0], state: p[1], next: p[2], nonce: p[3], verifier: p[4]}
		}
	}
	g.setCookie(w, oauthCookie, "", "/account", -time.Second)
	got := r.URL.Query().Get("state")
	return f, f.provider == provider && f.state != "" && subtle.ConstantTimeCompare([]byte(got), []byte(f.state)) == 1
}

// handleLogin starts sign-in with ?provider=github (the default, if enabled) or google, as a
// link can: never with the contact address, whose consent only the sign-in form (a same-origin
// POST) carries, so a link from elsewhere cannot tick the box for someone.
func (g *Gateway) handleLogin(w http.ResponseWriter, r *http.Request) {
	f := oauthFlow{provider: r.URL.Query().Get("provider")}
	if r.URL.Query().Get("next") == "chat" {
		f.next = "chat"
	}
	g.startLogin(w, r, f)
}

// handleLoginForm starts sign-in from the sign-in page's form: provider, next and the box
// "Email me about important changes and outages" (contact=1). The form also carries the email
// field of email sign-in, which is not read here.
func (g *Gateway) handleLoginForm(w http.ResponseWriter, r *http.Request) {
	if !g.sameOrigin(r) {
		http.Redirect(w, r, g.acfg.PublicURL+"/account", http.StatusSeeOther)
		return
	}
	f := oauthFlow{provider: r.PostFormValue("provider")}
	if r.PostFormValue("next") == "chat" {
		f.next = "chat"
	}
	if r.PostFormValue("contact") == "1" {
		f.contact = "1"
	}
	if r.PostFormValue("transfer") == TransferConsentVersion { // the box carries the version of the text it showed
		f.transfer = "1"
	}
	g.startLogin(w, r, f)
}

// startLogin sends the browser to the provider of f (github, the default if enabled, or
// google). Without contact, GitHub gets no scopes: /user without a scope gives the public
// profile, of which only the numeric id is kept; Google gets scope "openid" only (google.go).
// With contact, GitHub also gets "user:email" and Google "email" (contact.go).
func (g *Gateway) startLogin(w http.ResponseWriter, r *http.Request, f oauthFlow) {
	if f.provider == "" {
		f.provider = "github"
		if !g.acfg.githubOn() {
			f.provider = "google"
		}
	}
	f.state = randomToken()
	var target string
	switch {
	case f.provider == "github" && g.acfg.githubOn():
		q := url.Values{"client_id": {g.acfg.GitHubClientID}, "state": {f.state},
			"redirect_uri": {g.acfg.PublicURL + "/account/callback"}, "allow_signup": {"true"}}
		if f.contact == "1" {
			q.Set("scope", "user:email")
		}
		target = g.acfg.GitHubAuthURL + "?" + q.Encode()
	case f.provider == "google" && g.acfg.googleOn():
		f.nonce, f.verifier = randomToken(), randomToken()
		target = g.googleAuthURL(f)
	default:
		g.accountPage(w, r, http.StatusNotFound, pageData{Error: "There is no such way to sign in."})
		return
	}
	g.setCookie(w, oauthCookie, f.String(), "/account", 10*time.Minute)
	status := http.StatusFound
	if r.Method == http.MethodPost {
		status = http.StatusSeeOther
	}
	http.Redirect(w, r, target, status)
}

// handleCallback finishes GitHub sign-in: code → token → numeric id. The token is used once
// and dropped.
func (g *Gateway) handleCallback(w http.ResponseWriter, r *http.Request) {
	f, ok := g.takeFlow(w, r, "github")
	if !ok {
		g.accountPage(w, r, http.StatusBadRequest, pageData{Error: "The sign-in expired or came from elsewhere. Please try again."})
		return
	}
	id, addr, err := g.githubID(r, r.URL.Query().Get("code"), f.contact == "1")
	if err != nil {
		g.log.Warn("github sign-in failed", "err", err)
		g.accountPage(w, r, http.StatusBadGateway, pageData{Error: "GitHub sign-in failed. Please try again."})
		return
	}
	g.finishSignIn(w, r, f, id, addr, ContactGitHub)
}

// finishSignIn creates or finds the account for a verified provider id and starts a session.
// addr is the provider's verified address when the flow asked for one (else ""). A flow from
// the account page's "Connect email" only adds the address, and only to the signed-in account.
func (g *Gateway) finishSignIn(w http.ResponseWriter, r *http.Request, f oauthFlow, id, addr, source string) {
	if f.connect == "1" {
		if sid, ok := g.session(r); !ok || sid != id {
			d := pageData{}
			if ok {
				d = g.accountData(sid)
			}
			d.Error = "You chose a different " + sourceName(source) + " account than the one you are signed in with. Nothing was stored."
			g.accountPage(w, r, http.StatusBadRequest, d)
			return
		}
		back := "/account#contact"
		if !g.storeContact(id, addr, source) {
			back = "/account?contact=none#contact"
		}
		http.Redirect(w, r, g.acfg.PublicURL+back, http.StatusFound)
		return
	}
	a, err := g.accounts.SignIn(id, time.Now())
	next := f.next
	if err == nil && f.contact == "1" && a.Status != StatusBanned && !g.storeContact(id, addr, source) {
		next = contactNone
	}
	if err == nil && f.transfer == "1" && a.Status != StatusBanned {
		g.storeTransfer(id)
	}
	g.signedIn(w, r, a, err, next)
}

// signedIn starts a session for a found or created account (err from SignIn or SignInEmail).
func (g *Gateway) signedIn(w http.ResponseWriter, r *http.Request, a Account, err error, next string) {
	if errors.Is(err, errSignupClosed) {
		g.accountPage(w, r, http.StatusForbidden, pageData{Error: "Sign-up is closed for now. Please try again later."})
		return
	} else if err != nil {
		g.log.Error("account sign-in", "err", err)
		g.accountPage(w, r, http.StatusInternalServerError, pageData{Error: "Something went wrong. Please try again."})
		return
	}
	g.setCookie(w, sessionCookie, signSession(g.acfg.SessionSecret, a.ID, time.Now().Add(sessionTTL)), "/", sessionTTL)
	if next == "chat" && a.Status == StatusActive && g.acfg.ChatURL != "" {
		http.Redirect(w, r, g.acfg.ChatURL, http.StatusFound)
		return
	}
	if next == contactNone { // the account page says that no address was available
		http.Redirect(w, r, g.acfg.PublicURL+"/account?contact=none#contact", http.StatusFound)
		return
	}
	http.Redirect(w, r, g.acfg.PublicURL+"/account", http.StatusFound)
}

// githubID exchanges an OAuth code for the user's numeric GitHub id ("gh:<id>") and, with
// wantEmail (the user asked for notices, so the flow had scope user:email), their primary
// verified address, or "" if there is none.
func (g *Gateway) githubID(r *http.Request, code string, wantEmail bool) (string, string, error) {
	if code == "" {
		return "", "", errors.New("no code")
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
		return "", "", err
	}
	if tok.AccessToken == "" {
		return "", "", fmt.Errorf("token exchange: %s", tok.Error)
	}
	req, _ = http.NewRequestWithContext(r.Context(), "GET", g.acfg.GitHubUserURL, nil)
	req.Header.Set("Authorization", "Bearer "+tok.AccessToken)
	req.Header.Set("Accept", "application/vnd.github+json")
	var user struct {
		ID int64 `json:"id"`
	}
	if err := g.getJSON(req, &user); err != nil {
		return "", "", err
	}
	if user.ID <= 0 {
		return "", "", errors.New("no user id")
	}
	addr := ""
	if wantEmail {
		addr = g.githubEmail(r, tok.AccessToken)
	}
	return fmt.Sprintf("gh:%d", user.ID), addr, nil
}

// githubEmail is the user's primary address if GitHub has verified it, else "". Without it
// sign-in still works; the account page then says that no address was available.
func (g *Gateway) githubEmail(r *http.Request, token string) string {
	req, _ := http.NewRequestWithContext(r.Context(), "GET", g.acfg.GitHubEmailsURL, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	var list []struct {
		Email    string `json:"email"`
		Primary  bool   `json:"primary"`
		Verified bool   `json:"verified"`
	}
	if err := g.getJSON(req, &list); err != nil {
		g.log.Warn("github sign-in: no address list", "err", err) // host and status only
		return ""
	}
	for _, e := range list {
		if e.Primary && e.Verified && validEmail(e.Email) {
			return e.Email
		}
	}
	return ""
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
		// Only page loads are sent to sign in. The chat app's own calls (fetch, websocket) get
		// 401: a redirect to another origin fails there, and the app reloaded itself in a loop
		// after a sign-out (seen on staging, 2026-10-10). Caddy passes the original headers.
		if navigation(r) {
			http.Redirect(w, r, g.acfg.PublicURL+"/account?next=chat", http.StatusFound)
		} else {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			fmt.Fprintf(w, `{"detail":"Not signed in. Sign in at %s/account"}`, g.acfg.PublicURL)
		}
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
		g.accountPage(w, r, http.StatusOK, pageData{Next: r.URL.Query().Get("next") == "chat"})
		return
	}
	d := g.accountData(id)
	if r.URL.Query().Get("contact") == "none" && d.ContactMasked == "" {
		d.ContactNone = "No verified email address was available"
		if name := sourceName(d.ContactWay); d.ContactWay != ContactEmail && name != "" {
			d.ContactNone = name + " gave no verified email address"
		}
		d.ContactNone += ", so none is stored and you get no notices. Check that your address there is verified, then try again."
	}
	g.accountPage(w, r, http.StatusOK, d)
}

func (g *Gateway) accountData(id string) pageData {
	a, pos, ok := g.accounts.Get(id)
	if !ok || a.Status == StatusUnclaimed { // no sign-in makes such a session; treat one as none
		return pageData{}
	}
	used := g.budget.used1(accountBudgetKey(id), time.Now())
	d := pageData{SignedIn: true, Account: a, Position: pos, API: g.acfg.PublicURL,
		Used: used, Budget: g.acfg.Limits.TokensPerDay, MaxKeys: g.acfg.MaxKeys, Once: g.createToken(id, a.KeySeq),
		NodeOnce: g.nodeFormToken(id, a.NodeSeq), Admin: slices.Contains(g.acfg.Admins, id) && g.accounts.active(id)}
	now := time.Now()
	for _, n := range a.Nodes {
		c := g.credit.node(n.ID)
		version, proto := g.nodeVersion(id, n.ID)
		work, speed := g.nodeWorkView(id, n.ID)
		d.Nodes = append(d.Nodes, nodeRow{ID: n.ID, Name: n.Name, Created: n.Created, Status: g.nodeStatus(id, n.ID),
			Version: version, Protocol: proto, CreditToday: fmtCredit(c.TodayAt(now)), CreditTotal: fmtCredit(c.total()),
			Work: work, Speed: speed, Chat: g.chatView(id, n.ID)})
	}
	if c := g.credit.account(id); c != nil {
		d.CreditToday, d.CreditTotal, d.CreditRAC = fmtCredit(c.TodayAt(now)), fmtCredit(c.Total), fmtCredit(c.RAC(now))
	}
	if g.acfg.probation > 0 { // off by default (owner, 2026-10-12)
		if a.FirstNode != nil {
			d.ProbationEnd = a.FirstNode.Add(g.acfg.probation)
		}
		d.Probation = probationText(g.acfg.probation)
	}
	if a.ContactEmail != "" && a.ContactConsentAt != nil {
		d.ContactMasked, d.ContactSince, d.ContactFrom = maskEmail(a.ContactEmail), *a.ContactConsentAt, sourceName(a.ContactSource)
	}
	d.ContactWay = g.contactWay(id)
	g.chatRulesData(&d)
	d.Account.ContactEmail = "" // the page shows the masked form only
	st := g.accounts.Settings()
	d.MaxNodes = st.MaxNodesPerAccountOrDefault()
	d.NodesClosed = st.NodesClosed
	d.NetworkFull = g.accounts.nodeTotal() >= st.MaxNodesOrDefault()
	if a.Status == StatusActive {
		g.connectorData(&d, a)
	}
	return d
}

func (g *Gateway) handleKeyCreate(w http.ResponseWriter, r *http.Request) {
	id, ok := g.session(r)
	if !ok || !g.sameOrigin(r) {
		http.Redirect(w, r, g.acfg.PublicURL+"/account", http.StatusSeeOther)
		return
	}
	d := g.accountData(id)
	once := r.FormValue("once")
	agents := r.FormValue("agents") == "1"
	key, err := g.accounts.AddKey(id, g.acfg.MaxKeys, time.Now(), agents, func(seq int) bool {
		return hmac.Equal([]byte(once), []byte(g.createToken(id, seq)))
	})
	if errors.Is(err, errStaleForm) { // a reloaded, replayed or concurrent post of one form
		http.Redirect(w, r, g.acfg.PublicURL+"/account", http.StatusSeeOther)
		return
	}
	if err != nil {
		d.Error = err.Error()
		g.accountPage(w, r, http.StatusBadRequest, d)
		return
	}
	d = g.accountData(id)
	d.NewKey = key
	g.accountPage(w, r, http.StatusOK, d) // shown once: rendered here, never stored
}

func (g *Gateway) handleKeyRevoke(w http.ResponseWriter, r *http.Request) {
	id, ok := g.session(r)
	if ok && g.sameOrigin(r) {
		if err := g.accounts.RevokeKey(id, r.FormValue("prefix")); err != nil {
			d := g.accountData(id)
			d.Error = err.Error()
			g.accountPage(w, r, http.StatusBadRequest, d)
			return
		}
	}
	http.Redirect(w, r, g.acfg.PublicURL+"/account", http.StatusSeeOther)
}

func (g *Gateway) handleNodeCreate(w http.ResponseWriter, r *http.Request) {
	id, ok := g.session(r)
	if !ok || !g.sameOrigin(r) {
		http.Redirect(w, r, g.acfg.PublicURL+"/account", http.StatusSeeOther)
		return
	}
	once := r.FormValue("once")
	token, nodeID, err := g.accounts.AddNode(id, r.FormValue("name"), time.Now(), func(seq int) bool {
		return hmac.Equal([]byte(once), []byte(g.nodeFormToken(id, seq)))
	})
	if errors.Is(err, errStaleForm) {
		http.Redirect(w, r, g.acfg.PublicURL+"/account", http.StatusSeeOther)
		return
	}
	d := g.accountData(id)
	if err != nil {
		d.Error = err.Error()
		g.accountPage(w, r, http.StatusBadRequest, d)
		return
	}
	g.log.Info("node token created", "node", nodeID, "operator", g.pseudonym(id))
	d.NewNode, d.NewNodeID = token, nodeID
	g.accountPage(w, r, http.StatusOK, d) // shown once: rendered here, never stored
}

func (g *Gateway) handleNodeRevoke(w http.ResponseWriter, r *http.Request) {
	id, ok := g.session(r)
	if ok && g.sameOrigin(r) {
		if err := g.accounts.RevokeNode(id, r.FormValue("id")); err != nil {
			d := g.accountData(id)
			d.Error = err.Error()
			g.accountPage(w, r, http.StatusBadRequest, d)
			return
		}
		g.sweepNodes() // the node goes now, not at the next sweep
	}
	http.Redirect(w, r, g.acfg.PublicURL+"/account", http.StatusSeeOther)
}

func (g *Gateway) handleNodePref(w http.ResponseWriter, r *http.Request) {
	id, ok := g.session(r)
	if ok && g.sameOrigin(r) {
		if err := g.accounts.SetNodePref(id, r.FormValue("pref")); err != nil {
			d := g.accountData(id)
			d.Error = err.Error()
			g.accountPage(w, r, http.StatusBadRequest, d)
			return
		}
	}
	http.Redirect(w, r, g.acfg.PublicURL+"/account", http.StatusSeeOther)
}

func (g *Gateway) handleUsername(w http.ResponseWriter, r *http.Request) {
	id, ok := g.session(r)
	if ok && g.sameOrigin(r) {
		if err := g.accounts.SetUsername(id, r.FormValue("username")); err != nil {
			d := g.accountData(id)
			d.Error, d.TriedName = err.Error(), r.FormValue("username")
			g.accountPage(w, r, http.StatusBadRequest, d)
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
	shell
	GitHub   bool // sign-in providers on offer
	Google   bool
	Email    bool
	Separate string // "GitHub, Google and email" when more than one way is on offer
	SignedIn bool
	Admin    bool // the account is one of accounts.admins: the page links to /account/ops
	Next     bool
	Account  Account
	Position int
	NewKey   string
	Error    string
	API      string
	Used     int64
	Budget   int
	MaxKeys  int
	Once     string // one-time token for the "create a key" form

	Nodes        []nodeRow
	NewNode      string // a new node token, shown once
	NewNodeID    string
	NodeOnce     string // one-time token for the "add a node" form
	MaxNodes     int    // node tokens per account
	NodesClosed  bool
	NetworkFull  bool
	ProbationEnd time.Time
	Probation    string // its length, e.g. "7 days"

	CreditToday, CreditTotal, CreditRAC string // the account's credit; "" before any
	TriedName                           string // a username that was refused, to show it again

	// The contact address (contact.go): shown masked, never in full.
	ContactMasked string    // "" when there is none
	ContactSince  time.Time // when the user agreed
	ContactFrom   string    // "GitHub", "Google" or "email sign-in"
	ContactWay    string    // how to turn it on from this page: "github", "google", "email" or ""
	ContactNone   string    // after a sign-in that asked for an address and got none: says so

	chatRulesPage // operator terms and the opt-in to volunteers outside Europe (chatrules.go)

	// Connectors with accounts (account_connectors.go): a card each, the account's pilots, and
	// whether any of them lets pilots serve chat at all yet.
	Connectors []connectorCard
	Pilots     []pilotRow
	PilotsChat bool
}

type nodeRow struct {
	ID, Name, Status         string
	Version                  string // the node software's version while it is connected, "" otherwise
	Protocol                 int    // the protocol version it speaks while connected, 0 otherwise
	Created                  time.Time
	CreditToday, CreditTotal string
	// Work: what it serves right now, while connected (nodework.go: no content, nobody's identity);
	// Speed: its average over its last answers, "" before any.
	Work  []workItem
	Speed string
	Chat  string // while connected: whether it serves people's requests under the chat rules (chatrules.go)
}

// createToken ties a "create a key" form to the account's KeySeq (see AccountStore.AddKey).
func (g *Gateway) createToken(id string, seq int) string {
	m := hmac.New(sha256.New, []byte(g.acfg.SessionSecret))
	fmt.Fprintf(m, "create|%s|%d", id, seq)
	return hex.EncodeToString(m.Sum(nil)[:12])
}

// probationText says how long probation lasts, in days when it is whole days.
func probationText(p time.Duration) string {
	if p >= 24*time.Hour && p%(24*time.Hour) == 0 {
		if d := int(p / (24 * time.Hour)); d != 1 {
			return fmt.Sprintf("%d days", d)
		}
		return "1 day"
	}
	return p.String()
}

// nodeFormToken ties an "add a node" form to the account's NodeSeq (see AccountStore.AddNode).
func (g *Gateway) nodeFormToken(id string, seq int) string {
	m := hmac.New(sha256.New, []byte(g.acfg.SessionSecret))
	fmt.Fprintf(m, "node|%s|%d", id, seq)
	return hex.EncodeToString(m.Sum(nil)[:12])
}

func (g *Gateway) accountPage(w http.ResponseWriter, r *http.Request, status int, d pageData) {
	d.GitHub, d.Google, d.Email = g.acfg.githubOn(), g.acfg.googleOn(), g.email != nil
	var ways []string
	for _, w := range []struct {
		on   bool
		name string
	}{{d.GitHub, "GitHub"}, {d.Google, "Google"}, {d.Email, "email"}} {
		if w.on {
			ways = append(ways, w.name)
		}
	}
	if n := len(ways); n > 1 {
		d.Separate = strings.Join(ways[:n-1], ", ") + " and " + ways[n-1]
	}
	d.shell = shell{Title: "Your account", ChatURL: g.acfg.ChatURL, Theme: themeOf(r), Back: "/account"}
	// The sign-in form's opt-in to volunteers outside Europe (chatrules.go).
	d.RulesOn, d.TransferText, d.TransferVer = g.chatRulesOn(), TransferConsentText, TransferConsentVersion
	if d.NewKey == "" && d.NewNode == "" { // no button that leaves a secret's only showing
		d.Notice = g.noticeFor(r)
	}
	if d.Next {
		d.Back = "/account?next=chat"
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store") // a new key must not end up in a cache
	csp := pageCSP + "; form-action 'self'" + g.formTargets() + "; frame-ancestors 'none'"
	if d.NewKey != "" || d.NewNode != "" {
		csp += secretScriptSrc // the Copy button; every other account page runs no script
	}
	w.Header().Set("Content-Security-Policy", csp)
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.WriteHeader(status)
	if err := accountTmpl.Execute(w, d); err != nil {
		g.log.Error("account page", "err", err)
	}
}

// formTargets are the other origins a sign-in form's post may end at: the providers, and the
// chat that a finished sign-in with next=chat lands on (browsers apply CSP form-action to the
// whole redirect chain after a post, and a provider that remembers the user redirects straight
// back).
func (g *Gateway) formTargets() string {
	var out string
	for _, on := range []struct {
		on  bool
		url string
	}{{g.acfg.githubOn(), g.acfg.GitHubAuthURL}, {g.acfg.googleOn(), g.acfg.GoogleAuthURL}, {g.acfg.ChatURL != "", g.acfg.ChatURL}} {
		if u, err := url.Parse(on.url); on.on && err == nil && u.Host != "" {
			out += " " + u.Scheme + "://" + u.Host
		}
	}
	return out
}

var accountTmpl = newPage("account", template.FuncMap{
	"day": func(t time.Time) string { return t.Format("2006-01-02") },
	"secretView": func(title, secret string) map[string]string {
		return map[string]string{"Title": title, "Secret": secret}
	},
}, `{{template "head" .}}{{template "top" .}}
{{if .Error}}<div class="card err" role="alert">{{.Error}}</div>{{end}}
{{if not .SignedIn}}
<h1>Sign in</h1>
<div class="card">{{if or .GitHub .Google}}<p>AIs@home uses your {{if .GitHub}}GitHub{{end}}{{if and .GitHub .Google}} or {{end}}{{if .Google}}Google{{end}}
account only to tell accounts apart. We keep only its user id ({{if .GitHub}}GitHub's numeric
id{{end}}{{if and .GitHub .Google}}, {{end}}{{if .Google}}Google's account identifier{{end}}): no name, no password, and
no email unless you tick the box below.</p>{{end}}
<form class="form" id="signin" method="post" action="{{if .Email}}/account/email{{else}}/account/login{{end}}">{{if .Next}}<input type="hidden" name="next" value="chat">{{end}}
{{if .Email}}<button class="default-submit" tabindex="-1" aria-hidden="true">Email me a code</button>{{end}}
{{if or .GitHub .Google}}<div class="signin">{{if .GitHub}}<button class="btn-gh" formaction="/account/login" formnovalidate name="provider" value="github">`+githubMark+`<span>Sign in with GitHub</span></button>{{end}}
{{if .Google}}<button class="btn-google" formaction="/account/login" formnovalidate name="provider" value="google">{{if .Theme}}<img src="{{asset (printf "google-signin-%s.svg" .Theme)}}" alt="Sign in with Google" width="180" height="40">{{else}}<picture>
<source media="(prefers-color-scheme: dark)" srcset="{{asset "google-signin-dark.svg"}}">
<img src="{{asset "google-signin-light.svg"}}" alt="Sign in with Google" width="180" height="40"></picture>{{end}}</button>{{end}}</div>{{end}}
<label class="check"><input type="checkbox" name="contact" value="1" id="contact"><span>Email me about important changes and outages</span></label>
<p class="hint">Optional. If you tick it, we keep your email address{{if or .GitHub .Google}} ({{if .Email}}the one you type below, or {{end}}a verified one from {{if .GitHub}}GitHub{{end}}{{if and .GitHub .Google}} or {{end}}{{if .Google}}Google{{end}}){{end}}
for these notices only, until you turn them off on your account page.</p>
{{if .RulesOn}}<label class="check"><input type="checkbox" name="transfer" value="{{.TransferVer}}" id="transfer"><span>{{.TransferText}}</span></label>
<p class="hint">Optional. Unticked, your chats go only to nodes in Europe or countries the EU recognises as protecting personal data,
which can mean waiting longer. You can change it on your account page.</p>{{end}}
{{if .Email}}<div class="field"><label for="email">{{if or .GitHub .Google}}Or sign in with email: we{{else}}Sign in with email: we{{end}}
send you a one-time code.</label>
<div class="row"><input id="email" type="email" name="email" required autocomplete="email" maxlength="254">
<button class="btn secondary">Email me a code</button></div></div>
<p class="mut small">For email we keep a keyed hash of the address (still personal data), and the address itself only if you tick the box.</p>{{end}}</form>
{{if .Separate}}<p class="mut small">{{.Separate}} are separate accounts; use the same one each time.</p>{{end}}
<p class="mut small">New accounts are let in in order while there is room; otherwise you get a place in the
waitlist.</p>
<p class="small" id="age">You must be 18 or older to use AIs@home. By signing in you accept the <a href="/about/terms/">terms of use</a>;
see also the <a href="/about/privacy/">privacy notice</a>.</p></div>
{{else if eq .Account.Status "waitlisted"}}
<h1>You are on the waitlist</h1>
{{if .Position}}<div class="card"><p>Place <strong>{{.Position}}</strong>. Accounts are let in in order as room
appears; come back to this page to check.</p></div>
{{else}}<div class="card"><p>Your account is on hold: the maintainer lets it in by hand. You can write to
<a href="mailto:contact@aisathome.org">contact@aisathome.org</a>.</p></div>{{end}}
{{template "contact" .}}
{{else if eq .Account.Status "banned"}}
<h1>Account closed</h1>
<div class="card"><p>This account can no longer use AIs@home.</p></div>
{{else if .NewKey}}<section aria-labelledby="secret-title">
{{template "secret" (secretView "Your new API key" .NewKey)}}
<p>Use it with any OpenAI-compatible client at <code>{{.API}}/v1</code> (Authorization: Bearer).</p>
<form method="get" action="/account#keys"><button class="btn">Done</button></form></section>
{{else if .NewNode}}<section aria-labelledby="secret-title">
{{template "secret" (secretView "Your new node token" .NewNode)}}
<p>Paste it when the node installer asks for it. The node takes its id (<code>{{.NewNodeID}}</code>) from the token.</p>
<form method="get" action="/account#nodes"><button class="btn">Done</button></form></section>
{{else}}
<h1>Your account</h1>
<div class="card"><p><a class="btn" href="{{.ChatURL}}">Open the chat</a></p>
<p class="mut">Today: {{.Used}}{{if .Budget}} of {{.Budget}}{{end}} tokens (resets 00:00 UTC; chat and API share it).</p></div>
{{if .Admin}}<div class="card" id="ops"><h2>Operations</h2><p><a href="/account/ops">Operations page</a>: the runner, its tasks, the nodes and health warnings (admins only).</p></div>{{end}}
<div class="card" id="keys"><h2>API keys</h2>
{{if .Account.Keys}}<div class="table-wrap"><table>{{range .Account.Keys}}<tr><td><code>{{.Prefix}}…</code>{{if not .Agents}} <span class="mut small">tools: your own nodes only</span>{{end}}</td><td class="mut">{{day .Created}}</td>
<td class="end"><form method="post" action="/account/keys/revoke"><input type="hidden" name="prefix" value="{{.Prefix}}">
<button class="btn ghost">Revoke</button></form></td></tr>{{end}}</table></div>{{else}}<p class="mut">No keys yet.</p>{{end}}
{{if lt (len .Account.Keys) .MaxKeys}}<form class="form" method="post" action="/account/keys"><input type="hidden" name="once" value="{{.Once}}">
<label class="check"><input type="checkbox" name="agents" value="1" checked><span><strong>Run requests through nodes operated by
other people.</strong> If you untick this, you need your own node to use opencode or other tools.</span></label>
<div class="actions"><button class="btn">Create a key</button></div></form>
{{else}}<p>You have {{len .Account.Keys}} of {{.MaxKeys}} keys. Revoke one to create another.</p>{{end}}
<p class="mut small">Volunteers' answers are not checked yet: run opencode with approval of commands, and isolated.
See <a href="/use/api/">API and opencode</a>.</p></div>
<div class="card"><h2>Username</h2>
<form class="form" method="post" action="/account/username"><div class="field"><label for="username">Username</label>
<div class="row"><input id="username" name="username" maxlength="32" autocomplete="nickname" value="{{if .TriedName}}{{.TriedName}}{{else}}{{.Account.Username}}{{end}}">
<button class="btn ghost">Save</button></div>
<p class="hint">Shown publicly in the leaderboard and in the BOINC-format statistics, with your credit. Leave it empty to stay anonymous.</p></div></form>
{{if .Account.Username}}<p class="mut small">You appear as <strong>{{.Account.Username}}</strong> on the <a href="/network/">leaderboard</a>.</p>
{{else}}<p class="mut small">You are anonymous: your credit counts in the totals only.</p>{{end}}</div>
{{template "contact" .}}
{{template "transfer" .}}
<div class="card" id="nodes"><h2>Your nodes</h2>
<p class="mut">A node runs on your GPU and answers other people's chats. It can read what it serves, so the network
strips who asked and sends it no personal data. Requests with tools reach it only from users who allow that{{if .Probation}},
and only after a probation of {{.Probation}}{{end}}.</p>
{{if .CreditTotal}}<p>Credit: <strong>{{.CreditToday}}</strong> today, <strong>{{.CreditTotal}}</strong> in total
(recent average {{.CreditRAC}} a day). <a href="/node/credit/">How credit works</a>.</p>{{end}}
{{if .Nodes}}<div class="table-wrap"><table><tr><th>Node</th><th>Status</th><th>Version</th><th class="num">Credit today</th><th class="num">Credit total</th><th>Created</th><th></th></tr>
{{range .Nodes}}<tr class="node-main" data-node-id="{{.ID}}"><td>{{if .Name}}{{.Name}}<br>{{end}}<code>{{.ID}}</code></td><td><span class="badge {{.Status}}">{{.Status}}</span></td>
<td class="mut">{{if .Version}}<code>{{.Version}}</code>{{else if .Protocol}}unknown{{else}}–{{end}}{{if .Protocol}}<br>protocol {{.Protocol}}{{end}}</td>
<td class="num">{{.CreditToday}}</td><td class="num">{{.CreditTotal}}</td><td class="mut">{{day .Created}}</td>
<td class="end"><form method="post" action="/account/nodes/revoke"><input type="hidden" name="id" value="{{.ID}}">
<button class="btn danger">Revoke</button></form></td></tr>
<tr class="node-now" data-node-now="{{.ID}}"><td colspan="7"><span data-node-speed><span class="mut">Average speed:</span> {{if .Speed}}{{.Speed}}{{else}}not measured yet{{end}}</span>
{{with .Chat}}<span data-node-chat><span class="mut">Serves:</span> {{.}}</span>{{end}}
{{if .Protocol}}<span data-node-work><span class="mut">Working on now:</span> {{if .Work}}{{range $i, $w := .Work}}{{if $i}}; {{end}}{{if .Href}}<a href="{{.Href}}">{{.Text}}</a>{{else}}{{.Text}}{{end}}{{with .Detail}} <span class="mut">({{.}})</span>{{end}}{{end}}{{else}}nothing{{end}}</span>{{end}}</td></tr>{{end}}</table></div>
<p class="hint">Working on now is as of this page load: reload to see it change. Requests from people show as chat or API
requests only, never what they asked.</p>
{{if not .ProbationEnd.IsZero}}<p class="hint">Probation (chats only, no agents) until {{day .ProbationEnd}}.</p>{{end}}
{{else}}<p class="mut">No nodes yet.</p>{{end}}
{{if .NodesClosed}}<p class="mut">New nodes are not being accepted right now.</p>
{{else if ge (len .Nodes) .MaxNodes}}<p>You have {{len .Nodes}} of {{.MaxNodes}} nodes. Revoke one to add another.</p>
{{else if .NetworkFull}}<p class="mut">The network is full for now: no new nodes are being added.</p>
{{else}}<form class="form" method="post" action="/account/nodes"><input type="hidden" name="once" value="{{.NodeOnce}}">
<div class="field"><label for="nodename">Name (optional, only you see it)</label>
<div class="row"><input id="nodename" name="name" maxlength="40"><button class="btn secondary">Add a node</button></div></div></form>{{end}}
{{if or .Nodes .Account.NodePref}}<form class="form" method="post" action="/account/nodes/pref"><div class="field"><label for="pref">Your own chats and API requests go to</label>
<div class="row"><select id="pref" name="pref"><option value=""{{if eq .Account.NodePref ""}} selected{{end}}>my own nodes first, then any</option>
<option value="only"{{if eq .Account.NodePref "only"}} selected{{end}}>only my own nodes</option>
<option value="any"{{if eq .Account.NodePref "any"}} selected{{end}}>any node</option></select>
<button class="btn ghost">Save</button></div></div></form>{{end}}
<p class="mut small">See <a href="/node/">Run a node</a>.</p></div>
{{template "operator" .}}
{{if or .Connectors .Pilots}}{{template "connectors" .}}{{end}}
{{end}}
{{if or .NewKey .NewNode}}<script>`+secretScript+`</script>
{{else if .SignedIn}}<form method="post" action="/account/logout"><button class="btn ghost">Sign out</button></form>{{end}}
{{template "bottom" .}}
{{define "contact"}}<div class="card" id="contact"><div class="card-head"><h2>Email notices</h2>{{if .ContactMasked}}<span class="badge on">On</span>{{else}}<span class="badge off">Off</span>{{end}}</div>
{{if .ContactMasked}}<p>We may email <strong>{{.ContactMasked}}</strong> about important changes and outages, and about nothing else.
You agreed on {{day .ContactSince}}{{with .ContactFrom}} (the address is from {{.}}){{end}}.</p>
<form method="post" action="/account/contact/off"><button class="btn ghost">Turn off and delete the address</button></form>
{{else}}<p>Email me about important changes and outages: off. We keep no email address for this account.</p>
{{with .ContactNone}}<p class="mut" role="status">{{.}}</p>{{end}}
{{if eq .ContactWay "email"}}<form class="form" method="post" action="/account/contact/email"><div class="field"><label for="contact-email">To turn it on, type the address you sign in with</label>
<div class="row"><input id="contact-email" type="email" name="email" required autocomplete="email" maxlength="254"><button class="btn secondary">Turn on</button></div>
<p class="hint">We check it against the keyed hash we keep for signing in, then keep the address itself for these notices.</p></div></form>
{{else if .ContactWay}}<form method="post" action="/account/contact/connect"><button class="btn secondary">Connect email</button></form>
<p class="hint">Takes you to {{if eq .ContactWay "github"}}GitHub{{else}}Google{{end}} to share your email address; we keep it only if it is verified there.</p>
{{else}}<p class="mut">Not available for this account.</p>{{end}}{{end}}
<p class="mut small">Never shown publicly and never sent to nodes. Turning it off deletes the address at once.
See <a href="/about/privacy/">Privacy</a>.</p></div>{{end}}
{{define "connectors"}}{{range $c := .Connectors}}<div class="card" id="connector-{{$c.ID}}"><h2>{{$c.Name}}</h2>
{{range $c.Links}}<div class="link" data-link-user="{{.User}}"><p>Linked to {{$c.Name}} user <strong>{{.User}}</strong>{{if .Claimed}} (claimed){{end}}.
Its pilots run as your nodes, with your settings below.</p>
{{if .Key}}{{if .Command}}<p>To attach a machine with BOINC installed, run:</p>
<code class="key" data-attach-command>{{.Command}}</code>{{else}}<p>To attach a machine, add the project <code>{{$c.AttachURL}}</code> with this key:</p>
<code class="key" data-attach-key>{{.Key}}</code>{{end}}
<p class="hint">The key can only attach machines to this {{$c.Name}} account, nothing else. Keep it to yourself anyway.</p>{{end}}
{{if .Login}}<p>In BOINC Manager: Add project, <code>{{$c.AttachURL}}</code>, "existing account", and log in with
<code data-login>{{.Login}}</code> and the password you chose here (without one, use the command above).</p>
<form class="form" method="post" action="/account/connectors/{{$c.ID}}/email"><input type="hidden" name="user" value="{{.User}}">
<div class="field"><label for="own-email-{{.User}}">Use my own email address at {{$c.Name}}</label>
<div class="row"><input id="own-email-{{.User}}" type="email" name="email" required maxlength="254" autocomplete="email"><button class="btn secondary">Use it</button></div>
<p class="hint">Needed for one cross-project id (and Gridcoin). We pass it to {{$c.Name}} and delete it here as soon as {{$c.Name}}
has it; {{$c.Name}} then sends its own confirmation mail.</p></div></form>
{{else if .Created}}<p class="mut small">This {{$c.Name}} account uses your own email address.</p>{{end}}</div>{{end}}
{{with $c.Pending}}<p role="status" data-request-pending="{{.Kind}}"><span class="badge busy">Waiting</span> {{$c.Name}} is {{.What}} (asked {{.Since.Format "15:04 UTC"}}).
Reload this page in a few seconds.</p>{{end}}
{{with $c.Failed}}<p role="alert" data-request-failed="{{.Kind}}"><span class="badge off">Did not work</span> {{.What}}: {{.Error}}.</p>{{end}}
{{with $c.Done}}{{if eq .Kind "set_email"}}<p role="status" data-request-done="{{.Kind}}">{{$c.Name}} has your address now, and we deleted it here.</p>{{end}}{{end}}
{{if not $c.Pending}}{{if not $c.Links}}<form class="form" method="post" action="/account/connectors/{{$c.ID}}/run"><h3>Run on {{$c.Name}}</h3>
<p>Crunch for AIs@home with the {{$c.Name}} client you may already run. We make a {{$c.Name}} account for you and show you how to
attach your machine; its pilots then serve as your nodes and earn {{$c.Name}} credit.</p>
<fieldset class="field"><legend>Email address at {{$c.Name}}</legend>
<label class="check"><input type="radio" name="address" value="pseudonymous" checked><span>A made-up address, so {{$c.Name}} never
learns yours (we do not store it either)</span></label>
<label class="check"><input type="radio" name="address" value="own"><span>My own address (needed for one cross-project id and Gridcoin;
we pass it on and delete it here)</span></label>
<input type="email" name="email" maxlength="254" autocomplete="email" aria-label="Your own email address"></fieldset>
<div class="field"><label for="run-name-{{$c.ID}}">Display name (public in {{$c.Name}}'s statistics; empty: a made-up one)</label>
<input id="run-name-{{$c.ID}}" name="name" maxlength="40"></div>
<div class="field"><label for="run-pw-{{$c.ID}}">Password for BOINC Manager and the {{$c.Name}} site (optional, 8 or more characters)</label>
<input id="run-pw-{{$c.ID}}" type="password" name="password" maxlength="128" autocomplete="new-password">
<p class="hint">Choose it now: changing it later means attaching your machines again. Only {{$c.Name}} keeps it, as a hash; we keep nothing.
Without one, attach with the command line.</p></div>
<div class="actions"><button class="btn">Run on {{$c.Name}}</button></div></form>{{end}}
<form class="form" method="post" action="/account/connectors/{{$c.ID}}/claim"><h3>Claim a {{$c.Name}} account</h3>
<p>Already crunching for AIs@home on {{$c.Name}}? Your machines' work is kept under an unclaimed account until you claim it. Paste your
{{$c.Name}} account key{{if $c.KeyURL}} from <a href="{{$c.KeyURL}}">this page</a> (signed in to {{$c.Name}}){{end}}: the
attach-only key, which looks like <code>123456_</code> and 32 letters and digits.</p>
<div class="field"><label for="claim-key-{{$c.ID}}">{{$c.Name}} account key</label>
<div class="row"><input id="claim-key-{{$c.ID}}" name="key" required maxlength="200" autocomplete="off" spellcheck="false"><button class="btn secondary">Claim</button></div></div></form>{{end}}
</div>{{end}}
<div class="card" id="pilots"><div class="card-head"><h2>Your pilots</h2>{{if .Account.PilotChatOff}}<span class="badge off">Science only</span>{{else}}<span class="badge on">Chat on</span>{{end}}</div>
<p>A pilot is a serving session that a connector such as BOINC runs on your machine. It always serves science, which carries no
personal data. Unless you choose science only, it also serves people's chats, opencode and API requests, like any node
of yours: once you have accepted the operator terms.</p>
<p>Whoever runs a machine can read what it serves. The network strips who asked and sends no personal data to volunteers' machines,
but that is <strong>hardened, not confidential</strong>, and the <a href="/about/terms/">node-operator terms</a> apply: do not log or
look at what passes through.</p>
{{if not .PilotsChat}}<p class="mut" data-pilots-science-only>For now every pilot serves science only, whatever you choose here: people's chats reach
volunteers' machines only once the anti-retention measures and the privacy notice for them are in place.</p>{{end}}
<form method="post" action="/account/pilots/chat">{{if .Account.PilotChatOff}}<input type="hidden" name="chat" value="on"><button class="btn secondary">Allow chat on my pilots</button>
{{else}}<input type="hidden" name="chat" value="off"><button class="btn secondary">Science only on my pilots</button>{{end}}</form>
{{if .Pilots}}<div class="table-wrap"><table><tr><th>Pilot</th><th>State</th><th>Host</th><th>Started</th><th class="num">Served</th><th class="num">Answers</th><th class="num">Credit</th></tr>
{{range .Pilots}}<tr data-pilot-id="{{.ID}}"><td><code>{{.ID}}</code></td><td><span class="badge">{{.State}}</span></td><td class="mut">{{.Host}}</td>
<td class="mut">{{.Started.Format "2006-01-02 15:04"}}</td><td class="num">{{.Served}}</td><td class="num">{{.Requests}}</td>
<td class="num">{{.Credit}}{{if not .Final}} <span class="mut small">pending</span>{{end}}</td></tr>{{end}}</table></div>
<p class="hint">Pilot credit is granted through the connector (BOINC's own statistics), 24 hours after a pilot ends; it is not in the
credit above.</p>{{else}}<p class="mut">No pilots yet.</p>{{end}}</div>{{end}}
{{define "secret"}}<h1 id="secret-title">{{.Title}}</h1>
<div class="card accent"><p><strong>You will not see this again.</strong> Copy it now and keep it somewhere safe: only a hash
of it is stored, so nobody can show it to you later.</p>
<code class="key" id="secret">{{.Secret}}</code>
<div class="actions"><button type="button" class="btn secondary" id="copy" hidden>Copy</button>
<span id="copied" class="mut small" role="status" aria-live="polite"></span></div>
<noscript><p class="mut small">Select the text above and copy it.</p></noscript></div>{{end}}`+accountChatRulesTmpl)

// secretScript runs the Copy button of a new key or node token. Only the page that shows one
// has it, and its CSP allows it by hash. Without a script the button stays hidden and the token is selectable
// text; without the clipboard API (or permission) it selects the token and tries the older
// copy command, else asks the user to press Ctrl+C.
const secretScript = `(function(){var b=document.getElementById("copy"),k=document.getElementById("secret"),s=document.getElementById("copied");` +
	`if(!b||!k)return;b.hidden=false;b.focus();` +
	`function sel(){var r=document.createRange();r.selectNodeContents(k);var w=getSelection();w.removeAllRanges();w.addRange(r)}` +
	`function manual(){sel();var ok=false;try{ok=document.execCommand("copy")}catch(e){}s.textContent=ok?"Copied.":"Selected: press Ctrl+C to copy."}` +
	`b.addEventListener("click",function(){s.textContent="";if(navigator.clipboard&&window.isSecureContext){` +
	`navigator.clipboard.writeText(k.textContent.trim()).then(function(){s.textContent="Copied."},manual)}else{manual()}})})()`

// secretScriptSrc allows secretScript, and no other script, on the page that shows a secret.
var secretScriptSrc = func() string {
	sum := sha256.Sum256([]byte(secretScript))
	return "; script-src 'sha256-" + base64.StdEncoding.EncodeToString(sum[:]) + "'"
}()

// navigation reports whether a request is a page load in a browser (rather than a script's
// fetch or a websocket). Browsers send Sec-Fetch-Mode; without it, Accept decides.
func navigation(r *http.Request) bool {
	if m := r.Header.Get("Sec-Fetch-Mode"); m != "" {
		return m == "navigate"
	}
	return strings.Contains(r.Header.Get("Accept"), "text/html")
}
