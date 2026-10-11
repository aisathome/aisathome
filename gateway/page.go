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

// newPage parses a page that uses the shared shell:
//
//	{{template "head" "Title"}}{{template "top" .}} ... {{template "bottom" .}}
//
// "top" shows a Chat link when the data has a non-empty ChatURL field.
func newPage(name string, funcs template.FuncMap, body string) *template.Template {
	return template.Must(template.New(name).Funcs(template.FuncMap{"asset": assetURL}).Funcs(funcs).Parse(pageShell + body))
}

var pageShell = `{{define "head"}}<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<meta name="robots" content="noindex"><meta name="color-scheme" content="dark light"><title>{{.}} · AIs@home</title>
<link rel="icon" href="{{asset "favicon.svg"}}" type="image/svg+xml">
` + pageStyle + `</head>{{end}}
{{define "top"}}<body><a class="skip" href="#main">Skip to content</a>
<header class="site-head"><div class="wrap">
<a class="brand" href="/">` + logoSVG + `<span>AIs@home</span></a>
<nav aria-label="Main">{{with .ChatURL}}<a href="{{.}}">Chat</a>{{end}}<a href="/use/chat/">Docs</a><a href="/account" aria-current="page">Account</a></nav>
</div></header>
<main id="main" class="wrap">{{end}}
{{define "bottom"}}</main>
<footer class="site-foot"><div class="wrap">
<div><p class="foot-name">AIs@home</p><p>Open AI models on volunteers’ GPUs. Non-commercial, open source, run by volunteers.</p></div>
<nav aria-label="Footer"><ul><li><a href="/use/chat/">Docs</a></li><li><a href="/about/privacy/">Privacy</a></li>
<li><a href="/about/terms/">Terms</a></li><li><a href="https://github.com/aisathome/aisathome">Source code</a></li>
<li><a href="mailto:contact@aisathome.org">contact@aisathome.org</a></li></ul></nav>
</div></footer></body></html>{{end}}`

// logoSVG is site/src/assets/logo.svg.
const logoSVG = `<svg viewBox="0 0 32 32" width="32" height="32" aria-hidden="true"><rect width="32" height="32" rx="7" fill="#c2410c"/><text x="16" y="22" text-anchor="middle" font-family="ui-monospace,SFMono-Regular,Menlo,monospace" font-size="17" font-weight="700" fill="#fff8f0">@</text></svg>`

// githubMark is GitHub's mark, unaltered, from primer/octicons (icons/mark-github-16.svg). Use per
// https://github.com/logos: the mark in black or white, not recoloured or changed.
const githubMark = `<svg viewBox="0 0 16 16" width="20" height="20" aria-hidden="true"><path d="M6.766 11.328c-2.063-.25-3.516-1.734-3.516-3.656 0-.781.281-1.625.75-2.188-.203-.515-.172-1.609.063-2.062.625-.078 1.468.25 1.968.703.594-.187 1.219-.281 1.985-.281.765 0 1.39.094 1.953.265.484-.437 1.344-.765 1.969-.687.218.422.25 1.515.046 2.047.5.593.766 1.39.766 2.203 0 1.922-1.453 3.375-3.547 3.64.531.344.89 1.094.89 1.954v1.625c0 .468.391.734.86.547C13.781 14.359 16 11.53 16 8.03 16 3.61 12.406 0 7.984 0 3.563 0 0 3.61 0 8.031a7.88 7.88 0 0 0 5.172 7.422c.422.156.828-.125.828-.547v-1.25c-.219.094-.5.156-.75.156-1.031 0-1.64-.562-2.078-1.609-.172-.422-.36-.672-.719-.719-.187-.015-.25-.093-.25-.187 0-.188.313-.328.625-.328.453 0 .844.281 1.25.86.313.452.64.655 1.031.655s.641-.14 1-.5c.266-.265.47-.5.657-.656"/></svg>`

