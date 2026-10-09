// Package gateway is the AIs@home project server: an OpenAI-compatible API for users, a
// registry of nodes connected outward over WebSocket, routing with failover, and metering.
//
// SPDX-License-Identifier: AGPL-3.0-or-later
package gateway

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"

	"github.com/aisathome/aisathome/protocol"
)

// Config holds gateway settings. In M1 keys and tokens come from a config file; accounts and
// payments come later.
type Config struct {
	GatewayID  string               `json:"gateway_id"`
	APIKeys    map[string]string    `json:"api_keys"`    // API key → user id (no limits)
	Keys       map[string]KeyInfo   `json:"keys"`        // API key → user and limits
	NodeTokens map[string]NodeToken `json:"node_tokens"` // node token → operator (and node id)
	UsageLog   string               `json:"usage_log"`   // JSONL file, one line per request
	// QueueTimeoutSec is how long a request waits for a free slot, or for a node serving a
	// recently seen model to (re)connect, before failing with 503. Default 30.
	QueueTimeoutSec int `json:"queue_timeout_seconds"`
	// MaxQueued caps the requests waiting at once; more fail at once with 503. Default 64.
	MaxQueued int `json:"max_queued"`
	// DefaultMaxTokens is applied when a request sets no max_tokens/max_completion_tokens,
	// so one request cannot generate (or think) until a volunteer's context is full.
	// Default 16384; -1 disables it.
	DefaultMaxTokens int `json:"default_max_tokens"`
	// FirstChunkTimeoutSec and IdleTimeoutSec end a request whose node stalls (before the
	// first chunk, and between chunks); before any output it is retried on another node.
	// Slightly longer than the node agent's own engine timeouts. Defaults 330 and 150.
	FirstChunkTimeoutSec int `json:"first_chunk_timeout_seconds"`
	IdleTimeoutSec       int `json:"idle_timeout_seconds"`
	// HeartbeatTimeout drops nodes that stay silent for longer than this.
	HeartbeatTimeout time.Duration `json:"-"`
}

// NodeToken authorises node connections. In JSON it is either a plain operator id (any
// node id may use the token) or {"operator": "...", "node_id": "..."} to bind it to one node.
type NodeToken struct {
	Operator string `json:"operator"`
	NodeID   string `json:"node_id,omitempty"`
}

// UnmarshalJSON accepts both the plain-string and the object form.
func (t *NodeToken) UnmarshalJSON(b []byte) error {
	var s string
	if json.Unmarshal(b, &s) == nil {
		*t = NodeToken{Operator: s}
		return nil
	}
	type plain NodeToken
	return json.Unmarshal(b, (*plain)(t))
}

// knownFor is how long a model or role counts as "recently served", so that requests for it
// wait for a node instead of failing as unknown.
const knownFor = 24 * time.Hour

// startupGrace: right after a (re)start the gateway knows no models yet while its nodes
// reconnect, so while no node is connected, requests for any model wait instead of
// failing as unknown.
const startupGrace = 2 * time.Minute

// Gateway routes user requests to connected nodes.
type Gateway struct {
	cfg    Config
	log    *slog.Logger
	mu     sync.Mutex
	node   map[string]*nodeConn // by node id
	rr     int                  // round-robin cursor
	tokens map[string]NodeToken // guarded by mu; replaced by Reload
	known  map[string]time.Time // model or role → last time a node offered it; guarded by mu
	wake   chan struct{}        // closed and replaced whenever capacity may have changed
	queued atomic.Int32
	start  time.Time
	use    *usageWriter
	keys   atomic.Pointer[keyStoreBox]
	lim    *limiter
}

// keyStoreBox lets the key store be swapped atomically (Reload, SetKeyStore).
type keyStoreBox struct{ KeyStore }

