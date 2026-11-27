// Package pilot runs the node agent as a BOINC task: a "pilot" that serves the gateway for a
// bounded time and then ends. BOINC's stock wrapper starts `aisathome-node pilot` (job.xml in
// boinc/); the pilot starts llama-server itself, connects to the gateway over the node
// WebSocket with the credential it was given, and ends:
//
//   - after MaxServe (4 h): it pauses, lets requests in flight finish, leaves, exits 0;
//   - after IdleExit (30 min) with no request routed to it: leaves, exits 0;
//   - on SIGTERM (BOINC quits or preempts the task; the wrapper kills it 10 s later): it pauses,
//     gives requests in flight a moment, leaves and is gone within StopBudget;
//   - when the card cannot hold its tier or is below the speed floor (checked before it connects,
//     since no installer ran): exits 1 without connecting, with the reason "engine" or "slow";
//   - when the engine keeps failing: leaves, exits 1 (BOINC reports an error), unless it has
//     served an answer, in this run or an earlier run of the same task: then it exits 0, so that
//     BOINC validates the result and the work served earns its credit (the close reason and the
//     summary still say "engine").
//
// The connection closes with the reason ("pilot: idle", "pilot: time", "pilot: sigterm",
// "pilot: engine"). The pilot never opens BOINC's init_data.xml, which holds the volunteer's
// account key: nothing it needs is there, and nothing it reads can leak it. It has no settings
// of its own; what a volunteer chooses lives on aisathome.org.
//
// SPDX-License-Identifier: Apache-2.0
package pilot

import (
	"archive/tar"
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/ulikunitz/xz"

	"github.com/aisathome/aisathome/node"
	"github.com/aisathome/aisathome/node/engine"
	"github.com/aisathome/aisathome/node/setup"
	"github.com/aisathome/aisathome/protocol"
)

// Why a pilot ended: the close reason it sends the gateway is "pilot: " + the reason.
const (
	ReasonSigterm = "sigterm"
	ReasonIdle    = "idle"
	ReasonTime    = "time"
	ReasonEngine  = "engine"
	ReasonSlow    = "slow" // below the speed floor: it never connected
)

// Options is one pilot's command line.
type Options struct {
	Gateway string // ws(s)://host/v1/node/connect
	// CredentialFile holds the node credential (never the command line: see boinc/README.md). It
	// may be BOINC's soft-link file for the credential; the pilot follows the link itself.
	CredentialFile string
	Model          string // the GGUF file (the wrapper resolves BOINC's soft link: boinc_resolve(model.gguf))
	// Tier is the engine settings' tier (node/setup Tiers), or TierAuto (the default): the tier of
	// the model file, else of the card.
	Tier string
	// The engine: EngineArchive, the node's Linux bundle (.tar.xz with bin/llama-server and
	// lib/), is unpacked once under EngineDir and reused by later pilots. LlamaServer instead
	// names a llama-server to run as it is.
	EngineArchive string
	EngineDir     string
	LlamaServer   string

	// MinTPS is the speed floor: decode tokens/s after a SpeedPromptTokens prompt. 0 = the
	// installer's (setup.MinDecodeTPS); negative = measure, never refuse (tests).
	MinTPS float64
	// EngineWait bounds the wait for the engine to load the model before the speed test;
	// default 15 min.
	EngineWait time.Duration

	MaxServe  time.Duration // default 4 h
	IdleExit  time.Duration // default 30 min
	DrainWait time.Duration // at MaxServe: how long requests in flight may take to finish; default 10 min
	// StopBudget bounds the whole stop after SIGTERM; default 8 s, under the wrapper's 10 s.
	StopBudget time.Duration
	// The checkpoint rule (fairness inside the BOINC client): the pilot touches Checkpoint only
	// when it has been idle for YieldIdle (default 5 min) past a Slice boundary (default 60 min,
	// the client's rotation period), so an idle pilot can be rotated out and a busy one cannot.
	Slice     time.Duration
	YieldIdle time.Duration

	Checkpoint   string // job.xml's <checkpoint_filename>; "" = none
	FractionDone string // job.xml's <fraction_done_filename>; "" = none
	Summary      string // the task's output file (a small JSON); "" = none
}

