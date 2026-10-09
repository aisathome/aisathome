// SPDX-License-Identifier: AGPL-3.0-or-later

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
	RPM           int    `json:"rpm,omitempty"`        // requests per minute; 0 = unlimited
	MaxConcurrent int    `json:"concurrent,omitempty"` // requests in flight; 0 = unlimited
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
