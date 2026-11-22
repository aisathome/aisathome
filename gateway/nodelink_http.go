// The approval page of a node's link (nodelink.go): /link on the site, where the node's link and code
// lead. Signing in is required: without a session it sends the browser to sign in and back. Only an active
// account looks a code up; the page shows what the node reported, what approving does, and the operator
// terms line of "Add a node", then Approve or Deny.
//
// SPDX-License-Identifier: Apache-2.0
package gateway

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// nextLink is the prefix of a sign-in's "next" that returns to /link with a code.
const nextLink = "link:"

// cleanNext reads a sign-in's "next": "chat", "link:<code>" (a well-formed code; nothing else can come
// back from it) or "link" (the page without a code), else "".
func cleanNext(v string) string {
	switch {
	case v == "chat" || v == "link":
		return v
	case strings.HasPrefix(v, nextLink):
		if c, ok := normUserCode(strings.TrimPrefix(v, nextLink)); ok {
			return nextLink + c
		}
		return "link"
	}
	return ""
}

// nextPath is where a finished sign-in with next lands, for a link: the page with the code.
func nextPath(next string) (string, bool) {
	if next == "link" {
		return "/link", true
	}
	if c, ok := strings.CutPrefix(next, nextLink); ok {
		return "/link?code=" + url.QueryEscape(c), true
	}
	return "", false
}

// linkPage is the approval page's data.
type linkPage struct {
	shell
	chatRulesPage
	Code     string    // the code as typed, normalised ("" = ask for one)
	Error    string    // why the code or the form was refused
	Link     *linkView // the pending link of Code
	Once     string    // ties the form to the account and this link
	Status   string    // the account's status
	Position int       // its place in the waitlist
	Blocked  string    // why no node can be added now (limits); Deny still works
	Done     string    // "approved" or "denied"
	NodeID   string    // approved: the new node's id
	Name     string    // approved: its name
}

// linkFormToken ties an approval form to the account and the link (its random id).
func (g *Gateway) linkFormToken(account, id string) string {
	m := hmac.New(sha256.New, []byte(g.acfg.SessionSecret))
	fmt.Fprintf(m, "link|%s|%s", account, id)
	return hex.EncodeToString(m.Sum(nil)[:12])
}

// linkData fills the page for account: its status, the chat rules and, for an active account, whether a
// node can be added now.
func (g *Gateway) linkData(account string) (linkPage, bool) {
	a, pos, ok := g.accounts.Get(account)
	if !ok || a.Status == StatusUnclaimed {
		return linkPage{}, false
	}
	d := linkPage{Status: a.Status, Position: pos}
	var pd pageData
	pd.Account = a
	g.chatRulesData(&pd)
	d.chatRulesPage = pd.chatRulesPage
	st := g.accounts.Settings()
	switch {
	case st.NodesClosed:
		d.Blocked = errNodesClosed.Error()
	case len(a.Nodes) >= st.MaxNodesPerAccountOrDefault():
		d.Blocked = fmt.Sprintf("You have %d of %d nodes. Revoke one on your account page, then reload this page.", len(a.Nodes), st.MaxNodesPerAccountOrDefault())
	case g.accounts.nodeTotal() >= st.MaxNodesOrDefault():
		d.Blocked = errNetworkFull.Error()
	}
	return d, true
}

// handleLinkPage serves GET /link[?code=…].
func (g *Gateway) handleLinkPage(w http.ResponseWriter, r *http.Request) {
	raw := r.URL.Query().Get("code")
	id, ok := g.session(r)
	if !ok {
		next := "link"
		if c, ok := normUserCode(raw); ok {
			next = nextLink + c
		}
		http.Redirect(w, r, g.acfg.PublicURL+"/account?next="+url.QueryEscape(next), http.StatusSeeOther)
		return
	}
	d, ok := g.linkData(id)
	if !ok {
		http.Redirect(w, r, g.acfg.PublicURL+"/account", http.StatusSeeOther)
		return
	}
	d.Code, _ = normUserCode(raw)
	if d.Status == StatusActive && raw != "" {
		v, err := g.links.lookup(id, g.clientIP(r), raw)
		if err != nil {
			d.Error = err.Error()
		} else {
			d.Link, d.Once = &v, g.linkFormToken(id, v.ID)
		}
	}
	g.linkPageOut(w, r, http.StatusOK, d)
}

