// Email sign-in (research addendum 2026-10-10): one-time code and link, bound to the browser
// by a pending cookie; the same answer for every address; limits; only a keyed hash stored.
// A fake SMTP server stands in for SES and requires STARTTLS and AUTH like SES does.
//
// SPDX-License-Identifier: Apache-2.0
package e2e

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"mime"
	"mime/multipart"
	"net"
	"net/http"
	"net/mail"
	"net/textproto"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aisathome/aisathome/gateway"
)

// ---------------------------------------------------------------- fake SMTP

type sentMail struct {
	from, to      string
	subject       string
	text, html    string
	code, token   string
	headerTo, raw string
}

type fakeSMTP struct {
	ln    net.Listener
	pool  *x509.CertPool
	cert  tls.Certificate
	noTLS bool // offer no STARTTLS

	mu  sync.Mutex
	got []sentMail
}

func newFakeSMTP(t *testing.T, noTLS bool) *fakeSMTP {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "fake smtp"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}, KeyUsage: x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	leaf, _ := x509.ParseCertificate(der)
	pool := x509.NewCertPool()
	pool.AddCert(leaf)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeSMTP{ln: ln, pool: pool, noTLS: noTLS, cert: tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go f.serve(t, c)
		}
	}()
	t.Cleanup(func() { ln.Close() })
	return f
}

func (f *fakeSMTP) port() int { return f.ln.Addr().(*net.TCPAddr).Port }

func (f *fakeSMTP) serve(t *testing.T, c net.Conn) {
	defer func() { c.Close() }()
	tp := textproto.NewConn(c)
	tp.PrintfLine("220 fake ESMTP")
	tlsOn, authed := false, false
	var from, to string
	for {
		line, err := tp.ReadLine()
		if err != nil {
			return
		}
		verb, arg, _ := strings.Cut(line, " ")
		switch strings.ToUpper(verb) {
		case "EHLO", "HELO":
			ext := []string{"250-fake"}
			if !tlsOn && !f.noTLS {
				ext = append(ext, "250-STARTTLS")
			}
			if tlsOn {
				ext = append(ext, "250-AUTH PLAIN")
			}
			for _, e := range append(ext, "250 8BITMIME") {
				tp.PrintfLine("%s", e)
			}
		case "STARTTLS":
			if f.noTLS || tlsOn {
				tp.PrintfLine("502 no")
				continue
			}
			tp.PrintfLine("220 go ahead")
			tc := tls.Server(c, &tls.Config{Certificates: []tls.Certificate{f.cert}})
			if tc.Handshake() != nil {
				return
			}
			c, tp, tlsOn = tc, textproto.NewConn(tc), true
		case "AUTH":
			raw, _ := base64.StdEncoding.DecodeString(strings.TrimPrefix(arg, "PLAIN "))
			if !tlsOn || string(raw) != "\x00smtp-user\x00smtp-pass" {
				tp.PrintfLine("535 bad credentials")
				continue
			}
			authed = true
			tp.PrintfLine("235 ok")
		case "MAIL":
			if !authed {
				tp.PrintfLine("530 authenticate first")
				continue
			}
			from = angle(arg)
			tp.PrintfLine("250 ok")
		case "RCPT":
			to = angle(arg)
			tp.PrintfLine("250 ok")
		case "DATA":
			tp.PrintfLine("354 go")
			data, err := tp.ReadDotBytes()
			if err != nil {
				return
			}
			m := parseMail(t, string(data))
			m.from, m.to = from, to
			f.mu.Lock()
			f.got = append(f.got, m)
			f.mu.Unlock()
			tp.PrintfLine("250 queued")
		case "QUIT":
			tp.PrintfLine("221 bye")
			return
		default:
			tp.PrintfLine("502 unknown")
		}
	}
}

// angle returns the address in "FROM:<a> BODY=8BITMIME".
func angle(arg string) string {
	_, rest, _ := strings.Cut(arg, "<")
	a, _, _ := strings.Cut(rest, ">")
	return a
}

