// Pilots as nodes (connector.go): authenticating a pilot's connection, the rules that decide what
// it may serve, its metering into the pilot record, and the sweeps that end it.
//
// SPDX-License-Identifier: Apache-2.0
package gateway

import (
	"errors"
	"net/http"
	"time"

	"github.com/coder/websocket"
)

// pilotLogin is a pilot's credential resolved for a connection.
type pilotLogin struct {
	tok         NodeToken // operator: its account's pseudonym; node id: the pilot id; trust
	account     string
	pilot       *Pilot
	conn        *connector
	first       time.Time // its account's probation start
	scienceOnly bool
}

// pilotScienceOnly reports whether a pilot of connector c's user serves science only (connector.go).
func pilotScienceOnly(c *connector, user string, chatOff, trusted bool) bool {
	return c.noChat[user] || chatOff || (!c.PilotsChatEnabled && !trusted)
}

// authPilot resolves a pilot credential. A pilot connects only once bound, before its deadline,
// while its connector exists and its account may run pilots; status and reason say why not.
func (g *Gateway) authPilot(cred string) (*pilotLogin, int, string) {
	if _, ok := splitPilotCredential(cred); !ok || g.sci == nil || g.accounts == nil {
		return nil, http.StatusUnauthorized, "unknown node token"
	}
	p, err := g.sci.pilotByCredential(keyHash(cred))
	if err == errNoPilot {
		return nil, http.StatusUnauthorized, "unknown node token"
	}
	if err != nil {
		return nil, http.StatusServiceUnavailable, "pilot records unavailable, try again"
	}
	c := g.connector(p.Connector)
	now := g.pilotNow()
	switch {
	case c == nil:
		return nil, http.StatusUnauthorized, "unknown node token"
	case p.State == PilotEnded || !now.Before(p.Deadline):
		return nil, http.StatusUnauthorized, "this pilot has ended"
	case p.State == PilotCreated:
		// Held until the connector names who runs it: the node agent retries within seconds.
		return nil, http.StatusConflict, "pilot not bound yet"
	}
	first, chatOff, live := g.accounts.pilotAccountState(p.account)
	if !live {
		return nil, http.StatusForbidden, "this pilot's account is closed"
	}
	trusted := c.trusted[p.User] && !c.noChat[p.User]
	tok := NodeToken{Operator: g.pseudonym(p.account), NodeID: p.ID}
	if trusted {
		tok.Trust = "trusted"
	}
	return &pilotLogin{tok: tok, account: p.account, pilot: p, conn: c, first: first,
		scienceOnly: pilotScienceOnly(c, p.User, chatOff, trusted)}, 0, ""
}

// pilotClose is the reason in a pilot's close frame (err from its read loop): printable ASCII, at
// most 64 bytes; "connection lost" without one.
func pilotClose(err error) string {
	var ce websocket.CloseError
	if !errors.As(err, &ce) || ce.Reason == "" {
		return "connection lost"
	}
	r := ce.Reason
	if len(r) > 64 {
		r = r[:64]
	}
	for i := 0; i < len(r); i++ {
		if r[i] < 0x20 || r[i] > 0x7e {
			return "connection lost"
		}
	}
	return r
}

// dropPilot closes pilot id's connection, if it has one.
func (g *Gateway) dropPilot(id, why string) {
	g.mu.Lock()
	n := g.node[id]
	g.mu.Unlock()
	if n != nil && n.pilot == id {
		n.conn.Close(websocket.StatusPolicyViolation, why)
	}
}

// pilotServed meters one answer (or attempt) of pilot n into its record (relay.commit). Credit
// follows the ledger's rules: none for self-served, failed, blocked or undelivered answers.
func (g *Gateway) pilotServed(n *nodeConn, rec *usageRecord) {
	ok := rec.Outcome == "ok" || rec.Outcome == "cancelled" || rec.Outcome == outcomePreempted
	u := pilotUse{ok: ok, selfServed: rec.SelfServed}
	if rec.Tokens != nil {
		u.tokens = *rec.Tokens
	}
	if ok && !rec.SelfServed {
		u.credit = rec.Credit
	}
	perPilot, perDay := 0.0, 0.0
	if c := g.connector(n.connector); c != nil {
		perPilot, perDay = c.maxCredit()
	}
	if err := g.sci.meterPilot(n.pilot, n.account, u, perPilot, perDay, rec.Time); err != nil {
		g.log.Error("pilot answer not metered", "pilot", n.pilot, "err", err)
	}
}

// withholdPilotCredit withholds a pilot's credit (final 0) for reason: a guard held it, or the model
// check (BACKLOG 11) found it serving another model; that check calls this when it is built.
func (g *Gateway) withholdPilotCredit(id, reason string) {
	if g.sci == nil || !validPilotID(id) {
		return
	}
	if err := g.sci.withholdPilot(id, reason); err != nil {
		g.log.Error("pilot credit not withheld", "pilot", id, "reason", reason, "err", err)
		return
	}
	g.log.Warn("pilot credit withheld", "pilot", id, "reason", reason)
}

// sweepPilots ends pilots past their deadline (dropping their connections) and marks the
// connected ones as seen, so that a restart counts their serve time.
func (g *Gateway) sweepPilots() {
	now := g.pilotNow()
	if err := g.sci.expirePilots(now); err != nil {
		return
	}
	var ids []string
	var late []*nodeConn
	g.mu.Lock()
	for _, n := range g.node {
		if n.pilot == "" {
			continue
		}
		if !now.Before(n.deadline) {
			late = append(late, n)
		} else {
			ids = append(ids, n.pilot)
		}
	}
	g.mu.Unlock()
	for _, n := range late {
		n.conn.Close(websocket.StatusPolicyViolation, "pilot deadline passed")
	}
	g.sci.pilotsSeen(ids, now)
}
