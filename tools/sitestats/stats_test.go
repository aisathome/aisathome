// SPDX-License-Identifier: Apache-2.0
package main

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

const (
	host      = "aisathome.org"
	firefox   = "Mozilla/5.0 (X11; Linux x86_64; rv:140.0) Gecko/20100101 Firefox/140.0"
	chrome    = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/141.0.0.0 Safari/537.36"
	googlebot = "Mozilla/5.0 (compatible; Googlebot/2.1; +http://www.google.com/bot.html)"
)

var day1 = time.Date(2026, 10, 19, 10, 0, 0, 0, time.UTC)

type req struct {
	at                          time.Time
	ip, ua, uri, ref, host, how string
	status                      int
}

// line renders a request as Caddy's JSON access log does.
func line(r req) []byte {
	if r.host == "" {
		r.host = host
	}
	if r.how == "" {
		r.how = "GET"
	}
	if r.status == 0 {
		r.status = 200
	}
	if r.at.IsZero() {
		r.at = day1
	}
	h := map[string][]string{"Accept": {"text/html"}, "Cookie": {"REDACTED"}}
	if r.ua != "" {
		h["User-Agent"] = []string{r.ua}
	}
	if r.ref != "" {
		h["Referer"] = []string{r.ref}
	}
	b, _ := json.Marshal(map[string]any{
		"level": "info", "ts": float64(r.at.UnixNano()) / 1e9, "logger": "http.log.access.log0", "msg": "handled request",
		"request": map[string]any{"remote_ip": r.ip, "remote_port": "51234", "client_ip": r.ip, "proto": "HTTP/2.0",
			"method": r.how, "host": r.host, "uri": r.uri, "headers": h},
		"status": r.status, "size": 1234, "resp_headers": map[string][]string{"Content-Type": {"text/html"}},
	})
	return b
}

func newAgg() *Aggregator {
	return NewAggregator(host, &Stats{}, &State{})
}

func TestNoIPUserAgentOrHashInOutput(t *testing.T) {
	a := newAgg()
	ips := []string{"203.0.113.77", "2001:db8::4711", "198.51.100.23"}
	uas := []string{firefox, chrome, "Mozilla/5.0 (iPhone; CPU iPhone OS 18_0 like Mac OS X) Mobile/15E148 UniqueMarker42"}
	for i, ip := range ips {
		a.Add(line(req{ip: ip, ua: uas[i], uri: "/docs/?utm_source=secretquery&q=x", ref: "https://news.ycombinator.com/item?id=4711"}))
	}
	a.Add(line(req{ip: "192.0.2.9", ua: googlebot, uri: "/"}))
	a.Finish(day1)
	out, _ := json.Marshal(a.Stats)
	s := string(out)
	for _, bad := range append(append(ips, uas...), "192.0.2.9", "Googlebot/2.1", "UniqueMarker42", "secretquery",
		"utm_source", "item?id", "REDACTED", "51234", hex.EncodeToString(a.State.Salt)) {
		if strings.Contains(s, bad) {
			t.Errorf("the output contains %q: %s", bad, s)
		}
	}
	for h := range a.seen {
		if strings.Contains(s, h) {
			t.Errorf("the output contains a visitor hash")
		}
	}
	d := a.Stats.Days["2026-10-19"]
	if d.PageViews != 3 || d.Visitors != 3 || d.Pages["/docs/"] != 3 || d.Referrers["news.ycombinator.com"] != 3 {
		t.Errorf("counts: %+v", d)
	}
}

