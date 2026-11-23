// Contact address: a user may ask to be emailed about important changes and
// outages. It is opt-in: the box on the sign-in page starts unticked, and its state travels
// with that one sign-in (the OAuth flow, or the pending email sign-in), never in a cookie of its
// own. Only then does the gateway ask the provider for an address (GitHub: scope user:email and
// the primary verified address; Google: scope email and only a verified one) or keep the address
// typed for email sign-in. An account keeps the address, when consent was given and where the
// address came from: nothing else. Turning it off on the account page deletes it at once, and so
// does a ban. The address is never shown publicly, logged, written to the usage log or sent to
// nodes; the operator lists it with `aisathome-gateway accounts contacts` and writes by hand.
//
// The operator's notice (Notice) is a short plain-text banner on the account pages and, through
// the chat's own banner list, in the chat (handleNoticeChat).
//
// SPDX-License-Identifier: Apache-2.0
package gateway

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// Where a contact address came from (Account.ContactSource).
const (
	ContactGoogle = "google"
	ContactGitHub = "github"
	ContactEmail  = "email"
)

// contactNone is the internal "next" of a sign-in that asked for an address and got none: it
// lands on the account page, which says so.
const contactNone = "contact-none"

func (a *Account) clearContact() { a.ContactEmail, a.ContactConsentAt, a.ContactSource = "", nil, "" }

// SetContact stores addr as the account's contact address with the user's consent, now. A
// banned account gets none.
func (s *AccountStore) SetContact(id, addr, source string, now time.Time) error {
	if !validEmail(addr) {
		return errors.New("not a plain email address")
	}
	if source != ContactGoogle && source != ContactGitHub && source != ContactEmail {
		return errors.New("unknown contact source")
	}
	return s.Update(func(f *AccountsFile) error {
		a := f.find(id)
		if a == nil || a.Status == StatusBanned {
			return errors.New("no such account")
		}
		t := now.UTC()
		a.ContactEmail, a.ContactConsentAt, a.ContactSource = addr, &t, source
		return nil
	})
}

// ClearContact deletes the account's contact address (the user turned it off, or asked the
// operator to).
func (s *AccountStore) ClearContact(id string) error {
	return s.Update(func(f *AccountsFile) error {
		a := f.find(id)
		if a == nil {
			return errors.New("no such account")
		}
		a.clearContact()
		return nil
	})
}

// maskEmail shows enough of an address for its owner to recognise it: "al•••@example.com".
// Addresses are ASCII (validEmail).
func maskEmail(addr string) string {
	local, domain, ok := strings.Cut(addr, "@")
	if !ok || local == "" {
		return ""
	}
	keep := 1
	if len(local) > 4 {
		keep = 2
	}
	return local[:keep] + "•••@" + domain
}

// contactWay is how an account can turn the address on from its account page: "github" or
// "google" (sign in there again with the scope), "email" (type the sign-in address, checked
// against its hash), or "" (not available, e.g. a provider that is no longer configured).
func (g *Gateway) contactWay(id string) string {
	switch {
	case strings.HasPrefix(id, "gh:") && g.acfg.githubOn():
		return ContactGitHub
	case strings.HasPrefix(id, "google:") && g.acfg.googleOn():
		return ContactGoogle
	case strings.HasPrefix(id, "em:") && g.email != nil:
		return ContactEmail
	}
	return ""
}

// sourceName names a contact source in the page's text.
func sourceName(source string) string {
	return map[string]string{ContactGitHub: "GitHub", ContactGoogle: "Google", ContactEmail: "email sign-in"}[source]
}

