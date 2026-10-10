// Package gateway is the AIs@home project server: an OpenAI-compatible API for users, a
// registry of nodes connected outward over WebSocket, routing with failover, and metering.
//
// SPDX-License-Identifier: Apache-2.0
package gateway

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
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
	// KeysFile holds per-user keys (stored hashed) managed with `aisathome-gateway keys`;
	// changes apply within a few seconds, without a reload. Empty = none.
	KeysFile string `json:"keys_file"`
	// StateDir keeps state that must survive a restart (daily token budgets), so a restart
	// does not hand every key a fresh day. Empty = memory only.
	StateDir string `json:"state_dir"`
	// QueueTimeoutSec is how long a request waits for a free slot, or for a node serving a
	// recently seen model to (re)connect, before failing with 503. Default 30.
	QueueTimeoutSec int `json:"queue_timeout_seconds"`
	// MaxQueued caps the requests waiting at once; more fail at once with 503. Default 64.
	MaxQueued int `json:"max_queued"`
	// MaxNodesPerToken caps simultaneous node connections per node token. Default 8.
	MaxNodesPerToken int `json:"max_nodes_per_token"`
	// DefaultMaxTokens is applied when a request sets no max_tokens/max_completion_tokens,
	// so one request cannot generate (or think) until a volunteer's context is full.
	// Default 16384; -1 disables it.
	DefaultMaxTokens int `json:"default_max_tokens"`
	// MaxMaxTokens caps what a client may request. Default 32768.
	MaxMaxTokens int `json:"max_max_tokens"`
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
	// Trust "trusted" marks nodes that may serve user chats (MinTrust keys, and requests
	// carrying personal data). New volunteers start untrusted.
	Trust string `json:"trust,omitempty"`
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
	cfg      Config
	log      *slog.Logger
	mu       sync.Mutex
	node     map[string]*nodeConn // by node id
	rr       int                  // round-robin cursor
	tokens   map[string]NodeToken // guarded by mu; replaced by Reload
	known    map[string]time.Time // model or role → last time a node offered it; guarded by mu
	wake     chan struct{}        // closed and replaced whenever capacity may have changed
	queued   atomic.Int32
	buffered atomic.Int64 // bytes held for non-streaming answers, across all requests
	bufByTok sync.Map     // node token → *atomic.Int64: the share held for its nodes
	start    time.Time
	use      *usageWriter
	budget   *budgets
	keys     atomic.Pointer[keyStoreBox]
	keysMu   sync.Mutex // serialises key store updates (Reload, keys file watcher)
	keysCfg  Config     // the config the built-in keys came from; guarded by keysMu
	keysSum  [32]byte   // hash of the keys file last read; guarded by keysMu
	lim      *limiter
	stop     chan struct{} // closed by Close: ends background work
	stopOnce sync.Once
	bg       sync.WaitGroup
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
	if cfg.MaxNodesPerToken == 0 {
		cfg.MaxNodesPerToken = 8
	}
	if cfg.DefaultMaxTokens == 0 {
		cfg.DefaultMaxTokens = 16384
	}
	if cfg.MaxMaxTokens == 0 {
		cfg.MaxMaxTokens = 32768
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
		known: map[string]time.Time{}, wake: make(chan struct{}), use: uw, lim: newLimiter(), start: time.Now(),
		budget: newBudgets(), stop: make(chan struct{})}
	g.keys.Store(&keyStoreBox{&configKeys{plain: staticKeys(cfg)}})
	if err := g.loadKeys(cfg); err != nil {
		return nil, err
	}
	g.every(keysFileCheck, g.checkKeysFile)
	if cfg.StateDir != "" {
		if err := os.MkdirAll(cfg.StateDir, 0o700); err != nil {
			return nil, fmt.Errorf("state dir: %w", err)
		}
		g.budget.path = filepath.Join(cfg.StateDir, "budgets.json")
		g.budget.load(time.Now(), log)
		g.every(saveEvery, func() {
			if err := g.budget.save(); err != nil {
				log.Warn("saving budgets failed", "err", err)
			}
		})
	}
	return g, nil
}

