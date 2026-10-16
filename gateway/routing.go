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
// is under its share, then any, round-robin. It returns the node and the local model name.
func (g *Gateway) pick(rt *route) (*nodeConn, string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	now := time.Now()
	ids := make([]string, 0, len(g.node))
	for id := range g.node {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	threshold := g.thresholdLocked(rt, now)
	try := func(ok func(*nodeConn) bool) (*nodeConn, string) {
		for i := 0; i < len(ids); i++ {
			n := g.node[ids[(g.rr+i)%len(ids)]]
			if !rt.allows(n, now) || (ok != nil && !ok(n)) {
				continue
			}
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
				}
				n.mu.Unlock()
				if free {
					g.rr = (g.rr + i + 1) % len(ids)
					return n, m.Name
				}
			}
		}
		return nil, ""
	}
	if rt.preferSelf && rt.self != "" {
		if n, m := try(func(n *nodeConn) bool { return n.operator == rt.self }); n != nil {
			return n, m // serving oneself counts for no share
		}
	}
	over := g.overShareLocked(now)
	n, m := try(func(n *nodeConn) bool { return n.trusted || n.operator == rt.self || !over(n.operator) })
	if n == nil {
		n, m = try(nil) // every free node is over its share: better served than waiting
	}
	if n != nil && n.operator != rt.self {
		g.shares = append(g.shares, shareEntry{now, n.operator})
	}
	return n, m
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
func (g *Gateway) pickWait(ctx context.Context, rt *route, deadline time.Time) (*nodeConn, string, string) {
	waiting := false
	defer func() {
		if waiting {
			g.queued.Add(-1)
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
			if int(g.queued.Add(1)) > g.cfg.MaxQueued {
				g.queued.Add(-1)
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
