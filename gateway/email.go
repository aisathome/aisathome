// Email sign-in: a one-time code and link by email, no passwords (research addendum
// 2026-10-10 in research/accounts/NOTES.md). The address is never stored or logged: accounts
// keep a keyed hash of it (HMAC-SHA256 under a key ring) and get a random id. Codes and link
// tokens live only in memory, as SHA-256 hashes, for 10 minutes, bound to the browser that
// asked by a "pending" cookie; a restart drops pending sign-ins.
//
// Every request for a code gets the same answer, whether a mail goes out or not (unknown
// address, throwaway domain, rate limit, lock), and the mail is sent from a queue after the
// answer, so the response says nothing about the address.
//
// SPDX-License-Identifier: Apache-2.0
package gateway

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/x509"
	_ "embed"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"net/mail"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"time"
)

// EmailConfig turns on email sign-in. Mail goes out over SMTP with STARTTLS (AWS SES:
// email-smtp.eu-north-1.amazonaws.com, port 587).
type EmailConfig struct {
	SMTPHost     string     `json:"smtp_host"`
	SMTPPort     int        `json:"smtp_port"`
	SMTPUser     string     `json:"smtp_user"`
	SMTPPassword string     `json:"smtp_password"`
	From         string     `json:"from"`       // e.g. AIs@home <noreply@aisathome.org>
	Keys         []EmailKey `json:"email_keys"` // key ring for the address hash; the first is current

	RootCAs *x509.CertPool   `json:"-"` // tests: trust a fake SMTP server
	Now     func() time.Time `json:"-"` // tests: a clock
}

// EmailKey is one key of the ring. Old keys stay in the ring until every account that
// matters has signed in once (which rewrites its hash under the current key).
type EmailKey struct {
	ID     string `json:"id"`
	Secret string `json:"secret"`
}

func (c *EmailConfig) check() error {
	if c.SMTPHost == "" || c.SMTPUser == "" || c.SMTPPassword == "" || c.From == "" {
		return errors.New("accounts.email: set smtp_host, smtp_port, smtp_user, smtp_password, from and email_keys")
	}
	if c.SMTPPort <= 0 || c.SMTPPort > 65535 {
		return errors.New("accounts.email: smtp_port must be a port number (587 for STARTTLS)")
	}
	if _, err := parseFrom(c.From); err != nil {
		return fmt.Errorf("accounts.email: from: %w", err)
	}
	if len(c.Keys) == 0 {
		return errors.New("accounts.email: email_keys needs at least one {id, secret}")
	}
	seen := map[string]bool{}
	for _, k := range c.Keys {
		if k.ID == "" || strings.Trim(k.ID, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789_-") != "" || len(k.ID) > 32 {
			return fmt.Errorf("accounts.email: key id %q: use 1-32 letters, digits, _ or -", k.ID)
		}
		if seen[k.ID] {
			return fmt.Errorf("accounts.email: key id %q appears twice", k.ID)
		}
		seen[k.ID] = true
		if len(k.Secret) < 32 {
			return fmt.Errorf("accounts.email: key %q: secret must be at least 32 characters", k.ID)
		}
	}
	if c.Now == nil {
		c.Now = time.Now
	}
	return nil
}

const (
	pendingCookie  = "aisathome_pending"
	emailCodeTTL   = 10 * time.Minute
	emailCodeTries = 5              // wrong codes per attempt; then the attempt is gone
	emailMaxFails  = 20             // wrong codes per address in emailFailWindow ...
	emailFailWin   = 24 * time.Hour //
	emailLockout   = time.Hour      // ... lock email sign-in for that address this long
	mailQueueLen   = 32
)

// rule allows n sends per period.
type rule struct {
	n   int
	per time.Duration
}

// Send limits (research addendum). A request over any of them gets the usual answer and no
// mail.
var (
	perAddress = []rule{{1, time.Minute}, {5, time.Hour}, {10, 24 * time.Hour}}
	perClient  = []rule{{10, time.Hour}, {30, 24 * time.Hour}}
	perDay     = []rule{{300, 24 * time.Hour}}
)

// events counts timestamps per key over the last day.
type events map[string][]time.Time

