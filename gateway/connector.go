// Connectors (decision 0020): an outside system that brings machines to the
// network in bounded serving sessions, pilots (pilots.go). BOINC's bridge is the first; nothing here
// knows BOINC beyond the configuration. A connector exists only in the gateway config, has its own
// keys (by SHA-256) and an optional address allowlist, and its key opens only its own endpoints:
//
//	GET  /v1/connectors/{id}/demand                  how many pilots to create now (the formula below)
//	POST /v1/connectors/{id}/pilots                  create a pilot: {"deadline"} → id and credential (once)
//	GET  /v1/connectors/{id}/pilots?state=&limit=    its pilots, newest first
//	GET  /v1/connectors/{id}/pilots/{pid}            one pilot's record and totals
//	POST /v1/connectors/{id}/pilots/{pid}/bind       {"user", "host", "team"}: who runs it
//	POST /v1/connectors/{id}/pilots/{pid}/reported   {"outcome"}: the work item is done; the pilot ends
//	GET  /v1/connectors/{id}/pilots/{pid}/credit     its final credit, or not yet
//
// Pilots as nodes (decision 0007): a bound pilot is a node of the account its connector user maps to
// (account_links.go), untrusted, under the same rules as any account node: personal data only to
// trusted nodes, the share cap per operator, the guards, tools only past the account's probation
// (or, for a project with pilot_probation_ok, on pilots during it). On top of that a pilot serves
// science only, not chat, agents or API calls, when:
//   - its connector user is in no_chat_users (Science United's account: its hosts cannot reach the
//     switch), or
//   - its account turned "chat on pilots" off, or
//   - pilots_chat_enabled is false for its connector (the default) and its user is not in
//     trusted_users. This keeps the rule that people's prompts reach strangers' machines only after
//     anti-retention measures and the privacy notice are in place: the operator turns it on then.
//
// Pilots of trusted_users are the maintainer's own machines (test hosts): they serve chat before
// pilots_chat_enabled, but no pilot is ever trusted (a credential copied in transit must not
// receive personal data).
//
// SPDX-License-Identifier: Apache-2.0
package gateway

