// SPDX-License-Identifier: Apache-2.0
package gateway

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"testing"
	"time"
)

// fakeClock is a clock the test moves.
type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time      { return c.t }
func (c *fakeClock) add(d time.Duration) { c.t = c.t.Add(d) }
func testLinks() (*nodeLinks, *fakeClock) {
	c := &fakeClock{time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)}
	s := newNodeLinks()
	s.now = c.now
	return s, c
}

var userCodeRe = regexp.MustCompile(`^[BCDFGHJKLMNPQRSTVWXZ]{4}-[0-9]{4}$`)

func TestUserCodes(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 2000; i++ {
		c := newUserCode()
		if !userCodeRe.MatchString(c) {
			t.Fatalf("code %q has not the form KXQT-4821", c)
		}
		seen[c] = true
	}
	if len(seen) < 1990 {
		t.Errorf("only %d different codes in 2000", len(seen))
	}
	for in, want := range map[string]string{
		"KXQT-4821": "KXQT-4821", "kxqt4821": "KXQT-4821", " kxqt 48 21 ": "KXQT-4821",
		"KXQT-O8I1": "KXQT-0811", "kxqt-48l1": "KXQT-4811",
	} {
		if got, ok := normUserCode(in); !ok || got != want {
			t.Errorf("normUserCode(%q) = %q, %v; want %q", in, got, ok, want)
		}
	}
	for _, in := range []string{"", "KXQT-482", "KXQT-48211", "KAQT-4821", "KXQT-48A1", "4821-KXQT", "KXQT/4821", "ＫXQT-4821"} {
		if got, ok := normUserCode(in); ok {
			t.Errorf("normUserCode(%q) = %q, accepted", in, got)
		}
	}
}

// approve runs the page's approval of code on s, with a fake node creation.
func approve(t *testing.T, s *nodeLinks, account, code string) error {
	t.Helper()
	v, err := s.lookup(account, "198.51.100.1", code)
	if err != nil {
		return err
	}
	l, _, err := s.claim(account, "198.51.100.1", code, v.ID)
	if err != nil {
		return err
	}
	if !s.finish(l, "nt-n-0000beef."+strings.Repeat("x", 43), "n-0000beef", false) {
		return errors.New("finish: expired")
	}
	return nil
}

// A code is approved once and its token handed to the first poll only; the store keeps no code in clear.
func TestLinkApproveOnce(t *testing.T) {
	s, c := testLinks()
	device, user, err := s.start("203.0.113.5", LinkInfo{Name: "box\x07", GPU: "RTX 4090", VRAMMiB: 24564, OS: "Ubuntu", Agent: "aisathome-node/1.0"})
	if err != nil {
		t.Fatal(err)
	}
	if len(device) < 40 || !userCodeRe.MatchString(user) {
		t.Fatalf("device %q, user %q", device, user)
	}
	for k, l := range s.byDevice {
		for _, v := range []string{k, l.device, l.user, l.id} {
			if strings.Contains(v, device) || strings.Contains(v, user) || strings.Contains(v, strings.ReplaceAll(user, "-", "")) {
				t.Fatal("a code is kept in clear")
			}
		}
	}
	if st, iv, _, _ := s.poll(device); st != LinkPending || iv != linkInterval {
		t.Fatalf("poll before approval: %s %v", st, iv)
	}
	v, err := s.lookup("gh:1", "198.51.100.1", strings.ToLower(user))
	if err != nil {
		t.Fatal(err)
	}
	if v.Info.Name != "box" || v.Info.GPU != "RTX 4090" || v.Info.VRAMMiB != 24564 || !v.Expires.Equal(c.t.Add(linkTTL)) {
		t.Fatalf("view %+v", v)
	}
	c.add(6 * time.Second)
	if err := approve(t, s, "gh:1", user); err != nil {
		t.Fatal(err)
	}
	if err := approve(t, s, "gh:2", user); !errors.Is(err, errLinkUsed) {
		t.Fatalf("a second approval: %v", err)
	}
	if _, err := s.lookup("gh:1", "198.51.100.1", user); !errors.Is(err, errLinkUsed) {
		t.Fatalf("lookup after approval: %v", err)
	}
	c.add(6 * time.Second)
	st, _, tok, id := s.poll(device)
	if st != LinkApproved || !strings.HasPrefix(tok, "nt-n-0000beef.") || id != "n-0000beef" {
		t.Fatalf("poll after approval: %s %q %q", st, tok, id)
	}
	if st, _, tok, _ := s.poll(device); st != LinkExpired || tok != "" {
		t.Fatalf("second poll: %s %q", st, tok)
	}
	if _, err := s.lookup("gh:1", "198.51.100.1", user); !errors.Is(err, errLinkUnknown) {
		t.Fatalf("lookup after pickup: %v", err)
	}
	if len(s.byDevice) != 0 || len(s.byUser) != 0 {
		t.Fatal("the link is still kept")
	}
}