func (e events) allows(key string, now time.Time, rules []rule) bool {
	for _, r := range rules {
		if e.count(key, now, r.per) >= r.n {
			return false
		}
	}
	return true
}

func (e events) count(key string, now time.Time, per time.Duration) int {
	n := 0
	for _, t := range e[key] {
		if now.Sub(t) < per {
			n++
		}
	}
	return n
}

func (e events) add(key string, now time.Time) { e[key] = append(e[key], now) }

// prune drops what is older than a day, and keys with nothing left.
func (e events) prune(now time.Time) {
	for k, ts := range e {
		keep := ts[:0]
		for _, t := range ts {
			if now.Sub(t) < 24*time.Hour {
				keep = append(keep, t)
			}
		}
		if len(keep) == 0 {
			delete(e, k)
		} else {
			e[k] = keep
		}
	}
}

// emailAttempt is one pending sign-in: the code and link token as SHA-256, never in clear.
type emailAttempt struct {
	macs    []EmailMAC // the address under each key of the ring; macs[0] under the current one
	code    [32]byte
	token   [32]byte
	expires time.Time
	fails   int
	next    string
}

// emailMsg is a mail waiting in the queue. It holds the address, code and token only until
// it is sent.
type emailMsg struct {
	to, code, token string
	at              time.Time
}

// emailSignIn holds pending sign-ins and the counters, in memory only.
type emailSignIn struct {
	cfg       EmailConfig
	from      *mail.Address
	publicURL string
	site      string // the public URL's host, as named in the mail
	log       *slog.Logger
	queue     chan emailMsg

	mu       sync.Mutex
	pending  map[string]*emailAttempt // pending cookie value → attempt
	sends    events                   // "a:<mac>", "c:<client>", "*"
	fails    events                   // "<mac>": wrong codes
	locked   map[string]time.Time     // mac → locked until
	capNoted time.Time                // when the daily cap was last logged
}

func newEmailSignIn(cfg EmailConfig, publicURL string, log *slog.Logger) *emailSignIn {
	from, _ := parseFrom(cfg.From) // checked in check()
	site := publicURL
	if u, err := url.Parse(publicURL); err == nil && u.Host != "" {
		site = u.Host
	}
	return &emailSignIn{cfg: cfg, from: from, publicURL: publicURL, site: site, log: log,
		queue: make(chan emailMsg, mailQueueLen), pending: map[string]*emailAttempt{},
		sends: events{}, fails: events{}, locked: map[string]time.Time{}}
}

// macs returns the normalised address under every key of the ring, current key first.
func (e *emailSignIn) macs(norm string) []EmailMAC {
	out := make([]EmailMAC, 0, len(e.cfg.Keys))
	for _, k := range e.cfg.Keys {
		m := hmac.New(sha256.New, []byte(k.Secret))
		m.Write([]byte("email|" + norm))
		out = append(out, EmailMAC{KeyID: k.ID, MAC: hex.EncodeToString(m.Sum(nil))})
	}
	return out
}

// start queues a code for typed (normalised: norm) unless something stops it. It reports
// nothing on purpose: the caller answers the same either way.
func (e *emailSignIn) start(typed, norm, client, next, pending string) {
	macs := e.macs(norm)
	if isDisposable(norm[strings.LastIndexByte(norm, '@')+1:]) {
		return
	}
	mac := macs[0].MAC
	now := e.cfg.Now()
	code, token := newCode(), randomToken()
	e.mu.Lock()
	defer e.mu.Unlock()
	if now.Before(e.locked[mac]) || !e.sends.allows("a:"+mac, now, perAddress) || !e.sends.allows("c:"+client, now, perClient) {
		return
	}
	if !e.sends.allows("*", now, perDay) {
		if now.Sub(e.capNoted) > time.Hour {
			e.capNoted = now
			e.log.Warn("email sign-in: daily send limit reached; requests get no mail until it frees up")
		}
		return
	}
	select {
	case e.queue <- emailMsg{to: typed, code: code, token: token, at: now}:
	default:
		e.log.Warn("email sign-in: mail queue full, request dropped")
		return
	}
	for _, k := range []string{"a:" + mac, "c:" + client, "*"} {
		e.sends.add(k, now)
	}
	e.pending[pending] = &emailAttempt{macs: macs, code: sha256.Sum256([]byte(code)),
		token: sha256.Sum256([]byte(token)), expires: now.Add(emailCodeTTL), next: next}
}

