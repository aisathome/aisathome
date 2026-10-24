// Package gateway is the AIs@home project server: an OpenAI-compatible API for users, a
// registry of nodes connected outward over WebSocket, routing with failover, and metering.
//
// SPDX-License-Identifier: Apache-2.0
package gateway

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/aisathome/aisathome/protocol"
)

// Gateway routes user requests to connected nodes.
type Gateway struct {
	cfg      Config
	log      *slog.Logger
	mu       sync.Mutex
	node     map[string]*nodeConn  // by node id
	rr       int                   // round-robin cursor
	tokens   map[string]NodeToken  // guarded by mu; replaced by Reload
	known    map[string]time.Time  // model or role → last time a node offered it; guarded by mu
	flaps    map[string]*flapState // node id → connections replaced soon after connecting; guarded by mu
	wake     chan struct{}         // closed and replaced whenever capacity may have changed
	queued   atomic.Int32          // people's requests waiting for a node (chat and agent)
	queuedSc atomic.Int32          // science requests waiting for a node: they never fill people's queue
	waiting  map[*route]struct{}   // requests looking for a node, for priority (routing.go); guarded by mu
	science  map[*sciRun]struct{}  // science requests holding a slot, for preemption (preempt.go); guarded by mu
	projects atomic.Pointer[projectTable]
	hist     *history         // daily snapshots for the network page (history.go)
	pstatus  *projectStatuses // runners' latest status, for the operations page (projstatus.go)
	recent   recentUse        // the last science requests, to name an attempt's node (ops_http.go)
	buffered atomic.Int64     // bytes held for non-streaming answers, across all requests
	bodies   atomic.Int64     // bytes reserved for request bodies, across all requests
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
	shares   []shareEntry    // recent placements, for the share cap; guarded by mu
	held     sync.Map        // node id → true: held by a guard, without an accounts file
	unrec    unrecordedHolds // guard holds the accounts file has not recorded yet
	tok      *Tokenizer      // the gateway's own token counts; nil = the node's (credit.go)
	pcache   *prefixCache    // what each node served whom lately, for cached prompt tokens
	credit   *creditLedger
	statsOut statsCache    // the leaderboard and the BOINC-format exports
	stop     chan struct{} // closed by Close: ends background work
	stopOnce sync.Once
	bg       sync.WaitGroup
}

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
	if cfg.NodeProtocolMax == 0 {
		cfg.NodeProtocolMax = protocol.Version
	}
	if cfg.NodeProtocolMin == 0 {
		cfg.NodeProtocolMin = max(protocol.Unversioned, protocol.Version-1)
	}
	if cfg.NodeProtocolMin < protocol.Unversioned || cfg.NodeProtocolMin > cfg.NodeProtocolMax || cfg.NodeProtocolMax > protocol.Version {
		return nil, fmt.Errorf("node_protocol_min %d and node_protocol_max %d: need %d <= min <= max <= %d (this gateway's protocol version)",
			cfg.NodeProtocolMin, cfg.NodeProtocolMax, protocol.Unversioned, protocol.Version)
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
	if cfg.CancelAckTimeoutSec == 0 {
		cfg.CancelAckTimeoutSec = 120
	}
	if cfg.PreemptAfterSec == 0 {
		cfg.PreemptAfterSec = 8
	}
	uw, err := newUsageWriter(cfg.UsageLog)
	if err != nil {
		return nil, err
	}
	g := &Gateway{cfg: cfg, log: log, node: map[string]*nodeConn{}, tokens: copyTokens(cfg.NodeTokens),
		known: map[string]time.Time{}, waiting: map[*route]struct{}{}, science: map[*sciRun]struct{}{}, hist: newHistory(), flaps: map[string]*flapState{}, bodyBy: map[string]int64{}, wake: make(chan struct{}), use: uw, lim: newLimiter(), start: time.Now(),
		budget: newBudgets(), stop: make(chan struct{}), pcache: newPrefixCache(), credit: newCreditLedger(),
		pstatus: newProjectStatuses(cfg.StateDir, log)}
	if cfg.TokenizerFile != "" {
		if g.tok, err = LoadTokenizer(cfg.TokenizerFile, cfg.TokenizerSHA256); err != nil {
			return nil, err
		}
		log.Info("counting tokens with the gateway's tokenizer", "sha256", g.tok.SHA256)
	}
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
			g.recordHolds()
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
		g.hist.path = filepath.Join(cfg.StateDir, "history.json")
		g.hist.load(log)
		g.every(saveEvery, func() {
			if err := g.budget.save(); err != nil {
				log.Warn("saving budgets failed", "err", err)
			}
			if err := g.credit.save(); err != nil {
				log.Warn("saving credit failed", "err", err)
			}
			if err := g.hist.save(); err != nil {
				log.Warn("saving history failed", "err", err)
			}
		})
	}
	// The usage log rebuilds today's and all-time statistics, and the history's request and
	// token counts of the days it still covers.
	fromLog := newHistory()
	replayUsage(cfg.UsageLog, func(r usageRecord) {
		g.stats.add(r)
		fromLog.add(r)
	})
	g.hist.merge(fromLog)
	sample := cfg.HistorySample
	if sample == 0 {
		sample = time.Minute
	}
	g.every(sample, func() { g.hist.sample(g.NodeCount(), time.Now()) })
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
	return errors.Join(g.budget.save(), g.credit.save(), g.hist.save())
}