// After linkTTL a code is unknown on the page and the node's poll says expired; a node created for a token
// nobody picked up is handed back to be revoked.
func TestLinkExpiry(t *testing.T) {
	s, c := testLinks()
	device, user, _ := s.start("203.0.113.5", LinkInfo{})
	c.add(linkTTL - time.Second)
	if _, err := s.lookup("gh:1", "198.51.100.1", user); err != nil {
		t.Fatalf("a second before expiry: %v", err)
	}
	c.add(time.Second)
	if _, err := s.lookup("gh:1", "198.51.100.1", user); !errors.Is(err, errLinkUnknown) {
		t.Fatalf("at expiry: %v", err)
	}
	if st, _, _, _ := s.poll(device); st != LinkExpired {
		t.Fatalf("poll at expiry: %s", st)
	}
	if o := s.prune(); len(o) != 0 || len(s.byDevice) != 0 {
		t.Fatalf("prune: %v, %d left", o, len(s.byDevice))
	}

	// Approved late: the node still has linkPickup to pick its token up; after that it is an orphan.
	device, user, _ = s.start("203.0.113.5", LinkInfo{})
	c.add(linkTTL - 10*time.Second)
	if err := approve(t, s, "gh:1", user); err != nil {
		t.Fatal(err)
	}
	c.add(30 * time.Second)
	if o := s.prune(); len(o) != 0 {
		t.Fatalf("pruned within the pickup time: %v", o)
	}
	c.add(linkPickup)
	if o := s.prune(); len(o) != 1 || o[0] != (orphan{"gh:1", "n-0000beef"}) {
		t.Fatalf("orphans %v", o)
	}
	if st, _, tok, _ := s.poll(device); st != LinkExpired || tok != "" {
		t.Fatalf("poll after the pickup time: %s %q", st, tok)
	}

	// An approval that outlives its link (the node was created while it expired) reports it.
	_, user, _ = s.start("203.0.113.5", LinkInfo{})
	v, _ := s.lookup("gh:1", "198.51.100.1", user)
	l, _, err := s.claim("gh:1", "198.51.100.1", user, v.ID)
	if err != nil {
		t.Fatal(err)
	}
	c.add(linkTTL)
	if o := s.prune(); len(o) != 0 || len(s.byDevice) != 1 {
		t.Fatal("a link being approved was pruned")
	}
	if s.finish(l, "nt-x", "n-1", false) {
		t.Fatal("finish after expiry reported the link alive")
	}
	if len(s.byDevice) != 0 {
		t.Fatal("the expired link is still kept")
	}
}

