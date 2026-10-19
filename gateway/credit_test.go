// SPDX-License-Identifier: Apache-2.0
package gateway

import (
	"bytes"
	"compress/gzip"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func near(a, b, tol float64) bool { return math.Abs(a-b) <= tol*math.Max(1, math.Abs(b)) }

func TestCreditFormula(t *testing.T) {
	cases := []struct {
		tok  creditTokens
		want float64
	}{
		{creditTokens{Completion: 8}, 1},            // 8 output tokens = 1 credit
		{creditTokens{Prompt: 80}, 1},               // a new prompt token is a tenth
		{creditTokens{Prompt: 800, Cached: 800}, 1}, // a cached one a hundredth
		{creditTokens{Prompt: 1000, Cached: 200, Completion: 100}, 800*0.0125 + 200*0.00125 + 100*0.125},
		{creditTokens{Prompt: 10, Cached: 50}, 10 * 0.00125}, // cached never more than the prompt
		{creditTokens{Prompt: -5, Completion: -1}, 0},
	}
	for _, c := range cases {
		if got := c.tok.credit(); !near(got, c.want, 1e-12) {
			t.Errorf("%+v: %v, want %v", c.tok, got, c.want)
		}
	}
}

// updateAverage is BOINC's: a steady rate converges to itself, and with no work the average
// halves in a week (the reader's decay).
func TestUpdateAverageLikeBOINC(t *testing.T) {
	day := 86400.0
	var avg, at float64
	updateAverage(10*day, 9*day, 100, creditHalfLife, &avg, &at) // first grant: work over its duration
	if !near(avg, 100, 1e-12) || at != 10*day {
		t.Fatalf("first grant: avg %v at %v", avg, at)
	}
	for d := 11; d <= 80; d++ {
		updateAverage(float64(d)*day, 0, 100, creditHalfLife, &avg, &at)
	}
	if !near(avg, 100, 1e-9) {
		t.Fatalf("steady 100 a day: avg %v", avg)
	}
	// From 0 at a steady rate: about 94% after four weeks.
	avg, at = 0, 0
	updateAverage(day, 0, 1e-9, creditHalfLife, &avg, &at)
	for d := 2; d <= 29; d++ {
		updateAverage(float64(d)*day, 0, 100, creditHalfLife, &avg, &at)
	}
	if avg < 93 || avg > 95 {
		t.Fatalf("after four weeks: %v", avg)
	}
	// Two grants at the same instant use the limit (no division by zero).
	a2, t2 := 50.0, 5*day
	updateAverage(5*day, 0, 7, creditHalfLife, &a2, &t2)
	if math.IsInf(a2, 0) || math.IsNaN(a2) || !near(a2, 50+math.Ln2*7*day/creditHalfLife, 1e-12) {
		t.Fatalf("same instant: %v", a2)
	}
	now := time.Unix(int64(at+7*day), 0)
	if got := decayAverage(avg, at, now); !near(got, avg/2, 1e-9) {
		t.Fatalf("a week later: %v, want %v", got, avg/2)
	}
}

func TestLedgerGrantAndPersist(t *testing.T) {
	dir := t.TempDir()
	l := newCreditLedger()
	l.path = filepath.Join(dir, "credit.json")
	now := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	created := now.Add(-10 * 24 * time.Hour)
	l.grant("n-00000001", "gh:1", "", 10, created, created, now)
	l.grant("n-00000001", "gh:1", "", 5, created, created, now.Add(time.Hour))
	l.grant("n-00000002", "gh:2", "", 1, time.Time{}, time.Time{}, now)
	l.grant("home", "", "owner", 7, time.Time{}, time.Time{}, now)
	l.grant("home", "", "owner", 0, time.Time{}, time.Time{}, now)          // nothing
	l.grant("home", "", "owner", math.NaN(), time.Time{}, time.Time{}, now) // nothing
	a := l.account("gh:1")
	if a.Total != 15 || a.TodayAt(now) != 15 || a.TodayAt(now.Add(24*time.Hour)) != 0 || a.ID != 1 {
		t.Fatalf("account: %+v", a)
	}
	// First grant: credit per day since the account was created (10 days), not per second.
	if r := l.account("gh:2").RAC(now); !near(r, 1, 1e-9) {
		t.Fatalf("a new account's first grant gives %v a day, want 1 (over at least a day)", r)
	}
	if n := l.node("home"); n.Operator != "owner" || n.Account != "" || n.Total != 7 {
		t.Fatalf("maintainer node: %+v", n)
	}
	if l.f.Accounts["owner"] != nil || l.f.Operators["gh:1"] != nil {
		t.Fatal("operators and accounts must stay apart")
	}
	if err := l.save(); err != nil {
		t.Fatal(err)
	}
	l2 := newCreditLedger()
	l2.path = l.path
	l2.load(slog.New(slog.NewTextHandler(io.Discard, nil)))
	if got := l2.account("gh:1"); got == nil || got.Total != 15 || got.Avg != a.Avg || l2.f.NextUserID != 3 {
		t.Fatalf("after reload: %+v", got)
	}
	// A broken file is kept aside, not overwritten.
	os.WriteFile(l.path, []byte("{broken"), 0o600)
	l3 := newCreditLedger()
	l3.path = l.path
	l3.load(slog.New(slog.NewTextHandler(io.Discard, nil)))
	if _, err := os.Stat(l.path + ".bad"); err != nil || len(l3.f.Accounts) != 0 {
		t.Fatal("broken ledger not set aside")
	}
}

func TestPrefixCache(t *testing.T) {
	c := newPrefixCache()
	now := time.Now()
	sys := strings.Repeat("s", 600)
	p1 := sys + strings.Repeat("a", 1000)
	if m := c.served("n1", "alice", p1, "ANSWER", now, true); m != 0 {
		t.Fatalf("first request cached %d", m)
	}
	p2 := p1 + "ANSWER" + strings.Repeat("b", 500)
	if m := c.served("n1", "alice", p2, "", now.Add(time.Minute), true); m != (len(p1)+6)/cacheBlock*cacheBlock {
		t.Fatalf("follow-up cached %d bytes", m)
	}
	if m := c.served("n1", "bob", p2, "", now, false); m != 0 {
		t.Fatalf("another requester got %d cached bytes", m)
	}
	if m := c.served("n2", "alice", p2, "", now, false); m != 0 {
		t.Fatalf("another node got %d cached bytes", m)
	}
	// The continued conversation replaced its entry; a different one adds an entry.
	if n := len(c.nodes["n1"].convs); n != 1 {
		t.Fatalf("%d entries after a continued conversation", n)
	}
	c.served("n1", "alice", sys+strings.Repeat("z", 900), "", now.Add(2*time.Minute), true)
	if n := len(c.nodes["n1"].convs); n != 2 {
		t.Fatalf("%d entries after a second conversation", n)
	}
	if m := c.served("n1", "alice", p2, "", now.Add(2*time.Minute+cacheWindow+time.Second), false); m != 0 {
		t.Fatalf("after the window: %d cached bytes", m)
	}
	// Bounds: conversations per node, nodes, blocks per entry.
	for i := 0; i < 3*cacheConvsPerNode; i++ {
		c.served("n1", "who"+strings.Repeat("x", i), strings.Repeat("q", 300), "", now, true)
	}
	if n := len(c.nodes["n1"].convs); n > cacheConvsPerNode {
		t.Fatalf("%d conversations on one node", n)
	}
	for i := 0; i < cacheMaxNodes+20; i++ {
		c.served("node"+strings.Repeat("y", i), "a", "x", "", now.Add(time.Duration(i)), true)
	}
	if len(c.nodes) > cacheMaxNodes {
		t.Fatalf("%d nodes remembered", len(c.nodes))
	}
	if h := c.blockHashes(strings.Repeat("x", (cacheMaxBlocks+10)*cacheBlock)); len(h) != cacheMaxBlocks {
		t.Fatalf("%d blocks kept", len(h))
	}
}

func TestCleanUsername(t *testing.T) {
	ok := map[string]string{
		"  Crunch   Queen ": "Crunch Queen", "O'Neil": "O'Neil", "J.R. Smith": "J.R. Smith", "bjørn_42": "bjørn_42",
		"नमस्ते": "नमस्ते", "a-b": "a-b", "v1.2": "v1.2", "": "", "   ": "",
	}
	for in, want := range ok {
		if got, err := cleanUsername(in); err != nil || got != want {
			t.Errorf("%q: %q %v, want %q", in, got, err, want)
		}
	}
	bad := []string{"ab", strings.Repeat("x", 33), "example.com", "visit www.x", "http://x", "a@b", "a/b",
		"tab\there", "new\nline", "zero\u200bwidth", "rtl\u202eoverride", "<script>", "Anonymous", "ADMIN",
		"x.y.org", "\u0301mark", "a&b", "a\x00b"}
	for _, in := range bad {
		if got, err := cleanUsername(in); err == nil {
			t.Errorf("%q accepted as %q", in, got)
		}
	}
}

func TestUsernameUniqueAndActiveOnly(t *testing.T) {
	s, err := OpenAccountStore(filepath.Join(t.TempDir(), "a.json"))
	if err != nil {
		t.Fatal(err)
	}
	s.Update(func(f *AccountsFile) error { f.Settings.MaxActive = 2; return nil })
	for _, id := range []string{"gh:1", "gh:2", "gh:3"} {
		s.SignIn(id, time.Now())
	}
	if err := s.SetUsername("gh:1", "Alice"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetUsername("gh:2", "aLiCe"); err != errUsernameTaken {
		t.Fatalf("case-insensitive duplicate: %v", err)
	}
	if err := s.SetUsername("gh:1", "ALICE"); err != nil {
		t.Fatalf("changing one's own case: %v", err)
	}
	if err := s.SetUsername("gh:3", "Carol"); err == nil {
		t.Fatal("a waitlisted account set a username")
	}
	s.SetUsername("gh:2", "Bob")
	if n := s.publicNames(); len(n) != 2 || n["gh:1"].Name != "ALICE" {
		t.Fatalf("public names: %+v", n)
	}
	s.SetStatus("gh:2", StatusBanned) // banned accounts leave the public names, but keep theirs taken
	if _, ok := s.publicNames()["gh:2"]; ok {
		t.Fatal("a banned account is public")
	}
	s.SetUsername("gh:1", "")
	if len(s.publicNames()) != 0 {
		t.Fatal("clearing the username kept the account public")
	}
}

func TestFmtCredit(t *testing.T) {
	for in, want := range map[float64]string{0: "0.00", 1.234: "1.23", 12.34: "12.3", 999.94: "999.9", 1234.5: "1,235",
		1234567: "1,234,567", 100000: "100,000"} {
		if got := fmtCredit(in); got != want {
			t.Errorf("%v: %q, want %q", in, got, want)
		}
	}
}

// The exports hold only accounts with a username, in db_dump's format, with a stable ETag.
func TestExports(t *testing.T) {
	s, _ := OpenAccountStore(filepath.Join(t.TempDir(), "a.json"))
	s.Update(func(f *AccountsFile) error { f.Settings.MaxActive = 10; return nil })
	for _, id := range []string{"gh:1", "gh:2"} {
		s.SignIn(id, time.Now())
		s.AddNode(id, "private label", time.Now(), nil)
	}
	s.SetUsername("gh:1", "O'Neil")
	snapNodes := s.Snapshot()
	g := &Gateway{accounts: s, credit: newCreditLedger(), node: map[string]*nodeConn{}}
	now := time.Now()
	for i, a := range snapNodes.Accounts {
		g.credit.grant(a.Nodes[0].ID, a.ID, "", float64(10*(i+1)), a.Created, a.Nodes[0].Created, now)
	}
	g.credit.grant("home", "", "owner", 99, time.Time{}, time.Time{}, now)
	files := g.exports(now)
	gun := func(f exportFile) string {
		z, err := gzip.NewReader(bytes.NewReader(f.body))
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(z)
		return string(b)
	}
	users, hosts, teams, tables := gun(files["user.gz"]), gun(files["host.gz"]), gun(files["team.gz"]), string(files["tables.xml"].body)
	named := snapNodes.Accounts[0]
	for _, want := range []string{"<?xml version=\"1.0\" encoding=\"utf-8\"?>\n<users>\n<user>\n <id>1</id>\n <name>O&#39;Neil</name>\n",
		" <total_credit>10.000000</total_credit>\n", " <expavg_time>", "</user>\n</users>\n"} {
		if !strings.Contains(users, want) {
			t.Errorf("user.gz lacks %q:\n%s", want, users)
		}
	}
	if strings.Count(users, "<user>") != 1 || strings.Contains(users, "gh:") || strings.Contains(users, "cpid") {
		t.Errorf("user.gz holds the anonymous account, an account id or a CPID:\n%s", users)
	}
	if strings.Count(hosts, "<host>") != 1 || !strings.Contains(hosts, "<userid>1</userid>") ||
		strings.Contains(hosts, named.Nodes[0].ID) || strings.Contains(hosts, "private label") || strings.Contains(hosts, "owner") {
		t.Errorf("host.gz:\n%s", hosts)
	}
	if teams != "<?xml version=\"1.0\" encoding=\"utf-8\"?>\n<teams>\n</teams>\n" {
		t.Errorf("team.gz: %q", teams)
	}
	if !strings.Contains(tables, "<nusers_total>1</nusers_total>") || !strings.Contains(tables, "<nhosts_total>1</nhosts_total>") ||
		!strings.Contains(tables, "<total_credit>10.000000</total_credit>") {
		t.Errorf("tables.xml:\n%s", tables)
	}
	// Same data, same bytes and ETag (gzip without a time stamp).
	if again := g.exports(now); again["user.gz"].etag != files["user.gz"].etag {
		t.Error("user.gz ETag changes without a change")
	}
	// Over HTTP: ETag, 304, unknown files.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.SetPathValue("file", strings.TrimPrefix(r.URL.Path, "/stats/"))
		g.handleExport(w, r)
	}))
	defer srv.Close()
	resp, _ := http.Get(srv.URL + "/stats/user.gz")
	etag := resp.Header.Get("ETag")
	resp.Body.Close()
	if resp.StatusCode != 200 || etag == "" || resp.Header.Get("Content-Type") != "application/gzip" {
		t.Fatalf("user.gz: %d %q %q", resp.StatusCode, etag, resp.Header.Get("Content-Type"))
	}
	req, _ := http.NewRequest("GET", srv.URL+"/stats/user.gz", nil)
	req.Header.Set("If-None-Match", etag)
	if resp, _ := http.DefaultClient.Do(req); resp.StatusCode != http.StatusNotModified {
		t.Fatalf("If-None-Match: %d", resp.StatusCode)
	}
	if resp, _ := http.Get(srv.URL + "/stats/credit.json"); resp.StatusCode != 404 {
		t.Fatalf("unknown export: %d", resp.StatusCode)
	}
}
