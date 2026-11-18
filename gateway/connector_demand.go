// Pilots wanted (GET /v1/connectors/{id}/demand): how many pilots a connector should create now.
//
//	science  = Σ over projects whose agents may use pilots (agent_ok, not min_trust trusted):
//	           min(open tasks, runner workers active now)        the sessions runners can feed now
//	have     = sessions_per_slot × the GPU slots online that those projects may use, pilots of this
//	           connector left out (held and paused nodes too)
//	science pilots = ceil(max(0, science − have) / sessions_per_slot)    (a pilot is one slot)
//	chat pilots    = chat_headroom while pilots_chat_enabled, else 0
//	target   = min(science pilots + chat pilots, max_pilots, the spend cap if one is set)
//	outstanding = this connector's pilots that have not ended (created, bound, connected, disconnected)
//	pilots_wanted = min(max(0, target − outstanding), unsent_buffer − unsent, mint_per_hour left)
//	cancel_unsent = min(unsent, max(0, outstanding − target))
//
// "Unsent" are created pilots no machine has taken yet (not bound). The science number is the one
// runner autoscaling (BACKLOG 7v) starts from, seen from the other side: there, demand is open tasks
// bounded by GPU slots; here, pilots are bounded by what runners can feed. Neither grows the other
// from nothing: the runner pool's minimum size seeds both.
//
// SPDX-License-Identifier: Apache-2.0
package gateway

import (
	"net/http"
	"time"
)

// pilotDemand is the demand answer.
type pilotDemand struct {
	Connector    string         `json:"connector"`
	Wanted       int            `json:"pilots_wanted"`
	CancelUnsent int            `json:"cancel_unsent"`
	UnsentBuffer int            `json:"unsent_buffer"`
	Target       int            `json:"target"`
	CappedBy     string         `json:"capped_by,omitempty"` // "max_pilots", "spend" or "" (none)
	MaxPilots    int            `json:"max_pilots"`
	MintLeft     int            `json:"mint_left"` // pilots it may still create this hour
	Pilots       map[string]int `json:"pilots"`    // not ended, by state, and "outstanding"
	Science      demandScience  `json:"science"`
	Chat         demandChat     `json:"chat"`
	At           time.Time      `json:"at"`
}

type demandScience struct {
	OpenTasks       int `json:"open_tasks"`
	Workers         int `json:"runner_workers"`
	Sessions        int `json:"sessions"`     // Σ min(open tasks, workers)
	SlotsOnline     int `json:"slots_online"` // GPU slots science may use now, this connector's pilots left out
	SessionsPerSlot int `json:"sessions_per_slot"`
	Pilots          int `json:"pilots"`
}

type demandChat struct {
	Enabled  bool `json:"enabled"`
	Headroom int  `json:"headroom"`
	Pilots   int  `json:"pilots"`
}

// scienceDemand computes the science part of c's demand from the projects, their queues and the
// nodes connected now.
func (g *Gateway) scienceDemand(c *connector, now time.Time) demandScience {
	d := demandScience{SessionsPerSlot: c.SessionsPerSlot}
	var routes []*route
	trustedOnly := g.accounts != nil && g.accounts.Settings().Routing == RoutingTrustedOnly
	if pt := g.projects.Load(); pt != nil {
		for _, id := range pt.ids {
			p := pt.byID[id]
			if !p.AgentOK || p.MinTrust == "trusted" {
				continue // its agents never reach a pilot
			}
			open, workers := g.tasks.scienceLoad(p.ID)
			d.OpenTasks += open
			d.Workers += workers
			d.Sessions += min(open, workers)
			routes = append(routes, &route{tools: true, consent: true, class: ClassScience, trustedOnly: trustedOnly,
				pilotProbationOK: p.PilotProbationOK})
		}
	}
	g.mu.Lock()
	for _, n := range g.node {
		if n.held || (n.pilot != "" && n.connector == c.ID) {
			continue
		}
		n.mu.Lock()
		paused := n.paused
		n.mu.Unlock()
		if paused {
			continue
		}
		for _, rt := range routes {
			if rt.allows(n, now) {
				d.SlotsOnline += nodeSlots(n)
				break
			}
		}
	}
	g.mu.Unlock()
	if gap := d.Sessions - d.SlotsOnline*d.SessionsPerSlot; gap > 0 {
		d.Pilots = (gap + d.SessionsPerSlot - 1) / d.SessionsPerSlot
	}
	return d
}

// nodeSlots is how many requests a node runs at once: its models share its slots (pick), so the
// largest model's count.
func nodeSlots(n *nodeConn) int {
	s := 0
	for _, m := range n.models {
		s = max(s, m.Slots)
	}
	return s
}

// demand is c's pilots-wanted answer as of now.
func (g *Gateway) demand(c *connector, now time.Time) (*pilotDemand, error) {
	by, recent, err := g.sci.pilotCounts(c.ID, now)
	if err != nil {
		return nil, err
	}
	d := &pilotDemand{Connector: c.ID, MaxPilots: c.MaxPilots, UnsentBuffer: c.UnsentBuffer, Pilots: map[string]int{}, At: now.UTC()}
	outstanding := 0
	for _, st := range pilotStates[:len(pilotStates)-1] { // all but ended
		d.Pilots[st] = by[st]
		outstanding += by[st]
	}
	d.Pilots["outstanding"] = outstanding
	d.Science = g.scienceDemand(c, now)
	d.Chat = demandChat{Enabled: c.PilotsChatEnabled, Headroom: c.ChatHeadroom}
	if c.PilotsChatEnabled {
		d.Chat.Pilots = c.ChatHeadroom
	}
	d.Target = d.Science.Pilots + d.Chat.Pilots
	if d.Target > c.MaxPilots {
		d.Target, d.CappedBy = c.MaxPilots, "max_pilots"
	}
	if g.spendCap != nil {
		if limit, ok := g.spendCap(c.ID); ok && limit < d.Target {
			d.Target, d.CappedBy = max(limit, 0), "spend"
		}
	}
	unsent := by[PilotCreated]
	d.MintLeft = max(0, c.MintPerHour-recent)
	d.Wanted = min(max(0, d.Target-outstanding), max(0, c.UnsentBuffer-unsent), d.MintLeft)
	d.CancelUnsent = min(unsent, max(0, outstanding-d.Target))
	return d, nil
}

func (g *Gateway) handleConnectorDemand(w http.ResponseWriter, r *http.Request) {
	c := g.connectorFor(w, r)
	if c == nil {
		return
	}
	d, err := g.demand(c, g.pilotNow())
	if err != nil {
		taskError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, d)
}
