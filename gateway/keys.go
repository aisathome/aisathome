// SPDX-License-Identifier: Apache-2.0

package gateway

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// KeyInfo is what the gateway needs to know about an API key. Everything else about users
// (accounts, billing, websites) belongs to the operator's own services, outside the gateway.
type KeyInfo struct {
	User          string `json:"user"`
	RPM           int    `json:"rpm,omitempty"`            // requests per minute; 0 = unlimited
	MaxConcurrent int    `json:"concurrent,omitempty"`     // requests in flight; 0 = unlimited
	TokensPerDay  int    `json:"tokens_per_day,omitempty"` // weighted tokens per UTC day; 0 = unlimited
	// MinTrust "trusted" sends this key's requests only to nodes whose token is trusted
	// (user chats should not reach unknown volunteers).
	MinTrust string `json:"min_trust,omitempty"`
	// OnlyOperator restricts this key to nodes of one operator ("use only my own nodes").
	OnlyOperator string `json:"only_operator,omitempty"`
	// ForwardUsers marks a trusted front end (Open WebUI): its X-OpenWebUI-User-Id header
	// identifies the end user, who then gets UserLimits. Ignored for all other keys.
	ForwardUsers bool        `json:"forward_users,omitempty"`
	UserLimits   *UserLimits `json:"user_limits,omitempty"`
	// RestoreSecrets puts redacted secrets back into answer text (never into tool arguments).
	// Off by default: a malicious node can place a placeholder where the client fetches or
	// runs it (an image URL, a command written as text), and no context check catches every
	// form. Enable only for clients that render no remote content and execute nothing.
	RestoreSecrets bool `json:"restore_secrets,omitempty"`
	// VolunteerAgents lets this key's requests with tools (agents) reach volunteer nodes past
	// their probation; without it they go only to trusted nodes (decision 0007 addendum).
	// Never carried over to end users behind a front end.
	VolunteerAgents bool `json:"volunteer_agents,omitempty"`
}

// UserLimits apply to each end user behind a ForwardUsers key.
type UserLimits struct {
	RPM           int `json:"rpm,omitempty"`
	MaxConcurrent int `json:"concurrent,omitempty"`
	TokensPerDay  int `json:"tokens_per_day,omitempty"`
}

// forUser is the account of one end user behind a ForwardUsers key. The user id is the
// front end's pseudonymous id; names and email addresses are never used.
func (k KeyInfo) forUser(uid string) KeyInfo {
	u := KeyInfo{User: k.User + "/" + uid, MinTrust: k.MinTrust, OnlyOperator: k.OnlyOperator,
		RestoreSecrets: k.RestoreSecrets}
	if k.UserLimits != nil {
		u.RPM, u.MaxConcurrent, u.TokensPerDay = k.UserLimits.RPM, k.UserLimits.MaxConcurrent, k.UserLimits.TokensPerDay
	}
	return u
}

// within keeps the front-end key's routing restrictions (trusted nodes only, one operator's
// nodes only) for an end user's own policy: a front end restricted to some nodes stays so.
func (k KeyInfo) within(front KeyInfo) KeyInfo {
	if front.MinTrust == "trusted" {
		k.MinTrust = "trusted"
	}
	if front.OnlyOperator != "" {
		k.OnlyOperator = front.OnlyOperator
	}
	return k
}

// KeyStore resolves API keys. Operators plug in their own (static file, HTTP call to an
// accounts service, database). It is the only boundary between the open gateway and an
// operator's private business layer.
type KeyStore interface {
	Lookup(ctx context.Context, key string) (KeyInfo, bool)
}

// StaticKeys is a KeyStore backed by the gateway config.
type StaticKeys map[string]KeyInfo

// Lookup implements KeyStore.
func (s StaticKeys) Lookup(_ context.Context, key string) (KeyInfo, bool) {
	k, ok := s[key]
	return k, ok
}

// limiter enforces per-key RPM (token bucket) and concurrency.
type limiter struct {
	mu      sync.Mutex
	buckets map[string]*bucket
	swept   time.Time
}

type bucket struct {
	tokens   float64
	last     time.Time
	inFlight int
}

func newLimiter() *limiter { return &limiter{buckets: map[string]*bucket{}} }

// idleAfter: a bucket unused this long has refilled completely, so dropping it is the same
// as keeping it. Without this the map grows by one entry per key or end user, forever.
const idleAfter = time.Minute

// acquire reports whether the request may start, and returns a release function.
func (l *limiter) acquire(key string, k KeyInfo, now time.Time) (release func(), reason string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if now.Sub(l.swept) >= idleAfter {
		for id, b := range l.buckets {
			if b.inFlight == 0 && now.Sub(b.last) >= idleAfter {
				delete(l.buckets, id)
			}
		}
		l.swept = now
	}
	b := l.buckets[key]
	if b == nil {
		b = &bucket{tokens: float64(k.RPM), last: now}
		l.buckets[key] = b
	}
	if k.RPM > 0 {
		b.tokens += now.Sub(b.last).Minutes() * float64(k.RPM)
		if b.tokens > float64(k.RPM) {
			b.tokens = float64(k.RPM)
		}
	}
	b.last = now // also marks the bucket as in use for pruning
	if k.RPM > 0 && b.tokens < 1 {
		return nil, "rate limit exceeded"
	}
	if k.MaxConcurrent > 0 && b.inFlight >= k.MaxConcurrent {
		return nil, "too many concurrent requests"
	}
	if k.RPM > 0 {
		b.tokens--
	}
	b.inFlight++
	return func() {
		l.mu.Lock()
		b.inFlight--
		l.mu.Unlock()
	}, ""
}

