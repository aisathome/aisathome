// SPDX-License-Identifier: Apache-2.0
package main

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/url"
	"path"
	"regexp"
	"strings"
	"time"
)

// Stats is the output file: daily aggregates only.
type Stats struct {
	Host    string          `json:"host"`
	Updated time.Time       `json:"updated"`
	Days    map[string]*Day `json:"days"` // UTC day, "2006-01-02"
}

// Day holds one UTC day's counts. See doc.go for what each counts.
type Day struct {
	PageViews int            `json:"page_views"`
	Visitors  int            `json:"visitors"`
	Pages     map[string]int `json:"pages,omitempty"`
	Referrers map[string]int `json:"referrers,omitempty"`
	Feed      int            `json:"feed,omitempty"`
	NotFound  int            `json:"not_found,omitempty"`
	Bots      map[string]int `json:"bots,omitempty"`
	Tests     int            `json:"tests,omitempty"`
}

// State lives on the tmpfs only: where the log was read up to, and the current day's salt
// and visitor hashes (deleted when the UTC day ends).
type State struct {
	Inode  uint64   `json:"inode"`
	Offset int64    `json:"offset"`
	Day    string   `json:"day,omitempty"`
	Salt   []byte   `json:"salt,omitempty"`
	Seen   []string `json:"seen,omitempty"`
}

// Caps per day, so odd traffic cannot grow the file: the rest is counted under "(other)".
const (
	maxPaths     = 500
	maxReferrers = 300
	other        = "(other)"
	feedPath     = "/devlog/rss.xml"
)

// Paths on the website host that are not the static site (served by the gateway, or the
// staging password gate), and asset directories that are not pages.
var notSite = []string{"/v1/", "/account", "/stats/", "/__login", "/_astro/", "/pagefind/"}

// Our own automated tests mark their user agent; they are only counted.
var testMarkers = []string{"aisathome-test", "aisathome-smoke"}

// Known bots by name: the first match names the bot (case-insensitive substring).
var botNames = []struct{ name, match string }{
	{"Googlebot", "googlebot"}, {"Google (other)", "google"}, {"bingbot", "bingbot"},
	{"Applebot", "applebot"}, {"DuckDuckBot", "duckduckbot"}, {"YandexBot", "yandex"},
	{"Baiduspider", "baiduspider"}, {"GPTBot", "gptbot"}, {"OpenAI (other)", "openai"},
	{"ClaudeBot", "claudebot"}, {"Anthropic (other)", "anthropic"}, {"PerplexityBot", "perplexity"},
	{"CCBot", "ccbot"}, {"AhrefsBot", "ahrefs"}, {"SemrushBot", "semrush"}, {"PetalBot", "petalbot"},
	{"MJ12bot", "mj12bot"}, {"DotBot", "dotbot"}, {"Bytespider", "bytespider"},
	{"facebookexternalhit", "facebookexternalhit"}, {"Twitterbot", "twitterbot"},
	{"Discordbot", "discordbot"}, {"Slackbot", "slackbot"}, {"TelegramBot", "telegrambot"},
	{"redditbot", "redditbot"}, {"HeadlessChrome", "headlesschrome"}, {"curl", "curl/"},
	{"Wget", "wget"}, {"python", "python"}, {"Go-http-client", "go-http-client"},
}

// Anything else that says it is automated.
var botGeneric = regexp.MustCompile(`(?i)bot\b|bot/|crawl|spider|slurp|scan|fetch|monitor|preview|` +
	`http-?client|java/|okhttp|libwww|axios|node-fetch|scrapy|headless|lighthouse|uptime|feed|rss|` +
	`zgrab|masscan|nuclei|httpx|wordpress`)

// Caddy's JSON access log line (the fields used here).
type logLine struct {
	TS      float64 `json:"ts"`
	Status  int     `json:"status"`
	Request struct {
		ClientIP string              `json:"client_ip"`
		RemoteIP string              `json:"remote_ip"`
		Method   string              `json:"method"`
		Host     string              `json:"host"`
		URI      string              `json:"uri"`
		Headers  map[string][]string `json:"headers"`
	} `json:"request"`
}

func (l *logLine) header(name string) string {
	if v := l.Request.Headers[name]; len(v) > 0 {
		return v[0]
	}
	return ""
}

// Aggregator adds log lines to the daily counts.
type Aggregator struct {
	Host  string // the website's host name; lines for other hosts are ignored
	Stats *Stats
	State *State
	Rand  io.Reader // the salt's source (crypto/rand)
	seen  map[string]bool
}

// NewAggregator continues from the saved stats and state.
func NewAggregator(host string, stats *Stats, state *State) *Aggregator {
	if stats.Days == nil {
		stats.Days = map[string]*Day{}
	}
	stats.Host = host
	a := &Aggregator{Host: strings.ToLower(host), Stats: stats, State: state, Rand: rand.Reader, seen: map[string]bool{}}
	for _, h := range state.Seen {
		a.seen[h] = true
	}
	return a
}

func (a *Aggregator) day(d string) *Day {
	if a.Stats.Days[d] == nil {
		a.Stats.Days[d] = &Day{}
	}
	return a.Stats.Days[d]
}

