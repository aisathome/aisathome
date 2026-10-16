// Public credit statistics: the leaderboard (GET /v1/stats/credit, JSON for the website) and
// BOINC-format exports under /stats/ (tables.xml, user.gz, host.gz, team.gz; the format of
// BOINC's db_dump, research/boinc-stats/NOTES.md §1). Only accounts with a username appear,
// by that name and a numeric id; the rest count in totals only. No CPIDs yet: a CPID a user
// typed in proves nothing, and publishing one next to a verification path would make the
// project a CPID-theft vector (NOTES §3).
//
// SPDX-License-Identifier: Apache-2.0
package gateway

import (
	"bytes"
	"cmp"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"fmt"
	"math"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/aisathome/aisathome/protocol"
)

const (
	boardTop     = 100              // names on the leaderboard
	boardEvery   = time.Minute      // the leaderboard is recomputed at most this often
	exportsEvery = time.Hour        // the exports are regenerated at most this often
	exportsApp   = "chat-inference" // the one "application" in tables.xml
)

type statsCache struct {
	mu      sync.Mutex
	boardAt time.Time
	board   []byte
	filesAt time.Time
	files   map[string]exportFile
}

type exportFile struct {
	body  []byte
	etag  string
	ctype string
}

// grantCredit credits an answer by n.
func (g *Gateway) grantCredit(n *nodeConn, c float64, now time.Time) {
	var acctSince, nodeSince time.Time
	operator := n.operator
	if n.account != "" {
		operator = ""
		if g.accounts != nil {
			if a, _, ok := g.accounts.Get(n.account); ok {
				acctSince = a.Created
				for _, an := range a.Nodes {
					if an.ID == n.id {
						nodeSince = an.Created
					}
				}
			}
		}
	}
	g.credit.grant(n.id, n.account, operator, c, acctSince, nodeSince, now)
}

// meterRecord fills in the tokens credit is based on and the credit (relay.commit grants it). nodeU is what
// the node reported (clamped); it is used only when the gateway has no tokenizer.
func (g *Gateway) meterRecord(rec *usageRecord, mt *meter, n *nodeConn, who string, nodeU protocol.Usage, maxTokens int, now time.Time) {
	<-mt.ready
	gen, next := mt.output()
	ended := rec.Outcome == "ok"
	var t creditTokens
	if g.tok != nil {
		rec.Counted = "gateway"
		t.Prompt = len(mt.promptIDs)
		t.Completion = g.tok.Count(gen)
		if ended {
			t.Completion++ // <|im_end|>
		}
		t.Completion = min(t.Completion, maxTokens+1)
	} else {
		rec.Counted = "node"
		t.Prompt, t.Completion = nodeU.PromptTokens, nodeU.CompletionTokens
	}
	tail := ""
	if ended {
		tail = next
	}
	matched := g.pcache.servedHashed(n.id, keyHash(who), mt.hashes, mt.prompt, tail, now, rec.Outcome != outcomeBlocked)
	if g.tok != nil {
		b := 0
		for _, id := range mt.promptIDs {
			if b += g.tok.TokenBytes(id); b > matched {
				break
			}
			t.Cached++
		}
	} else if len(mt.prompt) > 0 {
		t.Cached = int(float64(t.Prompt) * float64(matched) / float64(len(mt.prompt)))
	}
	t.Cached = min(t.Cached, t.Prompt)
	rec.Tokens = &t
	if (rec.Outcome == "ok" || rec.Outcome == "cancelled") && !rec.SelfServed {
		rec.Credit = math.Round(t.credit()*1e6) / 1e6
	}
}

// ---------------------------------------------------------------- leaderboard

// handleCreditStats serves GET /v1/stats/credit without authentication.
func (g *Gateway) handleCreditStats(w http.ResponseWriter, _ *http.Request) {
	now := time.Now()
	g.statsOut.mu.Lock()
	if g.statsOut.board == nil || now.Sub(g.statsOut.boardAt) >= cmp.Or(g.cfg.StatsRefresh, boardEvery) {
		g.statsOut.board, g.statsOut.boardAt = mustJSON(g.leaderboard(now)), now
	}
	b := g.statsOut.board
	g.statsOut.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "public, max-age=60")
	w.Write(b)
}

type boardRow struct {
	Rank  int     `json:"rank"`
	Name  string  `json:"name"`
	Total float64 `json:"total_credit"`
	Today float64 `json:"today"`
	RAC   float64 `json:"recent_average"` // per day, BOINC's expavg_credit decayed to now
	Nodes int     `json:"nodes"`
}

func round1(x float64) float64 { return math.Round(x*10) / 10 }