// pageStyle is shared by the account and email pages. Dark is the main look, light follows the
// visitor's system setting (no script, so no theme toggle). Classes for page sections:
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
--mono:"JetBrains Mono Variable",ui-monospace,SFMono-Regular,Menlo,monospace;
--bg:#15110e;--nav:#1f1a16;--surface:#1f1a16;--band:#22180f;--fg:#d8cdc1;--head:#fff8f0;--mut:#a99d91;--line:#332c26;--line-hi:#4b423a;
--acc:#ff9842;--acc-hi:#ffc896;--acc-low:#3a1c06;--on-acc:#1c0d02;--link:#ffc896;
--ok:#5bd68a;--warn:#f5c84b;--info:#8fb8ff;--bad:#ff7a6e}
@media (prefers-color-scheme:light){:root{
--bg:#fdf8f1;--nav:#f3ebe0;--surface:#f6ede1;--band:#f8e6d2;--fg:#332c26;--head:#15110e;--mut:#6b5e52;--line:#d8cdc1;--line-hi:#a99d91;
--acc:#b4460c;--acc-hi:#8a3407;--acc-low:#fde3c8;--on-acc:#fff;--link:#b4460c;
--ok:#17703b;--warn:#8a5a00;--info:#2557c7;--bad:#b42318}}
*,::before,::after{box-sizing:border-box}
body{margin:0;min-height:100vh;display:flex;flex-direction:column;background:var(--bg);color:var(--fg);font:16px/1.6 var(--sans)}
main{flex:1;padding-block:2.5rem 3rem}
.wrap{width:100%;max-width:48rem;margin:0 auto;padding-inline:1rem}
a{color:var(--link);text-underline-offset:3px}a:hover{color:var(--acc)}
strong{color:var(--head)}
:where(a,button,input,select,textarea,summary,[tabindex]):focus-visible{outline:2px solid var(--acc);outline-offset:3px}
.skip{position:absolute;left:-999rem;top:.5rem;padding:.5rem 1rem;background:var(--surface);color:var(--head)}.skip:focus{left:1rem;z-index:2}
.site-head{background:var(--nav);border-bottom:1px solid var(--line)}
.site-head .wrap,.site-foot .wrap{max-width:72rem}
.site-head .wrap{display:flex;flex-wrap:wrap;align-items:center;justify-content:space-between;gap:.5rem 1.5rem;min-height:4rem;padding-block:.5rem}
.brand{display:inline-flex;align-items:center;gap:.6rem;color:var(--link);text-decoration:none;font:800 1.6rem/1 var(--display);letter-spacing:.01em}
.brand svg{width:2rem;height:2rem;flex:none}
.site-head nav{display:flex;flex-wrap:wrap;gap:.25rem 1.25rem}
.site-head nav a{color:var(--fg);text-decoration:none;font-size:.875rem;padding-block:.25rem}
.site-head nav a:hover{color:var(--acc-hi)}.site-head nav a[aria-current]{color:var(--head);box-shadow:inset 0 -2px var(--acc)}
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
@media (prefers-color-scheme:dark){.btn-gh{background:#131314;border-color:#8e918f;color:#e3e3e3}.btn-gh svg{fill:#fff}}
@media (prefers-reduced-motion:reduce){*{transition:none!important}}
</style>
`

// Font URLs (embedded, see assets above) and fontsource's latin unicode-range.
var (
	publicSans    = assetURL("public-sans.woff2")
	bigShoulders  = assetURL("big-shoulders-display.woff2")
	jetbrainsMono = assetURL("jetbrains-mono.woff2")
)

const latinRange = "U+0000-00FF,U+0131,U+0152-0153,U+02BB-02BC,U+02C6,U+02DA,U+02DC,U+0304,U+0308,U+0329,U+2000-206F,U+20AC,U+2122,U+2191,U+2193,U+2212,U+2215,U+FEFF,U+FFFD"
