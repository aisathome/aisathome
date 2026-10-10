// Accounts: self-service sign-in with GitHub, Google or email, API keys and a waitlist
// (research decision 0006). The gateway stores only the provider's user id: GitHub's numeric
// id ("gh:<id>") or Google's account identifier ("google:<sub>"). Email accounts get a random
// id ("em:<hex>") and keep only a keyed hash of the address (email.go). No address, no name,
// no password, no token. Accounts beyond the operator's limit wait in a first-come waitlist
// and are let in automatically when room appears.
//
// SPDX-License-Identifier: Apache-2.0
package gateway

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// AccountsConfig enables accounts. Everything about who may use the network lives in the
// accounts file, which the operator changes with `aisathome-gateway accounts ...` while the
// gateway runs.
type AccountsConfig struct {
	File               string       `json:"file"`                       // accounts and key hashes (writable volume)
	PublicURL          string       `json:"public_url"`                 // e.g. https://aisathome.org
	CookieDomain       string       `json:"cookie_domain"`              // e.g. aisathome.org, so the chat sees the session
	SessionSecret      string       `json:"session_secret"`             // at least 32 characters; signs session cookies
	GitHubClientID     string       `json:"github_client_id,omitempty"` // GitHub sign-in: both or neither
	GitHubClientSecret string       `json:"github_client_secret,omitempty"`
	GoogleClientID     string       `json:"google_client_id,omitempty"` // Google sign-in: both or neither
	GoogleClientSecret string       `json:"google_client_secret,omitempty"`
	Email              *EmailConfig `json:"email,omitempty"`     // email sign-in (one-time code and link)
	ChatURL            string       `json:"chat_url,omitempty"`  // where "next=chat" leads after sign-in
	Limits             UserLimits   `json:"limits"`              // per account: rpm, concurrent, tokens_per_day
	MinTrust           string       `json:"min_trust,omitempty"` // default "trusted"
	MaxKeys            int          `json:"max_keys,omitempty"`  // API keys per account; default 3
	Admins             []string     `json:"admins,omitempty"`    // account ids ("gh:123", "google:<sub>", "em:<hex>") that are chat admins

	// GitHub and Google endpoints; tests point them at fakes.
	GitHubAuthURL   string `json:"-"`
	GitHubTokenURL  string `json:"-"`
	GitHubUserURL   string `json:"-"`
	GoogleAuthURL   string `json:"-"`
	GoogleTokenURL  string `json:"-"`
	GoogleCertsURL  string `json:"-"` // Google's signing keys (JWKS)
	GoogleIssuerURL string `json:"-"` // the "iss" ID tokens must carry
}

// githubOn and googleOn report whether a sign-in provider is configured.
func (c *AccountsConfig) githubOn() bool { return c.GitHubClientID != "" && c.GitHubClientSecret != "" }
func (c *AccountsConfig) googleOn() bool { return c.GoogleClientID != "" && c.GoogleClientSecret != "" }

func (c *AccountsConfig) defaults() error {
	if c.File == "" || c.PublicURL == "" {
		return errors.New("accounts: file and public_url are required")
	}
	// Half a provider is a typo, not a choice: say so instead of hiding its button.
	if (c.GitHubClientID == "") != (c.GitHubClientSecret == "") {
		return errors.New("accounts: set both github_client_id and github_client_secret, or neither")
	}
	if (c.GoogleClientID == "") != (c.GoogleClientSecret == "") {
		return errors.New("accounts: set both google_client_id and google_client_secret, or neither")
	}
	if c.Email != nil {
		if err := c.Email.check(); err != nil {
			return err
		}
	}
	if !c.githubOn() && !c.googleOn() && c.Email == nil {
		return errors.New("accounts: configure GitHub, Google or email sign-in")
	}
	if len(c.SessionSecret) < 32 {
		return errors.New("accounts: session_secret must be at least 32 characters")
	}
	c.PublicURL = strings.TrimRight(c.PublicURL, "/")
	if c.MinTrust == "" {
		c.MinTrust = "trusted"
	}
	if c.MaxKeys == 0 {
		c.MaxKeys = 3
	}
	if c.GitHubAuthURL == "" {
		c.GitHubAuthURL = "https://github.com/login/oauth/authorize"
	}
	if c.GitHubTokenURL == "" {
		c.GitHubTokenURL = "https://github.com/login/oauth/access_token"
	}
	if c.GitHubUserURL == "" {
		c.GitHubUserURL = "https://api.github.com/user"
	}
	if c.GoogleAuthURL == "" {
		c.GoogleAuthURL = "https://accounts.google.com/o/oauth2/v2/auth"
	}
	if c.GoogleTokenURL == "" {
		c.GoogleTokenURL = "https://oauth2.googleapis.com/token"
	}
	if c.GoogleCertsURL == "" {
		c.GoogleCertsURL = "https://www.googleapis.com/oauth2/v3/certs"
	}
	if c.GoogleIssuerURL == "" {
		c.GoogleIssuerURL = "https://accounts.google.com"
	}
	return nil
}

