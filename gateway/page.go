// Look of the gateway's own pages (the account page, email sign-in): the website's "volunteer
// science" theme (site/src/styles/global.css and brand.css: warm dark, one orange accent, Big
// Shoulders Display, Public Sans, JetBrains Mono), with a header and footer like the site's.
//
// Nothing loads from another origin. Fonts, the favicon and Google's sign-in button are
// embedded (assets/) and served by the gateway under /account/assets/ with a content hash in
// the name, so they can be cached for good. The site's own font files have hashed names that
// change per build, so the gateway carries its own copies (the same fontsource 5.3.0 latin
// subsets; other scripts fall back to system fonts).
//
// SPDX-License-Identifier: Apache-2.0
package gateway

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"html/template"
	"net/http"
	"path"
	"strings"
)

// Assets (gateway/assets/):
//   - public-sans, big-shoulders-display, jetbrains-mono .woff2: @fontsource-variable/* 5.3.0,
//     latin subset, SIL OFL 1.1 (the .OFL.txt next to each, also served);
//   - google-signin-{light,dark}.svg: Google's pre-approved "Sign in with Google" buttons
//     (Android + Web, square, with text), unaltered, from
//     https://developers.google.com/static/identity/images/signin-assets.zip (2026-10-11);
//   - favicon.svg: a copy of site/public/favicon.svg.
//
//go:embed assets
var assetFS embed.FS

type asset struct {
	body  []byte
	ctype string
}

// assets maps a served (hashed) name to its file; assetURLs maps a file name to its URL.
var assets, assetURLs = loadAssets()

func loadAssets() (map[string]asset, map[string]string) {
	types := map[string]string{".woff2": "font/woff2", ".svg": "image/svg+xml", ".txt": "text/plain; charset=utf-8"}
	files, err := assetFS.ReadDir("assets")
	if err != nil {
		panic(err)
	}
	byName, urls := map[string]asset{}, map[string]string{}
	for _, f := range files {
		name := f.Name()
		body, err := assetFS.ReadFile("assets/" + name)
		if err != nil {
			panic(err)
		}
		ext := path.Ext(name)
		sum := sha256.Sum256(body)
		served := strings.TrimSuffix(name, ext) + "." + hex.EncodeToString(sum[:5]) + ext
		byName[served] = asset{body, types[ext]}
		urls[name] = "/account/assets/" + served
	}
	return byName, urls
}

// assetURL is the URL of an embedded file; a missing one is a programming error.
func assetURL(name string) string {
	u, ok := assetURLs[name]
	if !ok {
		panic("gateway: no asset " + name)
	}
	return u
}

func handleAsset(w http.ResponseWriter, r *http.Request) {
	a, ok := assets[r.PathValue("name")]
	if !ok || a.ctype == "" {
		http.NotFound(w, r)
		return
	}
	h := w.Header()
	h.Set("Content-Type", a.ctype)
	h.Set("Cache-Control", "public, max-age=31536000, immutable") // the name changes with the content
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'") // SVGs opened directly
	w.Write(a.body)
}

// pageCSP is the Content-Security-Policy of the gateway's pages, minus scripts and form
// targets: styles inline, fonts and images only from the gateway itself.
const pageCSP = "default-src 'none'; style-src 'unsafe-inline'; font-src 'self'; img-src 'self'"

// shell is the data every page's header needs; page data embeds it.
type shell struct {
	Title   string // the page's title, before " · AIs@home"
	ChatURL string // the header's Chat link ("" leaves it out)
	Theme   string // "dark" or "light" from the theme cookie; "" follows the system
	Back    string // where the theme form returns to: this page, as a GET
}

// navLink is an item of the header's main navigation.
type navLink struct {
	Label, Href string
	Current     bool
}

// headerNav is the website's header navigation, in its order and with its links
// (site/src/data/nav.json; e2e/header_test.go checks they match). Every gateway page is an
// account page, so Account is the current one.
func headerNav(chatURL string) []navLink {
	var nav []navLink
	if chatURL != "" {
		nav = append(nav, navLink{Label: "Chat", Href: chatURL})
	}
	return append(nav, navLink{"Account", "/account", true}, navLink{"Docs", "/docs/", false},
		navLink{"Run a node", "/node/", false}, navLink{"Network", "/network/", false},
		navLink{"Science", "/science/tauceti/", false}, navLink{"Dev log", "/devlog/", false}, navLink{"About", "/about/", false})
}

