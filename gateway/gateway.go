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
	"slices"
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
	fails    *failureCounts   // how attempts ended, per node and model and day (failures.go)
	pstatus  *projectStatuses // runners' latest status, for the operations page (projstatus.go)
	pubq     publicQueues     // built public views of task queues (queue_public.go)
	ptasks   publicTaskList   // the built list of GET /v1/tasks (tasks_public.go)
	served   servedIndex      // which nodes served each task, from the usage log (tasks_served.go)
	speeds   nodeSpeeds       // nodes' recent answer speeds, for their operators (nodework.go)
	tasks    *taskStore       // projects' task queues (tasks.go)
	sci      *scienceDB       // campaigns and external results (science_db.go, campaigns.go)
	campMu   sync.Mutex       // serialises the campaign sweep with a new task's reopen (campaigns_auto.go)
	recent   recentUse        // the last science requests, to name an attempt's node (ops_http.go)
	sitestat siteStatsCache   // the site statistics file, for the funnel card (funnel.go)
	buffered atomic.Int64     // bytes held for non-streaming answers, across all requests
	qbytes   atomic.Int64     // bytes of node messages queued for their requests, across all nodes (pendingReq)
	bodies   atomic.Int64     // bytes reserved for request bodies, across all requests
	bodyMu   sync.Mutex
	bodyBy   map[string]int64 // bytes reserved for request bodies, per user; guarded by bodyMu
	stats    dayStats         // today's counts for /v1/stats
	acfg     AccountsConfig
	accounts *AccountStore // nil without accounts
	gkeys    *jwksCache    // Google's signing keys, for Google sign-in
	email    *emailSignIn  // nil without email sign-in
	links    *nodeLinks    // nodes linking to an account (nodelink.go); nil without accounts
	anonReg  anonRegs      // registrations of nodes without an account, in memory only (anon.go)
	bufByTok sync.Map      // node token → *atomic.Int64: the share held for its nodes
	start    time.Time
	use      *usageWriter
	budget   *budgets
	keys     atomic.Pointer[keyStoreBox]
	keysMu   sync.Mutex // serialises key store updates (Reload, keys file watcher)
	// removedWarned: the removed config fields already named in the log (removed.go); guarded by keysMu.
	removedWarned map[string]bool
	keysCfg       Config   // the config the built-in keys came from; guarded by keysMu
	keysSum       [32]byte // hash of the keys file last read; guarded by keysMu
	lim           *limiter
	shares        []shareEntry    // recent placements, for the share cap; guarded by mu
	held          sync.Map        // node id → NodeHold: held by a guard, without an accounts file
	knownSrc      knownAddrs      // sources the guards know without the request (guard.go)
	installs      knownAddrs      // official installers: stopped, not held (guard_official.go)
	glog          guardLog        // recent guard events with their evidence (guard_evidence.go)
	mc            *modelChecker   // the model check of nodes (modelcheck.go); nil when off
	unrec         unrecordedHolds // guard holds the accounts file has not recorded yet
	tok           *Tokenizer      // the gateway's own token counts; nil = the node's (credit.go)
	counting      chan struct{}   // prompts being counted before routing (countPrompt)
	pcache        *prefixCache    // what each node served whom lately, for cached prompt tokens
	binds         *sessionBinds   // which model answered each conversation: a session stays on it (session.go)
	discards      *discardTracker // answers whose requester may still throw them away (discard.go)
	checks        nodeChecks      // per node: answers, failed checks and signals since the start (verify.go)
	credit        *creditLedger
	corrMu        sync.Mutex
	corrSeen      map[string]bool // credit corrections logged in this run (credit_correct.go)
	statsOut      statsCache      // the leaderboard and the BOINC-format exports
	stop          chan struct{}   // closed by Close: ends background work
	stopOnce      sync.Once
	bg            sync.WaitGroup
	// closeMu: a node connection's writes to the accounts file (its first connection, a last-seen time)
	// hold it for reading; Close takes it once, so it waits for those in flight and none starts after
	// it (a connection's goroutine outlives the HTTP server's Close).
	closeMu sync.RWMutex
	closed  bool // guarded by closeMu

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
	// sweepMu serialises sweepNodes (and sweepAnon, which deletes nodes and their holds).
	sweepMu sync.Mutex
	// sweepHook, tests only: runs inside sweepNodes between reading the tokens and reading the holds.
	sweepHook atomic.Pointer[func()]
	// ownOnlyNow: the operator's emergency switch (RoutingOwnOnly) as last read from the accounts file,
	// which requests that wait for a node check on every try (route.ownOnly).
	ownOnlyNow atomic.Bool
	// noChat rate-limits the WARN line for people's requests refused while no node may serve them
	// (servecap.go).
	noChat noChatLog
	// proxies: Config.TrustedProxies, parsed (clientaddr.go).
	proxies []netip.Prefix
	// untrustedXFF: an X-Forwarded-For from a peer not in proxies was logged once (clientaddr.go).
	untrustedXFF atomic.Bool
	// nrel: the latest node release, for the website (noderelease.go); nil when node_release_url is unset.
	nrel *nodeReleaseWatch
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
	verify, err := cfg.Verify.withDefaults()
	if err != nil {
		return nil, err
	}
	cfg.Verify = verify
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
		known: map[string]time.Time{}, waiting: map[*route]struct{}{}, science: map[*sciRun]struct{}{}, hist: newHistory(), press: newPressure(), fails: newFailureCounts(), flaps: map[string]*flapState{}, bodyBy: map[string]int64{}, wake: make(chan struct{}), counting: make(chan struct{}, countSlots), lim: newLimiter(), start: time.Now(),
		budget: newBudgets(), stop: make(chan struct{}), pcache: newPrefixCache(), binds: newSessionBinds(), discards: newDiscardTracker(), credit: newCreditLedger(), knownSrc: known, installs: installs,
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
		admins := slices.Clone(g.acfg.Admins)
		g.accounts.SetOwn(func(id string) bool { return slices.Contains(admins, id) })
		g.gkeys = newJWKSCache(g.acfg.GoogleCertsURL, g.getJSON)
		if g.acfg.Email != nil {
			g.email = newEmailSignIn(*g.acfg.Email, g.acfg.PublicURL, log)
		}
		g.links = newNodeLinks()
		if cfg.LinkNow != nil {
			g.links.now = cfg.LinkNow
		}
	}
	if g.proxies, err = parsePrefixes(cfg.TrustedProxies); err != nil {
		return fmt.Errorf("trusted_proxies: %w", err)
	}
	if cfg.NodeReleaseURL != "" {
		if g.nrel, err = newNodeReleaseWatch(cfg.NodeReleaseURL, log); err != nil {
			return err
		}
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
	if mcc := cfg.ModelCheck; mcc != nil && mcc.Enabled {
		if g.mc, err = newModelChecker(*mcc, cfg.StateDir, log); err != nil {
			return err
		}
		log.Info("model check on", "references", len(mcc.References), "withhold", mcc.Withhold,
			"withholding_now", g.mc.withholding(), "observing_since", g.mc.st.ObservingSince)
	}
	if cfg.StateDir != "" {
		g.budget.path = filepath.Join(cfg.StateDir, BudgetsFile)
		g.budget.load(time.Now(), log)
		g.credit.path = filepath.Join(cfg.StateDir, "credit.json")
		g.credit.load(log)
		g.applyCreditCorrections()
		g.hist.path = filepath.Join(cfg.StateDir, "history.json")
		g.hist.load(log)
		g.press.path = filepath.Join(cfg.StateDir, pressureFile)
		g.press.load(log, time.Now())
		g.fails.path = filepath.Join(cfg.StateDir, FailuresFile)
		g.fails.load(log, time.Now())
		g.glog.path = filepath.Join(cfg.StateDir, GuardLogFile)
		g.glog.load(log, time.Now())
	}
	// The usage log rebuilds today's and all-time statistics, the history's request and token
	// counts of the days it still covers, and who served each task (tasks_served.go).
	fromLog := newHistory()
	replayUsage(cfg.UsageLog, func(r usageRecord) {
		g.stats.add(r)
		fromLog.add(r)
		g.served.add(&r)
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
		g.every(linkPruneEvery, g.pruneLinks) // expired links; tokens nobody picked up are revoked
		g.every(anonRegPruneEvery, func() { g.anonReg.prune(g.anonNow()) })
		g.every(anonSweepEvery, g.sweepAnon)  // nodes without an account past their retention
		g.every(anonSeenEvery, g.anonSeenAll) // connected ones are seen
	}
	g.every(keysFileCheck, g.checkKeysFile)
	g.every(keysFileCheck, g.applyCreditCorrections) // the operator's CLI asks for corrections (accounts credit-adjust)
	g.every(taskSweepEvery, g.tasks.sweepAll)        // expired leases go back to the queue without a call
	g.every(pilotSeenEvery, g.sweepPilots)           // pilots past their deadline end; connected ones are seen
	g.every(connectorRequestSweep, g.sweepRequests)
	g.every(campaignSweepEvery, func() { g.sweepCampaigns(g.tasks.now()) }) // campaign_auto_finish
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
			if err := g.mc.save(); err != nil {
				log.Warn("saving the model check's results failed", "err", err)
			}
			if err := g.fails.save(); err != nil {
				log.Warn("saving the failure counts failed", "err", err)
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
	if g.mc != nil {
		g.mcStart()
	}
	if g.nrel != nil {
		g.bg.Add(1)
		go func() {
			defer g.bg.Done()
			g.nrel.run(g.stop)
		}()
	}
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
	g.closeMu.Lock()
	g.closed = true
	g.closeMu.Unlock()
	g.bg.Wait()
	return errors.Join(g.budget.save(), g.credit.save(), g.hist.save(), g.press.save(), g.glog.save(time.Now()), g.mc.save(), g.fails.save(), g.closeFiles())
}
