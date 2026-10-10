// SPDX-License-Identifier: Apache-2.0

package gateway

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

func TestAccountFromEmail(t *testing.T) {
	for email, want := range map[string]string{
		"gh-123@users.invalid":                                  "gh:123",
		"google-104578901234567890123@users.invalid":            "google:104578901234567890123",
		"GOOGLE-42@USERS.INVALID":                               "google:42", // the chat may change case
		"google-@users.invalid":                                 "",
		"google-12a@users.invalid":                              "",
		"google-1:2@users.invalid":                              "",
		"google-1-2@users.invalid":                              "",
		"google-1@users.invalid.evil.test":                      "",
		"google-" + strings.Repeat("1", 256) + "@users.invalid": "",
		"gh-@users.invalid":                                     "",
		"test-x@users.invalid":                                  "",
		"someone@example.com":                                   "",
	} {
		id, ok := accountFromEmail(email)
		if id != want || ok != (want != "") {
			t.Errorf("%s: got %q %v, want %q", email, id, ok, want)
		}
		if ok && !strings.EqualFold(accountEmail(id), email) {
			t.Errorf("%s does not round-trip: %s", email, accountEmail(id))
		}
	}
	if !validGoogleSub(strings.Repeat("9", 255)) || validGoogleSub(strings.Repeat("9", 256)) {
		t.Error("sub length limit is not 255")
	}
}

func TestSignInProvidersConfig(t *testing.T) {
	base := func() AccountsConfig {
		return AccountsConfig{File: "a.json", PublicURL: "https://x.test", SessionSecret: strings.Repeat("s", 32)}
	}
	for _, c := range []struct {
		name                   string
		ghID, ghSec, gID, gSec string
		ok, github, google     bool
	}{
		{"none", "", "", "", "", false, false, false},
		{"github", "i", "s", "", "", true, true, false},
		{"google", "", "", "i", "s", true, false, true},
		{"both", "i", "s", "i", "s", true, true, true},
		{"half google", "i", "s", "i", "", false, false, false},
		{"half github", "", "s", "i", "s", false, false, false},
	} {
		cfg := base()
		cfg.GitHubClientID, cfg.GitHubClientSecret, cfg.GoogleClientID, cfg.GoogleClientSecret = c.ghID, c.ghSec, c.gID, c.gSec
		err := cfg.defaults()
		if (err == nil) != c.ok || (c.ok && (cfg.githubOn() != c.github || cfg.googleOn() != c.google)) {
			t.Errorf("%s: err %v, github %v, google %v", c.name, err, cfg.githubOn(), cfg.googleOn())
		}
	}
}

// The key cache refetches on an unknown kid (rotation), but not more often than its
// refetch interval, and refetches everything once the cache is older than its TTL.
func TestJWKSCache(t *testing.T) {
	k1, _ := rsa.GenerateKey(rand.Reader, 2048)
	k2, _ := rsa.GenerateKey(rand.Reader, 2048)
	small, _ := rsa.GenerateKey(rand.Reader, 1024)
	var mu sync.Mutex
	served := map[string]*rsa.PublicKey{"k1": &k1.PublicKey, "weak": &small.PublicKey}
	var fetches atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fetches.Add(1)
		mu.Lock()
		defer mu.Unlock()
		var keys []map[string]string
		for kid, k := range served {
			keys = append(keys, map[string]string{"kty": "RSA", "alg": "RS256", "use": "sig", "kid": kid,
				"n": base64.RawURLEncoding.EncodeToString(k.N.Bytes()),
				"e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(k.E)).Bytes())})
		}
		json.NewEncoder(w).Encode(map[string]any{"keys": keys})
	}))
	defer srv.Close()
	c := newJWKSCache(srv.URL, (&Gateway{}).getJSON)
	ctx := context.Background()
	if k, err := c.key(ctx, "k1"); err != nil || k.N.Cmp(k1.N) != 0 {
		t.Fatalf("k1: %v", err)
	}
	if _, err := c.key(ctx, "weak"); err == nil {
		t.Fatal("a 1024-bit key was accepted")
	}
	c.key(ctx, "k1")
	if n := fetches.Load(); n != 1 {
		t.Fatalf("a known kid refetched: %d fetches", n)
	}
	// Google rotates: k2 appears. Within the refetch interval an unknown kid does not refetch.
	mu.Lock()
	served["k2"] = &k2.PublicKey
	mu.Unlock()
	if _, err := c.key(ctx, "k2"); err == nil || fetches.Load() != 1 {
		t.Fatalf("refetched within the interval: %v, %d fetches", err, fetches.Load())
	}
	c.refetch = 0
	if k, err := c.key(ctx, "k2"); err != nil || k.N.Cmp(k2.N) != 0 || fetches.Load() != 2 {
		t.Fatalf("rotation: %v, %d fetches", err, fetches.Load())
	}
	// An expired cache is refetched even for a known kid; a removed key is then gone.
	mu.Lock()
	delete(served, "k1")
	mu.Unlock()
	c.ttl = 0
	if _, err := c.key(ctx, "k1"); err == nil || fetches.Load() != 3 {
		t.Fatalf("stale cache: %v, %d fetches", err, fetches.Load())
	}
}