// handleLinkForm serves POST /link: action=approve or deny, with the code, the link's id and the form's
// token; approve also takes the node's name and the operator terms version the form showed.
func (g *Gateway) handleLinkForm(w http.ResponseWriter, r *http.Request) {
	id, ok := g.session(r)
	if !ok || !g.sameOrigin(r) {
		http.Redirect(w, r, g.acfg.PublicURL+"/link", http.StatusSeeOther)
		return
	}
	d, ok := g.linkData(id)
	if !ok {
		http.Redirect(w, r, g.acfg.PublicURL+"/account", http.StatusSeeOther)
		return
	}
	code, linkID := r.FormValue("code"), r.FormValue("link")
	d.Code, _ = normUserCode(code)
	fail := func(status int, err error) {
		d.Error = err.Error()
		// The form again, for a link that is still pending (a limit stopped it, or a stale form); a code that
		// is unknown, used or locked out is not looked up again (that would count a second wrong code).
		if !errors.Is(err, errLinkUnknown) && !errors.Is(err, errLinkUsed) && !errors.Is(err, errLinkTries) {
			if v, lerr := g.links.lookup(id, g.clientIP(r), code); lerr == nil {
				d.Link, d.Once = &v, g.linkFormToken(id, v.ID)
			}
		}
		g.linkPageOut(w, r, status, d)
	}
	if d.Status != StatusActive {
		g.linkPageOut(w, r, http.StatusForbidden, d)
		return
	}
	if !hmac.Equal([]byte(r.FormValue("once")), []byte(g.linkFormToken(id, linkID))) {
		fail(http.StatusBadRequest, errLinkForm)
		return
	}
	switch r.FormValue("action") {
	case "deny":
		if err := g.links.deny(id, g.clientIP(r), code, linkID); err != nil {
			fail(http.StatusBadRequest, err)
			return
		}
		g.log.Info("node link denied", "operator", g.pseudonym(id))
		d.Done = "denied"
		g.linkPageOut(w, r, http.StatusOK, d)
	case "approve":
		g.approveLink(w, r, id, d, code, linkID, fail)
	default:
		fail(http.StatusBadRequest, errLinkForm)
	}
}

// approveLink creates the node of an approved link, as "Add a node" does, and hands its token to the link.
func (g *Gateway) approveLink(w http.ResponseWriter, r *http.Request, account string, d linkPage, code, linkID string, fail func(int, error)) {
	l, info, err := g.links.claim(account, g.clientIP(r), code, linkID)
	if err != nil {
		fail(http.StatusBadRequest, err)
		return
	}
	name := r.FormValue("name")
	if _, sent := r.Form["name"]; !sent {
		name = info.Name
	}
	// As on the account page: the acceptance counts only from a form that showed the line, for the
	// current version.
	terms := g.chatRulesOn() && r.FormValue("terms") == OperatorTermsVersion
	token, nodeID, recorded, err := g.accounts.addNodeVia(account, name, time.Now(), nil, terms, ViaNodeLink)
	if !g.links.finish(l, token, nodeID, err != nil) && err == nil {
		// The link expired while the node was being created: nobody will pick its token up.
		if rerr := g.accounts.RevokeNode(account, nodeID); rerr != nil {
			g.log.Warn("link expired during its approval; revoking the new node failed", "node", nodeID, "err", rerr)
		}
		fail(http.StatusBadRequest, errLinkUnknown)
		return
	}
	if err != nil {
		fail(http.StatusBadRequest, err)
		return
	}
	g.log.Info("node linked", "node", nodeID, "operator", g.pseudonym(account))
	if recorded {
		g.sweepNodes() // the account's other nodes may answer chats now
	}
	d2, _ := g.linkData(account)
	d2.Code, d2.Done, d2.NodeID, d2.Name = d.Code, "approved", nodeID, cleanNodeName(name)
	g.linkPageOut(w, r, http.StatusOK, d2)
}

func (g *Gateway) linkPageOut(w http.ResponseWriter, r *http.Request, status int, d linkPage) {
	back := "/link"
	if d.Code != "" && d.Done == "" {
		back += "?code=" + url.QueryEscape(d.Code)
	}
	d.shell = shell{Title: "Link a node", ChatURL: g.acfg.ChatURL, Theme: themeOf(r), Back: back}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Security-Policy", pageCSP+"; form-action 'self'; frame-ancestors 'none'")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.WriteHeader(status)
	if err := linkTmpl.Execute(w, d); err != nil {
		g.log.Error("link page", "err", err)
	}
}

// gib shows a card's memory in GB, as the site names cards ("24 GB").
func gib(mib int) string {
	if mib <= 0 {
		return ""
	}
	return fmt.Sprintf("%.0f GB", float64(mib)/1024)
}