var (
	subjectRe = regexp.MustCompile(`^site\.test sign-in code: (\d{6})$`)
	linkRe    = regexp.MustCompile(`https://site\.test/account/email#t=([A-Za-z0-9_-]{43})`)
)

func parseMail(t *testing.T, raw string) sentMail {
	msg, err := mail.ReadMessage(strings.NewReader(raw))
	if err != nil {
		t.Errorf("mail: %v", err)
		return sentMail{}
	}
	m := sentMail{raw: raw, subject: msg.Header.Get("Subject"), headerTo: msg.Header.Get("To")}
	_, params, _ := mime.ParseMediaType(msg.Header.Get("Content-Type"))
	mr := multipart.NewReader(msg.Body, params["boundary"])
	for {
		p, err := mr.NextPart() // decodes quoted-printable
		if err != nil {
			break
		}
		b, _ := io.ReadAll(p)
		if strings.HasPrefix(p.Header.Get("Content-Type"), "text/html") {
			m.html = string(b)
		} else {
			m.text = string(b)
		}
	}
	if s := subjectRe.FindStringSubmatch(m.subject); s != nil {
		m.code = s[1]
	}
	if s := linkRe.FindStringSubmatch(m.text); s != nil {
		m.token = s[1]
	}
	return m
}

func (f *fakeSMTP) count() int { f.mu.Lock(); defer f.mu.Unlock(); return len(f.got) }

// wait returns the n-th mail (1-based), waiting for it to arrive.
func (f *fakeSMTP) wait(t *testing.T, n int) sentMail {
	t.Helper()
	for i := 0; i < 100; i++ {
		f.mu.Lock()
		if len(f.got) >= n {
			m := f.got[n-1]
			f.mu.Unlock()
			return m
		}
		f.mu.Unlock()
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("mail %d did not arrive (%d so far)", n, f.count())
	return sentMail{}
}

// ---------------------------------------------------------------- rig

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) Now() time.Time      { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *fakeClock) Add(d time.Duration) { c.mu.Lock(); c.t = c.t.Add(d); c.mu.Unlock() }

type emailRig struct {
	*acctRig
	smtp  *fakeSMTP
	clock *fakeClock
	ip    string // X-Forwarded-For of this test's browser
}

var emailKey1 = gateway.EmailKey{ID: "k1", Secret: strings.Repeat("1", 32)}

func newEmailRig(t *testing.T, maxActive int, mod func(*gateway.AccountsConfig)) *emailRig {
	return newEmailRigSMTP(t, maxActive, newFakeSMTP(t, false), &fakeClock{t: time.Now()}, mod)
}

func newEmailRigSMTP(t *testing.T, maxActive int, fs *fakeSMTP, clk *fakeClock, mod func(*gateway.AccountsConfig)) *emailRig {
	a := newAcctRig(t, maxActive, func(c *gateway.AccountsConfig) {
		c.Email = &gateway.EmailConfig{SMTPHost: "127.0.0.1", SMTPPort: fs.port(), SMTPUser: "smtp-user",
			SMTPPassword: "smtp-pass", From: "AIs@home <noreply@aisathome.org>",
			Keys: []gateway.EmailKey{emailKey1}, RootCAs: fs.pool, Now: clk.Now}
		if mod != nil {
			mod(c)
		}
	})
	return &emailRig{acctRig: a, smtp: fs, clock: clk, ip: "198.51.100.10"}
}

// request asks for a code for email and returns the pending cookie ("name=value").
func (e *emailRig) request(t *testing.T, email string) (string, *http.Response, string) {
	t.Helper()
	return e.requestFrom(t, email, "")
}

