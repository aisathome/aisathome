// Package gateway is the AIs@home project server: an OpenAI-compatible API for users, a
// registry of nodes connected outward over WebSocket, routing with failover, and metering.
//
// SPDX-License-Identifier: Apache-2.0
package gateway

import (
	"cmp"
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
	// FrontendAddr is a private listener (e.g. ":8081") for the trusted front end (Open
	// WebUI). Keys with forward_users work only there, never on the public listener, so a
	// leaked front-end key cannot speak for other users. Empty = no front end.
	FrontendAddr string `json:"frontend_addr"`
	// KeysFile holds per-user keys (stored hashed) managed with `aisathome-gateway keys`;
	// changes apply within a few seconds, without a reload. Empty = none.
	KeysFile string `json:"keys_file"`
	// StateDir keeps state that must survive a restart (daily token budgets), so a restart
	// does not hand every key a fresh day. Empty = memory only.
	StateDir string `json:"state_dir"`
	// Accounts enables GitHub and/or Google sign-in, self-service API keys and the waitlist
	// (see accounts.go).
	Accounts *AccountsConfig `json:"accounts,omitempty"`
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
	// TokenizerFile is the model's tokenizer.json (Qwen3.8's; README "Credit"). With it the
	// gateway counts the tokens of each answer itself, for credit; without it credit uses the
	// node's reported counts, clamped, and the usage log says counted "node".
	TokenizerFile string `json:"tokenizer_file"`
	// TokenizerSHA256, if set, must match the tokenizer file (hex), or the gateway won't start.
	TokenizerSHA256 string `json:"tokenizer_sha256"`
	// HeartbeatTimeout drops nodes that stay silent for longer than this.
	HeartbeatTimeout time.Duration `json:"-"`
	// StatsRefresh, if set, replaces how often the leaderboard (a minute) and the BOINC-format
	// exports (an hour) are rebuilt. For tests.
	StatsRefresh time.Duration `json:"-"`
}

