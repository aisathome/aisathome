// The funnel card on /account/ops (admins only): from a visit to the site to a node connecting, per
// UTC day, as totals only. Two sources:
//
//   - The site statistics file (site_stats_file) that tools/sitestats writes on the same host:
//     visitors, views of the node pages and fetches of the install script (/node.sh). Read-only,
//     reread when it changes; the gateway never writes it.
//   - The gateway's own daily counters (Funnel, in the accounts file next to the nodes they count):
//     nodes registered without an account, nodes added to an account, link approvals, nodes without
//     an account linked to one, and nodes that connected for the first time. Counted in the same
//     write that records the event, so a count never disagrees with the record; numbers only, no
//     node or account. The project's own nodes (nodes of the admin accounts) are counted apart too.
//
// A node record keeps one flag, Connected (it has connected at least once), so that a reconnect is
// not a first connection. First connections count only for nodes created after the counters
// started (Funnel.Since): an older node's first connection after the upgrade is not a new one.
//
// SPDX-License-Identifier: Apache-2.0
package gateway

import (
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"time"
)

// funnelKeepDays bounds the gateway's daily counters: older days are dropped.
const funnelKeepDays = 400

// funnelDays: the card shows today and this many UTC days before it.
const funnelDays = 7

// funnelNodePages are the site's node pages whose views the card adds up.
var funnelNodePages = []string{"/node/", "/node/windows/", "/node/install/", "/node/binaries/"}

// releasesURL is where the Windows installer and the Linux bundle are downloaded (GitHub counts
// those; the gateway does not fetch the counts).
const releasesURL = "https://github.com/aisathome/aisathome/releases"

// Funnel holds the gateway's daily counters (AccountsFile.Funnel).
type Funnel struct {
	Since time.Time             `json:"since"` // when counting started
	Days  map[string]*FunnelDay `json:"days,omitempty"`
}

// FunnelDay is one UTC day's counts. Own* are the part of the count before them that the admin
// accounts' nodes make (the project's own).
type FunnelDay struct {
	Anon         int `json:"anon_registered,omitempty"` // nodes registered without an account
	AccountNodes int `json:"account_nodes,omitempty"`   // nodes added to an account (account page or link)
	OwnNodes     int `json:"own_account_nodes,omitempty"`
	Links        int `json:"link_approvals,omitempty"` // of AccountNodes, by an approved link
	OwnLinks     int `json:"own_link_approvals,omitempty"`
	AnonLinked   int `json:"anon_linked,omitempty"`    // nodes without an account linked to an account
	FirstConnect int `json:"first_connects,omitempty"` // distinct nodes connected for the first time
	OwnFirst     int `json:"own_first_connects,omitempty"`
}

// funnelDay returns now's UTC day of the counters, starting the counters if needed, and drops days
// older than funnelKeepDays.
func (f *AccountsFile) funnelDay(now time.Time) *FunnelDay {
	if f.Funnel == nil {
		f.Funnel = &Funnel{Since: now.UTC()}
	}
	if f.Funnel.Days == nil {
		f.Funnel.Days = map[string]*FunnelDay{}
	}
	key := now.UTC().Format(time.DateOnly)
	d := f.Funnel.Days[key]
	if d == nil {
		d = &FunnelDay{}
		f.Funnel.Days[key] = d
		oldest := now.UTC().AddDate(0, 0, -funnelKeepDays).Format(time.DateOnly)
		for k := range f.Funnel.Days {
			if k < oldest {
				delete(f.Funnel.Days, k)
			}
		}
	}
	return d
}

// startFunnel starts the counters if the file has none (OpenAccountStore).
func (f *AccountsFile) startFunnel(now time.Time) {
	if f.Funnel == nil {
		f.Funnel = &Funnel{Since: now.UTC()}
	}
}

// isOwn reports whether account is one of the project's own (an admin account).
func (s *AccountStore) isOwn(account string) bool {
	return s.own != nil && s.own(account)
}

// SetOwn tells the store which accounts are the project's own (the admins), for the funnel's own
// counts. Call it before the store is in use.
func (s *AccountStore) SetOwn(own func(account string) bool) { s.own = own }