// requestFrom is request from a browser that holds the pending cookie pending.
func (e *emailRig) requestFrom(t *testing.T, email, pending string) (string, *http.Response, string) {
	t.Helper()
	req, _ := http.NewRequest("POST", e.gw.URL+"/account/email", strings.NewReader("email="+urlEscape(email)))
	if pending != "" {
		req.Header.Set("Cookie", pending)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	req.Header.Set("X-Forwarded-For", e.ip)
	resp, err := noRedirect.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return cookieValue(resp, "aisathome_pending"), resp, string(b)
}

func urlEscape(s string) string {
	return strings.NewReplacer("+", "%2B", "@", "%40", " ", "+").Replace(s)
}

// verify posts a code ("code=...") or token ("token=...") with the pending cookie (may be
// empty) and returns the session cookie, if any.
func (e *emailRig) verify(t *testing.T, pending, form string) (string, *http.Response, string) {
	t.Helper()
	req, _ := http.NewRequest("POST", e.gw.URL+"/account/email/verify", strings.NewReader(form))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	if pending != "" {
		req.Header.Set("Cookie", pending)
	}
	resp, err := noRedirect.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return cookieValue(resp, "aisathome_session"), resp, string(b)
}

// signInEmail runs the whole flow by code and returns the session; it moves the clock past
// the per-address minute first, and expects the mail to be number n.
func (e *emailRig) signInEmail(t *testing.T, email string, n int) (string, *http.Response) {
	t.Helper()
	e.clock.Add(61 * time.Second)
	pending, _, _ := e.request(t, email)
	m := e.smtp.wait(t, n)
	s, resp, _ := e.verify(t, pending, "code="+m.code)
	return s, resp
}

// accountOf returns the account id behind a session (via the chat's forward_auth).
func (e *emailRig) accountOf(t *testing.T, session string) string {
	t.Helper()
	r, _ := e.get(t, "/account/auth", session)
	id, ok := strings.CutSuffix(r.Header.Get("X-Auth-Email"), "@users.invalid")
	if r.StatusCode != http.StatusOK || !ok {
		t.Fatalf("forward_auth: %d %v", r.StatusCode, r.Header)
	}
	return strings.Replace(id, "-", ":", 1)
}

func wrongCode(code string) string {
	n, _ := strconv.Atoi(code)
	return fmt.Sprintf("%06d", (n+1)%1000000)
}

var emIDRe = regexp.MustCompile(`^em:[0-9a-f]{32}$`)

// ---------------------------------------------------------------- tests

func TestEmailSignInByCode(t *testing.T) {
	logs := &syncBuf{}
	logSink = logs
	defer func() { logSink = io.Discard }()
	e := newEmailRig(t, 100, nil)
	const addr = "Some.One+x@Example.org"

	pending, resp, _ := e.request(t, addr)
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "https://site.test/account/email" || pending == "" {
		t.Fatalf("request: %d %s %q", resp.StatusCode, resp.Header.Get("Location"), pending)
	}
	for _, c := range resp.Cookies() {
		if strings.HasPrefix(c.Name, "aisathome_pending_") && (!c.HttpOnly || !c.Secure || c.SameSite != http.SameSiteLaxMode ||
			c.MaxAge != 600 || c.Path != "/account/email" || len(c.Value) != 22) {
			t.Fatalf("pending cookie: %+v", c)
		}
	}
	m := e.smtp.wait(t, 1)
	// Sent to the address as typed, from the configured sender, with code, link and time.
	if m.to != addr || m.headerTo != "<"+addr+">" || m.from != "noreply@aisathome.org" || m.code == "" || m.token == "" {
		t.Fatalf("mail: to %q/%q from %q subject %q", m.to, m.headerTo, m.from, m.subject)
	}
	if !strings.Contains(m.raw, `From: "AIs@home" <noreply@aisathome.org>`) {
		t.Fatalf("From header: %s", m.raw[:300])
	}
	for _, want := range []string{m.code, "Only enter this code on site.test. If you didn't ask, ignore this email.",
		e.clock.Now().UTC().Format("2006-01-02 15:04 UTC")} {
		if !strings.Contains(m.text, want) || !strings.Contains(m.html, want) {
			t.Fatalf("mail lacks %q:\n%s\n%s", want, m.text, m.html)
		}
	}
	if !strings.Contains(m.html, `href="https://site.test/account/email#t=`+m.token+`"`) ||
		strings.Contains(m.html, "<img") || strings.Count(m.html, "http") != 1 {
		t.Fatalf("html part (one link, no images): %s", m.html)
	}
	// Another browser's cookie does not reach this attempt.
	other, _, _ := e.request(t, "someone-else@example.org")
	if s, r, _ := e.verify(t, other, "code="+m.code); s != "" || r.StatusCode != http.StatusBadRequest {
		t.Fatalf("code with another browser's cookie: %d", r.StatusCode)
	}

	session, resp, _ := e.verify(t, pending, "code="+m.code)
	if session == "" || resp.StatusCode != http.StatusFound || resp.Header.Get("Location") != "https://site.test/account" {
		t.Fatalf("verify: %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
	cleared := false
	for _, c := range resp.Cookies() {
		cleared = cleared || (strings.HasPrefix(c.Name, "aisathome_pending_") && c.MaxAge < 0)
	}
	if !cleared {
		t.Fatal("pending cookie not cleared")
	}
	id := e.accountOf(t, session)
	if !emIDRe.MatchString(id) {
		t.Fatalf("account id %q", id)
	}
	// The attempt is used up: neither the code nor the link works again.
	if s, _, _ := e.verify(t, pending, "code="+m.code); s != "" {
		t.Fatal("code worked twice")
	}
	if s, _, _ := e.verify(t, pending, "token="+m.token); s != "" {
		t.Fatal("link worked after the code")
	}
	// Signing in again (any spelling of the address's case) finds the same account.
	s2, _ := e.signInEmail(t, "some.one+x@example.org", 3)
	if e.accountOf(t, s2) != id {
		t.Fatal("second sign-in made another account")
	}
	// The file and the log hold no address, code or token; the file holds the keyed hash.
	raw, _ := os.ReadFile(e.file)
	var f gateway.AccountsFile
	json.Unmarshal(raw, &f)
	if len(f.Accounts) != 1 || len(f.Accounts[0].EmailMACs) != 1 || f.Accounts[0].EmailMACs[0].KeyID != "k1" ||
		len(f.Accounts[0].EmailMACs[0].MAC) != 64 {
		t.Fatalf("accounts file: %s", raw)
	}
	for _, leak := range []string{"some.one", "Some.One", "example.org", "Example.org", m.code, m.token,
		e.smtp.wait(t, 3).code} {
		if strings.Contains(string(raw), leak) || strings.Contains(logs.String(), leak) {
			t.Fatalf("accounts file or log holds %q", leak)
		}
	}
}

func TestEmailSignInByLink(t *testing.T) {
	e := newEmailRig(t, 100, nil)
	// The link's page changes nothing and allows its one script by hash.
	resp, page := e.get(t, "/account/email", "")
	script := regexp.MustCompile(`(?s)<script>(.*?)</script>`).FindStringSubmatch(page)
	if resp.StatusCode != 200 || script == nil || len(resp.Cookies()) != 0 {
		t.Fatalf("GET /account/email: %d cookies %v", resp.StatusCode, resp.Cookies())
	}
	sum := sha256.Sum256([]byte(script[1]))
	csp := resp.Header.Get("Content-Security-Policy")
	if !strings.Contains(csp, "; script-src 'sha256-"+base64.StdEncoding.EncodeToString(sum[:])+"'; ") {
		t.Fatalf("CSP %q does not pin the script", csp)
	}
	if r, _ := e.get(t, "/account", ""); strings.Contains(r.Header.Get("Content-Security-Policy"), "script-src") {
		t.Fatal("the account page allows scripts")
	}

	pending, _, _ := e.request(t, "link@example.org")
	m := e.smtp.wait(t, 1)
	// A mail scanner (no cookie) cannot use the link up ...
	if s, r, body := e.verify(t, "", "token="+m.token); s != "" || r.StatusCode != http.StatusBadRequest ||
		!strings.Contains(body, "browser where you asked") {
		t.Fatalf("token without the pending cookie: %d", r.StatusCode)
	}
	// ... nor can a browser with another pending sign-in.
	other, _, _ := e.request(t, "other@example.org")
	if s, _, _ := e.verify(t, other, "token="+m.token); s != "" {
		t.Fatal("token accepted with another browser's cookie")
	}
	// The browser that asked can.
	s, r, _ := e.verify(t, pending, "token="+m.token)
	if s == "" || r.StatusCode != http.StatusFound || !emIDRe.MatchString(e.accountOf(t, s)) {
		t.Fatalf("token with the pending cookie: %d", r.StatusCode)
	}
	if s, _, _ := e.verify(t, pending, "code="+m.code); s != "" {
		t.Fatal("the code worked after the link")
	}
}

// Every request for a code gets the same answer: new address, known address, throwaway
// domain, over the address limit, over the client limit.
func TestEmailSameAnswerForEveryAddress(t *testing.T) {
	e := newEmailRig(t, 100, nil)
	e.signInEmail(t, "known@example.org", 1)
	e.clock.Add(61 * time.Second)
	shape := func(resp *http.Response, body string) string {
		var c []string
		for _, k := range resp.Cookies() {
			c = append(c, fmt.Sprintf("%s|%d|%s|%s|%v|%v|%d", k.Name, len(k.Value), k.Path, k.Domain, k.HttpOnly, k.Secure, k.MaxAge))
		}
		return fmt.Sprintf("%d %s %v %q", resp.StatusCode, resp.Header.Get("Location"), c, body)
	}
	var shapes []string
	for _, addr := range []string{"new@example.org", "known@example.org", "x@mailinator.com", "y@sub.mailinator.com",
		"new@example.org" /* again within a minute */} {
		_, resp, body := e.request(t, addr)
		shapes = append(shapes, shape(resp, body))
	}
	for i := 1; i < 10; i++ { // the client limit: 10 per hour (2 used above, plus "known")
		e.request(t, fmt.Sprintf("many%d@example.org", i))
	}
	_, resp, body := e.request(t, "over-client-limit@example.org")
	shapes = append(shapes, shape(resp, body))
	for i, s := range shapes {
		if s != shapes[0] {
			t.Fatalf("answer %d differs:\n%s\n%s", i, s, shapes[0])
		}
	}
	// What went out: known (sign-in), new, known, and many1..many7 (10 per hour for this
	// client); then, from another client, a barrier mail. Nothing for the throwaway domains,
	// the repeat or the request over the limit.
	e.ip = "198.51.100.99"
	e.request(t, "barrier@example.org")
	last := e.smtp.wait(t, 11)
	if last.to != "barrier@example.org" {
		t.Fatalf("mail 11 went to %q", last.to)
	}
	e.smtp.mu.Lock()
	defer e.smtp.mu.Unlock()
	for _, m := range e.smtp.got {
		if strings.Contains(m.to, "mailinator") || m.to == "over-client-limit@example.org" || m.to == "many8@example.org" {
			t.Fatalf("mail sent to %q", m.to)
		}
	}
	if len(e.smtp.got) != 11 || e.smtp.got[1].to != "new@example.org" || e.smtp.got[3].to != "many1@example.org" {
		t.Fatalf("%d mails", len(e.smtp.got))
	}
}

func TestEmailFiveWrongCodesEndTheAttempt(t *testing.T) {
	e := newEmailRig(t, 100, nil)
	pending, _, _ := e.request(t, "guess@example.org")
	m := e.smtp.wait(t, 1)
	for i := 0; i < 5; i++ {
		if s, r, _ := e.verify(t, pending, "code="+wrongCode(m.code)); s != "" || r.StatusCode != http.StatusBadRequest {
			t.Fatalf("wrong code %d: %d", i, r.StatusCode)
		}
	}
	if s, _, _ := e.verify(t, pending, "code="+m.code); s != "" {
		t.Fatal("the right code worked after 5 wrong ones")
	}
	if s, _, _ := e.verify(t, pending, "token="+m.token); s != "" {
		t.Fatal("the link worked after 5 wrong codes")
	}
	// Four wrong codes leave the attempt alive.
	e.clock.Add(61 * time.Second)
	pending, _, _ = e.request(t, "guess@example.org")
	m = e.smtp.wait(t, 2)
	for i := 0; i < 4; i++ {
		e.verify(t, pending, "code="+wrongCode(m.code))
	}
	if s, _, _ := e.verify(t, pending, "code="+m.code); s == "" {
		t.Fatal("the right code failed after 4 wrong ones")
	}
}

// 20 wrong codes per address in 24 h lock email sign-in for the address for an hour; asking
// for a new code does not reset the count.
func TestEmailFailureLock(t *testing.T) {
	e := newEmailRig(t, 100, nil)
	const addr = "victim@example.org"
	ask := func(n int) (string, sentMail) {
		e.clock.Add(61 * time.Second)
		p, _, _ := e.request(t, addr)
		return p, e.smtp.wait(t, n)
	}
	for n := 1; n <= 3; n++ { // 15 wrong codes over three codes
		p, m := ask(n)
		for i := 0; i < 5; i++ {
			e.verify(t, p, "code="+wrongCode(m.code))
		}
	}
	p4, m4 := ask(4) // a new code: 4 more wrong (19)
	for i := 0; i < 4; i++ {
		e.verify(t, p4, "code="+wrongCode(m4.code))
	}
	p5, m5 := ask(5) // another new code: the 20th wrong one locks
	e.verify(t, p5, "code="+wrongCode(m5.code))
	if s, _, _ := e.verify(t, p5, "code="+m5.code); s != "" {
		t.Fatal("signed in after 20 wrong codes in a day")
	}
	if s, _, _ := e.verify(t, p4, "token="+m4.token); s != "" {
		t.Fatal("an older attempt's link worked during the lock")
	}
	// No mail while locked (the answer stays the same).
	e.clock.Add(30 * time.Minute)
	e.request(t, addr)
	e.ip = "198.51.100.98"
	e.request(t, "barrier@example.org")
	if m := e.smtp.wait(t, 6); m.to != "barrier@example.org" {
		t.Fatalf("mail sent during the lock: %q", m.to)
	}
	// An hour after the lock, a new code works.
	e.clock.Add(31 * time.Minute)
	if s, r := e.signInEmail(t, addr, 7); s == "" {
		t.Fatalf("sign-in after the lock: %d", r.StatusCode)
	}
}

func TestEmailCodeExpires(t *testing.T) {
	e := newEmailRig(t, 100, nil)
	pending, _, _ := e.request(t, "slow@example.org")
	m := e.smtp.wait(t, 1)
	e.clock.Add(10*time.Minute + time.Second)
	if s, _, _ := e.verify(t, pending, "code="+m.code); s != "" {
		t.Fatal("code worked after 10 minutes")
	}
	if s, _, _ := e.verify(t, pending, "token="+m.token); s != "" {
		t.Fatal("link worked after 10 minutes")
	}
	pending, _, _ = e.request(t, "quick@example.org")
	m = e.smtp.wait(t, 2)
	e.clock.Add(9*time.Minute + 50*time.Second)
	if s, _, _ := e.verify(t, pending, "code="+m.code); s == "" {
		t.Fatal("code failed within 10 minutes")
	}
}

// Asking again in the same browser: within the minute nothing is sent and the first code
// still works; later, the new code replaces the old one.
func TestEmailAskAgain(t *testing.T) {
	e := newEmailRig(t, 100, nil)
	p1, _, _ := e.request(t, "again@example.org")
	m1 := e.smtp.wait(t, 1)
	p2, _, _ := e.requestFrom(t, "again@example.org", p1)
	if p2 != p1 {
		t.Fatal("asking again changed the pending id")
	}
	if s, _, _ := e.verify(t, p1, "code="+m1.code); s == "" {
		t.Fatal("asking again within the minute broke the first code")
	}
	p3, _, _ := e.request(t, "twice@example.org")
	m3 := e.smtp.wait(t, 2)
	e.clock.Add(61 * time.Second)
	e.requestFrom(t, "twice@example.org", p3)
	m4 := e.smtp.wait(t, 3)
	if s, _, _ := e.verify(t, p3, "code="+m3.code); s != "" {
		t.Fatal("an older code worked after a new one was sent")
	}
	if s, _, _ := e.verify(t, p3, "code="+m4.code); s == "" {
		t.Fatal("the latest code failed")
	}
}

// Gmail ignores dots and +tags, so they name one account; elsewhere they do not.
func TestEmailGmailNormalisation(t *testing.T) {
	e := newEmailRig(t, 100, nil)
	s1, _ := e.signInEmail(t, "First.Last+news@gmail.com", 1)
	s2, _ := e.signInEmail(t, "firstlast@GoogleMail.com", 2)
	if m := e.smtp.wait(t, 2); m.to != "firstlast@GoogleMail.com" {
		t.Fatalf("mail went to %q, not the address as typed", m.to)
	}
	if a, b := e.accountOf(t, s1), e.accountOf(t, s2); a != b {
		t.Fatalf("gmail spellings made two accounts: %s %s", a, b)
	}
	s3, _ := e.signInEmail(t, "first.last@example.org", 3)
	s4, _ := e.signInEmail(t, "firstlast@example.org", 4)
	if e.accountOf(t, s3) == e.accountOf(t, s4) {
		t.Fatal("dots outside Gmail were dropped")
	}
}

// A new current key: an account found under the old key is rewritten under the new one, and
// then no longer needs the old key.
func TestEmailKeyRotation(t *testing.T) {
	file := filepath.Join(t.TempDir(), "accounts.json")
	fs, clk := newFakeSMTP(t, false), &fakeClock{t: time.Now()}
	k2 := gateway.EmailKey{ID: "k2", Secret: strings.Repeat("2", 32)}
	rig := func(keys ...gateway.EmailKey) *emailRig {
		return newEmailRigSMTP(t, 100, fs, clk, func(c *gateway.AccountsConfig) { c.File, c.Email.Keys = file, keys })
	}
	macs := func() []gateway.EmailMAC {
		raw, _ := os.ReadFile(file)
		var f gateway.AccountsFile
		json.Unmarshal(raw, &f)
		if len(f.Accounts) != 1 {
			t.Fatalf("%d accounts", len(f.Accounts))
		}
		return f.Accounts[0].EmailMACs
	}
	r1 := rig(emailKey1)
	s, _ := r1.signInEmail(t, "rotate@example.org", 1)
	id := r1.accountOf(t, s)
	if m := macs(); len(m) != 1 || m[0].KeyID != "k1" {
		t.Fatalf("macs: %+v", m)
	}
	r2 := rig(k2, emailKey1)
	if s, _ := r2.signInEmail(t, "rotate@example.org", 2); r2.accountOf(t, s) != id {
		t.Fatal("sign-in under the new key made another account")
	}
	if m := macs(); len(m) != 1 || m[0].KeyID != "k2" {
		t.Fatalf("not rewritten under the new key: %+v", m)
	}
	r3 := rig(k2) // the old key retired
	if s, _ := r3.signInEmail(t, "rotate@example.org", 3); r3.accountOf(t, s) != id {
		t.Fatal("the rewritten hash was not found under the new key alone")
	}
}

// The account's chat address (em-<hex>@users.invalid) shares one budget with its API keys.
func TestOneBudgetPerEmailAccount(t *testing.T) {
	e := newEmailRig(t, 100, func(c *gateway.AccountsConfig) { c.Limits.TokensPerDay = 40 })
	s, _ := e.signInEmail(t, "budget@example.org", 1)
	oneBudget(t, e.acctRig, s, strings.Replace(e.accountOf(t, s), ":", "-", 1)+"@users.invalid")
}

func TestEmailWaitlistAndClosedSignUp(t *testing.T) {
	e := newEmailRig(t, 1, nil)
	e.signIn(t, 1) // a GitHub account takes the one place
	s, r := e.signInEmail(t, "late@example.org", 1)
	if s == "" || r.StatusCode != http.StatusFound {
		t.Fatalf("email sign-in: %d", r.StatusCode)
	}
	if _, page := e.get(t, "/account", s); !strings.Contains(page, "waitlist") {
		t.Fatalf("email account should wait: %s", page)
	}
	if r, _ := e.get(t, "/account/auth", s); r.StatusCode == http.StatusOK {
		t.Fatal("a waiting email account reached the chat")
	}
	e.store.Update(func(f *gateway.AccountsFile) error { f.Settings.Closed = true; return nil })
	if s, r := e.signInEmail(t, "new@example.org", 2); s != "" || r.StatusCode != http.StatusForbidden {
		t.Fatalf("new email account with sign-up closed: %d", r.StatusCode)
	}
	if s, _ := e.signInEmail(t, "late@example.org", 3); s == "" {
		t.Fatal("closing sign-up locked out an existing email account")
	}
}

// The sign-in page offers email next to GitHub and says they are separate accounts.
func TestEmailOnSignInPage(t *testing.T) {
	e := newEmailRig(t, 100, nil)
	_, page := e.get(t, "/account?next=chat", "")
	for _, want := range []string{`action="/account/email"`, `name="next" value="chat"`, "Sign in with GitHub",
		"GitHub and email are separate accounts; use the same one each time."} {
		if !strings.Contains(page, want) {
			t.Fatalf("sign-in page lacks %q", want)
		}
	}
	// A malformed address is a form error, not a silent answer.
	if _, r, _ := e.request(t, "not an address"); r.StatusCode != http.StatusBadRequest {
		t.Fatalf("malformed address: %d", r.StatusCode)
	}
	// Cross-site posts send nothing.
	req, _ := http.NewRequest("POST", e.gw.URL+"/account/email", strings.NewReader("email=x%40example.org"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	if r, _ := noRedirect.Do(req); cookieValue(r, "aisathome_pending") != "" {
		t.Fatal("cross-site request started a sign-in")
	}
	// Google too: three ways, one note.
	g, _ := newGoogleRig(t, 100, func(c *gateway.AccountsConfig) {
		c.Email = &gateway.EmailConfig{SMTPHost: "127.0.0.1", SMTPPort: 1, SMTPUser: "u", SMTPPassword: "p",
			From: "noreply@aisathome.org", Keys: []gateway.EmailKey{emailKey1}}
	})
	if _, page := g.get(t, "/account", ""); !strings.Contains(page, "GitHub, Google and email are separate accounts; use the same one each time.") {
		t.Fatalf("three-way note missing: %s", page)
	}
}

// Without STARTTLS nothing is sent, and the log names no address.
func TestEmailRequiresSTARTTLS(t *testing.T) {
	logs := &syncBuf{}
	logSink = logs
	defer func() { logSink = io.Discard }()
	e := newEmailRigSMTP(t, 100, newFakeSMTP(t, true), &fakeClock{t: time.Now()}, nil)
	e.request(t, "plain@example.org")
	waitFor(t, func() bool { return strings.Contains(logs.String(), "sign-in email not sent") })
	if e.smtp.count() != 0 || !strings.Contains(logs.String(), "STARTTLS") || strings.Contains(logs.String(), "plain@") {
		t.Fatalf("mails %d, log: %s", e.smtp.count(), logs.String())
	}
}