// Account states.
const (
	StatusActive     = "active"
	StatusWaitlisted = "waitlisted"
	StatusBanned     = "banned"
)

// DefaultMaxActive is the limit a new accounts file starts with (owner, 2026-10-10).
const DefaultMaxActive = 100

// AccountSettings are the operator's switches, kept in the accounts file.
type AccountSettings struct {
	MaxActive int  `json:"max_active"`       // active accounts at most; the rest wait
	Closed    bool `json:"closed,omitempty"` // no new accounts at all ("raid mode")
}

// Account is one user.
type Account struct {
	ID        string       `json:"id"` // "gh:<numeric GitHub id>", "google:<sub>" or "em:<random hex>"
	Status    string       `json:"status"`
	Created   time.Time    `json:"created"`
	Activated *time.Time   `json:"activated,omitempty"`
	Held      bool         `json:"held,omitempty"`    // put back on the waitlist by the operator; not let in automatically
	KeySeq    int          `json:"key_seq,omitempty"` // counts created keys; the "create a key" form is tied to it
	Keys      []AccountKey `json:"keys,omitempty"`
	EmailMACs []EmailMAC   `json:"email_macs,omitempty"` // email accounts: the address as a keyed hash
}

// EmailMAC is an email address as HMAC-SHA256 (hex) under the email key with KeyID.
type EmailMAC struct {
	KeyID string `json:"key_id"`
	MAC   string `json:"mac"`
}

// AccountKey is an API key of an account; only its hash is stored.
type AccountKey struct {
	Hash    string    `json:"sha256"`
	Prefix  string    `json:"prefix"`
	Created time.Time `json:"created"`
}

// AccountsFile is the accounts file's format.
type AccountsFile struct {
	Settings AccountSettings `json:"settings"`
	Accounts []*Account      `json:"accounts"`
}

// promote lets waiting accounts in, oldest first, while there is room.
func (f *AccountsFile) promote(now time.Time) {
	active := 0
	var waiting []*Account
	for _, a := range f.Accounts {
		switch a.Status {
		case StatusActive:
			active++
		case StatusWaitlisted:
			if !a.Held {
				waiting = append(waiting, a)
			}
		}
	}
	sort.SliceStable(waiting, func(i, j int) bool { return waiting[i].Created.Before(waiting[j].Created) })
	for _, a := range waiting {
		if active >= f.Settings.MaxActive {
			return
		}
		t := now
		a.Status, a.Activated = StatusActive, &t
		active++
	}
}

// Find returns the account with id, or nil.
func (f *AccountsFile) Find(id string) *Account { return f.find(id) }

// find returns the account with id, or nil.
func (f *AccountsFile) find(id string) *Account {
	for _, a := range f.Accounts {
		if a.ID == id {
			return a
		}
	}
	return nil
}

// position is a waiting account's place in the queue (1 = next), or 0.
func (f *AccountsFile) position(id string) int {
	var waiting []*Account
	for _, a := range f.Accounts {
		if a.Status == StatusWaitlisted {
			waiting = append(waiting, a)
		}
	}
	sort.SliceStable(waiting, func(i, j int) bool { return waiting[i].Created.Before(waiting[j].Created) })
	for i, a := range waiting {
		if a.ID == id {
			return i + 1
		}
	}
	return 0
}

// AccountStore is the accounts file with a lock: the gateway and the operator's CLI both
// change it, so every change locks the file, rereads it, applies itself and writes it back.
type AccountStore struct {
	path string

	mu    sync.Mutex
	f     AccountsFile
	sum   [32]byte
	byKey map[string]*Account // key hash → account
}

// OpenAccountStore opens (or starts) the accounts file at path.
func OpenAccountStore(path string) (*AccountStore, error) {
	s := &AccountStore{path: path}
	if err := s.Update(func(*AccountsFile) error { return nil }); err != nil {
		return nil, err
	}
	return s, nil
}

func readAccounts(path string) (AccountsFile, [32]byte, error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return AccountsFile{Settings: AccountSettings{MaxActive: DefaultMaxActive}}, [32]byte{}, nil
	}
	if err != nil {
		return AccountsFile{}, [32]byte{}, err
	}
	var f AccountsFile
	if err := json.Unmarshal(raw, &f); err != nil {
		return AccountsFile{}, [32]byte{}, fmt.Errorf("accounts file %s: %w", path, err)
	}
	return f, sha256.Sum256(raw), nil
}