// handleContactConnect turns the address on for a GitHub or Google account: the provider's
// flow again, with the email scope, bound to the signed-in account.
func (g *Gateway) handleContactConnect(w http.ResponseWriter, r *http.Request) {
	id, ok := g.session(r)
	if !ok || !g.sameOrigin(r) {
		http.Redirect(w, r, g.acfg.PublicURL+"/account", http.StatusSeeOther)
		return
	}
	switch way := g.contactWay(id); way {
	case ContactGitHub, ContactGoogle:
		g.startLogin(w, r, oauthFlow{provider: way, contact: "1", connect: "1"})
	default:
		http.Redirect(w, r, g.acfg.PublicURL+"/account#contact", http.StatusSeeOther)
	}
}

// handleContactEmail turns the address on for an email account: the typed address must be the
// one it signs in with (its keyed hash matches), so no mail round trip is needed.
func (g *Gateway) handleContactEmail(w http.ResponseWriter, r *http.Request) {
	id, ok := g.session(r)
	if !ok || !g.sameOrigin(r) || g.contactWay(id) != ContactEmail {
		http.Redirect(w, r, g.acfg.PublicURL+"/account#contact", http.StatusSeeOther)
		return
	}
	typed := strings.TrimSpace(r.FormValue("email"))
	a, _, found := g.accounts.Get(id)
	norm, valid := normaliseEmail(typed)
	if !found || !valid || !emailMatch(a.EmailMACs, g.email.macs(norm)) {
		d := g.accountData(id)
		d.Error = "That is not the address you sign in with. Type the address you use to sign in."
		g.accountPage(w, r, http.StatusBadRequest, d)
		return
	}
	if err := g.accounts.SetContact(id, typed, ContactEmail, time.Now()); err != nil {
		g.log.Error("contact address not stored", "err", err) // errors never carry the address
	}
	http.Redirect(w, r, g.acfg.PublicURL+"/account#contact", http.StatusSeeOther)
}

// handleContactOff deletes the address at once.
func (g *Gateway) handleContactOff(w http.ResponseWriter, r *http.Request) {
	if id, ok := g.session(r); ok && g.sameOrigin(r) {
		if err := g.accounts.ClearContact(id); err != nil {
			d := g.accountData(id)
			d.Error = "Something went wrong. Please try again."
			g.log.Error("contact address not deleted", "err", err)
			g.accountPage(w, r, http.StatusInternalServerError, d)
			return
		}
	}
	http.Redirect(w, r, g.acfg.PublicURL+"/account#contact", http.StatusSeeOther)
}

// storeContact keeps a provider's verified address for id after a sign-in that asked for it;
// it reports whether one was kept.
func (g *Gateway) storeContact(id, addr, source string) bool {
	if addr == "" {
		return false
	}
	if err := g.accounts.SetContact(id, addr, source, time.Now()); err != nil {
		g.log.Warn("contact address not stored", "err", err)
		return false
	}
	return true
}

// ---------------------------------------------------------------- notice

// maxNotice is the longest notice, in characters.
const maxNotice = 300

// Notice is the operator's message to every user, with when it was set.
type Notice struct {
	Text string    `json:"text"`
	Set  time.Time `json:"set"`
}

// id changes with the text and the time it was set, so a new notice shows again to someone
// who dismissed the last one.
func (n Notice) id() string {
	sum := sha256.Sum256([]byte(n.Set.UTC().Format(time.RFC3339Nano) + "|" + n.Text))
	return hex.EncodeToString(sum[:6])
}

// CleanNotice checks a notice: plain text on one line, 1 to maxNotice characters, no control
// or format characters. Runs of spaces become one.
func CleanNotice(s string) (string, error) {
	if !utf8.ValidString(s) {
		return "", errors.New("the notice is not valid UTF-8")
	}
	for _, r := range s {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return "", errors.New("the notice must be one line of plain text (no control characters)")
		}
	}
	s = strings.Join(strings.Fields(s), " ")
	if n := utf8.RuneCountInString(s); n == 0 || n > maxNotice {
		return "", errors.New("a notice has 1 to 300 characters")
	}
	return s, nil
}

