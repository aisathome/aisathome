// Accounts and connectors (decision 0020, addendum 2026-11-17): every user a connector names (a BOINC
// user id) maps to one account here. An account linked to that user serves its pilots; without one,
// the first pilot of that user creates an unclaimed account keyed by a link to it, which its person
// claims later from the account page (connector_requests.go: claimLink merges the unclaimed account
// into theirs). The operator can also link by hand with `accounts link`. The link is an id, never an
// address.
//
// SPDX-License-Identifier: Apache-2.0
package gateway

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"time"
)

// linkOwner returns the account linked to connector's user, or nil.
func (f *AccountsFile) linkOwner(connector, user string) *Account {
	for _, a := range f.Accounts {
		for _, l := range a.Links {
			if l.Connector == connector && l.User == user {
				return a
			}
		}
	}
	return nil
}

// newUnclaimedID is "cx:" and 16 random bytes in hex: not derived from the connector's user id.
func newUnclaimedID() string {
	b := make([]byte, 16)
	rand.Read(b)
	return "cx:" + hex.EncodeToString(b)
}

// pilotAccount returns the account that serves the pilots of connector's user: the linked one, or
// a new unclaimed one. Its probation starts with its first node or pilot (FirstNode).
func (s *AccountStore) pilotAccount(connector, user string, now time.Time) (id string, unclaimed bool, err error) {
	s.mu.Lock()
	if a := s.f.linkOwner(connector, user); a != nil && a.FirstNode != nil {
		id, unclaimed = a.ID, a.Status == StatusUnclaimed
		s.mu.Unlock()
		return id, unclaimed, nil
	}
	s.mu.Unlock()
	err = s.Update(func(f *AccountsFile) error {
		t := now.UTC()
		a := f.linkOwner(connector, user)
		if a == nil {
			id := newUnclaimedID()
			for f.find(id) != nil {
				id = newUnclaimedID()
			}
			a = &Account{ID: id, Status: StatusUnclaimed, Created: t, Links: []AccountLink{{Connector: connector, User: user, Linked: t}}}
			f.Accounts = append(f.Accounts, a)
		}
		if a.FirstNode == nil {
			a.FirstNode = &t
		}
		id, unclaimed = a.ID, a.Status == StatusUnclaimed
		return nil
	})
	return id, unclaimed, err
}

// canLink: the statuses whose accounts may link a connector's user and run its pilots (owner,
// 2026-11-19: waitlisted ones too; the waitlist blocks only their own chat, API use and keys).
// Banned accounts may not.
func canLink(status string) bool { return status == StatusActive || status == StatusWaitlisted }

// runsPilots: the statuses whose accounts' pilots may serve (canLink, and unclaimed accounts).
func runsPilots(status string) bool { return canLink(status) || status == StatusUnclaimed }

// pilotAccountState says whether account id may run pilots of connector's user now (runsPilots, and
// still linked to that user: an unlinked or relinked user's pilots stop), when its
// probation started, and whether its "chat on pilots" switch is off.
func (s *AccountStore) pilotAccountState(id, connector, user string) (first time.Time, chatOff, live bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a := s.f.find(id)
	if a == nil || !runsPilots(a.Status) || s.f.linkOwner(connector, user) != a {
		return time.Time{}, false, false
	}
	if a.FirstNode != nil {
		first = *a.FirstNode
	}
	return first, a.PilotChatOff, true
}

// SetPilotChat turns an account's "chat on pilots" switch on or off.
func (s *AccountStore) SetPilotChat(id string, on bool) error {
	return s.Update(func(f *AccountsFile) error {
		a := f.find(id)
		if a == nil || !runsPilots(a.Status) {
			return fmt.Errorf("no active, waitlisted or unclaimed account %s", id)
		}
		a.PilotChatOff = !on
		return nil
	})
}

var errLinkTaken = errors.New("that connector user is linked to another account")

