// Node eligibility, capacity selection, and request queueing.
//
// SPDX-License-Identifier: Apache-2.0
package gateway

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"time"

	"github.com/aisathome/aisathome/protocol"
)

// knownFor is how long a model or role counts as "recently served", so that requests for it
// wait for a node instead of failing as unknown.
const knownFor = 24 * time.Hour

// startupGrace: right after a (re)start the gateway knows no models yet while its nodes
// reconnect, so while no node is connected, requests for any model wait instead of
// failing as unknown.
const startupGrace = 2 * time.Minute

// maxKnown caps the model and role names remembered for "temporarily unavailable" answers:
// nodes announce names, and a token reconnecting with fresh ones must not grow memory.
const maxKnown = 512

// rememberLocked records that models (and their roles) were offered at now. Caller holds g.mu.
func (g *Gateway) rememberLocked(models []protocol.ModelInfo, now time.Time) {
	for _, m := range models {
		for _, name := range append([]string{m.Name}, m.Roles...) {
			if _, ok := g.known[name]; ok || len(g.known) < maxKnown {
				g.known[name] = now
			}
		}
	}
}

// signal wakes requests waiting for capacity.
func (g *Gateway) signal() {
	g.mu.Lock()
	close(g.wake)
	g.wake = make(chan struct{})
	g.mu.Unlock()
}

// route is the placement policy of one request: the model or role, and which nodes may see
// it. exclude holds connections that already failed this request; a node that reconnected
// is a new connection and may be tried.
type route struct {
	want        string
	trusted     bool   // only trusted nodes, or the requester's own
	why         string // why trusted is set, for the error message ("" = the key's min_trust)
	trustedOnly bool   // the operator's emergency switch: trusted nodes only, not even one's own
	operator    string // only nodes of this operator ("" = any), e.g. a key's or front end's only_operator
	self        string // the requesting account's operator name: its own nodes count as trusted for it
	acct        string // the requesting account ("" = none), to name its own nodes in errors
	onlySelf    bool   // only the requester's own nodes (its preference), on top of operator
	preferSelf  bool   // try the requester's own nodes first
	tools       bool   // the request defines tools (an agent)
	consent     bool   // the key allows volunteer nodes for agents
	need        int    // context tokens: estimated prompt plus max_tokens
	needMin     int    // prompt plus minOutputRoom: enough when no node fits need
	exclude     map[*nodeConn]bool
	// For sticky routing: the requester (as the prefix cache keys it) and the block hashes of
	// the rendered conversation. Empty: no preference.
	who  string
	conv []uint64
	// Priority (project.go): the workload class, and for science the project and its cap on
	// the share of nodes.
	class   string
	project string
	share   float64
}

// usable reports whether n could serve rt now if it had a free slot: allowed, not paused,
// offering the model with at least threshold context (needMin: the request's smallest; the
// placement uses thresholdLocked). Caller holds g.mu.
func (rt *route) usable(n *nodeConn, now time.Time, threshold int) bool {
	if !rt.allows(n, now) {
		return false
	}
	n.mu.Lock()
	paused := n.paused
	n.mu.Unlock()
	if paused {
		return false
	}
	for _, m := range n.models {
		if serves(m, rt.want) && fits(m, threshold) {
			return true
		}
	}
	return false
}

// yieldsLocked returns whether rt must leave n to a request of a higher class that is looking
// for a node now (registered by pickWait) and could use it: a lower class never takes a node
// that a waiting higher class could have. "Could use" is judged at the waiting request's own
// placement threshold (thresholdLocked): a chat that waits for a node with its full context
// does not hold back a smaller node it would not take. Caller holds g.mu.
func (g *Gateway) yieldsLocked(rt *route, now time.Time) func(*nodeConn) bool {
	rank := classRank(rt.class)
	type waiter struct {
		rt        *route
		threshold int
	}
	var higher []waiter
	for w := range g.waiting {
		if classRank(w.class) > rank {
			higher = append(higher, waiter{w, g.thresholdLocked(w, now)})
		}
	}
	if len(higher) == 0 {
		return func(*nodeConn) bool { return false }
	}
	return func(n *nodeConn) bool {
		for _, w := range higher {
			if w.rt.usable(n, now, w.threshold) {
				return true
			}
		}
		return false
	}
}