func TestSaltRotatesEachUTCDayAndVisitsCannotBeLinked(t *testing.T) {
	a := newAgg()
	same := req{ip: "203.0.113.5", ua: firefox, uri: "/"}
	same.at = day1
	a.Add(line(same))
	same.uri = "/node/"
	a.Add(line(same)) // same visitor, same day: one visitor
	a.Add(line(req{ip: "203.0.113.6", ua: firefox, uri: "/"}))
	salt1 := append([]byte(nil), a.State.Salt...)
	hash1 := visitorHash(salt1, same.ip, same.ua)
	if len(salt1) != 32 || a.State.Day != "2026-10-19" {
		t.Fatalf("salt %d bytes for %q", len(salt1), a.State.Day)
	}

	// The next UTC day: a new salt, the old one and its hashes are gone.
	same.at = time.Date(2026, 10, 20, 0, 0, 1, 0, time.UTC)
	a.Add(line(same))
	if bytes.Equal(a.State.Salt, salt1) || a.State.Day != "2026-10-20" {
		t.Fatal("the salt did not change with the day")
	}
	if a.seen[hash1] || len(a.seen) != 1 {
		t.Fatalf("yesterday's hashes are still kept: %d", len(a.seen))
	}
	if visitorHash(a.State.Salt, same.ip, same.ua) == hash1 {
		t.Fatal("the same visitor has the same hash on two days")
	}
	// A late line for the previous day still counts as a view, but not as a visitor (no salt).
	a.Add(line(req{at: day1.Add(13 * time.Hour), ip: "203.0.113.7", ua: chrome, uri: "/"}))
	if d1, d2 := a.Stats.Days["2026-10-19"], a.Stats.Days["2026-10-20"]; d1.Visitors != 2 || d1.PageViews != 4 || d2.Visitors != 1 {
		t.Errorf("visitors day 1 %d (views %d), day 2 %d", d1.Visitors, d1.PageViews, d2.Visitors)
	}

	// Without any traffic, the end of the day still destroys the salt.
	salt2 := append([]byte(nil), a.State.Salt...)
	a.Finish(time.Date(2026, 10, 21, 0, 0, 30, 0, time.UTC))
	if bytes.Equal(a.State.Salt, salt2) || a.State.Day != "2026-10-21" || len(a.State.Seen) != 0 {
		t.Fatalf("the salt survived the end of the day: day %s, %d hashes", a.State.Day, len(a.State.Seen))
	}
	// Within a day, Finish keeps the salt and hands the hashes to the next run.
	a.Add(line(req{at: time.Date(2026, 10, 21, 8, 0, 0, 0, time.UTC), ip: "203.0.113.5", ua: firefox, uri: "/"}))
	salt3 := append([]byte(nil), a.State.Salt...)
	a.Finish(time.Date(2026, 10, 21, 8, 1, 0, 0, time.UTC))
	if !bytes.Equal(a.State.Salt, salt3) || len(a.State.Seen) != 1 {
		t.Fatal("Finish within the same day changed the salt or lost the hashes")
	}
}

func TestBotsAndOwnTestsAreCountedSeparately(t *testing.T) {
	a := newAgg()
	for _, ua := range []string{googlebot, "Mozilla/5.0 (compatible; bingbot/2.0)", "curl/8.5.0",
		"python-requests/2.32", "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 HeadlessChrome/141.0.0.0 Safari/537.36",
		"Mozilla/5.0 (compatible; SomeNewCrawler/1.0)", "", "Feedly/1.0", "facebookexternalhit/1.1"} {
		a.Add(line(req{ip: "192.0.2.1", ua: ua, uri: "/"}))
	}
	a.Add(line(req{ip: "192.0.2.2", ua: chrome + " aisathome-test", uri: "/devlog/"}))
	a.Add(line(req{ip: "192.0.2.2", ua: "aisathome-smoke", uri: "/"}))
	a.Add(line(req{ip: "192.0.2.3", ua: firefox, uri: "/"}))
	d := a.Stats.Days["2026-10-19"]
	if d.PageViews != 1 || d.Visitors != 1 || d.Pages["/"] != 1 || d.Pages["/devlog/"] != 0 {
		t.Errorf("humans: %+v", d)
	}
	if d.Tests != 2 {
		t.Errorf("tests %d, want 2", d.Tests)
	}
	want := map[string]int{"Googlebot": 1, "bingbot": 1, "curl": 1, "python": 1, "HeadlessChrome": 1, "other": 2,
		"no user agent": 1, "facebookexternalhit": 1}
	if fmt.Sprint(d.Bots) != fmt.Sprint(want) {
		t.Errorf("bots %v, want %v", d.Bots, want)
	}
}