// newPage parses a page that uses the shared shell (the data embeds shell):
//
//	{{template "head" .}}{{template "top" .}} ... {{template "bottom" .}}
func newPage(name string, funcs template.FuncMap, body string) *template.Template {
	return template.Must(template.New(name).Funcs(template.FuncMap{"asset": assetURL, "nav": headerNav}).Funcs(funcs).Parse(pageShell + body))
}

// The header follows the site's (Starlight's header with site/src/components/HeaderLinks.astro):
// logo and wordmark at the left, then the main navigation, the source code link and the theme
// control at the right. There is no search box (it needs the site's Pagefind script). Below
// 72rem the navigation moves into a menu, and below 50rem the source link and the theme
// control too, as in Starlight's mobile menu; the menu is a <details>, so no script.
var pageShell = `{{define "head"}}<!doctype html>
<html lang="en"{{with .Theme}} data-theme="{{.}}"{{end}}><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<meta name="robots" content="noindex"><meta name="color-scheme" content="dark light"><title>{{.Title}} · AIs@home</title>
<link rel="icon" href="{{asset "favicon.svg"}}" type="image/svg+xml">
` + pageStyle + `</head>{{end}}
{{define "navlinks"}}{{range nav .ChatURL}}<a href="{{.Href}}"{{if .Current}} aria-current="page"{{end}}>{{.Label}}</a>{{end}}{{end}}
{{define "source"}}<a class="head-src" href="https://github.com/aisathome/aisathome"><span class="sr-only">Source code</span>` + iconSource + `</a>{{end}}
{{define "theme"}}<details class="theme"><summary><span class="sr-only">Select theme: </span>{{if eq .Theme "dark"}}` + iconMoon + `<span>Dark</span>{{else if eq .Theme "light"}}` + iconSun + `<span>Light</span>{{else}}` + iconLaptop + `<span>Auto</span>{{end}}` + iconCaret + `</summary>
<form class="theme-menu" method="post" action="/account/theme"><input type="hidden" name="back" value="{{.Back}}">
<button name="theme" value="dark"{{if eq .Theme "dark"}} aria-pressed="true"{{end}}>` + iconMoon + `Dark</button>
<button name="theme" value="light"{{if eq .Theme "light"}} aria-pressed="true"{{end}}>` + iconSun + `Light</button>
<button name="theme" value="auto"{{if not .Theme}} aria-pressed="true"{{end}}>` + iconLaptop + `Auto</button></form></details>{{end}}
{{define "top"}}<body><a class="skip" href="#main">Skip to content</a>
<header class="site-head">
<a class="brand" href="/">` + logoSVG + `<span>AIs@home</span></a>
<div class="head-right"><nav class="head-nav" aria-label="Main">{{template "navlinks" .}}</nav>
<div class="head-src-wrap">{{template "source" .}}</div>{{template "theme" .}}</div>
<details class="head-menu"><summary>` + iconBars + iconClose + `<span class="sr-only">Menu</span></summary>
<div class="menu-pane"><nav aria-label="Main">{{template "navlinks" .}}</nav>
<div class="menu-foot">{{template "source" .}}{{template "theme" .}}</div></div></details>
</header>
<main id="main" class="wrap">{{end}}
{{define "bottom"}}</main>
<footer class="site-foot"><div class="wrap">
<div><p class="foot-name">AIs@home</p><p>Open AI models on volunteers’ GPUs. Non-commercial, open source, run by volunteers.</p></div>
<nav aria-label="Footer"><ul><li><a href="/use/chat/">Docs</a></li><li><a href="/about/privacy/">Privacy</a></li>
<li><a href="/about/terms/">Terms</a></li><li><a href="https://github.com/aisathome/aisathome">Source code</a></li>
<li><a href="https://www.reddit.com/r/aisathome/">r/aisathome</a></li>
<li><a href="mailto:contact@aisathome.org">contact@aisathome.org</a></li></ul></nav>
</div></footer></body></html>{{end}}`

