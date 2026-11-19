// SPDX-License-Identifier: Apache-2.0
package gateway

import (
	"crypto/md5"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
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

func (r *pilotRig) claim(id, key string) {
	r.t.Helper()
	if w := r.form(id, "/account/connectors/boinc/claim", url.Values{"key": {key}}); w.Code != http.StatusSeeOther {
		r.t.Fatalf("claim form: HTTP %d: %s", w.Code, w.Body)
	}
}

func (r *pilotRig) answer(rid string, body map[string]any) map[string]any {
	r.t.Helper()
	return r.want(200, "POST", requestsPath+"/"+rid, testConnectorKey, body)["request"].(map[string]any)
}

func weakKey(user string) string { return user + "_" + strings.Repeat("ab12", 8) }

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

	r.claim("gh:42", weakKey("5"))
	q := r.onlyPending()
	if q["kind"] != reqClaim || q["user"] != "5" || q["key"] != weakKey("5") || statusText(q) != statusText(map[string]any{
		"id": q["id"], "kind": "claim", "created_at": q["created_at"], "user": "5", "key": weakKey("5")}) {
		t.Fatalf("the connector's view of a claim: %v", q)
	}
	if strings.Contains(statusText(q), "gh:42") {
		t.Fatal("the connector sees which account asked")
	}
	if out := r.answer(q["id"].(string), map[string]any{"ok": true}); out["state"] != reqDone {
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
	for _, want := range []string{"boinccmd --project_attach https://boinc.test/proj/ " + weakKey("5"), "(claimed)", pid, "Chat on"} {
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
	// The same answer again changes nothing; another answer is a conflict.
	r.answer(q["id"].(string), map[string]any{"ok": true})
	if w, _ := r.call("POST", requestsPath+"/"+q["id"].(string), testConnectorKey, map[string]any{"ok": false, "error": "no"}, ""); w.Code != 409 {
		t.Errorf("a different answer to a done request: HTTP %d", w.Code)
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
		r.claim(thief, weakKey(owner.user))
		q := r.onlyPending()
		if out := r.answer(q["id"].(string), map[string]any{"ok": true}); out["state"] != reqFailed ||
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
		r.claim("gh:42", weakKey(user))
		r.answer(r.onlyPending()["id"].(string), map[string]any{"ok": true})
		a, _, _ := r.g.accounts.Get("gh:42")
		pl, _, _ := r.g.authPilot(cred)
		if a.PilotChatOff != tc.want || pl == nil || pl.scienceOnly != tc.want {
			t.Errorf("%+v: chat off %v, pilot science only %v", tc, a.PilotChatOff, pl != nil && pl.scienceOnly)
		}
	}
}

// Only an active account may claim: a banned (or waitlisted) one cannot ask, and one banned while
// its claim waits gets no link; the unclaimed account keeps its user.
func TestBannedCannotClaim(t *testing.T) {
	r := newLinkRig(t, nil)
	pid, _ := r.create()
	r.bind(pid, "5")
	p, _ := r.g.sci.pilot("boinc", pid)
	for _, st := range []string{StatusBanned, StatusWaitlisted} {
		r.addAccount("gh:"+st, st)
		if w := r.form("gh:"+st, "/account/connectors/boinc/claim", url.Values{"key": {weakKey("5")}}); w.Code != http.StatusBadRequest {
			t.Errorf("%s account's claim form: HTTP %d", st, w.Code)
		}
	}
	if l := r.pending(); len(l) != 0 {
		t.Fatalf("requests from inactive accounts: %v", l)
	}
	r.addAccount("gh:44", StatusActive)
	r.claim("gh:44", weakKey("5"))
	q := r.onlyPending()
	r.g.accounts.SetStatus("gh:44", StatusBanned)
	if out := r.answer(q["id"].(string), map[string]any{"ok": true}); out["state"] != reqFailed {
		t.Fatalf("a banned account's claim: %v", out)
	}
	if id, _ := r.g.accounts.linkHolder("boinc", "5"); id != p.account {
		t.Errorf("the user belongs to %q, want the unclaimed %q", id, p.account)
	}
	if a, _, _ := r.g.accounts.Get("gh:44"); len(a.Links) != 0 {
		t.Errorf("the banned account got links: %+v", a.Links)
	}
}

// "Run on BOINC": the connector gets a pseudonymous address (derived, never stored), the display
// name and the password's hash, and links the user it made; the page shows the attach command and
// the login. An own address passes through the pending request only; set_email works the same way.
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
	for _, want := range []string{"boinccmd --project_attach https://boinc.test/proj/ " + weakKey("900"), addr, "Use my own email address at BOINC"} {
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

	// Own address for that user: set_email.
	r.advance(time.Second)
	if w := r.form("gh:42", "/account/connectors/boinc/email", url.Values{"user": {"900"}, "email": {"ada@example.org"}}); w.Code != http.StatusSeeOther {
		t.Fatalf("email form: HTTP %d", w.Code)
	}
	q = r.onlyPending()
	if q["kind"] != reqSetEmail || q["user"] != "900" || q["email"] != "ada@example.org" {
		t.Fatalf("set_email: %v", q)
	}
	r.answer(q["id"].(string), map[string]any{"ok": true})
	if a, _, _ := r.g.accounts.Get("gh:42"); !a.Links[0].OwnEmail {
		t.Error("the link does not record the own address")
	}
	if page := r.page("gh:42"); strings.Contains(page, addr) || !strings.Contains(page, "has your address now") {
		t.Error("the page still shows the pseudonymous login, or no confirmation")
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
	if page := r.page("gh:43"); strings.Contains(page, "@nomail.test") || strings.Contains(page, "Use my own email") {
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
	if w := r.form("gh:42", "/account/connectors/boinc/claim", url.Values{"key": {weakKey("5")}}); w.Code != 400 ||
		!strings.Contains(w.Body.String(), "already waiting") {
		t.Errorf("second request: HTTP %d", w.Code)
	}
	for _, bad := range []string{"5", "5_short", "a@b_" + strings.Repeat("a", 32), "5_" + strings.Repeat("a", 32) + "!"} {
		if w := r.form("gh:43", "/account/connectors/boinc/claim", url.Values{"key": {bad}}); w.Code == http.StatusSeeOther {
			t.Errorf("claim key %q accepted", bad)
		}
	}
	if w := r.form("gh:42", "/account/connectors/nope/claim", url.Values{"key": {weakKey("5")}}); w.Code != 404 {
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
		w := r.form("gh:42", "/account/connectors/boinc/claim", url.Values{"key": {weakKey("5")}})
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
	r.claim("gh:42", weakKey("5"))
	q := r.onlyPending()
	// What handleRequestResult does, up to the crash.
	if ok, err := r.g.sci.verifyRequest(q["id"].(string), "5", "", p.account, r.clock()); !ok || err != nil {
		t.Fatal(ok, err)
	}
	if _, err := r.g.accounts.claimLink("gh:42", "boinc", "5", AccountLink{AttachKey: weakKey("5"), Via: LinkClaimed}, r.clock()); err != nil {
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
	if w := r.form(p.account, "/account/connectors/boinc/claim", url.Values{"key": {weakKey("6")}}); w.Code != 400 {
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
