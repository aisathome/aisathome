// Pilots wanted (GET /v1/connectors/{id}/demand): how many pilots a connector should create now.
//
//	science  = Σ over projects whose agents may use pilots (agent_ok):
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
// runner autoscaling (planned) starts from, seen from the other side: there, demand is open tasks
// bounded by GPU slots; here, pilots are bounded by what runners can feed. Neither grows the other
// from nothing: the runner pool's minimum size seeds both.
//
// A project's min_node_ctx (project.go) narrows "have" to the slots of nodes with that much context.
//
// Pilot tiers (a connector's pilot_tiers, connector.go): the same numbers, per kind of pilot, so the
// connector creates pilots that can serve what is waiting (a long session needs a large context):
//
//   - each project's sessions take the online slots first: projects with the highest floor
//     (min_node_ctx) first, each on the slots with the least context that clears its floor;
//   - what is left of a project's sessions needs pilots of the smallest tier whose ctx clears its
//     floor (none clears it: no pilot asked for); per tier, ceil(sessions left / sessions_per_slot);
//     chat headroom goes to the smallest tier;
//   - max_pilots and the spend cap cut the total target as above, taken from the smallest tiers first:
//     what only a larger tier can do keeps its pilots;
//   - each tier's outstanding pilots cover its own target; a larger tier's surplus covers a smaller
//     tier's need, never the reverse; pilots_wanted per tier is the need left;
//   - the totals' caps (unsent_buffer − unsent, mint_per_hour left, and max_pilots − outstanding) are
//     shared out from the largest tier down; cancel_unsent per tier is its unsent pilots that no
//     tier's target uses.
//
// A pilot created without a tier (an older connector), or with a tier the config no longer has,
// counts as the largest tier. The answer's top-level numbers are the sums; "tiers" has each tier's.
// Without pilot_tiers there is one kind of pilot and the answer has no "tiers".
//
// SPDX-License-Identifier: Apache-2.0
package gateway

import (
	"net/http"
	"slices"
	"time"
)

// pilotDemand is the demand answer.
type pilotDemand struct {
	Connector    string                 `json:"connector"`
	Wanted       int                    `json:"pilots_wanted"`
	CancelUnsent int                    `json:"cancel_unsent"`
	UnsentBuffer int                    `json:"unsent_buffer"`
	Target       int                    `json:"target"`
	CappedBy     string                 `json:"capped_by,omitempty"` // "max_pilots", "spend" or "" (none)
	MaxPilots    int                    `json:"max_pilots"`
	MintLeft     int                    `json:"mint_left"` // pilots it may still create this hour
	Pilots       map[string]int         `json:"pilots"`    // not ended, by state, and "outstanding"
	Science      demandScience          `json:"science"`
	Chat         demandChat             `json:"chat"`
	Tiers        map[string]*demandTier `json:"tiers,omitempty"` // with pilot_tiers: per tier
	At           time.Time              `json:"at"`
}

// demandTier is one pilot tier's part of the demand.
type demandTier struct {
	Ctx          int `json:"ctx"`
	Target       int `json:"target"`
	Wanted       int `json:"pilots_wanted"`
	CancelUnsent int `json:"cancel_unsent"`
	Outstanding  int `json:"outstanding"`
	Unsent       int `json:"unsent"`
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

// nodeCtx is the most context any of n's models announces; 0 when one announces none (assumed to fit
// anything, as in fits). Caller holds g.mu.
func nodeCtx(n *nodeConn) int {
	c := 0
	for _, m := range n.models {
		if m.Ctx == 0 {
			return 0
		}
		c = max(c, m.Ctx)
	}
	return c
}

// ctxClears reports whether context ctx (0: unknown, assumed to fit) clears floor.
func ctxClears(ctx, floor int) bool { return floor <= 0 || ctx == 0 || ctx >= floor }

// scienceDemand computes the science part of c's demand from the projects, their queues and the
// nodes connected now, with the science pilots per tier of c (one entry without tiers).
func (g *Gateway) scienceDemand(c *connector, now time.Time) (demandScience, []int) {
	d := demandScience{SessionsPerSlot: c.SessionsPerSlot}
	type proj struct {
		rt       *route
		sessions int
	}
	var projs []proj
	ownOnly := g.accounts != nil && g.accounts.Settings().OwnOnly()
	if pt := g.projects.Load(); pt != nil {
		for _, id := range pt.ids {
			p := pt.byID[id]
			if !p.AgentOK {
				continue // its agents never reach a pilot
			}
			open, workers := g.tasks.scienceLoad(p.ID)
			d.OpenTasks += open
			d.Workers += workers
			d.Sessions += min(open, workers)
			projs = append(projs, proj{scienceRoute(p, ownOnly), min(open, workers)})
		}
	}
	// The slots online, each node once: one that some project's agents may use, with the context that
	// project asks of a node. cap is in sessions.
	type slot struct{ ctx, cap int }
	var slots []slot
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
		for _, p := range projs {
			if p.rt.allows(n, now) && p.rt.nodeClearsFloor(n) {
				d.SlotsOnline += nodeSlots(n)
				slots = append(slots, slot{nodeCtx(n), nodeSlots(n) * d.SessionsPerSlot})
				break
			}
		}
	}
	g.mu.Unlock()
	// Least context first; a node that announces none last (it may be anything).
	eff := func(s slot) int {
		if s.ctx == 0 {
			return int(^uint(0) >> 1)
		}
		return s.ctx
	}
	slices.SortStableFunc(slots, func(a, b slot) int { return eff(a) - eff(b) })
	// Highest floor first: those sessions can use the fewest slots.
	slices.SortStableFunc(projs, func(a, b proj) int { return b.rt.minCtx - a.rt.minCtx })
	nt := max(len(c.tiers), 1)
	gap := make([]int, nt)
	probe := c.pilotProbe(now)
	for _, p := range projs {
		need := p.sessions
		for i := range slots {
			if need == 0 {
				break
			}
			if ctxClears(slots[i].ctx, p.rt.minCtx) {
				k := min(need, slots[i].cap)
				slots[i].cap -= k
				need -= k
			}
		}
		// Not under the emergency switch: a pilot is nobody's own node.
		if need == 0 || !p.rt.allows(probe, now) {
			continue
		}
		t := 0
		if len(c.tiers) > 0 {
			t = slices.IndexFunc(c.tiers, func(pt PilotTier) bool { return pt.Ctx >= p.rt.minCtx })
			if t < 0 {
				continue // no tier has the context this project asks of a node
			}
		}
		gap[t] += need
	}
	pilots := make([]int, nt)
	for i, s := range gap {
		pilots[i] = (s + d.SessionsPerSlot - 1) / d.SessionsPerSlot
		d.Pilots += pilots[i]
	}
	return d, pilots
}