// verify checks a code or a link token against the attempt of the pending cookie. A
// success consumes the attempt (code and token alike). Every wrong guess counts against the
// attempt and the address; a new code does not reset the address's count.
func (e *emailSignIn) verify(pending, code, token string) (macs []EmailMAC, next string, ok bool) {
	now := e.cfg.Now()
	e.mu.Lock()
	defer e.mu.Unlock()
	a := e.pending[pending]
	if a == nil {
		return nil, "", false
	}
	if !now.Before(a.expires) {
		delete(e.pending, pending)
		return nil, "", false
	}
	mac := a.macs[0].MAC
	if now.Before(e.locked[mac]) {
		return nil, "", false
	}
	var got, want [32]byte
	switch {
	case code != "" && token == "":
		got, want = sha256.Sum256([]byte(code)), a.code
	case token != "" && code == "":
		got, want = sha256.Sum256([]byte(token)), a.token
	default: // neither or both: not a guess
		return nil, "", false
	}
	if subtle.ConstantTimeCompare(got[:], want[:]) == 1 {
		delete(e.pending, pending)
		return a.macs, a.next, true
	}
	if a.fails++; a.fails >= emailCodeTries {
		delete(e.pending, pending)
	}
	e.fails.add(mac, now)
	if e.fails.count(mac, now, emailFailWin) >= emailMaxFails {
		e.locked[mac] = now.Add(emailLockout)
	}
	return nil, "", false
}

// prune forgets expired attempts, old counts and ended locks.
func (e *emailSignIn) prune() {
	now := e.cfg.Now()
	e.mu.Lock()
	defer e.mu.Unlock()
	for k, a := range e.pending {
		if !now.Before(a.expires) {
			delete(e.pending, k)
		}
	}
	e.sends.prune(now)
	e.fails.prune(now)
	for k, t := range e.locked {
		if !now.Before(t) {
			delete(e.locked, k)
		}
	}
}

// newCode is six digits, uniform, from the system's CSPRNG.
func newCode() string {
	n, err := rand.Int(rand.Reader, big.NewInt(1_000_000))
	if err != nil {
		panic(err) // crypto/rand does not fail on supported systems
	}
	return fmt.Sprintf("%06d", n.Int64())
}

// ---------------------------------------------------------------- addresses

// parseFrom reads "Name <address>" or a bare address. The name may contain "@", as in
// "AIs@home <noreply@aisathome.org>", which RFC 5322 only allows quoted; the header is written
// with the quotes (mail.Address.String).
func parseFrom(s string) (*mail.Address, error) {
	s = strings.TrimSpace(s)
	name, addr := "", s
	if i := strings.LastIndexByte(s, '<'); i >= 0 && strings.HasSuffix(s, ">") {
		name, addr = strings.Trim(strings.TrimSpace(s[:i]), `"`), s[i+1:len(s)-1]
	}
	if !validEmail(addr) || strings.ContainsAny(name, "\r\n\"<>") {
		return nil, fmt.Errorf("%q is not \"Name <address>\"", s)
	}
	return &mail.Address{Name: name, Address: addr}, nil
}

// normaliseEmail returns the form of an address that is hashed, or false if typed is not an
// address we take. Lowercase; for Gmail, without dots and "+tag" in the local part (Gmail
// ignores them) and with googlemail.com as gmail.com. Version 1 takes ASCII addresses only
// (SES does not deliver to non-ASCII local parts; a Unicode domain can be typed in its
// xn-- form), so Unicode normalisation (NFC) and IDNA have nothing to do.
func normaliseEmail(typed string) (string, bool) {
	if !validEmail(typed) {
		return "", false
	}
	local, domain, _ := strings.Cut(strings.ToLower(typed), "@")
	if domain == "gmail.com" || domain == "googlemail.com" {
		local, _, _ = strings.Cut(local, "+")
		local, domain = strings.ReplaceAll(local, ".", ""), "gmail.com"
		if local == "" {
			return "", false
		}
	}
	return local + "@" + domain, true
}

