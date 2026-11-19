// Chat rules for volunteers' nodes (item 12 tier 2; GDPR decisions 2026-11-19 D3 and D12; decision 0020
// addendum 2026-11-19 "chat from day one"). An untrusted node or pilot serves people's requests (chat,
// agents and API calls) only if its operator account has accepted the current operator terms
// (OperatorTermsVersion; the version and the time are recorded). Science (project keys) carries no
// personal data and is not affected; trusted nodes (the maintainer's own, by config) and the requester's
// own nodes are outside the rule. Where a node runs makes no difference (owner, 2026-11-19, revising D4).
//
// Accepted on the account page (a tick under the terms text), or, for a connector's user (a BOINC
// volunteer), by the connector when the user accepted there (handleConnectorTerms: the BOINC bridge, once
// BOINC records the user's ENROLL consent to terms_of_use.txt, which holds the operator terms verbatim,
// D8). A config node token counts as accepted when it is trusted, or when it names the version it
// accepted ("operator_terms").
//
// A draft of the terms (a version starting with "draft-") is accepted by nobody: while one is current, no
// untrusted node serves people's requests.
//
// No eligible node free: the request waits for one as any request does, and is never sent elsewhere.
// The rule is on unless the config says chat_rules_off (a private gateway of one's own nodes, or tests).
// Standalone nodes stay untrusted (decision 0007): personal data still goes only to trusted nodes.
//
// SPDX-License-Identifier: Apache-2.0
package gateway

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// OperatorTermsVersion is the version of the operator terms a node's account must have accepted. A new
// version needs a new acceptance: change it only together with the text below.
//
// A version starting with "draft-" never counts as accepted (termsFinal): a draft is no contract. It is a
// variable only so that tests can stand in other versions.
var OperatorTermsVersion = "2026-11-19"

// The operator terms as the account page shows them above the tick: the section "Requests your node
// answers" of the operator terms the owner approved on 2026-11-19, word for word.
const (
	operatorTermsIntro = "Users may not put personal data into their chats, and we send your node no account, name, email address or IP address. " +
		"Whatever a request contains, you agree to:"
	operatorTermsClose = "Your node answers other people's chats only after you have accepted these terms; until then it does science work only."
)

var operatorTermsPoints = []string{
	"Use requests only to compute the answer the gateway asks for, with the node software as we provide it.",
	"Not log, keep, copy, read or share prompts or answers, and not try to find out who sent them. Nothing from a request may outlive the request.",
	"Keep your machine and node token secure, and let no one else get at your node's requests.",
	"Not let anyone else run your node's work for you or pass requests on.",
	"Tell us at contact@aisathome.org without delay, within 24 hours if you can, if you think someone has seen or kept requests from your node, or if your token has leaked.",
	"Help us, where you can, when someone asks us about their data or when we deal with an incident.",
	"Delete anything from requests still on your machine when you stop running a node or when we ask. Our software keeps nothing on disk.",
	"Let us check your node (model checks, safety checks on answers, our records of what it served), and answer our questions about an incident.",
}

// OperatorTermsText is the operator terms as plain text, numbered.
var OperatorTermsText = func() string {
	var b strings.Builder
	b.WriteString(operatorTermsIntro + "\n")
	for i, p := range operatorTermsPoints {
		fmt.Fprintf(&b, "%d. %s\n", i+1, p)
	}
	b.WriteString(operatorTermsClose)
	return b.String()
}()

// UserRule is the rule for people who chat, shown under the age line on the sign-in page and on the
// account page (owner, 2026-11-19).
const UserRule = "Don't put personal data (yours or other people's) into chats: answers are computed on volunteers' machines. " +
	"If you need a GDPR-compliant or EU-hosted service for personal data, use an established provider."

// termsFinal reports whether the current operator terms are final: a draft is accepted by nobody.
func termsFinal() bool { return !strings.HasPrefix(OperatorTermsVersion, "draft-") }

// How an acceptance was recorded (Acceptance.Via).
const (
	ViaAccountPage = "account"
	viaConnector   = "connector:" // + the connector's id
)

// Acceptance records that an account accepted a text: which version, when, and how.
type Acceptance struct {
	Version string    `json:"version"`
	At      time.Time `json:"at"`
	Via     string    `json:"via,omitempty"`
}

func (a *Acceptance) current(version string) bool { return a != nil && a.Version == version }

