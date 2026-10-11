// The gateway's own pages (account, email sign-in) look like the website but load nothing from
// another origin, and the provider buttons follow GitHub's and Google's brand assets.
//
// SPDX-License-Identifier: Apache-2.0
package e2e

import (
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"github.com/aisathome/aisathome/gateway"
)

var (
	resourceRe = regexp.MustCompile(`(?:src|srcset)="([^"]*)"|<link[^>]*href="([^"]*)"|url\(([^)]*)\)`)
	absURLRe   = regexp.MustCompile(`(?:https?:)?//[^\s"'<>)]+`)
)

// threeWayRig offers GitHub, Google and email sign-in.
func threeWayRig(t *testing.T) *acctRig {
	g, _ := newGoogleRig(t, 100, func(c *gateway.AccountsConfig) {
		c.Email = &gateway.EmailConfig{SMTPHost: "127.0.0.1", SMTPPort: 1, SMTPUser: "u", SMTPPassword: "p",
			From: "noreply@aisathome.org", Keys: []gateway.EmailKey{emailKey1}}
	})
	return g
}

// Every resource a page loads is one of the gateway's own assets (and is served); absolute
// URLs appear only as links to follow, and the CSP allows no other origin to load anything.
func TestPagesLoadNothingFromOtherOrigins(t *testing.T) {
	g := threeWayRig(t)
	session, _ := g.signIn(t, 1)
	for _, p := range []struct{ path, session string }{
		{"/account?next=chat", ""}, {"/account", session}, {"/account/email", ""},
	} {
		resp, page := g.get(t, p.path, p.session)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s: %d", p.path, resp.StatusCode)
		}
		csp := resp.Header.Get("Content-Security-Policy")
		if !strings.HasPrefix(csp, "default-src 'none'; style-src 'unsafe-inline'; font-src 'self'; img-src 'self'") ||
			strings.Count(csp, "//") > strings.Count(csp, "form-action 'self' https://github.com") {
			t.Fatalf("%s: CSP %q", p.path, csp)
		}
		found := 0
		for _, m := range resourceRe.FindAllStringSubmatch(page, -1) {
			ref := strings.Trim(m[1]+m[2]+m[3], `"'`)
			if !strings.HasPrefix(ref, "/account/assets/") {
				t.Fatalf("%s loads %q, not a gateway asset", p.path, ref)
			}
			r, body := g.get(t, ref, "")
			if r.StatusCode != http.StatusOK || !strings.Contains(r.Header.Get("Cache-Control"), "immutable") ||
				!strings.HasPrefix(r.Header.Get("Content-Type"), "font/woff2") && !strings.HasPrefix(r.Header.Get("Content-Type"), "image/svg+xml") {
				t.Fatalf("%s: asset %s: %d %v", p.path, ref, r.StatusCode, r.Header)
			}
			if strings.HasSuffix(ref, ".svg") && (strings.Contains(body, "<script") || strings.Contains(body, "href=")) {
				t.Fatalf("%s: %s refers elsewhere or has a script", p.path, ref)
			}
			found++
		}
		if found < 4 { // three fonts and the favicon, at least
			t.Fatalf("%s: only %d assets found; the check is not looking", p.path, found)
		}
		for _, abs := range absURLRe.FindAllString(page, -1) {
			u, err := url.Parse(abs)
			if err != nil || u.Host != "chat.site.test" && u.Host != "github.com" ||
				!regexp.MustCompile(`<a [^>]*href="`+regexp.QuoteMeta(abs)+`"`).MatchString(page) {
				t.Fatalf("%s: absolute URL %q is not a link to the chat or the source code", p.path, abs)
			}
		}
	}
	for _, bad := range []string{"/account/assets/public-sans.woff2", "/account/assets/nope.svg", "/account/assets/..%2faccounts.json"} {
		if r, _ := g.get(t, bad, ""); r.StatusCode != http.StatusNotFound {
			t.Fatalf("%s: %d", bad, r.StatusCode)
		}
	}
}

// Both provider buttons render when both are configured, each only when it is.
func TestProviderButtons(t *testing.T) {
	gh := `<a class="btn-gh" href="/account/login?provider=github&amp;next=chat"><svg viewBox="0 0 16 16"`
	google := `<a class="btn-google" href="/account/login?provider=google&amp;next=chat"><picture>`
	_, page := threeWayRig(t).get(t, "/account?next=chat", "")
	for _, want := range []string{gh, "<span>Sign in with GitHub</span></a>", google,
		`srcset="/account/assets/google-signin-dark.`, `<img src="/account/assets/google-signin-light.`, `alt="Sign in with Google"`,
		`<button class="btn secondary">Email me a code</button>`} {
		if !strings.Contains(page, want) {
			t.Fatalf("sign-in page lacks %q:\n%s", want, page)
		}
	}
	if _, page := newAcctRig(t, 100, nil).get(t, "/account?next=chat", ""); !strings.Contains(page, gh) || strings.Contains(page, `class="btn-google"`) {
		t.Fatal("GitHub only: wrong buttons")
	}
	only, _ := newGoogleRig(t, 100, func(c *gateway.AccountsConfig) { c.GitHubClientID, c.GitHubClientSecret = "", "" })
	if _, page := only.get(t, "/account?next=chat", ""); !strings.Contains(page, google) || strings.Contains(page, `class="btn-gh"`) {
		t.Fatal("Google only: wrong buttons")
	}
}