// shareOKLocked returns whether a science request may take n under its project's cap on the
// share of nodes: a node already serving the project adds nothing; another one only while the
// project occupies fewer than its cap of the nodes that may serve the request. Caller holds g.mu.
func (g *Gateway) shareOKLocked(rt *route, now time.Time) func(*nodeConn) bool {
	if rt.project == "" || rt.share <= 0 || rt.share >= 1 {
		return func(*nodeConn) bool { return true }
	}
	pool, mine := 0, 0
	for _, n := range g.node {
		busy := n.projectLoad(rt.project) > 0
		if busy {
			mine++ // counted even if the route no longer allows it (excluded after a failure)
		}
		if busy || rt.usable(n, now, rt.needMin) {
			pool++
		}
	}
	limit := shareCap(rt.share, pool)
	return func(n *nodeConn) bool { return n.projectLoad(rt.project) > 0 || mine < limit }
}

// projectLoad is the number of the project's requests on n.
func (n *nodeConn) projectLoad(project string) int {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.projects[project]
}

// allows reports whether n may see the request, ignoring context and free slots. Caller
// holds g.mu (n.held, n.probEnd).
func (rt *route) allows(n *nodeConn, now time.Time) bool {
	switch {
	case rt.exclude[n] || n.held:
		return false
	case rt.operator != "" && n.operator != rt.operator:
		return false
	case rt.onlySelf && n.operator != rt.self:
		return false
	case n.trusted:
		return true
	case rt.trustedOnly:
		return false
	case rt.self != "" && n.operator == rt.self:
		return true // one's own node sees nothing one did not send oneself
	case rt.trusted:
		return false
	case rt.tools: // agents on volunteer nodes: consent, and the node's account past probation
		return rt.consent && !now.Before(n.probEnd)
	}
	return true
}

// minOutputRoom is the room for the answer that a node's context must leave when no node
// fits the request's full max_tokens: llama-server ends an answer at the end of the context
// (finish_reason "length") rather than failing, so a large max_tokens alone must not make a
// request unservable.
const minOutputRoom = 4096

// fits reports whether model m has context for the request at threshold (0 = no need given).
// A node that announces no context is assumed to fit; it reports an overflow if not.
func fits(m protocol.ModelInfo, threshold int) bool {
	return threshold == 0 || m.Ctx == 0 || m.Ctx >= threshold
}

// serves reports whether m offers the model or role want.
func serves(m protocol.ModelInfo, want string) bool { return m.Name == want || contains(m.Roles, want) }

// thresholdLocked is the context the request needs on this network now: need if any allowed
// node that is not paused fits it, otherwise needMin. A paused node (held ones are not
// allowed) may stay paused for hours: waiting for its larger context would time out while a
// smaller node could answer. Caller holds g.mu.
func (g *Gateway) thresholdLocked(rt *route, now time.Time) int {
	if rt.need == 0 {
		return 0
	}
	for _, n := range g.node {
		n.mu.Lock()
		paused := n.paused
		n.mu.Unlock()
		if paused || !rt.allows(n, now) {
			continue
		}
		for _, m := range n.models {
			if serves(m, rt.want) && fits(m, rt.need) {
				return rt.need
			}
		}
	}
	return rt.needMin
}