// termsCurrent reports whether a counts as an acceptance of the operator terms now: of the current
// version, and that version final.
func termsCurrent(a *Acceptance) bool { return termsFinal() && a.current(OperatorTermsVersion) }

// ---------------------------------------------------------------- the accounts file

var errNoOperator = errors.New("no such account")

// AcceptOperatorTerms records that account id accepted the operator terms (version) at at.
func (s *AccountStore) AcceptOperatorTerms(id, version, via string, at time.Time) error {
	return s.Update(func(f *AccountsFile) error {
		a := f.find(id)
		if a == nil || (a.Status != StatusActive && a.Status != StatusUnclaimed) {
			return errNoOperator
		}
		a.OperatorTerms = &Acceptance{Version: version, At: at.UTC(), Via: via}
		return nil
	})
}

// WithdrawOperatorTerms removes account id's acceptance: its nodes serve science only from then on.
func (s *AccountStore) WithdrawOperatorTerms(id string) error {
	return s.Update(func(f *AccountsFile) error {
		a := f.find(id)
		if a == nil {
			return errNoOperator
		}
		t := time.Now().UTC()
		if a.OperatorTerms != nil && a.OperatorTerms.At.After(t) {
			// An acceptance stamped by a connector clock a little ahead: the withdrawal must still come
			// after it, so a retry of that same record never undoes it.
			t = a.OperatorTerms.At
		}
		if a.TermsWithdrawn != nil && a.TermsWithdrawn.After(t) {
			t = *a.TermsWithdrawn // withdrawals only move forward: a second one never reopens the first's window
		}
		a.OperatorTerms, a.TermsWithdrawn = nil, &t
		return nil
	})
}

// operatorTerms reports whether account id has accepted the current operator terms.
func (s *AccountStore) operatorTerms(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	a := s.f.find(id)
	return a != nil && termsCurrent(a.OperatorTerms)
}

// connectorTerms records that connector's user accepted the operator terms (version) at at, on the
// account that user maps to: the linked one, or a new unclaimed one (as its first pilot would make). A
// record older than the account's own acceptance or withdrawal (a connector resending an old consent)
// changes nothing: applied says whether it was recorded, terms whether the account has accepted the
// current terms afterwards.
func (s *AccountStore) connectorTerms(connector, user, version string, at, now time.Time) (unclaimed, applied, terms bool, err error) {
	err = s.Update(func(f *AccountsFile) error {
		a := f.linkOwner(connector, user)
		if a == nil {
			id := newUnclaimedID()
			for f.find(id) != nil {
				id = newUnclaimedID()
			}
			t := now.UTC()
			a = &Account{ID: id, Status: StatusUnclaimed, Created: t, Links: []AccountLink{{Connector: connector, User: user, Linked: t}}}
			f.Accounts = append(f.Accounts, a)
		}
		if !runsPilots(a.Status) {
			return errNoOperator
		}
		unclaimed = a.Status == StatusUnclaimed
		if (a.OperatorTerms == nil || !at.Before(a.OperatorTerms.At)) && (a.TermsWithdrawn == nil || at.After(*a.TermsWithdrawn)) {
			a.OperatorTerms, applied = &Acceptance{Version: version, At: at.UTC(), Via: viaConnector + connector}, true
		}
		terms = termsCurrent(a.OperatorTerms)
		return nil
	})
	return unclaimed, applied, terms, err
}

// ---------------------------------------------------------------- nodes

// nodeRules is what the chat rules know of a node, computed when it connects and on every sweep (its
// account may accept or withdraw meanwhile). Kept on nodeConn, guarded by g.mu.
type nodeRules struct {
	terms bool // its operator accepted the current operator terms (or it is trusted)
}

// rulesFor works out a node's nodeRules: from its account (account nodes and pilots), or from its config
// token. It reads the accounts file under the store's own lock (g.mu may be held: g.mu, then the store).
func (g *Gateway) rulesFor(trusted bool, account string, tok NodeToken) nodeRules {
	var r nodeRules
	switch {
	case trusted:
		r.terms = true
	case account != "" && g.accounts != nil:
		r.terms = g.accounts.operatorTerms(account)
	case account == "":
		r.terms = termsFinal() && tok.OperatorTerms == OperatorTermsVersion
	}
	return r
}

