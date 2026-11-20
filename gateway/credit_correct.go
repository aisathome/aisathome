// Credit corrections: credit granted by mistake (a counting bug) is taken back, or credit
// withheld by mistake given, as a visible entry rather than by rewriting history. The operator's
// CLI (accounts credit-adjust) writes a request to credit-corrections.json in the state dir; the
// gateway applies each request once, to the node's credit and its owner's (the account or the
// config operator it earned for), keeps it in the ledger (credit.json, "corrections") and lists
// it on the public leaderboard with its reason. A correction changes totals only: not the day's
// credit nor the recent average, which a past mistake has long left or soon leaves.
//
// SPDX-License-Identifier: Apache-2.0
package gateway

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode"
)

// CreditCorrectionsFile holds the operator's correction requests, in the state dir.
const CreditCorrectionsFile = "credit-corrections.json"

// maxReason bounds a correction's reason: it is public.
const maxReason = 200

// CreditCorrection is one correction of a node's credit. Credit is signed: negative takes credit
// back. Account or Operator is whose credit it changed, set when it was applied.
type CreditCorrection struct {
	ID       string    `json:"id"`
	Time     time.Time `json:"time"` // requested
	Node     string    `json:"node"`
	Credit   float64   `json:"credit"`
	Reason   string    `json:"reason"`
	Applied  time.Time `json:"applied,omitzero"`
	Account  string    `json:"account,omitempty"`
	Operator string    `json:"operator,omitempty"`
}

// validReason: one line of printable text, at most maxReason bytes.
func validReason(s string) error {
	if strings.TrimSpace(s) == "" || len(s) > maxReason {
		return fmt.Errorf("the reason is public: one line of 1 to %d characters", maxReason)
	}
	for _, r := range s {
		if !unicode.IsPrint(r) {
			return errors.New("the reason must be one line of printable text")
		}
	}
	return nil
}

// correct applies c to the node's credit and its owner's, once (by id; applied: it was applied
// now). The node must have credit; a correction may not take a node, an account or an operator
// below zero.
func (l *creditLedger) correct(c CreditCorrection, now time.Time) (applied bool, err error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if c.ID == "" {
		return false, errors.New("a correction without an id")
	}
	for _, done := range l.f.Corrections {
		if done.ID == c.ID {
			return false, nil
		}
	}
	if c.Credit == 0 || math.IsNaN(c.Credit) || math.IsInf(c.Credit, 0) {
		return false, errors.New("a correction needs a credit amount other than zero")
	}
	if err := validReason(c.Reason); err != nil {
		return false, err
	}
	n := l.f.Nodes[c.Node]
	if n == nil {
		return false, fmt.Errorf("node %s has no credit", c.Node)
	}
	owner := l.f.Operators[n.Operator]
	if n.Account != "" {
		owner = l.f.Accounts[n.Account]
	}
	if owner == nil {
		return false, fmt.Errorf("node %s: its owner has no credit entry", c.Node)
	}
	if n.Total+c.Credit < -1e-9 || owner.Total+c.Credit < -1e-9 {
		return false, fmt.Errorf("node %s: %.3f would take its credit below zero", c.Node, c.Credit)
	}
	n.Total += c.Credit
	owner.Total += c.Credit
	c.Applied, c.Account, c.Operator = now.UTC(), n.Account, n.Operator
	l.f.Corrections = append(l.f.Corrections, c)
	l.dirty = true
	return true, nil
}

// readCorrections reads a correction file (none: nil).
func readCorrections(path string) ([]CreditCorrection, error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var cs []CreditCorrection
	if err := json.Unmarshal(raw, &cs); err != nil {
		return nil, fmt.Errorf("%s: %w", filepath.Base(path), err)
	}
	return cs, nil
}

// applyCreditCorrections applies the requests in the state dir that are not in the ledger yet.
// One that cannot be applied stays pending and is logged once per gateway run.
func (g *Gateway) applyCreditCorrections() {
	if g.cfg.StateDir == "" {
		return
	}
	cs, err := readCorrections(filepath.Join(g.cfg.StateDir, CreditCorrectionsFile))
	if err != nil {
		g.warnCorrection("file", "credit corrections not read", "err", err)
		return
	}
	for _, c := range cs {
		applied, err := g.credit.correct(c, time.Now())
		if err != nil {
			g.warnCorrection(c.ID, "credit correction not applied", "id", c.ID, "node", c.Node, "err", err)
		} else if applied {
			g.log.Info("credit corrected", "id", c.ID, "node", c.Node, "credit", c.Credit, "reason", c.Reason)
		}
	}
}