// NodeToken authorises node connections. In JSON it is either a plain operator id (any
// node id may use the token) or {"operator": "...", "node_id": "..."} to bind it to one node.
type NodeToken struct {
	Operator string `json:"operator"`
	NodeID   string `json:"node_id,omitempty"`
	// Trust "trusted" marks the maintainer's own nodes (not "secure"): they serve MinTrust
	// keys, requests carrying personal data, and agents without consent. Volunteers, and
	// every node registered from the account page, are untrusted.
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

// Request bodies: at most maxBody each, bodyBudget reserved across all requests at once
// (decoded JSON takes a few times more), and read within bodyReadTimeout.
const (
	maxBody         = 8 << 20
	bodyBudget      = 64 << 20
	bodyReadTimeout = 30 * time.Second
)

// reserveBody admits a request body of size bytes for user: at most maxBody in flight per
// user (so one user cannot fill the shared budget with stalled uploads), and bodyBudget across
// everyone. status is 429 when the user is over their share, 503 when the gateway is full.
func (g *Gateway) reserveBody(user string, size int64) (release func(), status int) {
	g.bodyMu.Lock()
	defer g.bodyMu.Unlock()
	if g.bodyBy[user]+size > maxBody {
		return nil, http.StatusTooManyRequests
	}
	if g.bodies.Load()+size > bodyBudget {
		return nil, http.StatusServiceUnavailable
	}
	g.bodies.Add(size)
	g.bodyBy[user] += size
	return func() {
		g.bodyMu.Lock()
		defer g.bodyMu.Unlock()
		g.bodies.Add(-size)
		if g.bodyBy[user] -= size; g.bodyBy[user] <= 0 {
			delete(g.bodyBy, user)
		}
	}, 0
}

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
	bodies   atomic.Int64 // bytes reserved for request bodies, across all requests
	bodyMu   sync.Mutex
	bodyBy   map[string]int64 // bytes reserved for request bodies, per user; guarded by bodyMu
	stats    dayStats         // today's counts for /v1/stats
	acfg     AccountsConfig
	accounts *AccountStore // nil without accounts
	gkeys    *jwksCache    // Google's signing keys, for Google sign-in
	email    *emailSignIn  // nil without email sign-in
	bufByTok sync.Map      // node token → *atomic.Int64: the share held for its nodes
	start    time.Time
	use      *usageWriter
	budget   *budgets
	keys     atomic.Pointer[keyStoreBox]
	keysMu   sync.Mutex // serialises key store updates (Reload, keys file watcher)
	keysCfg  Config     // the config the built-in keys came from; guarded by keysMu
	keysSum  [32]byte   // hash of the keys file last read; guarded by keysMu
	lim      *limiter
	shares   []shareEntry // recent placements, for the share cap; guarded by mu
	held     sync.Map     // node id → true: held by a guard, without an accounts file
	tok      *Tokenizer   // the gateway's own token counts; nil = the node's (credit.go)
	pcache   *prefixCache // what each node served whom lately, for cached prompt tokens
	credit   *creditLedger
	statsOut statsCache    // the leaderboard and the BOINC-format exports
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
		known: map[string]time.Time{}, bodyBy: map[string]int64{}, wake: make(chan struct{}), use: uw, lim: newLimiter(), start: time.Now(),
		budget: newBudgets(), stop: make(chan struct{}), pcache: newPrefixCache(), credit: newCreditLedger()}
	if cfg.TokenizerFile != "" {
		if g.tok, err = LoadTokenizer(cfg.TokenizerFile, cfg.TokenizerSHA256); err != nil {
			return nil, err
		}
		log.Info("counting tokens with the gateway's tokenizer", "sha256", g.tok.SHA256)
	}
	g.stats.replay(cfg.UsageLog, time.Now())
	if cfg.Accounts != nil {
		g.acfg = *cfg.Accounts
		if err := g.acfg.defaults(); err != nil {
			return nil, err
		}
		if g.accounts, err = OpenAccountStore(g.acfg.File); err != nil {
			return nil, err
		}
		g.gkeys = newJWKSCache(g.acfg.GoogleCertsURL, g.getJSON)
		if g.acfg.Email != nil {
			g.email = newEmailSignIn(*g.acfg.Email, g.acfg.PublicURL, log)
			g.bg.Add(1)
			go g.mailWorker()
			g.every(time.Minute, g.email.prune)
		}
		g.every(keysFileCheck, func() { // the operator's CLI changes the file while we run
			if err := g.accounts.Refresh(); err != nil {
				log.Error("accounts file not applied, keeping the accounts in use", "err", err)
			}
			g.sweepNodes() // banned accounts, revoked node tokens, held or cleared nodes
		})
	}
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
		g.credit.path = filepath.Join(cfg.StateDir, "credit.json")
		g.credit.load(log)
		g.every(saveEvery, func() {
			if err := g.budget.save(); err != nil {
				log.Warn("saving budgets failed", "err", err)
			}
			if err := g.credit.save(); err != nil {
				log.Warn("saving credit failed", "err", err)
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
	return errors.Join(g.budget.save(), g.credit.save())
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
		if n.account != "" { // an account's node: its token is in the accounts file (sweepNodes)
			continue
		}
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
	mux.HandleFunc("GET /v1/stats", g.handleStats)
	mux.HandleFunc("GET /v1/stats/credit", g.handleCreditStats)
	mux.HandleFunc("GET /stats/{$}", handleExportIndex)
	mux.HandleFunc("GET /stats/{file}", g.handleExport)
	if g.accounts != nil {
		g.accountRoutes(mux)
	}
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { fmt.Fprintln(w, "ok") })
	return mux
}

type frontendCtx struct{}

// FrontendHandler serves the same API on the private listener for the trusted front end
// (Config.FrontendAddr). Only requests that arrive here may use forward_users keys.
func (g *Gateway) FrontendHandler() http.Handler {
	h := g.Handler()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), frontendCtx{}, true)))
	})
}