func TestReferrerIsReducedToItsDomain(t *testing.T) {
	for ref, want := range map[string]string{
		"https://www.reddit.com/r/gridcoin/comments/abc/title/?share_id=x": "reddit.com",
		"https://old.reddit.com/r/gridcoin/":                               "old.reddit.com",
		"https://NEWS.ycombinator.com:443/item?id=1":                       "news.ycombinator.com",
		"android-app://com.reddit.frontpage/":                              "com.reddit.frontpage",
		"https://aisathome.org/docs/":                                      "", // ourselves
		"https://chat.aisathome.org/":                                      "",
		"https://user:pass@example.com/x":                                  "example.com",
		"https://192.0.2.1/":                                               "", // not a domain: could be an address
		"https://[2001:db8::1]/":                                           "",
		"https://localhost/":                                               "",
		"not a url":                                                        "",
		"":                                                                 "",
	} {
		if got := referrerDomain(ref, host); got != want {
			t.Errorf("referrerDomain(%q) = %q, want %q", ref, got, want)
		}
	}
	if got := referrerDomain("https://aisathome.org/", "staging.aisathome.org"); got != "" {
		t.Errorf("prod referring to staging counted as %q", got)
	}
}

func TestPathNormalisation(t *testing.T) {
	for uri, want := range map[string]string{
		"/":                            "/",
		"/index.html":                  "/",
		"/docs/?q=1&utm_source=x":      "/docs/",
		"/docs/index.html?x":           "/docs/",
		"//node//install/":             "/node/install/",
		"/devlog/../about/":            "/about/",
		"/about/privacy/#cookies":      "/about/privacy/",
		"/caf%C3%A9/":                  "/café/",
		"/404.html":                    "/404.html",
		"/devlog/rss.xml?format=x":     "/devlog/rss.xml",
		"/x%00y/":                      "",
		"/bad%zz/":                     "",
		"relative":                     "",
		"/" + strings.Repeat("a", 300): "",
	} {
		got, ok := normalizePath(uri)
		if (want == "") == ok || got != want {
			t.Errorf("normalizePath(%q) = %q, %v; want %q", uri, got, ok, want)
		}
	}
}

func TestOnlySitePagesCount(t *testing.T) {
	a := newAgg()
	for _, r := range []req{
		{uri: "/v1/stats"}, {uri: "/account"}, {uri: "/account/keys"}, {uri: "/stats/user.gz"}, {uri: "/__login"},
		{uri: "/_astro/x.css"}, {uri: "/favicon.svg"}, {uri: "/pagefind/pagefind.js"},
		{uri: "/", host: "api.aisathome.org"}, {uri: "/", host: "chat.aisathome.org"}, {uri: "/", host: "evil.example"},
		{uri: "/", how: "POST"}, {uri: "/", how: "HEAD"}, {uri: "/docs", status: 308}, {uri: "/", status: 500},
	} {
		r.ip, r.ua = "203.0.113.1", firefox
		a.Add(line(r))
	}
	a.Add(line(req{ip: "203.0.113.1", ua: firefox, uri: "/nope/?x=<script>", status: 404}))
	a.Add(line(req{ip: "203.0.113.1", ua: firefox, uri: "/devlog/rss.xml"}))
	a.Add(line(req{ip: "203.0.113.1", ua: firefox, uri: "/docs/", status: 304, host: "aisathome.org:443"}))
	a.Add([]byte("not json"))
	a.Add([]byte(`{"ts":1}`))
	d := a.Stats.Days["2026-10-19"]
	if d == nil || d.PageViews != 1 || d.Pages["/docs/"] != 1 || d.NotFound != 1 || d.Feed != 1 || len(d.Pages) != 1 {
		t.Fatalf("counts: %+v", d)
	}
}

func TestCapsKeepTheFileSmall(t *testing.T) {
	a := newAgg()
	for i := range maxReferrers + 50 {
		a.Add(line(req{ip: "203.0.113.1", ua: firefox, uri: "/", ref: fmt.Sprintf("https://spam%d.example/", i)}))
	}
	d := a.Stats.Days["2026-10-19"]
	if len(d.Referrers) != maxReferrers+1 || d.Referrers[other] != 50 {
		t.Errorf("%d referrers, %d other", len(d.Referrers), d.Referrers[other])
	}
}