// saveEvery bounds how much budget use a crash can lose, without a write per request.
const saveEvery = 5 * time.Second

// every runs f periodically until Close.
func (g *Gateway) every(d time.Duration, f func()) {
	g.bg.Add(1)
	go func() {
		defer g.bg.Done()
		t := time.NewTicker(d)
		defer t.Stop()
		for {
			select {
			case <-g.stop:
				return
			case <-t.C:
				f()
			}
		}
	}()
}

// Close stops background work and saves state. Call it on shutdown, after the HTTP server
// has stopped taking requests.
func (g *Gateway) Close() error {
	g.stopOnce.Do(func() { close(g.stop) })
	g.bg.Wait()
	return g.budget.save()
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

// keysFileCheck is how soon an added or revoked key takes effect.
const keysFileCheck = 2 * time.Second

// loadKeys builds the built-in key store from cfg and its keys file. It changes nothing if
// the keys file cannot be read, or if a custom key store was set.
func (g *Gateway) loadKeys(cfg Config) error {
	g.keysMu.Lock()
	defer g.keysMu.Unlock()
	return g.loadKeysLocked(cfg)
}

func (g *Gateway) loadKeysLocked(cfg Config) error {
	var hashed map[string]KeyInfo
	var sum [32]byte
	if cfg.KeysFile != "" {
		raw, err := os.ReadFile(cfg.KeysFile)
		if err != nil && !errors.Is(err, os.ErrNotExist) { // missing: no keys yet
			return fmt.Errorf("keys file: %w", err)
		}
		sum = sha256.Sum256(raw)
		f, err := parseKeysFile(raw)
		if err != nil {
			return err
		}
		hashed = make(map[string]KeyInfo, len(f.Keys))
		for _, r := range f.Keys {
			hashed[r.Hash] = r.KeyInfo
		}
	}
	if _, builtin := g.keys.Load().KeyStore.(*configKeys); builtin {
		g.keys.Store(&keyStoreBox{&configKeys{plain: staticKeys(cfg), hashed: hashed}})
	}
	g.keysCfg, g.keysSum = cfg, sum
	return nil
}

// checkKeysFile applies the keys file when its content changed. A broken file keeps the
// keys in use (and is reported once), rather than locking every user out.
func (g *Gateway) checkKeysFile() {
	g.keysMu.Lock()
	defer g.keysMu.Unlock()
	if g.keysCfg.KeysFile == "" {
		return
	}
	raw, err := os.ReadFile(g.keysCfg.KeysFile)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return // e.g. permissions; Reload reports it
	}
	sum := sha256.Sum256(raw)
	if sum == g.keysSum {
		return
	}
	if err := g.loadKeysLocked(g.keysCfg); err != nil {
		g.keysSum = sum // report this version once
		g.log.Error("keys file not applied, keeping the keys in use", "err", err)
		return
	}
	g.log.Info("keys file reloaded")
}

// Reload applies new API keys and node tokens without dropping connections. Nodes whose
// token was revoked, or is now bound to another node id, are disconnected. A custom key
// store set with SetKeyStore is kept; only the built-in keys are replaced. If the keys file
// cannot be read, nothing changes.
func (g *Gateway) Reload(cfg Config) error {
	if err := g.loadKeys(cfg); err != nil {
		return err
	}
	g.mu.Lock()
	g.tokens = copyTokens(cfg.NodeTokens)
	var revoked []*nodeConn
	for _, n := range g.node {
		if t, ok := g.tokens[n.token]; !ok || (t.NodeID != "" && t.NodeID != n.id) || (t.Trust == "trusted") != n.trusted {
			revoked = append(revoked, n)
			continue
		}
		for tok, other := range g.tokens { // the id is now bound to another token
			if tok != n.token && other.NodeID == n.id {
				revoked = append(revoked, n)
				break
			}
		}
	}
	g.mu.Unlock()
	for _, n := range revoked {
		g.log.Info("node token revoked, disconnecting", "node", n.id)
		n.conn.Close(websocket.StatusPolicyViolation, "node token revoked")
	}
	return nil
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
	trusted  bool   // from its token; only trusted nodes serve user chats
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
	ch       chan protocol.Msg
	done     chan struct{}
	slow     chan struct{} // closed when the client cannot keep up
	slowOnce sync.Once
}