// ---------------------------------------------------------------- nodes

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
	inflight int // slots reserved by pick, released when run ends; guarded by mu
	active   int
	paused   bool // from the last heartbeat: take no new requests; guarded by mu
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
		if p != nil && (m.Type == protocol.TypeChunk || m.Type == protocol.TypeDone || m.Type == protocol.TypeError) {
			p.deliver(m) // outside the lock: a slow client must not block other requests
		}
	}
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

// maxCtx bounds the context a node may announce.
const maxCtx = 4 << 20

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

// ---------------------------------------------------------------- user API

func (g *Gateway) user(r *http.Request) (string, bool) {
	k, _, ok := g.authenticate(r)
	return k.User, ok
}

// authenticate resolves the request's API key: configured keys first, then keys that accounts
// created themselves. accountID is set for the latter. Every endpoint uses this, so a key works
// the same everywhere.
func (g *Gateway) authenticate(r *http.Request) (info KeyInfo, accountID string, ok bool) {
	key := bearer(r)
	if info, ok = g.keys.Load().Lookup(r.Context(), key); ok {
		if info.ForwardUsers && r.Context().Value(frontendCtx{}) == nil {
			return KeyInfo{}, "", false // front-end keys work only on the private listener
		}
		return info, "", true
	}
	if g.accounts != nil {
		var agents bool
		if accountID, agents, ok = g.accounts.keyAccount(key); ok {
			info = g.accountInfo(accountID)
			info.VolunteerAgents = agents
			return info, accountID, true
		}
	}
	return KeyInfo{}, "", false
}

func bearer(r *http.Request) string {
	return strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
}

// minAdvertisedCtx: smaller announced contexts do not lower what /v1/models advertises.
const minAdvertisedCtx = 8192