import (
	"crypto/subtle"
	"fmt"
	"math"
	"net"
	"net/http"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Connector is an outside system that brings pilots (Config.Connectors).
type Connector struct {
	// ID names the connector in its endpoints ("boinc"): lower-case letters, digits and dashes.
	ID string `json:"id"`
	// KeySHA256 lists the SHA-256 (hex) of its keys (make one with `aisathome-gateway project-key`).
	KeySHA256 []string `json:"key_sha256"`
	// AllowFrom, if set, limits its endpoints to these addresses or prefixes ("203.0.113.7",
	// "2001:db8::/48"), as the client address is seen (behind a local reverse proxy, the last
	// X-Forwarded-For hop).
	AllowFrom []string `json:"allow_from,omitempty"`
	// MaxPilots: at most this many of its pilots that have not ended (0: it may create none).
	MaxPilots int `json:"max_pilots"`
	// MintPerHour: at most this many pilots created in an hour. Default 120.
	MintPerHour int `json:"mint_per_hour,omitempty"`
	// MaxDeadlineHours: a pilot's deadline at most this far ahead. Default 336 (14 days).
	MaxDeadlineHours int `json:"max_deadline_hours,omitempty"`
	// PilotsChatEnabled lets its pilots serve chat, agents and API calls besides science (each
	// account's switch still applies). Off by default: science only, until the operator turns it on.
	PilotsChatEnabled bool `json:"pilots_chat_enabled,omitempty"`
	// NoChatUsers: its users whose pilots serve science only, whatever else says.
	NoChatUsers []string `json:"no_chat_users,omitempty"`
	// TrustedUsers: its users whose pilots are the maintainer's own machines: they serve chat before
	// PilotsChatEnabled, as untrusted nodes.
	TrustedUsers []string `json:"trusted_users,omitempty"`
	// ChatHeadroom: pilots to keep for chat, agents and API calls (demand), counted only while
	// PilotsChatEnabled.
	ChatHeadroom int `json:"chat_headroom,omitempty"`
	// SessionsPerSlot: science sessions one GPU slot keeps busy (they alternate model calls with
	// their own tools). Default 2.
	SessionsPerSlot int `json:"sessions_per_slot,omitempty"`
	// UnsentBuffer: at most this many created pilots that no machine took yet. Default 2.
	UnsentBuffer int `json:"unsent_buffer,omitempty"`
	// MaxCreditPerPilot and MaxCreditPerAccountDay cap pilot credit (0: the defaults below; -1: none).
	MaxCreditPerPilot      float64 `json:"max_credit_per_pilot,omitempty"`
	MaxCreditPerAccountDay float64 `json:"max_credit_per_account_day,omitempty"`
}

// Defaults of a connector's limits. The credit caps are about 1.3 times what a pilot serving all
// the time for 4 hours, and two such GPUs all day, earn at the measured rate of about 28,750 credit
// an hour on a 3090 (2026-11-18, our science traffic): tune them once the first pilots are measured.
const (
	defaultMintPerHour       = 120
	defaultMaxDeadlineHours  = 14 * 24
	defaultSessionsPerSlot   = 2
	defaultUnsentBuffer      = 2
	defaultMaxCreditPerPilot = 150_000
	defaultMaxCreditPerDay   = 1_800_000
)

// connector is a checked Connector.
type connector struct {
	Connector
	keys    []string // hashes, lower case
	allow   []netip.Prefix
	noChat  map[string]bool
	trusted map[string]bool
}

func (c *connector) maxCredit() (perPilot, perDay float64) {
	perPilot, perDay = c.MaxCreditPerPilot, c.MaxCreditPerAccountDay
	if perPilot == 0 {
		perPilot = defaultMaxCreditPerPilot
	}
	if perDay == 0 {
		perDay = defaultMaxCreditPerDay
	}
	return max(perPilot, 0), max(perDay, 0) // -1: none (0 in meterPilot)
}

// connectorTable is replaced as a whole on reload.
type connectorTable struct {
	byID map[string]*connector
}

// newConnectorTable checks the connectors of a config; their keys must differ from the projects'.
func newConnectorTable(cs []Connector, projects *projectTable) (*connectorTable, error) {
	t := &connectorTable{byID: map[string]*connector{}}
	seen := map[string]bool{}
	for _, c := range cs {
		if !projectID.MatchString(c.ID) {
			return nil, fmt.Errorf("connector %q: id must be 1-32 lower-case letters, digits or dashes", c.ID)
		}
		if t.byID[c.ID] != nil {
			return nil, fmt.Errorf("connector %q is defined twice", c.ID)
		}
		if c.MaxPilots < 0 || c.MintPerHour < 0 || c.MaxDeadlineHours < 0 || c.ChatHeadroom < 0 || c.SessionsPerSlot < 0 || c.UnsentBuffer < 0 {
			return nil, fmt.Errorf("connector %q: limits must not be negative", c.ID)
		}
		for _, v := range []float64{c.MaxCreditPerPilot, c.MaxCreditPerAccountDay} {
			if math.IsNaN(v) || math.IsInf(v, 0) || (v < 0 && v != -1) {
				return nil, fmt.Errorf("connector %q: credit caps are positive, 0 (default) or -1 (none)", c.ID)
			}
		}
		cc := &connector{Connector: c, noChat: map[string]bool{}, trusted: map[string]bool{}}
		if cc.MintPerHour == 0 {
			cc.MintPerHour = defaultMintPerHour
		}
		if cc.MaxDeadlineHours == 0 {
			cc.MaxDeadlineHours = defaultMaxDeadlineHours
		}
		if cc.SessionsPerSlot == 0 {
			cc.SessionsPerSlot = defaultSessionsPerSlot
		}
		if cc.UnsentBuffer == 0 {
			cc.UnsentBuffer = defaultUnsentBuffer
		}
		if len(c.KeySHA256) == 0 {
			return nil, fmt.Errorf("connector %q has no key_sha256", c.ID)
		}
		for _, h := range c.KeySHA256 {
			h = strings.ToLower(strings.TrimSpace(h))
			if len(h) != 64 || strings.Trim(h, "0123456789abcdef") != "" {
				return nil, fmt.Errorf("connector %q: key_sha256 entries must be 64 hex characters", c.ID)
			}
			if seen[h] || (projects != nil && projects.byHash[h] != nil) {
				return nil, fmt.Errorf("connector %q: a key hash is listed twice, or is also a project's", c.ID)
			}
			seen[h] = true
			cc.keys = append(cc.keys, h)
		}
		for _, a := range c.AllowFrom {
			p, err := netip.ParsePrefix(a)
			if err != nil {
				addr, aerr := netip.ParseAddr(a)
				if aerr != nil {
					return nil, fmt.Errorf("connector %q: allow_from %q is not an address or prefix", c.ID, a)
				}
				p = netip.PrefixFrom(addr.Unmap(), addr.Unmap().BitLen())
			}
			cc.allow = append(cc.allow, p.Masked())
		}
		for _, list := range []struct {
			users []string
			set   map[string]bool
		}{{c.NoChatUsers, cc.noChat}, {c.TrustedUsers, cc.trusted}} {
			for _, u := range list.users {
				if !extIDRe.MatchString(u) {
					return nil, fmt.Errorf("connector %q: user %q: 1-64 letters, digits, '.', '_', ':' or '-'", c.ID, u)
				}
				list.set[u] = true
			}
		}
		t.byID[c.ID] = cc
	}
	return t, nil
}

// connector returns connector id of the current config, or nil.
func (g *Gateway) connector(id string) *connector {
	if t := g.connCfgs.Load(); t != nil {
		return t.byID[id]
	}
	return nil
}

// requestAddr is the client address of r as clientIP sees it (behind a local proxy, the last
// X-Forwarded-For hop), unmasked.
func requestAddr(r *http.Request) (netip.Addr, bool) {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	addr, err := netip.ParseAddr(host)
	if err != nil {
		return netip.Addr{}, false
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
	return addr, true
}

// connectorFor authenticates a call to connector id's endpoints: its own key, from an allowed
// address. Anything else is answered here.
func (g *Gateway) connectorFor(w http.ResponseWriter, r *http.Request) *connector {
	c := g.connector(r.PathValue("id"))
	h := keyHash(bearer(r))
	ok := false
	if c != nil && bearer(r) != "" {
		for _, k := range c.keys {
			ok = ok || subtle.ConstantTimeCompare([]byte(k), []byte(h)) == 1
		}
	}
	if !ok {
		apiError(w, http.StatusUnauthorized, "unknown connector or key")
		return nil
	}
	if len(c.allow) > 0 {
		addr, aok := requestAddr(r)
		if !aok || !slices.ContainsFunc(c.allow, func(p netip.Prefix) bool { return p.Contains(addr) }) {
			g.log.Warn("connector call from an address not in allow_from", "connector", c.ID, "addr", addr)
			apiError(w, http.StatusForbidden, "this connector's key is not accepted from this address")
			return nil
		}
	}
	if g.sci == nil || g.accounts == nil {
		apiError(w, http.StatusServiceUnavailable, "pilots need accounts and the science database")
		return nil
	}
	return c
}

func (g *Gateway) connectorRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/connectors/{id}/demand", g.handleConnectorDemand)
	mux.HandleFunc("POST /v1/connectors/{id}/pilots", g.handlePilotCreate)
	mux.HandleFunc("GET /v1/connectors/{id}/pilots", g.handlePilotList)
	mux.HandleFunc("GET /v1/connectors/{id}/pilots/{pid}", g.handlePilotGet)
	mux.HandleFunc("POST /v1/connectors/{id}/pilots/{pid}/bind", g.handlePilotBind)
	mux.HandleFunc("POST /v1/connectors/{id}/pilots/{pid}/reported", g.handlePilotReported)
	mux.HandleFunc("GET /v1/connectors/{id}/pilots/{pid}/credit", g.handlePilotCredit)
}

const maxConnectorBody = 4 << 10

// pilotNow is the pilots' clock (tests set Config.PilotsNow).
func (g *Gateway) pilotNow() time.Time {
	if g.cfg.PilotsNow != nil {
		return g.cfg.PilotsNow()
	}
	return time.Now()
}

func (g *Gateway) handlePilotCreate(w http.ResponseWriter, r *http.Request) {
	c := g.connectorFor(w, r)
	if c == nil {
		return
	}
	var body struct {
		Deadline time.Time `json:"deadline"`
	}
	if !decodeTaskBody(w, r, maxConnectorBody, &body) {
		return
	}
	now := g.pilotNow()
	if !body.Deadline.After(now) || body.Deadline.Sub(now) > time.Duration(c.MaxDeadlineHours)*time.Hour {
		apiError(w, http.StatusBadRequest, fmt.Sprintf("deadline: an RFC 3339 time in the next %d hours", c.MaxDeadlineHours))
		return
	}
	p, cred, err := g.sci.createPilot(c.ID, body.Deadline, c.MaxPilots, c.MintPerHour, now)
	if err != nil {
		taskError(w, err)
		return
	}
	g.log.Info("pilot created", "connector", c.ID, "pilot", p.ID, "deadline", p.Deadline)
	writeJSON(w, http.StatusCreated, map[string]any{"pilot": g.pilotView(p), "credential": cred})
}

func (g *Gateway) handlePilotList(w http.ResponseWriter, r *http.Request) {
	c := g.connectorFor(w, r)
	if c == nil {
		return
	}
	state := r.URL.Query().Get("state")
	if state != "" && !slices.Contains(pilotStates, state) {
		apiError(w, http.StatusBadRequest, "state: "+strings.Join(pilotStates, ", "))
		return
	}
	limit := 100
	if v := r.URL.Query().Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > pilotListMax {
			apiError(w, http.StatusBadRequest, fmt.Sprintf("limit: 1 to %d", pilotListMax))
			return
		}
		limit = n
	}
	ps, err := g.sci.pilots(c.ID, state, limit)
	if err != nil {
		taskError(w, err)
		return
	}
	out := make([]*Pilot, len(ps))
	for i, p := range ps {
		out[i] = g.pilotView(p)
	}
	writeJSON(w, http.StatusOK, map[string]any{"pilots": out})
}