// New creates a gateway.
func New(cfg Config, log *slog.Logger) (*Gateway, error) {
	if cfg.HeartbeatTimeout == 0 {
		cfg.HeartbeatTimeout = 45 * time.Second
	}
	if cfg.QueueTimeoutSec == 0 {
		cfg.QueueTimeoutSec = 30
	}
	if cfg.MaxQueued == 0 {
		cfg.MaxQueued = 64
	}
	if cfg.DefaultMaxTokens == 0 {
		cfg.DefaultMaxTokens = 16384
	}
	if cfg.FirstChunkTimeoutSec == 0 {
		cfg.FirstChunkTimeoutSec = 330
	}
	if cfg.IdleTimeoutSec == 0 {
		cfg.IdleTimeoutSec = 150
	}
	uw, err := newUsageWriter(cfg.UsageLog)
	if err != nil {
		return nil, err
	}
	g := &Gateway{cfg: cfg, log: log, node: map[string]*nodeConn{}, tokens: copyTokens(cfg.NodeTokens),
		known: map[string]time.Time{}, wake: make(chan struct{}), use: uw, lim: newLimiter(), start: time.Now()}
	g.keys.Store(&keyStoreBox{staticKeys(cfg)})
	return g, nil
}

func staticKeys(cfg Config) StaticKeys {
	keys := StaticKeys{}
	for k, u := range cfg.APIKeys {
		keys[k] = KeyInfo{User: u}
	}
	for k, info := range cfg.Keys {
		keys[k] = info
	}
	return keys
}

func copyTokens(in map[string]NodeToken) map[string]NodeToken {
	out := make(map[string]NodeToken, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

// SetKeyStore replaces the key store (e.g. with an operator's accounts service).
func (g *Gateway) SetKeyStore(ks KeyStore) { g.keys.Store(&keyStoreBox{ks}) }

// Reload applies new API keys and node tokens without dropping connections. Nodes whose
// token was revoked, or is now bound to another node id, are disconnected. A custom key
// store set with SetKeyStore is kept; only the built-in static keys are replaced.
func (g *Gateway) Reload(cfg Config) {
	if _, static := g.keys.Load().KeyStore.(StaticKeys); static {
		g.keys.Store(&keyStoreBox{staticKeys(cfg)})
	}
	g.mu.Lock()
	g.tokens = copyTokens(cfg.NodeTokens)
	var revoked []*nodeConn
	for _, n := range g.node {
		if t, ok := g.tokens[n.token]; !ok || (t.NodeID != "" && t.NodeID != n.id) {
			revoked = append(revoked, n)
		}
	}
	g.mu.Unlock()
	for _, n := range revoked {
		g.log.Info("node token revoked, disconnecting", "node", n.id)
		n.conn.Close(websocket.StatusPolicyViolation, "node token revoked")
	}
}

// signal wakes requests waiting for capacity.
func (g *Gateway) signal() {
	g.mu.Lock()
	close(g.wake)
	g.wake = make(chan struct{})
	g.mu.Unlock()
}

// NodeCount returns the number of connected nodes.
func (g *Gateway) NodeCount() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.node)
}

// Handler returns the HTTP routes.
func (g *Gateway) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/chat/completions", g.handleChat)
	mux.HandleFunc("GET /v1/models", g.handleModels)
	mux.HandleFunc("GET /v1/node/connect", g.handleNodeConnect)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { fmt.Fprintln(w, "ok") })
	return mux
}

// ---------------------------------------------------------------- nodes

type nodeConn struct {
	id       string
	operator string
	token    string // to disconnect the node if its token is revoked
	models   []protocol.ModelInfo
	conn     *websocket.Conn
	wmu      sync.Mutex // serialises writes
	mu       sync.Mutex
	pending  map[string]*pendingReq
	inflight int // slots reserved by pick, released when run ends; guarded by mu
	active   int
	lastSeen time.Time
	closed   chan struct{}
}

// pendingReq is one request in flight on a node. The reader delivers messages on ch; the
// request handler closes done when it stops listening, so delivery never blocks forever.
type pendingReq struct {
	ch   chan protocol.Msg
	done chan struct{}
}