// Update changes the accounts file under its lock and lets waiting accounts in afterwards.
func (s *AccountStore) Update(fn func(*AccountsFile) error) error {
	unlock, err := lockFile(s.path + ".lock")
	if err != nil {
		return err
	}
	defer unlock()
	f, _, err := readAccounts(s.path)
	if err != nil {
		return err
	}
	if err := fn(&f); err != nil {
		return err
	}
	f.promote(time.Now().UTC())
	raw, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, append(raw, '\n'), 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, s.path); err != nil {
		return err
	}
	s.apply(f, sha256.Sum256(append(raw, '\n')))
	return nil
}

// Refresh rereads the file if someone else (the CLI) changed it. A broken file keeps the
// accounts in use.
func (s *AccountStore) Refresh() error {
	unlock, err := lockFile(s.path + ".lock") // never apply a read that an Update overtook
	if err != nil {
		return err
	}
	defer unlock()
	f, sum, err := readAccounts(s.path)
	if err != nil {
		return err
	}
	s.mu.Lock()
	same := sum == s.sum
	s.mu.Unlock()
	if !same {
		s.apply(f, sum)
	}
	return nil
}

func (s *AccountStore) apply(f AccountsFile, sum [32]byte) {
	byKey := map[string]*Account{}
	for _, a := range f.Accounts {
		for _, k := range a.Keys {
			byKey[k.Hash] = a
		}
	}
	s.mu.Lock()
	s.f, s.sum, s.byKey = f, sum, byKey
	s.mu.Unlock()
}

// Get returns a copy of the account with id, and its waitlist position.
func (s *AccountStore) Get(id string) (Account, int, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a := s.f.find(id)
	if a == nil {
		return Account{}, 0, false
	}
	return *a, s.f.position(id), true
}

// Snapshot returns a copy of the whole file (for the CLI and tests).
func (s *AccountStore) Snapshot() AccountsFile {
	s.mu.Lock()
	defer s.mu.Unlock()
	raw, _ := json.Marshal(s.f)
	var f AccountsFile
	json.Unmarshal(raw, &f)
	return f
}

// keyAccount returns the active account that owns key.
func (s *AccountStore) keyAccount(key string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a := s.byKey[keyHash(key)]
	if a == nil || a.Status != StatusActive {
		return "", false
	}
	return a.ID, true
}

// active reports whether id is an active account.
func (s *AccountStore) active(id string) bool {
	a, _, ok := s.Get(id)
	return ok && a.Status == StatusActive
}

// SignIn returns the account for id, creating it (waitlisted, then let in if there is room)
// unless sign-up is closed.
func (s *AccountStore) SignIn(id string, now time.Time) (Account, error) {
	err := s.Update(func(f *AccountsFile) error {
		if f.find(id) != nil {
			return nil
		}
		if f.Settings.Closed {
			return errSignupClosed
		}
		f.Accounts = append(f.Accounts, &Account{ID: id, Status: StatusWaitlisted, Created: now.UTC()})
		return nil
	})
	if err != nil {
		return Account{}, err
	}
	a, _, _ := s.Get(id)
	return a, nil
}

// SignInEmail returns the email account whose stored hash matches one of macs (the address
// under each key of the ring, current key first), creating one with a random id unless
// sign-up is closed. A match under an older key is rewritten under the current key, so a key
// can be retired once every active account has signed in since.
func (s *AccountStore) SignInEmail(macs []EmailMAC, now time.Time) (Account, error) {
	if len(macs) == 0 {
		return Account{}, errors.New("no email hash")
	}
	var id string
	err := s.Update(func(f *AccountsFile) error {
		for _, a := range f.Accounts {
			if emailMatch(a.EmailMACs, macs) {
				id = a.ID
				if len(a.EmailMACs) != 1 || a.EmailMACs[0] != macs[0] {
					a.EmailMACs = []EmailMAC{macs[0]}
				}
				return nil
			}
		}
		if f.Settings.Closed {
			return errSignupClosed
		}
		for id == "" || f.find(id) != nil {
			id = newEmailAccountID()
		}
		f.Accounts = append(f.Accounts, &Account{ID: id, Status: StatusWaitlisted, Created: now.UTC(),
			EmailMACs: []EmailMAC{macs[0]}})
		return nil
	})
	if err != nil {
		return Account{}, err
	}
	a, _, _ := s.Get(id)
	return a, nil
}

func emailMatch(stored, macs []EmailMAC) bool {
	for _, s := range stored {
		for _, m := range macs {
			if s.KeyID == m.KeyID && hmac.Equal([]byte(s.MAC), []byte(m.MAC)) {
				return true
			}
		}
	}
	return false
}

// newEmailAccountID is "em:" and 16 random bytes in lowercase hex. It is never derived from
// the address, and lowercase because the chat lowercases the addresses it is given.
func newEmailAccountID() string {
	b := make([]byte, 16)
	rand.Read(b)
	return "em:" + hex.EncodeToString(b)
}