// Link links account id to connector's user (the operator, `accounts link`). A user may be
// linked to one account only; an unclaimed account holding it is claimed from the account page instead
// (claimLink merges it).
func (s *AccountStore) Link(id, connector, user string, now time.Time) error {
	if !extIDRe.MatchString(user) || !projectID.MatchString(connector) {
		return errors.New("connector: lower-case letters, digits, dashes; user: 1-64 letters, digits, '.', '_', ':' or '-'")
	}
	return s.Update(func(f *AccountsFile) error {
		a := f.find(id)
		if a == nil {
			return fmt.Errorf("no account %s", id)
		}
		if o := f.linkOwner(connector, user); o != nil {
			if o == a {
				return nil
			}
			return errLinkTaken
		}
		a.Links = append(a.Links, AccountLink{Connector: connector, User: user, Linked: now.UTC()})
		return nil
	})
}

// Unlink removes a link (operator).
func (s *AccountStore) Unlink(id, connector, user string) error {
	return s.Update(func(f *AccountsFile) error {
		a := f.find(id)
		if a == nil {
			return fmt.Errorf("no account %s", id)
		}
		for i, l := range a.Links {
			if l.Connector == connector && l.User == user {
				a.Links = append(a.Links[:i], a.Links[i+1:]...)
				return nil
			}
		}
		return fmt.Errorf("%s has no link to %s user %s", id, connector, user)
	})
}

var errClaimInactive = errors.New("a closed account cannot link a connector's user")

// claimLink links connector's user to account target after the connector vouched for it (it made
// the user for target, or target proved it with the attach key). The rules:
//   - target must be active or waitlisted (canLink: not banned or unclaimed), unless the user is linked to it
//     already (a link finished before a crash, then a ban: its records still follow);
//   - a user linked to target already keeps its link (with the new attach key);
//   - a user linked to an unclaimed account moves, and that account is merged into target: all its
//     links move, target's probation starts at the earlier of the two first-node times, its chat
//     switch is off if either was off, its operator terms carry over (mergeOperatorTerms), and the
//     unclaimed account is deleted. Its id is returned so
//     that the pilot records and their daily credit follow (scienceDB.completeRequest);
//   - a user linked to any other account is not taken (errLinkTaken): the operator unlinks it first.
//
// Moving the link stops that user's connected pilots (pilotAccountState: the user is no longer the
// old account's); they reconnect as target's once their records point there.
func (s *AccountStore) claimLink(target, connector, user string, link AccountLink, now time.Time) (from string, err error) {
	err = s.Update(func(f *AccountsFile) error {
		from = ""
		t := f.find(target)
		o := f.linkOwner(connector, user)
		if t == nil || (!canLink(t.Status) && o != t) {
			return errClaimInactive // linked already (a retry after a crash) is done whatever the status
		}
		link.Connector, link.User = connector, user
		switch {
		case o == t: // linked already: a fresh key; how it came stays
			for i := range t.Links {
				if l := &t.Links[i]; l.Connector == connector && l.User == user {
					if link.AttachKey != "" {
						l.AttachKey = link.AttachKey
					}
					if l.Via == "" || link.Via == LinkClaimed {
						// A claim with the account key: the person manages that user at the connector
						// now (maybe with their own address), so the page stops showing a made-up login.
						l.Via, l.OwnEmail = link.Via, link.OwnEmail
					}
				}
			}
			return nil
		case o == nil:
			link.Linked = now.UTC()
			t.Links = append(t.Links, link)
			return nil
		case o.Status != StatusUnclaimed:
			return errLinkTaken
		}
		for _, l := range o.Links {
			if l.Connector == connector && l.User == user {
				link.Linked = now.UTC()
				l = link
			}
			t.Links = append(t.Links, l)
		}
		if o.FirstNode != nil && (t.FirstNode == nil || o.FirstNode.Before(*t.FirstNode)) {
			first := *o.FirstNode
			t.FirstNode = &first
		}
		t.PilotChatOff = t.PilotChatOff || o.PilotChatOff
		mergeOperatorTerms(t, o)
		f.moveCheckHolds(o.ID, t.ID) // a held unclaimed account stays held as the claimant (account_holds.go)
		f.Accounts = slices.DeleteFunc(f.Accounts, func(a *Account) bool { return a == o })
		from = o.ID
		return nil
	})
	if err != nil {
		from = ""
	}
	return from, err
}

