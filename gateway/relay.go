// Relaying a chat request to a node and delivering its answer.
//
// SPDX-License-Identifier: Apache-2.0
package gateway

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/aisathome/aisathome/protocol"
)

// chatReq is one chat request as handleChat resolved it, the same for every node it tries.
type chatReq struct {
	user         string         // the requester's user id, for the usage log
	who          string         // the requester (account, front-end user or key) whose conversations a node may have cached
	charge       []string       // the budget keys the answer is charged to
	self         string         // the requesting account's operator name, to record answers an account served itself
	want         string         // the model or role the client asked for
	clientStream bool           // the client asked for a stream (nodes always stream)
	body         map[string]any // redacted; forNode picks what a node may see
	red          *redactor
	conv         *convPrint // body as a node gets it, rendered and hashed (routing and credit)
}

// maxCollected bounds a non-streaming answer; all of them share g.buffered.
const maxCollected = 2 << 20

var errTooMuchOutput = errors.New("node sent more output than requested")

// relay is one attempt of a request on one node: what reached the client, what the gateway
// metered, and what it buffers.
type relay struct {
	g     *Gateway
	w     http.ResponseWriter
	rc    *http.ResponseController
	ctx   context.Context // the client's request: done when it went away
	req   *chatReq
	n     *nodeConn
	model string
	gd    *guard    // checks the answer (guard.go), or nil
	rest  *restorer // puts secrets back into a streamed answer, or nil

	reqID     string
	raw       []byte // the request as the node got it
	mt        *meter // the gateway's own token counts, for credit
	maxTokens int
	maxChunks int
	t0        time.Time

	started bool // output reached the client: a retry is no longer possible
	// relayed counts chunks carrying generated text, as seen by the gateway itself. It meters
	// requests that end early (the node sends no usage then) and cross-checks node-reported
	// completion tokens (llama.cpp streams about one token per chunk).
	relayed, relayedChars int
	flag                  string // a guard's finding, for the usage log
	sse                   *bufio.Writer
	collected             []json.RawMessage // a non-streaming answer, assembled at the end
	chunks                int
	collectedBytes        int
	free                  func()        // frees the node's slot; once, however often called
	stopping              bool          // the node was told to stop: its slot waits for its confirmation
	sent                  delivered     // what reached the client, as the node sent it (credit)
	bufHeld               int64         // this request's share of g.buffered and of tokBuf
	tokBuf                *atomic.Int64 // what the node token's requests buffer together
}

