// Package pilot runs the node agent as a BOINC task: a "pilot" that serves the gateway for a
// bounded time and then ends. BOINC's stock wrapper starts `aisathome-node pilot` (job.xml in
// boinc/); the pilot starts llama-server itself, connects to the gateway over the node
// WebSocket with the credential it was given, and ends:
//
//   - after MaxServe (4 h): it pauses, lets requests in flight finish, leaves, exits 0;
//   - after IdleExit (30 min) with no request routed to it: leaves, exits 0;
//   - on SIGTERM (BOINC quits or preempts the task; the wrapper kills it 10 s later): it pauses,
//     gives requests in flight a moment, leaves and is gone within StopBudget;
//   - when the engine keeps failing: leaves, exits 1 (BOINC reports an error).
//
// The connection closes with the reason ("pilot: idle", "pilot: time", "pilot: sigterm",
// "pilot: engine"). The pilot never opens BOINC's init_data.xml, which holds the volunteer's
// account key: nothing it needs is there, and nothing it reads can leak it. It has no settings
// of its own; what a volunteer chooses lives on aisathome.org.
//
// SPDX-License-Identifier: Apache-2.0
package pilot

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

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
)

// Options is one pilot's command line.
type Options struct {
	Gateway        string // ws(s)://host/v1/node/connect
	CredentialFile string // a file holding the node credential (never the command line: see boinc/README.md)
	Model          string // the GGUF file (the wrapper resolves BOINC's soft link: boinc_resolve(model.gguf))
	Tier           string // the engine settings' tier; default "big"
	// The engine: EngineArchive, the node's Linux bundle (.tar.xz with bin/llama-server and
	// lib/), is unpacked once under EngineDir and reused by later pilots. LlamaServer instead
	// names a llama-server to run as it is.
	EngineArchive string
	EngineDir     string
	LlamaServer   string

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
		o.Tier = "big"
	}
	for _, d := range []struct {
		p *time.Duration
		v time.Duration
	}{{&o.MaxServe, 4 * time.Hour}, {&o.IdleExit, 30 * time.Minute}, {&o.DrainWait, 10 * time.Minute},
		{&o.StopBudget, 8 * time.Second}, {&o.Slice, time.Hour}, {&o.YieldIdle, 5 * time.Minute}} {
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
}

// Result is how a pilot ended.
type Result struct {
	Reason string
	Code   int // the process's exit code
}

// Summary is the pilot's output file: what BOINC uploads as the task's result. It holds no
// credential, account or node identity; the gateway's pilot record is the source of truth.
type Summary struct {
	Agent            string `json:"agent"`
	Reason           string `json:"reason"`
	ServedSeconds    int64  `json:"served_s"`
	Connected        bool   `json:"connected"` // the gateway accepted the pilot at least once
	Requests         int64  `json:"requests"`
	Failed           int64  `json:"failed"`
	PromptTokens     int64  `json:"prompt_tokens"`
	CompletionTokens int64  `json:"completion_tokens"`
	Error            string `json:"error,omitempty"` // why the pilot could not start
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
	log := d.Log
	start := d.Now()
	fail := func(err error) Result {
		log.Error("pilot cannot start", "err", err)
		writeSummary(o.Summary, Summary{Agent: node.AgentVersion(), Reason: ReasonEngine, Error: err.Error()}, log)
		return Result{Reason: ReasonEngine, Code: 1}
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
		if exe, libDir, err = Unpack(o.EngineArchive, o.EngineDir); err != nil {
			return fail(err)
		}
	}
	tier, ok := setup.TierByName(o.Tier)
	if !ok {
		return fail(fmt.Errorf("unknown tier %q", o.Tier))
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
	if _, err := os.Stat(o.Model); err != nil {
		return fail(fmt.Errorf("model: %w", err))
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
	actx, stopAgent := context.WithCancel(context.Background())
	agentDone := make(chan error, 1)
	go func() { agentDone <- agent.Run(actx) }()
	log.Info("pilot started", "agent", node.AgentVersion(), "tier", tier.Name, "max_serve", o.MaxServe, "idle_exit", o.IdleExit)

	parent := d.Parent()
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
	// with the reason, stop the engine.
	now := d.Now()
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
		reason, deadline = ReasonSigterm, d.Now().Add(o.StopBudget)
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
		PromptTokens: st.Total.PromptTokens, CompletionTokens: st.Total.CompletionTokens}
	writeSummary(o.Summary, sum, log)
	if reason == ReasonTime || reason == ReasonIdle {
		writeFraction(o.FractionDone, 1)
	}
	code := 0
	if reason == ReasonEngine {
		code = 1
	}
	log.Info("pilot ended", "reason", reason, "requests", sum.Requests, "served_s", sum.ServedSeconds)
	return Result{Reason: reason, Code: code}
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

// readCredential reads the node credential from its file: one line, no spaces.
func readCredential(path string) (string, error) {
	if path == "" {
		return "", errors.New("--credential-file is required")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("credential file: %w", err)
	}
	c := strings.TrimSpace(string(b))
	if c == "" || strings.ContainsAny(c, " \t\r\n") || len(c) > 512 {
		return "", errors.New("credential file: must hold one credential on one line")
	}
	return c, nil
}

// Unpack unpacks the node's Linux bundle (a .tar.xz holding <dir>/bin/llama-server and
// <dir>/lib) once into a folder under dir named after the archive, and returns its llama-server
// and lib folder. A folder already unpacked is reused; pilots that unpack at the same time each
// use their own temporary folder and the first rename wins.
func Unpack(archive, dir string) (exe, lib string, err error) {
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
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	if out, err := exec.CommandContext(ctx, "tar", "-xJf", archive, "-C", tmp, "--no-same-owner").CombinedOutput(); err != nil {
		return "", "", fmt.Errorf("unpacking the engine: %v: %s", err, strings.TrimSpace(string(out)))
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