// Starlight's icons (@astrojs/starlight 0.42.6, components-internals/Icons.js; MIT), as the
// site's header draws them. iconSource is Starlight's "github" icon, which the site's header uses
// for the source code link.
const (
	svg24      = `<svg viewBox="0 0 24 24" width="16" height="16" fill="currentColor" aria-hidden="true"`
	iconLaptop = svg24 + ` class="ic"><path d="M21 14h-1V7a3 3 0 0 0-3-3H7a3 3 0 0 0-3 3v7H3a1 1 0 0 0-1 1v2a3 3 0 0 0 3 3h14a3 3 0 0 0 3-3v-2a1 1 0 0 0-1-1ZM6 7a1 1 0 0 1 1-1h10a1 1 0 0 1 1 1v7H6V7Zm14 10a1 1 0 0 1-1 1H5a1 1 0 0 1-1-1v-1h16v1Z"/></svg>`
	iconSun    = svg24 + ` class="ic"><path d="M5 12a1 1 0 0 0-1-1H3a1 1 0 0 0 0 2h1a1 1 0 0 0 1-1Zm.64 5-.71.71a1 1 0 0 0 0 1.41 1 1 0 0 0 1.41 0l.71-.71A1 1 0 0 0 5.64 17ZM12 5a1 1 0 0 0 1-1V3a1 1 0 0 0-2 0v1a1 1 0 0 0 1 1Zm5.66 2.34a1 1 0 0 0 .7-.29l.71-.71a1 1 0 1 0-1.41-1.41l-.66.71a1 1 0 0 0 0 1.41 1 1 0 0 0 .66.29Zm-12-.29a1 1 0 0 0 1.41 0 1 1 0 0 0 0-1.41l-.71-.71a1.004 1.004 0 1 0-1.43 1.41l.73.71ZM21 11h-1a1 1 0 0 0 0 2h1a1 1 0 0 0 0-2Zm-2.64 6A1 1 0 0 0 17 18.36l.71.71a1 1 0 0 0 1.41 0 1 1 0 0 0 0-1.41l-.76-.66ZM12 6.5a5.5 5.5 0 1 0 5.5 5.5A5.51 5.51 0 0 0 12 6.5Zm0 9a3.5 3.5 0 1 1 0-7 3.5 3.5 0 0 1 0 7Zm0 3.5a1 1 0 0 0-1 1v1a1 1 0 0 0 2 0v-1a1 1 0 0 0-1-1Z"/></svg>`
	iconMoon   = svg24 + ` class="ic"><path d="M21.64 13a1 1 0 0 0-1.05-.14 8.049 8.049 0 0 1-3.37.73 8.15 8.15 0 0 1-8.14-8.1 8.59 8.59 0 0 1 .25-2A1 1 0 0 0 8 2.36a10.14 10.14 0 1 0 14 11.69 1 1 0 0 0-.36-1.05Zm-9.5 6.69A8.14 8.14 0 0 1 7.08 5.22v.27a10.15 10.15 0 0 0 10.14 10.14 9.784 9.784 0 0 0 2.1-.22 8.11 8.11 0 0 1-7.18 4.32v-.04Z"/></svg>`
	iconCaret  = svg24 + ` class="caret"><path d="M17 9.17a1 1 0 0 0-1.41 0L12 12.71 8.46 9.17a1 1 0 1 0-1.41 1.42l4.24 4.24a1.002 1.002 0 0 0 1.42 0L17 10.59a1.002 1.002 0 0 0 0-1.42Z"/></svg>`
	iconBars   = svg24 + ` class="open"><path d="M3 8h18a1 1 0 1 0 0-2H3a1 1 0 0 0 0 2Zm18 8H3a1 1 0 0 0 0 2h18a1 1 0 0 0 0-2Zm0-5H3a1 1 0 0 0 0 2h18a1 1 0 0 0 0-2Z"/></svg>`
	iconClose  = svg24 + ` class="close"><path d="m13.41 12 6.3-6.29a1.004 1.004 0 1 0-1.42-1.42L12 10.59l-6.29-6.3a1.004 1.004 0 0 0-1.42 1.42l6.3 6.29-6.3 6.29a1 1 0 0 0 0 1.42.998.998 0 0 0 1.42 0l6.29-6.3 6.29 6.3a.999.999 0 0 0 1.42 0 1 1 0 0 0 0-1.42L13.41 12Z"/></svg>`
	iconSource = svg24 + `><path d="M12 .3a12 12 0 0 0-3.8 23.38c.6.12.83-.26.83-.57L9 21.07c-3.34.72-4.04-1.61-4.04-1.61-.55-1.39-1.34-1.76-1.34-1.76-1.08-.74.09-.73.09-.73 1.2.09 1.83 1.24 1.83 1.24 1.08 1.83 2.81 1.3 3.5 1 .1-.78.42-1.31.76-1.61-2.67-.3-5.47-1.33-5.47-5.93 0-1.31.47-2.38 1.24-3.22-.14-.3-.54-1.52.1-3.18 0 0 1-.32 3.3 1.23a11.5 11.5 0 0 1 6 0c2.28-1.55 3.29-1.23 3.29-1.23.64 1.66.24 2.88.12 3.18a4.65 4.65 0 0 1 1.23 3.22c0 4.61-2.8 5.63-5.48 5.92.42.36.81 1.1.81 2.22l-.01 3.29c0 .31.2.69.82.57A12 12 0 0 0 12 .3Z"/></svg>`
)