// firstConnect records that the node with tokenHash connected, once: the first time it counts as a
// first connection of the day (if the node was created after the counters started). A node that has
// connected before costs no write.
func (s *AccountStore) firstConnect(tokenHash string, now time.Time) error {
	s.mu.Lock()
	r, ok := s.byNode[tokenHash]
	done := !ok || r.node.Connected
	s.mu.Unlock()
	if done {
		return nil
	}
	return s.Update(func(f *AccountsFile) error {
		for _, a := range f.Accounts {
			for i := range a.Nodes {
				n := &a.Nodes[i]
				if n.Hash != tokenHash || n.Connected {
					continue
				}
				n.Connected = true
				f.startFunnel(now)
				if !n.Created.Before(f.Funnel.Since) {
					d := f.funnelDay(now)
					d.FirstConnect++
					if s.isOwn(a.ID) {
						d.OwnFirst++
					}
				}
				return nil
			}
		}
		return nil
	})
}

// linkAnon deletes the identity of the node without an account claim, now linked to an account as
// its node nodeID, and counts the link. The new node keeps the old one's Connected: the same machine
// connecting under its new id is not a first connection.
func (s *AccountStore) linkAnon(claim, nodeID string, now time.Time) error {
	if !isAnon(claim) {
		return fmt.Errorf("not a node without an account")
	}
	return s.Update(func(f *AccountsFile) error {
		connected, found := false, false
		for i, a := range f.Accounts {
			if a.ID == claim {
				for _, n := range a.Nodes {
					connected = connected || n.Connected || n.LastSeen != nil
				}
				f.Accounts = append(f.Accounts[:i], f.Accounts[i+1:]...)
				found = true
				break
			}
		}
		if !found {
			return nil // already gone (the retention sweep, or a second poll): counted once
		}
		if connected {
			if r, ok := f.nodeByID(nodeID); ok {
				r.node.Connected = true
			}
		}
		f.funnelDay(now).AnonLinked++
		return nil
	})
}

// nodeByID finds the account node with id in the file.
func (f *AccountsFile) nodeByID(id string) (nodeRef, bool) {
	for _, a := range f.Accounts {
		for i := range a.Nodes {
			if a.Nodes[i].ID == id {
				return nodeRef{a, &a.Nodes[i]}, true
			}
		}
	}
	return nodeRef{}, false
}

// funnel returns a copy of the counters (nil if they have not started).
func (s *AccountStore) funnel() *Funnel {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.f.Funnel == nil {
		return nil
	}
	c := &Funnel{Since: s.f.Funnel.Since, Days: make(map[string]*FunnelDay, len(s.f.Funnel.Days))}
	for k, d := range s.f.Funnel.Days {
		dd := *d
		c.Days[k] = &dd
	}
	return c
}

// ---------------------------------------------------------------- the site statistics file

// siteStats is the part of tools/sitestats' output the card reads (its doc.go says what each counts).
type siteStats struct {
	Updated time.Time                `json:"updated"`
	Days    map[string]*siteStatsDay `json:"days"`
}

type siteStatsDay struct {
	Visitors       int            `json:"visitors"`
	Pages          map[string]int `json:"pages"`
	Installs       int            `json:"installs"`
	InstallFetches map[string]int `json:"install_fetches"`
}

// siteStatsCache rereads the file only when its size or time changed.
type siteStatsCache struct {
	mu   sync.Mutex
	mod  time.Time
	size int64
	s    *siteStats
}

