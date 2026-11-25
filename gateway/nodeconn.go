// Connected node sessions: registration, message delivery, and cancellation.
//
// SPDX-License-Identifier: Apache-2.0
package gateway

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/aisathome/aisathome/protocol"
	"github.com/coder/websocket"
)

// NodeCount returns the number of connected nodes, held ones included.
func (g *Gateway) NodeCount() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.node)
}

// nodesOnline counts the connected nodes that a guard does not hold: what the public statistics
// and the health warnings call online. A held node gets no requests.
func (g *Gateway) nodesOnline() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	c := 0
	for _, n := range g.node {
		if !n.held {
			c++
		}
	}
	return c
}

type nodeConn struct {
	id       string
	operator string // the config token's operator, or the account's pseudonym
	token    string // to disconnect the node if its token is revoked
	account  string // the account that registered it; "" for a token from the config
	ip       string // client address it connected from (account nodes: maxNodesPerIP)
	// unconfirmed: a linked node its owner has not confirmed as their machine yet (AccountNode.unconfirmed):
	// not its account's own node; guarded by g.mu, updated by sweepNodes.
	unconfirmed bool
	held        bool      // stopped by a guard until the operator clears it; guarded by g.mu
	probEnd     time.Time // account nodes: no agents (requests with tools) before; guarded by g.mu
	agent       string    // the node software's version from its hello ("aisathome-node/0.2.0"), "" if none or invalid
	proto       int       // the protocol version it speaks (hello's protocol_version; 1 if none)
	since       time.Time // when it connected
	models      []protocol.ModelInfo
	conn        *websocket.Conn
	wmu         sync.Mutex // serialises writes
	mu          sync.Mutex
	pending     map[string]*pendingReq
	inflight    int                 // slots reserved by pick, freed once the node is finished with the request (relay.free); guarded by mu
	projects    map[string]int      // of inflight, the science projects' requests (share cap); guarded by mu
	work        map[string]nodeWork // the requests in flight, by id, for the operator's page (nodework.go); guarded by mu
	active      int
	paused      bool // from the last heartbeat: take no new requests; guarded by mu
	lastSeen    time.Time
	closed      chan struct{}
	// Pilots (connector.go): id is the pilot id. Set before the node is registered, never changed.
	pilot     string    // the pilot id; "" for a node
	connector string    // its connector
	extUser   string    // its connector user (no_chat_users)
	deadline  time.Time // its credential's end
	// scienceOnly: the pilot serves only science requests (connector.go); guarded by g.mu, updated
	// by sweepNodes when its account's switch or the connector's settings change.
	scienceOnly bool
	// registering: a pilot registered but not yet routable, until its record is checked once more
	// (a report during its handshake ends it); guarded by g.mu.
	registering bool
	// rules: what the chat rules know of it (chatrules.go); guarded by g.mu, updated by sweepNodes and
	// Reload.
	rules nodeRules
	// anon: a node without an account (anon.go): science only, once admitted. Set at connect, never changed.
	anon bool
	// admitted: a node without an account passed the model check's admission (modelcheck.go); guarded by g.mu.
	admitted bool
}

// pendingReq is one request in flight on a node. The reader delivers messages on ch; the
// request handler closes done when it stops listening, so delivery never blocks forever.
// ended is closed when the node ends the request itself (done or error), also after the
// handler stopped listening: after a cancel that is the node saying its engine has stopped.
type pendingReq struct {
	ch       chan protocol.Msg
	done     chan struct{}
	slow     chan struct{} // closed when the client cannot keep up
	slowOnce sync.Once
	ended    chan struct{}
	endOnce  sync.Once
}

func newPendingReq() *pendingReq {
	return &pendingReq{ch: make(chan protocol.Msg, 64), done: make(chan struct{}), slow: make(chan struct{}),
		ended: make(chan struct{})}
}

func (p *pendingReq) end() { p.endOnce.Do(func() { close(p.ended) }) }

// deliver never blocks the node's shared reader: if this request's client is not keeping up,
// the request is dropped rather than stalling the node's other requests and heartbeats.
// deliverGrace is how long a full request buffer may block the node's read loop before the
// request is called too slow. Without it a burst of small chunks, or a briefly busy gateway,
// was taken for a slow client (seen under the race detector, 2026-10-10); a client that
// really stopped reading still holds the node for at most this long.
const deliverGrace = 500 * time.Millisecond