func (g *Gateway) handlePilotGet(w http.ResponseWriter, r *http.Request) {
	c := g.connectorFor(w, r)
	if c == nil {
		return
	}
	p, err := g.sci.pilot(c.ID, r.PathValue("pid"))
	if err != nil {
		taskError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"pilot": g.pilotView(p)})
}

// pilotView fills in what a pilot's account is (linked or unclaimed), never which.
func (g *Gateway) pilotView(p *Pilot) *Pilot {
	if p.account != "" && g.accounts != nil {
		p.AccountKind = "linked"
		if a, _, ok := g.accounts.Get(p.account); ok && a.Status == StatusUnclaimed {
			p.AccountKind = "unclaimed"
		}
	}
	return p
}

func (g *Gateway) handlePilotBind(w http.ResponseWriter, r *http.Request) {
	c := g.connectorFor(w, r)
	if c == nil {
		return
	}
	var body struct {
		User string `json:"user"`
		Host string `json:"host"`
		Team string `json:"team"`
	}
	if !decodeTaskBody(w, r, maxConnectorBody, &body) {
		return
	}
	if !extIDRe.MatchString(body.User) || !extIDRe.MatchString(body.Host) || (body.Team != "" && !extIDRe.MatchString(body.Team)) {
		apiError(w, http.StatusBadRequest, "user and host (team optional): the connector's own ids, 1-64 letters, digits, '.', '_', ':' or '-'")
		return
	}
	pid := r.PathValue("pid")
	p, err := g.sci.pilot(c.ID, pid)
	if err != nil {
		taskError(w, err)
		return
	}
	now := g.pilotNow()
	account := p.account
	if p.State == PilotCreated { // map the user to its account only for a bind that can succeed
		var aerr error
		if account, _, aerr = g.accounts.pilotAccount(c.ID, body.User, now); aerr != nil {
			g.log.Error("pilot bind: accounts file not written", "connector", c.ID, "pilot", pid, "err", aerr)
			taskError(w, terr(http.StatusServiceUnavailable, "accounts unavailable, try again"))
			return
		}
	}
	if p, err = g.sci.bindPilot(c.ID, pid, body.User, body.Host, body.Team, account, now); err != nil {
		taskError(w, err)
		return
	}
	g.log.Info("pilot bound", "connector", c.ID, "pilot", pid)
	writeJSON(w, http.StatusOK, map[string]any{"pilot": g.pilotView(p)})
}

