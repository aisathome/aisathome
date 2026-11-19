// SPDX-License-Identifier: Apache-2.0
package gateway

import (
	"context"
	"crypto/md5"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/aisathome/aisathome/protocol"
)

const requestsPath = "/v1/connectors/boinc/requests"

// newLinkRig is a pilot rig whose boinc connector offers linking from the account page.
func newLinkRig(t *testing.T, mod func(*Config)) *pilotRig {
	return newPilotRig(t, func(c *Config) {
		c.Connectors[0].Accounts = &ConnectorAccounts{Name: "BOINC", AttachURL: "https://boinc.test/proj/",
			AttachCommand: "boinccmd --project_attach {url} {key}", KeyURL: "https://boinc.test/proj/weak_auth.php",
			AddressDomain: "nomail.test"}
		if mod != nil {
			mod(c)
		}
	})
}

// addAccount adds an account with id and status.
func (r *pilotRig) addAccount(id, status string) {
	r.t.Helper()
	if err := r.g.accounts.Update(func(f *AccountsFile) error {
		f.Accounts = append(f.Accounts, &Account{ID: id, Status: status, Created: r.clock(), Held: status == StatusWaitlisted})
		return nil
	}); err != nil {
		r.t.Fatal(err)
	}
}

// form posts an account-page form as account id's browser does.
func (r *pilotRig) form(id, path string, v url.Values) *httptest.ResponseRecorder {
	r.t.Helper()
	req := httptest.NewRequest("POST", path, strings.NewReader(v.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	req.AddCookie(&http.Cookie{Name: r.g.cookieName(sessionCookie), Value: signSession(r.g.acfg.SessionSecret, id, time.Now().Add(time.Hour))})
	w := httptest.NewRecorder()
	r.g.Handler().ServeHTTP(w, req)
	return w
}

// page is account id's account page.
func (r *pilotRig) page(id string) string {
	r.t.Helper()
	req := httptest.NewRequest("GET", "/account", nil)
	req.AddCookie(&http.Cookie{Name: r.g.cookieName(sessionCookie), Value: signSession(r.g.acfg.SessionSecret, id, time.Now().Add(time.Hour))})
	w := httptest.NewRecorder()
	r.g.Handler().ServeHTTP(w, req)
	if w.Code != 200 {
		r.t.Fatalf("account page: HTTP %d", w.Code)
	}
	return w.Body.String()
}

// pending returns the connector's pending requests.
func (r *pilotRig) pending() []map[string]any {
	r.t.Helper()
	out := r.want(200, "GET", requestsPath, testConnectorKey, nil)
	var l []map[string]any
	for _, x := range out["requests"].([]any) {
		l = append(l, x.(map[string]any))
	}
	return l
}

// onlyPending returns the one pending request.
func (r *pilotRig) onlyPending() map[string]any {
	r.t.Helper()
	l := r.pending()
	if len(l) != 1 {
		r.t.Fatalf("pending requests: %v", l)
	}
	return l[0]
}

// claim posts account id's claim form with the account key of connector user user.
func (r *pilotRig) claim(id, user string) {
	r.t.Helper()
	if w := r.form(id, "/account/connectors/boinc/claim", url.Values{"key": {acctKey(user)}}); w.Code != http.StatusSeeOther {
		r.t.Fatalf("claim form: HTTP %d: %s", w.Code, w.Body)
	}
}

// yes answers request rid as a connector that found the user its account key belongs to (a claim) does.
func (r *pilotRig) yes(rid string) map[string]any {
	r.t.Helper()
	q, err := r.g.sci.request("boinc", rid)
	if err != nil {
		r.t.Fatal(err)
	}
	user := keyUsers[q.AttachKey]
	return r.answer(rid, map[string]any{"ok": true, "user": user, "key": weakKey(user)})
}

func (r *pilotRig) answer(rid string, body map[string]any) map[string]any {
	r.t.Helper()
	return r.want(200, "POST", requestsPath+"/"+rid, testConnectorKey, body)["request"].(map[string]any)
}

func weakKey(user string) string { return user + "_" + strings.Repeat("ab12", 8) }

// keyUsers: the test users of the account keys acctKey made.
var keyUsers = map[string]string{}

// acctKey is a test user's full account key: 32 hex digits, as BOINC's authenticator.
func acctKey(user string) string {
	sum := md5.Sum([]byte("account|" + user))
	k := hex.EncodeToString(sum[:])
	keyUsers[k] = user
	return k
}

func (r *pilotRig) creditDay(account string) float64 {
	r.t.Helper()
	var c float64
	r.g.sci.db.QueryRow(`SELECT COALESCE(SUM(credit), 0) FROM pilot_credit_days WHERE account = ?`, account).Scan(&c)
	return c
}

// A claim moves the unclaimed account's user, pilots, daily credit and probation start into the
// signed-in account, and the unclaimed account goes; the pilot connects again as the new account's.
func TestClaimMergesUnclaimedAccount(t *testing.T) {
	r := newLinkRig(t, nil)
	pid, cred := r.create()
	r.bind(pid, "5")
	p, _ := r.g.sci.pilot("boinc", pid)
	cx := p.account
	if err := r.g.sci.meterPilot(pid, cx, pilotUse{ok: true, credit: 4}, 10, 15, r.clock()); err != nil {
		t.Fatal(err)
	}
	// The claiming account has nodes of its own since later, and pilot credit today already.
	r.advance(time.Hour)
	r.addAccount("gh:42", StatusActive)
	if err := r.g.accounts.Link("gh:42", "boinc", "42", r.clock()); err != nil {
		t.Fatal(err)
	}
	own, _ := r.create()
	r.bind(own, "42")
	if err := r.g.sci.meterPilot(own, "gh:42", pilotUse{ok: true, credit: 3}, 10, 15, r.clock()); err != nil {
		t.Fatal(err)
	}
	cxAcct, _, _ := r.g.accounts.Get(cx)
	first := *cxAcct.FirstNode

	r.claim("gh:42", "5")
	q := r.onlyPending()
	if q["kind"] != reqClaim || q["account_key"] != acctKey("5") || statusText(q) != statusText(map[string]any{
		"id": q["id"], "kind": "claim", "created_at": q["created_at"], "account_key": acctKey("5")}) {
		t.Fatalf("the connector's view of a claim: %v", q)
	}
	if strings.Contains(statusText(q), "gh:42") {
		t.Fatal("the connector sees which account asked")
	}
	if out := r.yes(q["id"].(string)); out["state"] != reqDone {
		t.Fatalf("after the answer: %v", out)
	}

	if _, _, ok := r.g.accounts.Get(cx); ok {
		t.Error("the unclaimed account is still there")
	}
	a, _, _ := r.g.accounts.Get("gh:42")
	var link *AccountLink
	for i := range a.Links {
		if a.Links[i].User == "5" {
			link = &a.Links[i]
		}
	}
	if link == nil || link.Via != LinkClaimed || link.AttachKey != weakKey("5") {
		t.Fatalf("links after the claim: %+v", a.Links)
	}
	if a.FirstNode == nil || !a.FirstNode.Equal(first) {
		t.Errorf("probation starts %v, want the unclaimed account's %v", a.FirstNode, first)
	}
	if p, _ := r.g.sci.pilot("boinc", pid); p.account != "gh:42" {
		t.Errorf("the pilot belongs to %q", p.account)
	}
	if c := r.creditDay("gh:42"); c != 7 {
		t.Errorf("daily pilot credit %v, want 3 + 4", c)
	}
	if c := r.creditDay(cx); c != 0 {
		t.Errorf("credit left with the unclaimed account: %v", c)
	}
	if pl, _, why := r.g.authPilot(cred); pl == nil || pl.account != "gh:42" || pl.tok.Operator != r.g.pseudonym("gh:42") {
		t.Fatalf("the pilot reconnecting: %+v %s", pl, why)
	}
	if v := r.want(200, "GET", pilotsPath+"/"+pid, testConnectorKey, nil)["pilot"].(map[string]any); v["account"] != "linked" {
		t.Errorf("connector's view: %v", v["account"])
	}
	page := r.page("gh:42")
	for _, want := range []string{"boinccmd --project_attach https://boinc.test/proj/ " + weakKey("5"), "(claimed)", pid, "Chat on",
		"log in to the BOINC site with your account key"} {
		if !strings.Contains(page, want) {
			t.Errorf("account page lacks %q", want)
		}
	}
	// The key left the request once it was in the accounts file.
	var key string
	r.g.sci.db.QueryRow(`SELECT attach_key FROM connector_requests WHERE id = ?`, q["id"]).Scan(&key)
	if key != "" {
		t.Error("the request keeps the key")
	}
	// The same answer again changes nothing; another answer is a conflict, also the same user with
	// another key.
	rid := q["id"].(string)
	r.answer(rid, map[string]any{"ok": true, "user": "5", "key": weakKey("5")})
	for _, body := range []map[string]any{{"ok": false, "error": "no"}, {"ok": true, "user": "5", "key": "5_" + strings.Repeat("cd34", 8)}} {
		if w, _ := r.call("POST", requestsPath+"/"+rid, testConnectorKey, body, ""); w.Code != 409 {
			t.Errorf("answer %v to a done request: HTTP %d", body, w.Code)
		}
	}
}

// A claim cannot take a user linked to another account (active or banned), even with a key the
// connector accepts; the link and its pilots stay where they were.
func TestClaimCannotStealLink(t *testing.T) {
	r := newLinkRig(t, nil)
	for _, owner := range []struct{ id, status, user string }{{"gh:42", StatusActive, "77"}, {"gh:43", StatusBanned, "78"}} {
		r.addAccount(owner.id, StatusActive)
		if err := r.g.accounts.Link(owner.id, "boinc", owner.user, r.clock()); err != nil {
			t.Fatal(err)
		}
		pid, _ := r.create()
		r.bind(pid, owner.user)
		r.g.accounts.SetStatus(owner.id, owner.status)
		thief := "gh:5" + owner.user
		r.addAccount(thief, StatusActive)
		r.claim(thief, owner.user)
		q := r.onlyPending()
		if out := r.yes(q["id"].(string)); out["state"] != reqFailed ||
			!strings.Contains(out["error"].(string), "linked to another") {
			t.Fatalf("claim of %s's user: %v", owner.status, out)
		}
		if id, _ := r.g.accounts.linkHolder("boinc", owner.user); id != owner.id {
			t.Errorf("%s's user now belongs to %q", owner.status, id)
		}
		if p, _ := r.g.sci.pilot("boinc", pid); p.account != owner.id {
			t.Errorf("%s's pilot moved to %q", owner.status, p.account)
		}
		if a, _, _ := r.g.accounts.Get(thief); len(a.Links) != 0 {
			t.Errorf("the claimer got links: %+v", a.Links)
		}
		if !strings.Contains(r.page(thief), "linked to another AIs@home account") {
			t.Error("the page does not say why")
		}
	}
}

// After a claim the account's switch decides for the user's pilots: chat off if either account had
// it off (on stays the default).
func TestClaimChatSetting(t *testing.T) {
	for i, tc := range []struct{ targetOff, unclaimedOff, want bool }{{false, false, false}, {true, false, true}, {false, true, true}} {
		r := newLinkRig(t, func(c *Config) { c.Connectors[0].PilotsChatEnabled = true })
		pid, cred := r.create()
		user := string(rune('5' + i))
		r.bind(pid, user)
		p, _ := r.g.sci.pilot("boinc", pid)
		if pl, _, _ := r.g.authPilot(cred); pl == nil || pl.scienceOnly {
			t.Fatalf("unclaimed account's pilot: chat on by default: %+v", pl)
		}
		r.g.accounts.SetPilotChat(p.account, !tc.unclaimedOff)
		r.addAccount("gh:42", StatusActive)
		r.g.accounts.SetPilotChat("gh:42", !tc.targetOff)
		r.claim("gh:42", user)
		r.yes(r.onlyPending()["id"].(string))
		a, _, _ := r.g.accounts.Get("gh:42")
		pl, _, _ := r.g.authPilot(cred)
		if a.PilotChatOff != tc.want || pl == nil || pl.scienceOnly != tc.want {
			t.Errorf("%+v: chat off %v, pilot science only %v", tc, a.PilotChatOff, pl != nil && pl.scienceOnly)
		}
	}
}

// A banned account may not claim: it cannot ask, and one banned while its claim waits gets no link;
// the unclaimed account keeps its user.
func TestBannedCannotClaim(t *testing.T) {
	r := newLinkRig(t, nil)
	pid, _ := r.create()
	r.bind(pid, "5")
	p, _ := r.g.sci.pilot("boinc", pid)
	r.addAccount("gh:9", StatusBanned)
	for _, path := range []string{"claim", "run"} {
		if w := r.form("gh:9", "/account/connectors/boinc/"+path, url.Values{"key": {acctKey("5")}}); w.Code != http.StatusBadRequest {
			t.Errorf("banned account's %s form: HTTP %d", path, w.Code)
		}
	}
	if w := r.form("gh:9", "/account/pilots/chat", url.Values{"chat": {"off"}}); w.Code != http.StatusBadRequest {
		t.Errorf("banned account's chat switch: HTTP %d", w.Code)
	}
	if l := r.pending(); len(l) != 0 {
		t.Fatalf("requests from a banned account: %v", l)
	}
	r.addAccount("gh:44", StatusActive)
	r.claim("gh:44", "5")
	q := r.onlyPending()
	r.g.accounts.SetStatus("gh:44", StatusBanned)
	if out := r.yes(q["id"].(string)); out["state"] != reqFailed {
		t.Fatalf("a banned account's claim: %v", out)
	}
	if id, _ := r.g.accounts.linkHolder("boinc", "5"); id != p.account {
		t.Errorf("the user belongs to %q, want the unclaimed %q", id, p.account)
	}
	if a, _, _ := r.g.accounts.Get("gh:44"); len(a.Links) != 0 {
		t.Errorf("the banned account got links: %+v", a.Links)
	}
}

// A waitlisted account may link (owner, 2026-11-19): claim, Run on BOINC, its pilots serve under it
// and follow its switch; the waitlist still keeps it from keys, the chat and the API.
func TestWaitlistedCanLink(t *testing.T) {
	r := newLinkRig(t, func(c *Config) { c.Connectors[0].PilotsChatEnabled = true })
	pid, cred := r.create()
	r.bind(pid, "5")
	r.addAccount("gh:7", StatusWaitlisted) // held: stays on the waitlist
	page := r.page("gh:7")
	for _, want := range []string{"You are on the waitlist", "Claim a BOINC account", "Run on BOINC</button>", "Science only on my pilots"} {
		if !strings.Contains(page, want) {
			t.Errorf("waitlist page lacks %q", want)
		}
	}
	r.claim("gh:7", "5")
	if out := r.yes(r.onlyPending()["id"].(string)); out["state"] != reqDone {
		t.Fatalf("waitlisted claim: %v", out)
	}
	if p, _ := r.g.sci.pilot("boinc", pid); p.account != "gh:7" {
		t.Fatalf("the pilot belongs to %q", p.account)
	}
	pl, _, why := r.g.authPilot(cred)
	if pl == nil || pl.account != "gh:7" || pl.tok.Trust == "trusted" {
		t.Fatalf("waitlisted account's pilot: %+v %s", pl, why)
	}
	if err := r.g.sci.meterPilot(pid, "gh:7", pilotUse{ok: true, credit: 2}, 10, 15, r.clock()); err != nil || r.creditDay("gh:7") != 2 {
		t.Errorf("credit of a waitlisted account's pilot: %v %v", r.creditDay("gh:7"), err)
	}
	if w := r.form("gh:7", "/account/pilots/chat", url.Values{"chat": {"off"}}); w.Code != http.StatusSeeOther {
		t.Fatalf("switch: HTTP %d", w.Code)
	}
	if pl, _, _ := r.g.authPilot(cred); pl == nil || !pl.scienceOnly {
		t.Error("the switch does not keep chat off the waitlisted account's pilot")
	}
	page = r.page("gh:7")
	if !strings.Contains(page, pid) || !strings.Contains(page, "(claimed)") {
		t.Error("the waitlist page does not show the link and the pilot")
	}
	// Still waitlisted: no key, no chat, no API.
	if a, _, _ := r.g.accounts.Get("gh:7"); a.Status != StatusWaitlisted {
		t.Fatalf("status %s", a.Status)
	}
	if _, err := r.g.accounts.AddKey("gh:7", 3, r.clock(), true, nil); err == nil {
		t.Error("a waitlisted account got an API key")
	}
	req := httptest.NewRequest("GET", "/account/auth", nil)
	req.Header.Set("Sec-Fetch-Mode", "navigate")
	req.AddCookie(&http.Cookie{Name: r.g.cookieName(sessionCookie), Value: signSession(r.g.acfg.SessionSecret, "gh:7", time.Now().Add(time.Hour))})
	w := httptest.NewRecorder()
	r.g.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusFound || w.Header().Get(authEmailHeader) != "" {
		t.Errorf("chat for a waitlisted account: HTTP %d", w.Code)
	}
	// Run on BOINC for another waitlisted account.
	r.addAccount("gh:8", StatusWaitlisted)
	if w := r.form("gh:8", "/account/connectors/boinc/run", nil); w.Code != http.StatusSeeOther {
		t.Fatalf("waitlisted run: HTTP %d", w.Code)
	}
	r.answer(r.onlyPending()["id"].(string), map[string]any{"ok": true, "user": "800", "key": weakKey("800")})
	if a, _, _ := r.g.accounts.Get("gh:8"); len(a.Links) != 1 {
		t.Fatalf("waitlisted run: links %+v", a.Links)
	}
	p2, _ := r.create()
	if v := r.bind(p2, "800")["pilot"].(map[string]any); v["account"] != "linked" {
		t.Errorf("a waitlisted account's user's pilot: %v", v["account"])
	}
}

// "Run on BOINC": the connector gets a pseudonymous address (derived, never stored), the display
// name and the password's hash, and links the user it made; the page shows the attach command and
// the login. An own address passes through the pending request only.
// No address is kept anywhere once the requests are done.
func TestRunOnConnector(t *testing.T) {
	r := newLinkRig(t, nil)
	r.addAccount("gh:42", StatusActive)
	if w := r.form("gh:42", "/account/connectors/boinc/run", url.Values{"name": {"Ada"}, "password": {"correct horse"}}); w.Code != http.StatusSeeOther {
		t.Fatalf("run form: HTTP %d: %s", w.Code, w.Body)
	}
	q := r.onlyPending()
	addr := r.g.pseudonymousAddress(r.g.connector("boinc"), "gh:42")
	sum := md5.Sum([]byte("correct horse" + addr))
	if q["kind"] != reqCreateLink || q["email"] != addr || q["user_name"] != "Ada" || q["passwd_hash"] != hex.EncodeToString(sum[:]) ||
		q["own_email"] != nil || !strings.HasSuffix(addr, "@nomail.test") || strings.Contains(addr, "42") {
		t.Fatalf("create_link as the connector sees it: %v (address %s)", q, addr)
	}
	if !strings.Contains(r.page("gh:42"), "is creating your BOINC account") {
		t.Error("the page does not say it is waiting")
	}
	// Bad answers.
	for _, body := range []map[string]any{{}, {"ok": true}, {"ok": true, "user": "900"}, {"ok": true, "user": "900", "key": weakKey("901")},
		{"ok": true, "user": "a@b", "key": "a@b_" + strings.Repeat("a", 32)}, {"ok": false}, {"ok": false, "error": " \n "}} {
		if w, _ := r.call("POST", requestsPath+"/"+q["id"].(string), testConnectorKey, body, ""); w.Code != 400 {
			t.Errorf("answer %v: HTTP %d", body, w.Code)
		}
	}
	r.answer(q["id"].(string), map[string]any{"ok": true, "user": "900", "key": weakKey("900")})
	a, _, _ := r.g.accounts.Get("gh:42")
	if len(a.Links) != 1 || a.Links[0].User != "900" || a.Links[0].Via != LinkCreated || a.Links[0].OwnEmail || a.Links[0].AttachKey != weakKey("900") {
		t.Fatalf("link: %+v", a.Links)
	}
	page := r.page("gh:42")
	for _, want := range []string{"boinccmd --project_attach https://boinc.test/proj/ " + weakKey("900"), addr} {
		if !strings.Contains(page, want) {
			t.Errorf("page lacks %q", want)
		}
	}
	if strings.Contains(page, "Run on BOINC</button>") {
		t.Error("a linked account is offered Run on BOINC again")
	}
	if w := r.form("gh:42", "/account/connectors/boinc/run", nil); w.Code != 400 {
		t.Errorf("run again: HTTP %d", w.Code)
	}

	if page := r.page("gh:42"); !strings.Contains(page, "change it in your BOINC account settings") || strings.Contains(page, "data-account-key-login") {
		t.Error("the page does not say how to use one's own address")
	}
	if w := r.form("gh:42", "/account/connectors/boinc/email", url.Values{"user": {"900"}, "email": {"ada@example.org"}}); w.Code != http.StatusNotFound &&
		w.Code != http.StatusMethodNotAllowed {
		t.Errorf("the set_email form is still there: HTTP %d", w.Code)
	}

	// Another account with its own address from the start; the connector says no once, then yes.
	r.addAccount("gh:43", StatusActive)
	for _, v := range []url.Values{{"address": {"own"}, "email": {"not an address"}}, {"address": {"own"}, "email": {"Ada <ada@example.org>"}},
		{"name": {"me@example.org"}}, {"password": {"short"}}} {
		if w := r.form("gh:43", "/account/connectors/boinc/run", v); w.Code != 400 {
			t.Errorf("run form %v: HTTP %d", v, w.Code)
		}
	}
	r.form("gh:43", "/account/connectors/boinc/run", url.Values{"address": {"own"}, "email": {"bob@example.org"}})
	q = r.onlyPending()
	if q["email"] != "bob@example.org" || q["own_email"] != true || !strings.HasPrefix(q["user_name"].(string), "aisathome-") || q["passwd_hash"] != nil {
		t.Fatalf("own address: %v", q)
	}
	r.answer(q["id"].(string), map[string]any{"ok": false, "error": "email\taddress already in use\n"})
	r.advance(time.Second)
	if page := r.page("gh:43"); !strings.Contains(page, "email address already in use") {
		t.Error("the page does not show the connector's reason")
	}
	r.form("gh:43", "/account/connectors/boinc/run", url.Values{"address": {"own"}, "email": {"bob@example.org"}})
	r.answer(r.onlyPending()["id"].(string), map[string]any{"ok": true, "user": "901", "key": weakKey("901")})
	if a, _, _ := r.g.accounts.Get("gh:43"); len(a.Links) != 1 || !a.Links[0].OwnEmail {
		t.Fatalf("own-address link: %+v", a.Links)
	}
	if page := r.page("gh:43"); strings.Contains(page, "@nomail.test") || strings.Contains(page, "data-own-address") {
		t.Error("an own-address user is shown a pseudonymous login")
	}

	// Nothing kept: no address, password hash or key in any request, no address in the accounts file.
	var n int
	r.g.sci.db.QueryRow(`SELECT COUNT(*) FROM connector_requests WHERE email != '' OR passwd_hash != '' OR attach_key != ''`).Scan(&n)
	if n != 0 {
		t.Errorf("%d requests keep an address, password hash or key", n)
	}
	if raw := statusText(r.g.accounts.Snapshot()); strings.Contains(raw, "@") {
		t.Error("an address in the accounts file")
	}
}

// A create_link answer naming a user held by an unclaimed account (a bind that came first) merges it,
// as a claim does.
func TestCreateLinkMergesPlaceholder(t *testing.T) {
	r := newLinkRig(t, nil)
	r.addAccount("gh:42", StatusActive)
	r.form("gh:42", "/account/connectors/boinc/run", nil)
	q := r.onlyPending()
	pid, _ := r.create()
	r.bind(pid, "900") // the user ran a pilot before the answer came
	r.answer(q["id"].(string), map[string]any{"ok": true, "user": "900", "key": weakKey("900")})
	if p, _ := r.g.sci.pilot("boinc", pid); p.account != "gh:42" {
		t.Errorf("the pilot belongs to %q", p.account)
	}
	for _, x := range r.g.accounts.Snapshot().Accounts {
		if x.Status == StatusUnclaimed {
			t.Errorf("unclaimed account left: %s", x.ID)
		}
	}
}

// The request API: only its connector's key, only its own requests; one open request per account
// and connector, a daily limit; unanswered requests fail after a day (their address gone) and old
// ones are deleted; the connector never sees finished requests.
func TestConnectorRequestRules(t *testing.T) {
	r := newLinkRig(t, func(c *Config) {
		c.Connectors[1].Accounts = &ConnectorAccounts{Name: "Other", AttachURL: "https://o.test/", AddressDomain: "o.test"}
	})
	r.addAccount("gh:42", StatusActive)
	r.form("gh:42", "/account/connectors/boinc/run", url.Values{"address": {"own"}, "email": {"ada@example.org"}})
	q := r.onlyPending()
	for _, key := range []string{"", otherConnectorKey, testProjectKey} {
		if w, _ := r.call("GET", requestsPath, key, nil, ""); w.Code != 401 {
			t.Errorf("list with key %q: HTTP %d", key, w.Code)
		}
	}
	if w, _ := r.call("POST", "/v1/connectors/other/requests/"+q["id"].(string), otherConnectorKey, map[string]any{"ok": false, "error": "x"},
		"203.0.113.9:1"); w.Code != 404 {
		t.Errorf("another connector answering: HTTP %d", w.Code)
	}
	if w, _ := r.call("POST", requestsPath+"/r-0000000000000000", testConnectorKey, map[string]any{"ok": false, "error": "x"}, ""); w.Code != 404 {
		t.Errorf("unknown request: HTTP %d", w.Code)
	}
	// One open request per connector; a claim meanwhile is refused.
	if w := r.form("gh:42", "/account/connectors/boinc/claim", url.Values{"key": {acctKey("5")}}); w.Code != 400 ||
		!strings.Contains(w.Body.String(), "already waiting") {
		t.Errorf("second request: HTTP %d", w.Code)
	}
	r.addAccount("gh:43", StatusActive)
	for _, bad := range []string{"5", "5_short", weakKey("5"), strings.Repeat("a", 31), strings.Repeat("g", 32), acctKey("5") + "!"} {
		if w := r.form("gh:43", "/account/connectors/boinc/claim", url.Values{"key": {bad}}); w.Code == http.StatusSeeOther {
			t.Errorf("claim key %q accepted", bad)
		}
	}
	if w := r.form("gh:42", "/account/connectors/nope/claim", url.Values{"key": {acctKey("5")}}); w.Code != 404 {
		t.Errorf("unknown connector: HTTP %d", w.Code)
	}
	// Unanswered for a day: failed, the address gone, no longer listed.
	r.advance(connectorRequestTTL + time.Minute)
	r.g.sweepRequests()
	if l := r.pending(); len(l) != 0 {
		t.Fatalf("expired request still pending: %v", l)
	}
	got, _ := r.g.sci.request("boinc", q["id"].(string))
	if got.State != reqFailed || got.Email != "" || !strings.Contains(r.page("gh:42"), "no answer in time") {
		t.Errorf("expired: %+v", got)
	}
	// Daily limit.
	for i := 0; i <= connectorRequestsPerDay; i++ {
		w := r.form("gh:42", "/account/connectors/boinc/claim", url.Values{"key": {acctKey("5")}})
		if i < connectorRequestsPerDay && w.Code != http.StatusSeeOther {
			t.Fatalf("claim %d: HTTP %d", i, w.Code)
		}
		if i == connectorRequestsPerDay {
			if w.Code != 400 || !strings.Contains(w.Body.String(), "too many requests") {
				t.Errorf("over the daily limit: HTTP %d", w.Code)
			}
			break
		}
		r.answer(r.onlyPending()["id"].(string), map[string]any{"ok": false, "error": "wrong key"})
	}
	// Finished requests are deleted after connectorRequestKeep.
	r.advance(connectorRequestKeep + time.Minute)
	r.g.sweepRequests()
	var n int
	r.g.sci.db.QueryRow(`SELECT COUNT(*) FROM connector_requests`).Scan(&n)
	if n != 0 {
		t.Errorf("%d old requests kept", n)
	}
}

// A link interrupted after the accounts file (a crash before the pilot records moved) is finished by
// the sweep: the records follow the merged account.
func TestConnectorRequestRecovery(t *testing.T) {
	r := newLinkRig(t, nil)
	pid, _ := r.create()
	r.bind(pid, "5")
	p, _ := r.g.sci.pilot("boinc", pid)
	r.addAccount("gh:42", StatusActive)
	r.claim("gh:42", "5")
	q := r.onlyPending()
	// What handleRequestResult does, up to the crash.
	if ok, err := r.g.sci.verifyRequest(q["id"].(string), "5", weakKey("5"), p.account, r.clock()); !ok || err != nil {
		t.Fatal(ok, err)
	}
	if _, err := r.g.accounts.claimLink("gh:42", "boinc", "5", AccountLink{AttachKey: weakKey("5"), Via: LinkClaimed}, r.clock(), nil); err != nil {
		t.Fatal(err)
	}
	if p, _ := r.g.sci.pilot("boinc", pid); p.account == "gh:42" {
		t.Fatal("moved too early")
	}
	r.g.sweepRequests()
	if p, _ := r.g.sci.pilot("boinc", pid); p.account != "gh:42" {
		t.Errorf("after the sweep the pilot belongs to %q", p.account)
	}
	if got, _ := r.g.sci.request("boinc", q["id"].(string)); got.State != reqDone || got.From != p.account {
		t.Errorf("request after the sweep: %+v", got)
	}
}

// The account page's switch: "science only on my pilots" and back, explained as hardened, not
// confidential; connected pilots follow at once.
func TestPilotChatForm(t *testing.T) {
	r := newLinkRig(t, func(c *Config) { c.Connectors[0].PilotsChatEnabled = true })
	r.addAccount("gh:42", StatusActive)
	r.g.accounts.Link("gh:42", "boinc", "42", r.clock())
	page := r.page("gh:42")
	for _, want := range []string{"hardened, not confidential", "Science only on my pilots", "Chat on"} {
		if !strings.Contains(page, want) {
			t.Errorf("page lacks %q", want)
		}
	}
	if strings.Contains(page, "data-pilots-science-only") {
		t.Error("says science only while the connector allows chat")
	}
	if w := r.form("gh:42", "/account/pilots/chat", url.Values{"chat": {"off"}}); w.Code != http.StatusSeeOther {
		t.Fatalf("switch: HTTP %d", w.Code)
	}
	if a, _, _ := r.g.accounts.Get("gh:42"); !a.PilotChatOff {
		t.Fatal("still on")
	}
	if page := r.page("gh:42"); !strings.Contains(page, "Allow chat on my pilots") || !strings.Contains(page, "Science only</span>") {
		t.Error("the page does not show it off")
	}
	r.form("gh:42", "/account/pilots/chat", url.Values{"chat": {"on"}})
	if a, _, _ := r.g.accounts.Get("gh:42"); a.PilotChatOff {
		t.Fatal("still off")
	}
	if w := r.form("gh:42", "/account/pilots/chat", url.Values{"chat": {"maybe"}}); w.Code != 400 {
		t.Errorf("bad value: HTTP %d", w.Code)
	}
	// While the connector keeps pilots on science, the page says so.
	r2 := newLinkRig(t, nil)
	r2.addAccount("gh:42", StatusActive)
	if !strings.Contains(r2.page("gh:42"), "data-pilots-science-only") {
		t.Error("no note that pilots serve science only for now")
	}
}

// An unclaimed account cannot sign in, hold API keys or node tokens, or use the chat.
func TestUnclaimedCannotSignInOrHoldKeys(t *testing.T) {
	r := newLinkRig(t, nil)
	pid, _ := r.create()
	r.bind(pid, "5")
	p, _ := r.g.sci.pilot("boinc", pid)
	if _, err := r.g.accounts.AddKey(p.account, 3, r.clock(), true, nil); err == nil {
		t.Error("an unclaimed account got an API key")
	}
	if _, _, err := r.g.accounts.AddNode(p.account, "x", r.clock(), nil); err == nil {
		t.Error("an unclaimed account got a node token")
	}
	if _, ok := accountFromEmail(accountEmail(p.account)); ok {
		t.Error("the chat can name an unclaimed account")
	}
	req := httptest.NewRequest("GET", "/account/auth", nil)
	req.Header.Set("Sec-Fetch-Mode", "navigate")
	req.AddCookie(&http.Cookie{Name: r.g.cookieName(sessionCookie), Value: signSession(r.g.acfg.SessionSecret, p.account, time.Now().Add(time.Hour))})
	w := httptest.NewRecorder()
	r.g.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusFound || w.Header().Get(authEmailHeader) != "" {
		t.Errorf("chat for an unclaimed account: HTTP %d", w.Code)
	}
	if strings.Contains(r.page(p.account), "Your account</h1>") {
		t.Error("the account page opens for an unclaimed account")
	}
	if w := r.form(p.account, "/account/connectors/boinc/claim", url.Values{"key": {acctKey("6")}}); w.Code != 400 {
		t.Errorf("an unclaimed account's claim: HTTP %d", w.Code)
	}
}

// A science database from the gateway before connector requests (version 2) gets the table and keeps
// its pilots.
func TestScienceDBRequestsMigration(t *testing.T) {
	all := scienceMigrations
	defer func() { scienceMigrations = all }()
	dir := t.TempDir()
	scienceMigrations = all[:2]
	s, err := openScienceDB(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.createPilot("boinc", time.Now().Add(time.Hour), 1, 1, time.Now()); err != nil {
		t.Fatal(err)
	}
	s.close()
	scienceMigrations = all
	if s, err = openScienceDB(dir); err != nil {
		t.Fatal(err)
	}
	defer s.close()
	var n int
	if v, _ := s.schemaVersion(); v != len(all) {
		t.Fatalf("version %d", v)
	}
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM pilots`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("pilots after the migration: %d %v", n, err)
	}
	if _, err := s.request("", "r-0000000000000000"); err != errNoRequest {
		t.Fatalf("requests table: %v", err)
	}
}

// A connector's accounts settings are checked at load.
func TestConnectorAccountsConfigChecked(t *testing.T) {
	good := ConnectorAccounts{Name: "BOINC", AttachURL: "https://b.test/p/", AddressDomain: "nomail.b.test"}
	for _, mod := range []func(*ConnectorAccounts){
		func(a *ConnectorAccounts) { a.Name = "" },
		func(a *ConnectorAccounts) { a.Name = " BOINC" },
		func(a *ConnectorAccounts) { a.AttachURL = "http://b.test/p/" },
		func(a *ConnectorAccounts) { a.AttachURL = "https://user@b.test/p/" },
		func(a *ConnectorAccounts) { a.KeyURL = "javascript:alert(1)" },
		func(a *ConnectorAccounts) { a.AttachCommand = "boinccmd\n--project_attach" },
		func(a *ConnectorAccounts) { a.AddressDomain = "" },
		func(a *ConnectorAccounts) { a.AddressDomain = "Nomail.Test" },
		func(a *ConnectorAccounts) { a.AddressDomain = "x@nomail.test" },
	} {
		a := good
		mod(&a)
		cs := testConnectors()
		cs[0].Accounts = &a
		if _, err := newConnectorTable(cs, nil); err == nil {
			t.Errorf("accepted %+v", a)
		}
	}
	cs := testConnectors()
	cs[0].Accounts = &good
	if _, err := newConnectorTable(cs, nil); err != nil {
		t.Fatal(err)
	}
}

// Review fixes: a connector's no cannot undo a yes already taken; a link finished before a crash is
// completed by the sweep even if the account was banned since; an answer metered after a claim
// counts for the record's (new) account.
func TestConnectorRequestAfterTheYes(t *testing.T) {
	r := newLinkRig(t, nil)
	pid, _ := r.create()
	r.bind(pid, "5")
	p, _ := r.g.sci.pilot("boinc", pid)
	cx := p.account
	r.addAccount("gh:42", StatusActive)
	r.claim("gh:42", "5")
	q := r.onlyPending()
	rid := q["id"].(string)
	if ok, err := r.g.sci.verifyRequest(rid, "5", weakKey("5"), cx, r.clock()); !ok || err != nil {
		t.Fatal(ok, err)
	}
	if _, err := r.g.accounts.claimLink("gh:42", "boinc", "5", AccountLink{AttachKey: weakKey("5"), Via: LinkClaimed}, r.clock(), nil); err != nil {
		t.Fatal(err)
	}
	if w, _ := r.call("POST", requestsPath+"/"+rid, testConnectorKey, map[string]any{"ok": false, "error": "changed my mind"}, ""); w.Code != 409 {
		t.Errorf("a no after the yes: HTTP %d", w.Code)
	}
	r.g.accounts.SetStatus("gh:42", StatusBanned)
	r.g.sweepRequests()
	if got, _ := r.g.sci.request("boinc", rid); got.State != reqDone {
		t.Errorf("after the sweep: %s %s", got.State, got.Error)
	}
	if p, _ := r.g.sci.pilot("boinc", pid); p.account != "gh:42" {
		t.Errorf("the pilot stayed with %q", p.account)
	}
	// The connection still names the unclaimed account; its next answer counts for the record's.
	if err := r.g.sci.meterPilot(pid, cx, pilotUse{ok: true, credit: 2}, 10, 15, r.clock()); err != nil {
		t.Fatal(err)
	}
	if c := r.creditDay(cx); c != 0 {
		t.Errorf("credit for the merged account: %v", c)
	}
	if c := r.creditDay("gh:42"); c != 2 {
		t.Errorf("credit for the account: %v", c)
	}
}

// The daily copy of the science database keeps no open request's address, password hash or key.
func TestScienceCopyScrubsRequests(t *testing.T) {
	dir := t.TempDir()
	s, err := openScienceDB(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()
	q := &connRequest{Connector: "boinc", Kind: reqCreateLink, Account: "gh:1", Email: "ada@example.org", PasswdHash: strings.Repeat("a", 32)}
	if err := s.addRequest(q, time.Now()); err != nil {
		t.Fatal(err)
	}
	day := time.Date(2026, 11, 19, 3, 0, 0, 0, time.UTC)
	if _, err := s.backup(day); err != nil {
		t.Fatal(err)
	}
	c, err := sql.Open("sqlite", filepath.Join(dir, backupName(day)))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	var email, hash, state string
	if err := c.QueryRow(`SELECT email, passwd_hash, state FROM connector_requests WHERE id = ?`, q.ID).Scan(&email, &hash, &state); err != nil {
		t.Fatal(err)
	}
	if email != "" || hash != "" {
		t.Errorf("the copy keeps %q %q", email, hash)
	}
	if state != reqFailed { // restored, it could not be carried out without them
		t.Errorf("the copy's open request is %s", state)
	}
	// A verified request keeps its answer's attach key and state: restored, it can still finish.
	v := &connRequest{Connector: "boinc", Kind: reqClaim, Account: "gh:2", AttachKey: acctKey("9")}
	s.addRequest(v, time.Now())
	s.verifyRequest(v.ID, "9", weakKey("9"), "", time.Now())
	day2 := day.AddDate(0, 0, 1)
	if _, err := s.backup(day2); err != nil {
		t.Fatal(err)
	}
	c2, _ := sql.Open("sqlite", filepath.Join(dir, backupName(day2)))
	defer c2.Close()
	var vstate, vkey string
	c2.QueryRow(`SELECT state, attach_key FROM connector_requests WHERE id = ?`, v.ID).Scan(&vstate, &vkey)
	if vstate != reqVerified || vkey != weakKey("9") {
		t.Errorf("the copy's verified request: %s %q", vstate, vkey)
	}
	if live, _ := s.request("", q.ID); live.Email != "ada@example.org" {
		t.Error("the live request lost its address")
	}
}

// A ban or a chat switch that comes while a pilot registers counts before it is routable.
func TestPilotAccountChangeDuringRegistration(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(r *pilotRig, account string)
		closed bool
	}{
		{"banned", func(r *pilotRig, a string) { r.g.accounts.SetStatus(a, StatusBanned) }, true},
		{"chat off", func(r *pilotRig, a string) { r.g.accounts.SetPilotChat(a, false) }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newPilotRig(t, func(c *Config) { c.Connectors[0].PilotsChatEnabled = true })
			srv := httptest.NewServer(r.g.Handler())
			defer srv.Close()
			id, cred := r.create()
			r.bind(id, "5")
			p, _ := r.g.sci.pilot("boinc", id)
			r.g.pilotRegistered = func(string) { tc.change(r, p.account) }
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			c, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http")+"/v1/node/connect",
				&websocket.DialOptions{HTTPHeader: http.Header{"Authorization": {"Bearer " + cred}}})
			if err != nil {
				t.Fatal(err)
			}
			defer c.CloseNow()
			hello, _ := json.Marshal(protocol.Msg{Type: protocol.TypeHello, NodeID: id, Protocol: protocol.LegacyVersion,
				Models: []protocol.ModelInfo{{Name: "m", Roles: []string{"deep"}, Slots: 1}}})
			c.Write(ctx, websocket.MessageText, hello)
			if tc.closed {
				var routable atomic.Bool // the pilot became routable (not the sweep's job to stop it)
				done := make(chan struct{})
				go func() {
					for {
						select {
						case <-done:
							return
						default:
						}
						r.g.mu.Lock()
						if x := r.g.node[id]; x != nil && !x.registering {
							routable.Store(true)
						}
						r.g.mu.Unlock()
						time.Sleep(time.Millisecond)
					}
				}()
				for {
					if _, _, err = c.Read(ctx); err != nil {
						break
					}
				}
				close(done)
				if websocket.CloseStatus(err) != websocket.StatusPolicyViolation {
					t.Fatalf("not closed: %v", err)
				}
				if routable.Load() {
					t.Fatal("the banned account's pilot became routable")
				}
				return
			}
			var n *nodeConn
			for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(10 * time.Millisecond) {
				r.g.mu.Lock()
				if x := r.g.node[id]; x != nil && !x.registering {
					n = x
				}
				so := n != nil && n.scienceOnly
				r.g.mu.Unlock()
				if n != nil {
					if !so {
						t.Fatal("the switch turned off during registration did not count")
					}
					return
				}
				if time.Now().After(deadline) {
					t.Fatal("never routable")
				}
			}
		})
	}
}

// A claim carries the unclaimed account's operator terms over: the newer acceptance, the later
// withdrawal (an acceptance older than a withdrawal goes).
func TestClaimCarriesOperatorTerms(t *testing.T) {
	t0 := time.Date(2026, 11, 18, 10, 0, 0, 0, time.UTC)
	acc := func(h int) *Acceptance {
		return &Acceptance{Version: OperatorTermsVersion, At: t0.Add(time.Duration(h) * time.Hour), Via: "x"}
	}
	at := func(h int) *time.Time { x := t0.Add(time.Duration(h) * time.Hour); return &x }
	for i, tc := range []struct {
		name              string
		target, unclaimed Account
		wantAt            int // hours after t0; -1: no acceptance
		wantWithdrawn     string
	}{
		{"only the unclaimed accepted", Account{}, Account{OperatorTerms: acc(1)}, 1, ""},
		{"the target's newer acceptance stays", Account{OperatorTerms: acc(2)}, Account{OperatorTerms: acc(1)}, 2, ""},
		{"the unclaimed's newer acceptance wins", Account{OperatorTerms: acc(1)}, Account{OperatorTerms: acc(3)}, 3, ""},
		{"a later withdrawal on the target removes it", Account{TermsWithdrawn: at(3)}, Account{OperatorTerms: acc(1)}, -1, "3"},
		{"an acceptance after the target's withdrawal stands", Account{TermsWithdrawn: at(3)}, Account{OperatorTerms: acc(4)}, 4, "3"},
		{"a withdrawal on the unclaimed carries over", Account{OperatorTerms: acc(1)}, Account{TermsWithdrawn: at(2)}, -1, "2"},
	} {
		r := newLinkRig(t, nil)
		user := string(rune('a' + i))
		pid, _ := r.create()
		r.bind(pid, user)
		p, _ := r.g.sci.pilot("boinc", pid)
		r.addAccount("gh:42", StatusActive)
		r.g.accounts.Update(func(f *AccountsFile) error {
			for _, x := range []struct {
				id  string
				src Account
			}{{"gh:42", tc.target}, {p.account, tc.unclaimed}} {
				a := f.find(x.id)
				a.OperatorTerms, a.TermsWithdrawn = x.src.OperatorTerms, x.src.TermsWithdrawn
			}
			return nil
		})
		r.claim("gh:42", user)
		r.yes(r.onlyPending()["id"].(string))
		a, _, _ := r.g.accounts.Get("gh:42")
		switch {
		case tc.wantAt < 0 && a.OperatorTerms != nil, tc.wantAt >= 0 && (a.OperatorTerms == nil || !a.OperatorTerms.At.Equal(t0.Add(time.Duration(tc.wantAt)*time.Hour))):
			t.Errorf("%s: acceptance %+v", tc.name, a.OperatorTerms)
		}
		if got := ""; a.TermsWithdrawn != nil {
			got = a.TermsWithdrawn.Sub(t0).String()
			if got != tc.wantWithdrawn+"h0m0s" {
				t.Errorf("%s: withdrawn %s", tc.name, got)
			}
		} else if tc.wantWithdrawn != "" {
			t.Errorf("%s: withdrawal lost", tc.name)
		}
	}
}

// Review fixes (2026-11-19): the weak key no longer claims (BOINC means it to be shared), the account
// key does; claiming a user linked already refreshes its attach key; a connector's error text keeps
// nothing the person sent; the WAL is truncated after a request is blanked.
func TestClaimNeedsTheAccountKey(t *testing.T) {
	r := newLinkRig(t, nil)
	pid, _ := r.create()
	r.bind(pid, "5")
	r.addAccount("gh:42", StatusActive)
	w := r.form("gh:42", "/account/connectors/boinc/claim", url.Values{"key": {weakKey("5")}})
	if w.Code != 400 || !strings.Contains(w.Body.String(), "weak account key") {
		t.Fatalf("a weak key: HTTP %d", w.Code)
	}
	if l := r.pending(); len(l) != 0 {
		t.Fatalf("a weak key made a request: %v", l)
	}
	r.claim("gh:42", "5")
	q := r.onlyPending()
	if q["account_key"] != acctKey("5") || q["user"] != nil || q["key"] != nil {
		t.Fatalf("claim as the connector sees it: %v", q)
	}
	r.yes(q["id"].(string))
	if p, _ := r.g.sci.pilot("boinc", pid); p.account != "gh:42" {
		t.Fatalf("the pilot belongs to %q", p.account)
	}
	// After a password change at BOINC the weak key changes: claiming again refreshes it.
	r.advance(time.Second)
	r.claim("gh:42", "5")
	newKey := "5_" + strings.Repeat("cd34", 8)
	r.answer(r.onlyPending()["id"].(string), map[string]any{"ok": true, "user": "5", "key": newKey})
	if a, _, _ := r.g.accounts.Get("gh:42"); len(a.Links) != 1 || a.Links[0].AttachKey != newKey {
		t.Errorf("links after a second claim: %+v", a.Links)
	}
	// The connector's error text keeps nothing the person sent.
	r.advance(time.Second)
	r.addAccount("gh:43", StatusActive)
	r.form("gh:43", "/account/connectors/boinc/run", url.Values{"address": {"own"}, "email": {"bob@example.org"}, "password": {"a long password"}})
	q = r.onlyPending()
	msg := "address bob@example.org in use (hash " + q["passwd_hash"].(string) + ", other x@y.z, key " + strings.Repeat("ab", 16) + ")"
	r.answer(q["id"].(string), map[string]any{"ok": false, "error": msg})
	got, _ := r.g.sci.request("boinc", q["id"].(string))
	for _, leak := range []string{"bob@example.org", q["passwd_hash"].(string), "x@y.z", strings.Repeat("ab", 16)} {
		if strings.Contains(got.Error, leak) {
			t.Errorf("the stored error keeps %q: %s", leak, got.Error)
		}
	}
	if !strings.Contains(got.Error, "in use") {
		t.Errorf("the reason went too: %s", got.Error)
	}
	// The WAL holds no older copy of a blanked request.
	if fi, err := os.Stat(r.g.sci.path + "-wal"); err == nil && fi.Size() != 0 {
		t.Errorf("the WAL has %d bytes after a request was blanked", fi.Size())
	}
}

// Review fix (batch review 2026-11-19, high): a connector's acceptance is stored with the time it
// sent; a withdrawal comes after it even if that time is ahead of ours, so retrying the same record
// never undoes the withdrawal; more than a minute ahead is refused; an optional country is ignored.
func TestConnectorTermsRetryAfterWithdrawal(t *testing.T) {
	r := newLinkRig(t, nil)
	path := "/v1/connectors/boinc/users/77/operator-terms"
	now := r.clock()
	if w, _ := r.call("POST", path, testConnectorKey, map[string]any{"version": OperatorTermsVersion, "accepted_at": now.Add(2 * time.Minute)}, ""); w.Code != 400 {
		t.Errorf("two minutes ahead: HTTP %d", w.Code)
	}
	ahead := now.Add(30 * time.Second)
	body := map[string]any{"version": OperatorTermsVersion, "accepted_at": ahead, "country": "NO"}
	if out := r.want(200, "POST", path, testConnectorKey, body); out["recorded"] != true {
		t.Fatalf("acceptance: %v", out)
	}
	id, _ := r.g.accounts.linkHolder("boinc", "77")
	if a, _, _ := r.g.accounts.Get(id); a.OperatorTerms == nil || !a.OperatorTerms.At.Equal(ahead) {
		t.Fatalf("stored acceptance %+v, sent %v", a.OperatorTerms, ahead)
	}
	if err := r.g.accounts.WithdrawOperatorTerms(id); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		r.advance(10 * time.Second) // retries come later, still before the sent time
		delete(body, "country")
		if out := r.want(200, "POST", path, testConnectorKey, body); out["recorded"] != false || out["current"] != false {
			t.Fatalf("retry %d after the withdrawal: %v", i, out)
		}
	}
	if a, _, _ := r.g.accounts.Get(id); a.OperatorTerms != nil {
		t.Errorf("the retry undid the withdrawal: %+v", a.OperatorTerms)
	}
	// On the gateway's own clock (the withdrawal's): accepted 30 s ahead, withdrawn now, retried.
	real := time.Now()
	soon := real.Add(30 * time.Second)
	if _, applied, _, err := r.g.accounts.connectorTerms("boinc", "78", OperatorTermsVersion, soon, real); !applied || err != nil {
		t.Fatal(applied, err)
	}
	id78, _ := r.g.accounts.linkHolder("boinc", "78")
	r.g.accounts.WithdrawOperatorTerms(id78)
	if _, applied, terms, _ := r.g.accounts.connectorTerms("boinc", "78", OperatorTermsVersion, soon, real.Add(time.Second)); applied || terms {
		t.Error("a retry of an acceptance from ahead of our clock undid the withdrawal")
	}
}

// Second gate review (2026-11-19): withdrawals only move forward; a busy WAL checkpoint is retried;
// claims made with a weak key before the upgrade fail cleanly; a verified request's retry must repeat
// its answer; the account a merge deletes is saved first; a copy keeps verified requests; a reclaim
// stops showing the made-up login.
func TestConnectorReview2(t *testing.T) {
	r := newLinkRig(t, nil)
	// Withdrawals only move forward.
	real := time.Now()
	soon := real.Add(30 * time.Second)
	r.g.accounts.connectorTerms("boinc", "70", OperatorTermsVersion, soon, real)
	id70, _ := r.g.accounts.linkHolder("boinc", "70")
	r.g.accounts.WithdrawOperatorTerms(id70)
	r.g.accounts.WithdrawOperatorTerms(id70) // again, before soon
	if a, _, _ := r.g.accounts.Get(id70); a.TermsWithdrawn == nil || a.TermsWithdrawn.Before(soon) {
		t.Fatalf("the second withdrawal moved it back: %v", a.TermsWithdrawn)
	}
	if _, applied, _, _ := r.g.accounts.connectorTerms("boinc", "70", OperatorTermsVersion, soon, real.Add(time.Second)); applied {
		t.Error("a replayed acceptance undid two withdrawals")
	}

	// A claim from before the upgrade (a weak key and a user id) is failed, never listed as an account key.
	r.addAccount("gh:41", StatusActive)
	old := &connRequest{Connector: "boinc", Kind: reqClaim, Account: "gh:41", User: "5", AttachKey: weakKey("5")}
	if err := r.g.sci.addRequest(old, r.clock()); err != nil {
		t.Fatal(err)
	}
	if l := r.pending(); len(l) != 0 {
		t.Fatalf("an old claim listed: %v", l)
	}
	if got, _ := r.g.sci.request("boinc", old.ID); got.State != reqFailed || got.AttachKey != "" || !strings.Contains(got.Error, "full account key") {
		t.Fatalf("old claim: %+v", got)
	}

	// A verified request's retry must repeat its answer.
	pid, _ := r.create()
	r.bind(pid, "5")
	r.addAccount("gh:42", StatusActive)
	r.advance(time.Second)
	r.claim("gh:42", "5")
	rid := r.onlyPending()["id"].(string)
	if ok, _ := r.g.sci.verifyRequest(rid, "5", weakKey("5"), "", r.clock()); !ok {
		t.Fatal("not verified")
	}
	if w, _ := r.call("POST", requestsPath+"/"+rid, testConnectorKey, map[string]any{"ok": true, "user": "6", "key": weakKey("6")}, ""); w.Code != 409 {
		t.Errorf("another answer to a verified request: HTTP %d", w.Code)
	}

	// The unclaimed account the merge will delete is saved before the accounts file changes: with the
	// accounts file unwritable the request stays verified and names it.
	p, _ := r.g.sci.pilot("boinc", pid)
	if os.Geteuid() != 0 { // root writes through a 0500 directory
		dir := filepath.Dir(r.g.acfg.File)
		os.Chmod(dir, 0o500)
		q, _ := r.g.sci.request("boinc", rid)
		r.g.linkMu.Lock()
		r.g.finishRequest(q)
		r.g.linkMu.Unlock()
		os.Chmod(dir, 0o700)
		if got, _ := r.g.sci.request("boinc", rid); got.State != reqVerified || got.From != p.account {
			t.Fatalf("after a failed merge: %s from %q, want verified from %q", got.State, got.From, p.account)
		}
	}
	r.g.sweepRequests()
	if p, _ := r.g.sci.pilot("boinc", pid); p.account != "gh:42" {
		t.Fatalf("the pilot belongs to %q after the sweep", p.account)
	}

	// A reclaim of a user made by Run on BOINC stops showing the made-up login.
	r.addAccount("gh:43", StatusActive)
	r.form("gh:43", "/account/connectors/boinc/run", nil)
	r.answer(r.onlyPending()["id"].(string), map[string]any{"ok": true, "user": "430", "key": weakKey("430")})
	if !strings.Contains(r.page("gh:43"), "data-login") {
		t.Fatal("no login shown for a pseudonymous user")
	}
	r.advance(time.Second)
	r.claim("gh:43", "430")
	r.yes(r.onlyPending()["id"].(string))
	if page := r.page("gh:43"); strings.Contains(page, "data-login") || !strings.Contains(page, "data-account-key-login") {
		t.Error("after a reclaim the page still shows the made-up login")
	}
}

// A WAL truncation a reader blocks is retried by the sweep.
func TestRequestWALRetry(t *testing.T) {
	r := newLinkRig(t, nil)
	r.addAccount("gh:42", StatusActive)
	r.form("gh:42", "/account/connectors/boinc/run", url.Values{"address": {"own"}, "email": {"ada@example.org"}})
	rid := r.onlyPending()["id"].(string)
	reader, err := sql.Open("sqlite", r.g.sci.path)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	tx, err := reader.Begin()
	if err != nil {
		t.Fatal(err)
	}
	var n int
	tx.QueryRow(`SELECT COUNT(*) FROM connector_requests`).Scan(&n) // holds a read snapshot of the WAL
	r.answer(rid, map[string]any{"ok": false, "error": "no"})
	if !r.g.sci.walDirty.Load() {
		t.Fatal("a busy truncation was not noticed")
	}
	tx.Rollback()
	r.g.sweepRequests()
	if r.g.sci.walDirty.Load() {
		t.Error("the sweep did not truncate the WAL")
	}
	if fi, err := os.Stat(r.g.sci.path + "-wal"); err == nil && fi.Size() != 0 {
		t.Errorf("WAL %d bytes after the retry", fi.Size())
	}
}

// Third gate review (2026-11-19): a reply already in flight cannot verify a claim made with a weak key
// before the account-key change; the account a merge deletes is the one in the accounts file at that
// moment (not the cached copy), saved through keep before it goes, and a failing keep stops the merge.
func TestConnectorReview3(t *testing.T) {
	r := newLinkRig(t, nil)
	r.addAccount("gh:41", StatusActive)
	old := &connRequest{Connector: "boinc", Kind: reqClaim, Account: "gh:41", User: "5", AttachKey: weakKey("5")}
	if err := r.g.sci.addRequest(old, r.clock()); err != nil {
		t.Fatal(err)
	}
	// The reply comes before any listing or sweep.
	if w, _ := r.call("POST", requestsPath+"/"+old.ID, testConnectorKey, map[string]any{"ok": true, "user": "5", "key": weakKey("5")}, ""); w.Code != 409 {
		t.Errorf("a reply to a weak-key claim: HTTP %d", w.Code)
	}
	if got, _ := r.g.sci.request("boinc", old.ID); got.State != reqFailed || !strings.Contains(got.Error, "full account key") {
		t.Fatalf("weak-key claim after the reply: %+v", got)
	}
	if a, _, _ := r.g.accounts.Get("gh:41"); len(a.Links) != 0 {
		t.Fatalf("a weak-key claim linked: %+v", a.Links)
	}

	// Another writer (the operator's CLI) gives user 9 an unclaimed account the gateway's cache has not
	// seen yet: keep gets that id from the file, and an error from keep leaves everything as it was.
	cli, err := OpenAccountStore(r.g.acfg.File)
	if err != nil {
		t.Fatal(err)
	}
	cli.Update(func(f *AccountsFile) error {
		f.Accounts = append(f.Accounts, &Account{ID: "cx:" + strings.Repeat("9", 32), Status: StatusUnclaimed, Created: r.clock(),
			Links: []AccountLink{{Connector: "boinc", User: "9", Linked: r.clock()}}})
		return nil
	})
	if id, _ := r.g.accounts.linkHolder("boinc", "9"); id != "" {
		t.Fatal("the cache already knows the CLI's change; the test needs a stale cache")
	}
	r.addAccount("gh:42", StatusActive)
	var kept string
	stop := errors.New("database down")
	if _, err := r.g.accounts.claimLink("gh:42", "boinc", "9", AccountLink{Via: LinkClaimed}, r.clock(), func(id string) error {
		kept = id
		return stop
	}); !errors.Is(err, stop) {
		t.Fatalf("claimLink with a failing keep: %v", err)
	}
	if kept != "cx:"+strings.Repeat("9", 32) {
		t.Fatalf("keep got %q", kept)
	}
	if id, _ := r.g.accounts.linkHolder("boinc", "9"); id != kept {
		t.Fatalf("the failed merge moved the user to %q", id)
	}
}

// A gateway that stopped with frames left in the WAL (a truncation a reader blocked, then a crash)
// truncates it when it opens the database again.
func TestScienceDBTruncatesWALAtOpen(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	s, err := openScienceDB(src)
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()
	reader, _ := sql.Open("sqlite", s.path)
	defer reader.Close()
	tx, _ := reader.Begin()
	var n int
	tx.QueryRow(`SELECT COUNT(*) FROM connector_requests`).Scan(&n) // keeps the frames in the WAL
	q := &connRequest{Connector: "boinc", Kind: reqCreateLink, Account: "gh:1", Email: "ada@example.org"}
	if err := s.addRequest(q, time.Now()); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{ScienceDBFile, ScienceDBFile + "-wal"} { // the files as a crash leaves them
		raw, err := os.ReadFile(filepath.Join(src, name))
		if err != nil {
			t.Fatal(err)
		}
		os.WriteFile(filepath.Join(dst, name), raw, 0o600)
	}
	tx.Rollback()
	if fi, _ := os.Stat(filepath.Join(dst, ScienceDBFile+"-wal")); fi == nil || fi.Size() == 0 {
		t.Fatal("the test needs frames in the WAL")
	}
	s2, err := openScienceDB(dst)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.close()
	if fi, err := os.Stat(filepath.Join(dst, ScienceDBFile+"-wal")); err == nil && fi.Size() != 0 {
		t.Errorf("the WAL has %d bytes after the open", fi.Size())
	}
	if got, err := s2.request("", q.ID); err != nil || got.Email != "ada@example.org" {
		t.Fatalf("the request after the open: %+v %v", got, err)
	}
}