// atext is what RFC 5322 allows in an unquoted local part, besides dots.
const atext = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789!#$%&'*+/=?^_`{|}~-"

// validEmail accepts plain ASCII addresses: a dot-atom local part (no quoting, no comments)
// and a host name with at least two labels (no IP literals).
func validEmail(s string) bool {
	if len(s) > 254 {
		return false
	}
	local, domain, ok := strings.Cut(s, "@")
	if !ok || local == "" || len(local) > 64 || strings.Contains(domain, "@") {
		return false
	}
	for _, part := range strings.Split(local, ".") {
		if part == "" || strings.Trim(part, atext) != "" {
			return false
		}
	}
	labels := strings.Split(domain, ".")
	if len(domain) > 253 || len(labels) < 2 {
		return false
	}
	for _, l := range labels {
		if l == "" || len(l) > 63 || l[0] == '-' || l[len(l)-1] == '-' ||
			strings.Trim(l, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-") != "" {
			return false
		}
	}
	return strings.Trim(labels[len(labels)-1], "0123456789") != "" // not an IP address
}

// disposableList is a CC0 list of throwaway-mail domains (see the file's header). It is a
// speed bump, not a wall.
//
//go:embed disposable_domains.txt
var disposableList string

var disposableSet = sync.OnceValue(func() map[string]bool {
	set := map[string]bool{}
	for _, l := range strings.Split(disposableList, "\n") {
		if l = strings.TrimSpace(l); l != "" && !strings.HasPrefix(l, "#") {
			set[strings.ToLower(l)] = true
		}
	}
	return set
})

// isDisposable reports whether domain or one of its parents is on the list.
func isDisposable(domain string) bool {
	set := disposableSet()
	for d := domain; d != ""; {
		if set[d] {
			return true
		}
		_, d, _ = strings.Cut(d, ".")
	}
	return false
}

// clientIP is the address limits count against: the connecting address, or, when that is
// Caddy (loopback or a private network), the rightmost X-Forwarded-For entry, which Caddy
// appended. IPv6 clients count per /64.
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	addr, err := netip.ParseAddr(host)
	if err != nil {
		return r.RemoteAddr
	}
	addr = addr.Unmap()
	if addr.IsLoopback() || addr.IsPrivate() {
		if xff := r.Header.Values("X-Forwarded-For"); len(xff) > 0 {
			hops := strings.Split(xff[len(xff)-1], ",")
			if a, err := netip.ParseAddr(strings.TrimSpace(hops[len(hops)-1])); err == nil {
				addr = a.Unmap()
			}
		}
	}
	if addr.Is6() {
		p, _ := addr.WithZone("").Prefix(64)
		return p.String()
	}
	return addr.String()
}

// ---------------------------------------------------------------- HTTP

func (g *Gateway) emailRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /account/email", g.handleEmailPage)
	mux.HandleFunc("POST /account/email", smallForm(g.handleEmailStart))
	mux.HandleFunc("POST /account/email/verify", smallForm(g.handleEmailVerify))
}

// handleEmailStart asks for a code. The answer is the same whatever happens next.
func (g *Gateway) handleEmailStart(w http.ResponseWriter, r *http.Request) {
	if !g.sameOrigin(r) {
		http.Redirect(w, r, g.acfg.PublicURL+"/account", http.StatusSeeOther)
		return
	}
	typed := strings.TrimSpace(r.FormValue("email"))
	norm, ok := normaliseEmail(typed)
	if !ok { // the form, not the address's fate: says nothing about any account
		g.accountPage(w, http.StatusBadRequest, pageData{Next: r.FormValue("next") == "chat",
			Error: "Please enter a plain email address (for an international domain, its xn-- form)."})
		return
	}
	next := ""
	if r.FormValue("next") == "chat" {
		next = "chat"
	}
	// A browser that asks again keeps its pending id: a sent code replaces its attempt, and a
	// refused request (e.g. within the minute) leaves the earlier code working. The value
	// does not depend on the outcome, so the answer stays the same.
	var pending string
	if c, err := r.Cookie(g.cookieName(pendingCookie)); err == nil && len(c.Value) == 22 &&
		strings.Trim(c.Value, "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_") == "" {
		pending = c.Value
	} else {
		b := make([]byte, 16)
		rand.Read(b)
		pending = base64.RawURLEncoding.EncodeToString(b)
	}
	g.email.start(typed, norm, clientIP(r), next, pending)
	g.setCookie(w, pendingCookie, pending, "/account/email", emailCodeTTL)
	http.Redirect(w, r, g.acfg.PublicURL+"/account/email", http.StatusSeeOther)
}

