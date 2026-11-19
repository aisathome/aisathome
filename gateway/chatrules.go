// Chat rules for volunteers' nodes (item 12 tier 2; GDPR decisions 2026-11-19 D3, D4 option B, D5 and
// D12; decision 0020 addendum 2026-11-19 "chat from day one"). They decide which nodes may see people's
// requests (chat, agents and API calls); science (project keys) carries no personal data and is not
// affected. Trusted nodes (the maintainer's own, by config) and the requester's own nodes are outside
// both rules.
//
//   - Operator terms (D3). An untrusted node or pilot serves people's requests only if its operator
//     account has accepted the current operator terms (OperatorTermsVersion; the version and the time
//     are recorded). Accepted on the account page (a tick under the terms text), or, for a connector's
//     user (a BOINC volunteer), by the connector when the user accepted there (handleConnectorTerms:
//     the BOINC bridge, once BOINC records the user's ENROLL consent to terms_of_use.txt, which holds
//     the operator terms verbatim, D8). A config node token counts as accepted when it is trusted, or
//     when it names the version it accepted ("operator_terms").
//   - Country (D4, D5). A node's country is the one its connection address has in the GeoIP table
//     (geoip.go), and only if its operator states the same country (account page, connector, or a
//     config token's "country"); otherwise it is unknown. A request from a user who has not opted in to
//     transfers goes only to nodes in the EEA or a country with an adequacy decision
//     (transfer_countries.txt). With the opt-in (TransferConsent: unticked by default, set at sign-in
//     or on the account page; the account's API keys and chat follow it) it may go to any node that
//     passed the operator terms. Requests without an account (config keys, a front end's anonymous
//     users) have no opt-in.
//
// No eligible node free: the request waits for one as any request does, and is never sent elsewhere.
// The rules are on unless the config says chat_rules_off (a private gateway of one's own nodes, or
// tests). Standalone nodes stay untrusted (decision 0007): personal data still goes only to trusted
// nodes.
//
// SPDX-License-Identifier: Apache-2.0
package gateway

import (
	"bufio"
	_ "embed"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"
)

// OperatorTermsVersion is the version of the operator terms a node's account must have accepted. A new
// version needs a new acceptance: change it only together with OperatorTermsText.
//
// The text is a placeholder until the drafted operator terms (GDPR decisions D2, D3) are approved; the
// approved text comes with a new version id, so nobody counts as having accepted the placeholder.
const OperatorTermsVersion = "draft-2026-11-19"

// OperatorTermsText is shown above the tick on the account page.
const OperatorTermsText = `PLACEHOLDER: the operator terms are being drafted. ` +
	`By running a node or pilot that serves people's requests you process their prompts on our behalf, ` +
	`only to answer them: you do not read, keep, share or log them, you keep the machine secure, ` +
	`you tell us at once of a breach, you delete everything when you stop, and you allow our checks. ` +
	`You are 18 or older.`

// TransferConsentVersion is the version of the transfer opt-in text; a new version needs a new opt-in.
const TransferConsentVersion = "2026-11-19"

// TransferConsentText is the opt-in (D4 option B, Art. 49(1)(a) GDPR): explicit, specific and naming the
// risk. Shown next to the tick at sign-in and on the account page.
const TransferConsentText = `Also send my chats and API requests to volunteers' computers outside Europe. ` +
	`There the GDPR does not protect my prompts: those volunteers could read or keep them.`

// How an acceptance was recorded (Acceptance.Via).
const (
	ViaAccountPage = "account"
	ViaSignIn      = "sign-in"
	viaConnector   = "connector:" // + the connector's id
)

// Acceptance records that an account accepted a text: which version, when, and how.
type Acceptance struct {
	Version string    `json:"version"`
	At      time.Time `json:"at"`
	Via     string    `json:"via,omitempty"`
}

//go:embed transfer_countries.txt
var transferCountriesTxt string

// transferFree are the countries a request may go to without the user's opt-in, and countryNames their
// names, for the account page.
var transferFree, countryNames, countryCodes = func() (map[string]bool, map[string]string, []string) {
	set, names := map[string]bool{}, map[string]string{}
	var codes []string
	sc := bufio.NewScanner(strings.NewReader(transferCountriesTxt))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		code, name, _ := strings.Cut(line, " ")
		if len(code) != 2 || strings.ToUpper(code) != code {
			panic("transfer_countries.txt: bad line " + line)
		}
		set[code], names[code] = true, strings.TrimSpace(name)
		codes = append(codes, code)
	}
	sort.Slice(codes, func(i, j int) bool { return names[codes[i]] < names[codes[j]] })
	return set, names, codes
}()