var linkTmpl = newPage("link", template.FuncMap{
	"day": func(t time.Time) string { return t.Format("2006-01-02") },
	"gib": gib,
	"hm":  func(t time.Time) string { return t.UTC().Format("15:04 UTC") },
}, `{{template "head" .}}{{template "top" .}}
<h1>Link a node</h1>
{{if eq .Status "waitlisted"}}<div class="card" id="link-waitlist"><p>Your account is on the waitlist{{if .Position}} (place <strong>{{.Position}}</strong>){{end}}.
Nodes can be linked once it is let in; the code on the node works for 10 minutes, so start the link again on the node then.</p>
<p><a href="/account">Your account</a></p></div>
{{else if ne .Status "active"}}<div class="card" id="link-closed"><p>This account can no longer use AIs@home.</p></div>
{{else if eq .Done "approved"}}<div class="card ok" id="link-approved" role="status"><p><strong>Approved.</strong> {{if .Name}}{{.Name}} ({{end}}<code>{{.NodeID}}</code>{{if .Name}}){{end}} is now one of
your nodes. It picks up its node token within a few seconds and starts; it is listed under <a href="/account#nodes">Your nodes</a>, where you can revoke it.</p>
{{if .RulesOn}}{{if .TermsAccepted}}<p class="small" id="terms-created">Running this node means you accept the {{template "terms-link" .}} (accepted {{day .TermsSince}}). You can switch your nodes to science only on your account page.</p>
{{else if .TermsOptedOut}}<p class="small" id="terms-created">Your nodes do science only, as you chose; this one too.</p>{{end}}{{end}}</div>
{{else if eq .Done "denied"}}<div class="card" id="link-denied" role="status"><p><strong>Denied.</strong> The node gets no token and is not linked. If you did not start this
link yourself, nothing else happened.</p><p><a href="/account">Your account</a></p></div>
{{else}}{{if .Error}}<div class="card err" role="alert">{{.Error}}</div>{{end}}
{{with .Link}}<div class="card accent" id="link-request"><p><strong>A node asks to join your account</strong> with the code <code id="link-code">{{$.Code}}</code>.
Check that the node shows this same code.</p>
<dl class="kv">{{with .Info.Name}}<dt>Name</dt><dd data-link="name">{{.}}</dd>{{end}}
<dt>GPU</dt><dd data-link="gpu">{{if .Info.GPU}}{{.Info.GPU}}{{else}}not reported{{end}}{{with gib .Info.VRAMMiB}} ({{.}}){{end}}</dd>
<dt>System</dt><dd data-link="os">{{if .Info.OS}}{{.Info.OS}}{{else}}not reported{{end}}</dd>
<dt>Node software</dt><dd data-link="agent">{{if .Info.Agent}}{{.Info.Agent}}{{else}}not reported{{end}}</dd>
<dt>Asked at</dt><dd>{{hm .Created}}; the code works until {{hm .Expires}}</dd></dl>
<p class="hint">The node reported these details itself; nobody has checked them.</p></div>
<div class="card" id="link-what"><h2>What approving does</h2>
<ul><li>The node becomes one of your nodes, as if you had added it on your account page, and gets its own node token. It is listed under
<a href="/account#nodes">Your nodes</a>, where you can revoke it at any time.</li>
<li>What it serves counts for your account. Your own chats and API requests can go to it like to any of your nodes, also those with
personal data, which go only to your own nodes.</li>
<li>Like your other nodes, it answers other people's chats only under the operator terms; otherwise it does science only.</li>
<li><strong>Only approve a code that a machine you run shows you.</strong> If someone sent you this code or link, deny it: approving
would link their machine to your account.</li></ul></div>
{{if $.Blocked}}<div class="card" id="link-blocked"><p>{{$.Blocked}}</p>
<form method="post" action="/link"><input type="hidden" name="code" value="{{$.Code}}"><input type="hidden" name="link" value="{{.ID}}"><input type="hidden" name="once" value="{{$.Once}}">
<button class="btn danger" name="action" value="deny">Deny</button></form></div>
{{else}}<form class="form card" id="link-form" method="post" action="/link"><input type="hidden" name="code" value="{{$.Code}}"><input type="hidden" name="link" value="{{.ID}}"><input type="hidden" name="once" value="{{$.Once}}">
<div class="field"><label for="nodename">Name (optional, only you see it)</label>
<input id="nodename" name="name" maxlength="40" value="{{.Info.Name}}"></div>
{{template "terms-on-create" $}}
<div class="actions"><button class="btn" name="action" value="approve">Approve</button>
<button class="btn danger" name="action" value="deny">Deny</button></div></form>{{end}}
{{else}}<div class="card" id="link-ask"><p>Type the code the node shows: in the terminal after <code>aisathome-node link</code>, or on the node's
setup page after "Link to my account".</p>
<form class="form" method="get" action="/link"><div class="field"><label for="code">Code</label>
<div class="row"><input id="code" name="code" required maxlength="12" autocomplete="off" autocapitalize="characters" spellcheck="false" placeholder="KXQT-4821" value="{{.Code}}">
<button class="btn">Continue</button></div></div></form>
<p class="mut small">No code yet? See <a href="/node/">Run a node</a>.</p></div>{{end}}{{end}}
{{template "bottom" .}}`+accountChatRulesTmpl)