// logoSVG is site/src/assets/logo.svg.
const logoSVG = `<svg viewBox="0 0 32 32" width="32" height="32" aria-hidden="true"><rect width="32" height="32" rx="7" fill="#c2410c"/><text x="16" y="22" text-anchor="middle" font-family="ui-monospace,SFMono-Regular,Menlo,monospace" font-size="17" font-weight="700" fill="#fff8f0">@</text></svg>`

// githubMark is GitHub's mark, unaltered, from primer/octicons (icons/mark-github-16.svg). Use per
// https://github.com/logos: the mark in black or white, not recoloured or changed.
const githubMark = `<svg viewBox="0 0 16 16" width="20" height="20" aria-hidden="true"><path d="M6.766 11.328c-2.063-.25-3.516-1.734-3.516-3.656 0-.781.281-1.625.75-2.188-.203-.515-.172-1.609.063-2.062.625-.078 1.468.25 1.968.703.594-.187 1.219-.281 1.985-.281.765 0 1.39.094 1.953.265.484-.437 1.344-.765 1.969-.687.218.422.25 1.515.046 2.047.5.593.766 1.39.766 2.203 0 1.922-1.453 3.375-3.547 3.64.531.344.89 1.094.89 1.954v1.625c0 .468.391.734.86.547C13.781 14.359 16 11.53 16 8.03 16 3.61 12.406 0 7.984 0 3.563 0 0 3.61 0 8.031a7.88 7.88 0 0 0 5.172 7.422c.422.156.828-.125.828-.547v-1.25c-.219.094-.5.156-.75.156-1.031 0-1.64-.562-2.078-1.609-.172-.422-.36-.672-.719-.719-.187-.015-.25-.093-.25-.187 0-.188.313-.328.625-.328.453 0 .844.281 1.25.86.313.452.64.655 1.031.655s.641-.14 1-.5c.266-.265.47-.5.657-.656"/></svg>`