// countryOther is the stated country "somewhere else": outside the EEA and the adequate countries.
const countryOther = "other"

// validStatedCountry reports whether c may be stored as an operator's stated country.
func validStatedCountry(c string) bool { return c == "" || c == countryOther || transferFree[c] }

func (a *Acceptance) current(version string) bool { return a != nil && a.Version == version }

// ---------------------------------------------------------------- the accounts file

var errNoOperator = errors.New("no such account")

// AcceptOperatorTerms records that account id accepted the operator terms (version) at at, and its
// stated country if country is not "" (one of transfer_countries.txt, or countryOther).
func (s *AccountStore) AcceptOperatorTerms(id, version, via, country string, at time.Time) error {
	if !validStatedCountry(country) {
		return errors.New("unknown country")
	}
	return s.Update(func(f *AccountsFile) error {
		a := f.find(id)
		if a == nil || (a.Status != StatusActive && a.Status != StatusUnclaimed) {
			return errNoOperator
		}
		a.OperatorTerms = &Acceptance{Version: version, At: at.UTC(), Via: via}
		if country != "" {
			a.OperatorCountry = country
		}
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
		a.OperatorTerms, a.TermsWithdrawn = nil, &t
		return nil
	})
}

// SetOperatorCountry sets account id's stated country (one of transfer_countries.txt, countryOther, or ""
// for none).
func (s *AccountStore) SetOperatorCountry(id, country string) error {
	if !validStatedCountry(country) {
		return errors.New("unknown country")
	}
	return s.Update(func(f *AccountsFile) error {
		a := f.find(id)
		if a == nil {
			return errNoOperator
		}
		a.OperatorCountry = country
		return nil
	})
}

// SetTransferConsent records (on) or withdraws account id's opt-in to volunteers outside Europe.
func (s *AccountStore) SetTransferConsent(id string, on bool, via string, now time.Time) error {
	return s.Update(func(f *AccountsFile) error {
		a := f.find(id)
		if a == nil || a.Status == StatusBanned {
			return errNoOperator
		}
		if on {
			a.TransferConsent = &Acceptance{Version: TransferConsentVersion, At: now.UTC(), Via: via}
		} else {
			a.TransferConsent = nil
		}
		return nil
	})
}

// operatorState is what the chat rules need of a node's account: whether it accepted the current
// operator terms, and the country it states.
func (s *AccountStore) operatorState(id string) (terms bool, country string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a := s.f.find(id)
	if a == nil {
		return false, ""
	}
	return a.OperatorTerms.current(OperatorTermsVersion), a.OperatorCountry
}

// transferOK reports whether account id has opted in (the current text) to volunteers outside Europe.
func (s *AccountStore) transferOK(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	a := s.f.find(id)
	return a != nil && a.Status == StatusActive && a.TransferConsent.current(TransferConsentVersion)
}

// connectorTerms records that connector's user accepted the operator terms (version) at at, on the
// account that user maps to: the linked one, or a new unclaimed one (as its first pilot would make).
func (s *AccountStore) connectorTerms(connector, user, version, country string, at, now time.Time) (unclaimed bool, err error) {
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
		if a.Status != StatusActive && a.Status != StatusUnclaimed {
			return errNoOperator
		}
		// A record older than the account's own acceptance or withdrawal (a connector resending an old
		// consent) changes nothing, its country included.
		if (a.OperatorTerms != nil && at.Before(a.OperatorTerms.At)) || (a.TermsWithdrawn != nil && !at.After(*a.TermsWithdrawn)) {
			unclaimed = a.Status == StatusUnclaimed
			return nil
		}
		a.OperatorTerms = &Acceptance{Version: version, At: at.UTC(), Via: viaConnector + connector}
		if country != "" {
			a.OperatorCountry = country
		}
		unclaimed = a.Status == StatusUnclaimed
		return nil
	})
	return unclaimed, err
}

// ---------------------------------------------------------------- nodes

// nodeRules is what the chat rules know of a node, computed when it connects and on every sweep (its
// account may accept or withdraw meanwhile). Kept on nodeConn, guarded by g.mu.
type nodeRules struct {
	terms    bool   // its operator accepted the current operator terms (or it is trusted)
	geo      string // its address's country in the GeoIP table, "" if unknown
	stated   string // the country its operator states, "" if none
	adequate bool   // geo is in the EEA or an adequate country and matches stated
}

