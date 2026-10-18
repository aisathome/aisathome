// Connected node sessions: registration, message delivery, and cancellation.
//
// SPDX-License-Identifier: Apache-2.0
package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/aisathome/aisathome/protocol"
	"github.com/coder/websocket"
)

// NodeCount returns the number of connected nodes.
func (g *Gateway) NodeCount() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.node)
}

type nodeConn struct {
	id       string
	operator string    // the config token's operator, or the account's pseudonym
	token    string    // to disconnect the node if its token is revoked
	trusted  bool      // from its token: the maintainer's own nodes (not "secure"; see README)
	account  string    // the account that registered it; "" for a token from the config
	ip       string    // client address it connected from (account nodes: maxNodesPerIP)
	held     bool      // stopped by a guard until the operator clears it; guarded by g.mu
	probEnd  time.Time // account nodes: no agents (requests with tools) before; guarded by g.mu
	models   []protocol.ModelInfo
	conn     *websocket.Conn
	wmu      sync.Mutex // serialises writes
	mu       sync.Mutex
	pending  map[string]*pendingReq
	inflight int // slots reserved by pick, freed once the node is finished with the request (relay.free); guarded by mu
	active   int
	paused   bool // from the last heartbeat: take no new requests; guarded by mu
	lastSeen time.Time
	closed   chan struct{}
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
	tok, account, ok := g.nodeAuth(token)
	if !ok {
		http.Error(w, "unknown node token", http.StatusUnauthorized)
		return
	}
	operator, ip := tok.Operator, clientIP(r)
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
	if major(hello.Protocol) != major(protocol.Version) {
		c.Close(websocket.StatusPolicyViolation, "protocol version "+hello.Protocol+" not supported")
		return
	}
	if tok.NodeID != "" && tok.NodeID != hello.NodeID {
		c.Close(websocket.StatusPolicyViolation, "this node token belongs to another node id")
		return
	}
	if account == "" && validNodeID(hello.NodeID) { // the gateway assigns these to account nodes
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
	n := &nodeConn{id: hello.NodeID, operator: operator, token: token, trusted: tok.Trust == "trusted", account: account, ip: ip,
		models: hello.Models, conn: c, pending: map[string]*pendingReq{}, lastSeen: time.Now(), closed: make(chan struct{})}
	if err := n.send(ctx, protocol.Msg{Type: protocol.TypeWelcome, GatewayID: g.cfg.GatewayID, Protocol: protocol.Version}); err != nil {
		return
	}
	var first time.Time
	if account != "" {
		first, _ = g.accounts.nodeLive(keyHash(token))
	}
	g.mu.Lock()
	// Reload, a revocation or a ban may have changed the token while we waited for hello:
	// decide with the token as it is now, under the same lock that registers the node.
	var still bool
	if account == "" {
		var cur NodeToken
		cur, still = g.tokens[token]
		still = still && (cur.NodeID == "" || cur.NodeID == hello.NodeID)
		n.trusted, n.operator = cur.Trust == "trusted", cur.Operator
	} else {
		_, _, still = g.accounts.nodeToken(token)
		n.probEnd = first.Add(g.acfg.probation)
	}
	if !still || g.idReservedLocked(token, hello.NodeID) {
		g.mu.Unlock()
		c.Close(websocket.StatusPolicyViolation, "node token revoked or changed")
		return
	}
	old := g.node[n.id]
	if old != nil && old.token != token { // only the node's own token may replace it
		g.mu.Unlock()
		c.Close(websocket.StatusPolicyViolation, "node id is in use")
		return
	}
	if old == nil && g.connectionsLocked(token) >= g.cfg.MaxNodesPerToken {
		g.mu.Unlock()
		c.Close(websocket.StatusPolicyViolation, "too many nodes for this token")
		return
	}
	if account != "" && g.nodesFromLocked(ip, n.id) >= maxNodesPerIP {
		g.mu.Unlock()
		c.Close(websocket.StatusPolicyViolation, fmt.Sprintf("at most %d nodes per address", maxNodesPerIP))
		return
	}
	n.held = g.nodeHeld(n.id)
	g.node[n.id] = n
	now := time.Now()
	for name, last := range g.known { // forget models nobody has offered for a long time
		if now.Sub(last) > knownFor {
			delete(g.known, name)
		}
	}
	g.rememberLocked(n.models, now)
	g.mu.Unlock()
	if old != nil { // reconnect replaces the stale connection; its close handshake may stall
		old.conn.Close(websocket.StatusGoingAway, "replaced by new connection")
	}
	g.signal() // waiting requests may now find a slot
	g.log.Info("node connected", "node", n.id, "operator", n.operator, "models", len(n.models))

	defer func() {
		g.mu.Lock()
		if g.node[n.id] == n {
			delete(g.node, n.id)
		}
		g.rememberLocked(n.models, time.Now()) // "known" for a while after its last node leaves
		g.mu.Unlock()
		close(n.closed)
		g.signal()
		// In-flight requests see n.closed and fail over or report the error themselves.
		g.log.Info("node disconnected", "node", n.id)
	}()
	g.readLoop(ctx, n)
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
func (g *Gateway) readLoop(ctx context.Context, n *nodeConn) {
	c := n.conn
	badFrames := 0
	for {
		rctx, cancel := context.WithTimeout(ctx, g.cfg.HeartbeatTimeout)
		_, b, err := c.Read(rctx)
		cancel()
		if err != nil {
			return
		}
		var m protocol.Msg
		if json.Unmarshal(b, &m) != nil { // skip a bad frame; only a run of them ends the node
			if badFrames++; badFrames > 10 {
				c.Close(websocket.StatusPolicyViolation, "malformed frames")
				return
			}
			continue
		}
		badFrames = 0
		if m.Type == protocol.TypeChunk && len(m.Data) > maxChunkBytes {
			c.Close(websocket.StatusPolicyViolation, "chunk too large")
			return
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

func major(v string) string { return strings.SplitN(v, ".", 2)[0] }