// handleEmailPage shows the code form; with "#t=<token>" (the link in the mail) its script
// offers a Continue button. GET changes nothing, so a mail scanner opening the link uses up
// nothing.
func (g *Gateway) handleEmailPage(w http.ResponseWriter, r *http.Request) {
	g.emailPage(w, http.StatusOK, "")
}

// handleEmailVerify takes a code or link token, only together with the pending cookie.
func (g *Gateway) handleEmailVerify(w http.ResponseWriter, r *http.Request) {
	if !g.sameOrigin(r) {
		http.Redirect(w, r, g.acfg.PublicURL+"/account", http.StatusSeeOther)
		return
	}
	c, err := r.Cookie(g.cookieName(pendingCookie))
	if err != nil || c.Value == "" {
		g.emailPage(w, http.StatusBadRequest, "Open the link in the browser where you asked for the code, or type the code there. A code works for 10 minutes.")
		return
	}
	macs, next, ok := g.email.verify(c.Value, strings.TrimSpace(r.FormValue("code")), r.FormValue("token"))
	if !ok { // one message for every failure, so it tells nothing about the address
		g.emailPage(w, http.StatusBadRequest, "That did not work: the code is wrong, used or expired. Check the latest email or ask for a new code. After many wrong codes, email sign-in for the address pauses for an hour.")
		return
	}
	g.setCookie(w, pendingCookie, "", "/account/email", -time.Second)
	a, err := g.accounts.SignInEmail(macs, time.Now())
	g.signedIn(w, r, a, err, next)
}

// emailScript reads the link token from the fragment (which never reaches a server), offers
// it in a form and takes it out of the address bar. It is the only script; the page's CSP
// allows it by hash.
const emailScript = `var m=/^#t=([A-Za-z0-9_-]{43})$/.exec(location.hash);if(m){document.getElementById("token").value=m[1];document.getElementById("link").hidden=false;history.replaceState(null,"",location.pathname)}`

var emailScriptCSP = func() string {
	sum := sha256.Sum256([]byte(emailScript))
	return pageCSP + "; script-src 'sha256-" +
		base64.StdEncoding.EncodeToString(sum[:]) + "'; form-action 'self'; frame-ancestors 'none'"
}()

func (g *Gateway) emailPage(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Security-Policy", emailScriptCSP)
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.WriteHeader(status)
	if err := emailTmpl.Execute(w, struct{ Error, ChatURL string }{msg, g.acfg.ChatURL}); err != nil {
		g.log.Error("email page", "err", err)
	}
}

var emailTmpl = newPage("email", nil, `{{template "head" "Sign in with email"}}{{template "top" .}}
<p class="back"><a href="/account">← Sign in</a></p>
{{if .Error}}<div class="card err" role="alert">{{.Error}}</div>{{end}}
<h1>Check your email</h1>
<div class="card" id="link" hidden><p>Continue signing in with the link from your email.</p>
<form method="post" action="/account/email/verify"><input type="hidden" name="token" id="token"><button class="btn">Continue</button></form></div>
<div class="card"><p>If the address can receive mail, a sign-in code is on its way. It works once, for 10 minutes.</p>
<form class="form" method="post" action="/account/email/verify"><div class="field"><label for="code">Code</label>
<div class="row"><input id="code" name="code" inputmode="numeric" autocomplete="one-time-code" pattern="[0-9]{6}" maxlength="6" required size="8">
<button class="btn">Sign in</button></div></div></form>
<p class="mut">Use the code or the link in this browser. No mail after a few minutes? Check your spam folder, or
<a href="/account">ask again</a>.</p></div>
<script>`+emailScript+`</script>
{{template "bottom" .}}`)