// run sends one request to a node and relays the result. started reports whether any
// output reached the client (then a retry is no longer possible). gd, if set, checks the
// answer (guard.go); a stopped tool call returns a *guardError.
func (g *Gateway) run(w http.ResponseWriter, r *http.Request, req *chatReq, n *nodeConn, model string, gd *guard) (started bool, err error) {
	reqID := newID()
	nb := forNode(req.body) // only the fields a node may see
	nb["model"] = model
	nb["stream"] = true
	nb["stream_options"] = map[string]any{"include_usage": true}
	raw, _ := json.Marshal(nb)
	rl := &relay{g: g, w: w, ctx: r.Context(), req: req, n: n, model: model, gd: gd, reqID: reqID, raw: raw}
	rl.mt = newMeter(nb, req.conv, g.tok)
	rl.maxTokens = requestMaxTokens(nb, g.cfg.DefaultMaxTokens)
	// A node may not send more than it was asked for: bounds memory (non-streaming answers are
	// collected) and work. About one token per chunk, with room for role and usage chunks.
	rl.maxChunks = 2*rl.maxTokens + 256
	if req.red.restoring() {
		rl.rest = newRestorer(req.red)
	}

	p := newPendingReq()
	n.mu.Lock()
	n.pending[reqID] = p
	n.mu.Unlock()
	// The slot reserved by pick is freed once: as soon as the node is finished with the request
	// (Done, its error, or it left), before the end goes out to the client, so that the client's
	// next turn finds the node holding its conversation free. A request the node was told to
	// stop keeps its slot until the node confirms (awaitStopped); any other when run returns.
	rl.free = sync.OnceFunc(func() {
		n.mu.Lock()
		n.inflight--
		n.mu.Unlock()
		g.signal() // a slot is free again
	})
	defer func() {
		close(p.done)
		if rl.stopping {
			g.awaitStopped(n, reqID, p, rl.free)
			return
		}
		n.mu.Lock()
		delete(n.pending, reqID)
		n.mu.Unlock()
		rl.free()
	}()

	rl.t0 = time.Now()
	sctx, scancel := context.WithTimeout(r.Context(), 15*time.Second)
	err = n.send(sctx, protocol.Msg{Type: protocol.TypeRequest, ReqID: reqID, Body: raw})
	scancel()
	if err != nil {
		return false, err
	}

	rl.rc = http.NewResponseController(w)
	tb, _ := g.bufByTok.LoadOrStore(n.token, new(atomic.Int64))
	rl.tokBuf = tb.(*atomic.Int64)
	defer func() { g.buffered.Add(-rl.bufHeld); rl.tokBuf.Add(-rl.bufHeld) }()
	stall := time.NewTimer(time.Duration(g.cfg.FirstChunkTimeoutSec) * time.Second)
	defer stall.Stop()
	for {
		select {
		case <-stall.C: // the node went silent (e.g. a hung engine): give up on it
			g.log.Warn("node stalled", "node", n.id, "req", reqID, "started", rl.started)
			err = rl.fail("failed", "the node stopped responding", errors.New("node stalled"))
		case <-p.slow: // the client is not reading; free the node for others
			err = rl.fail("cancelled", "", errors.New("client too slow"))
		case <-r.Context().Done(): // client went away: tell the node to stop
			err = rl.fail("cancelled", "", r.Context().Err())
		case <-n.closed: // nothing left to cancel
			// The node may have sent the rest of the answer, even its end, just before it left:
			// the reader delivered all of it before closing n.closed, so finish with that first.
			for err == nil {
				select {
				case m := <-p.ch:
					var done bool
					if done, err = rl.onMsg(m); done {
						return true, nil
					}
					continue
				default:
				}
				rl.free() // the node is gone: nothing runs in its slot
				rl.partial("failed")
				err = errors.New("node disconnected")
			}
		case m := <-p.ch:
			stall.Reset(time.Duration(g.cfg.IdleTimeoutSec) * time.Second)
			var done bool
			if done, err = rl.onMsg(m); done {
				return true, nil
			}
		}
		if err != nil {
			return rl.started, err
		}
	}
}

// onMsg handles one message from the node. done means the client got the whole answer; a
// non-nil error ends the attempt.
func (rl *relay) onMsg(m protocol.Msg) (done bool, err error) {
	switch m.Type {
	case protocol.TypeChunk:
		err = rl.onChunk(m)
	case protocol.TypeError:
		err = rl.onError(m)
	case protocol.TypeDone:
		err = rl.onDone(m)
		done = err == nil
	}
	return done, err
}

// fail ends an attempt the gateway gives up on: the node is told to stop, the work it did is
// billed as an estimate, and a client whose answer already started gets clientMsg as an
// error event ("" = none). It returns err for run to return.
func (rl *relay) fail(outcome, clientMsg string, err error) error {
	rl.stop()
	rl.partial(outcome)
	if clientMsg != "" {
		rl.sseError(map[string]any{"message": clientMsg})
	}
	return err
}

// stop tells the node to stop the request. Its engine may go on for a while (llama-server
// notices a closed connection within a second and stops at the end of its current batch), so
// the slot stays reserved until the node ends the request (awaitStopped).
func (rl *relay) stop() {
	cancelOnNode(rl.n, rl.reqID)
	rl.stopping = true
}

