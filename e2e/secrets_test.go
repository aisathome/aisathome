// A new API key or node token is shown once, on its own panel instead of the account page.
//
// SPDX-License-Identifier: Apache-2.0
package e2e

import (
	"crypto/sha256"
	"encoding/base64"
	"net/http"
	"regexp"
	"strings"
	"testing"
)

var scriptRe = regexp.MustCompile(`(?s)<script>(.*?)</script>`)

// secretPanel checks the page that shows a new secret: the panel replaces the account page,
// says it is shown once, has a Copy button (its one script allowed by hash) and a Done
// button back to the right section.
func secretPanel(t *testing.T, resp *http.Response, page, secret, title, section string) {
	t.Helper()
	if resp.StatusCode != 200 || secret == "" || !strings.Contains(page, secret) {
		t.Fatalf("%s: status %d, secret not shown", title, resp.StatusCode)
	}
	for _, want := range []string{`<h1 id="secret-title">` + title + `</h1>`, "You will not see this again", `id="copy"`,
		`<form method="get" action="/account#` + section + `"><button class="btn">Done</button>`} {
		if !strings.Contains(page, want) {
			t.Fatalf("%s: the panel lacks %q: %s", title, want, page)
		}
	}
	// The rest of the account page is not there: the panel replaces it.
	for _, gone := range []string{`id="keys"`, `id="nodes"`, "Sign out", `action="/account/username"`} {
		if strings.Contains(page, gone) {
			t.Fatalf("%s: the account page is still shown around the panel (%q)", title, gone)
		}
	}
	script := scriptRe.FindStringSubmatch(page)
	if script == nil {
		t.Fatalf("%s: no script for the Copy button", title)
	}
	sum := sha256.Sum256([]byte(script[1]))
	if csp := resp.Header.Get("Content-Security-Policy"); !strings.Contains(csp, "script-src 'sha256-"+base64.StdEncoding.EncodeToString(sum[:])+"'") {
		t.Fatalf("%s: CSP %q does not allow the Copy script by hash", title, csp)
	}
}

func TestNewSecretsOnTheirOwnPanel(t *testing.T) {
	a := nodeRig(t, nil)
	s, _ := a.signIn(t, 1)
	resp, page := a.createKey(t, s)
	secretPanel(t, resp, page, keyRe.FindString(page), "Your new API key", "keys")

	_, before := a.get(t, "/account", s)
	once := nodeOnceRe.FindStringSubmatch(before)[1]
	resp, page = a.post(t, "/account/nodes", s, "once="+once+"&name=rig")
	secretPanel(t, resp, page, nodeTokenRe.FindString(page), "Your new node token", "nodes")

	// Done (GET /account) shows the account page again: no secret, no script, sections there.
	resp, page = a.get(t, "/account", s)
	if keyRe.MatchString(page) || nodeTokenRe.MatchString(page) || scriptRe.MatchString(page) ||
		strings.Contains(resp.Header.Get("Content-Security-Policy"), "script-src") {
		t.Fatal("the account page shows a secret again, or runs a script")
	}
	if !strings.Contains(page, `id="keys"`) || !strings.Contains(page, `id="nodes"`) || strings.Contains(page, "You will not see this again") {
		t.Fatalf("the account page after Done: %s", page)
	}
}