// pageStyle is shared by the account and email pages. Dark is the main look; light follows the
// visitor's system setting, or their choice in the theme control (<html data-theme>, from the
// cookie the site shares, theme.go). Classes for page sections:
//
//	layout     .wrap (centred column)  .card (panel)  .card.accent (orange top rule)
//	           .card.err / .card.ok (notices)  .card-head (h2 + badge or button on one line)
//	text       h1, h2 (display face, upper case)  .mut (muted)  .small  .back (back link)
//	           code, .key (an API key or token shown once: selectable block)
//	buttons    .btn (primary)  .btn.secondary (outline)  .btn.ghost (quiet)  .btn.danger
//	           .btn.sm (compact; buttons in tables are compact anyway)  .actions (a row of buttons)
//	forms      form.form (block form)  .field (label + control + .hint)  .row (controls inline)
//	           .check (checkbox or radio + label)  fieldset/legend  .hint
//	tables     table (full width)  th  .num (right-aligned figures)  .end (right-aligned cell)
//	           .table-wrap (scrolls sideways on narrow screens)
//	data       dl.kv (label/value pairs)
//	status     .badge + .online | .busy | .offline | .probation | .paused | .held | .banned (dot + text, so the
//	           state never depends on colour alone)
//	sign-in    .signin (provider buttons)  .btn-gh  .btn-google (brand buttons, see above)
//
// Keep provider names out of the CSS and the shell: a test checks that a page without GitHub
// sign-in never says "GitHub".
var pageStyle = `<style>
@font-face{font-family:"Public Sans Variable";font-style:normal;font-display:swap;font-weight:100 900;src:url(` + publicSans + `) format("woff2");unicode-range:` + latinRange + `}
@font-face{font-family:"Big Shoulders Display Variable";font-style:normal;font-display:swap;font-weight:100 900;src:url(` + bigShoulders + `) format("woff2");unicode-range:` + latinRange + `}
@font-face{font-family:"JetBrains Mono Variable";font-style:normal;font-display:swap;font-weight:100 800;src:url(` + jetbrainsMono + `) format("woff2");unicode-range:` + latinRange + `}
:root{color-scheme:dark light;
--sans:"Public Sans Variable",ui-sans-serif,system-ui,sans-serif;--display:"Big Shoulders Display Variable","Arial Narrow",sans-serif;
--mono:"JetBrains Mono Variable",ui-monospace,SFMono-Regular,Menlo,monospace;` + darkTokens + `}
:root[data-theme=dark]{color-scheme:dark}
@media (prefers-color-scheme:light){:root:not([data-theme=dark]){` + lightTokens + `}}
:root[data-theme=light]{color-scheme:light;` + lightTokens + `}
*,::before,::after{box-sizing:border-box}
body{margin:0;min-height:100vh;display:flex;flex-direction:column;background:var(--bg);color:var(--fg);font:16px/1.6 var(--sans)}
main{flex:1;padding-block:2.5rem 3rem}
.wrap{width:100%;max-width:48rem;margin:0 auto;padding-inline:1rem}
a{color:var(--link);text-underline-offset:3px}a:hover{color:var(--acc)}
strong{color:var(--head)}
:where(a,button,input,select,textarea,summary,[tabindex]):focus-visible{outline:2px solid var(--acc);outline-offset:3px}
.skip{position:absolute;left:-999rem;top:.5rem;padding:.5rem 1rem;background:var(--surface);color:var(--head)}.skip:focus{left:1rem;z-index:5}
.sr-only{position:absolute;width:1px;height:1px;padding:0;margin:-1px;overflow:hidden;clip:rect(0,0,0,0);white-space:nowrap;border:0}
.site-foot .wrap{max-width:72rem}
` + headerStyle + `
.site-foot{border-top:1px solid var(--line);color:var(--mut);font-size:.875rem}
.site-foot .wrap{display:flex;flex-wrap:wrap;gap:1.25rem 3rem;justify-content:space-between;align-items:flex-start;padding-block:2rem 1.5rem}
.site-foot p{margin:0;max-width:28rem}.site-foot .foot-name{font:700 1rem/1.4 var(--display);color:var(--head);margin-bottom:.25rem}
.site-foot ul{display:flex;flex-wrap:wrap;gap:.5rem 1.5rem;list-style:none;margin:0;padding:0}
.site-foot a{color:var(--fg);text-decoration:none}.site-foot a:hover{color:var(--acc-hi);text-decoration:underline}
h1,h2,h3{color:var(--head);font-family:var(--display);text-transform:uppercase;line-height:.95}
h1{font-size:clamp(2.6rem,8vw,3.6rem);font-weight:900;letter-spacing:-.005em;margin:0 0 1.5rem}
h2{font-size:1.75rem;font-weight:800;letter-spacing:.01em;margin:2rem 0 .75rem}
h3{font-size:1.3rem;font-weight:800;margin:1.25rem 0 .5rem}
p{margin:.75rem 0}
.card{background:var(--surface);border:1px solid var(--line);border-radius:.5rem;padding:1.25rem 1.4rem;margin:1rem 0}
.card>:first-child{margin-top:0}.card>:last-child{margin-bottom:0}
.card.accent{border-top:5px solid var(--acc)}
.card.err{border-color:var(--bad);border-left:5px solid var(--bad)}
.card.ok{border-color:var(--ok);border-left:5px solid var(--ok)}
.card-head{display:flex;flex-wrap:wrap;align-items:center;justify-content:space-between;gap:.5rem 1rem;margin-bottom:.75rem}.card-head>*{margin:0}
.mut{color:var(--mut)}.small{font-size:.9rem}.back{margin:0 0 1rem;font-size:.9rem}
code,kbd,pre{font-family:var(--mono);font-size:.9em}
.key{display:block;word-break:break-all;background:var(--bg);color:var(--head);padding:.65rem .8rem;border-radius:.4rem;border:1px solid var(--acc);user-select:all}
.btn{display:inline-flex;align-items:center;justify-content:center;gap:.5rem;min-height:2.75rem;padding:.55rem 1.3rem;border:2px solid var(--acc);border-radius:.4rem;
background:var(--acc);color:var(--on-acc);font:800 1.15rem/1 var(--display);letter-spacing:.04em;text-transform:uppercase;text-decoration:none;cursor:pointer;
transition:background-color .15s,color .15s,border-color .15s}
.btn:hover{background:var(--acc-hi);border-color:var(--acc-hi);color:var(--on-acc)}
.btn.secondary{background:transparent;color:var(--head);border-color:var(--head)}.btn.secondary:hover{color:var(--acc);border-color:var(--acc)}
.btn.ghost{background:transparent;color:var(--fg);border:1px solid var(--line-hi)}.btn.ghost:hover{color:var(--acc);border-color:var(--acc)}
.btn.danger{background:transparent;color:var(--bad);border-color:var(--bad)}.btn.danger:hover{background:var(--bad);color:var(--bg)}
.btn.sm,td .btn{min-height:2.1rem;padding:.3rem .8rem;font-size:1rem}
.btn:disabled{opacity:.5;cursor:not-allowed}
.actions{display:flex;flex-wrap:wrap;gap:.6rem;align-items:center;margin:1rem 0}
form{display:inline}form.form{display:block;margin:1rem 0}
.field{margin:1rem 0}.field>label,legend{display:block;font-weight:600;color:var(--head);margin-bottom:.35rem}
.row{display:flex;flex-wrap:wrap;gap:.6rem;align-items:center}
.check{display:flex;gap:.6rem;align-items:flex-start;margin:.5rem 0}.check input{margin:.3rem 0 0;width:1.1rem;height:1.1rem;flex:none}
fieldset{border:0;padding:0;margin:1rem 0}
.hint{font-size:.9rem;color:var(--mut);margin:.3rem 0 0}
input,select,textarea{font:inherit;color:var(--head);background:var(--bg);border:1px solid var(--line-hi);border-radius:.4rem;padding:.5rem .75rem;max-width:100%;min-height:2.75rem}
input:focus-visible,select:focus-visible,textarea:focus-visible{outline-offset:1px;border-color:var(--acc)}
input[type=checkbox],input[type=radio]{accent-color:var(--acc);min-height:0}
table{width:100%;border-collapse:collapse}
th{text-align:left;font-size:.78rem;font-weight:600;letter-spacing:.05em;text-transform:uppercase;color:var(--mut);padding:.4rem .75rem .4rem 0;border-bottom:1px solid var(--line-hi)}
td{padding:.5rem .75rem .5rem 0;border-bottom:1px solid var(--line);vertical-align:middle}
th:last-child,td:last-child{padding-right:0}tr:last-child td{border-bottom:0}
.num{text-align:right;font-variant-numeric:tabular-nums}.end{text-align:right}
.table-wrap{overflow-x:auto}td code{white-space:nowrap}
@media (max-width:30rem){.card{padding:1rem}}
dl.kv{display:grid;grid-template-columns:max-content 1fr;gap:.35rem 1.25rem;margin:1rem 0}dl.kv dt{color:var(--mut)}dl.kv dd{margin:0}
.badge{display:inline-flex;align-items:center;gap:.4em;padding:.05em .6em;border:1px solid currentColor;border-radius:999px;font-size:.75rem;font-weight:700;
letter-spacing:.05em;text-transform:uppercase;line-height:1.6;white-space:nowrap;color:var(--mut)}
.badge::before{content:"";width:.55em;height:.55em;border-radius:50%;background:currentColor}
.badge.online{color:var(--ok)}.badge.busy{color:var(--acc)}.badge.probation{color:var(--warn)}.badge.paused{color:var(--info)}.badge.held,.badge.banned{color:var(--bad)}
.badge.offline::before{background:transparent;box-shadow:inset 0 0 0 1.5px currentColor}
.signin{display:flex;flex-wrap:wrap;gap:.75rem;margin:1.25rem 0}
.btn-gh,.btn-google{display:inline-flex;border-radius:4px;text-decoration:none}
.btn-gh{align-items:center;gap:10px;height:40px;min-width:180px;padding:0 12px;border:1px solid #747775;background:#fff;color:#1f1f1f;font:500 14px/20px var(--sans)}
.btn-gh svg{flex:none;fill:#000}
.btn-google img{display:block;height:40px;width:auto}
.btn-gh:hover,.btn-google:hover{box-shadow:0 1px 3px rgba(0,0,0,.35)}
@media (prefers-color-scheme:dark){:root:not([data-theme=light]) .btn-gh{` + ghDark + `}:root:not([data-theme=light]) .btn-gh svg{fill:#fff}}
:root[data-theme=dark] .btn-gh{` + ghDark + `}:root[data-theme=dark] .btn-gh svg{fill:#fff}
@media (prefers-reduced-motion:reduce){*{transition:none!important}}
</style>
`

