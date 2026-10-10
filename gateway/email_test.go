// SPDX-License-Identifier: Apache-2.0
package gateway

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestNormaliseEmail(t *testing.T) {
	cases := []struct {
		in, want string
		ok       bool
	}{
		{"Someone@Example.org", "someone@example.org", true},
		{"First.Last+news@gmail.com", "firstlast@gmail.com", true},
		{"f.i.r.s.t.l.a.s.t@GoogleMail.com", "firstlast@gmail.com", true},
		{"first.last+tag@example.org", "first.last+tag@example.org", true}, // only Gmail drops dots and tags
		{"o'brien@sub.example.co.uk", "o'brien@sub.example.co.uk", true},
		{"user@xn--bcher-kva.example", "user@xn--bcher-kva.example", true},
		{"+tag@gmail.com", "", false}, // nothing left of the local part
		{"user@bücher.example", "", false},
		{"usér@example.org", "", false},
		{"no-at-sign", "", false},
		{"a@b@example.org", "", false},
		{"a..b@example.org", "", false},
		{".a@example.org", "", false},
		{"a@localhost", "", false},
		{"a@192.168.1.1", "", false},
		{"a@[192.168.1.1]", "", false},
		{"a@-bad.example", "", false},
		{"\"quoted\"@example.org", "", false},
		{"a b@example.org", "", false},
		{"a@example.org\r\nBcc: x@example.org", "", false},
		{"Name <a@example.org>", "", false},
		{strings.Repeat("a", 65) + "@example.org", "", false},
	}
	for _, c := range cases {
		got, ok := normaliseEmail(c.in)
		if got != c.want || ok != c.ok {
			t.Errorf("normaliseEmail(%q) = %q, %v; want %q, %v", c.in, got, ok, c.want, c.ok)
		}
	}
}

func TestDisposable(t *testing.T) {
	for d, want := range map[string]bool{"mailinator.com": true, "x.mailinator.com": true, "gmail.com": false,
		"example.org": false, "amailinator.com.example": false} {
		if isDisposable(d) != want {
			t.Errorf("isDisposable(%q) != %v", d, want)
		}
	}
}

func TestClientIP(t *testing.T) {
	cases := []struct{ remote, xff, want string }{
		{"203.0.113.9:4000", "198.51.100.1", "203.0.113.9"}, // not from Caddy: XFF ignored
		{"127.0.0.1:4000", "198.51.100.1, 198.51.100.2", "198.51.100.2"},
		{"10.88.0.5:4000", "1.1.1.1, 198.51.100.7", "198.51.100.7"}, // rightmost: what Caddy saw
		{"[::1]:4000", "2001:db8:1:2:3:4:5:6", "2001:db8:1:2::/64"},
		{"[2001:db8:a:b:c::1]:4000", "", "2001:db8:a:b::/64"},
		{"127.0.0.1:4000", "garbage", "127.0.0.1"},
		{"127.0.0.1:4000", "", "127.0.0.1"},
		{"[::ffff:127.0.0.1]:4000", "198.51.100.3", "198.51.100.3"},
	}
	for _, c := range cases {
		r, _ := http.NewRequest("POST", "/", nil)
		r.RemoteAddr = c.remote
		if c.xff != "" {
			r.Header.Set("X-Forwarded-For", c.xff)
		}
		if got := clientIP(r); got != c.want {
			t.Errorf("clientIP(%s, %q) = %q, want %q", c.remote, c.xff, got, c.want)
		}
	}
}

// The send limits: per address 1/min, 5/h, 10/day; per client 10/h, 30/day; 300/day in all.
func TestEmailSendLimits(t *testing.T) {
	now := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	e := events{}
	send := func(key string, rules []rule) bool {
		if !e.allows(key, now, rules) {
			return false
		}
		e.add(key, now)
		return true
	}
	if !send("a", perAddress) || send("a", perAddress) {
		t.Fatal("one send per minute per address")
	}
	n := 1
	for i := 0; i < 30; i++ {
		now = now.Add(61 * time.Second)
		if send("a", perAddress) {
			n++
		}
	}
	if n != 5 {
		t.Fatalf("%d sends in an hour, want 5", n)
	}
	for i := 0; i < 24*60; i++ {
		now = now.Add(time.Minute)
		if send("a", perAddress) {
			n++
		}
		if i == 22*60 && n != 10 {
			t.Fatalf("%d sends within a day, want 10", n)
		}
	}
	n = 0
	for i := 0; i < 50; i++ {
		if send("c", perClient) {
			n++
		}
	}
	if n != 10 {
		t.Fatalf("%d sends per client in an hour, want 10", n)
	}
	n = 0
	for i := 0; i < 400; i++ {
		if send("*", perDay) {
			n++
		}
	}
	if n != 300 {
		t.Fatalf("%d sends in a day, want 300", n)
	}
	e.prune(now.Add(25 * time.Hour))
	if len(e) != 0 {
		t.Fatalf("prune left %d keys", len(e))
	}
}

func TestAccountFromEmailEm(t *testing.T) {
	id := newEmailAccountID()
	if got, ok := accountFromEmail(accountEmail(id)); !ok || got != id {
		t.Fatalf("round trip %s: %s %v", id, got, ok)
	}
	if got, ok := accountFromEmail(strings.ToUpper(accountEmail(id))); !ok || got != id {
		t.Fatal("the chat lowercases addresses; an uppercase copy must map to the same account")
	}
	for _, bad := range []string{"em-@users.invalid", "em-" + strings.Repeat("g", 32) + "@users.invalid",
		"em-" + strings.Repeat("a", 31) + "@users.invalid", "em-" + strings.Repeat("a", 33) + "@users.invalid",
		"em-" + strings.Repeat("a", 30) + ":x@users.invalid"} {
		if _, ok := accountFromEmail(bad); ok {
			t.Errorf("accepted %q", bad)
		}
	}
}

func TestEmailConfigCheck(t *testing.T) {
	good := func() EmailConfig {
		return EmailConfig{SMTPHost: "smtp.example", SMTPPort: 587, SMTPUser: "u", SMTPPassword: "p",
			From: "AIs@home <noreply@aisathome.org>", Keys: []EmailKey{{"k1", strings.Repeat("x", 32)}}}
	}
	c := good()
	if err := c.check(); err != nil {
		t.Fatal(err)
	}
	for name, mod := range map[string]func(*EmailConfig){
		"no host":       func(c *EmailConfig) { c.SMTPHost = "" },
		"no port":       func(c *EmailConfig) { c.SMTPPort = 0 },
		"no password":   func(c *EmailConfig) { c.SMTPPassword = "" },
		"bad from":      func(c *EmailConfig) { c.From = "not an address" },
		"no keys":       func(c *EmailConfig) { c.Keys = nil },
		"short secret":  func(c *EmailConfig) { c.Keys[0].Secret = "short" },
		"duplicate id":  func(c *EmailConfig) { c.Keys = append(c.Keys, c.Keys[0]) },
		"odd key id":    func(c *EmailConfig) { c.Keys[0].ID = "a:b" },
		"empty key id":  func(c *EmailConfig) { c.Keys[0].ID = "" },
		"port too high": func(c *EmailConfig) { c.SMTPPort = 70000 },
	} {
		c := good()
		mod(&c)
		if c.check() == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestNewCodeIsSixDigits(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 200; i++ {
		c := newCode()
		if len(c) != 6 || strings.Trim(c, "0123456789") != "" {
			t.Fatalf("code %q", c)
		}
		seen[c] = true
	}
	if len(seen) < 190 {
		t.Fatalf("only %d distinct codes in 200", len(seen))
	}
}