func (g *Gateway) handleModels(w http.ResponseWriter, r *http.Request) {
	if _, ok := g.user(r); !ok {
		apiError(w, http.StatusUnauthorized, "invalid API key")
		return
	}
	// One entry per model or role, with the context that is reliably available for it: the
	// smallest among the connected nodes serving it, so that a client sizing its compaction
	// from context_length (opencode) fits on every node. Announcements below
	// minAdvertisedCtx count only if no node offers more: one node announcing a tiny context
	// must not shrink it for everyone (it still gets the requests that fit it).
	var ids []string
	small, all := map[string]int{}, map[string]int{} // smallest ≥ minAdvertisedCtx; smallest of any
	g.mu.Lock()
	for _, n := range g.node {
		if n.held {
			continue
		}
		for _, m := range n.models {
			for _, id := range append([]string{m.Name}, m.Roles...) {
				if _, ok := all[id]; !ok {
					ids = append(ids, id)
					all[id] = m.Ctx
				}
				if m.Ctx > 0 && (all[id] == 0 || m.Ctx < all[id]) {
					all[id] = m.Ctx
				}
				if m.Ctx >= minAdvertisedCtx && (small[id] == 0 || m.Ctx < small[id]) {
					small[id] = m.Ctx
				}
			}
		}
	}
	g.mu.Unlock()
	data := []map[string]any{}
	for _, id := range ids {
		e := map[string]any{"id": id, "object": "model", "owned_by": "AIs@home"}
		if c := cmp.Or(small[id], all[id]); c > 0 {
			e["context_length"] = c
		}
		data = append(data, e)
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": data})
}

// keyLimit is a key whose budget and request limits a chat request is held to.
type keyLimit struct {
	k    string
	info KeyInfo
}

func (g *Gateway) handleChat(w http.ResponseWriter, r *http.Request) {
	key := bearer(r)
	info, accountID, ok := g.authenticate(r)
	if !ok {
		apiError(w, http.StatusUnauthorized, "invalid API key")
		return
	}
	// End users behind a trusted front end (Open WebUI) are identified by its pseudonymous
	// user id and get their own limits, on top of the front end's key limits. Signed-in
	// accounts arrive as gh-<id>@users.invalid, google-<sub>@users.invalid or
	// em-<hex>@users.invalid and share one budget with their API keys.
	acct, userKey, reqAcct := info, key, accountID // reqAcct: the account behind the request, API key or chat
	if accountID != "" {
		userKey = "acct:" + accountID
	} else if info.ForwardUsers {
		if id, isAcct := accountFromEmail(r.Header.Get("X-OpenWebUI-User-Email")); isAcct {
			if g.accounts == nil || !g.accounts.active(id) { // banned or waiting: not through the chat either
				apiError(w, http.StatusForbidden, "this account is not active")
				return
			}
			acct, userKey, reqAcct = g.accountInfo(id).within(info), "acct:"+id, id
		} else if uid := r.Header.Get("X-OpenWebUI-User-Id"); uid != "" {
			acct, userKey = info.forUser(uid), key+"|"+uid
		}
	}
	now := time.Now()
	charge := []string{key}
	limits := []keyLimit{{key, info}}
	if accountID != "" { // an account key: limits and budget belong to the account
		charge, limits[0].k = []string{userKey}, userKey
	} else if userKey != key {
		// Only the end user is charged: a node inflating usage must not drain the shared
		// front-end key for everyone. The front end keeps its request-rate limits.
		charge = []string{userKey}
		limits = append(limits, keyLimit{userKey, acct})
	}
	for _, l := range limits {
		if wait, over := g.budget.over(l.k, l.info.TokensPerDay, now); over {
			w.Header().Set("Retry-After", strconv.Itoa(wait))
			apiError(w, http.StatusTooManyRequests, "daily token budget used up; it resets at 00:00 UTC")
			return
		}
	}
	// The body is read before taking a concurrency slot (a slow upload must not hold one),
	// within a deadline and a memory budget shared by all requests.
	size := int64(maxBody)
	if r.ContentLength >= 0 && r.ContentLength < size {
		size = r.ContentLength
	}
	release, status := g.reserveBody(userKey, size)
	if release == nil {
		w.Header().Set("Retry-After", "5")
		if status == http.StatusTooManyRequests {
			apiError(w, status, "too many uploads in progress for this user")
		} else {
			apiError(w, status, "the gateway is busy; try again shortly")
		}
		return
	}
	defer release()
	rc := http.NewResponseController(w)
	rc.SetReadDeadline(time.Now().Add(bodyReadTimeout))
	var body map[string]any
	err := json.NewDecoder(http.MaxBytesReader(w, r.Body, size)).Decode(&body)
	rc.SetReadDeadline(time.Time{})
	if err != nil {
		apiError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	for _, l := range limits {
		release, reason := g.lim.acquire(l.k, l.info, time.Now())
		if release == nil {
			apiError(w, http.StatusTooManyRequests, reason)
			return
		}
		defer release()
	}
	if msgs, ok := body["messages"].([]any); body == nil || !ok || len(msgs) == 0 {
		apiError(w, http.StatusBadRequest, "the body must be a JSON object with a non-empty messages array")
		return
	}
	if n, ok := body["n"].(float64); ok && n != 1 { // answers are assembled as one choice
		apiError(w, http.StatusBadRequest, "n must be 1")
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
	// Privacy: personal data goes only to trusted nodes (and the requester's own); secrets
	// never reach a node at all (replaced by placeholders, restored in the answer).
	rt := g.routeFor(want, body, acct, reqAcct, acct.VolunteerAgents && !info.ForwardUsers)
	reqText := requestText(body) // before redaction: addresses the user wrote, for the guards
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

	req := &chatReq{user: acct.User, who: userKey, charge: charge, self: rt.self, want: want,
		clientStream: clientStream, body: body, red: red}
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
		var gd *guard // answers from strangers' nodes are checked (guard.go)
		if !n.trusted && n.operator != rt.self {
			gd = newGuard(reqText)
		}
		started, err := g.run(w, r, req, n, model, gd)
		var flagged *guardError
		if errors.As(err, &flagged) && !started {
			// Not one byte of the stopped answer went out: try a trusted node if one is free now,
			// within all of the request's restrictions (operator, own nodes only, context).
			fb := *rt
			fb.trustedOnly, fb.preferSelf = true, false
			if tn, tmodel := g.pick(&fb); tn != nil {
				if started, err = g.run(w, r, req, tn, tmodel, nil); err == nil || started {
					return
				}
			}
			apiError(w, http.StatusBadGateway, "the volunteer node's answer was stopped by a safety check (a tool call that "+
				"downloads and runs code, reads credentials, sends files, or uses a web address not in the conversation), "+
				"and no trusted node is free to answer instead; try again later")
			return
		}
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

// outcomeBlocked: a guard stopped the answer (guard.go).
const outcomeBlocked = "blocked"

type usageRecord struct {
	Time      time.Time      `json:"time"`
	ReqID     string         `json:"req_id"`
	User      string         `json:"user"`
	Node      string         `json:"node"`
	Operator  string         `json:"operator"`
	Model     string         `json:"model"`
	Requested string         `json:"requested"`
	Outcome   string         `json:"outcome"`        // ok, cancelled (client left), failed or blocked
	Relayed   int            `json:"relayed_chunks"` // text chunks the gateway relayed itself
	Estimated bool           `json:"estimated,omitempty"`
	Clamped   bool           `json:"clamped,omitempty"` // node reported implausible usage
	Usage     protocol.Usage `json:"usage"`             // node-reported, or estimated from relayed chunks
	// SelfServed: the node's operator is the requesting account (no credit for serving oneself).
	SelfServed bool `json:"self_served,omitempty"`
	// Tools: the request carried tool definitions (an agent request); never which ones.
	Tools bool   `json:"tools,omitempty"`
	Guard string `json:"guard,omitempty"` // a guard's rule: the answer was stopped, or (text) flagged
	// Counted says who counted Tokens: "gateway" (its tokenizer) or "node" (reported counts,
	// clamped). Cached tokens are always the gateway's decision. Credit is what the node earned.
	Counted string        `json:"counted,omitempty"`
	Tokens  *creditTokens `json:"tokens,omitempty"`
	Credit  float64       `json:"credit,omitempty"`
}

// textLen is the number of generated characters in a streamed chunk (for budget bounds).
// Generated output is text, reasoning and tool-call arguments: all of it costs GPU time and
// counts against budgets.
func textLen(chunk []byte) int {
	var c struct {
		Choices []struct {
			Delta struct {
				Content          string `json:"content"`
				ReasoningContent string `json:"reasoning_content"`
				ToolCalls        []struct {
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"delta"`
		} `json:"choices"`
	}
	if json.Unmarshal(chunk, &c) != nil {
		return 0
	}
	n := 0
	for _, ch := range c.Choices {
		n += len(ch.Delta.Content) + len(ch.Delta.ReasoningContent)
		for _, tc := range ch.Delta.ToolCalls {
			n += len(tc.Function.Name) + len(tc.Function.Arguments)
		}
	}
	return n
}

// hasText reports whether a streamed chunk carries generated text (answer or reasoning).
func hasText(chunk []byte) bool { return textLen(chunk) > 0 }

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
	case msg == "node paused", msg == "node disconnected", msg == "node stalled", msg == "client too slow",
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

// hasTools reports whether a request carries tool definitions (an agent request).
func hasTools(body map[string]any) bool {
	tools, _ := body["tools"].([]any)
	return len(tools) > 0
}