// pick chooses an allowed node serving the requested model or role, with enough context and
// a free slot: the requester's own nodes first if it prefers them, then nodes whose operator
// is under its share (or that hold this very conversation, already counted for it), then
// any. Within each of these steps the node that holds the longest prefix of this conversation
// for this requester comes first (stickyOrder), then the others round-robin. It returns the node and the local model name.
func (g *Gateway) pick(rt *route) (*nodeConn, string) {
	now := time.Now()
	var hits map[string]prefixHit
	var key uint64
	if g.pcache != nil && rt.who != "" {
		hits = g.pcache.held(rt.who, rt.conv, now) // its own lock, not under g.mu
		key = g.pcache.convKey(rt.who, rt.conv)
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	ids := make([]string, 0, len(g.node))
	for id := range g.node {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	sticky := stickyOrder(hits, g.node)
	threshold := g.thresholdLocked(rt, now)
	take := func(n *nodeConn) (string, bool) {
		for _, m := range n.models {
			if !serves(m, rt.want) || !fits(m, threshold) {
				continue
			}
			// Reserve the slot here, under the lock: checking now and registering the
			// request later lets concurrent requests oversubscribe the node.
			n.mu.Lock()
			free := n.inflight < m.Slots && !n.paused // the volunteer paused it
			if free {
				n.inflight++
				if rt.project != "" { // freed with the slot (relay.free)
					if n.projects == nil {
						n.projects = map[string]int{}
					}
					n.projects[rt.project]++
				}
			}
			n.mu.Unlock()
			if free {
				return m.Name, true
			}
		}
		return "", false
	}
	yields, shareOK := g.yieldsLocked(rt, now), g.shareOKLocked(rt, now)
	try := func(ok func(*nodeConn) bool) (*nodeConn, string) {
		allowed := func(n *nodeConn) bool {
			return rt.allows(n, now) && !yields(n) && shareOK(n) && (ok == nil || ok(n))
		}
		for _, n := range sticky {
			if !allowed(n) {
				continue
			}
			if m, free := take(n); free {
				g.log.Debug("routed", "sticky", true, "node", n.id, "held_kib", hits[n.id].blocks*cacheBlock/1024)
				return n, m
			}
		}
		for i := 0; i < len(ids); i++ {
			n := g.node[ids[(g.rr+i)%len(ids)]]
			if !allowed(n) {
				continue
			}
			if m, free := take(n); free {
				g.rr = (g.rr + i + 1) % len(ids)
				if len(sticky) > 0 {
					g.log.Debug("routed", "sticky", false, "node", n.id, "held_elsewhere_kib", hits[sticky[0].id].blocks*cacheBlock/1024)
				}
				return n, m
			}
		}
		return nil, ""
	}
	if rt.preferSelf && rt.self != "" {
		if n, m := try(func(n *nodeConn) bool { return n.operator == rt.self }); n != nil {
			return n, m // serving oneself counts for no share
		}
	}
	over, seen := g.overShareLocked(now, key)
	// counted: the request goes on with a conversation this node holds, and that conversation
	// already counts for the node's operator in the window. It adds nothing to the share.
	counted := func(n *nodeConn) bool { return hits[n.id].cont && seen(n.operator) }
	n, m := try(func(n *nodeConn) bool {
		return n.trusted || n.operator == rt.self || !over(n.operator) || counted(n)
	})
	if n == nil {
		n, m = try(nil) // every free node is over its share: better served than waiting
	}
	if n != nil && n.operator != rt.self && !counted(n) {
		g.shares = append(g.shares, shareEntry{now, n.operator, key})
	}
	return n, m
}

// stickyOrder lists the connected nodes that hold a prefix of the conversation, longest
// first, then the most recent. When none of them is free the request goes elsewhere at once
// rather than waiting. A wait pays off only if the holder frees up sooner than another node
// re-reads the conversation (about 90 s for 100k tokens on a 3090), and the gateway cannot
// know when it will: an agent's answer can take minutes, and the holder is often busy with
// the same requester's parallel request. The node that answers instead holds the
// conversation from then on, so the following turns stick to it.
func stickyOrder(hits map[string]prefixHit, nodes map[string]*nodeConn) []*nodeConn {
	if len(hits) == 0 {
		return nil
	}
	out := make([]*nodeConn, 0, len(hits))
	for id := range hits {
		if n := nodes[id]; n != nil {
			out = append(out, n)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := hits[out[i].id], hits[out[j].id]
		if a.blocks != b.blocks {
			return a.blocks > b.blocks
		}
		if !a.at.Equal(b.at) {
			return a.at.After(b.at)
		}
		return out[i].id < out[j].id
	})
	return out
}

// Why a request could not be placed on a node.
const (
	unknownModel = "unknown"  // no node offered this model or role recently
	queueFull    = "full"     // too many requests already waiting
	timedOut     = "timeout"  // waited QueueTimeoutSec without a free slot
	clientGone   = "canceled" // the client disconnected while waiting
	tooLong      = "context"  // no allowed node has the context the request needs
)

// pickWait is pick, but waits until deadline for capacity: a free slot, or a node serving a
// recently seen model (re)connecting. Unknown models fail at once.
//
// While it looks for a node, the request is registered for priority: requests of lower
// classes leave it every node it could use (yieldsLocked). Science waits in its own queue, so
// it never fills the queue of people's requests.
func (g *Gateway) pickWait(ctx context.Context, rt *route, deadline time.Time) (*nodeConn, string, string) {
	queue := &g.queued
	if rt.class == ClassScience {
		queue = &g.queuedSc
	}
	g.mu.Lock()
	g.waiting[rt] = struct{}{}
	g.mu.Unlock()
	waiting := false
	defer func() {
		if waiting {
			queue.Add(-1)
		}
		g.mu.Lock()
		delete(g.waiting, rt)
		lower := false // a lower class may have left a node to this request: let it look again
		for w := range g.waiting {
			lower = lower || classRank(w.class) < classRank(rt.class)
		}
		g.mu.Unlock()
		if lower {
			g.signal()
		}
	}()
	for {
		g.mu.Lock()
		wake := g.wake // taken before pick, so a signal in between is not lost
		last, seen := g.known[rt.want]
		g.mu.Unlock()
		if n, model := g.pick(rt); n != nil {
			return n, model, ""
		}
		if rt.need > 0 && g.servesFor(rt) == 0 { // allowed nodes are here, but none is large enough
			loose := *rt
			loose.need = 0
			if g.servesFor(&loose) > 0 {
				return nil, "", tooLong
			}
		}
		// Unknown models fail at once, except just after a restart while no node is back yet.
		restarting := time.Since(g.start) < startupGrace && g.NodeCount() == 0
		if (!seen || time.Since(last) > knownFor) && !restarting {
			if g.servesNow(rt.want) == 0 {
				return nil, "", unknownModel
			}
		}
		// A retry waits only if another allowed node serves the model (it is just busy);
		// otherwise there is nothing to wait for.
		if len(rt.exclude) > 0 && g.servesFor(rt) == 0 {
			return nil, "", timedOut
		}
		if !waiting {
			if int(queue.Add(1)) > g.cfg.MaxQueued {
				queue.Add(-1)
				return nil, "", queueFull
			}
			waiting = true
		}
		d := time.Until(deadline)
		if d <= 0 {
			return nil, "", timedOut
		}
		t := time.NewTimer(d)
		select {
		case <-wake:
			t.Stop()
		case <-ctx.Done():
			t.Stop()
			return nil, "", clientGone
		case <-t.C:
			return nil, "", timedOut
		}
	}
}

// servesNow counts connected nodes offering the model or role (held ones included: they
// are online, just not for this network's users).
func (g *Gateway) servesNow(want string) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	c := 0
	for _, n := range g.node {
		for _, m := range n.models {
			if serves(m, want) {
				c++
				break
			}
		}
	}
	return c
}

// servesFor counts connected nodes that offer the model or role with enough context and
// that the route allows.
func (g *Gateway) servesFor(rt *route) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	now := time.Now()
	threshold := g.thresholdLocked(rt, now)
	c := 0
	for _, n := range g.node {
		if !rt.allows(n, now) {
			continue
		}
		for _, m := range n.models {
			if serves(m, rt.want) && fits(m, threshold) {
				c++
				break
			}
		}
	}
	return c
}

