// SPDX-License-Identifier: Apache-2.0
package localui

import (
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"
)

// askLink is what the pipe answers a caller it let in.
func askLink(t *testing.T, e *env) string {
	t.Helper()
	ans := e.ui.pipeAnswer([]byte(`{"op":"login-link"}` + "\n"))
	if ans.Error != "" || ans.URL == "" {
		t.Fatalf("pipe answer %+v", ans)
	}
	if !ValidLoginLink(ans.URL, DefaultPort) {
		t.Fatalf("not a valid link: %s", ans.URL)
	}
	u, _ := url.Parse(ans.URL)
	return u.RequestURI() // the test server listens elsewhere
}

func sessionCookie(resp *http.Response) *http.Cookie {
	for _, c := range resp.Cookies() {
		if c.Name == cookieName && c.Value != "" {
			return c
		}
	}
	return nil
}

func signedIn(t *testing.T, e *env, c *http.Cookie) bool {
	t.Helper()
	var st StatusResponse
	if err := json.Unmarshal([]byte(body(e.req(t, "GET", "/api/status", "", nil, c))), &st); err != nil {
		t.Fatal(err)
	}
	return st.Controls.SignedIn
}

// A link from the pipe signs in once: a session that works for the controls; the same link
// again, or after a minute, signs nobody in and leads to the page (which asks for the password).
func TestLoginLink(t *testing.T) {
	e := newEnv(t)
	now := time.Now()
	e.ui.auth.now = func() time.Time { return now }
	link := askLink(t, e)
	resp := e.req(t, "GET", link, "", map[string]string{"Sec-Fetch-Site": "none"})
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/" {
		t.Fatalf("link: %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
	c := sessionCookie(resp)
	if c == nil || !c.HttpOnly || c.SameSite != http.SameSiteStrictMode {
		t.Fatalf("link gave no proper session cookie: %+v", c)
	}
	if !signedIn(t, e, c) {
		t.Fatal("the link's session is not signed in")
	}
	if r := e.req(t, "POST", "/api/pause", "{}", nil, c); r.StatusCode != 200 || !e.agent.isPaused() {
		t.Fatalf("pause with the link's session: %d", r.StatusCode)
	}
	again := e.req(t, "GET", link, "", nil)
	if again.StatusCode != http.StatusSeeOther || sessionCookie(again) != nil {
		t.Fatalf("a used link signed in again: %d %v", again.StatusCode, again.Cookies())
	}

	late := askLink(t, e)
	now = now.Add(linkTTL)
	if r := e.req(t, "GET", late, "", nil); sessionCookie(r) != nil {
		t.Fatal("an expired link signed in")
	}
	now = now.Add(-linkTTL + time.Second)
	if r := e.req(t, "GET", late, "", nil); sessionCookie(r) != nil {
		t.Fatal("the expired link was kept and worked later")
	}
	for _, bad := range []string{"/login-link", "/login-link?t=", "/login-link?t=" + strings.Repeat("0", 64)} {
		if r := e.req(t, "GET", bad, "", nil); r.StatusCode != http.StatusSeeOther || sessionCookie(r) != nil {
			t.Errorf("%s: %d, cookie %v", bad, r.StatusCode, sessionCookie(r))
		}
	}
}

// Another site's page cannot use a link, even a valid one (it stays unused).
func TestLoginLinkNotCrossSite(t *testing.T) {
	e := newEnv(t)
	link := askLink(t, e)
	for _, site := range []string{"cross-site", "same-site"} {
		r := e.req(t, "GET", link, "", map[string]string{"Sec-Fetch-Site": site})
		if r.StatusCode != http.StatusForbidden || sessionCookie(r) != nil {
			t.Fatalf("Sec-Fetch-Site %s: %d", site, r.StatusCode)
		}
	}
	if r := e.req(t, "GET", link, "", map[string]string{"Sec-Fetch-Site": "none"}); sessionCookie(r) == nil {
		t.Fatal("the link was used up by a refused request")
	}
}

// Only maxLinks unused links are kept: a burst drops the oldest, not the newest.
func TestLoginLinkLimit(t *testing.T) {
	e := newEnv(t)
	first := askLink(t, e)
	var last string
	for i := 0; i < maxLinks; i++ {
		last = askLink(t, e)
	}
	if len(e.ui.auth.links) != maxLinks {
		t.Fatalf("%d links kept, want %d", len(e.ui.auth.links), maxLinks)
	}
	if r := e.req(t, "GET", first, "", nil); sessionCookie(r) != nil {
		t.Fatal("the oldest link survived the limit")
	}
	if r := e.req(t, "GET", last, "", nil); sessionCookie(r) == nil {
		t.Fatal("the newest link was dropped")
	}
}

// A new password ends links handed out before, as it ends sessions; with no password there
// are no links.
func TestLoginLinkPassword(t *testing.T) {
	e := newEnv(t)
	link := askLink(t, e)
	if err := os.Remove(e.pw); err != nil {
		t.Fatal(err)
	}
	if ans := e.ui.pipeAnswer([]byte(`{"op":"login-link"}`)); ans.URL != "" || !strings.Contains(ans.Error, "controls are off") {
		t.Fatalf("no password file: %+v", ans)
	}
	if r := e.req(t, "GET", link, "", nil); sessionCookie(r) != nil {
		t.Fatal("a link signed in without a password file")
	}
	for _, line := range []string{"", "nonsense", `{"op":"status"}`} {
		if ans := e.ui.pipeAnswer([]byte(line)); ans.URL != "" || ans.Error == "" {
			t.Errorf("%q: %+v", line, ans)
		}
	}
}

func TestValidLoginLink(t *testing.T) {
	sec := strings.Repeat("ab", 32)
	ok := "http://127.0.0.1:7381/login-link?t=" + sec
	if !ValidLoginLink(ok, 7381) {
		t.Fatal("rejected a good link")
	}
	for _, u := range []string{
		"http://127.0.0.1:7382/login-link?t=" + sec, // another port
		"https://127.0.0.1:7381/login-link?t=" + sec,
		"http://localhost:7381/login-link?t=" + sec,
		"http://evil.example/login-link?t=" + sec,
		"http://127.0.0.1:7381/elsewhere?t=" + sec,
		"http://127.0.0.1:7381/login-link?t=" + sec + "&next=http://evil.example",
		"http://127.0.0.1:7381/login-link?t=short",
		"http://u@127.0.0.1:7381/login-link?t=" + sec,
		"http://127.0.0.1:7381/login-link?t=" + sec + "#x",
		"javascript:alert(1)",
	} {
		if ValidLoginLink(u, 7381) {
			t.Errorf("accepted %s", u)
		}
	}
}

// The pipe's rule: administrators in any state, and readers of the password file.
func TestMayReadPassword(t *testing.T) {
	const (
		user  = "S-1-5-21-1-2-3-1001"
		other = "S-1-5-21-1-2-3-1002"
		users = "S-1-5-32-545"
		svc   = "S-1-5-80-1"
	)
	enabled := func(sid string) tokenGroup { return tokenGroup{SID: sid, Attrs: groupEnabled | 0x3} }
	denyOnly := func(sid string) tokenGroup { return tokenGroup{SID: sid, Attrs: 0x10} }
	installed := []string{"S-1-5-18", sidAdministrators, svc, user} // the installer's file: SYSTEM, admins, service, the user
	cases := []struct {
		name        string
		c           trayCaller
		allow, deny []string
		want        bool
	}{
		{"the installing user", trayCaller{User: user, Groups: []tokenGroup{enabled(users)}}, installed, nil, true},
		{"another standard user", trayCaller{User: other, Groups: []tokenGroup{enabled(users)}}, installed, nil, false},
		{"an admin, not elevated (UAC)", trayCaller{User: other, Groups: []tokenGroup{denyOnly(sidAdministrators), enabled(users)}}, installed, nil, true},
		{"an admin, elevated", trayCaller{User: other, Groups: []tokenGroup{enabled(sidAdministrators)}}, nil, nil, true},
		{"a group that may read", trayCaller{User: other, Groups: []tokenGroup{enabled(users)}}, []string{users}, nil, true},
		{"a deny-only group does not grant", trayCaller{User: other, Groups: []tokenGroup{denyOnly(users)}}, []string{users}, nil, false},
		{"a deny entry wins", trayCaller{User: user, Groups: []tokenGroup{enabled(users)}}, installed, []string{users}, false},
		{"a deny entry matches deny-only groups", trayCaller{User: user, Groups: []tokenGroup{denyOnly(users)}}, installed, []string{users}, false},
		{"no permissions readable: only admins", trayCaller{User: user}, nil, nil, false},
	}
	for _, c := range cases {
		if got := mayReadPassword(c.c, c.allow, c.deny); got != c.want {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
		}
	}
}

// The tray polls without the GPU: no nvidia-smi run for it.
func TestStatusWithoutGPU(t *testing.T) {
	e := newEnv(t)
	var st StatusResponse
	json.Unmarshal([]byte(body(e.req(t, "GET", "/api/status?gpu=0", "", nil))), &st)
	if len(st.GPU.GPUs) != 0 || st.GPU.Note != "" || st.NodeID == "" {
		t.Fatalf("gpu=0 ran the probe or lost the status: %+v", st)
	}
	json.Unmarshal([]byte(body(e.req(t, "GET", "/api/status", "", nil))), &st)
	if len(st.GPU.GPUs) != 1 {
		t.Fatalf("without gpu=0 the probe did not run: %+v", st.GPU)
	}
}