// The colours: dark (the main look) and light. The header's match the site's Starlight colours
// (--nav-line: --sl-color-hairline-shade, --hair: the mobile menu's rule, --menu-*: its button).
const (
	darkTokens = `--bg:#15110e;--nav:#1f1a16;--nav-line:#15110e;--hair:#1f1a16;--surface:#1f1a16;--band:#22180f;--fg:#d8cdc1;--head:#fff8f0;--mut:#a99d91;
--line:#332c26;--line-hi:#4b423a;--acc:#ff9842;--acc-hi:#ffc896;--acc-low:#3a1c06;--on-acc:#1c0d02;--link:#ffc896;
--menu-bg:#fff8f0;--menu-fg:#15110e;--shadow:0 8px 4px #00000014,0 5px 2px #00000014,0 3px 2px #0000001f,0 1px 1px #00000026;
--ok:#5bd68a;--warn:#f5c84b;--info:#8fb8ff;--bad:#ff7a6e`
	lightTokens = `--bg:#fdf8f1;--nav:#f3ebe0;--nav-line:#ece3d8;--hair:#ece3d8;--surface:#f6ede1;--band:#f8e6d2;--fg:#332c26;--head:#15110e;--mut:#6b5e52;
--line:#d8cdc1;--line-hi:#a99d91;--acc:#b4460c;--acc-hi:#8a3407;--acc-low:#fde3c8;--on-acc:#fff;--link:#b4460c;
--menu-bg:#fdf8f1;--menu-fg:#15110e;--shadow:0 8px 4px #00000008,0 5px 2px #00000008,0 3px 2px #0000000f,0 1px 1px #0000000f;
--ok:#17703b;--warn:#8a5a00;--info:#2557c7;--bad:#b42318`
	ghDark = `background:#131314;border-color:#8e918f;color:#e3e3e3`
)