// SetNotice sets the notice (operator).
func (s *AccountStore) SetNotice(text string, now time.Time) error {
	text, err := CleanNotice(text)
	if err != nil {
		return err
	}
	return s.Update(func(f *AccountsFile) error {
		f.Settings.Notice = &Notice{Text: text, Set: now.UTC()}
		return nil
	})
}

// ClearNotice removes the notice (operator).
func (s *AccountStore) ClearNotice() error {
	return s.Update(func(f *AccountsFile) error { f.Settings.Notice = nil; return nil })
}

// notice is the current notice, if there is one that passes CleanNotice (the file may have been
// edited by hand).
func (s *AccountStore) notice() (Notice, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := s.f.Settings.Notice
	if n == nil {
		return Notice{}, false
	}
	text, err := CleanNotice(n.Text)
	if err != nil || text != n.Text {
		return Notice{}, false
	}
	return *n, true
}

// noticeView is what a page's banner needs.
type noticeView struct{ Text, ID string }

const noticeCookie = "aisathome_notice"

// noticeFor is the banner for this request: the notice, unless this browser dismissed it.
func (g *Gateway) noticeFor(r *http.Request) noticeView {
	n, ok := g.accounts.notice()
	if !ok {
		return noticeView{}
	}
	id := n.id()
	if c, err := r.Cookie(g.cookieName(noticeCookie)); err == nil && c.Value == id {
		return noticeView{}
	}
	return noticeView{Text: n.Text, ID: id}
}

// handleNoticeDismiss hides the current notice in this browser (a cookie with its id), so the
// pages need no script; a new notice has a new id and shows again.
func (g *Gateway) handleNoticeDismiss(w http.ResponseWriter, r *http.Request) {
	if g.sameOrigin(r) {
		if n, ok := g.accounts.notice(); ok && r.FormValue("id") == n.id() {
			g.setCookie(w, noticeCookie, n.id(), "/account", 365*24*time.Hour)
		}
	}
	http.Redirect(w, r, g.acfg.PublicURL+safeBack(r.FormValue("back")), http.StatusSeeOther)
}

// chatBanner is a banner as Open WebUI's GET /api/v1/configs/banners lists it (v0.11.4,
// BannerModel). The chat shows it above the conversation and remembers a dismissal by id in
// the browser's localStorage.
type chatBanner struct {
	ID          string `json:"id"`
	Type        string `json:"type"`
	Content     string `json:"content"`
	Dismissible bool   `json:"dismissible"`
	Timestamp   int64  `json:"timestamp"`
}

// handleNoticeChat answers the chat's request for its banners (the reverse proxy sends Open
// WebUI's GET /api/v1/configs/banners here): the notice, or none. The same for everyone.
func (g *Gateway) handleNoticeChat(w http.ResponseWriter, r *http.Request) {
	banners := []chatBanner{}
	if n, ok := g.accounts.notice(); ok {
		banners = append(banners, chatBanner{ID: "aisathome-" + n.id(), Type: "info", Content: markdownText(n.Text),
			Dismissible: true, Timestamp: n.Set.Unix()})
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	json.NewEncoder(w).Encode(banners)
}

// markdownText makes plain text show as itself where it is rendered as Markdown and then HTML
// (Open WebUI's banner: marked, then DOMPurify): HTML's special characters become entities and
// every other ASCII punctuation character is backslash-escaped, so nothing becomes markup,
// emphasis or a link.
func markdownText(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch r {
		case '&':
			b.WriteString("&amp;")
		case '<':
			b.WriteString("&lt;")
		case '>':
			b.WriteString("&gt;")
		case '"':
			b.WriteString("&quot;")
		case '\'':
			b.WriteString("&#39;")
		default:
			if r < utf8.RuneSelf && unicode.IsPunct(r) || strings.ContainsRune("$+<=>^`|~", r) {
				b.WriteByte('\\')
			}
			b.WriteRune(r)
		}
	}
	return b.String()
}