func TestLinkDenyAndFailedApproval(t *testing.T) {
	s, _ := testLinks()
	device, user, _ := s.start("203.0.113.5", LinkInfo{})
	v, _ := s.lookup("gh:1", "198.51.100.1", user)
	if err := s.deny("gh:1", "198.51.100.1", user, "another-link"); !errors.Is(err, errLinkForm) {
		t.Fatalf("deny with another link's form: %v", err)
	}
	// A form shown for one link approves no other (the person saw that link's details).
	if _, _, err := s.claim("gh:1", "198.51.100.1", user, "another-link"); !errors.Is(err, errLinkForm) {
		t.Fatalf("approve with another link's form: %v", err)
	}
	// A failed creation (a limit) puts the link back: the person may fix it and approve again.
	l, _, _ := s.claim("gh:1", "198.51.100.1", user, v.ID)
	if err := s.deny("gh:1", "198.51.100.1", user, v.ID); !errors.Is(err, errLinkUsed) {
		t.Fatalf("deny while approving: %v", err)
	}
	if !s.finish(l, "", "", true) {
		t.Fatal("finish after a failed creation")
	}
	if st, _, _, _ := s.poll(device); st != LinkPending {
		t.Fatalf("after a failed approval: %s", st)
	}
	if err := s.deny("gh:1", "198.51.100.1", user, v.ID); err != nil {
		t.Fatal(err)
	}
	if err := approve(t, s, "gh:1", user); !errors.Is(err, errLinkUsed) {
		t.Fatalf("approve after deny: %v", err)
	}
	if st, _, tok, _ := s.poll(device); st != LinkDenied || tok != "" {
		t.Fatalf("poll after deny: %s", st)
	}
	if st, _, _, _ := s.poll(device); st != LinkExpired {
		t.Fatalf("second poll after deny: %s", st)
	}
}

// Polling faster than the interval gets slow_down and a longer interval (RFC 8628 §3.5), up to linkMaxWait.
func TestLinkSlowDown(t *testing.T) {
	s, c := testLinks()
	device, _, _ := s.start("203.0.113.5", LinkInfo{})
	s.poll(device)
	c.add(time.Second)
	st, iv, _, _ := s.poll(device)
	if st != LinkSlowDown || iv != linkInterval+linkSlowDown {
		t.Fatalf("fast poll: %s %v", st, iv)
	}
	c.add(iv)
	if st, iv2, _, _ := s.poll(device); st != LinkPending || iv2 != iv {
		t.Fatalf("poll at the new interval: %s %v", st, iv2)
	}
	for i := 0; i < 20; i++ {
		s.poll(device)
	}
	if _, iv, _, _ := s.poll(device); iv != linkMaxWait {
		t.Fatalf("interval grew to %v", iv)
	}
	if st, _, _, _ := s.poll("not-a-code"); st != LinkExpired {
		t.Fatalf("unknown device code: %s", st)
	}
}