func (o *Options) defaults() {
	if o.Tier == "" {
		o.Tier = TierAuto
	}
	if o.MinTPS == 0 {
		o.MinTPS = setup.MinDecodeTPS
	}
	for _, d := range []struct {
		p *time.Duration
		v time.Duration
	}{{&o.MaxServe, 4 * time.Hour}, {&o.IdleExit, 30 * time.Minute}, {&o.DrainWait, 10 * time.Minute},
		{&o.StopBudget, 8 * time.Second}, {&o.Slice, time.Hour}, {&o.YieldIdle, 5 * time.Minute},
		{&o.EngineWait, 15 * time.Minute}} {
		if *d.p <= 0 {
			*d.p = d.v
		}
	}
}

// Engine is the engine process: package engine's Supervisor.
type Engine interface {
	node.EngineControl
	Run(ctx context.Context) error
}

// Deps is what a pilot runs with; the zero value is the real thing (tests replace parts).
type Deps struct {
	Log          *slog.Logger // the pilot's and the agent's log (BOINC uploads stderr: keep it free of secrets)
	EngineOutput io.Writer    // llama-server's output; nil discards it
	// NewEngine returns the engine for cmd; default: an engine.Supervisor.
	NewEngine func(cmd engine.Command, s setup.EngineSettings, onExit func(error, time.Duration)) Engine
	Tick      time.Duration // how often the pilot checks its exit rules; default 1 s
	Parent    func() int    // the parent's pid (the wrapper); default os.Getppid
	Now       func() time.Time
	// AgentConfig, when set, adjusts the agent's config (tests: shorter timeouts).
	AgentConfig func(*node.Config)
	// Devices lists the card the engine would use (cmd is the engine's command, without
	// arguments); default: `llama-server --list-devices` in the engine's environment.
	Devices func(ctx context.Context, cmd engine.Command) (Device, error)
	// SpeedTest measures the running engine at its base URL; default: setup.SpeedTest.
	SpeedTest func(ctx context.Context, base string) (setup.Speed, error)
}

// Result is how a pilot ended.
type Result struct {
	Reason string
	Code   int // the process's exit code
}

// Summary is the pilot's output file: what BOINC uploads as the task's result. It holds no
// credential, account or node identity; the gateway's pilot record is the source of truth.
type Summary struct {
	Agent            string  `json:"agent"`
	Reason           string  `json:"reason"`
	ServedSeconds    int64   `json:"served_s"`
	Connected        bool    `json:"connected"` // the gateway accepted the pilot at least once
	Requests         int64   `json:"requests"`
	Failed           int64   `json:"failed"`
	PromptTokens     int64   `json:"prompt_tokens"`
	CompletionTokens int64   `json:"completion_tokens"`
	Tier             string  `json:"tier,omitempty"`      // the tier it served (node/setup)
	SpeedTPS         float64 `json:"speed_tps,omitempty"` // its speed test, when it ran one this run
	Error            string  `json:"error,omitempty"`     // why the pilot could not start
}

// engineFailures is how many times the engine may end on its own before the pilot gives up.
const engineFailures = 3