func (p *pendingReq) deliver(m protocol.Msg) {
	select {
	case p.ch <- m:
		return
	case <-p.done:
		return
	case <-p.slow: // already given up on: drop at once, never hold the node again
		return
	default:
	}
	t := time.NewTimer(deliverGrace)
	defer t.Stop()
	select {
	case p.ch <- m:
	case <-p.done:
	case <-t.C:
		p.slowOnce.Do(func() { close(p.slow) })
	}
}

func (n *nodeConn) send(ctx context.Context, m protocol.Msg) error {
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	n.wmu.Lock()
	defer n.wmu.Unlock()
	return n.conn.Write(ctx, websocket.MessageText, b)
}

func (n *nodeConn) slots(model string) int {
	for _, m := range n.models {
		if m.Name == model {
			return m.Slots
		}
	}
	return 0
}

func (g *Gateway) handleNodeConnect(w http.ResponseWriter, r *http.Request) {
	token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	var pl *pilotLogin
	var tok NodeToken
	var account string
	if strings.HasPrefix(token, "pc-") { // a pilot's credential (pilot_nodes.go)
		var status int
		var why string
		if pl, status, why = g.authPilot(token); pl == nil {
			http.Error(w, why, status)
			return
		}
		tok, account = pl.tok, pl.account
	} else {
		var ok bool
		if tok, account, ok = g.nodeAuth(token); !ok {
			http.Error(w, "unknown node token", http.StatusUnauthorized)
			return
		}
	}
	operator, ip := tok.Operator, g.clientIP(r)
	c, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	c.SetReadLimit(4 << 20)
	ctx := r.Context()

	var hello protocol.Msg
	hctx, hcancel := context.WithTimeout(ctx, 10*time.Second) // a silent connection must not linger
	err = readMsg(hctx, c, &hello)
	hcancel()
	if err != nil || hello.Type != protocol.TypeHello {
		c.Close(websocket.StatusPolicyViolation, "expected hello")
		return
	}
	if reason := validHello(hello); reason != "" {
		c.Close(websocket.StatusPolicyViolation, reason)
		return
	}
	proto := hello.HelloVersion()
	if proto < g.cfg.NodeProtocolMin || proto > g.cfg.NodeProtocolMax {
		// After authentication: only a node with a valid token learns the range. The reason
		// fits a close frame (at most 123 bytes).
		g.log.Warn("node refused: unsupported protocol version", "node", hello.NodeID, "operator", operator,
			"protocol", proto, "supported", protoRange(g.cfg.NodeProtocolMin, g.cfg.NodeProtocolMax), "version", cmp.Or(cleanAgent(hello.Agent), "unknown"))
		c.Close(protocol.CloseUnsupportedProtocol, unsupportedReason(proto, g.cfg.NodeProtocolMin, g.cfg.NodeProtocolMax))
		return
	}
	if tok.NodeID != "" && tok.NodeID != hello.NodeID {
		c.Close(websocket.StatusPolicyViolation, "this node token belongs to another node id")
		return
	}
	if isAnon(account) && len(hello.Models) != 1 {
		// Admission (modelcheck.go) checks one model: a node without an account serves only that one.
		c.Close(websocket.StatusPolicyViolation, "a node without an account announces exactly one model")
		return
	}
	if account == "" && (validNodeID(hello.NodeID) || validPilotID(hello.NodeID)) { // the gateway assigns these to account nodes and pilots
		c.Close(websocket.StatusPolicyViolation, "node id is reserved")
		return
	}
	g.mu.Lock()
	reserved := g.idReservedLocked(token, hello.NodeID)
	g.mu.Unlock()
	if reserved {
		c.Close(websocket.StatusPolicyViolation, "node id is reserved")
		return
	}
	n := &nodeConn{id: hello.NodeID, operator: operator, token: token, account: account, ip: ip, anon: isAnon(account),
		agent: cleanAgent(hello.Agent), proto: proto, since: time.Now(), models: hello.Models, conn: c, pending: map[string]*pendingReq{}, lastSeen: time.Now(), closed: make(chan struct{})}
	if pl != nil {
		n.pilot, n.connector, n.extUser, n.deadline, n.scienceOnly = pl.pilot.ID, pl.conn.ID, pl.pilot.User, pl.pilot.Deadline, pl.scienceOnly
	}
	if err := n.send(ctx, protocol.Msg{Type: protocol.TypeWelcome, GatewayID: g.cfg.GatewayID, Protocol: protocol.LegacyVersion,
		ProtocolVersion: proto, ProtocolMin: g.cfg.NodeProtocolMin, ProtocolMax: g.cfg.NodeProtocolMax}); err != nil {
		return
	}
	var first time.Time
	switch {
	case pl != nil:
		first = pl.first
	case account != "":
		first, n.unconfirmed, _ = g.accounts.nodeLive(keyHash(token))
	}
	if n.anon && g.mc != nil { // admitted before (the model check keeps its record across reconnects)
		n.admitted = g.mc.admitted(n)
	}
	if pl != nil {
		// The record decides before the pilot is routable: one reported, ended or past its deadline
		// since authPilot may not connect. With no live connection here, a record still "connected"
		// is one whose disconnect was not written, and the pilot may continue it.
		g.mu.Lock()
		live := g.node[n.id] != nil
		g.mu.Unlock()
		if live {
			c.Close(websocket.StatusPolicyViolation, "this pilot is already connected")
			return
		}
		if ok, err := g.sci.pilotConnected(n.pilot, n.agent, g.pilotNow()); err != nil {
			c.Close(websocket.StatusTryAgainLater, "pilot records unavailable, try again")
			return
		} else if !ok {
			c.Close(websocket.StatusPolicyViolation, "this pilot has ended")
			return
		}
	}
	// refuse closes a connection the checks below turn away; a pilot's record, marked connected
	// above, goes back to disconnected unless another connection of it holds it.
	refuse := func(why string, revert bool) {
		if pl != nil && revert {
			if err := g.sci.pilotDisconnected(n.pilot, "refused", time.Now(), g.pilotNow()); err != nil {
				g.log.Error("pilot refusal not recorded", "pilot", n.pilot, "err", err)
			}
		}
		c.Close(websocket.StatusPolicyViolation, why)
	}
	g.mu.Lock()
	// Reload, a revocation or a ban may have changed the token while we waited for hello:
	// decide with the token as it is now, under the same lock that registers the node.
	var still bool
	switch {
	case pl != nil: // its record was checked when it was marked connected, above
		still = true
		n.probEnd = first.Add(g.acfg.probation)
	case account == "":
		var cur NodeToken
		cur, still = g.tokens[token]
		still = still && (cur.NodeID == "" || cur.NodeID == hello.NodeID)
		n.operator = cur.Operator
	default:
		_, _, still = g.accounts.nodeToken(token)
		n.probEnd = first.Add(g.acfg.probation)
	}
	if !still || g.idReservedLocked(token, hello.NodeID) {
		g.mu.Unlock()
		refuse("node token revoked or changed", true)
		return
	}
	n.rules = g.rulesFor(account, g.tokens[token])
	old := g.node[n.id]
	if old != nil && old.token != token { // only the node's own token may replace it
		g.mu.Unlock()
		refuse("node id is in use", false)
		return
	}
	if old != nil && pl != nil { // a pilot's credential serves one connection at a time
		g.mu.Unlock()
		refuse("this pilot is already connected", false) // the live one holds the record
		return
	}
	if old == nil && g.connectionsLocked(token) >= g.cfg.MaxNodesPerToken {
		g.mu.Unlock()
		refuse("too many nodes for this token", true)
		return
	}
	if account != "" && !n.anon && g.nodesFromLocked(ip, n.id, false) >= maxNodesPerIP {
		g.mu.Unlock()
		refuse(fmt.Sprintf("at most %d nodes per address", maxNodesPerIP), true)
		return
	}
	if n.anon && g.nodesFromLocked(ip, n.id, true) >= maxAnonPerIP {
		g.mu.Unlock()
		refuse(fmt.Sprintf("at most %d nodes without an account per address; sign in to add more", maxAnonPerIP), true)
		return
	}
	n.held = g.nodeHeld(n.id)
	n.registering = pl != nil
	g.node[n.id] = n
	now := time.Now()
	flaps := 0
	if old != nil {
		flaps = g.noteReplaceLocked(n.id, old.since, now)
	}
	for name, last := range g.known { // forget models nobody has offered for a long time
		if now.Sub(last) > knownFor {
			delete(g.known, name)
		}
	}
	g.rememberLocked(n.models, now)
	g.mu.Unlock()
	if pl != nil {
		// A report or the deadline may have ended it while it registered: dropPilot ran before the
		// socket was here to close. It is registered but not routable (registering) until the record,
		// read now that dropPilot can find the socket, says it is still connected.
		if g.pilotRegistered != nil {
			g.pilotRegistered(n.pilot)
		}
		// Its account, link and switch are read again too: authPilot's answer is from before hello.
		p, err := g.sci.pilot("", n.pilot)
		first, chatOff, live := g.accounts.pilotAccountState(n.account, n.connector, n.extUser)
		c := g.connector(n.connector)
		switch {
		case err != nil:
			n.conn.Close(websocket.StatusTryAgainLater, "pilot records unavailable, try again")
		case p.State != PilotConnected:
			n.conn.Close(websocket.StatusPolicyViolation, "this pilot has ended")
		case !live || c == nil:
			n.conn.Close(websocket.StatusPolicyViolation, "this pilot's account is closed")
		default:
			g.mu.Lock()
			n.probEnd, n.scienceOnly = first.Add(g.acfg.probation), pilotScienceOnly(c, n.extUser, chatOff)
			n.registering = false
			g.mu.Unlock()
		}
	}
	if old != nil { // reconnect replaces the stale connection; its close handshake may stall
		old.conn.Close(websocket.StatusGoingAway, "replaced by new connection")
	}
	g.signal() // waiting requests may now find a slot
	g.log.Info("node connected", "node", n.id, "operator", n.operator, "models", len(n.models), "version", cmp.Or(n.agent, "unknown"),
		"protocol", n.proto, "anonymous", n.anon)
	g.noteAnonSeen(n)
	if account != "" && pl == nil { // an account's node or one without an account (pilots and config tokens are not counted)
		g.noteFirstConnect(n)
	}
	if flaps > 0 {
		g.log.Warn("node connections keep replacing each other: two clients may be running with the same node token and id",
			"node", n.id, "operator", n.operator, "replaced", flaps, "same_address", old.ip == n.ip)
	}

	defer func() {
		g.mu.Lock()
		if g.node[n.id] == n {
			delete(g.node, n.id)
			delete(g.flaps, n.id)
		}
		g.rememberLocked(n.models, time.Now()) // "known" for a while after its last node leaves
		g.mu.Unlock()
		close(n.closed)
		g.signal()
		// In-flight requests see n.closed and fail over or report the error themselves.
		g.log.Info("node disconnected", "node", n.id)
		g.noteAnonSeen(n)
	}()
	err = g.readLoop(ctx, n)
	if pl != nil {
		reason := pilotClose(err)
		if err := g.sci.pilotDisconnected(n.pilot, reason, n.since, g.pilotNow()); err != nil {
			g.log.Error("pilot disconnect not recorded", "pilot", n.pilot, "err", err)
		}
		g.bufByTok.Delete(token) // a pilot's credential never comes back after its end; nothing to keep
		g.speeds.forget(n.pilot)
		g.log.Info("pilot disconnected", "pilot", n.pilot, "reason", reason)
	}
}

