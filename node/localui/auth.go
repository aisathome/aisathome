// SPDX-License-Identifier: Apache-2.0
package localui

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	cookieName = "aisathome_node_ui"
	sessionTTL = 90 * 24 * time.Hour // "entered once": a session lasts until the password changes or 90 days pass
	failDelay  = time.Second         // after a wrong password, the next attempt waits this long
)

// auth checks the local password and issues session cookies. Sessions are stateless: the
// cookie holds an expiry and an HMAC keyed by the password, so they survive agent restarts
// (an engine restart restarts the agent) and all end when the password file changes.
type auth struct {
	file string
	now  func() time.Time

	mu       sync.Mutex
	lastFail time.Time
}

func newAuth(file string) *auth { return &auth{file: file, now: time.Now} }

// password reads the password file; it is read on every use, so a new password takes effect
// at once.
func (a *auth) password() (string, error) {
	if a.file == "" {
		return "", errors.New("no password file is configured")
	}
	b, err := os.ReadFile(a.file)
	if err != nil {
		return "", fmt.Errorf("cannot read the password file %s (re-run the installer to create it)", a.file)
	}
	pw := strings.TrimSpace(string(b))
	if len(pw) < 12 {
		return "", fmt.Errorf("the password in %s is shorter than 12 characters", a.file)
	}
	return pw, nil
}

func sign(pw string, exp int64) string {
	k := hmac.New(sha256.New, []byte(pw))
	k.Write([]byte("aisathome-node-ui session key v1"))
	m := hmac.New(sha256.New, k.Sum(nil))
	m.Write([]byte("session|" + strconv.FormatInt(exp, 10)))
	return hex.EncodeToString(m.Sum(nil))
}

// login checks a password and returns a session cookie, or an HTTP status and error.
func (a *auth) login(given string) (*http.Cookie, int, error) {
	pw, err := a.password()
	if err != nil {
		return nil, http.StatusServiceUnavailable, errors.New("controls are off: " + err.Error())
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if now := a.now(); now.Sub(a.lastFail) < failDelay {
		return nil, http.StatusTooManyRequests, errors.New("too many attempts; wait a second")
	}
	// Compare digests, so the comparison takes the same time whatever the lengths.
	g, w := sha256.Sum256([]byte(strings.TrimSpace(given))), sha256.Sum256([]byte(pw))
	if subtle.ConstantTimeCompare(g[:], w[:]) != 1 {
		a.lastFail = a.now()
		return nil, http.StatusUnauthorized, errors.New("wrong password")
	}
	exp := a.now().Add(sessionTTL).Unix()
	return &http.Cookie{Name: cookieName, Value: strconv.FormatInt(exp, 10) + "." + sign(pw, exp), Path: "/",
		MaxAge: int(sessionTTL / time.Second), HttpOnly: true, SameSite: http.SameSiteStrictMode}, 0, nil
}

// clear returns a cookie that deletes the session cookie.
func (a *auth) clear() *http.Cookie {
	return &http.Cookie{Name: cookieName, Value: "", Path: "/", MaxAge: -1, HttpOnly: true, SameSite: http.SameSiteStrictMode}
}

// valid reports whether the request carries a session cookie for the current password.
func (a *auth) valid(r *http.Request) bool {
	c, err := r.Cookie(cookieName)
	if err != nil {
		return false
	}
	expS, mac, ok := strings.Cut(c.Value, ".")
	if !ok {
		return false
	}
	exp, err := strconv.ParseInt(expS, 10, 64)
	if err != nil || a.now().Unix() >= exp {
		return false
	}
	pw, err := a.password()
	if err != nil {
		return false
	}
	return hmac.Equal([]byte(mac), []byte(sign(pw, exp)))
}
