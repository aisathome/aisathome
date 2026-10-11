// Google sign-in: OpenID Connect with the "openid" scope only, so Google tells us nothing
// but its account identifier ("sub"). No email, no profile; the ID token is checked and
// dropped, and only "google:<sub>" is kept. The flow is the authorization-code flow with
// state, a nonce and PKCE (S256).
//
// SPDX-License-Identifier: Apache-2.0
package gateway

import (
	"context"
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

const (
	// idTokenSkew is the clock difference tolerated on exp and iat.
	idTokenSkew = time.Minute
	// maxGoogleSub: OpenID Connect allows up to 255 ASCII characters.
	maxGoogleSub = 255
)

// googleAuthURL is where handleLogin sends the browser: scope "openid" and nothing else.
func (g *Gateway) googleAuthURL(f oauthFlow) string {
	q := url.Values{"client_id": {g.acfg.GoogleClientID}, "response_type": {"code"}, "scope": {"openid"},
		"redirect_uri": {g.acfg.PublicURL + "/account/callback/google"}, "state": {f.state},
		"nonce": {f.nonce}, "code_challenge": {pkceChallenge(f.verifier)}, "code_challenge_method": {"S256"}}
	return g.acfg.GoogleAuthURL + "?" + q.Encode()
}

// pkceChallenge is the S256 code challenge for a PKCE verifier (RFC 7636).
func pkceChallenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// handleGoogleCallback finishes Google sign-in: code (+ PKCE verifier) → ID token → sub.
func (g *Gateway) handleGoogleCallback(w http.ResponseWriter, r *http.Request) {
	f, ok := g.takeFlow(w, r, "google")
	if !ok || f.nonce == "" || f.verifier == "" {
		g.accountPage(w, r, http.StatusBadRequest, pageData{Error: "The sign-in expired or came from elsewhere. Please try again."})
		return
	}
	id, err := g.googleID(r.Context(), r.URL.Query().Get("code"), f)
	if err != nil {
		g.log.Warn("google sign-in failed", "err", err) // errors never carry tokens or claims
		g.accountPage(w, r, http.StatusBadGateway, pageData{Error: "Google sign-in failed. Please try again."})
		return
	}
	g.finishSignIn(w, r, id, f.next)
}

// googleID exchanges an authorization code for an ID token and returns "google:<sub>". The
// access token in the same answer is never decoded.
func (g *Gateway) googleID(ctx context.Context, code string, f oauthFlow) (string, error) {
	if code == "" {
		return "", errors.New("no code")
	}
	form := url.Values{"client_id": {g.acfg.GoogleClientID}, "client_secret": {g.acfg.GoogleClientSecret},
		"code": {code}, "grant_type": {"authorization_code"}, "code_verifier": {f.verifier},
		"redirect_uri": {g.acfg.PublicURL + "/account/callback/google"}}
	req, _ := http.NewRequestWithContext(ctx, "POST", g.acfg.GoogleTokenURL, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	var tok struct {
		IDToken string `json:"id_token"`
		Error   string `json:"error"`
	}
	if err := g.getJSON(req, &tok); err != nil {
		return "", err
	}
	if tok.IDToken == "" {
		return "", fmt.Errorf("token exchange: no id_token (%.40s)", tok.Error)
	}
	sub, err := g.verifyIDToken(ctx, tok.IDToken, f.nonce, time.Now())
	if err != nil {
		return "", err
	}
	return "google:" + sub, nil
}

// verifyIDToken checks a Google ID token and returns its sub.
//
// The token comes straight from Google's token endpoint over TLS, which OpenID Connect
// (Core 3.1.3.7) accepts in place of a signature check. We check the RS256 signature against
// Google's published keys anyway, as defence in depth: a wrong token URL, a proxy or a bug in
// the exchange then cannot turn a forged token into an account. The claim checks (iss, aud,
// exp, iat, nonce) are needed either way.
func (g *Gateway) verifyIDToken(ctx context.Context, raw, nonce string, now time.Time) (string, error) {
	parts := strings.Split(raw, ".")
	if len(parts) != 3 {
		return "", errors.New("id token: not a JWS")
	}
	var hdr struct {
		Alg string `json:"alg"`
		Kid string `json:"kid"`
	}
	if err := decodeSegment(parts[0], &hdr); err != nil {
		return "", fmt.Errorf("id token header: %w", err)
	}
	if hdr.Alg != "RS256" { // never "none" or an HMAC keyed with something public
		return "", errors.New("id token: alg is not RS256")
	}
	key, err := g.gkeys.key(ctx, hdr.Kid)
	if err != nil {
		return "", err
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return "", errors.New("id token: bad signature encoding")
	}
	sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(key, crypto.SHA256, sum[:], sig); err != nil {
		return "", errors.New("id token: bad signature")
	}
	// Only these claims are decoded; anything else in the token (there should be nothing
	// personal with scope "openid") is never held.
	var c struct {
		Iss   string          `json:"iss"`
		Aud   json.RawMessage `json:"aud"`
		Sub   string          `json:"sub"`
		Exp   int64           `json:"exp"`
		Iat   int64           `json:"iat"`
		Nonce string          `json:"nonce"`
	}
	if err := decodeSegment(parts[1], &c); err != nil {
		return "", fmt.Errorf("id token claims: %w", err)
	}
	iss := g.acfg.GoogleIssuerURL // Google uses it with and without "https://"
	if c.Iss != iss && c.Iss != strings.TrimPrefix(iss, "https://") {
		return "", errors.New("id token: wrong issuer")
	}
	if !audIs(c.Aud, g.acfg.GoogleClientID) {
		return "", errors.New("id token: wrong audience")
	}
	if c.Exp == 0 || !now.Before(time.Unix(c.Exp, 0).Add(idTokenSkew)) {
		return "", errors.New("id token: expired")
	}
	if c.Iat == 0 || time.Unix(c.Iat, 0).After(now.Add(idTokenSkew)) {
		return "", errors.New("id token: issued in the future")
	}
	if nonce == "" || subtle.ConstantTimeCompare([]byte(c.Nonce), []byte(nonce)) != 1 {
		return "", errors.New("id token: wrong nonce")
	}
	if !validGoogleSub(c.Sub) {
		return "", errors.New("id token: sub is not a Google account number")
	}
	return c.Sub, nil
}

// validGoogleSub accepts what Google issues: decimal digits, at most 255 of them. Letters
// are refused rather than allowed, because the chat lowercases addresses: two subs that
// differ only in case would otherwise share one chat account and one budget.
func validGoogleSub(sub string) bool {
	if sub == "" || len(sub) > maxGoogleSub {
		return false
	}
	for i := 0; i < len(sub); i++ {
		if sub[i] < '0' || sub[i] > '9' {
			return false
		}
	}
	return true
}

// audIs reports whether aud (a string, or an array with one string) is exactly want.
func audIs(aud json.RawMessage, want string) bool {
	var one string
	if json.Unmarshal(aud, &one) == nil {
		return one == want
	}
	var many []string
	return json.Unmarshal(aud, &many) == nil && len(many) == 1 && many[0] == want
}

func decodeSegment(seg string, v any) error {
	raw, err := base64.RawURLEncoding.DecodeString(seg)
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, v)
}

// ---------------------------------------------------------------- signing keys

const (
	jwksTTL     = time.Hour   // Google rotates keys over days and lists the next one early
	jwksRefetch = time.Minute // an unknown kid refetches at most this often
)

// jwksCache holds Google's signing keys by kid. A key not in the cache refetches the set
// (Google may have rotated), but at most once per refetch interval; a cache older than
// ttl is refetched before use, and if that fails sign-in fails rather than trust old keys.
type jwksCache struct {
	url     string
	ttl     time.Duration
	refetch time.Duration
	fetch   func(*http.Request, any) error
	mu      sync.Mutex
	keys    map[string]*rsa.PublicKey
	fetched time.Time // last successful fetch
	tried   time.Time // last attempt
}

func newJWKSCache(certsURL string, fetch func(*http.Request, any) error) *jwksCache {
	return &jwksCache{url: certsURL, ttl: jwksTTL, refetch: jwksRefetch, fetch: fetch}
}

func (c *jwksCache) key(ctx context.Context, kid string) (*rsa.PublicKey, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	stale := c.keys == nil || now.Sub(c.fetched) >= c.ttl
	if k, ok := c.keys[kid]; ok && !stale {
		return k, nil
	}
	if stale || now.Sub(c.tried) >= c.refetch {
		c.tried = now
		keys, err := c.load(ctx)
		if err != nil {
			return nil, fmt.Errorf("google keys: %w", err)
		}
		c.keys, c.fetched = keys, now
	}
	if k, ok := c.keys[kid]; ok {
		return k, nil
	}
	return nil, errors.New("id token: unknown signing key")
}

func (c *jwksCache) load(ctx context.Context) (map[string]*rsa.PublicKey, error) {
	req, _ := http.NewRequestWithContext(ctx, "GET", c.url, nil)
	var set struct {
		Keys []struct {
			Kty string `json:"kty"`
			Alg string `json:"alg"`
			Use string `json:"use"`
			Kid string `json:"kid"`
			N   string `json:"n"`
			E   string `json:"e"`
		} `json:"keys"`
	}
	if err := c.fetch(req, &set); err != nil {
		return nil, err
	}
	keys := map[string]*rsa.PublicKey{}
	for _, k := range set.Keys {
		if k.Kty != "RSA" || k.Kid == "" || (k.Alg != "" && k.Alg != "RS256") || (k.Use != "" && k.Use != "sig") {
			continue
		}
		n, err1 := base64.RawURLEncoding.DecodeString(k.N)
		e, err2 := base64.RawURLEncoding.DecodeString(k.E)
		if err1 != nil || err2 != nil || len(e) == 0 || len(e) > 4 {
			continue
		}
		pub := &rsa.PublicKey{N: new(big.Int).SetBytes(n), E: int(new(big.Int).SetBytes(e).Int64())}
		if pub.N.BitLen() < 2048 || pub.E < 3 || pub.E%2 == 0 {
			continue
		}
		keys[k.Kid] = pub
	}
	if len(keys) == 0 {
		return nil, errors.New("no usable RSA keys")
	}
	return keys, nil
}