// Two clients with one node token and id replace each other's connection in turn, each
// reconnecting after it is replaced (staging, 2026-10-20). A connection replaced within
// flapWindow of connecting counts as quick; flapCount quick replacements in a row log a
// warning, at most once per flapQuiet per node id.
const (
	flapWindow = 2 * time.Minute
	flapCount  = 3
	flapQuiet  = 10 * time.Minute
)

type flapState struct {
	quick  int       // quick replacements since the last warning
	warned time.Time // last warning
}

// noteReplaceLocked records that node id's connection from oldSince was replaced now, and
// returns how many quick replacements to warn about (0: no warning). Caller holds g.mu.
func (g *Gateway) noteReplaceLocked(id string, oldSince, now time.Time) int {
	f := g.flaps[id]
	if now.Sub(oldSince) >= flapWindow { // it had lasted: an ordinary reconnect
		if f != nil {
			f.quick = 0
		}
		return 0
	}
	if f == nil {
		f = &flapState{}
		g.flaps[id] = f
	}
	f.quick++
	if f.quick < flapCount || (!f.warned.IsZero() && now.Sub(f.warned) < flapQuiet) {
		return 0
	}
	n := f.quick
	f.quick, f.warned = 0, now
	return n
}

// agentRe is what a node's software version may look like ("aisathome-node/0.2.0",
// "aisathome-node/dev"); anything else is not recorded.
var agentRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+/-]{0,63}$`)

func cleanAgent(s string) string {
	if agentRe.MatchString(s) {
		return s
	}
	return ""
}

// idReservedLocked reports whether node id is bound to a token other than token: such an id
// cannot be squatted. Caller holds g.mu.
func (g *Gateway) idReservedLocked(token, id string) bool {
	for t, other := range g.tokens {
		if t != token && other.NodeID == id {
			return true
		}
	}
	return false
}

// readLoop reads a node's messages until the connection ends: heartbeats update its state,
// answers go to the requests waiting for them.
func (g *Gateway) readLoop(ctx context.Context, n *nodeConn) error {
	c := n.conn
	badFrames := 0
	for {
		rctx, cancel := context.WithTimeout(ctx, g.cfg.HeartbeatTimeout)
		_, b, err := c.Read(rctx)
		cancel()
		if err != nil {
			return err
		}
		var m protocol.Msg
		if json.Unmarshal(b, &m) != nil { // skip a bad frame; only a run of them ends the node
			if badFrames++; badFrames > 10 {
				c.Close(websocket.StatusPolicyViolation, "malformed frames")
				return nil
			}
			continue
		}
		badFrames = 0
		if m.Type == protocol.TypeChunk && len(m.Data) > maxChunkBytes {
			c.Close(websocket.StatusPolicyViolation, "chunk too large")
			return nil
		}
		n.mu.Lock()
		n.lastSeen = time.Now()
		resumed := false
		if m.Type == protocol.TypeHeartbeat {
			n.active = m.Active
			resumed = n.paused && !m.Paused
			n.paused = m.Paused
		}
		p := n.pending[m.ReqID]
		n.mu.Unlock()
		if resumed {
			g.signal() // waiting requests may use it again
		}
		if p != nil && (m.Type == protocol.TypeDone || m.Type == protocol.TypeError) {
			p.end()
		}
		if p != nil && (m.Type == protocol.TypeChunk || m.Type == protocol.TypeDone || m.Type == protocol.TypeError) {
			p.deliver(m) // outside the lock: a slow client must not block other requests
		}
	}
}

// maxCtx bounds the context a node may announce.
const maxCtx = 4 << 20

// maxChunkBytes bounds one streamed chunk (a token or a few, plus JSON framing). Larger
// frames are a malicious or broken node; they would let one request pin gigabytes.
const maxChunkBytes = 64 << 10

// connectionsLocked counts connected nodes using token. Caller holds g.mu.
func (g *Gateway) connectionsLocked(token string) int {
	c := 0
	for _, n := range g.node {
		if n.token == token {
			c++
		}
	}
	return c
}

// validHello bounds what a node may announce, so one node cannot bloat routing state.
func validHello(h protocol.Msg) string {
	if h.NodeID == "" || len(h.NodeID) > 64 {
		return "node id must be 1-64 characters"
	}
	if len(h.Models) == 0 || len(h.Models) > 16 {
		return "a node announces 1-16 models"
	}
	for _, m := range h.Models {
		if m.Name == "" || len(m.Name) > 128 || len(m.Roles) > 8 || m.Slots < 1 || m.Slots > 64 || m.Ctx < 0 || m.Ctx > maxCtx {
			return "invalid model announcement"
		}
		for _, r := range m.Roles {
			if r == "" || len(r) > 32 {
				return "invalid role"
			}
		}
	}
	return ""
}

// cancelOnNode tells a node to stop a request, without waiting long on a stuck connection.
func cancelOnNode(n *nodeConn, reqID string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = n.send(ctx, protocol.Msg{Type: protocol.TypeCancel, ReqID: reqID})
}

func readMsg(ctx context.Context, c *websocket.Conn, m *protocol.Msg) error {
	_, b, err := c.Read(ctx)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, m)
}

// unsupportedReason is the close reason for a node whose protocol version the gateway does
// not accept: what the volunteer should do, in at most 123 bytes.
func unsupportedReason(proto, lo, hi int) string {
	what := "update the node agent"
	if proto > hi {
		what = "this gateway does not speak it yet"
	}
	return fmt.Sprintf("node protocol version %d not supported (gateway supports %s): %s", proto, protoRange(lo, hi), what)
}

func protoRange(lo, hi int) string {
	if lo == hi {
		return fmt.Sprint(lo)
	}
	return fmt.Sprintf("%d-%d", lo, hi)
}

// NodeProtocol returns the protocol version a connected node speaks (false: not connected).
func (g *Gateway) NodeProtocol(id string) (int, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	n := g.node[id]
	if n == nil {
		return 0, false
	}
	return n.proto, true
}
