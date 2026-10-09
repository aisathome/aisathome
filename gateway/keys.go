// SPDX-License-Identifier: Apache-2.0

package gateway

import (
	"context"
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
	u := KeyInfo{User: k.User + "/" + uid, MinTrust: k.MinTrust, OnlyOperator: k.OnlyOperator}
	if k.UserLimits != nil {
		u.RPM, u.MaxConcurrent, u.TokensPerDay = k.UserLimits.RPM, k.UserLimits.MaxConcurrent, k.UserLimits.TokensPerDay
	}
	return u
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
}

type bucket struct {
	tokens   float64
	last     time.Time
	inFlight int
}

func newLimiter() *limiter { return &limiter{buckets: map[string]*bucket{}} }

// acquire reports whether the request may start, and returns a release function.
func (l *limiter) acquire(key string, k KeyInfo, now time.Time) (release func(), reason string) {
	l.mu.Lock()
	defer l.mu.Unlock()
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
		b.last = now
		if b.tokens < 1 {
			return nil, "rate limit exceeded"
		}
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

// budgets track weighted tokens per key and UTC day (in memory; a restart resets the day).
type budgets struct {
	mu   sync.Mutex
	used map[string]*dayUse
}

type dayUse struct {
	day string
	n   int64
}

func newBudgets() *budgets { return &budgets{used: map[string]*dayUse{}} }

// over reports whether key has used its daily limit, and the seconds until it resets.
func (b *budgets) over(key string, limit int, now time.Time) (int, bool) {
	if limit <= 0 {
		return 0, false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	u := b.used[key]
	day := now.UTC().Format("2006-01-02")
	if u == nil || u.day != day || u.n < int64(limit) {
		return 0, false
	}
	next := now.UTC().Truncate(24 * time.Hour).Add(24 * time.Hour)
	return int(next.Sub(now).Seconds()) + 1, true
}

func (b *budgets) add(key string, n int64, now time.Time) {
	b.mu.Lock()
	defer b.mu.Unlock()
	day := now.UTC().Format("2006-01-02")
	u := b.used[key]
	if u == nil || u.day != day {
		u = &dayUse{day: day}
		b.used[key] = u
	}
	u.n += n
}
