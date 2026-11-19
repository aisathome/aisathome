// Package gateway is the AIs@home project server: an OpenAI-compatible API for users, a
// registry of nodes connected outward over WebSocket, routing with failover, and metering.
//
// SPDX-License-Identifier: Apache-2.0
package gateway

import (
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
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
	press    *pressure        // hourly and daily network pressure (pressure.go)
	pstatus  *projectStatuses // runners' latest status, for the operations page (projstatus.go)
	pubq     publicQueues     // built public views of task queues (queue_public.go)
	ptasks   publicTaskList   // the built list of GET /v1/tasks (tasks_public.go)
	speeds   nodeSpeeds       // nodes' recent answer speeds, for their operators (nodework.go)
	tasks    *taskStore       // projects' task queues (tasks.go, decision 0015)
	sci      *scienceDB       // campaigns and external results (science_db.go, campaigns.go)
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
	held     sync.Map        // node id → NodeHold: held by a guard, without an accounts file
	knownSrc knownAddrs      // sources the guards know without the request (guard.go)
	installs knownAddrs      // official installers: stopped, not held (guard_official.go)
	glog     guardLog        // recent guard events with their evidence (guard_evidence.go)
	unrec    unrecordedHolds // guard holds the accounts file has not recorded yet
	tok      *Tokenizer      // the gateway's own token counts; nil = the node's (credit.go)
	counting chan struct{}   // prompts being counted before routing (countPrompt)
	pcache   *prefixCache    // what each node served whom lately, for cached prompt tokens
	binds    *sessionBinds   // which model answered each conversation: a session stays on it (session.go)
	credit   *creditLedger
	statsOut statsCache    // the leaderboard and the BOINC-format exports
	stop     chan struct{} // closed by Close: ends background work
	stopOnce sync.Once
	bg       sync.WaitGroup

	// connCfgs: the config's connectors (connector.go); replaced on reload.
	connCfgs atomic.Pointer[connectorTable]
	// linkMu orders the steps that map a connector's user to an account: pilot binds and finishing
	// connector requests (connector_requests.go).
	linkMu sync.Mutex
	// spendCap, if set, caps a connector's pilots by spend (demand): the hook for the monthly cap on
	// feeding pilots with science. ok false: no cap.
	spendCap func(connector string) (pilots int, ok bool)
	// pilotRegistered, if set, runs when a pilot's connection is registered, before its record is
	// checked once more (tests: a report racing the handshake).
	pilotRegistered func(pilot string)
	// sweepMu serialises sweepNodes.
	sweepMu sync.Mutex
	// proxies: Config.TrustedProxies, parsed (clientaddr.go).
	proxies []netip.Prefix
	// untrustedXFF: an X-Forwarded-For from a peer not in proxies was logged once (clientaddr.go).
	untrustedXFF atomic.Bool
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
		cfg.PreemptAfterSec = 1
	}
	if cfg.GuardKnownAddresses == nil {
		cfg.GuardKnownAddresses = defaultKnownAddrs
	}
	known, err := parseKnownAddrs(cfg.GuardKnownAddresses)
	if err != nil {
		return nil, err
	}
	if cfg.GuardOfficialInstallers == nil {
		cfg.GuardOfficialInstallers = defaultInstallers
	}
	installs, err := parseAddrList("guard_official_installers", cfg.GuardOfficialInstallers)
	if err != nil {
		return nil, err
	}
	// Startup order (refactor review 2026-11-17, finding 4): every step that can fail comes first,
	// and what it opens is closed again when a later one fails; background work starts last, once
	// nothing can fail any more, so a gateway that does not start leaves no goroutine or file behind.
	g := &Gateway{cfg: cfg, log: log, node: map[string]*nodeConn{}, tokens: copyTokens(cfg.NodeTokens),
		known: map[string]time.Time{}, waiting: map[*route]struct{}{}, science: map[*sciRun]struct{}{}, hist: newHistory(), press: newPressure(), flaps: map[string]*flapState{}, bodyBy: map[string]int64{}, wake: make(chan struct{}), counting: make(chan struct{}, countSlots), lim: newLimiter(), start: time.Now(),
		budget: newBudgets(), stop: make(chan struct{}), pcache: newPrefixCache(), binds: newSessionBinds(), credit: newCreditLedger(), knownSrc: known, installs: installs,
		pstatus: newProjectStatuses(cfg.StateDir, log),
		tasks:   newTaskStore(cfg.StateDir, log)}
	g.tasks.compactAge = g.taskCompactAge
	if cfg.TasksNow != nil {
		g.tasks.now = cfg.TasksNow
	}
	if err := g.setup(cfg); err != nil {
		g.closeFiles()
		return nil, err
	}
	g.startBackground()
	return g, nil
}