func (g *Gateway) publicNames() map[string]publicAccount {
	if g.accounts == nil {
		return map[string]publicAccount{}
	}
	return g.accounts.publicNames()
}

func (g *Gateway) leaderboard(now time.Time) map[string]any {
	snap := g.credit.snapshot()
	names := g.publicNames()
	type sum struct {
		credit, today, rac float64
		people, nodes      int
	}
	var vol, anon, maint sum
	nodesOf := map[string]int{}
	for _, e := range snap.Nodes {
		if e.Total <= 0 {
			continue
		}
		if e.Account != "" {
			vol.nodes++
			nodesOf[e.Account]++
		} else {
			maint.nodes++
		}
	}
	var rows []boardRow
	for id, e := range snap.Accounts {
		if e.Total <= 0 {
			continue
		}
		vol.credit += e.Total
		vol.today += e.TodayAt(now)
		vol.rac += e.RAC(now)
		vol.people++
		pa, named := names[id]
		if !named {
			anon.credit += e.Total
			anon.today += e.TodayAt(now)
			anon.people++
			continue
		}
		rows = append(rows, boardRow{Name: pa.Name, Total: round1(e.Total), Today: round1(e.TodayAt(now)),
			RAC: round1(e.RAC(now)), Nodes: nodesOf[id]})
	}
	for _, e := range snap.Operators {
		maint.credit += e.Total
		maint.today += e.TodayAt(now)
		maint.rac += e.RAC(now)
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Total != rows[j].Total {
			return rows[i].Total > rows[j].Total
		}
		return strings.ToLower(rows[i].Name) < strings.ToLower(rows[j].Name)
	})
	if len(rows) > boardTop {
		rows = rows[:boardTop]
	}
	for i := range rows {
		rows[i].Rank = i + 1
	}
	if rows == nil {
		rows = []boardRow{}
	}
	online := g.NodeCount()
	return map[string]any{
		"updated": now.UTC().Format(time.RFC3339),
		"credit_per_token": map[string]any{"output": CreditPerOutputToken, "prompt": CreditPerPromptToken,
			"cached_prompt": CreditPerCachedToken},
		"nodes": map[string]any{"online": online, "with_credit": vol.nodes + maint.nodes},
		"total": map[string]any{"credit": round1(vol.credit + maint.credit), "today": round1(vol.today + maint.today),
			"recent_average": round1(vol.rac + maint.rac)},
		"volunteers": map[string]any{"count": vol.people, "nodes": vol.nodes, "credit": round1(vol.credit),
			"today": round1(vol.today), "recent_average": round1(vol.rac)},
		"anonymous":  map[string]any{"count": anon.people, "credit": round1(anon.credit), "today": round1(anon.today)},
		"maintainer": map[string]any{"nodes": maint.nodes, "credit": round1(maint.credit), "today": round1(maint.today)},
		"top":        rows,
	}
}

// ---------------------------------------------------------------- BOINC-format exports

