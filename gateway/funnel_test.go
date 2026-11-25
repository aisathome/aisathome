// SPDX-License-Identifier: Apache-2.0
package gateway

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The gateway's daily counters: each event counts once, in the write that records it, on its UTC day;
// the admins' nodes are counted apart; a reconnect, or a node from before the counters started, is not
// a first connection; a node without an account linked to an account counts as linked, and the same
// machine under its new id is not connected "for the first time" again. The counters survive a reopen.
func TestFunnelCounts(t *testing.T) {
	path := filepath.Join(t.TempDir(), "accounts.json")
	s, err := OpenAccountStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if f := s.Snapshot(); f.Funnel == nil || f.Funnel.Since.IsZero() {
		t.Fatalf("a new store starts its counters: %+v", f.Funnel)
	}
	s.SetOwn(func(id string) bool { return id == "gh:1" })
	t0 := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	day0, day1 := "2026-11-20", "2026-11-21"
	for _, id := range []string{"gh:1", "gh:2"} {
		s.SignIn(id, t0)
		s.SetStatus(id, StatusActive)
	}
	// A node from before the counters started.
	oldTok, _, _, err := s.addNodeVia("gh:2", "", t0.Add(-time.Hour), nil, false, "")
	if err != nil {
		t.Fatal(err)
	}
	s.Update(func(f *AccountsFile) error { f.Funnel = &Funnel{Since: t0}; return nil })

	ownTok, _, _, _ := s.addNodeVia("gh:1", "", t0, nil, false, "")
	linkTok, linkID, _, _ := s.addNodeVia("gh:2", "", t0, nil, false, ViaNodeLink)
	s.addNodeVia("gh:1", "", t0, nil, false, ViaNodeLink)
	anonTok, _, err := s.RegisterAnon(t0)
	if err != nil {
		t.Fatal(err)
	}
	for _, tok := range []string{oldTok, ownTok, ownTok, linkTok, anonTok, anonTok} { // ownTok and anonTok reconnect
		if err := s.firstConnect(keyHash(tok), t0.Add(time.Minute)); err != nil {
			t.Fatal(err)
		}
	}
	got := *s.funnel().Days[day0]
	want := FunnelDay{Anon: 1, AccountNodes: 3, OwnNodes: 2, Links: 2, OwnLinks: 1, FirstConnect: 3, OwnFirst: 1}
	if got != want {
		t.Fatalf("day 0:\n got %+v\nwant %+v", got, want)
	}
	if r, _ := s.byNodeID(linkID); !r.node.Connected {
		t.Fatal("a connected node is not marked so")
	}

	// The next day: the node without an account (connected) is linked to gh:2 as a new node.
	anonAcct, _, _ := s.nodeToken(anonTok)
	t1 := t0.Add(24 * time.Hour)
	newTok, newID, _, _ := s.addNodeVia("gh:2", "", t1, nil, false, ViaNodeLink)
	if err := s.linkAnon(anonAcct, newID, t1); err != nil {
		t.Fatal(err)
	}
	if err := s.linkAnon(anonAcct, newID, t1); err != nil { // a second poll: counted once
		t.Fatal(err)
	}
	if _, _, ok := s.nodeToken(anonTok); ok {
		t.Fatal("the identity without an account is still there")
	}
	s.firstConnect(keyHash(newTok), t1)
	// One that never connected before its link connects for the first time under its new id.
	anon2, _, _ := s.RegisterAnon(t1)
	anon2Acct, _, _ := s.nodeToken(anon2)
	new2Tok, new2ID, _, _ := s.addNodeVia("gh:2", "", t1, nil, false, ViaNodeLink)
	s.linkAnon(anon2Acct, new2ID, t1)
	s.firstConnect(keyHash(new2Tok), t1)

	s2, err := OpenAccountStore(path) // reopened: the counters are on disk
	if err != nil {
		t.Fatal(err)
	}
	f := s2.funnel()
	if !f.Since.Equal(t0) || *f.Days[day0] != want {
		t.Fatalf("after reopening: since %v, day 0 %+v", f.Since, f.Days[day0])
	}
	want1 := FunnelDay{Anon: 1, AccountNodes: 2, Links: 2, AnonLinked: 2, FirstConnect: 1}
	if *f.Days[day1] != want1 {
		t.Fatalf("day 1:\n got %+v\nwant %+v", *f.Days[day1], want1)
	}
	raw, _ := os.ReadFile(path)
	var file struct {
		Funnel json.RawMessage `json:"funnel"`
	}
	json.Unmarshal(raw, &file)
	for _, s := range []string{"gh:", "n-", "a-", "ax:"} { // numbers only, no account or node
		if strings.Contains(string(file.Funnel), s) {
			t.Fatalf("the counters name %q: %s", s, file.Funnel)
		}
	}
}

