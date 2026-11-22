// Node eligibility, capacity selection, and request queueing.
//
// SPDX-License-Identifier: Apache-2.0
package gateway

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"sync/atomic"
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
	model       string // only nodes serving this model name: the session's (session.go); "" = any serving want
	pii         string // the kind of personal data found (piiKind): only the requester's own nodes; "" = none
	ownOnlyMode bool   // the operator's emergency switch (RoutingOwnOnly): only the requester's own nodes
	// ownOnlyNow is the switch as the gateway knows it now (Gateway.ownOnlyNow), for a request that
	// waits: turned on meanwhile, it keeps a stranger's node from the request too. nil: ownOnlyMode alone.
	ownOnlyNow *atomic.Bool
	operator   string // only nodes of this operator ("" = any), e.g. a key's or front end's only_operator
	self       string // the requesting account's operator name: its own nodes (own) may see anything it sends
	acct       string // the requesting account ("" = none), to name its own nodes in errors
	onlySelf   bool   // only the requester's own nodes (its preference, or past its budget), on top of operator
	preferSelf bool   // try the requester's own nodes first
	tools      bool   // the request defines tools (an agent)
	consent    bool   // the key allows volunteer nodes for agents
	need       int    // context tokens: estimated prompt plus max_tokens
	needMin    int    // prompt plus minOutputRoom: enough when no node fits need
	exclude    map[*nodeConn]bool
	// For sticky routing: the requester (as the prefix cache keys it) and the block hashes of
	// the rendered conversation. Empty: no preference.
	who  string
	conv []uint64
	// Priority (project.go): the workload class, and for science the project and its cap on
	// the share of nodes.
	class   string
	project string
	share   float64
	// pilotProbationOK: the project's agents may use pilots of accounts still in probation, and nodes
	// without an account (admitted by the model check, anon.go) in theirs (Project.PilotProbationOK).
	pilotProbationOK bool
	// rules: the chat rules apply to this gateway (chatrules.go).
	rules bool
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
		if rt.offers(m) && fits(m, threshold) {
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

// own reports whether n is the requester's own node: a standalone node registered to the requesting
// account. Not a pilot: the connector binds it to the account, and a connector (or its host) could bind
// a stranger's machine. Not a linked node its owner has not confirmed yet: a code someone else sent may
// have linked a stranger's machine (nodelink.go). Caller holds g.mu (n.unconfirmed).
func (rt *route) own(n *nodeConn) bool {
	return rt.self != "" && n.operator == rt.self && n.pilot == "" && !n.unconfirmed
}

// allows reports whether n may see the request, ignoring context and free slots. Every node is
// untrusted (decision 0007, addendum 2026-11-20); the one exception is the requester's own node, which
// sees nothing the requester did not send itself, so it may get the requester's personal data too: one's
// own data on one's own machine. Caller holds g.mu (n.held, n.probEnd).
func (rt *route) allows(n *nodeConn, now time.Time) bool {
	switch {
	case rt.exclude[n] || n.held:
		return false
	case n.anon && (rt.class != ClassScience || !n.admitted):
		return false // a node without an account: science only, and only once the model check admitted it (anon.go)
	case n.pilot != "" && (n.registering || !now.Before(n.deadline) || (n.scienceOnly && rt.class != ClassScience)):
		return false // a pilot still registering, past its deadline, or without chat (connector.go)
	case rt.operator != "" && n.operator != rt.operator:
		return false
	case rt.own(n):
		return true
	case rt.onlySelf || rt.ownOnly() || rt.pii != "":
		return false // personal data, the emergency switch or the requester's choice: its own nodes only
	case rt.gatedBy(n):
		return false // the chat rules (chatrules.go): its operator has not accepted the operator terms
	case rt.tools: // agents on volunteer nodes: consent, and the node's account past probation
		return rt.consent && (!now.Before(n.probEnd) || ((n.pilot != "" || n.anon) && rt.pilotProbationOK))
	}
	return true
}

// ownOnly reports whether the emergency switch keeps rt on its sender's own nodes.
func (rt *route) ownOnly() bool {
	return rt.ownOnlyMode || (rt.ownOnlyNow != nil && rt.ownOnlyNow.Load())
}

// minOutputRoom is the room for the answer that a node's context must leave when no node
// fits the request's full max_tokens: llama-server ends an answer at the end of the context
// (finish_reason "length") rather than failing, so a large max_tokens alone must not make a
// request unservable.
const minOutputRoom = 4096

// countSlots is how many prompts are counted at once. Counting comes before the request has a
// node, so anyone with a key can ask for it: real text costs 30 MB/s, but a run of letters as
// long as a whole body (8 MiB) takes 3 s and about 270 MiB. The others wait, as the budget for
// request bodies bounds how many there are.
const countSlots = 4

// countPrompt counts cp's tokens for routing, and for credit after it.
func (g *Gateway) countPrompt(cp *convPrint) {
	if g.tok == nil {
		return
	}
	g.counting <- struct{}{}
	defer func() { <-g.counting }()
	cp.ids = g.tok.Encode(cp.prompt)
}

// promptEstimate is the context a rendered prompt takes on a node, in tokens.
//
// With a tokenizer it is the gateway's own count of the rendered prompt, which on 713 real
// requests (up to 114,070 tokens) was never under llama-server's count and at most 47 over.
// The margin of 1% and 16 tokens covers what the rendering leaves out (an image counts as its
// markers); a request that still overflows has minOutputRoom left before the node refuses it.
//
// Without one it is bytes/3 of the same text, safe rather than near: real sessions run 3.1-3.7
// bytes per token (non-ASCII Lean), English nearer 4, CJK about 3.
func promptEstimate(cp *convPrint) int {
	if cp.ids != nil {
		return len(cp.ids) + len(cp.ids)/100 + 16
	}
	return len(cp.prompt)/3 + 16
}

// setPrompt sets the context the request needs from its rendered prompt and max_tokens.
func (rt *route) setPrompt(cp *convPrint, maxTokens int) {
	prompt := promptEstimate(cp)
	rt.need, rt.needMin = prompt+maxTokens, prompt+min(maxTokens, minOutputRoom)
}

// fits reports whether model m has context for the request at threshold (0 = no need given).
// A node that announces no context is assumed to fit; it reports an overflow if not.
func fits(m protocol.ModelInfo, threshold int) bool {
	return threshold == 0 || m.Ctx == 0 || m.Ctx >= threshold
}

// serves reports whether m offers the model or role want.
func serves(m protocol.ModelInfo, want string) bool { return m.Name == want || contains(m.Roles, want) }

// offers reports whether m may serve rt: it offers the model or role asked for, and it is the
// session's model if the request continues a session.
func (rt *route) offers(m protocol.ModelInfo) bool {
	return serves(m, rt.want) && (rt.model == "" || m.Name == rt.model)
}

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
			if rt.offers(m) && fits(m, rt.need) {
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
			if !rt.offers(m) || !fits(m, threshold) {
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
	own := rt.own
	if rt.preferSelf && rt.self != "" {
		if n, m := try(own); n != nil {
			return n, m // serving oneself counts for no share
		}
	}
	over, seen := g.overShareLocked(now, key)
	// counted: the request goes on with a conversation this node holds, and that conversation
	// already counts for the node's operator in the window. It adds nothing to the share.
	counted := func(n *nodeConn) bool { return hits[n.id].cont && seen(n.operator) }
	n, m := try(func(n *nodeConn) bool {
		// Not own(n): an own node is never capped (it serves through try(nil) below, and counts for
		// no share), but without the account's preference it is not preferred either.
		return !over(n.operator) || counted(n)
	})
	if n == nil {
		n, m = try(nil) // every free node is over its share: better served than waiting
	}
	if n != nil && !own(n) && !counted(n) {
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
	personal     = "personal" // personal data, and none of the requester's own nodes is online to take it
)

// pickWait is pick, but waits until deadline for capacity: a free slot, or a node serving a
// recently seen model (re)connecting. Unknown models fail at once.
//
// While it looks for a node, the request is registered for priority: requests of lower
// classes leave it every node it could use (yieldsLocked). Science waits in its own queue, so
// it never fills the queue of people's requests. A chat or agent request that has waited
// PreemptAfterSec stops a science request on a node it could use (preempt.go), one at a time:
// another only once the slot of the first is free and someone else took it.
func (g *Gateway) pickWait(ctx context.Context, rt *route, deadline time.Time) (*nodeConn, string, string) {
	queue := &g.queued
	if rt.class == ClassScience {
		queue = &g.queuedSc
	}
	g.mu.Lock()
	g.waiting[rt] = struct{}{}
	g.mu.Unlock()
	waiting := false
	var preemptAt time.Time // zero: this request does not preempt
	if after, ok := g.preempts(rt.class); ok {
		preemptAt = time.Now().Add(after)
	}
	var claim *sciRun // the science request this one preempted, until its slot is free
	why := ""
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
		// A retry waits only if another allowed node serves the model (it is just busy).
		// Otherwise there is nothing to wait for.
		if len(rt.exclude) > 0 && g.servesFor(rt) == 0 {
			return nil, "", timedOut
		}
		// Personal data waits only for the requester's own node, and only while one is online (just
		// busy), or while its registered nodes may still reconnect after a gateway start. Otherwise it
		// is refused at once (unavailable): no other node will ever take it.
		if rt.pii != "" && g.servesFor(rt) == 0 && (time.Since(g.start) >= startupGrace || !g.hasOwnNodes(rt.acct)) {
			return nil, "", personal
		}
		if !waiting {
			if int(queue.Add(1)) > g.cfg.MaxQueued {
				queue.Add(-1)
				return nil, "", queueFull
			}
			waiting = true
			g.press.queuePeak(int(g.queued.Load()), int(g.queuedSc.Load()), time.Now())
		}
		d := time.Until(deadline)
		if d <= 0 {
			return nil, "", timedOut
		}
		var preemptC <-chan time.Time
		pt := (*time.Timer)(nil)
		if !preemptAt.IsZero() && (claim == nil || claim.isFreed()) {
			if wait := time.Until(preemptAt); wait > 0 {
				pt = time.NewTimer(wait)
				preemptC = pt.C
			} else if claim = g.preemptFor(rt); claim != nil {
				continue // its slot frees once the node has stopped it, and the signal wakes us
			}
		}
		t := time.NewTimer(d)
		select {
		case <-wake:
		case <-preemptC:
		case <-ctx.Done():
			why = clientGone
		case <-t.C:
			why = timedOut
		}
		t.Stop()
		if pt != nil {
			pt.Stop()
		}
		if why != "" {
			return nil, "", why
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
			if rt.offers(m) && fits(m, threshold) {
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
				if rt.offers(m) {
					best = max(best, m.Ctx)
				}
			}
		}
	}
	return best
}

// unavailable answers a request that could not be placed, with an honest reason, and logs the
// refusal by reason (never the request's text): before 2026-11-08 refusals left no trace, and two
// sessions refused for 50 minutes each looked like a node outage.
func (g *Gateway) unavailable(w http.ResponseWriter, rt *route, why string) {
	want := rt.want
	reason := ""
	defer func() {
		if reason != "" && g.log != nil {
			g.log.Info("request refused", "reason", reason, "model", want, "personal_data", rt.pii,
				"class", rt.class, "project", rt.project, "session_model", rt.model)
			g.noChatDemand(rt.class) // a WARN if no node online may serve people's requests (servecap.go)
		}
	}()
	switch why {
	case clientGone:
		return
	case unknownModel:
		reason = "unknown_model"
		apiError(w, http.StatusNotFound, fmt.Sprintf("model %q is not served on this network", want))
	case tooLong:
		reason = "too_long"
		loose := *rt
		loose.need, loose.exclude = 0, nil
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": map[string]any{
			"message": fmt.Sprintf("the conversation (about %d tokens, with room for the answer) is longer than the context of any node that may serve it (largest: %d tokens); shorten or compact it",
				rt.needMin, g.largestCtx(&loose)),
			"type": "invalid_request_error", "code": "context_length_exceeded"}})
	case personal:
		reason = "personal_data"
		personalDataError(w, rt.pii)
	case queueFull:
		reason = "queue_full"
		w.Header().Set("Retry-After", "10")
		apiError(w, http.StatusServiceUnavailable, "too many requests are waiting; try again shortly")
	default:
		if rt.model != "" {
			// The request continues a session: no other model may answer it (session.go), and the
			// answers below name the model it is held to.
			want = rt.model
			if g.servesNow(want) == 0 {
				reason = "session_model_offline"
				w.Header().Set("Retry-After", "60")
				apiError(w, http.StatusServiceUnavailable, fmt.Sprintf("this conversation started on %q, and no node serving "+
					"that model is online right now. A conversation stays on the model it started on, so that its answers "+
					"do not change model midway; try again later, or start a new conversation", want))
				return
			}
		}
		loose := *rt
		loose.exclude = nil
		if rt.pii != "" && g.servesFor(&loose) == 0 { // its own node went away while it waited
			reason = "personal_data"
			personalDataError(w, rt.pii)
			return
		}
		if g.servesNow(want) == 0 {
			reason = "no_node"
			w.Header().Set("Retry-After", "60")
			apiError(w, http.StatusServiceUnavailable, fmt.Sprintf(
				"no node serving %q is online right now; AIs@home runs on volunteer machines, try again later", want))
			return
		}
		if g.servesFor(&loose) == 0 {
			w.Header().Set("Retry-After", "60")
			reason = "no_allowed_node"
			which, because := "node", "this request may not go to the nodes that are online"
			switch {
			case rt.onlySelf:
				reason = "own_nodes_only"
				which, because = "node of your own", "your account is set to use only your own nodes"
			case rt.operator != "":
				reason = "operator_nodes_only"
				which = "node of your own"
			case rt.ownOnly():
				reason = "own_only_mode"
				which, because = "node of your own", "for now the network serves each request only on its sender's own nodes"
			case g.rulesBlocked(&loose):
				// The chat rules (chatrules.go) left no node.
				reason = "operator_terms"
				which, because = "node", "only nodes whose operators accepted the operator terms serve people's requests"
			case rt.tools:
				// Name the actual reason: the account's own node being offline is not a key problem.
				// (Its own node online but busy is counted above and gets the busy answer.)
				if g.ownNodesOffline(rt) {
					reason = "own_node_offline"
					apiError(w, http.StatusServiceUnavailable, fmt.Sprintf("no node may run agents for this key right now: "+
						"your own node is offline, and no other node serving %q may take them. Start your node, or try again later", want))
					return
				}
				if !rt.consent {
					reason = "tools_no_consent"
					apiError(w, http.StatusServiceUnavailable, fmt.Sprintf("no node may run agents for this key right now: "+
						"requests with tools go only to your own node, and none serving %q is online. "+
						"Create a key with \"Run requests through nodes operated by other people\" ticked on your account page, or try again later", want))
					return
				}
				reason = "tools_probation"
				which, because = "volunteer node past its probation", "requests with tools go only to nodes past their probation"
			}
			apiError(w, http.StatusServiceUnavailable, fmt.Sprintf("no %s serving %q is online right now (%s)", which, want, because))
			return
		}
		reason = "busy"
		w.Header().Set("Retry-After", "10")
		apiError(w, http.StatusServiceUnavailable, fmt.Sprintf("all nodes serving %q are busy; try again shortly", want))
	}
}