// setup does New's steps that can fail, in order: nothing in it starts background work.
func (g *Gateway) setup(cfg Config) (err error) {
	log := g.log
	if cfg.TokenizerFile != "" {
		if g.tok, err = LoadTokenizer(cfg.TokenizerFile, cfg.TokenizerSHA256); err != nil {
			return err
		}
		log.Info("counting tokens with the gateway's tokenizer", "sha256", g.tok.SHA256)
	}
	if cfg.Accounts != nil {
		g.acfg = *cfg.Accounts
		if err := g.acfg.defaults(); err != nil {
			return err
		}
		if g.accounts, err = OpenAccountStore(g.acfg.File); err != nil {
			return err
		}
		g.gkeys = newJWKSCache(g.acfg.GoogleCertsURL, g.getJSON)
		if g.acfg.Email != nil {
			g.email = newEmailSignIn(*g.acfg.Email, g.acfg.PublicURL, log)
		}
	}
	if g.proxies, err = parsePrefixes(cfg.TrustedProxies); err != nil {
		return fmt.Errorf("trusted_proxies: %w", err)
	}
	g.keys.Store(&keyStoreBox{&configKeys{plain: staticKeys(cfg)}})
	if err := g.loadKeys(cfg); err != nil {
		return err
	}
	if cfg.StateDir != "" {
		if err := os.MkdirAll(cfg.StateDir, 0o700); err != nil {
			return fmt.Errorf("state dir: %w", err)
		}
	}
	if g.use, err = newUsageWriter(cfg.UsageLog); err != nil {
		return err
	}
	if g.sci, err = openScienceDB(cfg.StateDir); err != nil {
		return err
	}
	g.sci.log = log
	if err := g.sci.recoverPilots(); err != nil { // connections a stopped gateway left open
		return err
	}
	if cfg.StateDir != "" {
		g.budget.path = filepath.Join(cfg.StateDir, BudgetsFile)
		g.budget.load(time.Now(), log)
		g.credit.path = filepath.Join(cfg.StateDir, "credit.json")
		g.credit.load(log)
		g.hist.path = filepath.Join(cfg.StateDir, "history.json")
		g.hist.load(log)
		g.press.path = filepath.Join(cfg.StateDir, pressureFile)
		g.press.load(log, time.Now())
		g.glog.path = filepath.Join(cfg.StateDir, GuardLogFile)
		g.glog.load(log, time.Now())
	}
	// The usage log rebuilds today's and all-time statistics, and the history's request and
	// token counts of the days it still covers.
	fromLog := newHistory()
	replayUsage(cfg.UsageLog, func(r usageRecord) {
		g.stats.add(r)
		fromLog.add(r)
	})
	g.hist.merge(fromLog)
	return nil
}

// startBackground starts New's periodic work; Close stops it.
func (g *Gateway) startBackground() {
	log := g.log
	if g.accounts != nil {
		if g.email != nil {
			g.bg.Add(1)
			go g.mailWorker()
			g.every(time.Minute, g.email.prune)
		}
		g.every(keysFileCheck, func() { // the operator's CLI changes the file while we run
			g.recordHolds()
			if err := g.accounts.dropOldEvidence(time.Now()); err != nil {
				log.Error("dropping guard evidence older than 14 days from the accounts file failed; retried", "err", err)
			}
			if err := g.accounts.Refresh(); err != nil {
				log.Error("accounts file not applied, keeping the accounts in use", "err", err)
			}
			g.sweepNodes() // banned accounts, revoked node tokens, held or cleared nodes
		})
	}
	g.every(keysFileCheck, g.checkKeysFile)
	g.every(taskSweepEvery, g.tasks.sweepAll) // expired leases go back to the queue without a call
	g.every(pilotSeenEvery, g.sweepPilots)    // pilots past their deadline end; connected ones are seen
	g.every(connectorRequestSweep, g.sweepRequests)
	if g.cfg.StateDir != "" {
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
			if err := g.glog.save(time.Now()); err != nil {
				log.Warn("saving the guard log failed", "err", err)
			}
		})
		g.every(pressureSaveEvery, func() {
			if err := g.press.save(); err != nil {
				log.Warn("saving pressure statistics failed", "err", err)
			}
		})
		g.every(scienceBackupCheck, g.backupScience)
	}
	sample := g.cfg.HistorySample
	if sample == 0 {
		sample = time.Minute
	}
	g.every(sample, func() { g.hist.sample(g.nodesOnline(), time.Now()) })
	g.every(pressureSampleEvery, func() { g.press.sample(g.pressureNow(), time.Now()) })
}

// closeFiles closes what New opened: the usage log and the science database.
func (g *Gateway) closeFiles() error {
	var errs []error
	if g.use != nil {
		errs = append(errs, g.use.close())
	}
	if g.sci != nil {
		errs = append(errs, g.sci.close())
	}
	return errors.Join(errs...)
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
	return errors.Join(g.budget.save(), g.credit.save(), g.hist.save(), g.press.save(), g.glog.save(time.Now()), g.closeFiles())
}
