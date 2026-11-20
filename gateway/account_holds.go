// Account holds (node verification, BACKLOG 11e; verify_escalate.go): an account whose nodes the
// answer checks held repeatedly is held for review. Its nodes and pilots serve nothing, new pilots
// are refused with how to reach us, and the credit of its pilots that is not final yet is withheld.
// A hold is not a ban: bans are decided by a person (the operator terms' "Holds, bans and how to
// appeal"). The operator lifts it with `accounts account-clear <id>`; withheld pilot credit stays
// withheld, as after a guard hold (BACKLOG 12h).
//
// SPDX-License-Identifier: Apache-2.0
package gateway

import (
	"fmt"
	"slices"
	"time"
)

// AccountHold is one held account.
type AccountHold struct {
	Account string    `json:"account"`
	Rule    string    `json:"rule"`   // "answer-checks"
	Reason  string    `json:"reason"` // the gateway's own words: how many of its nodes were held, and why
	At      time.Time `json:"at"`
}

// HoldAccount holds an account until ClearAccountHold (the first hold stands).
func (s *AccountStore) HoldAccount(h AccountHold) error {
	return s.Update(func(f *AccountsFile) error {
		for _, q := range f.AccountHolds {
			if q.Account == h.Account {
				return nil
			}
		}
		f.AccountHolds = append(f.AccountHolds, h)
		return nil
	})
}

// ClearAccountHold lifts an account's hold (operator).
func (s *AccountStore) ClearAccountHold(id string) error {
	return s.Update(func(f *AccountsFile) error {
		for i, q := range f.AccountHolds {
			if q.Account == id {
				f.AccountHolds = append(f.AccountHolds[:i], f.AccountHolds[i+1:]...)
				return nil
			}
		}
		return fmt.Errorf("account %s is not held", id)
	})
}

// moveCheckHolds gives account from's holds by the answer checks to account to (a claim merges an
// unclaimed account into the claimant: its pilots serve as the claimant's from then on). An account
// hold the claimant already has stands.
func (f *AccountsFile) moveCheckHolds(from, to string) {
	for i := range f.NodeHolds {
		if f.NodeHolds[i].Account == from {
			f.NodeHolds[i].Account = to
		}
	}
	held := slices.ContainsFunc(f.AccountHolds, func(h AccountHold) bool { return h.Account == to })
	f.AccountHolds = slices.DeleteFunc(f.AccountHolds, func(h AccountHold) bool {
		if h.Account != from {
			return false
		}
		if held {
			return true
		}
		held = true
		return false
	})
	for i := range f.AccountHolds {
		if f.AccountHolds[i].Account == from {
			f.AccountHolds[i].Account = to
		}
	}
}

// accountHolds lists the held accounts.
func (s *AccountStore) accountHolds() []AccountHold {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.f.AccountHolds)
}

// AccountHeldList is accountHolds for the operator's tool.
func (s *AccountStore) AccountHeldList() []AccountHold { return s.accountHolds() }

// isAccountHeld reports whether account id is held.
func (s *AccountStore) isAccountHeld(id string) bool {
	if id == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, q := range s.f.AccountHolds {
		if q.Account == id {
			return true
		}
	}
	return false
}

// checkHoldsOf counts the holds on record by the answer checks of account id's nodes and pilots.
func (s *AccountStore) checkHoldsOf(id string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, p := range s.f.NodeHolds {
		if p.Account == id && p.Rule == ruleAnswerChecks {
			n++
		}
	}
	return n
}

// accountHeld reports whether account (of a node or pilot; "" for config-token nodes) is held.
func (g *Gateway) accountHeld(account string) bool {
	return g.accounts != nil && g.accounts.isAccountHeld(account)
}

// heldAccountMsg is what a pilot of a held account is told when it connects (the node agent logs it;
// for BOINC it lands in the task's stderr).
const heldAccountMsg = "this account is held for review after failed answer checks; it is not a ban. " +
	"Write to contact@aisathome.org with your account id to speed it up (https://aisathome.org/about/terms/)"