// Run runs one pilot until it ends by one of its rules or ctx ends (SIGTERM). It returns why.
func Run(ctx context.Context, o Options, d Deps) Result {
	o.defaults()
	if d.Log == nil {
		d.Log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if d.Tick <= 0 {
		d.Tick = time.Second
	}
	if d.Parent == nil {
		d.Parent = os.Getppid
	}
	if d.Now == nil {
		d.Now = time.Now
	}
	if d.Devices == nil {
		out := d.EngineOutput
		d.Devices = func(ctx context.Context, cmd engine.Command) (Device, error) { return listDevices(ctx, cmd, out) }
	}
	if d.SpeedTest == nil {
		d.SpeedTest = speedTest
	}
	log := d.Log
	start := d.Now()
	// The wrapper, as it was when the pilot started: if it goes away during the checks below, the
	// pilot must not take its new parent for it.
	parent := d.Parent()
	fail := func(err error) Result {
		log.Error("pilot cannot start", "err", err)
		writeSummary(o.Summary, Summary{Agent: node.AgentVersion(), Reason: ReasonEngine, Error: err.Error()}, log)
		return Result{Reason: ReasonEngine, Code: engineExitCode(servedBefore(o))}
	}
	// stopped: SIGTERM before the pilot served (nothing of the engine runs).
	stopped := func(start time.Time) Result {
		log.Info("pilot ended before serving", "reason", ReasonSigterm)
		writeSummary(o.Summary, Summary{Agent: node.AgentVersion(), Reason: ReasonSigterm,
			ServedSeconds: int64(d.Now().Sub(start) / time.Second)}, log)
		return Result{Reason: ReasonSigterm}
	}
	if !strings.HasPrefix(o.Gateway, "wss://") && !strings.HasPrefix(o.Gateway, "ws://") {
		return fail(fmt.Errorf("--gateway must be a ws:// or wss:// URL"))
	}
	cred, err := readCredential(o.CredentialFile)
	if err != nil {
		return fail(err)
	}
	// BOINC gives the task's files as paths relative to its slot folder (boinc_resolve), and
	// llama-server runs in its own folder: make every path absolute first.
	for _, p := range []*string{&o.Model, &o.LlamaServer, &o.EngineArchive, &o.EngineDir} {
		if *p != "" {
			if *p, err = filepath.Abs(*p); err != nil {
				return fail(err)
			}
		}
	}
	exe, libDir := o.LlamaServer, ""
	if exe == "" {
		if exe, libDir, err = Unpack(ctx, o.EngineArchive, o.EngineDir); err != nil {
			if ctx.Err() != nil {
				// SIGTERM during the first unpack: nothing runs yet, and the partial unpack is gone.
				log.Info("pilot ended while unpacking the engine", "reason", ReasonSigterm)
				writeSummary(o.Summary, Summary{Agent: node.AgentVersion(), Reason: ReasonSigterm,
					ServedSeconds: int64(d.Now().Sub(start) / time.Second)}, log)
				return Result{Reason: ReasonSigterm}
			}
			return fail(err)
		}
	}
	mi, err := os.Stat(o.Model)
	if err != nil {
		return fail(fmt.Errorf("model: %w", err))
	}
	// The card as the engine will see it: the same program, libraries and CUDA_VISIBLE_DEVICES.
	devCmd := engine.Command{Path: exe, Env: []string{"GGML_NO_BACKTRACE=1"}, Dir: filepath.Dir(exe)}
	if libDir != "" {
		devCmd.Env = append(devCmd.Env, "LD_LIBRARY_PATH="+libDir)
	}
	dev, err := d.Devices(ctx, devCmd)
	if err != nil {
		if ctx.Err() != nil {
			return stopped(start)
		}
		return fail(err)
	}
	tier, how, err := pickTier(o.Tier, mi.Size(), dev)
	if err != nil {
		return fail(err)
	}
	log.Info("pilot tier", "tier", tier.Name, "chosen_by", how, "model", tier.Alias, "ctx", tier.Ctx,
		"gpu", dev.Desc, "total_mib", dev.TotalMiB, "free_mib", dev.FreeMiB)
	if err := checkCard(tier, dev); err != nil {
		return fail(err)
	}
	s := tier.Engine(exe, o.Model)
	// Several pilots (one per GPU) and the volunteer's own programs share 127.0.0.1: take a
	// free port rather than the node's fixed 8000.
	if s.Port, err = freePort(); err != nil {
		return fail(err)
	}
	if err := s.Validate(); err != nil {
		return fail(fmt.Errorf("engine: %w", err))
	}
	// The wrapper reports a checkpoint when the file's time changes, not when it first appears:
	// it must exist before the first one counts.
	if err := touch(o.Checkpoint); err != nil {
		return fail(fmt.Errorf("checkpoint file: %w", err))
	}
	// The wrapper sets CUDA_VISIBLE_DEVICES to the GPU the client assigned (job.xml); the
	// engine inherits it with the rest of the environment.
	cmd := engine.Command{Path: exe, Args: s.Args(), Env: s.Env(), Dir: filepath.Dir(exe)}
	if libDir != "" {
		cmd.Env = append(cmd.Env, "LD_LIBRARY_PATH="+libDir)
	}

	var mu sync.Mutex
	exits := 0
	engineDown := make(chan struct{})
	onExit := func(err error, in time.Duration) {
		mu.Lock()
		defer mu.Unlock()
		exits++
		log.Warn("engine ended", "err", err, "restart_in", in, "count", exits)
		if exits == engineFailures {
			close(engineDown)
		}
	}
	newEngine := d.NewEngine
	if newEngine == nil {
		newEngine = func(cmd engine.Command, s setup.EngineSettings, onExit func(error, time.Duration)) Engine {
			return engine.New(cmd, engine.Options{Output: d.EngineOutput, Log: log.With("part", "engine"),
				OnExit: onExit, PreStart: s.CheckPort, StopWait: 2 * time.Second})
		}
	}
	eng := newEngine(cmd, s, onExit)

	cfg := node.Config{GatewayURL: o.Gateway, Token: cred, Upstream: s.Upstream(),
		Models: []protocol.ModelInfo{s.ModelInfo()}, Preset: s.Preset(tier.Name), EngineControl: eng,
		Watchdog: -1} // a dead gateway ends the pilot by the idle rule, not by a restart
	if d.AgentConfig != nil {
		d.AgentConfig(&cfg)
	}
	agent := node.New(cfg, log)

	ectx, stopEngine := context.WithCancel(context.Background())
	engineDone := make(chan struct{})
	go func() {
		defer close(engineDone)
		eng.Run(ectx)
	}()

	// Before it connects: the engine loads the model, then the speed floor (or a recent pass on
	// this card, tier and engine). The engine is stopped again on a refusal or SIGTERM.
	speed, why, err := ready(ctx, o, d, s, tier, dev, engineID(o, exe), engineDown, parent, log)
	if why == "" && d.Parent() != parent { // gone between ready's own checks
		log.Warn("the wrapper is gone; stopping")
		why = ReasonSigterm
	}
	if why != "" {
		stopEngine()
		select {
		case <-engineDone:
		case <-time.After(o.StopBudget):
			log.Warn("engine still stopping; leaving it to its parent-death signal")
		}
		sum := Summary{Agent: node.AgentVersion(), Reason: why, ServedSeconds: int64(d.Now().Sub(start) / time.Second),
			Tier: tier.Name, SpeedTPS: speed}
		code := 0
		if why != ReasonSigterm {
			sum.Error = err.Error()
			code = engineExitCode(servedBefore(o))
			log.Error("pilot cannot serve", "reason", why, "err", err)
		}
		writeSummary(o.Summary, sum, log)
		log.Info("pilot ended", "reason", why, "requests", 0, "code", code)
		return Result{Reason: why, Code: code}
	}

	actx, stopAgent := context.WithCancel(context.Background())
	agentDone := make(chan error, 1)
	go func() { agentDone <- agent.Run(actx) }()
	log.Info("pilot started", "agent", node.AgentVersion(), "tier", tier.Name, "max_serve", o.MaxServe, "idle_exit", o.IdleExit)

	tick := time.NewTicker(d.Tick)
	defer tick.Stop()
	w := watch{o: o, start: start, lastActive: start, nextSlice: start.Add(o.Slice)}
	reason := ""
	for reason == "" {
		select {
		case <-ctx.Done():
			reason = ReasonSigterm
		case <-engineDown:
			reason = ReasonEngine
		case err := <-agentDone:
			// Watchdog off and no Leave yet: only a bug ends the agent here.
			log.Error("the agent ended", "err", err)
			agentDone <- err
			reason = ReasonEngine
		case <-tick.C:
			if p := d.Parent(); p != parent {
				log.Warn("the wrapper is gone; stopping")
				reason = ReasonSigterm
				break
			}
			reason = w.step(d.Now(), agent.Status(), log)
		}
	}

	// Stop: no new requests, let those in flight finish (as long as the reason allows), leave
	// with the reason, stop the engine. The stop's deadlines are wall-clock time, like the
	// wrapper's kill and the time.Until that measures them, not d.Now's.
	now := time.Now()
	deadline := now.Add(o.StopBudget)
	drain := min(3*time.Second, o.StopBudget/2)
	if reason == ReasonTime {
		drain, deadline = o.DrainWait, now.Add(o.DrainWait+o.StopBudget)
	}
	log.Info("pilot stopping", "reason", reason)
	agent.Pause()
	// Only the long drain at the time limit gives way to SIGTERM; the others are short already.
	dctx := context.Background()
	if reason == ReasonTime {
		dctx = ctx
	}
	if reason != ReasonEngine && !waitIdle(dctx, agent, drain, d.Tick/4+time.Millisecond) {
		// SIGTERM during the drain at the time limit: the wrapper's 10 s count now.
		reason, deadline = ReasonSigterm, time.Now().Add(o.StopBudget)
		log.Info("pilot stopping", "reason", reason)
	}
	agent.Leave("pilot: "+reason, min(2*time.Second, time.Until(deadline)))
	stopAgent()
	select {
	case <-agentDone:
	case <-time.After(max(time.Until(deadline)/4, 0)):
	}
	stopEngine()
	select {
	case <-engineDone:
	case <-time.After(max(time.Until(deadline), 0)):
		log.Warn("engine still stopping; leaving it to its parent-death signal")
	}

	st := agent.Status()
	sum := Summary{Agent: node.AgentVersion(), Reason: reason, ServedSeconds: int64(d.Now().Sub(start) / time.Second),
		Connected: w.connected, Requests: st.Total.Requests, Failed: st.Total.Failed,
		PromptTokens: st.Total.PromptTokens, CompletionTokens: st.Total.CompletionTokens, Tier: tier.Name, SpeedTPS: speed}
	writeSummary(o.Summary, sum, log)
	if reason == ReasonTime || reason == ReasonIdle {
		writeFraction(o.FractionDone, 1)
	}
	served := sum.Requests > 0
	if served {
		markServed(o, log)
	}
	code := 0
	if reason == ReasonEngine {
		code = engineExitCode(served || servedBefore(o))
	}
	log.Info("pilot ended", "reason", reason, "requests", sum.Requests, "served_s", sum.ServedSeconds, "code", code)
	return Result{Reason: reason, Code: code}
}

// ready waits for the engine to load the model and applies the speed floor, unless this card, tier
// and engine passed it within speedCacheFor. It returns the speed measured (0 when none ran) and,
// when the pilot must not serve, the reason (ReasonSigterm, ReasonEngine or ReasonSlow) and why.
// SIGTERM, the engine failing for good and the wrapper going away end it at once.
func ready(ctx context.Context, o Options, d Deps, s setup.EngineSettings, tier setup.Tier, dev Device, engine string,
	engineDown <-chan struct{}, parent int, log *slog.Logger) (float64, string, error) {
	pctx, cancel := context.WithCancel(ctx)
	defer cancel()
	why := ReasonSigterm
	var mu sync.Mutex
	go func() {
		t := time.NewTicker(d.Tick)
		defer t.Stop()
		for {
			select {
			case <-pctx.Done():
				return
			case <-engineDown:
				mu.Lock()
				why = ReasonEngine
				mu.Unlock()
				cancel()
				return
			case <-t.C:
				if d.Parent() != parent {
					log.Warn("the wrapper is gone; stopping")
					cancel()
					return
				}
			}
		}
	}()
	// ended: pctx ended (SIGTERM, the wrapper gone, or the engine failing for good).
	ended := func() (float64, string, error) {
		mu.Lock()
		defer mu.Unlock()
		if why == ReasonEngine {
			return 0, ReasonEngine, errors.New("the engine kept failing before the pilot could serve")
		}
		return 0, ReasonSigterm, nil
	}
	if err := waitHealthy(pctx, s.Upstream(), o.EngineWait, min(250*time.Millisecond, d.Tick*25)); err != nil {
		if pctx.Err() == nil {
			return 0, ReasonEngine, fmt.Errorf("the engine did not load the model within %s", o.EngineWait)
		}
		return ended()
	}
	key := speedKey(dev, tier.Name, engine)
	// A pass counts only under the floor in force now; with no floor the test always runs.
	if p, ok := cachedPass(o.EngineDir, key, d.Now()); ok && o.MinTPS > 0 && p.TPS >= o.MinTPS {
		log.Info("speed floor passed recently; no test", "tps", p.TPS, "at", p.At.UTC().Format(time.RFC3339))
		return 0, "", nil
	}
	log.Info("speed test", "prompt_tokens", setup.SpeedPromptTokens, "min_tps", max(o.MinTPS, 0))
	// The engine supervisor restarts a crashed engine: test again once it answers, until the engine
	// has failed for good (engineDown ends pctx) or the test failed engineFailures times.
	var sp setup.Speed
	for try := 1; ; try++ {
		var err error
		if sp, err = d.SpeedTest(pctx, s.Upstream()); err == nil {
			break
		}
		if pctx.Err() != nil {
			return ended()
		}
		if try == engineFailures {
			return 0, ReasonEngine, fmt.Errorf("speed test: %w", err)
		}
		log.Warn("speed test failed; trying again once the engine answers", "err", err, "try", try)
		if err := waitHealthy(pctx, s.Upstream(), o.EngineWait, min(250*time.Millisecond, d.Tick*25)); err != nil {
			if pctx.Err() == nil {
				return 0, ReasonEngine, fmt.Errorf("the engine did not come back within %s", o.EngineWait)
			}
			return ended()
		}
	}
	log.Info("speed test done", "tps", sp.TPS, "prompt_tokens", sp.PromptTokens, "prompt_s", sp.PromptSeconds)
	if o.MinTPS > 0 && sp.TPS < o.MinTPS {
		return sp.TPS, ReasonSlow, slowError(dev, sp, o.MinTPS)
	}
	if o.MinTPS > 0 {
		if err := rememberPass(o.EngineDir, key, speedPass{TPS: sp.TPS, At: d.Now()}); err != nil {
			log.Warn("cannot remember the speed test", "err", err)
		}
	}
	return sp.TPS, "", nil
}

// servedMarker is a file in the task's folder, next to the checkpoint, that says an earlier run of
// this task served an answer: a task that BOINC restarted (after a suspend, say) runs the pilot
// again in the same folder, and the gateway's record, not the pilot's summary, adds up its runs.
func servedMarker(o Options) string {
	if o.Checkpoint == "" {
		return ""
	}
	return o.Checkpoint + ".served"
}

func markServed(o Options, log *slog.Logger) {
	if p := servedMarker(o); p != "" {
		if err := touch(p); err != nil {
			log.Warn("cannot record that this task served", "err", err)
		}
	}
}

func servedBefore(o Options) bool {
	p := servedMarker(o)
	if p == "" {
		return false
	}
	_, err := os.Stat(p)
	return err == nil
}

// engineExitCode: an engine failure is an error for BOINC (exit 1) unless the task served an
// answer: then exit 0, so that BOINC validates the result and the served work earns its credit.
func engineExitCode(served bool) int {
	if served {
		return 0
	}
	return 1
}

// watch applies the time, idle and checkpoint rules to the agent's status, one tick at a time.
type watch struct {
	o          Options
	start      time.Time
	lastActive time.Time
	lastCount  int64
	nextSlice  time.Time
	lastFrac   time.Time
	connected  bool
}

// step returns the reason to stop, or "".
func (w *watch) step(now time.Time, st node.Status, log *slog.Logger) string {
	if st.Connected {
		w.connected = true
	}
	if n := st.Total.Requests + st.Total.Failed; st.SlotsInUse > 0 || n != w.lastCount {
		w.lastActive, w.lastCount = now, n
	}
	elapsed := now.Sub(w.start)
	if now.Sub(w.lastFrac) >= 10*time.Second || w.lastFrac.IsZero() {
		writeFraction(w.o.FractionDone, min(float64(elapsed)/float64(w.o.MaxServe), 0.99))
		w.lastFrac = now
	}
	if elapsed >= w.o.MaxServe {
		return ReasonTime
	}
	idle := now.Sub(w.lastActive)
	if idle >= w.o.IdleExit {
		return ReasonIdle
	}
	if due, next := checkpointDue(now, w.nextSlice, idle, w.o.Slice, w.o.YieldIdle); due {
		w.nextSlice = next
		if err := touch(w.o.Checkpoint); err != nil {
			log.Warn("checkpoint file", "err", err)
		}
	}
	return ""
}

// checkpointDue reports whether the pilot should report a checkpoint now: past the slice
// boundary nextSlice and idle for at least yieldIdle. A busy pilot never checkpoints, so the
// client keeps it running; an idle one lets the client rotate to the volunteer's other projects.
// next is the boundary after now.
func checkpointDue(now, nextSlice time.Time, idle, slice, yieldIdle time.Duration) (due bool, next time.Time) {
	if now.Before(nextSlice) || idle < yieldIdle {
		return false, nextSlice
	}
	next = nextSlice
	for !next.After(now) {
		next = next.Add(slice)
	}
	return true, next
}

// waitIdle waits until the agent has no request in flight, at most d. It returns false when
// ctx ends first (SIGTERM).
func waitIdle(ctx context.Context, a *node.Agent, d, poll time.Duration) bool {
	end := time.Now().Add(d)
	for a.Status().SlotsInUse > 0 && time.Now().Before(end) {
		select {
		case <-ctx.Done():
			return false
		case <-time.After(poll):
		}
	}
	return true
}

// readCredential reads the node credential from its file: one line, no spaces. The file may be
// BOINC's soft-link file in the slot folder (`<soft_link>../../projects/…/name</soft_link>`, or
// a symlink): the pilot follows it itself rather than through the wrapper's boinc_resolve(),
// which would print the credential's physical name (also its download URL) into the stderr that
// BOINC shows on the public result page. For the same reason no error names a file.
func readCredential(path string) (string, error) {
	if path == "" {
		return "", errors.New("--credential-file is required")
	}
	b, err := readFile(path)
	if err != nil {
		return "", fmt.Errorf("credential file: %w", err)
	}
	if link, ok := softLink(b); ok {
		if !filepath.IsAbs(link) {
			link = filepath.Join(filepath.Dir(path), link)
		}
		if b, err = readFile(link); err != nil {
			return "", fmt.Errorf("credential file (soft link): %w", err)
		}
	}
	c := strings.TrimSpace(string(b))
	if c == "" || strings.ContainsAny(c, " \t\r\n<>") || len(c) > 512 {
		return "", errors.New("credential file: must hold one credential on one line")
	}
	return c, nil
}

// softLink returns the target of a BOINC soft-link file: its first line is
// <soft_link>path</soft_link> (client/app_start.cpp, make_soft_link), relative to the slot folder.
func softLink(b []byte) (string, bool) {
	line, _, _ := bytes.Cut(b, []byte("\n"))
	rest, ok := strings.CutPrefix(strings.TrimSpace(string(line)), "<soft_link>")
	if !ok {
		return "", false
	}
	p, ok := strings.CutSuffix(rest, "</soft_link>")
	return p, ok && p != ""
}

// readFile is os.ReadFile with an error that does not name the file.
func readFile(path string) ([]byte, error) {
	b, err := os.ReadFile(path)
	if pe := (*fs.PathError)(nil); errors.As(err, &pe) {
		err = fmt.Errorf("%s: %w", pe.Op, pe.Err)
	}
	return b, err
}

// Unpack unpacks the node's Linux bundle (a .tar.xz holding <dir>/bin/llama-server and
// <dir>/lib) once into a folder under dir named after the archive, and returns its llama-server
// and lib folder. A folder already unpacked is reused; pilots that unpack at the same time each
// use their own temporary folder and the first rename wins. It runs no tar or xz program: the
// wrapper starts the pilot with only job.xml's <setenv> variables (no PATH unless job.xml sets
// one), and a volunteer's machine or container may have no xz. When ctx ends (SIGTERM) it stops
// and removes the partial unpack.
func Unpack(ctx context.Context, archive, dir string) (exe, lib string, err error) {
	if archive == "" || dir == "" {
		return "", "", errors.New("--engine-archive and --engine-dir (or --llama-server) are required")
	}
	name := strings.TrimSuffix(strings.TrimSuffix(filepath.Base(archive), ".xz"), ".tar")
	dest := filepath.Join(dir, name)
	if exe, lib, err := findEngine(dest); err == nil {
		return exe, lib, nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", "", fmt.Errorf("engine folder: %w", err)
	}
	tmp, err := os.MkdirTemp(dir, ".unpack-")
	if err != nil {
		return "", "", fmt.Errorf("engine folder: %w", err)
	}
	defer os.RemoveAll(tmp)
	if err := extract(ctx, archive, tmp); err != nil {
		return "", "", fmt.Errorf("unpacking the engine: %w", err)
	}
	if _, _, err := findEngine(tmp); err != nil {
		return "", "", fmt.Errorf("the engine archive: %w", err)
	}
	if err := os.Rename(tmp, dest); err != nil {
		// Another pilot finished first: use its copy.
		if exe, lib, err2 := findEngine(dest); err2 == nil {
			return exe, lib, nil
		}
		return "", "", fmt.Errorf("engine folder: %w", err)
	}
	return findEngine(dest)
}

// openArchive opens the engine archive (a variable so that tests can make it slow).
var openArchive = func(path string) (io.ReadCloser, error) { return os.Open(path) }

// extract unpacks the .tar.xz archive into dest. Everything is written through an os.Root, so no
// entry, symlink or hard link can reach outside dest; symlinks must point inside it too (the
// bundle's lib/ has library symlinks). It checks ctx between reads.
func extract(ctx context.Context, archive, dest string) error {
	f, err := openArchive(archive)
	if err != nil {
		return err
	}
	defer f.Close()
	xr, err := xz.NewReader(ctxReader{ctx, bufio.NewReaderSize(f, 1<<20)})
	if err != nil {
		return err
	}
	root, err := os.OpenRoot(dest)
	if err != nil {
		return err
	}
	defer root.Close()
	tr := tar.NewReader(ctxReader{ctx, xr})
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		name := filepath.FromSlash(h.Name)
		if !filepath.IsLocal(name) {
			return fmt.Errorf("%q: not a relative path inside the archive", h.Name)
		}
		name = filepath.Clean(name)
		switch h.Typeflag {
		case tar.TypeDir:
			err = root.MkdirAll(name, 0o755)
		case tar.TypeReg:
			err = writeEntry(root, name, h.FileInfo().Mode().Perm()&0o755, tr)
		case tar.TypeSymlink:
			target := filepath.FromSlash(h.Linkname)
			if filepath.IsAbs(target) || !filepath.IsLocal(filepath.Join(filepath.Dir(name), target)) {
				return fmt.Errorf("%q: symlink out of the archive", h.Name)
			}
			if err = mkParent(root, name); err == nil {
				err = root.Symlink(target, name)
			}
		case tar.TypeLink:
			old := filepath.FromSlash(h.Linkname)
			if !filepath.IsLocal(old) {
				return fmt.Errorf("%q: hard link out of the archive", h.Name)
			}
			if err = mkParent(root, name); err == nil {
				err = root.Link(filepath.Clean(old), name)
			}
		case tar.TypeXGlobalHeader:
		default:
			return fmt.Errorf("%q: unsupported entry type %q", h.Name, h.Typeflag)
		}
		if err != nil {
			return err
		}
	}
}

