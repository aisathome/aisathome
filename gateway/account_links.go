// Accounts and connectors (decision 0020, addendum 2026-11-17): every user a connector names (a BOINC
// user id) maps to one account here. An account linked to that user serves its pilots; without one,
// the first pilot of that user creates an unclaimed account keyed by a link to it, which its person
// claims later (the claim and linking from the account page come with the account work, map PR 4b;
// the operator links by hand with `accounts link`). The link is an id, never an address.
//
// SPDX-License-Identifier: Apache-2.0
package gateway

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
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

// pilotAccountState says whether account id may run pilots now (active or unclaimed), when its
// probation started, and whether its "chat on pilots" switch is off.
func (s *AccountStore) pilotAccountState(id string) (first time.Time, chatOff, live bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a := s.f.find(id)
	if a == nil || (a.Status != StatusActive && a.Status != StatusUnclaimed) {
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
		if a == nil || (a.Status != StatusActive && a.Status != StatusUnclaimed) {
			return fmt.Errorf("no active or unclaimed account %s", id)
		}
		a.PilotChatOff = !on
		return nil
	})
}

var errLinkTaken = errors.New("that connector user is linked to another account")

// Link links account id to connector's user (operator: until the claim flow exists). A user may be
// linked to one account only; an unclaimed account holding it must be claimed instead.
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

// handleAccountSettings serves GET and PATCH /v1/account/settings for an account's own API key:
// {"chat_on_pilots": bool}, the switch for its connector pilots (the account page gets it later).
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