// rulesBlocked reports whether the chat rules alone keep rt from every connected node: only nodes
// whose operators have not accepted the operator terms serve it.
func (g *Gateway) rulesBlocked(rt *route) bool {
	if !rt.rules || rt.class == ClassScience {
		return false
	}
	off := *rt
	off.rules = false
	return g.servesFor(&off) > 0
}

// hasOwnNodes reports whether account id has registered nodes of its own (they may be offline): not a
// linked node waiting for its owner's confirmation.
func (g *Gateway) hasOwnNodes(id string) bool {
	return g.accounts != nil && id != "" && g.accounts.confirmedNodes(id)
}

// hasNodes reports whether account id has registered nodes (they may be offline).
func (g *Gateway) hasNodes(id string) bool {
	if g.accounts == nil || id == "" {
		return false
	}
	a, _, ok := g.accounts.Get(id)
	return ok && len(a.Nodes) > 0
}

// personalDataError refuses a request that looks like it carries personal data (piiKind) when none of
// the requester's own nodes can take it: every other node is a stranger's. The user rule of the terms,
// and what was found, so a false positive can be told apart (never the value). 400: sending it again
// changes nothing, so clients do not retry.
func personalDataError(w http.ResponseWriter, kind string) {
	writeJSON(w, http.StatusBadRequest, map[string]any{"error": map[string]any{
		"message": fmt.Sprintf("This looks like personal data (%s). Personal data isn't allowed in chats on AIs@home: "+
			"answers are computed on volunteers' machines. Remove it and try again. If it isn't personal data, "+
			"rewrite that part (for example, split a long number).", kind),
		"type": "invalid_request_error", "code": "personal_data"}})
}

// ownNodesOffline reports whether the requesting account has registered nodes and none of
// them is connected.
func (g *Gateway) ownNodesOffline(rt *route) bool {
	if rt.self == "" || !g.hasOwnNodes(rt.acct) {
		return false
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, n := range g.node {
		if n.operator == rt.self && !n.unconfirmed {
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