// read returns the file's counts, or why there are none.
func (c *siteStatsCache) read(path string) (*siteStats, error) {
	if path == "" {
		return nil, fmt.Errorf("no site_stats_file in the gateway config")
	}
	fi, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.s != nil && fi.ModTime().Equal(c.mod) && fi.Size() == c.size {
		return c.s, nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var s siteStats
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	c.s, c.mod, c.size = &s, fi.ModTime(), fi.Size()
	return c.s, nil
}

// ---------------------------------------------------------------- the card

// opsFunnel is the funnel card.
type opsFunnel struct {
	Rows     []opsFunnelRow // today first
	Total    opsFunnelRow
	Summary  string
	Site     bool   // the site statistics were read
	SiteNote string // why not, or when they were updated
	Since    string // the gateway's counters started
	Releases string
}

// opsFunnelRow is one day (or the total): site counts, then the gateway's.
type opsFunnelRow struct {
	Day                           string
	Visitors, NodeViews, Installs int
	Fetches                       string // curl and wget fetches of the install script, of all fetches
	HasGateway                    bool   // the gateway counted that day (on or after Since)
	Anon, AccountNodes, OwnNodes  int
	Links, OwnLinks, AnonLinked   int
	FirstConnect, OwnFirst        int
	cli, fetc                     int // for Fetches
}

func (r *opsFunnelRow) add(o opsFunnelRow) {
	r.Visitors += o.Visitors
	r.NodeViews += o.NodeViews
	r.Installs += o.Installs
	r.cli += o.cli
	r.fetc += o.fetc
	r.Anon += o.Anon
	r.AccountNodes += o.AccountNodes
	r.OwnNodes += o.OwnNodes
	r.Links += o.Links
	r.OwnLinks += o.OwnLinks
	r.AnonLinked += o.AnonLinked
	r.FirstConnect += o.FirstConnect
	r.OwnFirst += o.OwnFirst
	r.HasGateway = r.HasGateway || o.HasGateway
}

func fetches(cli, all int) string {
	if all == 0 {
		return "0"
	}
	return fmt.Sprintf("%d of %d", cli, all)
}

// rate is n of all as a percentage, or "–".
func rate(n, all int) string {
	if all <= 0 {
		return "–"
	}
	return fmt.Sprintf("%.1f%%", 100*float64(n)/float64(all))
}

// opsFunnel builds the card for today and the funnelDays UTC days before it.
func (g *Gateway) opsFunnel(now time.Time) *opsFunnel {
	c := &opsFunnel{Releases: releasesURL}
	site, err := g.sitestat.read(g.cfg.SiteStatsFile)
	if err != nil {
		c.SiteNote = "Site statistics unavailable: " + err.Error() + "."
	} else {
		c.Site = true
		c.SiteNote = "Site statistics updated " + site.Updated.UTC().Format("2006-01-02 15:04") + " UTC."
	}
	var fun *Funnel
	if g.accounts != nil {
		fun = g.accounts.funnel()
	}
	sinceDay := ""
	if fun != nil {
		c.Since = fun.Since.UTC().Format("2006-01-02 15:04") + " UTC"
		sinceDay = fun.Since.UTC().Format(time.DateOnly)
	}
	for i := 0; i <= funnelDays; i++ {
		key := now.UTC().AddDate(0, 0, -i).Format(time.DateOnly)
		row := opsFunnelRow{Day: key}
		if site != nil {
			if d := site.Days[key]; d != nil {
				row.Visitors, row.Installs = d.Visitors, d.Installs
				for _, p := range funnelNodePages {
					row.NodeViews += d.Pages[p]
				}
				for k, v := range d.InstallFetches {
					row.fetc += v
					if k == "curl" || k == "Wget" {
						row.cli += v
					}
				}
			}
		}
		row.Fetches = fetches(row.cli, row.fetc)
		if fun != nil && key >= sinceDay {
			row.HasGateway = true
			if d := fun.Days[key]; d != nil {
				row.Anon, row.AccountNodes, row.OwnNodes = d.Anon, d.AccountNodes, d.OwnNodes
				row.Links, row.OwnLinks, row.AnonLinked = d.Links, d.OwnLinks, d.AnonLinked
				row.FirstConnect, row.OwnFirst = d.FirstConnect, d.OwnFirst
			}
		}
		c.Rows = append(c.Rows, row)
		c.Total.add(row)
	}
	c.Total.Day = "total"
	c.Total.Fetches = fetches(c.Total.cli, c.Total.fetc)
	c.Summary = funnelSummary(c.Total, c.Site)
	return c
}

// funnelSummary is the one-line conversion over the card's days, without the project's own nodes.
func funnelSummary(t opsFunnelRow, site bool) string {
	added, first := t.AccountNodes-t.OwnNodes, t.FirstConnect-t.OwnFirst
	gw := fmt.Sprintf("%d registered without an account, %d added to an account → %d connected for the first time", t.Anon, added, first)
	if !t.HasGateway {
		gw = "no gateway counts yet"
	}
	if !site {
		return fmt.Sprintf("Last %d days and today, not counting our own nodes: %s.", funnelDays, gw)
	}
	return fmt.Sprintf("Last %d days and today, not counting our own nodes: %d visitors → %d node page views (%s) → %d install starts (%s of visitors) → %s (%s of install starts).",
		funnelDays, t.Visitors, t.NodeViews, rate(t.NodeViews, t.Visitors), t.Installs, rate(t.Installs, t.Visitors), gw, rate(first, t.Installs))
}