// deliver never blocks the node's shared reader: if this request's client is not keeping up,
// the request is dropped rather than stalling the node's other requests and heartbeats.
func (p *pendingReq) deliver(m protocol.Msg) {
	select {
	case p.ch <- m:
	case <-p.done:
	default:
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
	g.mu.Lock()
	reserved := false
	for t, other := range g.tokens { // an id bound to another token cannot be squatted
		if t != token && other.NodeID == hello.NodeID {
			reserved = true
		}
	}
	g.mu.Unlock()
	if reserved {
		c.Close(websocket.StatusPolicyViolation, "node id is reserved")
		return
	}
	n := &nodeConn{id: hello.NodeID, operator: operator, token: token, trusted: tok.Trust == "trusted", models: hello.Models, conn: c,
		pending: map[string]*pendingReq{}, lastSeen: time.Now(), closed: make(chan struct{})}
	if err := n.send(ctx, protocol.Msg{Type: protocol.TypeWelcome, GatewayID: g.cfg.GatewayID, Protocol: protocol.Version}); err != nil {
		return
	}
	g.mu.Lock()
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
	if old != nil { // reconnect replaces the stale connection
		old.conn.Close(websocket.StatusGoingAway, "replaced by new connection")
	}
	g.node[n.id] = n
	now := time.Now()
	for name, last := range g.known { // forget models nobody has offered for a long time
		if now.Sub(last) > knownFor {
			delete(g.known, name)
		}
	}
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

// route is the placement policy of one request: the model or role, and which nodes may see
// it. exclude holds connections that already failed this request; a node that reconnected
// is a new connection and may be tried.
type route struct {
	want     string
	trusted  bool   // only nodes whose token is trusted
	operator string // only nodes of this operator ("" = any)
	exclude  map[*nodeConn]bool
}

func (rt *route) allows(n *nodeConn) bool {
	return !rt.exclude[n] && (!rt.trusted || n.trusted) && (rt.operator == "" || n.operator == rt.operator)
}

// maxBufferedTotal bounds all non-streaming answers held at once, and maxBufferedPerToken
// the share one node token can take, so one operator cannot fill it for everyone.
const (
	maxBufferedTotal    = 64 << 20
	maxBufferedPerToken = 12 << 20
)

// errGatewayBusy means the gateway itself is out of buffer space: not the node's fault.
var errGatewayBusy = errors.New("gateway busy")

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
		if m.Name == "" || len(m.Name) > 128 || len(m.Roles) > 8 || m.Slots < 1 || m.Slots > 64 || m.Ctx < 0 {
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

// pick chooses an allowed node serving the requested model or role that has a free slot,
// round-robin. It returns the node and the local model name to request.
func (g *Gateway) pick(rt *route) (*nodeConn, string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	ids := make([]string, 0, len(g.node))
	for id := range g.node {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for i := 0; i < len(ids); i++ {
		n := g.node[ids[(g.rr+i)%len(ids)]]
		if !rt.allows(n) {
			continue
		}
		for _, m := range n.models {
			if m.Name != rt.want && !contains(m.Roles, rt.want) {
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

// servesNow counts connected nodes offering the model or role.
func (g *Gateway) servesNow(want string) int { return g.servesFor(&route{want: want}) }

// servesFor counts connected nodes that offer the model or role and that the route allows.
func (g *Gateway) servesFor(rt *route) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	c := 0
	for _, n := range g.node {
		if !rt.allows(n) {
			continue
		}
		for _, m := range n.models {
			if m.Name == rt.want || contains(m.Roles, rt.want) {
				c++
				break
			}
		}
	}
	return c
}

// unavailable answers a request that could not be placed, with an honest reason.
func (g *Gateway) unavailable(w http.ResponseWriter, rt *route, why string) {
	want := rt.want
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
		if g.servesFor(&route{want: want, trusted: rt.trusted, operator: rt.operator}) == 0 {
			w.Header().Set("Retry-After", "60")
			which := "trusted node"
			if rt.operator != "" {
				which = "node of your own"
			}
			apiError(w, http.StatusServiceUnavailable, fmt.Sprintf(
				"no %s serving %q is online right now (this request may only go to such nodes)", which, want))
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
	// End users behind a trusted front end (Open WebUI) are identified by its pseudonymous
	// user id and get their own limits, on top of the front end's key limits.
	acct, userKey := info, key
	if info.ForwardUsers {
		if uid := r.Header.Get("X-OpenWebUI-User-Id"); uid != "" {
			acct, userKey = info.forUser(uid), key+"|"+uid
		}
	}
	now := time.Now()
	charge := []string{key}
	limits := []struct {
		k    string
		info KeyInfo
	}{{key, info}}
	if userKey != key {
		// Only the end user is charged: a node inflating usage must not drain the shared
		// front-end key for everyone. The front end keeps its request-rate limits.
		charge = []string{userKey}
		limits = append(limits, struct {
			k    string
			info KeyInfo
		}{userKey, acct})
	}
	for _, l := range limits {
		if wait, over := g.budget.over(l.k, l.info.TokensPerDay, now); over {
			w.Header().Set("Retry-After", strconv.Itoa(wait))
			apiError(w, http.StatusTooManyRequests, "daily token budget used up; it resets at 00:00 UTC")
			return
		}
		release, reason := g.lim.acquire(l.k, l.info, now)
		if release == nil {
			apiError(w, http.StatusTooManyRequests, reason)
			return
		}
		defer release()
	}
	var body map[string]any
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<20)).Decode(&body); err != nil {
		apiError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if msgs, ok := body["messages"].([]any); body == nil || !ok || len(msgs) == 0 {
		apiError(w, http.StatusBadRequest, "the body must be a JSON object with a non-empty messages array")
		return
	}
	want, _ := body["model"].(string)
	clientStream, _ := body["stream"].(bool)
	if body["max_tokens"] == nil && body["max_completion_tokens"] == nil && g.cfg.DefaultMaxTokens > 0 {
		body["max_tokens"] = g.cfg.DefaultMaxTokens
	}
	for _, k := range []string{"max_tokens", "max_completion_tokens"} { // bounds node output and memory
		if v, ok := body[k].(float64); ok && v > float64(g.cfg.MaxMaxTokens) {
			body[k] = g.cfg.MaxMaxTokens
		}
	}
	// Privacy: personal data goes only to trusted nodes; secrets never reach a node at all
	// (replaced by placeholders, restored in the answer).
	rt := &route{want: want, trusted: acct.MinTrust == "trusted", operator: acct.OnlyOperator,
		exclude: map[*nodeConn]bool{}}
	if !rt.trusted && hasPII(body) {
		rt.trusted = true
	}
	red := newRedactor()
	// Nothing to hide only when the key itself belongs to the operator whose nodes serve it
	// (not for end users behind a shared front-end key).
	ownNodes := !info.ForwardUsers && info.OnlyOperator != "" && info.OnlyOperator == info.User
	if !ownNodes {
		red.redactBody(body)
	}
	red.restoreOn = acct.RestoreSecrets
	if red.overflow {
		apiError(w, http.StatusBadRequest, "too many secrets in one request; remove credentials and try again")
		return
	}

	deadline := time.Now().Add(time.Duration(g.cfg.QueueTimeoutSec) * time.Second)
	for attempt := 0; attempt < 2; attempt++ { // one retry before any output, on another connection
		n, model, why := g.pickWait(r.Context(), rt, deadline)
		if n == nil {
			if attempt == 0 {
				g.unavailable(w, rt, why)
			} else if why != clientGone {
				apiError(w, http.StatusBadGateway, "request failed on all available nodes")
			}
			return
		}
		rt.exclude[n] = true
		started, err := g.run(w, r, acct.User, charge, n, model, want, body, clientStream, red)
		if err == nil || started {
			return // success, or failure after output began: the client sees the error
		}
		if isContextOverflow(err.Error()) { // the request itself is too large: no retry
			contextError(w, err.Error())
			return
		}
		if errors.Is(err, errGatewayBusy) {
			w.Header().Set("Retry-After", "10")
			apiError(w, http.StatusServiceUnavailable, "the gateway is busy; try again shortly")
			return
		}
		g.log.Warn("node failed before output, retrying", "node", n.id, "err", errCategory(err.Error()))
	}
	apiError(w, http.StatusBadGateway, "request failed on all available nodes")
}

// run sends one request to a node and relays the result. started reports whether any
// output reached the client (then a retry is no longer possible).
func (g *Gateway) run(w http.ResponseWriter, r *http.Request, user string, charge []string, n *nodeConn,
	model, want string, body map[string]any, clientStream bool, red *redactor) (started bool, err error) {
	reqID := newID()
	nb := forNode(body) // only the fields a node may see
	nb["model"] = model
	nb["stream"] = true
	nb["stream_options"] = map[string]any{"include_usage": true}
	raw, _ := json.Marshal(nb)
	maxTokens := g.cfg.DefaultMaxTokens
	for _, k := range []string{"max_tokens", "max_completion_tokens"} {
		if v, ok := nb[k].(float64); ok && v > 0 {
			maxTokens = int(v)
		} else if v, ok := nb[k].(int); ok && v > 0 {
			maxTokens = v
		}
	}
	if maxTokens <= 0 {
		maxTokens = 16384
	}
	// A node may not send more than it was asked for: bounds memory (non-streaming answers are
	// collected) and work. About one token per chunk, with room for role and usage chunks.
	maxChunks := 2*maxTokens + 256
	const maxCollected = 2 << 20 // per non-streaming request; all of them share g.buffered
	var rest *restorer
	if red.restoring() {
		rest = newRestorer(red)
	}

	p := &pendingReq{ch: make(chan protocol.Msg, 64), done: make(chan struct{}), slow: make(chan struct{})}
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
	sctx, scancel := context.WithTimeout(r.Context(), 15*time.Second)
	err = n.send(sctx, protocol.Msg{Type: protocol.TypeRequest, ReqID: reqID, Body: raw})
	scancel()
	if err != nil {
		return false, err
	}

	// relayed counts chunks carrying generated text, as seen by the gateway itself. It meters
	// requests that end early (the node sends no usage then) and cross-checks node-reported
	// completion tokens (llama.cpp streams about one token per chunk).
	relayed, relayedChars := 0, 0
	record := func(outcome string, u protocol.Usage, estimated bool) {
		u, clamped := clampUsage(u, len(raw), maxTokens, relayed)
		u.DurationMs = time.Since(t0).Milliseconds()
		g.use.record(usageRecord{Time: time.Now().UTC(), User: user, Node: n.id, Operator: n.operator,
			Model: model, Requested: want, Usage: u, ReqID: reqID, Outcome: outcome, Relayed: relayed,
			Estimated: estimated, Clamped: clamped})
		// Budgets are charged from what the gateway itself saw, so a node cannot drain them.
		bu := u
		bu.PromptTokens = min(bu.PromptTokens, len(raw)/3+256)
		bu.CachedTokens = min(bu.CachedTokens, bu.PromptTokens)
		bu.CompletionTokens = min(bu.CompletionTokens, relayedChars/2+16) // by characters, not chunks
		for _, k := range charge {
			g.budget.add(k, weighted(bu), time.Now())
		}
	}
	partial := func(outcome string) {
		if relayed > 0 { // the node did visible work; bill an estimate rather than nothing
			record(outcome, protocol.Usage{CompletionTokens: relayed}, true)
		}
	}

	var sse *bufio.Writer
	var collected []json.RawMessage
	rc := http.NewResponseController(w)
	// writeSSE sends one event; a client that does not take it within 30 s is treated as gone.
	writeSSE := func(data []byte) error {
		if sse == nil {
			w.Header().Set("Content-Type", "text/event-stream")
			w.Header().Set("Cache-Control", "no-cache")
			w.WriteHeader(http.StatusOK)
			sse = bufio.NewWriter(w)
		}
		_ = rc.SetWriteDeadline(time.Now().Add(30 * time.Second))
		fmt.Fprintf(sse, "data: %s\n\n", data)
		if err := sse.Flush(); err != nil {
			return err
		}
		return rc.Flush()
	}
	chunks, collectedBytes := 0, 0
	var held int64 // this request's share of g.buffered and of its node token's share
	tokBuf, _ := g.bufByTok.LoadOrStore(n.token, new(atomic.Int64))
	tokHeld := tokBuf.(*atomic.Int64)
	defer func() { g.buffered.Add(-held); tokHeld.Add(-held) }()
	stall := time.NewTimer(time.Duration(g.cfg.FirstChunkTimeoutSec) * time.Second)
	defer stall.Stop()
	for {
		select {
		case <-stall.C: // the node went silent (e.g. a hung engine): give up on it
			cancelOnNode(n, reqID)
			partial("failed")
			g.log.Warn("node stalled", "node", n.id, "req", reqID, "started", started)
			if started {
				writeSSE(mustJSON(map[string]any{"error": map[string]any{"message": "the node stopped responding"}}))
			}
			return started, errors.New("node stalled")
		case <-p.slow: // the client is not reading; free the node for others
			cancelOnNode(n, reqID)
			partial("cancelled")
			return started, errors.New("client too slow")
		case <-r.Context().Done(): // client went away: tell the node to stop
			cancelOnNode(n, reqID)
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
				chunks++
				collectedBytes += len(m.Data)
				relayedChars += textLen(m.Data)
				overToken, overGlobal := false, false
				if !clientStream {
					held += int64(len(m.Data))
					overToken = tokHeld.Add(int64(len(m.Data))) > maxBufferedPerToken
					overGlobal = g.buffered.Add(int64(len(m.Data))) > maxBufferedTotal
				}
				if overGlobal && !overToken && chunks <= maxChunks && collectedBytes <= maxCollected {
					cancelOnNode(n, reqID)
					partial("failed")
					g.log.Warn("gateway answer buffers full", "node", n.id)
					return started, errGatewayBusy
				}
				if chunks > maxChunks || (!clientStream && collectedBytes > maxCollected) || overToken {
					cancelOnNode(n, reqID)
					partial("failed")
					g.log.Warn("node sent more output than requested", "node", n.id, "chunks", chunks)
					if started {
						writeSSE(mustJSON(map[string]any{"error": map[string]any{"message": "the node sent more output than requested"}}))
					}
					return started, errors.New("node sent more output than requested")
				}
				if !clientStream {
					collected = append(collected, m.Data)
					continue
				}
				started = true
				data := []byte(m.Data)
				if rest != nil {
					data = rest.chunk(data)
				}
				if err := writeSSE(data); err != nil {
					cancelOnNode(n, reqID)
					partial("cancelled")
					return started, err
				}
			case protocol.TypeError:
				m.Error = errCategory(m.Error) // never pass on (or log) free text from a node
				if started {
					e := map[string]any{"message": m.Error}
					if isContextOverflow(m.Error) {
						e["type"], e["code"] = "invalid_request_error", "context_length_exceeded"
					}
					writeSSE(mustJSON(map[string]any{"error": e}))
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
				if clientStream { // writeSSE also answers an empty stream properly
					if rest != nil {
						if f := rest.flush(); f != nil {
							writeSSE(f)
						}
					}
					writeSSE([]byte("[DONE]"))
				} else {
					writeJSON(w, http.StatusOK, restoreAnswer(assemble(reqID, model, collected, u), red))
				}
				return true, nil
			}
		}
	}
}

// cancelOnNode tells a node to stop a request, without waiting long on a stuck connection.
func cancelOnNode(n *nodeConn, reqID string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = n.send(ctx, protocol.Msg{Type: protocol.TypeCancel, ReqID: reqID})
}

// clampUsage bounds node-reported usage to what is plausible for the request: nodes are
// untrusted, and inflated numbers would exhaust shared budgets (or inflate credit).
// promptBytes is the size of the request sent to the node; tokens are never more than bytes.
func clampUsage(u protocol.Usage, promptBytes, maxTokens, relayed int) (protocol.Usage, bool) {
	orig := u
	u.PromptTokens = min(max(u.PromptTokens, 0), promptBytes+4096)
	u.CachedTokens = min(max(u.CachedTokens, 0), u.PromptTokens)
	u.CompletionTokens = min(max(u.CompletionTokens, 0), maxTokens, 8*relayed+64)
	return u, u != orig
}

// weighted is the cost of a request for budgets and credit: new prompt tokens count fully,
// cached prompt tokens a tenth (they cost the node little), generated tokens fully.
func weighted(u protocol.Usage) int64 {
	cached := min(u.CachedTokens, u.PromptTokens)
	return int64(u.PromptTokens-cached) + int64(cached/10) + int64(u.CompletionTokens)
}

// restoreAnswer puts redacted secrets back into the text of a non-streaming answer (never
// into tool arguments; see the restore policy in privacy.go).
func restoreAnswer(a map[string]any, red *redactor) map[string]any {
	if !red.restoring() {
		return a
	}
	choices, _ := a["choices"].([]any)
	for _, c := range choices {
		cm, _ := c.(map[string]any)
		msg, _ := cm["message"].(map[string]any)
		if msg == nil {
			continue
		}
		for _, f := range []string{"content", "reasoning_content"} {
			if s, ok := msg[f].(string); ok {
				msg[f] = red.restore(s)
			}
		}
	}
	return a
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
	Clamped   bool           `json:"clamped,omitempty"` // node reported implausible usage
	Usage     protocol.Usage `json:"usage"`             // node-reported, or estimated from relayed chunks
}

// textLen is the number of generated characters in a streamed chunk (for budget bounds).
func textLen(chunk []byte) int {
	var c struct {
		Choices []struct {
			Delta struct {
				Content          string `json:"content"`
				ReasoningContent string `json:"reasoning_content"`
			} `json:"delta"`
		} `json:"choices"`
	}
	if json.Unmarshal(chunk, &c) != nil {
		return 0
	}
	n := 0
	for _, ch := range c.Choices {
		n += len(ch.Delta.Content) + len(ch.Delta.ReasoningContent)
	}
	return n
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

var engineStatusRe = regexp.MustCompile(`engine returned \d{3}`)

// errCategory reduces an error from a node to a fixed category. Node text is untrusted and
// may quote the prompt (older nodes forward engine response bodies), so it is never logged
// or passed on as is.
func errCategory(msg string) string {
	switch {
	case isContextOverflow(msg):
		return "context_length_exceeded"
	case engineStatusRe.MatchString(msg):
		return engineStatusRe.FindString(msg)
	case strings.Contains(msg, "engine unreachable"):
		return "engine unreachable"
	case strings.Contains(msg, "engine stalled"):
		return "engine stalled"
	case strings.Contains(msg, "ended before the answer was complete"):
		return "engine stream ended early"
	case msg == "node disconnected", msg == "node stalled", msg == "client too slow",
		msg == "node sent more output than requested", msg == "context canceled":
		return msg
	}
	return "node error"
}

// isContextOverflow recognises llama-server's context errors: before generation (HTTP 400,
// "exceed_context_size_error") and during it ("Context size has been exceeded").
func isContextOverflow(msg string) bool {
	m := strings.ToLower(msg)
	return strings.Contains(m, "context_length_exceeded") || strings.Contains(m, "exceed_context_size_error") ||
		strings.Contains(m, "exceeds the available context") ||
		strings.Contains(m, "context size has been exceeded")
}

// contextError answers as OpenAI does, so clients can compact and retry.
func contextError(w http.ResponseWriter, detail string) {
	writeJSON(w, http.StatusBadRequest, map[string]any{"error": map[string]any{
		"message": "the conversation is longer than this node's context; shorten or compact it",
		"type":    "invalid_request_error", "code": "context_length_exceeded"}})
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