// rulesFor works out a node's nodeRules: from its account (account nodes and pilots), or from its config
// token. It reads the accounts file under the store's own lock (g.mu may be held: g.mu, then the store).
func (g *Gateway) rulesFor(trusted bool, account string, tok NodeToken, ip string) nodeRules {
	r := nodeRules{geo: g.geo.country(ip)}
	switch {
	case account != "" && g.accounts != nil:
		r.terms, r.stated = g.accounts.operatorState(account)
	case account == "":
		r.terms, r.stated = tok.OperatorTerms == OperatorTermsVersion, strings.ToUpper(tok.Country)
	}
	r.terms = r.terms || trusted
	r.adequate = r.geo != "" && r.geo == r.stated && transferFree[r.geo]
	return r
}

// chatRulesOn reports whether the rules apply (they do unless the config turns them off).
func (g *Gateway) chatRulesOn() bool { return !g.cfg.ChatRulesOff }

// gatedBy says why rt may not use n under the chat rules ("" = it may). Only for people's requests on
// untrusted nodes that are not the requester's own (route.allows checks those first). Caller holds g.mu.
func (rt *route) gatedBy(n *nodeConn) string {
	switch {
	case !rt.rules || rt.class == ClassScience:
		return ""
	case !n.rules.terms:
		return "terms"
	case !rt.transfer && !n.rules.adequate:
		return "country"
	}
	return ""
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
	r := n.rules
	switch {
	case !r.terms:
		return "science only: accept the operator terms below to serve chat"
	case r.adequate:
		return fmt.Sprintf("chat and science (connects from %s)", countryLabel(r.geo))
	case r.geo == "":
		return "chat only from users who allow volunteers outside Europe (its address's country is unknown)"
	case r.stated == "":
		return fmt.Sprintf("chat only from users who allow volunteers outside Europe (connects from %s; state your country below)", countryLabel(r.geo))
	case r.geo != r.stated:
		return fmt.Sprintf("chat only from users who allow volunteers outside Europe (connects from %s, not the country you state)", countryLabel(r.geo))
	}
	return fmt.Sprintf("chat only from users who allow volunteers outside Europe (connects from %s)", countryLabel(r.geo))
}

// countryLabel names a country code for a page: its name if it is in transfer_countries.txt.
func countryLabel(code string) string {
	if n := countryNames[code]; n != "" {
		return n
	}
	return code
}

// ---------------------------------------------------------------- HTTP: the connector

// handleConnectorTerms serves POST /v1/connectors/{id}/users/{user}/operator-terms: the connector says
// its user accepted the operator terms, {"version", "accepted_at", "country"}. version is the operator
// terms version the user accepted (OperatorTermsVersion for the current one; another is recorded but
// does not let pilots serve chat), accepted_at when (RFC 3339, not in the future), country optional:
// the ISO 3166-1 alpha-2 code the user states (BOINC's country field), "other" or empty. The user's
// account is the linked one or a new unclaimed one. Repeating a call changes nothing.
func (g *Gateway) handleConnectorTerms(w http.ResponseWriter, r *http.Request) {
	c := g.connectorFor(w, r)
	if c == nil {
		return
	}
	user := r.PathValue("user")
	var body struct {
		Version    string    `json:"version"`
		AcceptedAt time.Time `json:"accepted_at"`
		Country    string    `json:"country"`
	}
	if !decodeTaskBody(w, r, maxConnectorBody, &body) {
		return
	}
	now := g.pilotNow()
	country := strings.ToUpper(body.Country)
	if country == strings.ToUpper(countryOther) {
		country = countryOther
	}
	switch {
	case !extIDRe.MatchString(user):
		apiError(w, http.StatusBadRequest, "user: the connector's own id, 1-64 letters, digits, '.', '_', ':' or '-'")
		return
	case !validVersion(body.Version):
		apiError(w, http.StatusBadRequest, "version: 1-40 letters, digits, '.', '_' or '-'")
		return
	case body.AcceptedAt.IsZero() || body.AcceptedAt.After(now.Add(5*time.Minute)):
		apiError(w, http.StatusBadRequest, "accepted_at: an RFC 3339 time, not in the future")
		return
	case len(country) == 2 && !transferFree[country]:
		country = countryOther // outside the EEA and the adequate countries: all the rules need to know
	case !validStatedCountry(country):
		apiError(w, http.StatusBadRequest, "country: an ISO 3166-1 alpha-2 code, \"other\" or empty")
		return
	}
	unclaimed, err := g.accounts.connectorTerms(c.ID, user, body.Version, country, body.AcceptedAt, now)
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
	writeJSON(w, http.StatusOK, map[string]any{"recorded": true, "current": body.Version == OperatorTermsVersion,
		"current_version": OperatorTermsVersion, "account_kind": kind})
}