// awaitStopped frees the slot of a request the node was told to stop once the node ended it
// (done or error: its engine has let go of it), the node left, or CancelAckTimeoutSec passed,
// for a node that never answers a cancel. Until then no other request is sent to that slot:
// it would wait behind the cancelled one in the engine (the 300 s first-chunk timeouts of the
// TauCeti review test, 2026-10-15).
func (g *Gateway) awaitStopped(n *nodeConn, reqID string, p *pendingReq, free func()) {
	release := func() {
		n.mu.Lock()
		delete(n.pending, reqID)
		n.mu.Unlock()
		free()
	}
	select {
	case <-p.ended: // already ended (it finished as it was cancelled)
		release()
		return
	case <-n.closed:
		release()
		return
	default:
	}
	go func() {
		t := time.NewTimer(time.Duration(g.cfg.CancelAckTimeoutSec) * time.Second)
		defer t.Stop()
		select {
		case <-p.ended:
		case <-n.closed:
		case <-g.stop:
		case <-t.C:
			g.log.Warn("node did not end a cancelled request; freeing its slot anyway", "node", n.id, "req", reqID,
				"after_s", g.cfg.CancelAckTimeoutSec)
		}
		release()
	}()
}

// sseError ends a stream that already started with an error event. Before any output the
// client gets an HTTP error from handleChat instead (or the answer of a retry).
func (rl *relay) sseError(e map[string]any) {
	if rl.started {
		rl.writeSSE(mustJSON(map[string]any{"error": e}))
	}
}

func (rl *relay) record(outcome string, u protocol.Usage, estimated bool) {
	rl.commit(rl.meter(outcome, u, estimated))
}

// meter builds the usage record of the attempt: tokens and credit as the gateway counts them.
// It also remembers the prompt (and for an ok answer the answer) as served by the node, for
// cache credit and routing, so onDone runs it before the end of the answer goes out.
func (rl *relay) meter(outcome string, u protocol.Usage, estimated bool) *usageRecord {
	n, req := rl.n, rl.req
	u, clamped := clampUsage(u, len(rl.raw), rl.maxTokens, rl.relayed)
	u.DurationMs = time.Since(rl.t0).Milliseconds()
	rec := &usageRecord{Time: time.Now().UTC(), User: req.user, Node: n.id, Operator: n.operator,
		Model: rl.model, Requested: req.want, Usage: u, ReqID: rl.reqID, Outcome: outcome, Relayed: rl.relayed,
		Estimated: estimated, Clamped: clamped, SelfServed: req.self != "" && n.operator == req.self, Guard: rl.flag,
		Tools: hasTools(req.body)}
	rl.g.meterRecord(rec, rl.mt, &rl.sent, n, req.who, u, rl.maxTokens, rec.Time)
	return rec
}

// undelivered turns the record of an ok answer whose end never reached the client into that
// of a cancelled one, credited for what did reach it.
func (rl *relay) undelivered(rec *usageRecord) {
	rec.Outcome = "cancelled"
	rl.g.countOutput(rec, rl.mt, &rl.sent, rec.Usage, rl.maxTokens)
}

// commit logs the record, grants its credit and charges the budgets.
func (rl *relay) commit(rec *usageRecord) {
	g, n, req := rl.g, rl.n, rl.req
	if (rec.Outcome == "ok" || rec.Outcome == "cancelled") && !rec.SelfServed {
		g.grantCredit(n, rec.Credit, rec.Time)
	}
	g.use.record(*rec)
	g.stats.add(*rec) // clamped numbers: a node cannot inflate the public statistics either
	if rec.Outcome == outcomeBlocked || rec.SelfServed {
		return // the user never got the answer, or it came from the account's own node
	}
	// Budgets are charged from what the gateway itself saw, so a node cannot drain them.
	bu := rec.Usage
	bu.PromptTokens = min(bu.PromptTokens, len(rl.raw)/3+256)
	bu.CachedTokens = min(bu.CachedTokens, bu.PromptTokens)
	bu.CompletionTokens = min(bu.CompletionTokens, rl.relayedChars/2+16) // by characters, not chunks
	for _, k := range req.charge {
		g.budget.add(k, weighted(bu), time.Now())
	}
}

// partial records an attempt that ended early.
func (rl *relay) partial(outcome string) {
	if rl.relayed > 0 { // the node did visible work; bill an estimate rather than nothing
		rl.record(outcome, protocol.Usage{CompletionTokens: rl.relayed}, true)
	}
}