// handleExport serves /stats/<file>, regenerated at most once an hour, with an ETag (which
// Gridcoin's scraper requires).
func (g *Gateway) handleExport(w http.ResponseWriter, r *http.Request) {
	now := time.Now()
	g.statsOut.mu.Lock()
	if g.statsOut.files == nil || now.Sub(g.statsOut.filesAt) >= cmp.Or(g.cfg.StatsRefresh, exportsEvery) {
		g.statsOut.files, g.statsOut.filesAt = g.exports(now), now
	}
	f, ok := g.statsOut.files[r.PathValue("file")]
	g.statsOut.mu.Unlock()
	if !ok {
		http.NotFound(w, r)
		return
	}
	h := w.Header()
	h.Set("Content-Type", f.ctype)
	h.Set("ETag", f.etag)
	h.Set("Cache-Control", "public, max-age=600")
	h.Set("X-Content-Type-Options", "nosniff")
	if match := r.Header.Get("If-None-Match"); match != "" && strings.Contains(match, f.etag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.Write(f.body)
}

// handleExportIndex serves /stats/.
func handleExportIndex(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	fmt.Fprint(w, "AIs@home credit in BOINC's db_dump format, regenerated at most hourly:\n"+
		"tables.xml user.gz host.gz team.gz\nOnly volunteers who set a username appear. No CPIDs yet.\n"+
		"How credit works: /node/credit/ on the project's website.\n")
}

func xmlEsc(s string) string {
	var b bytes.Buffer
	xml.EscapeText(&b, []byte(s))
	return b.String()
}

// exports builds the files from the ledger and the accounts' usernames.
func (g *Gateway) exports(now time.Time) map[string]exportFile {
	snap := g.credit.snapshot()
	names := g.publicNames()
	type user struct {
		e  *creditEntry
		pa publicAccount
	}
	var users []user
	userID := map[string]int64{}
	total := 0.0
	for id, e := range snap.Accounts {
		pa, ok := names[id]
		if !ok || e.Total <= 0 {
			continue
		}
		users = append(users, user{e, pa})
		userID[id] = e.ID
		total += e.Total
	}
	sort.Slice(users, func(i, j int) bool { return users[i].e.ID < users[j].e.ID })
	var u bytes.Buffer
	u.WriteString("<?xml version=\"1.0\" encoding=\"utf-8\"?>\n<users>\n")
	for _, x := range users {
		fmt.Fprintf(&u, "<user>\n <id>%d</id>\n <name>%s</name>\n <create_time>%d</create_time>\n"+
			" <total_credit>%f</total_credit>\n <expavg_credit>%f</expavg_credit>\n <expavg_time>%f</expavg_time>\n</user>\n",
			x.e.ID, xmlEsc(x.pa.Name), x.pa.Created, x.e.Total, x.e.Avg, x.e.AvgTime)
	}
	u.WriteString("</users>\n")

	type host struct {
		id string
		e  *creditEntry
	}
	var hosts []host
	for id, e := range snap.Nodes {
		if _, ok := userID[e.Account]; ok && e.Account != "" && e.Total > 0 {
			hosts = append(hosts, host{id, e})
		}
	}
	sort.Slice(hosts, func(i, j int) bool { return hosts[i].e.ID < hosts[j].e.ID })
	var h bytes.Buffer
	h.WriteString("<?xml version=\"1.0\" encoding=\"utf-8\"?>\n<hosts>\n")
	for _, x := range hosts {
		created, ok := names[x.e.Account].Nodes[x.id]
		if !ok {
			created = x.e.First.Unix()
		}
		fmt.Fprintf(&h, "<host>\n    <id>%d</id>\n    <userid>%d</userid>\n    <total_credit>%f</total_credit>\n"+
			"    <expavg_credit>%f</expavg_credit>\n    <expavg_time>%f</expavg_time>\n    <p_vendor></p_vendor>\n"+
			"    <p_model></p_model>\n    <os_name></os_name>\n    <os_version></os_version>\n    <misc><![CDATA[]]></misc>\n"+
			"  <create_time>%d</create_time>\n  <rpc_time>%d</rpc_time>\n</host>\n",
			x.e.ID, userID[x.e.Account], x.e.Total, x.e.Avg, x.e.AvgTime, created, x.e.Last.Unix())
	}
	h.WriteString("</hosts>\n")

	team := []byte("<?xml version=\"1.0\" encoding=\"utf-8\"?>\n<teams>\n</teams>\n")
	var t bytes.Buffer
	t.WriteString("<?xml version=\"1.0\" encoding=\"utf-8\"?>\n<tables>\n")
	fmt.Fprintf(&t, "    <update_time>%d</update_time>\n", now.Unix())
	if len(users) > 0 {
		fmt.Fprintf(&t, "    <nusers_total>%d</nusers_total>\n", len(users))
	}
	if len(hosts) > 0 {
		fmt.Fprintf(&t, "    <nhosts_total>%d</nhosts_total>\n", len(hosts))
	}
	if total > 0 {
		fmt.Fprintf(&t, "    <total_credit>%f</total_credit>\n", total)
	}
	fmt.Fprintf(&t, "    <applications>\n        <application><name>%s</name><results_unsent>0</results_unsent>"+
		"<results_in_progress>0</results_in_progress><results_over>0</results_over></application>\n    </applications>\n", exportsApp)
	t.WriteString("</tables>\n")

	files := map[string]exportFile{
		"user.gz":    gzFile(u.Bytes()),
		"host.gz":    gzFile(h.Bytes()),
		"team.gz":    gzFile(team),
		"tables.xml": newExportFile(t.Bytes(), "text/xml; charset=utf-8"),
	}
	return files
}

func newExportFile(b []byte, ctype string) exportFile {
	sum := sha256.Sum256(b)
	return exportFile{body: b, etag: `"` + hex.EncodeToString(sum[:16]) + `"`, ctype: ctype}
}

// gzFile compresses b without a name or time in the header, so the same content always gives
// the same bytes and ETag.
func gzFile(b []byte) exportFile {
	var buf bytes.Buffer
	z, _ := gzip.NewWriterLevel(&buf, gzip.BestCompression)
	z.Write(b)
	z.Close()
	return newExportFile(buf.Bytes(), "application/gzip")
}