// budgets track weighted tokens per key and UTC day. Entries are keyed by a hash of the API
// key (plus end user), so the state file on disk never holds keys. With a path they are
// saved there and survive restarts; otherwise a restart resets the day.
type budgets struct {
	mu     sync.Mutex
	used   map[string]*dayUse // key hash → today's use
	day    string             // the UTC day old entries were last pruned for
	path   string             // state file; "" = memory only
	dirty  bool
	saveMu sync.Mutex // one save at a time (ticker and Close)
}

type dayUse struct {
	Day string `json:"day"`
	N   int64  `json:"n"`
}

func newBudgets() *budgets { return &budgets{used: map[string]*dayUse{}} }

// keyHash identifies an API key (or key|end user) without storing it.
func keyHash(key string) string {
	h := sha256.Sum256([]byte(key))
	return hex.EncodeToString(h[:])
}

func utcDay(t time.Time) string { return t.UTC().Format("2006-01-02") }

// over reports whether key has used its daily limit, and the seconds until it resets.
func (b *budgets) over(key string, limit int, now time.Time) (int, bool) {
	if limit <= 0 {
		return 0, false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	u := b.used[keyHash(key)]
	if u == nil || u.Day != utcDay(now) || u.N < int64(limit) {
		return 0, false
	}
	next := now.UTC().Truncate(24 * time.Hour).Add(24 * time.Hour)
	return int(next.Sub(now).Seconds()) + 1, true
}

// used1 is what key has used today.
func (b *budgets) used1(key string, now time.Time) int64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	if u := b.used[keyHash(key)]; u != nil && u.Day == utcDay(now) {
		return u.N
	}
	return 0
}

func (b *budgets) add(key string, n int64, now time.Time) {
	b.mu.Lock()
	defer b.mu.Unlock()
	day := utcDay(now)
	switch {
	case day < b.day: // a request from before midnight, finished after: no longer counts
		return
	case day > b.day: // a new day: yesterday's entries count for nothing any more
		for id, u := range b.used {
			if u.Day != day {
				delete(b.used, id)
			}
		}
		b.day = day
	}
	id := keyHash(key)
	u := b.used[id]
	if u == nil || u.Day != day {
		u = &dayUse{Day: day}
		b.used[id] = u
	}
	u.N += n
	b.dirty = true
}

// budgetState is the state file's format.
type budgetState struct {
	Version int               `json:"version"`
	Used    map[string]dayUse `json:"used"`
}

// load reads the state file, keeping only today's entries. A missing file is a first start;
// an unreadable or corrupt one is logged and ignored: budgets then restart from zero, which
// is what happened on every restart before they were saved, so it must not stop the gateway.
func (b *budgets) load(now time.Time, log *slog.Logger) {
	raw, err := os.ReadFile(b.path)
	if errors.Is(err, os.ErrNotExist) {
		return
	}
	var st budgetState
	if err == nil {
		err = json.Unmarshal(raw, &st)
	}
	if err != nil {
		log.Warn("budget state unreadable, starting with empty budgets", "file", b.path, "err", err)
		return
	}
	day := utcDay(now)
	b.mu.Lock()
	defer b.mu.Unlock()
	for id, u := range st.Used {
		if u.Day == day && u.N > 0 {
			b.used[id] = &dayUse{Day: u.Day, N: u.N}
		}
	}
	b.day = day
}

// save writes the state file if anything changed since the last save.
func (b *budgets) save() error {
	b.saveMu.Lock()
	defer b.saveMu.Unlock()
	b.mu.Lock()
	if b.path == "" || !b.dirty {
		b.mu.Unlock()
		return nil
	}
	st := budgetState{Version: 1, Used: make(map[string]dayUse, len(b.used))}
	for id, u := range b.used {
		st.Used[id] = *u
	}
	b.dirty = false
	b.mu.Unlock()
	raw, _ := json.Marshal(st)
	if err := writeFileAtomic(b.path, raw); err != nil {
		b.mu.Lock()
		b.dirty = true // try again next time
		b.mu.Unlock()
		return err
	}
	return nil
}

// writeFileAtomic replaces path with data so that readers (and a crash) see either the old
// or the new file, never a partial one: write a temporary file next to it, then rename.
func writeFileAtomic(path string, data []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*.tmp") // mode 0600
	if err != nil {
		return err
	}
	tmp := f.Name()
	_, err = f.Write(data)
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp, path)
	}
	if err != nil {
		os.Remove(tmp)
	}
	return err
}