func writeEntry(root *os.Root, name string, mode fs.FileMode, r io.Reader) error {
	if err := mkParent(root, name); err != nil {
		return err
	}
	f, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	_, err = io.Copy(f, r)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

func mkParent(root *os.Root, name string) error {
	if d := filepath.Dir(name); d != "." {
		return root.MkdirAll(d, 0o755)
	}
	return nil
}

// ctxReader fails its reads once ctx has ended.
type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (c ctxReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}

// findEngine finds bin/llama-server in root or in its one top-level folder (the bundle's).
func findEngine(root string) (exe, lib string, err error) {
	for _, base := range []string{root, ""} {
		if base == "" {
			ents, err := os.ReadDir(root)
			if err != nil || len(ents) != 1 || !ents[0].IsDir() {
				break
			}
			base = filepath.Join(root, ents[0].Name())
		}
		exe = filepath.Join(base, "bin", "llama-server")
		if fi, err := os.Stat(exe); err == nil && fi.Mode().IsRegular() && fi.Mode()&0o111 != 0 {
			return exe, filepath.Join(base, "lib"), nil
		}
	}
	return "", "", errors.New("no bin/llama-server in it")
}

// freePort is a free TCP port on 127.0.0.1 (the engine's PreStart check catches a race).
func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

// writeFraction writes the fraction done for the wrapper (it reads the file's last number).
func writeFraction(path string, f float64) {
	if path != "" {
		writeFile(path, []byte(fmt.Sprintf("%.4f\n", f)))
	}
}

func writeSummary(path string, s Summary, log *slog.Logger) {
	if path == "" {
		return
	}
	b, _ := json.Marshal(s)
	if err := writeFile(path, append(b, '\n')); err != nil {
		log.Warn("summary file", "err", err)
	}
}

// writeFile replaces path's content. BOINC's soft-link files are resolved by the wrapper
// (boinc_resolve in job.xml), so path is the real file.
func writeFile(path string, b []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func touch(path string) error {
	if path == "" {
		return nil
	}
	now := time.Now()
	if err := os.Chtimes(path, now, now); err == nil {
		return nil
	}
	return os.WriteFile(path, []byte(now.UTC().Format(time.RFC3339)+"\n"), 0o644)
}
