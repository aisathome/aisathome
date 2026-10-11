// Usernames (owner, 2026-10-11): like the name of a BOINC account. Setting one is the
// consent to appear publicly, with one's credit, in the leaderboard and in the BOINC-format
// statistics; without one an account counts in totals only and never appears by name or id.
// Clearing it takes the account out of the next leaderboard and export.
//
// SPDX-License-Identifier: Apache-2.0
package gateway

import (
	"errors"
	"regexp"
	"strings"
	"unicode"
)

const (
	minUsername = 3
	maxUsername = 32
)

// reservedNames cannot be taken (compared case-insensitively).
var reservedNames = []string{"anonymous", "admin", "administrator", "maintainer", "moderator", "operator",
	"aisathome", "ais at home", "root", "system", "staff", "support", "boinc", "gridcoin"}

// domainLike catches web addresses written without a scheme ("example.com", "x.y.org").
var domainLike = regexp.MustCompile(`(?i)[\p{L}\p{N}-]+\.\p{L}{2,}`)

var (
	errUsernameLength = errors.New("A username has 3 to 32 characters.")
	errUsernameChars  = errors.New("A username may use letters, digits, spaces and - _ . ' only.")
	errUsernameURL    = errors.New("A username cannot be a web address.")
	errUsernameTaken  = errors.New("That username is taken. Please choose another.")
)

// cleanUsername trims and collapses spaces and checks the rules; "" means no username.
func cleanUsername(s string) (string, error) {
	s = strings.Join(strings.FieldsFunc(s, func(r rune) bool { return r == ' ' }), " ")
	if s == "" {
		return "", nil
	}
	n := 0
	for _, r := range s {
		n++
		switch {
		case unicode.IsLetter(r), unicode.IsDigit(r), unicode.IsMark(r) && n > 1:
		case r == ' ', r == '-', r == '_', r == '.', r == '\'':
		default:
			return "", errUsernameChars // also control and format characters, '@', '/', ':'
		}
	}
	if n < minUsername || n > maxUsername {
		return "", errUsernameLength
	}
	low := strings.ToLower(s)
	if domainLike.MatchString(s) || strings.Contains(low, "www") && strings.Contains(low, ".") {
		return "", errUsernameURL
	}
	for _, r := range reservedNames {
		if strings.EqualFold(s, r) {
			return "", errUsernameTaken
		}
	}
	return s, nil
}

// SetUsername sets (or with "" clears) an account's username: unique case-insensitively
// among all accounts, banned ones included, so a banned name cannot be taken over.
func (s *AccountStore) SetUsername(id, name string) error {
	name, err := cleanUsername(name)
	if err != nil {
		return err
	}
	return s.Update(func(f *AccountsFile) error {
		a := f.find(id)
		if a == nil || a.Status != StatusActive {
			return errors.New("only active accounts can set a username")
		}
		if name != "" {
			for _, o := range f.Accounts {
				if o.ID != id && strings.EqualFold(o.Username, name) {
					return errUsernameTaken
				}
			}
		}
		a.Username = name
		return nil
	})
}

// ClearUsername removes an account's username (operator: an offensive or impersonating name).
func (s *AccountStore) ClearUsername(id string) error {
	return s.Update(func(f *AccountsFile) error {
		a := f.find(id)
		if a == nil {
			return errors.New("no such account")
		}
		a.Username = ""
		return nil
	})
}

// publicNames maps active accounts that have a username to it.
func (s *AccountStore) publicNames() map[string]publicAccount {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[string]publicAccount{}
	for _, a := range s.f.Accounts {
		if a.Status == StatusActive && a.Username != "" {
			pa := publicAccount{Name: a.Username, Created: a.Created.Unix(), Nodes: map[string]int64{}}
			for _, n := range a.Nodes {
				pa.Nodes[n.ID] = n.Created.Unix()
			}
			out[a.ID] = pa
		}
	}
	return out
}

type publicAccount struct {
	Name    string
	Created int64
	Nodes   map[string]int64 // node id → created (Unix), for nodes that still have a token
}