// Run end to end on a file: new lines are counted once, read lines are freed, a partial line
// waits, the salt stays in the state directory and never reaches the output.
func TestRunReadsFreesAndContinues(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux only")
	}
	dir := t.TempDir()
	logPath, stateDir, out := filepath.Join(dir, "access.log"), filepath.Join(dir, "state"), filepath.Join(dir, "stats.json")
	os.MkdirAll(stateDir, 0o700)
	appendLog := func(b ...[]byte) {
		f, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
		if err != nil {
			t.Fatal(err)
		}
		for _, x := range b {
			f.Write(x)
		}
		f.Close()
	}
	read := func() Stats {
		var s Stats
		b, err := os.ReadFile(out)
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(b, &s); err != nil {
			t.Fatal(err)
		}
		return s
	}
	nl := []byte{'\n'}
	appendLog(line(req{ip: "203.0.113.50", ua: firefox, uri: "/"}), nl,
		line(req{ip: "203.0.113.51", ua: chrome, uri: "/node/"}), nl)
	partial := line(req{ip: "203.0.113.52", ua: firefox, uri: "/about/"})
	appendLog(partial[:40])

	if err := Run(host, logPath, stateDir, out, day1.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if d := read().Days["2026-10-19"]; d.PageViews != 2 || d.Visitors != 2 {
		t.Fatalf("first run: %+v", d)
	}
	raw, _ := os.ReadFile(logPath)
	if bytes.Contains(raw, []byte("203.0.113.5")) || bytes.Contains(raw, []byte("Firefox")) {
		t.Fatal("the read lines are still in the log file")
	}
	if !bytes.HasSuffix(raw, partial[:40]) {
		t.Fatal("the partial line was lost")
	}

	appendLog(partial[40:], nl, line(req{ip: "203.0.113.50", ua: firefox, uri: "/docs/"}), nl)
	if err := Run(host, logPath, stateDir, out, day1.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	s := read()
	if d := s.Days["2026-10-19"]; d.PageViews != 4 || d.Visitors != 3 || d.Pages["/about/"] != 1 {
		t.Fatalf("second run: %+v", d)
	}
	var st State
	b, _ := os.ReadFile(filepath.Join(stateDir, "state.json"))
	json.Unmarshal(b, &st)
	if len(st.Salt) != 32 || len(st.Seen) != 3 {
		t.Fatalf("state: salt %d bytes, %d hashes", len(st.Salt), len(st.Seen))
	}
	if fi, _ := os.Stat(filepath.Join(stateDir, "state.json")); fi.Mode().Perm() != 0o600 {
		t.Errorf("state file mode %v", fi.Mode().Perm())
	}
	outRaw, _ := os.ReadFile(out)
	for _, bad := range []string{"203.0.113", "Firefox", "Chrome", hex.EncodeToString(st.Salt), st.Seen[0]} {
		if bytes.Contains(outRaw, []byte(bad)) {
			t.Errorf("the output contains %q", bad)
		}
	}
	raw, _ = os.ReadFile(logPath)
	if len(bytes.Trim(raw, "\x00")) != 0 {
		t.Fatalf("log not fully freed: %q", bytes.Trim(raw, "\x00"))
	}

	// The log is replaced (tmpfs remounted after a reboot): read from its start.
	os.Remove(logPath)
	appendLog(line(req{ip: "203.0.113.60", ua: firefox, uri: "/"}), nl)
	if err := Run(host, logPath, stateDir, out, day1.Add(3*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if d := read().Days["2026-10-19"]; d.PageViews != 5 {
		t.Fatalf("after a new log file: %+v", d)
	}
	// No log file at all is fine (nothing served yet).
	os.Remove(logPath)
	if err := Run(host, logPath, stateDir, out, day1.Add(4*time.Minute)); err != nil {
		t.Fatal(err)
	}
}

func TestReport(t *testing.T) {
	s := &Stats{Host: host, Updated: day1, Days: map[string]*Day{
		"2026-10-18": {PageViews: 3, Visitors: 2, Pages: map[string]int{"/": 2, "/devlog/": 1}, Bots: map[string]int{"Googlebot": 4}},
		"2026-10-19": {PageViews: 5, Visitors: 4, Pages: map[string]int{"/": 5}, Referrers: map[string]int{"reddit.com": 2}, Tests: 7},
	}}
	var b bytes.Buffer
	Report(&b, s, 2, 10, day1)
	got := b.String()
	for _, want := range []string{"2026-10-18       3         2", "2026-10-19       5         4", "total            8         6",
		"       7  /", "       2  reddit.com", "       4  Googlebot"} {
		if !strings.Contains(got, want) {
			t.Errorf("report lacks %q:\n%s", want, got)
		}
	}
}
