// The account pages' header matches the website's (site/src/data/nav.json) and shares its
// light/dark choice through the cookie aisathome_theme (gateway/theme.go,
// site/src/scripts/theme-sync.js).
//
// SPDX-License-Identifier: Apache-2.0
package e2e

import (
	"encoding/json"
	"html"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"testing"
)

func (a *acctRig) getWith(t *testing.T, path string, cookies ...string) string {
	t.Helper()
	req, _ := http.NewRequest("GET", a.gw.URL+path, nil)
	if len(cookies) > 0 {
		req.Header.Set("Cookie", strings.Join(cookies, "; "))
	}
	resp, err := noRedirect.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("%s: %d", path, resp.StatusCode)
	}
	return string(b)
}

// The cookie sets <html data-theme> for an explicit choice; auto, garbage or no cookie leave it
// to the system. The theme control shows the choice, and the Google button follows it.
func TestThemeCookieSetsDataTheme(t *testing.T) {
	g := threeWayRig(t)
	session, _ := g.signIn(t, 1)
	for _, c := range []struct{ cookie, want string }{
		{"", ""}, {"aisathome_theme=auto", ""}, {"aisathome_theme=dark", "dark"}, {"aisathome_theme=light", "light"},
		{"aisathome_theme=blue", ""}, {"aisathome_theme=dark%22%3E", ""},
	} {
		for _, p := range []struct{ path, session string }{{"/account?next=chat", ""}, {"/account", session}, {"/account/email", ""}} {
			cookies := []string{}
			for _, v := range []string{c.cookie, p.session} {
				if v != "" {
					cookies = append(cookies, v)
				}
			}
			page := g.getWith(t, p.path, cookies...)
			open := regexp.MustCompile(`<html[^>]*>`).FindString(page)
			wantOpen := `<html lang="en">`
			if c.want != "" {
				wantOpen = `<html lang="en" data-theme="` + c.want + `">`
			}
			if open != wantOpen {
				t.Fatalf("%s with %q: %s, want %s", p.path, c.cookie, open, wantOpen)
			}
			pressed := regexp.MustCompile(`<button name="theme" value="(\w+)" aria-pressed="true">`).FindAllStringSubmatch(page, -1)
			want := c.want
			if want == "" {
				want = "auto"
			}
			if len(pressed) != 2 || pressed[0][1] != want || pressed[1][1] != want { // header and menu
				t.Fatalf("%s with %q: theme control marks %v, want %s", p.path, c.cookie, pressed, want)
			}
			if p.path == "/account?next=chat" {
				google := regexp.MustCompile(`<a class="btn-google"[^>]*>(?s:.*?)</a>`).FindString(page)
				if (c.want == "") != strings.Contains(google, "<picture>") ||
					c.want != "" && !strings.Contains(google, `<img src="/account/assets/google-signin-`+c.want+`.`) {
					t.Fatalf("with %q: Google button %s", c.cookie, google)
				}
			}
		}
	}
	// The CSS handles all three: the system's choice, and an explicit one against it.
	page := g.getWith(t, "/account")
	for _, want := range []string{"@media (prefers-color-scheme:light){:root:not([data-theme=dark]){--bg:#fdf8f1;",
		":root[data-theme=dark]{color-scheme:dark}", ":root[data-theme=light]{color-scheme:light;--bg:#fdf8f1;",
		":root[data-theme=dark] .btn-gh{", ":root:not([data-theme=light]) .btn-gh{"} {
		if !strings.Contains(page, want) {
			t.Fatalf("page CSS lacks %q", want)
		}
	}
}