// writeSSE sends one event; a client that does not take it within 30 s is treated as gone.
func (rl *relay) writeSSE(data []byte) error {
	if rl.sse == nil {
		rl.w.Header().Set("Content-Type", "text/event-stream")
		rl.w.Header().Set("Cache-Control", "no-cache")
		rl.w.WriteHeader(http.StatusOK)
		rl.sse = bufio.NewWriter(rl.w)
	}
	_ = rl.rc.SetWriteDeadline(time.Now().Add(30 * time.Second))
	fmt.Fprintf(rl.sse, "data: %s\n\n", data)
	if err := rl.sse.Flush(); err != nil {
		return err
	}
	return rl.rc.Flush()
}

// send passes a piece of the answer to a streaming client, and notes it as delivered once
// it was written.
func (rl *relay) send(data []byte) error {
	rl.started = true
	out, orig := data, data
	if rl.rest != nil {
		out, orig = rl.rest.chunk(data) // orig: only the text the restorer released
	}
	if err := rl.writeSSE(out); err != nil {
		return err
	}
	rl.delivered(orig)
	return nil
}

// delivered notes a piece of the answer, as the node sent it, as having reached the client.
func (rl *relay) delivered(data []byte) {
	rl.sent.chunk(data)
	if hasText(data) {
		rl.sent.chunks++
	}
}

// block stops an answer a guard flagged: nothing more of it reaches the client, and the
// node gets no requests until the operator clears it. Unlike fail it records the attempt
// even without output, as blocked and with the rule.
func (rl *relay) block(rule string) error {
	rl.stop()
	rl.flag = rule
	rl.record(outcomeBlocked, protocol.Usage{CompletionTokens: rl.relayed}, true)
	rl.g.holdNode(rl.n, rule, rl.reqID)
	rl.sseError(map[string]any{"message": "the answer was stopped by a safety check"})
	return &guardError{rule}
}

// onChunk meters and relays (or collects) one piece of the answer. A non-nil error ends
// the attempt.
func (rl *relay) onChunk(m protocol.Msg) error {
	g := rl.g
	if hasText(m.Data) {
		rl.relayed++
	}
	rl.mt.chunk(m.Data)
	rl.chunks++
	rl.collectedBytes += len(m.Data)
	rl.relayedChars += textLen(m.Data)
	overToken, overGlobal := false, false
	if !rl.req.clientStream {
		rl.bufHeld += int64(len(m.Data))
		overToken = rl.tokBuf.Add(int64(len(m.Data))) > maxBufferedPerToken
		overGlobal = g.buffered.Add(int64(len(m.Data))) > maxBufferedTotal
	}
	if overGlobal && !overToken && rl.chunks <= rl.maxChunks && rl.collectedBytes <= maxCollected {
		g.log.Warn("gateway answer buffers full", "node", rl.n.id)
		return rl.fail("failed", "", errGatewayBusy) // not the node's fault: retried, not reported
	}
	if rl.chunks > rl.maxChunks || (!rl.req.clientStream && rl.collectedBytes > maxCollected) || overToken {
		g.log.Warn("node sent more output than requested", "node", rl.n.id, "chunks", rl.chunks)
		return rl.fail("failed", "the node sent more output than requested", errTooMuchOutput)
	}
	outs := [][]byte{[]byte(m.Data)}
	if gd := rl.gd; gd != nil {
		var big bool
		outs, big = gd.filter(m.Data)
		if gd.violation != "" {
			return rl.block(gd.violation)
		}
		if big {
			return rl.fail("failed", "the node sent more output than requested", errTooMuchOutput)
		}
		if gd.finish != nil { // a finish_reason after tool calls: they are complete
			if rule := gd.check(); rule != "" {
				return rl.block(rule)
			}
			outs = append(outs, gd.release()...)
		}
	}
	if !rl.req.clientStream {
		rl.collected = append(rl.collected, m.Data)
		return nil
	}
	for _, data := range outs {
		if err := rl.send(data); err != nil {
			return rl.fail("cancelled", "", err)
		}
	}
	return nil
}

// onError passes on a node's error. The node ended the request itself: nothing to cancel.
func (rl *relay) onError(m protocol.Msg) error {
	rl.free()
	msg := errCategory(m.Error) // never pass on (or log) free text from a node
	e := map[string]any{"message": msg}
	if isContextOverflow(msg) {
		e["type"], e["code"] = "invalid_request_error", "context_length_exceeded"
	}
	rl.sseError(e)
	rl.partial("failed")
	return errors.New(msg)
}