func validVersion(v string) bool {
	if v == "" || len(v) > 40 {
		return false
	}
	return strings.Trim(v, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789._-") == ""
}

// ---------------------------------------------------------------- HTTP: the account page

// handleOperatorTerms takes the account page's operator form: accept (the tick, and a country), change
// the country, or withdraw.
func (g *Gateway) handleOperatorTerms(w http.ResponseWriter, r *http.Request) {
	id, ok := g.session(r)
	if !ok || !g.sameOrigin(r) {
		http.Redirect(w, r, g.acfg.PublicURL+"/account", http.StatusSeeOther)
		return
	}
	country := r.FormValue("country")
	msg := "" // why the form was refused, for the page
	var err error
	switch r.FormValue("action") {
	case "accept":
		switch {
		case r.FormValue("accept") != "1" || r.FormValue("version") != OperatorTermsVersion:
			msg = "Tick the box to accept the operator terms. If the terms changed while the page was open, read them again."
		case !validStatedCountry(country):
			msg = "Choose the country your nodes run in."
		case !g.accounts.active(id):
			msg = "Only an active account can accept the operator terms."
		default:
			err = g.accounts.AcceptOperatorTerms(id, OperatorTermsVersion, ViaAccountPage, country, time.Now())
		}
	case "country":
		if !validStatedCountry(country) {
			msg = "Choose the country your nodes run in."
		} else {
			err = g.accounts.SetOperatorCountry(id, country)
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

// handleTransferConsent takes the account page's opt-in form: on (with the tick) or off.
func (g *Gateway) handleTransferConsent(w http.ResponseWriter, r *http.Request) {
	id, ok := g.session(r)
	if !ok || !g.sameOrigin(r) {
		http.Redirect(w, r, g.acfg.PublicURL+"/account", http.StatusSeeOther)
		return
	}
	on := r.FormValue("action") == "on"
	if on && (r.FormValue("transfer") != "1" || r.FormValue("version") != TransferConsentVersion) {
		d := g.accountData(id)
		d.Error = "Tick the box to allow volunteers outside Europe."
		g.accountPage(w, r, http.StatusBadRequest, d)
		return
	}
	if err := g.accounts.SetTransferConsent(id, on, ViaAccountPage, time.Now()); err != nil {
		g.log.Error("transfer consent not recorded", "err", err)
		d := g.accountData(id)
		d.Error = "Your choice was not saved. Please try again."
		g.accountPage(w, r, http.StatusInternalServerError, d)
		return
	}
	http.Redirect(w, r, g.acfg.PublicURL+"/account#transfer", http.StatusSeeOther)
}

// storeTransfer records the opt-in ticked at sign-in. An unticked box at sign-in withdraws nothing.
func (g *Gateway) storeTransfer(id string) {
	if err := g.accounts.SetTransferConsent(id, true, ViaSignIn, time.Now()); err != nil {
		g.log.Error("transfer consent not recorded", "err", err)
	}
}

// chatRulesData fills the account page's parts for the chat rules.
func (g *Gateway) chatRulesData(d *pageData) {
	a := d.Account
	d.RulesOn = g.chatRulesOn()
	d.TermsText, d.TermsVersion = OperatorTermsText, OperatorTermsVersion
	d.TransferText, d.TransferVer = TransferConsentText, TransferConsentVersion
	d.TermsAccepted = a.OperatorTerms.current(OperatorTermsVersion)
	d.TermsOutdated = a.OperatorTerms != nil && !d.TermsAccepted
	if d.TermsAccepted {
		d.TermsSince = a.OperatorTerms.At
	}
	d.TransferOn = a.TransferConsent.current(TransferConsentVersion)
	if d.TransferOn {
		d.TransferSince = a.TransferConsent.At
	}
	d.Countries = make([]countryOption, 0, len(countryCodes)+1)
	for _, c := range countryCodes {
		d.Countries = append(d.Countries, countryOption{c, countryNames[c], a.OperatorCountry == c})
	}
	d.Countries = append(d.Countries, countryOption{countryOther, "Another country", a.OperatorCountry == countryOther})
	d.StatedCountry = map[string]string{"": "", countryOther: "another country"}[a.OperatorCountry]
	if d.StatedCountry == "" && a.OperatorCountry != "" {
		d.StatedCountry = countryLabel(a.OperatorCountry)
	}
}

// chatRulesPage is the account page's data for the chat rules (part of pageData).
type chatRulesPage struct {
	RulesOn                      bool
	TermsText, TermsVersion      string
	TransferText, TransferVer    string
	TermsAccepted, TermsOutdated bool
	TermsSince                   time.Time
	TransferOn                   bool
	TransferSince                time.Time
	Countries                    []countryOption
	StatedCountry                string // its name, "" if none
}

type countryOption struct {
	Code, Name string
	Selected   bool
}

// accountChatRulesTmpl holds the account page's cards for the chat rules (part of accountTmpl).
const accountChatRulesTmpl = `
{{define "transfer"}}{{if .RulesOn}}<div class="card" id="transfer"><div class="card-head"><h2>Volunteers outside Europe</h2>{{if .TransferOn}}<span class="badge on">Allowed</span>{{else}}<span class="badge off">Not allowed</span>{{end}}</div>
{{if .TransferOn}}<p>Your chats and API requests may go to volunteers' computers anywhere, also outside Europe, where the GDPR
does not protect your prompts and those volunteers could read or keep them. You agreed on {{day .TransferSince}}.</p>
<form method="post" action="/account/transfer"><input type="hidden" name="action" value="off"><button class="btn ghost">Stop: Europe only</button></form>
{{else}}<p>Your chats and API requests go only to nodes in the EEA or a country the EU recognises as protecting personal data
(and to the maintainer's own nodes). This can mean waiting longer for a free node.</p>
<form class="form" method="post" action="/account/transfer"><input type="hidden" name="action" value="on"><input type="hidden" name="version" value="{{.TransferVer}}">
<label class="check"><input type="checkbox" name="transfer" value="1" required><span>{{.TransferText}}</span></label>
<div class="actions"><button class="btn secondary">Allow</button></div></form>{{end}}
<p class="mut small">Applies to the chat and to all your API keys. Requests that look like they contain personal data go only to the
maintainer's nodes either way. See <a href="/about/privacy/">Privacy</a>.</p></div>{{end}}{{end}}
{{define "operator"}}{{if .RulesOn}}<div class="card" id="operator"><div class="card-head"><h2>Operator terms</h2>{{if .TermsAccepted}}<span class="badge on">Accepted</span>{{else}}<span class="badge off">Not accepted</span>{{end}}</div>
<p>Your nodes{{if .Account.Links}} and pilots{{end}} serve people's chats and API requests only once you accept the operator terms; until then they serve science only.</p>
{{if .TermsAccepted}}<p>You accepted version <code>{{.TermsVersion}}</code> on {{day .TermsSince}}.{{if .StatedCountry}} Your nodes run in: <strong>{{.StatedCountry}}</strong>.{{end}}</p>
<form class="form" method="post" action="/account/operator"><input type="hidden" name="action" value="country"><div class="field"><label for="op-country">Country your nodes run in</label>
<div class="row"><select id="op-country" name="country"><option value="">not stated</option>{{range .Countries}}<option value="{{.Code}}"{{if .Selected}} selected{{end}}>{{.Name}}</option>{{end}}</select>
<button class="btn ghost">Save</button></div></div></form>
<form method="post" action="/account/operator"><input type="hidden" name="action" value="withdraw"><button class="btn ghost">Withdraw: science only</button></form>
{{else}}{{if .TermsOutdated}}<p role="status"><strong>The operator terms changed.</strong> Accept the new version to keep serving chat.</p>{{end}}
<div class="terms" id="operator-terms" tabindex="0"><p>{{.TermsText}}</p></div>
<form class="form" method="post" action="/account/operator"><input type="hidden" name="action" value="accept"><input type="hidden" name="version" value="{{.TermsVersion}}">
<div class="field"><label for="op-country">Country your nodes run in</label>
<select id="op-country" name="country"><option value="">not stated</option>{{range .Countries}}<option value="{{.Code}}"{{if .Selected}} selected{{end}}>{{.Name}}</option>{{end}}</select></div>
<label class="check"><input type="checkbox" name="accept" value="1" required><span>I have read and accept the operator terms (version <code>{{.TermsVersion}}</code>), and I am 18 or older.</span></label>
<div class="actions"><button class="btn secondary">Accept</button></div></form>{{end}}
<p class="mut small">Chats from people who did not allow volunteers outside Europe reach a node only when it connects from the EEA or a
country the EU recognises, and that is the country you state here. The country of a node's address comes from
<a href="https://db-ip.com">IP Geolocation by DB-IP</a>; a VPN changes it.</p></div>{{end}}{{end}}`
