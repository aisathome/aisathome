// Escalation of failed answer checks (node verification, BACKLOG 11e; verify.go): one failed answer
// earns nothing (enforce mode); repeated failures hold the node for review, and repeated node holds
// hold its account. Holds, never bans: a ban is decided by a person after looking at the evidence,
// with the appeal of the operator terms ("Holds, bans and how to appeal").
//
//   - Node: escalate.node_after failed answers of one node within escalate.window_hours (defaults 3
//     and 24) hold it with rule "answer-checks", like a guard hold: it serves nothing until the
//     operator clears it (`accounts node-clear`, or the operations page), and a pilot's credit is
//     withheld (final 0).
//   - Account: escalate.account_after such holds on record for one account's nodes and pilots
//     (default 2; each pilot is a node of its own, so a BOINC user's second held pilot counts) hold
//     the account (account_holds.go): every node and pilot of it stops, new pilots are refused with
//     how to reach us, and the credit of its pilots that is not final yet is withheld.
//
// Only in enforce mode, and only for untrusted nodes (pilots are never trusted); in observe mode the
// gateway logs that it would have held the node. -1 turns a level off. The failure counts live in
// memory, so a restart starts them again; the holds are in the accounts file.
//
// SPDX-License-Identifier: Apache-2.0
package gateway

import (
	"fmt"
	"strings"
	"time"
)

// ruleAnswerChecks is the rule of holds by the answer checks.
const ruleAnswerChecks = "answer-checks"

// EscalateConfig sets the escalation (VerifyConfig.Escalate). Zero values take the defaults.
type EscalateConfig struct {
	NodeAfter    int `json:"node_after,omitempty"`    // failed answers that hold a node (default 3; -1 never)
	AccountAfter int `json:"account_after,omitempty"` // check holds of its nodes that hold an account (default 2; -1 never)
	WindowHours  int `json:"window_hours,omitempty"`  // the span node_after counts in (default 24)
}

func (e EscalateConfig) withDefaults() (EscalateConfig, error) {
	if e.NodeAfter == 0 {
		e.NodeAfter = 3
	}
	if e.AccountAfter == 0 {
		e.AccountAfter = 2
	}
	if e.WindowHours == 0 {
		e.WindowHours = 24
	}
	if e.NodeAfter < -1 || e.AccountAfter < -1 || e.WindowHours < 0 {
		return e, fmt.Errorf("verify.escalate: node_after and account_after 1 or more (-1: never), window_hours 1 or more")
	}
	return e, nil
}

// failure notes a failed answer of node at now and returns how many it had in the window (at most
// keep are remembered).
func (c *nodeChecks) failure(node string, now time.Time, window time.Duration, keep int) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	s := c.get(node, "", now)
	live := s.fails[:0]
	for _, t := range s.fails {
		if now.Sub(t) < window {
			live = append(live, t)
		}
	}
	s.fails = append(live, now)
	if len(s.fails) > keep {
		s.fails = s.fails[len(s.fails)-keep:]
	}
	return len(s.fails)
}

// escalate counts a failed answer of n (relay.commit, after checkAnswer) and holds the node, and
// then its account, when the counts say so.
func (g *Gateway) escalate(n *nodeConn, rec *usageRecord) {
	v := g.cfg.Verify
	e := v.Escalate
	if e.NodeAfter < 0 || n.trusted || len(rec.Checks) == 0 {
		return
	}
	window := time.Duration(e.WindowHours) * time.Hour
	k := g.checks.failure(n.id, rec.Time, window, max(e.NodeAfter, 1))
	if k < e.NodeAfter {
		return
	}
	reason := fmt.Sprintf("%d answers failed the answer checks within %d hours (the last: %s)", k, e.WindowHours,
		strings.Join(rec.Checks, ", "))
	if v.Mode != VerifyEnforce {
		g.log.Warn("answer checks would hold the node (observe mode)", "node", n.id, "operator", n.operator, "reason", reason)
		return
	}
	g.mu.Lock()
	already := n.held
	g.mu.Unlock()
	if already || g.nodeHeld(n.id) {
		return
	}
	g.log.Warn("answer checks held a node", "node", n.id, "operator", n.operator, "reason", reason, "req", rec.ReqID)
	g.recordHold(n, NodeHold{Node: n.id, Operator: n.operator, Rule: ruleAnswerChecks, Reason: reason, At: rec.Time,
		Req: rec.ReqID, Account: n.account}, "answer checks")
	if n.account == "" || e.AccountAfter < 0 || g.accounts == nil || g.accountHeld(n.account) {
		return
	}
	if held := g.accounts.checkHoldsOf(n.account); held >= e.AccountAfter {
		g.holdAccount(n.account, n.operator, fmt.Sprintf("%d of its nodes were held by the answer checks", held), rec.Time)
	}
}

// holdAccount holds account for review: its connected nodes and pilots stop now, the rest at their
// next connection, and its pilots' credit that is not final yet is withheld.
func (g *Gateway) holdAccount(account, operator, reason string, now time.Time) {
	if err := g.accounts.HoldAccount(AccountHold{Account: account, Rule: ruleAnswerChecks, Reason: reason, At: now.UTC()}); err != nil {
		// Its nodes stay held one by one; the next node hold tries the account again.
		g.log.Error("ACCOUNT HOLD NOT RECORDED: writing the accounts file failed", "operator", operator, "err", err)
		return
	}
	g.log.Warn("answer checks held an account", "operator", operator, "reason", reason)
	g.mu.Lock()
	held := 0
	for _, n := range g.node {
		if n.account == account && !n.held {
			n.held = true
			held++
		}
	}
	g.mu.Unlock()
	for range held {
		g.press.hold(now)
	}
	if g.sci != nil {
		if err := g.sci.withholdAccountPilots(account, "answer checks: account held", now); err != nil {
			g.log.Error("PILOT CREDIT NOT WITHHELD for a held account; its hold refuses new pilots", "operator", operator, "err", err)
		}
	}
}

// withholdAccountPilots withholds the credit of account's pilots that is not final yet (pilots.go
// creditView: final pilotCreditHold after the last disconnect of an ended pilot).
func (s *scienceDB) withholdAccountPilots(account, reason string, now time.Time) error {
	ctx, cancel := pilotCtx()
	defer cancel()
	if _, err := s.db.ExecContext(ctx, `UPDATE pilots SET withheld = ? WHERE account = ? AND withheld = ''
		AND (state != 'ended' OR COALESCE(disconnected_at, ended_at, created_at) > ?)`,
		reason, account, ptime(now.Add(-pilotCreditHold))); err != nil {
		return s.fail("pilot withhold", "", err)
	}
	return nil
}