func (g *Gateway) handlePilotReported(w http.ResponseWriter, r *http.Request) {
	c := g.connectorFor(w, r)
	if c == nil {
		return
	}
	var body struct {
		Outcome string `json:"outcome"`
	}
	if !decodeTaskBody(w, r, maxConnectorBody, &body) {
		return
	}
	if !slices.Contains(pilotOutcomes, body.Outcome) {
		apiError(w, http.StatusBadRequest, "outcome: "+strings.Join(pilotOutcomes, ", "))
		return
	}
	pid := r.PathValue("pid")
	p, err := g.sci.pilot(c.ID, pid)
	if err == errNoPilot {
		taskError(w, err)
		return
	}
	if err == nil {
		p, err = g.sci.endPilot(c.ID, pid, "reported: "+body.Outcome, g.pilotNow())
	}
	// Dropped whatever the database said: a reported pilot has exited, and one that failed to end in
	// the record must not keep serving until its deadline. Its own connection says whose it is.
	g.dropPilot(c.ID, pid, "pilot reported")
	if err != nil {
		taskError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"pilot": g.pilotView(p)})
}

func (g *Gateway) handlePilotCredit(w http.ResponseWriter, r *http.Request) {
	c := g.connectorFor(w, r)
	if c == nil {
		return
	}
	p, err := g.sci.pilot(c.ID, r.PathValue("pid"))
	if err != nil {
		taskError(w, err)
		return
	}
	if p.Withheld == "" && g.nodeHeld(p.ID) { // a hold whose withholding write failed
		p.Withheld = "guard hold"
	}
	writeJSON(w, http.StatusOK, p.creditView(g.pilotNow()))
}