func (p *pendingReq) deliver(m protocol.Msg) {
	select {
	case p.ch <- m:
	case <-p.done:
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
	g.mu.Lock()
	tok, ok := g.tokens[token]
	g.mu.Unlock()
	if !ok {
		http.Error(w, "unknown node token", http.StatusUnauthorized)
		return
	}
	operator := tok.Operator
	c, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	c.SetReadLimit(4 << 20)
	ctx := r.Context()

	var hello protocol.Msg
	if err := readMsg(ctx, c, &hello); err != nil || hello.Type != protocol.TypeHello || hello.NodeID == "" {
		c.Close(websocket.StatusPolicyViolation, "expected hello")
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
	n := &nodeConn{id: hello.NodeID, operator: operator, token: token, models: hello.Models, conn: c,
		pending: map[string]*pendingReq{}, lastSeen: time.Now(), closed: make(chan struct{})}
	if err := n.send(ctx, protocol.Msg{Type: protocol.TypeWelcome, GatewayID: g.cfg.GatewayID, Protocol: protocol.Version}); err != nil {
		return
	}
	g.mu.Lock()
	if old := g.node[n.id]; old != nil { // reconnect replaces the stale connection
		old.conn.Close(websocket.StatusGoingAway, "replaced by new connection")
	}
	g.node[n.id] = n
	now := time.Now()
	for _, m := range n.models {
		g.known[m.Name] = now
		for _, role := range m.Roles {
			g.known[role] = now
		}
	}
	g.mu.Unlock()
	g.signal() // waiting requests may now find a slot
	g.log.Info("node connected", "node", n.id, "operator", operator, "models", len(n.models))

	defer func() {
		g.mu.Lock()
		if g.node[n.id] == n {
			delete(g.node, n.id)
		}
		now := time.Now() // a model stays "known" for a while after its last node leaves
		for _, m := range n.models {
			g.known[m.Name] = now
			for _, role := range m.Roles {
				g.known[role] = now
			}
		}
		g.mu.Unlock()
		close(n.closed)
		g.signal()
		// In-flight requests see n.closed and fail over or report the error themselves.
		g.log.Info("node disconnected", "node", n.id)
	}()

	for {
		rctx, cancel := context.WithTimeout(ctx, g.cfg.HeartbeatTimeout)
		var m protocol.Msg
		err := readMsg(rctx, c, &m)
		cancel()
		if err != nil {
			return
		}
		n.mu.Lock()
		n.lastSeen = time.Now()
		if m.Type == protocol.TypeHeartbeat {
			n.active = m.Active
		}
		p := n.pending[m.ReqID]
		n.mu.Unlock()
		if p != nil && (m.Type == protocol.TypeChunk || m.Type == protocol.TypeDone || m.Type == protocol.TypeError) {
			p.deliver(m) // outside the lock: a slow client must not block other requests
		}
	}
}

// pick chooses a node serving the requested model or role that has a free slot, round-robin.
// It returns the node and the local model name to request. exclude holds connections that
// already failed this request; a node that reconnected is a new connection and may be tried.
func (g *Gateway) pick(want string, exclude map[*nodeConn]bool) (*nodeConn, string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	ids := make([]string, 0, len(g.node))
	for id := range g.node {
		ids = append(ids, id)
	}
	sortStrings(ids)
	for i := 0; i < len(ids); i++ {
		n := g.node[ids[(g.rr+i)%len(ids)]]
		if exclude[n] {
			continue
		}
		for _, m := range n.models {
			if m.Name != want && !contains(m.Roles, want) {
				continue
			}
			// Reserve the slot here, under the lock: checking now and registering the
			// request later lets concurrent requests oversubscribe the node.
			n.mu.Lock()
			free := n.inflight < m.Slots
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

// Why a request could not be placed on a node.
const (
	unknownModel = "unknown"  // no node offered this model or role recently
	queueFull    = "full"     // too many requests already waiting
	timedOut     = "timeout"  // waited QueueTimeoutSec without a free slot
	clientGone   = "canceled" // the client disconnected while waiting
)

// pickWait is pick, but waits until deadline for capacity: a free slot, or a node serving a
// recently seen model (re)connecting. Unknown models fail at once.
func (g *Gateway) pickWait(ctx context.Context, want string, exclude map[*nodeConn]bool,
	deadline time.Time) (*nodeConn, string, string) {
	waiting := false
	defer func() {
		if waiting {
			g.queued.Add(-1)
		}
	}()
	for {
		g.mu.Lock()
		wake := g.wake // taken before pick, so a signal in between is not lost
		last, seen := g.known[want]
		g.mu.Unlock()
		if n, model := g.pick(want, exclude); n != nil {
			return n, model, ""
		}
		// Unknown models fail at once, except just after a restart while no node is back yet.
		restarting := time.Since(g.start) < startupGrace && g.NodeCount() == 0
		if (!seen || time.Since(last) > knownFor) && !restarting {
			if g.servesNow(want) == 0 {
				return nil, "", unknownModel
			}
		}
		// A retry waits only if another connected node serves the model (it is just busy);
		// otherwise there is nothing to wait for.
		if len(exclude) > 0 && g.servesNowExcluding(want, exclude) == 0 {
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

// servesNow counts connected nodes offering the model or role.
func (g *Gateway) servesNow(want string) int { return g.servesNowExcluding(want, nil) }

// servesNowExcluding is servesNow without the given connections.
func (g *Gateway) servesNowExcluding(want string, exclude map[*nodeConn]bool) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	c := 0
	for _, n := range g.node {
		if exclude[n] {
			continue
		}
		for _, m := range n.models {
			if m.Name == want || contains(m.Roles, want) {
				c++
				break
			}
		}
	}
	return c
}

// unavailable answers a request that could not be placed, with an honest reason.
func (g *Gateway) unavailable(w http.ResponseWriter, want, why string) {
	switch why {
	case clientGone:
		return
	case unknownModel:
		apiError(w, http.StatusNotFound, fmt.Sprintf("model %q is not served on this network", want))
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
		w.Header().Set("Retry-After", "10")
		apiError(w, http.StatusServiceUnavailable, fmt.Sprintf("all nodes serving %q are busy; try again shortly", want))
	}
}

// ---------------------------------------------------------------- user API

func (g *Gateway) user(r *http.Request) (string, bool) {
	k, ok := g.keys.Load().Lookup(r.Context(), bearer(r))
	return k.User, ok
}

func bearer(r *http.Request) string {
	return strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
}

func (g *Gateway) handleModels(w http.ResponseWriter, r *http.Request) {
	if _, ok := g.user(r); !ok {
		apiError(w, http.StatusUnauthorized, "invalid API key")
		return
	}
	// One entry per model or role, with the largest context any connected node offers for
	// it (clients such as opencode size their compaction from context_length).
	var ids []string
	ctx := map[string]int{}
	g.mu.Lock()
	for _, n := range g.node {
		for _, m := range n.models {
			for _, id := range append([]string{m.Name}, m.Roles...) {
				if _, ok := ctx[id]; !ok {
					ids = append(ids, id)
				}
				ctx[id] = max(ctx[id], m.Ctx)
			}
		}
	}
	g.mu.Unlock()
	data := []map[string]any{}
	for _, id := range ids {
		e := map[string]any{"id": id, "object": "model", "owned_by": "AIs@home"}
		if ctx[id] > 0 {
			e["context_length"] = ctx[id]
		}
		data = append(data, e)
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": data})
}

func (g *Gateway) handleChat(w http.ResponseWriter, r *http.Request) {
	key := bearer(r)
	info, ok := g.keys.Load().Lookup(r.Context(), key)
	if !ok {
		apiError(w, http.StatusUnauthorized, "invalid API key")
		return
	}
	user := info.User
	release, reason := g.lim.acquire(key, info, time.Now())
	if release == nil {
		apiError(w, http.StatusTooManyRequests, reason)
		return
	}
	defer release()
	var body map[string]any
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<20)).Decode(&body); err != nil {
		apiError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	want, _ := body["model"].(string)
	clientStream, _ := body["stream"].(bool)
	if body["max_tokens"] == nil && body["max_completion_tokens"] == nil && g.cfg.DefaultMaxTokens > 0 {
		body["max_tokens"] = g.cfg.DefaultMaxTokens
	}

	deadline := time.Now().Add(time.Duration(g.cfg.QueueTimeoutSec) * time.Second)
	tried := map[*nodeConn]bool{}
	for attempt := 0; attempt < 2; attempt++ { // one retry before any output, on another connection
		n, model, why := g.pickWait(r.Context(), want, tried, deadline)
		if n == nil {
			if attempt == 0 {
				g.unavailable(w, want, why)
			} else if why != clientGone {
				apiError(w, http.StatusBadGateway, "request failed on all available nodes")
			}
			return
		}
		tried[n] = true
		started, err := g.run(w, r, user, n, model, want, body, clientStream)
		if err == nil || started {
			return // success, or failure after output began: the client sees the error
		}
		if isContextOverflow(err.Error()) { // the request itself is too large: no retry
			contextError(w, err.Error())
			return
		}
		g.log.Warn("node failed before output, retrying", "node", n.id, "err", err)
	}
	apiError(w, http.StatusBadGateway, "request failed on all available nodes")
}

// run sends one request to a node and relays the result. started reports whether any
// output reached the client (then a retry is no longer possible).
func (g *Gateway) run(w http.ResponseWriter, r *http.Request, user string, n *nodeConn, model, want string,
	body map[string]any, clientStream bool) (started bool, err error) {
	reqID := newID()
	body["model"] = model
	body["stream"] = true
	body["stream_options"] = map[string]any{"include_usage": true}
	raw, _ := json.Marshal(body)

	p := &pendingReq{ch: make(chan protocol.Msg, 64), done: make(chan struct{})}
	ch := p.ch
	n.mu.Lock()
	n.pending[reqID] = p
	n.mu.Unlock()
	defer func() {
		close(p.done)
		n.mu.Lock()
		delete(n.pending, reqID)
		n.inflight-- // reserved by pick
		n.mu.Unlock()
		g.signal() // a slot is free again
	}()

	t0 := time.Now()
	if err := n.send(r.Context(), protocol.Msg{Type: protocol.TypeRequest, ReqID: reqID, Body: raw}); err != nil {
		return false, err
	}

	// relayed counts chunks carrying generated text, as seen by the gateway itself. It meters
	// requests that end early (the node sends no usage then) and cross-checks node-reported
	// completion tokens (llama.cpp streams about one token per chunk).
	relayed := 0
	record := func(outcome string, u protocol.Usage, estimated bool) {
		u.DurationMs = time.Since(t0).Milliseconds()
		g.use.record(usageRecord{Time: time.Now().UTC(), User: user, Node: n.id, Operator: n.operator,
			Model: model, Requested: want, Usage: u, ReqID: reqID, Outcome: outcome, Relayed: relayed,
			Estimated: estimated})
	}
	partial := func(outcome string) {
		if relayed > 0 { // the node did visible work; bill an estimate rather than nothing
			record(outcome, protocol.Usage{CompletionTokens: relayed}, true)
		}
	}

	var sse *bufio.Writer
	var flusher http.Flusher
	var collected []json.RawMessage
	stall := time.NewTimer(time.Duration(g.cfg.FirstChunkTimeoutSec) * time.Second)
	defer stall.Stop()
	for {
		select {
		case <-stall.C: // the node went silent (e.g. a hung engine): give up on it
			_ = n.send(context.Background(), protocol.Msg{Type: protocol.TypeCancel, ReqID: reqID})
			partial("failed")
			g.log.Warn("node stalled", "node", n.id, "req", reqID, "started", started)
			if started {
				fmt.Fprintf(sse, "data: %s\n\n", mustJSON(map[string]any{"error": map[string]any{"message": "the node stopped responding"}}))
				sse.Flush()
			}
			return started, errors.New("node stalled")
		case <-r.Context().Done(): // client went away: tell the node to stop
			_ = n.send(context.Background(), protocol.Msg{Type: protocol.TypeCancel, ReqID: reqID})
			partial("cancelled")
			return started, r.Context().Err()
		case <-n.closed:
			partial("failed")
			return started, errors.New("node disconnected")
		case m := <-ch:
			stall.Reset(time.Duration(g.cfg.IdleTimeoutSec) * time.Second)
			switch m.Type {
			case protocol.TypeChunk:
				if hasText(m.Data) {
					relayed++
				}
				if !clientStream {
					collected = append(collected, m.Data)
					continue
				}
				if sse == nil {
					w.Header().Set("Content-Type", "text/event-stream")
					w.Header().Set("Cache-Control", "no-cache")
					w.WriteHeader(http.StatusOK)
					sse = bufio.NewWriter(w)
					flusher, _ = w.(http.Flusher)
				}
				started = true
				fmt.Fprintf(sse, "data: %s\n\n", m.Data)
				sse.Flush()
				if flusher != nil {
					flusher.Flush()
				}
			case protocol.TypeError:
				if started {
					e := map[string]any{"message": m.Error}
					if isContextOverflow(m.Error) {
						e["type"], e["code"] = "invalid_request_error", "context_length_exceeded"
					}
					fmt.Fprintf(sse, "data: %s\n\n", mustJSON(map[string]any{"error": e}))
					sse.Flush()
				}
				partial("failed")
				return started, errors.New(m.Error)
			case protocol.TypeDone:
				u := protocol.Usage{}
				if m.Usage != nil {
					u = *m.Usage
				}
				record("ok", u, false)
				u.DurationMs = time.Since(t0).Milliseconds()
				if clientStream {
					if sse == nil { // empty stream: still answer properly
						w.Header().Set("Content-Type", "text/event-stream")
						w.WriteHeader(http.StatusOK)
						sse = bufio.NewWriter(w)
					}
					fmt.Fprint(sse, "data: [DONE]\n\n")
					sse.Flush()
				} else {
					writeJSON(w, http.StatusOK, assemble(reqID, model, collected, u))
				}
				return true, nil
			}
		}
	}
}

// assemble turns streamed chunks into one non-streaming chat.completion object.
func assemble(id, model string, chunks []json.RawMessage, u protocol.Usage) map[string]any {
	var content, reasoning strings.Builder
	finish := "stop"
	type call struct {
		id, typ, name string
		args          strings.Builder
	}
	var calls []*call // by stream index; arguments arrive in pieces
	for _, c := range chunks {
		var ch struct {
			Choices []struct {
				Delta struct {
					Content          string `json:"content"`
					ReasoningContent string `json:"reasoning_content"`
					ToolCalls        []struct {
						Index    int    `json:"index"`
						ID       string `json:"id"`
						Type     string `json:"type"`
						Function struct {
							Name      string `json:"name"`
							Arguments string `json:"arguments"`
						} `json:"function"`
					} `json:"tool_calls"`
				} `json:"delta"`
				FinishReason *string `json:"finish_reason"`
			} `json:"choices"`
		}
		if json.Unmarshal(c, &ch) != nil {
			continue
		}
		for _, choice := range ch.Choices {
			content.WriteString(choice.Delta.Content)
			reasoning.WriteString(choice.Delta.ReasoningContent)
			for _, tc := range choice.Delta.ToolCalls {
				if tc.Index < 0 || tc.Index > 255 {
					continue
				}
				for len(calls) <= tc.Index {
					calls = append(calls, &call{typ: "function"})
				}
				k := calls[tc.Index]
				if tc.ID != "" {
					k.id = tc.ID
				}
				if tc.Type != "" {
					k.typ = tc.Type
				}
				if tc.Function.Name != "" {
					k.name = tc.Function.Name
				}
				k.args.WriteString(tc.Function.Arguments)
			}
			if choice.FinishReason != nil {
				finish = *choice.FinishReason
			}
		}
	}
	msg := map[string]any{"role": "assistant", "content": content.String()}
	if reasoning.Len() > 0 {
		msg["reasoning_content"] = reasoning.String()
	}
	if len(calls) > 0 {
		var tcs []any
		for _, k := range calls {
			tcs = append(tcs, map[string]any{"id": k.id, "type": k.typ,
				"function": map[string]any{"name": k.name, "arguments": k.args.String()}})
		}
		msg["tool_calls"] = tcs
		if content.Len() == 0 {
			msg["content"] = nil // as OpenAI returns it for pure tool calls
		}
	}
	return map[string]any{
		"id": "chatcmpl-" + id, "object": "chat.completion", "created": time.Now().Unix(), "model": model,
		"choices": []any{map[string]any{"index": 0, "message": msg, "finish_reason": finish}},
		"usage": map[string]any{"prompt_tokens": u.PromptTokens, "completion_tokens": u.CompletionTokens,
			"total_tokens":          u.PromptTokens + u.CompletionTokens,
			"prompt_tokens_details": map[string]any{"cached_tokens": u.CachedTokens}},
	}
}

// ---------------------------------------------------------------- metering

type usageRecord struct {
	Time      time.Time      `json:"time"`
	ReqID     string         `json:"req_id"`
	User      string         `json:"user"`
	Node      string         `json:"node"`
	Operator  string         `json:"operator"`
	Model     string         `json:"model"`
	Requested string         `json:"requested"`
	Outcome   string         `json:"outcome"`        // ok, cancelled (client left) or failed
	Relayed   int            `json:"relayed_chunks"` // text chunks the gateway relayed itself
	Estimated bool           `json:"estimated,omitempty"`
	Usage     protocol.Usage `json:"usage"` // node-reported, or estimated from relayed chunks
}

// hasText reports whether a streamed chunk carries generated text (answer or reasoning).
func hasText(chunk []byte) bool {
	var c struct {
		Choices []struct {
			Delta struct {
				Content          string `json:"content"`
				ReasoningContent string `json:"reasoning_content"`
			} `json:"delta"`
		} `json:"choices"`
	}
	if json.Unmarshal(chunk, &c) != nil {
		return false
	}
	for _, ch := range c.Choices {
		if ch.Delta.Content != "" || ch.Delta.ReasoningContent != "" {
			return true
		}
	}
	return false
}

type usageWriter struct {
	mu sync.Mutex
	f  *os.File
}

func newUsageWriter(path string) (*usageWriter, error) {
	if path == "" {
		return &usageWriter{}, nil
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, fmt.Errorf("usage log: %w", err)
	}
	return &usageWriter{f: f}, nil
}

func (u *usageWriter) record(r usageRecord) {
	if u.f == nil {
		return
	}
	b, _ := json.Marshal(r)
	u.mu.Lock()
	defer u.mu.Unlock()
	u.f.Write(append(b, '\n'))
}

// ---------------------------------------------------------------- helpers

// isContextOverflow recognises llama-server's context errors: before generation (HTTP 400,
// "exceed_context_size_error") and during it ("Context size has been exceeded").
func isContextOverflow(msg string) bool {
	m := strings.ToLower(msg)
	return strings.Contains(m, "exceed_context_size_error") || strings.Contains(m, "exceeds the available context") ||
		strings.Contains(m, "context size has been exceeded")
}

// contextError answers as OpenAI does, so clients can compact and retry.
func contextError(w http.ResponseWriter, detail string) {
	writeJSON(w, http.StatusBadRequest, map[string]any{"error": map[string]any{
		"message": "the conversation is longer than this node's context; shorten or compact it (" + truncate(detail, 200) + ")",
		"type":    "invalid_request_error", "code": "context_length_exceeded"}})
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func readMsg(ctx context.Context, c *websocket.Conn, m *protocol.Msg) error {
	_, b, err := c.Read(ctx)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, m)
}

func apiError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]any{"error": map[string]any{"message": msg, "type": http.StatusText(code)}})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func mustJSON(v any) []byte { b, _ := json.Marshal(v); return b }

func newID() string {
	b := make([]byte, 12)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func major(v string) string { return strings.SplitN(v, ".", 2)[0] }

func contains(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}

func sortStrings(xs []string) {
	for i := 1; i < len(xs); i++ {
		for j := i; j > 0 && xs[j] < xs[j-1]; j-- {
			xs[j], xs[j-1] = xs[j-1], xs[j]
		}
	}
}