// warnCorrection logs a problem with a correction once per gateway run.
func (g *Gateway) warnCorrection(key, msg string, args ...any) {
	if g.correctionSeen("warn:" + key) {
		g.log.Warn(msg, args...)
	}
}

// correctionSeen reports whether key is new to this gateway run, and remembers it.
func (g *Gateway) correctionSeen(key string) bool {
	g.corrMu.Lock()
	defer g.corrMu.Unlock()
	if g.corrSeen == nil {
		g.corrSeen = map[string]bool{}
	}
	if g.corrSeen[key] {
		return false
	}
	g.corrSeen[key] = true
	return true
}

// AddCreditCorrection requests a correction of node's credit (signed) for the gateway to apply
// within seconds (or at its next start). The node must have credit in the ledger and a negative
// correction may not exceed it. It returns the request.
func AddCreditCorrection(stateDir, node string, credit float64, reason string, now time.Time) (CreditCorrection, error) {
	c := CreditCorrection{Time: now.UTC(), Node: node, Credit: credit, Reason: strings.TrimSpace(reason)}
	if err := validReason(c.Reason); err != nil {
		return c, err
	}
	if credit == 0 || math.IsNaN(credit) || math.IsInf(credit, 0) || math.Abs(credit) > 1e9 {
		return c, errors.New("the credit must be a number other than zero (negative takes credit back)")
	}
	_, pending, err := ReadCreditCorrections(stateDir)
	if err != nil {
		return c, err
	}
	var f creditFile
	raw, err := os.ReadFile(filepath.Join(stateDir, "credit.json"))
	if err != nil {
		return c, fmt.Errorf("credit ledger: %w", err)
	}
	if err := json.Unmarshal(raw, &f); err != nil {
		return c, fmt.Errorf("credit ledger: %w", err)
	}
	n := f.Nodes[node]
	if n == nil {
		return c, fmt.Errorf("node %s has no credit in the ledger", node)
	}
	left := n.Total
	for _, p := range pending {
		if p.Node == node {
			left += p.Credit
		}
	}
	if left+credit < -1e-9 {
		return c, fmt.Errorf("node %s has %.3f credit (pending corrections included): %.3f would take it below zero", node, left, credit)
	}
	b := make([]byte, 8)
	rand.Read(b)
	c.ID = "corr-" + hex.EncodeToString(b)
	all := append(append([]CreditCorrection{}, pending...), c) // applied ones leave the file: the ledger keeps them
	raw, err = json.MarshalIndent(all, "", "  ")
	if err != nil {
		return c, err
	}
	path := filepath.Join(stateDir, CreditCorrectionsFile)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(raw, '\n'), 0o600); err != nil {
		return c, err
	}
	return c, os.Rename(tmp, path)
}

// ReadCreditCorrections returns the corrections the ledger holds (as of the gateway's last save)
// and the requests it has not applied yet.
func ReadCreditCorrections(stateDir string) (applied, pending []CreditCorrection, err error) {
	var f creditFile
	raw, err := os.ReadFile(filepath.Join(stateDir, "credit.json"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, nil, fmt.Errorf("credit ledger: %w", err)
	}
	if err == nil {
		if err := json.Unmarshal(raw, &f); err != nil {
			return nil, nil, fmt.Errorf("credit ledger: %w", err)
		}
	}
	reqs, err := readCorrections(filepath.Join(stateDir, CreditCorrectionsFile))
	if err != nil {
		return nil, nil, err
	}
	done := map[string]bool{}
	for _, c := range f.Corrections {
		done[c.ID] = true
	}
	for _, c := range reqs {
		if !done[c.ID] {
			pending = append(pending, c)
		}
	}
	return f.Corrections, pending, nil
}

// publicCorrections lists the applied corrections for the leaderboard: when, how much and why;
// never whose.
func publicCorrections(cs []CreditCorrection) []map[string]any {
	out := []map[string]any{}
	for _, c := range cs {
		out = append(out, map[string]any{"time": c.Applied.UTC().Format(time.RFC3339), "credit": math.Round(c.Credit*1000) / 1000,
			"reason": c.Reason})
	}
	return out
}
