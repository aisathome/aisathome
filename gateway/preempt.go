// Preempting science for people (BACKLOG v0.3.1, owner 2026-10-24).
//
// The workload classes (project.go) order the queue, but a science request that holds a node
// keeps it until it ends, and an agent runner's step takes minutes. On a network of one node a
// person's chat then waited until its queue timeout and failed ("all nodes serving deep are
// busy"), while the node ran a TauCeti step (prod, 2026-10-24).
//
// So a chat or agent request that has waited PreemptAfterSec for a node stops a science request
// on a node it could use (at its own placement threshold, as yieldsLocked judges it): the one
// that started last, which loses the least work. The node is told to cancel it (its engine
// stops, as for a client that left), the slot frees once the node confirms, and the waiting
// request takes it: lower classes yield to it, the preempted request included. People's
// requests are never preempted.
//
// The preempted request: if nothing of it reached its client yet, the gateway sends it again
// itself, behind the people (handleChat); otherwise its stream ends with a retryable error
// event (code 503, reason "preempted"), which opencode v2 retries by continuing the step (its
// "previous response was interrupted" continuation). It is logged with outcome "preempted",
// counted per class and project in /v1/stats, and the node is credited for what reached the
// client, as for a client that left.
//
// SPDX-License-Identifier: Apache-2.0
package gateway

import (
	"errors"
	"net/http"
	"time"

	"github.com/aisathome/aisathome/protocol"
)

// outcomePreempted: a science request stopped for a person's request.
const outcomePreempted = "preempted"

// errPreempted ends an attempt that was preempted.
var errPreempted = errors.New("preempted for a person")

// preemptMsg is what the preempted request's client reads. "try again" and the 503 make
// opencode classify it as a provider-side failure it retries; "busy" is what the TauCeti
// harness looks for to resume a session after a gateway error.
const preemptMsg = "preempted for a person: chat and agent requests go first and the nodes are busy with them; try again shortly"

// maxPreemptRetries bounds how often the gateway sends a preempted request again itself before
// it gives the client the retryable error instead.
const maxPreemptRetries = 3

// sciRun is a science request holding a slot on a node, for preemption. Guarded by g.mu.
type sciRun struct {
	n     *nodeConn
	start time.Time
	stop  chan struct{} // closed to preempt it
	freed chan struct{} // closed once its slot is free again
	taken bool          // a waiting request has preempted it
}

// addScience registers a science request that holds a slot on n.
func (g *Gateway) addScience(n *nodeConn) *sciRun {
	sr := &sciRun{n: n, start: time.Now(), stop: make(chan struct{}), freed: make(chan struct{})}
	g.mu.Lock()
	g.science[sr] = struct{}{}
	g.mu.Unlock()
	return sr
}

// dropScience forgets a science request whose slot is free again.
func (g *Gateway) dropScience(sr *sciRun) {
	g.mu.Lock()
	delete(g.science, sr)
	g.mu.Unlock()
	close(sr.freed)
}

// preempts reports whether requests of class preempt science after waiting, and after how long.
func (g *Gateway) preempts(class string) (time.Duration, bool) {
	if g.cfg.PreemptAfterSec <= 0 || classRank(class) <= classRank(ClassScience) {
		return 0, false
	}
	return time.Duration(g.cfg.PreemptAfterSec) * time.Second, true
}

// preemptFor stops the science request that started last on a node rt could use, judged at
// rt's own placement threshold, and returns it (nil: none runs there). A request already
// preempted for another waiter is not chosen again.
func (g *Gateway) preemptFor(rt *route) *sciRun {
	now := time.Now()
	g.mu.Lock()
	defer g.mu.Unlock()
	threshold := g.thresholdLocked(rt, now)
	var last *sciRun
	for sr := range g.science {
		if sr.taken || !rt.usable(sr.n, now, threshold) {
			continue
		}
		if last == nil || sr.start.After(last.start) {
			last = sr
		}
	}
	if last != nil {
		last.taken = true
		close(last.stop)
		g.log.Info("preempting a science request for a person", "node", last.n.id, "for", rt.class,
			"ran_s", int(now.Sub(last.start).Seconds()))
	}
	return last
}

// isFreed reports whether a preempted request's slot is free again (nil: nothing preempted).
func (sr *sciRun) isFreed() bool {
	select {
	case <-sr.freed:
		return true
	default:
		return false
	}
}

// preempted ends an attempt that a person's request preempted: the node is told to stop, the
// attempt is logged (always: the statistics count preemptions), and a client whose stream
// started gets the retryable error event.
func (rl *relay) preempted() error {
	rl.stop()
	rl.record(outcomePreempted, protocol.Usage{CompletionTokens: rl.relayed}, true)
	rl.sseError(map[string]any{"message": preemptMsg, "type": "server_error", "code": http.StatusServiceUnavailable,
		"reason": outcomePreempted})
	return errPreempted
}

// preemptedError answers a preempted science request that the gateway did not place again:
// retryable, with Retry-After.
func preemptedError(w http.ResponseWriter) {
	w.Header().Set("Retry-After", "10")
	writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": map[string]any{
		"message": preemptMsg, "type": "server_error", "code": outcomePreempted}})
}