// headerStyle follows Starlight's header (components/Header.astro, SiteTitle.astro,
// SocialIcons.astro, Select.astro, MobileMenuToggle.astro; props.css: 3.5rem high with 1rem side
// padding, 4rem and 1.5rem from 50rem, 1.5rem gaps from 72rem) and site/src/components/HeaderLinks.astro (the links from
// 72rem). The menu pane takes the place of Starlight's mobile sidebar.
const headerStyle = `.site-head{--nav-h:3.5rem;--pad-x:1rem;--gap:1rem;--logo:2rem;position:relative;z-index:3;display:flex;align-items:center;gap:var(--gap);
height:var(--nav-h);padding:.75rem var(--pad-x);background:var(--nav);border-bottom:1px solid var(--nav-line)}
@media (min-width:50rem){.site-head{--nav-h:4rem;--pad-x:1.5rem;--logo:2.5rem}}
.brand{display:flex;align-items:center;gap:var(--gap);min-width:0;margin-right:auto;color:var(--link);text-decoration:none;white-space:nowrap;
font:800 1.6rem/1.75 var(--display);letter-spacing:.01em}
.brand span{overflow:hidden}.brand svg{width:var(--logo);height:var(--logo);flex:none;letter-spacing:0}
.head-right{display:none;align-items:center;gap:1rem}
.head-nav{display:none;gap:1.25rem;margin-inline-end:.75rem}
.head-nav a{color:var(--fg);text-decoration:none;font-size:.875rem;line-height:1.75}
.head-nav a:hover{color:var(--acc-hi)}.head-nav a[aria-current]{color:var(--head);box-shadow:inset 0 -2px var(--acc)}
.head-src-wrap{display:flex;align-items:center;gap:1rem}.head-src-wrap::after{content:"";height:2rem;border-inline-end:1px solid var(--line)}
.head-src{display:flex;padding:.5em;margin:-.5em;color:var(--link)}.head-src:hover{color:var(--head)}.head-src svg{width:1rem;height:1rem}
summary{list-style:none;cursor:pointer}summary::-webkit-details-marker{display:none}
.theme{position:relative;font-size:.875rem}
.theme summary{position:relative;width:calc(6.25em + 1rem);margin-inline:-.5rem;padding:.625rem 2rem .625rem 1.625rem;line-height:1.75;color:var(--fg);white-space:nowrap}
.theme summary:hover,.theme[open] summary{color:var(--head)}
.theme summary svg{position:absolute;top:50%;transform:translateY(-50%)}
.theme summary .ic{left:.5rem;width:.875rem;height:.875rem}.theme summary .caret{right:.5rem;width:1.25rem;height:1.25rem}
form.theme-menu{position:absolute;right:-.5rem;top:calc(100% + .25rem);z-index:4;display:flex;flex-direction:column;min-width:9rem;padding:.25rem;
background:var(--nav);border:1px solid var(--line);border-radius:.5rem;box-shadow:var(--shadow)}
.theme-menu button{display:flex;align-items:center;gap:.6rem;width:100%;padding:.45rem .7rem;border:0;border-radius:.3rem;background:none;color:var(--fg);
font:inherit;line-height:1.5;text-align:left;cursor:pointer}
.theme-menu button:hover{background:var(--line);color:var(--head)}.theme-menu button[aria-pressed=true]{color:var(--head);font-weight:700}
.theme-menu svg{width:.875rem;height:.875rem;flex:none}
.head-menu>summary{display:flex;align-items:center;justify-content:center;width:2rem;height:2rem;padding:.5rem;border-radius:50%;
background:var(--menu-bg);color:var(--menu-fg);box-shadow:var(--shadow)}
.head-menu>summary svg{width:1rem;height:1rem;flex:none}
.head-menu[open]>summary{background:#d8cdc1;box-shadow:none}.head-menu>summary .close,.head-menu[open]>summary .open{display:none}.head-menu[open]>summary .close{display:block}
.menu-pane{position:fixed;inset:var(--nav-h) 0 0;z-index:3;overflow-y:auto;padding:1rem var(--pad-x);background:var(--bg)}
.menu-pane nav{display:flex;flex-direction:column;gap:.125rem}
.menu-pane nav a{padding:.3em .5rem;border-radius:.25rem;color:var(--fg);text-decoration:none}
.menu-pane nav a:hover{color:var(--acc-hi)}.menu-pane nav a[aria-current]{background:var(--link);color:var(--acc-low);font-weight:600}
.menu-foot{display:flex;align-items:center;justify-content:space-between;gap:1rem;margin-top:1rem;padding:.5rem 0;border-top:1px solid var(--hair)}
.menu-foot .head-src{margin-block:0}
@media (max-width:49.99rem){body:has(.head-menu[open]){overflow:hidden}}
@media (min-width:50rem){.head-right{display:flex}.menu-foot{display:none}
.menu-pane{position:absolute;inset:calc(100% + .25rem) var(--pad-x) auto auto;min-width:14rem;padding:.5rem;background:var(--nav);
border:1px solid var(--line);border-radius:.5rem;box-shadow:var(--shadow)}}
@media (min-width:72rem){.site-head{--gap:1.5rem}.head-nav{display:flex;align-items:center}.head-menu{display:none}}`

// Font URLs (embedded, see assets above) and fontsource's latin unicode-range.
var (
	publicSans    = assetURL("public-sans.woff2")
	bigShoulders  = assetURL("big-shoulders-display.woff2")
	jetbrainsMono = assetURL("jetbrains-mono.woff2")
)

const latinRange = "U+0000-00FF,U+0131,U+0152-0153,U+02BB-02BC,U+02C6,U+02DA,U+02DC,U+0304,U+0308,U+0329,U+2000-206F,U+20AC,U+2122,U+2191,U+2193,U+2212,U+2215,U+FEFF,U+FFFD"
