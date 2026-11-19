// The account page's side of connectors (connector_requests.go): "Run on BOINC", "Claim a BOINC
// account", the attach steps of a linked user, the own-address change, the "chat on my pilots" switch
// and the list of the account's pilots. Forms like the rest of the page: no script.
//
// SPDX-License-Identifier: Apache-2.0
package gateway

import (
	"crypto/md5"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/mail"
	"slices"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// accountPilotsShown: the newest pilots the account page lists.
const accountPilotsShown = 20

// connectorCard is one connector's card on the account page.
type connectorCard struct {
	ID, Name, AttachURL, KeyURL string
	Links                       []linkRow
	Pending                     *requestRow // a request waiting for the connector
	Failed                      *requestRow // the newest request, if it failed in the last day
	Done                        *requestRow // the newest request, if it finished in the last 10 minutes
}

type linkRow struct {
	User, Key, Command string
	Login              string // the pseudonymous login of a user made here, while it has no own address
	Created, Claimed   bool
	OwnEmail           bool
}

type requestRow struct {
	Kind, What, Error string
	Since             time.Time
}

// pilotRow is one pilot in the account page's list.
type pilotRow struct {
	ID, Connector, State, Host string
	Started                    time.Time
	Served                     string
	Requests                   int64
	Credit                     string
	Final                      bool
}

func requestWhat(kind, name string) string {
	switch kind {
	case reqCreateLink:
		return "creating your " + name + " account"
	case reqClaim:
		return "checking your " + name + " account key"
	default:
		return "changing your address at " + name
	}
}

// fmtServed is a serve time as "2 h 05 min" or "4 min".
func fmtServed(sec int64) string {
	if sec < 3600 {
		return fmt.Sprintf("%d min", sec/60)
	}
	return fmt.Sprintf("%d h %02d min", sec/3600, sec%3600/60)
}

// connectorData fills the account page's connector cards and pilots for account a.
func (g *Gateway) connectorData(d *pageData, a Account) {
	t := g.connCfgs.Load()
	if t == nil || g.sci == nil {
		return
	}
	now := g.pilotNow()
	var reqs []*connRequest
	ids := make([]string, 0, len(t.byID))
	for id, c := range t.byID {
		if c.Accounts != nil {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	if len(ids) > 0 {
		var err error
		if reqs, err = g.sci.requests("", "", a.ID, 20); err != nil {
			d.Error = "Your connector requests could not be read just now; reload in a moment."
		}
	}
	for _, id := range ids {
		c := t.byID[id]
		card := connectorCard{ID: id, Name: c.Accounts.Name, AttachURL: c.Accounts.AttachURL, KeyURL: c.Accounts.KeyURL}
		for _, l := range a.Links {
			if l.Connector != id {
				continue
			}
			row := linkRow{User: l.User, Key: l.AttachKey, Created: l.Via == LinkCreated, Claimed: l.Via == LinkClaimed, OwnEmail: l.OwnEmail}
			if l.AttachKey != "" && c.Accounts.AttachCommand != "" {
				row.Command = strings.NewReplacer("{url}", c.Accounts.AttachURL, "{key}", l.AttachKey).Replace(c.Accounts.AttachCommand)
			}
			if row.Created && !l.OwnEmail {
				row.Login = g.pseudonymousAddress(c, a.ID)
			}
			card.Links = append(card.Links, row)
		}
		newest := true
		for _, q := range reqs { // newest first
			if q.Connector != id {
				continue
			}
			row := &requestRow{Kind: q.Kind, What: requestWhat(q.Kind, card.Name), Error: q.Error, Since: q.Created}
			switch {
			case q.State == reqPending || q.State == reqVerified:
				card.Pending = row
			case newest && q.State == reqFailed && now.Sub(q.Updated) < 24*time.Hour:
				card.Failed = row
			case newest && q.State == reqDone && now.Sub(q.Updated) < 10*time.Minute:
				card.Done = row
			}
			newest = false
		}
		d.Connectors = append(d.Connectors, card)
		d.PilotsChat = d.PilotsChat || c.PilotsChatEnabled
	}
	ps, err := g.sci.accountPilots(a.ID, accountPilotsShown)
	if err != nil {
		return
	}
	for _, p := range ps {
		row := pilotRow{ID: p.ID, Connector: p.Connector, State: p.State, Host: p.Host, Started: p.Created,
			Served: fmtServed(p.ServedSec), Requests: p.Requests}
		if p.First != nil {
			row.Started = *p.First
		}
		cv := p.creditView(now)
		row.Final = cv.Final
		credit := p.Credit
		if p.Withheld != "" {
			credit = 0
		}
		row.Credit = fmtCredit(credit)
		d.Pilots = append(d.Pilots, row)
	}
}

// connectorForm is the start of every connector form: the signed-in, active account and the
// connector (with accounts on) the form is for. Anything else is answered here.
func (g *Gateway) connectorForm(w http.ResponseWriter, r *http.Request) (string, *connector, bool) {
	id, ok := g.session(r)
	if !ok || !g.sameOrigin(r) {
		http.Redirect(w, r, g.acfg.PublicURL+"/account", http.StatusSeeOther)
		return "", nil, false
	}
	c := g.connector(r.PathValue("id"))
	if c == nil || c.Accounts == nil {
		http.NotFound(w, r)
		return "", nil, false
	}
	if !g.accounts.active(id) {
		g.formError(w, r, id, errClaimInactive)
		return "", nil, false
	}
	return id, c, true
}

func (g *Gateway) formError(w http.ResponseWriter, r *http.Request, id string, err error) {
	d := g.accountData(id)
	d.Error = err.Error()
	g.accountPage(w, r, http.StatusBadRequest, d)
}

func (g *Gateway) connectorDone(w http.ResponseWriter, r *http.Request, c *connector) {
	http.Redirect(w, r, g.acfg.PublicURL+"/account#connector-"+c.ID, http.StatusSeeOther)
}

// ownAddress checks an address a person typed for a connector user: one plain address.
func ownAddress(s string) (string, error) {
	s = strings.TrimSpace(s)
	a, err := mail.ParseAddress(s)
	if err != nil || a.Address != s || a.Name != "" || len(s) > 254 || strings.ContainsAny(s, " <>\"") {
		return "", errors.New("type one email address, such as name@example.org")
	}
	return s, nil
}

// displayName checks a display name for a connector user (BOINC publishes it): 1-40 printable
// characters, no address.
func displayName(s string) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		b := make([]byte, 3)
		rand.Read(b)
		return "aisathome-" + hex.EncodeToString(b), nil
	}
	if !utf8.ValidString(s) || utf8.RuneCountInString(s) > 40 || strings.ContainsAny(s, "@<>&\"") ||
		strings.IndexFunc(s, func(r rune) bool { return !unicode.IsPrint(r) }) >= 0 {
		return "", errors.New("display name: up to 40 characters, no email address and none of < > & \"")
	}
	return s, nil
}

// handleConnectorRun is "Run on BOINC": a create_link request with a pseudonymous address (or the
// person's own), a display name and, if chosen, the password's hash.
func (g *Gateway) handleConnectorRun(w http.ResponseWriter, r *http.Request) {
	id, c, ok := g.connectorForm(w, r)
	if !ok {
		return
	}
	a, _, _ := g.accounts.Get(id)
	if slices.ContainsFunc(a.Links, func(l AccountLink) bool { return l.Connector == c.ID }) {
		g.formError(w, r, id, fmt.Errorf("this account is linked to a %s account already", c.Accounts.Name))
		return
	}
	q := &connRequest{Connector: c.ID, Kind: reqCreateLink, Account: id}
	var err error
	if r.FormValue("address") == "own" {
		if q.Email, err = ownAddress(r.FormValue("email")); err != nil {
			g.formError(w, r, id, err)
			return
		}
		q.OwnEmail = true
	} else {
		q.Email = g.pseudonymousAddress(c, id)
	}
	if q.UserName, err = displayName(r.FormValue("name")); err != nil {
		g.formError(w, r, id, err)
		return
	}
	if pw := r.FormValue("password"); pw != "" {
		if n := utf8.RuneCountInString(pw); n < 8 || n > 128 {
			g.formError(w, r, id, errors.New("password: 8 to 128 characters, or none"))
			return
		}
		sum := md5.Sum([]byte(pw + strings.ToLower(q.Email))) // BOINC's passwd_hash; the password itself is never kept
		q.PasswdHash = hex.EncodeToString(sum[:])
	}
	if err := g.sci.addRequest(q, g.pilotNow()); err != nil {
		g.formError(w, r, id, err)
		return
	}
	g.log.Info("connector request", "connector", c.ID, "request", q.ID, "kind", q.Kind)
	g.connectorDone(w, r, c)
}

// handleConnectorClaim is "Claim a BOINC account": a claim request with the pasted key.
func (g *Gateway) handleConnectorClaim(w http.ResponseWriter, r *http.Request) {
	id, c, ok := g.connectorForm(w, r)
	if !ok {
		return
	}
	key := strings.TrimSpace(r.FormValue("key"))
	m := claimKeyRe.FindStringSubmatch(key)
	if m == nil {
		g.formError(w, r, id, fmt.Errorf("that is not a %s account key: it looks like 123456_ followed by 32 letters and digits", c.Accounts.Name))
		return
	}
	a, _, _ := g.accounts.Get(id)
	if slices.ContainsFunc(a.Links, func(l AccountLink) bool { return l.Connector == c.ID && l.User == m[1] }) {
		g.formError(w, r, id, fmt.Errorf("this %s account is linked to your account already", c.Accounts.Name))
		return
	}
	q := &connRequest{Connector: c.ID, Kind: reqClaim, Account: id, User: m[1], AttachKey: key}
	if err := g.sci.addRequest(q, g.pilotNow()); err != nil {
		g.formError(w, r, id, err)
		return
	}
	g.log.Info("connector request", "connector", c.ID, "request", q.ID, "kind", q.Kind)
	g.connectorDone(w, r, c)
}

// handleConnectorEmail gives a user made here with a pseudonymous address the person's own.
func (g *Gateway) handleConnectorEmail(w http.ResponseWriter, r *http.Request) {
	id, c, ok := g.connectorForm(w, r)
	if !ok {
		return
	}
	user := r.FormValue("user")
	a, _, _ := g.accounts.Get(id)
	if !slices.ContainsFunc(a.Links, func(l AccountLink) bool {
		return l.Connector == c.ID && l.User == user && l.Via == LinkCreated && !l.OwnEmail
	}) {
		g.formError(w, r, id, fmt.Errorf("only a %s account made here with a pseudonymous address can switch to your own", c.Accounts.Name))
		return
	}
	addr, err := ownAddress(r.FormValue("email"))
	if err != nil {
		g.formError(w, r, id, err)
		return
	}
	q := &connRequest{Connector: c.ID, Kind: reqSetEmail, Account: id, User: user, Email: addr, OwnEmail: true}
	if err := g.sci.addRequest(q, g.pilotNow()); err != nil {
		g.formError(w, r, id, err)
		return
	}
	g.log.Info("connector request", "connector", c.ID, "request", q.ID, "kind", q.Kind)
	g.connectorDone(w, r, c)
}

// handlePilotChatForm is the account page's "chat on my pilots" switch (PATCH /v1/account/settings
// for API keys sets the same).
func (g *Gateway) handlePilotChatForm(w http.ResponseWriter, r *http.Request) {
	id, ok := g.session(r)
	if !ok || !g.sameOrigin(r) {
		http.Redirect(w, r, g.acfg.PublicURL+"/account", http.StatusSeeOther)
		return
	}
	on := r.FormValue("chat")
	if on != "on" && on != "off" {
		g.formError(w, r, id, errors.New("chat: on or off"))
		return
	}
	if !g.accounts.active(id) {
		g.formError(w, r, id, errors.New("only an active account can change this"))
		return
	}
	if err := g.accounts.SetPilotChat(id, on == "on"); err != nil {
		g.formError(w, r, id, err)
		return
	}
	g.sweepNodes() // connected pilots follow now, not at the next sweep
	http.Redirect(w, r, g.acfg.PublicURL+"/account#pilots", http.StatusSeeOther)
}