// largestCtx is the largest context among nodes the route allows (0 = unknown).
func (g *Gateway) largestCtx(rt *route) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	now, best := time.Now(), 0
	for _, n := range g.node {
		if rt.allows(n, now) {
			for _, m := range n.models {
				if serves(m, rt.want) {
					best = max(best, m.Ctx)
				}
			}
		}
	}
	return best
}

// unavailable answers a request that could not be placed, with an honest reason.
func (g *Gateway) unavailable(w http.ResponseWriter, rt *route, why string) {
	want := rt.want
	switch why {
	case clientGone:
		return
	case unknownModel:
		apiError(w, http.StatusNotFound, fmt.Sprintf("model %q is not served on this network", want))
	case tooLong:
		loose := *rt
		loose.need, loose.exclude = 0, nil
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": map[string]any{
			"message": fmt.Sprintf("the conversation (about %d tokens, with room for the answer) is longer than the context of any node that may serve it (largest: %d tokens); shorten or compact it",
				rt.needMin, g.largestCtx(&loose)),
			"type": "invalid_request_error", "code": "context_length_exceeded"}})
	case queueFull:
		w.Header().Set("Retry-After", "10")
		apiError(w, http.StatusServiceUnavailable, "too many requests are waiting; try again shortly")
	default:
		if g.servesNow(want) == 0 {
			w.Header().Set("Retry-After", "60")
			apiError(w, http.StatusServiceUnavailable, fmt.Sprintf(
				"no node serving %q is online right now; AIs@home runs on volunteer machines, try again later", want))
			return
		}
		loose := *rt
		loose.exclude = nil
		if g.servesFor(&loose) == 0 {
			w.Header().Set("Retry-After", "60")
			which, because := "trusted node", "this request may only go to such nodes"
			switch {
			case rt.onlySelf:
				which, because = "node of your own", "your account is set to use only your own nodes"
			case rt.operator != "":
				which = "node of your own"
			case rt.trustedOnly:
				because = "the network is in trusted-only mode for now"
			case rt.trusted && rt.why != "":
				because = rt.why
			case rt.tools && !rt.trusted:
				// Name the actual reason: the account's own node being offline is not a key problem.
				// (Its own node online but busy is counted above and gets the busy answer.)
				if g.ownNodesOffline(rt) {
					apiError(w, http.StatusServiceUnavailable, fmt.Sprintf("no node may run agents for this key right now: "+
						"your own node is offline, and no maintainer node serving %q is online. Start your node, or try again later", want))
					return
				}
				which = "trusted node, or volunteer node past its probation,"
				because = "requests with tools go to volunteer nodes only with an API key that allows them"
				if !rt.consent {
					apiError(w, http.StatusServiceUnavailable, fmt.Sprintf("no node may run agents for this key right now: "+
						"requests with tools go only to your own node or the maintainer's, and none serving %q is online. "+
						"Create a key with \"Run requests through nodes operated by other people\" ticked on your account page, or try again later", want))
					return
				}
			}
			apiError(w, http.StatusServiceUnavailable, fmt.Sprintf("no %s serving %q is online right now (%s)", which, want, because))
			return
		}
		w.Header().Set("Retry-After", "10")
		apiError(w, http.StatusServiceUnavailable, fmt.Sprintf("all nodes serving %q are busy; try again shortly", want))
	}
}

// ownNodesOffline reports whether the requesting account has registered nodes and none of
// them is connected.
func (g *Gateway) ownNodesOffline(rt *route) bool {
	if g.accounts == nil || rt.acct == "" || rt.self == "" {
		return false
	}
	if a, _, ok := g.accounts.Get(rt.acct); !ok || len(a.Nodes) == 0 {
		return false
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, n := range g.node {
		if n.operator == rt.self {
			return false
		}
	}
	return true
}

func contains(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}