// Add counts one access-log line; lines that are not JSON or not for the site are skipped.
func (a *Aggregator) Add(raw []byte) {
	var l logLine
	if json.Unmarshal(raw, &l) != nil || l.TS <= 0 {
		return
	}
	if host := strings.ToLower(stripPort(l.Request.Host)); host != a.Host {
		return
	}
	p, ok := normalizePath(l.Request.URI)
	if !ok || excluded(p) {
		return
	}
	day := time.Unix(int64(l.TS), 0).UTC().Format(time.DateOnly)
	d := a.day(day)
	ua := l.header("User-Agent")
	switch kind, name := classify(ua); kind {
	case kindTest:
		d.Tests++
		return
	case kindBot:
		if d.Bots == nil {
			d.Bots = map[string]int{}
		}
		d.Bots[name]++
		return
	}
	if l.Status == 404 {
		d.NotFound++
		return
	}
	if l.Request.Method != "GET" || (l.Status != 200 && l.Status != 304) {
		return
	}
	if p == feedPath {
		d.Feed++
		return
	}
	if !isPage(p) {
		return
	}
	d.PageViews++
	d.Pages = bump(d.Pages, p, maxPaths)
	if ref := referrerDomain(l.header("Referer"), a.Host); ref != "" {
		d.Referrers = bump(d.Referrers, ref, maxReferrers)
	}
	ip := l.Request.ClientIP
	if ip == "" {
		ip = l.Request.RemoteIP
	}
	if a.useDay(day) {
		h := visitorHash(a.State.Salt, ip, ua)
		if !a.seen[h] {
			a.seen[h] = true
			d.Visitors++
		}
	}
}

// useDay makes sure the salt is the one for day; false for an earlier day, whose salt is
// already gone (its page views still count, its visitors cannot).
func (a *Aggregator) useDay(day string) bool {
	if day == a.State.Day && len(a.State.Salt) > 0 {
		return true
	}
	if day < a.State.Day {
		return false
	}
	a.newDay(day)
	return true
}

func (a *Aggregator) newDay(day string) {
	salt := make([]byte, 32)
	if _, err := io.ReadFull(a.Rand, salt); err != nil {
		panic("no randomness for the salt: " + err.Error())
	}
	a.State.Day, a.State.Salt, a.seen = day, salt, map[string]bool{}
}

// Finish ends a run: when the UTC day has changed, the old salt and hashes are destroyed even
// without new traffic. It also copies the hashes back into the state for the next run.
func (a *Aggregator) Finish(now time.Time) {
	if today := now.UTC().Format(time.DateOnly); a.State.Day < today {
		a.newDay(today)
	}
	a.State.Seen = a.State.Seen[:0]
	for h := range a.seen {
		a.State.Seen = append(a.State.Seen, h)
	}
	a.Stats.Updated = now.UTC().Truncate(time.Second)
}

// visitorHash is SHA-256(salt + IP + user agent), truncated to 16 bytes for the day's set.
func visitorHash(salt []byte, ip, ua string) string {
	h := sha256.New()
	h.Write(salt)
	h.Write([]byte(ip))
	h.Write([]byte{0})
	h.Write([]byte(ua))
	return hex.EncodeToString(h.Sum(nil)[:16])
}

const (
	kindHuman = iota
	kindBot
	kindTest
)

func classify(ua string) (kind int, name string) {
	low := strings.ToLower(ua)
	for _, m := range testMarkers {
		if strings.Contains(low, m) {
			return kindTest, ""
		}
	}
	if strings.TrimSpace(ua) == "" {
		return kindBot, "no user agent"
	}
	for _, b := range botNames {
		if strings.Contains(low, b.match) {
			return kindBot, b.name
		}
	}
	if botGeneric.MatchString(ua) || !strings.HasPrefix(ua, "Mozilla/") {
		return kindBot, "other"
	}
	return kindHuman, ""
}

// normalizePath reduces a request URI to its path: no query or fragment, decoded, cleaned,
// "index.html" folded into its directory. False for anything odd.
func normalizePath(uri string) (string, bool) {
	if i := strings.IndexAny(uri, "?#"); i >= 0 {
		uri = uri[:i]
	}
	p, err := url.PathUnescape(uri)
	if err != nil || !strings.HasPrefix(p, "/") || len(p) > 256 {
		return "", false
	}
	for _, r := range p {
		if r < 0x20 || r == 0x7f {
			return "", false
		}
	}
	dir := strings.HasSuffix(p, "/")
	p = path.Clean(p)
	if dir && p != "/" {
		p += "/"
	}
	if strings.HasSuffix(p, "/index.html") {
		p = strings.TrimSuffix(p, "index.html")
	}
	return p, true
}

func excluded(p string) bool {
	for _, x := range notSite {
		if strings.HasPrefix(p, x) {
			return true
		}
	}
	return false
}

func isPage(p string) bool { return strings.HasSuffix(p, "/") || strings.HasSuffix(p, ".html") }

var domainRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)+$`)

// referrerDomain reduces a Referer header to the referring site's domain ("www." dropped);
// empty for none, our own domains, or anything that is not a domain name.
func referrerDomain(ref, own string) string {
	if ref == "" {
		return ""
	}
	u, err := url.Parse(ref)
	if err != nil {
		return ""
	}
	host := strings.TrimSuffix(strings.ToLower(u.Hostname()), ".")
	host = strings.TrimPrefix(host, "www.")
	tld := host[strings.LastIndexByte(host, '.')+1:]
	if len(host) > 253 || !domainRe.MatchString(host) || strings.Trim(tld, "0123456789") == "" { // an IPv4 address

		return ""
	}
	if host == own || strings.HasSuffix(host, "."+own) || strings.HasSuffix(own, "."+host) {
		return ""
	}
	return host
}

func stripPort(h string) string {
	if i := strings.LastIndexByte(h, ':'); i >= 0 && !strings.Contains(h[i:], "]") {
		return h[:i]
	}
	return h
}

func bump(m map[string]int, k string, limit int) map[string]int {
	if m == nil {
		m = map[string]int{}
	}
	if _, ok := m[k]; !ok && len(m) >= limit {
		k = other
	}
	m[k]++
	return m
}
