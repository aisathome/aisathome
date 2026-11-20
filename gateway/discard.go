// Discarded answers (node verification, BACKLOG 11d): a requester that throws an answer away sends
// its next prompt without it. The gateway recognises that the way it recognises a session
// (session.go): per requester it remembers, for each answer that reached the client, the prompt it
// answered and the forms the answer reappears in later (meter.conversations). A later prompt of
// the same requester that starts with the answered prompt but continues none of the answer's forms
// discarded it (a retry, a regenerate, an agent that could not use the answer); one that continues
// a form kept it; any other prompt belongs to another conversation and leaves it open. Each
// discarded answer is counted on the node that served it and named in the usage record of the
// request that discarded it ("discarded": [{"req_id", "node"}]), so the log keeps one line per
// attempt. A signal, compared per node with our trusted nodes' rate; never a verdict. Like the
// session binds, no text is kept: lengths and hashes only.
//
// SPDX-License-Identifier: Apache-2.0
package gateway

import (
	"sync"
	"time"
)

const (
	// discardTTL: an answer not followed up within this long is forgotten (neither kept nor
	// discarded as far as the gateway knows).
	discardTTL = time.Hour
	// discardPerWho and discardWhos bound the open answers remembered (about 120 bytes each).
	discardPerWho = 32
	discardWhos   = 1024
)

// openAnswer is an answer that reached its client and whose fate is not known yet.
type openAnswer struct {
	prompt convMark    // the prompt it answered
	forms  [3]convMark // the conversation with it, as later prompts start (session.go)
	node   string
	op     string // the node's operator
	reqID  string
	at     time.Time
}

type whoAnswers struct {
	list []openAnswer // oldest first
	used time.Time
}

// discardTracker remembers each requester's open answers.
type discardTracker struct {
	h  *sessionBinds // only its hashing (seeded per gateway)
	mu sync.Mutex
	by map[string]*whoAnswers
}

func newDiscardTracker() *discardTracker {
	return &discardTracker{h: newSessionBinds(), by: map[string]*whoAnswers{}}
}

// discardRef names a discarded answer in the usage record of the request that discarded it.
type discardRef struct {
	ReqID string `json:"req_id"`
	Node  string `json:"node"`
}

// startsWith reports whether the nonce-free prompt t (chain ch) starts with all of c (or is c).
func (d *discardTracker) startsWith(c convMark, t string, ch []uint64) bool {
	if c.n == 0 || len(t) < c.n {
		return false
	}
	k, prev := c.n/bindBlock, uint64(0)
	if k > 0 {
		prev = ch[k-1]
	}
	return prev == c.full && d.h.step(prev, t[k*bindBlock:c.n]) == c.tail
}

// answered remembers an answer of node (operator op, request reqID) to who's prompt, with forms its
// conversation as later prompts start.
func (d *discardTracker) answered(who, prompt string, forms []string, node, op, reqID string, now time.Time) {
	if d == nil || who == "" || prompt == "" {
		return
	}
	a := openAnswer{prompt: d.h.mark(prompt), node: node, op: op, reqID: reqID, at: now}
	k := 0
	for _, f := range forms {
		if f != "" && k < len(a.forms) {
			a.forms[k] = d.h.mark(f)
			k++
		}
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	w := d.by[who]
	if w == nil {
		if len(d.by) >= discardWhos {
			var oldest string
			for k, x := range d.by {
				if oldest == "" || x.used.Before(d.by[oldest].used) {
					oldest = k
				}
			}
			delete(d.by, oldest)
		}
		w = &whoAnswers{}
		d.by[who] = w
	}
	w.used = now
	if len(w.list) >= discardPerWho {
		w.list = w.list[1:]
	}
	w.list = append(w.list, a)
}

// next takes who's new prompt: the open answers it keeps are closed, those it discards are
// closed and returned.
func (d *discardTracker) next(who, prompt string, now time.Time) []openAnswer {
	if d == nil || who == "" {
		return nil
	}
	d.mu.Lock()
	w := d.by[who]
	if w == nil || len(w.list) == 0 {
		d.mu.Unlock()
		return nil
	}
	list := append([]openAnswer(nil), w.list...)
	d.mu.Unlock()
	t := nonceFree(prompt)
	ch := d.h.chain(t)
	closed := map[string]bool{}
	var out []openAnswer
	for _, a := range list {
		if now.Sub(a.at) > discardTTL {
			closed[a.reqID] = true
			continue
		}
		kept := false
		for _, f := range a.forms {
			if d.startsWith(f, t, ch) {
				kept = true
				break
			}
		}
		switch {
		case kept:
			closed[a.reqID] = true
		case d.startsWith(a.prompt, t, ch):
			closed[a.reqID] = true
			out = append(out, a)
		}
	}
	if len(closed) == 0 {
		return nil
	}
	// Closed under the lock, and returned only if still open then: a concurrent prompt of the same
	// requester may have closed it meanwhile, and an answer counts as discarded once.
	d.mu.Lock()
	defer d.mu.Unlock()
	w = d.by[who]
	if w == nil {
		return nil
	}
	still := map[string]bool{}
	live := w.list[:0]
	for _, a := range w.list {
		if closed[a.reqID] {
			still[a.reqID] = true
		} else {
			live = append(live, a)
		}
	}
	w.list = live
	w.used = now
	kept := out[:0]
	for _, a := range out {
		if still[a.reqID] {
			kept = append(kept, a)
		}
	}
	return kept
}

// forget drops who's open answer reqID: it did not reach its client after all (relay.undelivered).
func (d *discardTracker) forget(who, reqID string) {
	if d == nil {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if w := d.by[who]; w != nil {
		live := w.list[:0]
		for _, a := range w.list {
			if a.reqID != reqID {
				live = append(live, a)
			}
		}
		w.list = live
	}
}

// noteDiscards closes who's open answers that prompt keeps or discards, counts the discarded
// ones and returns them for the request's usage record (api.go chatPrepare, before routing).
func (g *Gateway) noteDiscards(who, prompt string, now time.Time) []discardRef {
	var out []discardRef
	for _, a := range g.discards.next(who, prompt, now) {
		g.checks.discarded(a.node, now)
		g.log.Info("answer discarded by its requester", "node", a.node, "operator", a.op, "req", a.reqID)
		out = append(out, discardRef{ReqID: a.reqID, Node: a.node})
	}
	return out
}
