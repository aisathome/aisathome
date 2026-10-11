// Light/dark choice shared with the website. The site's theme select (Starlight, localStorage
// "starlight-theme") and the account pages' theme form both write the cookie aisathome_theme;
// the site's head script (site/src/scripts/theme-sync.js) applies it on load, and the gateway
// renders <html data-theme> from it. The last change wins on both.
//
// SPDX-License-Identifier: Apache-2.0
package gateway

import (
	"net/http"
	"net/url"
	"strings"
	"time"
)

// themeCookie holds "auto", "dark" or "light". It is not HttpOnly (the site's script reads and
// writes it) and has no Domain: the site and /account share the origin, and staging's site must
// not see prod's choice. It is not secret and authorises nothing.
const themeCookie = "aisathome_theme"

// themeOf is the visitor's explicit choice ("dark" or "light"), or "" to follow the system.
func themeOf(r *http.Request) string {
	if c, err := r.Cookie(themeCookie); err == nil && (c.Value == "dark" || c.Value == "light") {
		return c.Value
	}
	return ""
}

// handleTheme is the account pages' script-free theme control: POST theme=auto|dark|light and
// back=<the page it came from>; sets the cookie and goes back.
func (g *Gateway) handleTheme(w http.ResponseWriter, r *http.Request) {
	if !g.sameOrigin(r) {
		http.Error(w, "cross-site request", http.StatusForbidden)
		return
	}
	theme := r.PostFormValue("theme")
	if theme != "auto" && theme != "dark" && theme != "light" {
		http.Error(w, "theme must be auto, dark or light", http.StatusBadRequest)
		return
	}
	http.SetCookie(w, &http.Cookie{Name: themeCookie, Value: theme, Path: "/", MaxAge: int((365 * 24 * time.Hour).Seconds()),
		Secure: true, SameSite: http.SameSiteLaxMode})
	http.Redirect(w, r, g.acfg.PublicURL+safeBack(r.PostFormValue("back")), http.StatusSeeOther)
}

// safeBack returns back if it is a path on this site ("/account", "/account?next=chat"), else
// "/account": never another host ("//evil.test", "/\evil.test", "https://…").
func safeBack(back string) string {
	u, err := url.Parse(back)
	if err != nil || !strings.HasPrefix(back, "/") || strings.HasPrefix(back, "//") || strings.ContainsAny(back, "\\\r\n\t") ||
		u.Scheme != "" || u.Host != "" || u.User != nil || len(back) > 200 {
		return "/account"
	}
	return back
}