// tierIndex is the index in c.tiers of a pilot's tier: 0 without tiers; the largest for "" (created
// without one) or a tier the config no longer has.
func (c *connector) tierIndex(name string) int {
	if i := slices.IndexFunc(c.tiers, func(t PilotTier) bool { return t.Name == name }); i >= 0 {
		return i
	}
	return max(len(c.tiers)-1, 0)
}

// demand is c's pilots-wanted answer as of now.
func (g *Gateway) demand(c *connector, now time.Time) (*pilotDemand, error) {
	byTier, recent, err := g.sci.pilotCounts(c.ID, now)
	if err != nil {
		return nil, err
	}
	nt := max(len(c.tiers), 1)
	d := &pilotDemand{Connector: c.ID, MaxPilots: c.MaxPilots, UnsentBuffer: c.UnsentBuffer, Pilots: map[string]int{}, At: now.UTC()}
	out, unsent := make([]int, nt), make([]int, nt) // per tier
	outstanding, unsentAll := 0, 0
	for _, st := range pilotStates[:len(pilotStates)-1] { // all but ended
		d.Pilots[st] = 0
	}
	for name, by := range byTier {
		i := c.tierIndex(name)
		for _, st := range pilotStates[:len(pilotStates)-1] {
			d.Pilots[st] += by[st]
			out[i] += by[st]
			outstanding += by[st]
		}
		unsent[i] += by[PilotCreated]
		unsentAll += by[PilotCreated]
	}
	d.Pilots["outstanding"] = outstanding
	var sci []int
	d.Science, sci = g.scienceDemand(c, now)
	d.Chat = demandChat{Enabled: c.PilotsChatEnabled, Headroom: c.ChatHeadroom}
	if c.PilotsChatEnabled {
		d.Chat.Pilots = c.ChatHeadroom
	}
	target := slices.Clone(sci)
	target[0] += d.Chat.Pilots // chat: the smallest tier
	d.Target = d.Science.Pilots + d.Chat.Pilots
	limit := d.Target
	if limit > c.MaxPilots {
		limit, d.CappedBy = c.MaxPilots, "max_pilots"
	}
	if g.spendCap != nil {
		if l, ok := g.spendCap(c.ID); ok && l < limit {
			limit, d.CappedBy = max(l, 0), "spend"
		}
	}
	for i, cut := 0, d.Target-limit; i < nt && cut > 0; i++ { // the smallest tiers give way first
		k := min(cut, target[i])
		target[i] -= k
		cut -= k
	}
	d.Target = limit
	// Each tier's outstanding pilots cover its own target; a larger tier's surplus then covers a
	// smaller tier's need.
	left, need := make([]int, nt), make([]int, nt)
	for i := range nt {
		used := min(out[i], target[i])
		left[i], need[i] = out[i]-used, target[i]-used
	}
	for i := range nt {
		for j := i + 1; j < nt && need[i] > 0; j++ {
			k := min(need[i], left[j])
			need[i] -= k
			left[j] -= k
		}
	}
	d.MintLeft = max(0, c.MintPerHour-recent)
	room := min(max(0, c.UnsentBuffer-unsentAll), d.MintLeft, max(0, c.MaxPilots-outstanding))
	wanted, cancel := make([]int, nt), make([]int, nt)
	for i := nt - 1; i >= 0; i-- { // the largest tier first
		wanted[i] = min(need[i], room)
		room -= wanted[i]
		d.Wanted += wanted[i]
		cancel[i] = min(unsent[i], left[i])
		d.CancelUnsent += cancel[i]
	}
	if len(c.tiers) > 0 {
		d.Tiers = map[string]*demandTier{}
		for i, t := range c.tiers {
			d.Tiers[t.Name] = &demandTier{Ctx: t.Ctx, Target: target[i], Wanted: wanted[i], CancelUnsent: cancel[i],
				Outstanding: out[i], Unsent: unsent[i]}
		}
	}
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