// Codes per address and network-wide, and pending links at once, are limited; so are wrong codes per
// account and per address.
func TestLinkLimits(t *testing.T) {
	s, c := testLinks()
	burst, hourly := linkPerClient[0], linkPerClient[1]
	for i := 0; i < burst.n; i++ {
		if _, _, err := s.start("203.0.113.5", LinkInfo{}); err != nil {
			t.Fatalf("code %d: %v", i+1, err)
		}
	}
	if _, _, err := s.start("203.0.113.5", LinkInfo{}); !errors.Is(err, errLinkBusy) {
		t.Fatalf("code %d in %v: %v", burst.n+1, burst.per, err)
	}
	if _, _, err := s.start("203.0.113.6", LinkInfo{}); err != nil {
		t.Fatalf("another address: %v", err)
	}
	c.add(burst.per)
	if _, _, err := s.start("203.0.113.5", LinkInfo{}); err != nil {
		t.Fatalf("after %v: %v", burst.per, err)
	}
	s, c = testLinks()
	step := hourly.per / time.Duration(hourly.n) // spread evenly: never burst.n within burst.per
	for i := 0; i < hourly.n; i++ {
		if _, _, err := s.start("203.0.113.5", LinkInfo{}); err != nil {
			t.Fatalf("code %d spread over the hour: %v", i+1, err)
		}
		c.add(step - time.Second)
	}
	if _, _, err := s.start("203.0.113.5", LinkInfo{}); !errors.Is(err, errLinkBusy) {
		t.Fatalf("code %d in an hour: %v", hourly.n+1, err)
	}

	// Network-wide.
	s, _ = testLinks()
	for i := 0; i < linkNetwork[0].n; i++ {
		if _, _, err := s.start(fmt.Sprintf("10.0.%d.%d", i/200, i%200), LinkInfo{}); err != nil {
			t.Fatalf("code %d: %v", i+1, err)
		}
	}
	if _, _, err := s.start("10.9.9.9", LinkInfo{}); !errors.Is(err, errLinkFull) {
		t.Fatalf("over the network-wide limit: %v", err)
	}
	// Pending at once.
	s, _ = testLinks()
	for i := 0; i < linkMaxOpen; i++ {
		l := &nodeLink{device: fmt.Sprint("d", i), user: fmt.Sprint("u", i), expires: s.now().Add(time.Hour)}
		s.byDevice[l.device], s.byUser[l.user] = l, l
	}
	if _, _, err := s.start("10.9.9.9", LinkInfo{}); !errors.Is(err, errLinkFull) {
		t.Fatalf("over the pending limit: %v", err)
	}

	// Wrong codes: an account, then an address.
	s, c = testLinks()
	_, user, _ := s.start("203.0.113.5", LinkInfo{})
	for i := 0; i < linkMissAcct[0].n; i++ {
		if _, err := s.lookup("gh:1", "198.51.100.1", "BCDF-0000"); !errors.Is(err, errLinkUnknown) {
			t.Fatalf("wrong code %d: %v", i+1, err)
		}
	}
	if _, err := s.lookup("gh:1", "198.51.100.1", user); !errors.Is(err, errLinkTries) {
		t.Fatalf("the right code after too many wrong ones: %v", err)
	}
	if _, err := s.lookup("gh:2", "198.51.100.2", user); err != nil {
		t.Fatalf("another account elsewhere: %v", err)
	}
	for i := 0; i < linkMissAddr[0].n; i++ {
		s.lookup(fmt.Sprint("gh:", 10+i/5), "198.51.100.9", "not a code")
	}
	if _, err := s.lookup("gh:99", "198.51.100.9", user); !errors.Is(err, errLinkTries) {
		t.Fatalf("a fresh account at an address with too many wrong codes: %v", err)
	}
	if _, _, err := s.claim("gh:99", "198.51.100.9", user, "x"); !errors.Is(err, errLinkTries) {
		t.Fatalf("approve from that address: %v", err)
	}
	c.add(time.Hour)
	if _, err := s.lookup("gh:1", "198.51.100.1", user); !errors.Is(err, errLinkUnknown) { // expired by now
		t.Fatalf("an hour later: %v", err)
	}
}

func TestCleanNext(t *testing.T) {
	for in, want := range map[string]string{
		"chat": "chat", "link": "link", "link:kxqt4821": "link:KXQT-4821", "link:KXQT-4821": "link:KXQT-4821",
		"link:https://evil.test": "link", "link:KXQT-4821/../x": "link", "": "", "https://evil.test": "", "//evil": "",
		"Chat": "", "link:KXQT-4821\r\nX": "link",
	} {
		if got := cleanNext(in); got != want {
			t.Errorf("cleanNext(%q) = %q, want %q", in, got, want)
		}
	}
	if p, ok := nextPath("link:KXQT-4821"); !ok || p != "/link?code=KXQT-4821" {
		t.Errorf("nextPath: %q %v", p, ok)
	}
	if _, ok := nextPath("chat"); ok {
		t.Error("nextPath(chat) is a link path")
	}
}

func TestLinkInfoClean(t *testing.T) {
	i := cleanLinkInfo(LinkInfo{Name: strings.Repeat("n", 100), GPU: "<b>\x00RTX</b>", VRAMMiB: -5, OS: strings.Repeat("o", 200), Agent: "a\nb"})
	if len([]rune(i.Name)) != maxNodeName || i.GPU != "<b>RTX</b>" || i.VRAMMiB != 0 || len(i.OS) != 80 || i.Agent != "ab" {
		t.Errorf("cleaned %+v", i)
	}
}