// Days older than funnelKeepDays are dropped when a new day starts.
func TestFunnelKeepsDays(t *testing.T) {
	var f AccountsFile
	t0 := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	f.funnelDay(t0).Anon++
	f.funnelDay(t0.AddDate(0, 0, funnelKeepDays-1)).Anon++
	if len(f.Funnel.Days) != 2 {
		t.Fatalf("%d days", len(f.Funnel.Days))
	}
	f.funnelDay(t0.AddDate(0, 0, funnelKeepDays+1)).Anon++
	if _, ok := f.Funnel.Days["2025-01-01"]; ok || len(f.Funnel.Days) != 2 {
		t.Fatalf("days kept: %v", f.Funnel.Days)
	}
}

func writeSiteStats(t *testing.T, path string, days map[string]any) {
	t.Helper()
	raw, _ := json.Marshal(map[string]any{"host": "site.test", "updated": time.Now().UTC(), "days": days})
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatal(err)
	}
}

// The card: today and the 7 days before it, newest first, with the site's counts from the statistics
// file and the gateway's own; days before the gateway's counters show none; the summary leaves the
// project's own nodes out; the page shows no node or account.
func TestOpsFunnelCard(t *testing.T) {
	dir := t.TempDir()
	stats := filepath.Join(dir, "stats.json")
	g, err := New(Config{StateDir: dir, SiteStatsFile: stats,
		Accounts: &AccountsConfig{File: dir + "/accounts.json", PublicURL: "https://site.test", Admins: []string{"gh:1"},
			SessionSecret: strings.Repeat("s", 32), GitHubClientID: "id", GitHubClientSecret: "secret"}}, quietLog())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { g.Close() })
	now := time.Now().UTC()
	today, yday := now.Format(time.DateOnly), now.AddDate(0, 0, -1).Format(time.DateOnly)
	old := now.AddDate(0, 0, -5).Format(time.DateOnly)
	writeSiteStats(t, stats, map[string]any{
		today: map[string]any{"page_views": 50, "visitors": 40, "pages": map[string]int{"/node/": 10, "/node/windows/": 3, "/node/install/": 2, "/node/binaries/": 1, "/": 30},
			"installs": 4, "install_fetches": map[string]int{"curl": 4, "Wget": 1, "browser": 2}},
		yday: map[string]any{"visitors": 60, "pages": map[string]int{"/node/": 4}},
		old:  map[string]any{"visitors": 1000}, // before the gateway counted
		now.AddDate(0, 0, -9).Format(time.DateOnly): map[string]any{"visitors": 99999}, // not shown
	})
	for _, id := range []string{"gh:1", "gh:2"} {
		g.accounts.SignIn(id, now)
		g.accounts.SetStatus(id, StatusActive)
	}
	g.accounts.Update(func(f *AccountsFile) error { f.Funnel.Since = now.AddDate(0, 0, -2); return nil })
	ownTok, ownID, _, _ := g.accounts.addNodeVia("gh:1", "", now, nil, false, "")
	tok, id, _, _ := g.accounts.addNodeVia("gh:2", "", now, nil, false, ViaNodeLink)
	g.accounts.RegisterAnon(now)
	g.accounts.firstConnect(keyHash(ownTok), now)
	g.accounts.firstConnect(keyHash(tok), now)

	c := g.opsFunnel(now)
	if len(c.Rows) != funnelDays+1 || c.Rows[0].Day != today || c.Rows[1].Day != yday {
		t.Fatalf("rows %+v", c.Rows)
	}
	r := c.Rows[0]
	if r.Visitors != 40 || r.NodeViews != 16 || r.Installs != 4 || r.Fetches != "5 of 7" || !r.HasGateway ||
		r.Anon != 1 || r.AccountNodes != 2 || r.OwnNodes != 1 || r.Links != 1 || r.FirstConnect != 2 || r.OwnFirst != 1 {
		t.Fatalf("today %+v", r)
	}
	if c.Rows[5].Day != old || c.Rows[5].Visitors != 1000 || c.Rows[5].HasGateway || !c.Rows[2].HasGateway {
		t.Fatalf("days before the counters: %+v / %+v", c.Rows[5], c.Rows[2])
	}
	if c.Total.Visitors != 1100 || c.Total.NodeViews != 20 || c.Total.Installs != 4 || c.Total.FirstConnect != 2 {
		t.Fatalf("total %+v", c.Total)
	}
	for _, want := range []string{"1100 visitors", "20 node page views (1.8%)", "4 install-script fetchers (/node.sh, 0.4% of visitors; curl/Wget fetches: 5 of 7)",
		"gateway: 1 registered without an account, 1 added to an account, 1 nodes connected for the first time (all install paths)"} {
		if !strings.Contains(c.Summary, want) {
			t.Errorf("summary lacks %q: %s", want, c.Summary)
		}
	}
	// No rate between the install script and first connections: those come from every install path.
	if strings.Contains(c.Summary, "of install") || strings.Contains(c.Summary, "25.0%") {
		t.Errorf("summary relates first connections to install-script fetches: %s", c.Summary)
	}

	var b strings.Builder
	d := g.opsData("", now)
	d.shell = shell{Title: "Operations"}
	if err := opsTmpl.Execute(&b, d); err != nil {
		t.Fatal(err)
	}
	page := b.String()
	i := strings.Index(page, `id="funnel"`)
	if i < 0 {
		t.Fatal("no funnel card")
	}
	card := page[i:]
	card = card[:strings.Index(card, "release page</a>.</p></div>")+len("release page</a>")]
	for _, want := range []string{`data-funnel="` + today + `"`, `data-funnel="total"`, "(1 own)", "5 of 7", releasesURL, "–"} {
		if !strings.Contains(card, want) {
			t.Errorf("card lacks %q", want)
		}
	}
	for _, no := range []string{ownID, id, "gh:", "ax:"} {
		if strings.Contains(card, no) {
			t.Errorf("card names %q", no)
		}
	}

	// No statistics file: the gateway's counts, and why the site's are missing.
	os.Remove(stats)
	c = g.opsFunnel(now)
	if c.Site || !strings.Contains(c.SiteNote, "unavailable") || c.Rows[0].Visitors != 0 || c.Rows[0].Anon != 1 ||
		strings.Contains(c.Summary, "visitors") || !strings.Contains(c.Summary, "1 nodes connected for the first time") {
		t.Fatalf("without the file: %+v %q", c.Rows[0], c.Summary)
	}
}