// POST /account/theme sets the shared cookie and goes back to the page it came from, never to
// another site, and only for same-origin posts.
func TestThemeEndpoint(t *testing.T) {
	a := threeWayRig(t)
	post := func(form string, headers map[string]string) *http.Response {
		req, _ := http.NewRequest("POST", a.gw.URL+"/account/theme", strings.NewReader(form))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		resp, err := noRedirect.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp
	}
	ours := map[string]string{"Sec-Fetch-Site": "same-origin", "Origin": "null"} // a real browser, our form
	for _, theme := range []string{"dark", "light", "auto"} {
		resp := post("theme="+theme+"&back=%2Faccount%3Fnext%3Dchat", ours)
		if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "https://site.test/account?next=chat" {
			t.Fatalf("%s: %d to %q", theme, resp.StatusCode, resp.Header.Get("Location"))
		}
		set := resp.Header.Values("Set-Cookie")
		if len(set) != 1 || set[0] != "aisathome_theme="+theme+"; Path=/; Max-Age=31536000; Secure; SameSite=Lax" {
			t.Fatalf("%s: Set-Cookie %q (want host-only, Secure, Lax, readable by the site's script)", theme, set)
		}
	}
	for _, back := range []string{"", "account", "//evil.test/x", "/\\evil.test", "https://evil.test/account",
		"javascript:alert(1)", "/account\r\nX: y", " /account", "\\\\evil.test", "/" + strings.Repeat("a", 300)} {
		resp := post("theme=dark&back="+url.QueryEscape(back), ours)
		if loc := resp.Header.Get("Location"); resp.StatusCode != http.StatusSeeOther || loc != "https://site.test/account" {
			t.Fatalf("back %q: %d to %q", back, resp.StatusCode, loc)
		}
	}
	for _, back := range []string{"/account/email", "/", "/node/"} { // pages on this site
		if loc := post("theme=dark&back="+url.QueryEscape(back), ours).Header.Get("Location"); loc != "https://site.test"+back {
			t.Fatalf("back %q: to %q", back, loc)
		}
	}
	for _, h := range []map[string]string{
		{"Origin": "https://evil.test"}, {"Sec-Fetch-Site": "cross-site", "Origin": "https://evil.test"},
		{"Sec-Fetch-Site": "same-site", "Origin": "https://chat.site.test"},
	} {
		if resp := post("theme=dark&back=%2Faccount", h); resp.StatusCode != http.StatusForbidden || len(resp.Header.Values("Set-Cookie")) != 0 {
			t.Fatalf("%v: %d, Set-Cookie %q", h, resp.StatusCode, resp.Header.Values("Set-Cookie"))
		}
	}
	for _, form := range []string{"theme=blue", "theme=", "back=%2Faccount", "theme=dark%00"} {
		if resp := post(form, ours); resp.StatusCode != http.StatusBadRequest || len(resp.Header.Values("Set-Cookie")) != 0 {
			t.Fatalf("%q: %d, Set-Cookie %q", form, resp.StatusCode, resp.Header.Values("Set-Cookie"))
		}
	}
	// The pages' forms post here, each back to itself.
	for path, back := range map[string]string{"/account?next=chat": "/account?next=chat", "/account/email": "/account/email"} {
		page := a.getWith(t, path)
		if n := strings.Count(page, `<form class="theme-menu" method="post" action="/account/theme"><input type="hidden" name="back" value="`+html.EscapeString(back)+`">`); n != 2 {
			t.Fatalf("%s: %d theme forms back to %s", path, n, back)
		}
	}
	// The account page's CSP lets the form post to the gateway.
	resp, _ := a.get(t, "/account", "")
	if !strings.Contains(resp.Header.Get("Content-Security-Policy"), "form-action 'self'") {
		t.Fatalf("CSP %q", resp.Header.Get("Content-Security-Policy"))
	}
}

var (
	headNavRe = regexp.MustCompile(`(?s)<nav class="head-nav" aria-label="Main">(.*?)</nav>`)
	menuNavRe = regexp.MustCompile(`(?s)<div class="menu-pane"><nav aria-label="Main">(.*?)</nav>`)
	navLinkRe = regexp.MustCompile(`<a href="([^"]*)"( aria-current="page")?>([^<]*)</a>`)
)

// The header's navigation is the website's: the same items, in the same order, to the same
// places (site/src/data/nav.json, which the site's header renders), with Account marked.
func TestHeaderNavMatchesSite(t *testing.T) {
	raw, err := os.ReadFile("../site/src/data/nav.json")
	if err != nil {
		t.Fatal(err)
	}
	var site []struct{ Label, Href string }
	if err := json.Unmarshal(raw, &site); err != nil {
		t.Fatal(err)
	}
	base, _ := url.Parse("https://site.test/") // the rig's public URL; the chat is chat.site.test
	resolve := func(href string) string {
		u, err := url.Parse(strings.ReplaceAll(href, "{domain}", "site.test"))
		if err != nil {
			t.Fatal(err)
		}
		return base.ResolveReference(u).String()
	}
	var want []string
	for _, l := range site {
		want = append(want, l.Label+" "+resolve(l.Href))
	}
	if got := strings.Join(want, ", "); got != "Chat https://chat.site.test, Account https://site.test/account, Docs https://site.test/docs/, "+
		"Run a node https://site.test/node/, Network https://site.test/network/, Dev log https://site.test/devlog/, About https://site.test/about/" {
		t.Fatalf("site nav changed: %s", got)
	}
	g := threeWayRig(t)
	session, _ := g.signIn(t, 1)
	for _, p := range []struct{ path, session string }{{"/account?next=chat", ""}, {"/account", session}, {"/account/email", ""}} {
		page := g.getWith(t, p.path, p.session)
		for _, re := range []*regexp.Regexp{headNavRe, menuNavRe} {
			m := re.FindStringSubmatch(page)
			if m == nil {
				t.Fatalf("%s: no %s", p.path, re)
			}
			var got []string
			for _, l := range navLinkRe.FindAllStringSubmatch(m[1], -1) {
				got = append(got, html.UnescapeString(l[3])+" "+resolve(html.UnescapeString(l[1])))
				if (l[2] != "") != (l[3] == "Account") {
					t.Fatalf("%s: %q marked current: %v", p.path, l[3], l[2] != "")
				}
			}
			if strings.Join(got, ", ") != strings.Join(want, ", ") {
				t.Fatalf("%s: nav\n got %v\nwant %v", p.path, got, want)
			}
		}
		if !strings.Contains(page, `<a class="head-src" href="https://github.com/aisathome/aisathome">`) {
			t.Fatalf("%s: no source code link in the header", p.path)
		}
	}
}