var (
	errSignupClosed = errors.New("sign-up is closed for now")
	errStaleForm    = errors.New("this form was already used; reload the page")
)

// AddKey creates an API key for an active account and returns it (shown once). valid checks
// the form's one-time token against the account's KeySeq inside the file lock, and KeySeq
// then goes up, so two concurrent posts of one form cannot both create a key, and revoking a
// key never makes an old form valid again. nil skips the check (CLI, tests).
func (s *AccountStore) AddKey(id string, max int, now time.Time, valid func(seq int) bool) (string, error) {
	key, err := newAPIKey()
	if err != nil {
		return "", err
	}
	err = s.Update(func(f *AccountsFile) error {
		a := f.find(id)
		switch {
		case a == nil || a.Status != StatusActive:
			return errors.New("only active accounts can create keys")
		case len(a.Keys) >= max:
			return fmt.Errorf("at most %d keys; revoke one first", max)
		case valid != nil && !valid(a.KeySeq):
			return errStaleForm
		}
		a.KeySeq++
		a.Keys = append(a.Keys, AccountKey{Hash: keyHash(key), Prefix: key[:keyPrefixLen], Created: now.UTC()})
		return nil
	})
	return key, err
}

// RevokeKey removes the account's key with prefix.
func (s *AccountStore) RevokeKey(id, prefix string) error {
	return s.Update(func(f *AccountsFile) error {
		a := f.find(id)
		if a == nil {
			return errors.New("no such account")
		}
		for i, k := range a.Keys {
			if k.Prefix == prefix {
				a.Keys = append(a.Keys[:i], a.Keys[i+1:]...)
				return nil
			}
		}
		return errors.New("no such key")
	})
}

// SetStatus activates, waitlists or bans an account (operator). A banned account keeps its
// record, so signing in again does not create a fresh one; its keys stop working.
func (s *AccountStore) SetStatus(id, status string) error {
	return s.Update(func(f *AccountsFile) error {
		a := f.find(id)
		if a == nil {
			return fmt.Errorf("no account %s", id)
		}
		if status == StatusActive && a.Status != StatusActive {
			t := time.Now().UTC()
			a.Activated = &t
		}
		a.Status, a.Held = status, status == StatusWaitlisted // only the operator waitlists an account by hand
		if status == StatusBanned {
			a.Keys = nil
		}
		return nil
	})
}

// newAPIKey returns an "ak-" key, the form the gateway's redaction recognises.
func newAPIKey() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return "ak-" + base64.RawURLEncoding.EncodeToString(b), nil
}

// ---------------------------------------------------------------- sessions

const (
	sessionCookie = "aisathome_session"
	sessionTTL    = 30 * 24 * time.Hour
)

// signSession returns a cookie value for id that expires at exp: payload.mac, both base64.
func signSession(secret, id string, exp time.Time) string {
	payload := id + "|" + strconv.FormatInt(exp.Unix(), 10)
	m := hmac.New(sha256.New, []byte(secret))
	m.Write([]byte(payload))
	return base64.RawURLEncoding.EncodeToString([]byte(payload)) + "." +
		base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}

// verifySession returns the account id in a valid, unexpired cookie value.
func verifySession(secret, v string, now time.Time) (string, bool) {
	p, mac, ok := strings.Cut(v, ".")
	if !ok {
		return "", false
	}
	strict := base64.RawURLEncoding.Strict() // one encoding per value: no variants in unused bits
	payload, err1 := strict.DecodeString(p)
	sum, err2 := strict.DecodeString(mac)
	if err1 != nil || err2 != nil {
		return "", false
	}
	m := hmac.New(sha256.New, []byte(secret))
	m.Write(payload)
	if !hmac.Equal(sum, m.Sum(nil)) {
		return "", false
	}
	id, expS, ok := strings.Cut(string(payload), "|")
	exp, err := strconv.ParseInt(expS, 10, 64)
	if !ok || err != nil || now.Unix() >= exp {
		return "", false
	}
	return id, true
}

// accountInfo is the KeyInfo of an account's requests (API key or chat).
func (g *Gateway) accountInfo(id string) KeyInfo {
	l := g.acfg.Limits
	return KeyInfo{User: g.pseudonym(id), RPM: l.RPM, MaxConcurrent: l.MaxConcurrent, TokensPerDay: l.TokensPerDay,
		MinTrust: g.acfg.MinTrust}
}

// pseudonym names an account in the usage log without its GitHub, Google or email id; with
// the session secret the operator can still map an account to its records (e.g. to delete
// them).
func (g *Gateway) pseudonym(id string) string {
	m := hmac.New(sha256.New, []byte(g.acfg.SessionSecret))
	m.Write([]byte("usage|" + id))
	return "acct-" + base64.RawURLEncoding.EncodeToString(m.Sum(nil)[:12])
}