// chatRulesOn reports whether the rules apply (they do unless the config turns them off).
func (g *Gateway) chatRulesOn() bool { return !g.cfg.ChatRulesOff }

// gatedBy reports whether the chat rules keep rt from n. Only for people's requests on untrusted nodes
// that are not the requester's own (route.allows checks those first). Caller holds g.mu.
func (rt *route) gatedBy(n *nodeConn) bool {
	return rt.rules && rt.class != ClassScience && !n.rules.terms
}

// chatView says, for an account's node row, whether the node serves people's requests and why not.
func (g *Gateway) chatView(account, id string) string {
	if !g.chatRulesOn() {
		return ""
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	n := g.node[id]
	if n == nil || n.account != account || n.trusted {
		return ""
	}
	switch {
	case n.rules.terms:
		return "chat and science"
	case !termsFinal():
		return "science only: the operator terms are not final yet"
	}
	return "science only: accept the operator terms below to serve chat"
}

// ---------------------------------------------------------------- HTTP: the connector

// handleConnectorTerms serves POST /v1/connectors/{id}/users/{user}/operator-terms: the connector says
// its user accepted the operator terms, {"version", "accepted_at"}. version is the operator terms version
// the user accepted (OperatorTermsVersion for the current one; another is recorded but lets no pilot
// serve chat), accepted_at when (RFC 3339; a time ahead of the gateway's clock counts as now). The user's
// account is the linked one or a new unclaimed one. A record older than the account's own acceptance or
// withdrawal changes nothing ("recorded": false). "current" says whether the account's pilots may serve
// chat afterwards. Repeating a call changes nothing.
func (g *Gateway) handleConnectorTerms(w http.ResponseWriter, r *http.Request) {
	c := g.connectorFor(w, r)
	if c == nil {
		return
	}
	user := r.PathValue("user")
	var body struct {
		Version    string    `json:"version"`
		AcceptedAt time.Time `json:"accepted_at"`
		Country    string    `json:"country"` // accepted and ignored: connectors built for the country rule send it
	}
	if !decodeTaskBody(w, r, maxConnectorBody, &body) {
		return
	}
	now := g.pilotNow()
	switch {
	case !extIDRe.MatchString(user):
		apiError(w, http.StatusBadRequest, "user: the connector's own id, 1-64 letters, digits, '.', '_', ':' or '-'")
		return
	case !validVersion(body.Version):
		apiError(w, http.StatusBadRequest, "version: 1-40 letters, digits, '.', '_' or '-'")
		return
	case body.AcceptedAt.IsZero() || body.AcceptedAt.After(now.Add(connectorClockSkew)):
		apiError(w, http.StatusBadRequest, "accepted_at: an RFC 3339 time, not in the future (a minute of clock skew is allowed)")
		return
	}
	// Stored as sent, never moved to now: a retry of the same record then compares the same time with a
	// withdrawal (which comes after any acceptance it withdraws, WithdrawOperatorTerms), so it cannot undo it.
	at := body.AcceptedAt
	g.linkMu.Lock() // like a bind, it may make the user's unclaimed account: not while a claim moves it
	unclaimed, applied, terms, err := g.accounts.connectorTerms(c.ID, user, body.Version, at, now)
	g.linkMu.Unlock()
	if errors.Is(err, errNoOperator) {
		apiError(w, http.StatusConflict, "that user's account is closed")
		return
	} else if err != nil {
		g.log.Error("operator terms: accounts file not written", "connector", c.ID, "err", err)
		apiError(w, http.StatusServiceUnavailable, "accounts unavailable, try again")
		return
	}
	g.sweepNodes() // its connected pilots may serve chat now
	kind := "linked"
	if unclaimed {
		kind = "unclaimed"
	}
	writeJSON(w, http.StatusOK, map[string]any{"recorded": applied, "current": terms,
		"current_version": OperatorTermsVersion, "account_kind": kind})
}

// connectorClockSkew: how far ahead of the gateway's clock a connector's accepted_at may be.
const connectorClockSkew = time.Minute

func validVersion(v string) bool {
	if v == "" || len(v) > 40 {
		return false
	}
	return strings.Trim(v, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789._-") == ""
}

// ---------------------------------------------------------------- HTTP: the account page

// handleOperatorTerms takes the account page's operator form: accept (the tick) or withdraw.
func (g *Gateway) handleOperatorTerms(w http.ResponseWriter, r *http.Request) {
	id, ok := g.session(r)
	if !ok || !g.sameOrigin(r) {
		http.Redirect(w, r, g.acfg.PublicURL+"/account", http.StatusSeeOther)
		return
	}
	msg := "" // why the form was refused, for the page
	var err error
	switch r.FormValue("action") {
	case "accept":
		switch {
		case r.FormValue("accept") != "1" || r.FormValue("version") != OperatorTermsVersion:
			msg = "Tick the box to accept the operator terms. If the terms changed while the page was open, read them again."
		case !g.accounts.active(id):
			msg = "Only an active account can accept the operator terms."
		default:
			err = g.accounts.AcceptOperatorTerms(id, OperatorTermsVersion, ViaAccountPage, time.Now())
		}
	case "withdraw":
		err = g.accounts.WithdrawOperatorTerms(id)
	}
	if err != nil {
		g.log.Error("operator terms not recorded", "err", err)
		msg = "Something went wrong. Please try again."
	}
	if msg != "" {
		d := g.accountData(id)
		d.Error = msg
		g.accountPage(w, r, http.StatusBadRequest, d)
		return
	}
	g.sweepNodes() // its nodes serve (or stop serving) chat now
	http.Redirect(w, r, g.acfg.PublicURL+"/account#operator", http.StatusSeeOther)
}

// chatRulesData fills the account page's parts for the chat rules.
func (g *Gateway) chatRulesData(d *pageData) {
	a := d.Account
	d.RulesOn = g.chatRulesOn()
	d.TermsIntro, d.TermsPoints, d.TermsClose = operatorTermsIntro, operatorTermsPoints, operatorTermsClose
	d.TermsVersion, d.TermsDraft = OperatorTermsVersion, !termsFinal()
	d.TermsAccepted = a.OperatorTerms.current(OperatorTermsVersion)
	d.TermsOutdated = a.OperatorTerms != nil && !d.TermsAccepted
	if d.TermsAccepted {
		d.TermsSince = a.OperatorTerms.At
	}
}

// chatRulesPage is the account page's data for the chat rules (part of pageData).
type chatRulesPage struct {
	RulesOn                      bool
	UserRule                     string // the rule for people who chat (UserRule), on the sign-in and account pages
	TermsIntro, TermsClose       string
	TermsPoints                  []string
	TermsVersion                 string
	TermsDraft                   bool // the terms are a draft: no acceptance counts yet
	TermsAccepted, TermsOutdated bool
	TermsSince                   time.Time
}

// accountChatRulesTmpl holds the account page's card for the chat rules (part of accountTmpl).
const accountChatRulesTmpl = `
{{define "operator"}}{{if .RulesOn}}<div class="card" id="operator"><div class="card-head"><h2>Operator terms</h2>{{if .TermsAccepted}}<span class="badge on">Accepted</span>{{else}}<span class="badge off">Not accepted</span>{{end}}</div>
<p>Your nodes{{if .Account.Links}} and pilots{{end}} serve people's chats and API requests only once you accept the operator terms; until then they serve science only.</p>
{{if .TermsDraft}}<p role="status"><strong>These terms are a draft.</strong> Your nodes serve science only until the final terms are published and you accept them.</p>{{end}}
{{if .TermsAccepted}}<p>You accepted version <code>{{.TermsVersion}}</code> on {{day .TermsSince}}.</p>
<form method="post" action="/account/operator"><input type="hidden" name="action" value="withdraw"><button class="btn ghost">Withdraw: science only</button></form>
{{else}}{{if .TermsOutdated}}<p role="status"><strong>The operator terms changed.</strong> Accept the new version to keep serving chat.</p>{{end}}
<div class="terms" id="operator-terms" tabindex="0"><p>{{.TermsIntro}}</p>
<ol>{{range .TermsPoints}}<li>{{.}}</li>{{end}}</ol>
<p>{{.TermsClose}}</p></div>
<form class="form" method="post" action="/account/operator"><input type="hidden" name="action" value="accept"><input type="hidden" name="version" value="{{.TermsVersion}}">
<label class="check"><input type="checkbox" name="accept" value="1" required><span>I have read and accept the operator terms (version <code>{{.TermsVersion}}</code>), and I am 18 or older.</span></label>
<div class="actions"><button class="btn secondary">Accept</button></div></form>{{end}}</div>{{end}}{{end}}`