// mergeOperatorTerms carries an unclaimed account's operator terms (chatrules.go) into t, which claims
// it: the newer acceptance, the later withdrawal, and no acceptance if a withdrawal came after it (the
// latest event of the person's, on either account, stands).
func mergeOperatorTerms(t, o *Account) {
	if o.OperatorTerms != nil && (t.OperatorTerms == nil || o.OperatorTerms.At.After(t.OperatorTerms.At)) {
		acc := *o.OperatorTerms
		t.OperatorTerms = &acc
	}
	if o.TermsWithdrawn != nil && (t.TermsWithdrawn == nil || o.TermsWithdrawn.After(*t.TermsWithdrawn)) {
		w := *o.TermsWithdrawn
		t.TermsWithdrawn = &w
	}
	if t.OperatorTerms != nil && t.TermsWithdrawn != nil && !t.OperatorTerms.At.After(*t.TermsWithdrawn) {
		t.OperatorTerms = nil
	}
}

// linkedWithKey reports whether account id holds connector's user with attach key key (a repeated
// answer to a request that linked it).
func (s *AccountStore) linkedWithKey(id, connector, user, key string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	a := s.f.linkOwner(connector, user)
	if a == nil || a.ID != id {
		return false
	}
	for _, l := range a.Links {
		if l.Connector == connector && l.User == user {
			return key == "" || l.AttachKey == key
		}
	}
	return false
}

// linkHolder returns the id and status of the account linked to connector's user, if any.
func (s *AccountStore) linkHolder(connector, user string) (id, status string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if a := s.f.linkOwner(connector, user); a != nil {
		return a.ID, a.Status
	}
	return "", ""
}

// setLinkOwnEmail records that account id's link to connector's user now has the person's own
// address at the connector (a set_email request done). A link that is gone is no error.
func (s *AccountStore) setLinkOwnEmail(id, connector, user string) error {
	return s.Update(func(f *AccountsFile) error {
		if a := f.find(id); a != nil {
			for i := range a.Links {
				if l := &a.Links[i]; l.Connector == connector && l.User == user {
					l.OwnEmail = true
				}
			}
		}
		return nil
	})
}

// handleAccountSettings serves GET and PATCH /v1/account/settings for an account's own API key:
// {"chat_on_pilots": bool}, the switch for its connector pilots (the account page has it too: handlePilotChatForm).
func (g *Gateway) handleAccountSettings(w http.ResponseWriter, r *http.Request) {
	_, acct, ok := g.authenticate(r)
	if !ok || acct == "" {
		apiError(w, http.StatusUnauthorized, "an account's API key is needed")
		return
	}
	if r.Method == http.MethodPatch {
		var body struct {
			ChatOnPilots *bool `json:"chat_on_pilots"`
		}
		if !decodeTaskBody(w, r, 1<<10, &body) {
			return
		}
		if body.ChatOnPilots == nil {
			apiError(w, http.StatusBadRequest, "chat_on_pilots: true or false")
			return
		}
		if err := g.accounts.SetPilotChat(acct, *body.ChatOnPilots); err != nil {
			apiError(w, http.StatusConflict, err.Error())
			return
		}
	}
	a, _, ok := g.accounts.Get(acct)
	if !ok {
		apiError(w, http.StatusUnauthorized, "an account's API key is needed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"chat_on_pilots": !a.PilotChatOff})
}