// The statistics file is reread only when it changes; a broken one is reported, not shown as zeros.
func TestSiteStatsCache(t *testing.T) {
	var c siteStatsCache
	if _, err := c.read(""); err == nil {
		t.Fatal("no path: no error")
	}
	path := filepath.Join(t.TempDir(), "stats.json")
	writeSiteStats(t, path, map[string]any{"2026-11-20": map[string]any{"visitors": 3}})
	s, err := c.read(path)
	if err != nil || s.Days["2026-11-20"].Visitors != 3 {
		t.Fatalf("%+v %v", s, err)
	}
	if s2, _ := c.read(path); s2 != s {
		t.Fatal("an unchanged file was reread")
	}
	os.WriteFile(path, []byte("{broken"), 0o644)
	if _, err := c.read(path); err == nil {
		t.Fatal("a broken file read without error")
	}
}

// A node connection's goroutine can outlive the gateway's Close (the HTTP server does not wait for
// hijacked connections): once Close has returned, it writes nothing more to the accounts file (CI,
// 2026-11-25: a first connection written during a test's cleanup broke the removal of its directory).
func TestNoAccountsWriteAfterClose(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "accounts.json")
	g, err := New(Config{Accounts: &AccountsConfig{File: file, PublicURL: "https://site.test",
		SessionSecret: strings.Repeat("s", 32), GitHubClientID: "id", GitHubClientSecret: "secret"}}, quietLog())
	if err != nil {
		t.Fatal(err)
	}
	g.accounts.SignIn("gh:2", time.Now())
	g.accounts.SetStatus("gh:2", StatusActive)
	tok, id, _, _ := g.accounts.addNodeVia("gh:2", "", time.Now(), nil, false, "")
	anonTok, anonID, _ := g.accounts.RegisterAnon(time.Now())
	anonAcct, _, _ := g.accounts.nodeToken(anonTok)
	g.Close()
	before, _ := os.ReadFile(file)
	g.noteFirstConnect(&nodeConn{id: id, token: tok, account: "gh:2"})
	g.noteAnonSeen(&nodeConn{id: anonID, token: anonTok, account: anonAcct, anon: true})
	if after, _ := os.ReadFile(file); string(after) != string(before) {
		t.Fatal("the accounts file was written after Close")
	}
}