// onDone finishes the answer: final guard checks, the usage record, and the end of the stream
// or the assembled answer. nil means the client got the answer.
func (rl *relay) onDone(m protocol.Msg) error {
	u := protocol.Usage{}
	if m.Usage != nil {
		u = *m.Usage
	}
	// Every check runs before the slot is freed: a node a check stops is held first (block),
	// so no waiting request (another user's prompt) can be sent to it in between. Its slot is
	// freed when run returns.
	var released [][]byte // tool calls the guard held back until they were complete
	if gd := rl.gd; gd != nil {
		if rule := gd.check(); rule != "" {
			return rl.block(rule)
		}
		released = gd.release()
		if rl.flag = gd.textFlag(); rl.flag != "" { // text is only flagged, not stopped
			rl.g.log.Warn("guard flagged a node's answer", "node", rl.n.id, "operator", rl.n.operator, "rule", rl.flag, "req", rl.reqID)
		}
	}
	var ans map[string]any
	if !rl.req.clientStream {
		ans = assemble(rl.reqID, rl.model, rl.collected, u)
		if rl.gd != nil { // what the client gets, not the pieces the guard saw
			if rule := checkAssembled(ans, rl.gd.req); rule != "" {
				return rl.block(rule)
			}
		}
	}
	rl.free() // the node is finished with it, and it passed: free before the client sees the end
	// The end must reach the client: one that left before it (or whose connection fails on it)
	// did not get the answer, so it is recorded as cancelled, not ok.
	rl.started = true // output is going out: no retry from here on
	if err := rl.ctx.Err(); err != nil {
		rl.record("cancelled", u, false)
		return fmt.Errorf("client gone before the end of the answer: %w", err)
	}
	// Metered (and the conversation remembered for its node) before the client can see the
	// end and send its next turn; logged and credited once the outcome is known.
	rec := rl.meter("ok", u, false)
	err := rl.deliverEnd(released, ans)
	if err == nil {
		// Writes into a closed connection can still succeed for a while: a client whose request
		// ended during metering or delivery is gone all the same.
		if cerr := rl.ctx.Err(); cerr != nil {
			err = cerr
		}
	}
	if err != nil {
		rl.undelivered(rec)
	}
	rl.commit(rec)
	if err != nil {
		return fmt.Errorf("client gone at the end of the answer: %w", err)
	}
	return nil
}

// deliverEnd writes the end of the answer: the tool calls the guard released and [DONE], or
// the assembled answer.
func (rl *relay) deliverEnd(released [][]byte, ans map[string]any) error {
	if !rl.req.clientStream {
		rl.w.Header().Set("Content-Type", "application/json")
		rl.w.WriteHeader(http.StatusOK)
		if _, err := rl.w.Write(append(mustJSON(restoreAnswer(ans, rl.req.red)), '\n')); err != nil {
			return err
		}
		if err := rl.rc.Flush(); err != nil {
			return err
		}
		for _, data := range rl.collected { // written whole: all of it reached the client
			rl.delivered(data)
		}
		return nil
	}
	for _, data := range released {
		if err := rl.send(data); err != nil {
			return err
		}
	}
	if rl.rest != nil {
		if f, orig := rl.rest.flush(); f != nil {
			if err := rl.writeSSE(f); err != nil {
				return err
			}
			rl.delivered(orig)
		}
	}
	return rl.writeSSE([]byte("[DONE]")) // also answers an empty stream properly
}

// maxBufferedTotal bounds all non-streaming answers held at once, and maxBufferedPerToken
// the share one node token can take, so one operator cannot fill it for everyone.
const (
	maxBufferedTotal    = 64 << 20
	maxBufferedPerToken = 12 << 20
)

// errGatewayBusy means the gateway itself is out of buffer space: not the node's fault.
var errGatewayBusy = errors.New("gateway busy")

func newID() string {
	b := make([]byte, 12)
	rand.Read(b)
	return hex.EncodeToString(b)
}
