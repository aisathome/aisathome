// The gateway's model check (BACKLOG 11): does a node or pilot compute like the model it announces?
//
// The gateway sends secret probe prompts to nodes at random times, through the same connection and
// in the same message as any request, and compares the answer's top-20 logprobs with a reference
// recorded on the maintainer's own card, by the method of `aisathome-node modelcheck` (node/modelcheck): KL
// divergence per position over groups of 4 prompts, and confident flips. The prompts and their
// reference are secret: a file pinned by SHA-256 in the gateway's config (model_check.references),
// recorded with `aisathome-node modelcheck record -prompts` on a maintainer's own card and kept with
// the gateway's secrets, never in a repository (the public prompt set is an operator's self-check,
// and a node could cache its answers).
//
// What a probe looks like to the node: a `request` like any other, with a fresh request id, the
// gateway's default max_tokens, streamed, one user message. It asks for greedy decoding, top-20
// logprobs and no thinking, as the reference was recorded; the answer runs to its natural end (up
// to mcMaxAnswer tokens), and only its first positions are compared. A share of people's requests
// without tools (cover_share) asks for the same logprobs, removed before the answer reaches the
// client (relay.go), so "logprobs" alone does not mark a probe. A node agent modified to recognise
// probes (temperature 0, no tools, a short prompt) can still route them to the real model: the
// check catches a wrong model file, another model or a proxy without logprobs, not a careful
// cheater (docs/modelcheck.md, "What the gateway's check catches"). The replay of answers (11f)
// is what catches text the model did not write.
//
// Probes take only a free slot that no waiting request could use, yield to a person's request like
// science does (preempt.go), earn no credit, are in no usage record, budget or statistic, and cost
// nobody anything. A node's result is a rolling window of its last mcWindow checks, shown on
// /account/ops and by `aisathome-gateway accounts model-check`. The check observes only, unless
// model_check.withhold is on and the gateway has observed for mcObserveFirst: then a node that
// fails holds like a guard hit (its pilot's credit withheld), cleared with `accounts node-clear`.
//
// SPDX-License-Identifier: Apache-2.0
package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"math/rand/v2"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/aisathome/aisathome/node/modelcheck"
	"github.com/aisathome/aisathome/protocol"
)

// ModelCheckConfig turns the gateway's model check on (model_check in the config).
type ModelCheckConfig struct {
	Enabled bool `json:"enabled"`
	// References are the secret prompt sets with their recorded answers, one per model name nodes
	// announce ("qwen3.8-27b"): files in the aisathome-modelcheck/1 format, pinned by SHA-256.
	References []ModelCheckReference `json:"references"`
	// IntervalSec is the mean time between two probe prompts to a node once it has a result
	// (exponential, so a node cannot predict them); 4 prompts make a check. Default 1800 (a check
	// about every 2 hours).
	IntervalSec int `json:"interval_seconds"`
	// RecheckSec is the mean time between probe prompts before a node's first check is complete
	// and while a failed check waits for its re-check. Default 300 (a first result within ~20 min).
	RecheckSec int `json:"recheck_seconds"`
	// AdmitSec is the mean time between probe prompts to a node without an account that is not admitted
	// yet (anon.go): its admission takes anonAdmitChecks checks. Default 20 (admitted within minutes; a
	// node not admitted gets nothing else to do).
	AdmitSec int `json:"admit_seconds"`
	// LegacyIncludeTrusted is the removed "include_trusted" (removed.go): accepted and ignored. The
	// check is for every node.
	LegacyIncludeTrusted bool `json:"include_trusted,omitempty"`
	// Withhold makes a failing node held (no requests; its pilot's credit withheld) instead of
	// only recorded. Off by default, and without effect until the gateway has observed for
	// mcObserveFirst (3 days, owner D2): the limits must first hold on our own honest nodes.
	Withhold bool `json:"withhold"`
	// CoverShare is the share of people's requests without tools to checked nodes that also ask
	// for the probes' logprobs (removed before the client sees them). Absent: 0.2; 0 turns it off.
	CoverShare *float64 `json:"cover_share,omitempty"`

	// For tests: the scheduler's tick, the intervals, and the clock.
	Tick     time.Duration    `json:"-"`
	Interval time.Duration    `json:"-"`
	Recheck  time.Duration    `json:"-"`
	Admit    time.Duration    `json:"-"`
	Now      func() time.Time `json:"-"`
}

// ModelCheckReference is one secret reference: the model name it checks, its file and pin.
type ModelCheckReference struct {
	Model  string `json:"model"`
	File   string `json:"file"`
	SHA256 string `json:"sha256"`
}

const (
	// mcWindow is how many of a node's latest checks its result is judged on: 25 checks of 4
	// prompts are the 100 prompts the published flip limit is for.
	mcWindow = 25
	// mcObserveFirst: withholding starts only after the check has observed this long.
	mcObserveFirst = ModelCheckObserveFirst
	// mcMaxAnswer bounds a probe's answer; one that goes on is stopped (a greedy answer to a
	// probe ends after a few hundred tokens).
	mcMaxAnswer = 4096
	// mcKeepNodes: a node not seen for this long is forgotten (it holds an operator name).
	mcKeepNodes = 30 * 24 * time.Hour
	// mcGap: a results file last saved longer ago than this was left by a check that was off.
	mcGap = 24 * time.Hour
	// ModelCheckFile is the result file in state_dir.
	ModelCheckFile = "model-check.json"
	// defaultCoverShare: see ModelCheckConfig.CoverShare.
	defaultCoverShare = 0.2
)

// ModelCheckObserveFirst is how long the check observes before model_check.withhold takes effect.
const ModelCheckObserveFirst = 72 * time.Hour

// ModelCheckSnapshot is the check's state now (false: the check is off), for tests and tools.
func (g *Gateway) ModelCheckSnapshot() (ModelCheckState, bool) {
	if g.mc == nil {
		return ModelCheckState{}, false
	}
	return g.mc.snapshot(), true
}

// Node results (ModelCheckNode.Status).
const (
	MCPass    = "pass"    // checked, no check failed in the window
	MCSuspect = "suspect" // one check failed in the window: its re-check is due soon
	MCFail    = "fail"    // two or more failed, or too many confident flips over the window
	MCUnknown = "unknown" // no complete check yet
)

// ModelCheckResult is one check: GroupSize probe prompts compared with the reference.
type ModelCheckResult struct {
	At        time.Time `json:"at"`
	Verdict   string    `json:"verdict"` // modelcheck.Pass or modelcheck.Fail
	KL        float64   `json:"kl"`      // the group's KL statistic
	Flips     int       `json:"flips"`
	Positions int       `json:"positions"`
	Diverged  int       `json:"diverged"`
	Prompts   []string  `json:"prompts"` // the secret prompts' ids (never their text)
	Reasons   []string  `json:"reasons,omitempty"`
}

// ModelCheckNode is what the check knows about one node.
type ModelCheckNode struct {
	Node     string             `json:"node"`
	Operator string             `json:"operator,omitempty"`
	Model    string             `json:"model"`
	Pass     int                `json:"pass"`    // checks passed, all time
	Fail     int                `json:"fail"`    // checks failed, all time
	Dropped  int                `json:"dropped"` // probe prompts without an answer (node error, disconnect, stall, preempted)
	Checks   []ModelCheckResult `json:"checks"`  // the latest, oldest first, at most mcWindow
	Used     []string           `json:"used"`    // prompts this node has been sent, until the pool is used up
	LastSeen time.Time          `json:"last_seen"`
	HeldAt   time.Time          `json:"held_at,omitzero"` // when the check held it (withholding on)
}

// Status is the node's result over its window: the verdict, the largest check KL and the
// confident flips in it.
func (s *ModelCheckNode) Status(th modelcheck.Thresholds) (verdict string, worstKL float64, flips int) {
	if len(s.Checks) == 0 {
		return MCUnknown, 0, 0
	}
	fails := 0
	for _, c := range s.Checks {
		worstKL = max(worstKL, c.KL)
		flips += c.Flips
		if c.Verdict == modelcheck.Fail {
			fails++
		}
	}
	switch {
	case fails >= 2 || flips > th.MaxFlips:
		return MCFail, worstKL, flips
	case fails == 1:
		return MCSuspect, worstKL, flips
	}
	return MCPass, worstKL, flips
}

// ModelCheckState is the result file (state_dir/model-check.json).
type ModelCheckState struct {
	ObservingSince time.Time                  `json:"observing_since"`
	LastSaved      time.Time                  `json:"last_saved"` // a gap of more than mcGap means the check was off: observing starts again
	Thresholds     modelcheck.Thresholds      `json:"thresholds"`
	Withhold       bool                       `json:"withhold"` // the switch as configured (not whether it is in effect yet)
	Nodes          map[string]*ModelCheckNode `json:"nodes"`
	Uses           map[string]int             `json:"uses"` // prompt id: times sent, over all nodes
}

// ReadModelCheckState reads the result file the running gateway saves in stateDir.
func ReadModelCheckState(stateDir string) (*ModelCheckState, error) {
	b, err := os.ReadFile(filepath.Join(stateDir, ModelCheckFile))
	if err != nil {
		return nil, err
	}
	var s ModelCheckState
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, fmt.Errorf("%s: %w", ModelCheckFile, err)
	}
	return &s, nil
}

// mcRef is a loaded reference: by prompt id, in file order.
type mcRef struct {
	ref  *modelcheck.Reference
	byID map[string]*modelcheck.Result
	ids  []string
}

// mcAnswer is one probe prompt's answer, waiting for the rest of its group.
type mcAnswer struct {
	id    string
	model string // the reference it answers: a group never mixes two (a node may come back with another model)
	toks  []modelcheck.Token
	bad   string // why the answer cannot be compared (no logprobs, an error), "" if it can
}

// modelChecker runs the check. Its state is guarded by mu.
type modelChecker struct {
	cfg   ModelCheckConfig
	th    modelcheck.Thresholds
	refs  map[string]*mcRef // by model name
	cover float64
	path  string // the result file, "" without a state_dir
	log   *slog.Logger

	mu      sync.Mutex
	st      ModelCheckState
	next    map[string]time.Time  // node id: when its next probe prompt is due
	pending map[string][]mcAnswer // node id: answers of its check in progress
	running map[string]bool       // node id: a probe is in flight
	dirty   bool
	rng     *rand.Rand
}

func (mc *modelChecker) now() time.Time {
	if mc.cfg.Now != nil {
		return mc.cfg.Now()
	}
	return time.Now()
}

// newModelChecker loads the references and the saved results.
func newModelChecker(cfg ModelCheckConfig, stateDir string, log *slog.Logger) (*modelChecker, error) {
	if len(cfg.References) == 0 {
		return nil, errors.New("model_check: enabled without references")
	}
	if cfg.IntervalSec == 0 {
		cfg.IntervalSec = 1800
	}
	if cfg.RecheckSec == 0 {
		cfg.RecheckSec = 300
	}
	if cfg.AdmitSec == 0 {
		cfg.AdmitSec = 20
	}
	if cfg.IntervalSec < 60 || cfg.RecheckSec < 30 || cfg.AdmitSec < 5 {
		return nil, fmt.Errorf("model_check: interval_seconds %d (at least 60), recheck_seconds %d (at least 30) and admit_seconds %d (at least 5)",
			cfg.IntervalSec, cfg.RecheckSec, cfg.AdmitSec)
	}
	mc := &modelChecker{cfg: cfg, th: modelcheck.DefaultThresholds, refs: map[string]*mcRef{}, cover: defaultCoverShare, log: log,
		next: map[string]time.Time{}, pending: map[string][]mcAnswer{}, running: map[string]bool{},
		rng: rand.New(rand.NewPCG(rand.Uint64(), rand.Uint64()))}
	if cfg.CoverShare != nil {
		if *cfg.CoverShare < 0 || *cfg.CoverShare > 1 {
			return nil, fmt.Errorf("model_check: cover_share %v: want 0 to 1", *cfg.CoverShare)
		}
		mc.cover = *cfg.CoverShare
	}
	for _, r := range cfg.References {
		if r.Model == "" || r.File == "" || len(r.SHA256) != 64 {
			return nil, fmt.Errorf("model_check: a reference needs model, file and its sha256 (64 hex digits): %+v", r)
		}
		if mc.refs[r.Model] != nil {
			return nil, fmt.Errorf("model_check: two references for model %s", r.Model)
		}
		ref, err := modelcheck.LoadFile(r.File, r.SHA256)
		if err != nil {
			return nil, fmt.Errorf("model_check: %w", err)
		}
		if ref.Request.Thinking || ref.Request.TopLogprobs != modelcheck.DefaultRequest.TopLogprobs {
			return nil, fmt.Errorf("model_check: %s was recorded with %+v; the check sends %+v", r.File, ref.Request, modelcheck.DefaultRequest)
		}
		if len(ref.Results) < 4*mc.th.GroupSize {
			return nil, fmt.Errorf("model_check: %s has %d prompts; a pool needs at least %d", r.File, len(ref.Results), 4*mc.th.GroupSize)
		}
		m := &mcRef{ref: ref, byID: map[string]*modelcheck.Result{}}
		for i := range ref.Results {
			m.byID[ref.Results[i].ID] = &ref.Results[i]
			m.ids = append(m.ids, ref.Results[i].ID)
		}
		mc.refs[r.Model] = m
	}
	mc.st = ModelCheckState{Nodes: map[string]*ModelCheckNode{}, Uses: map[string]int{}}
	if stateDir != "" {
		mc.path = filepath.Join(stateDir, ModelCheckFile)
		if s, err := ReadModelCheckState(stateDir); err == nil {
			mc.st = *s
			if mc.st.Nodes == nil {
				mc.st.Nodes = map[string]*ModelCheckNode{}
			}
			if mc.st.Uses == nil {
				mc.st.Uses = map[string]int{}
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			log.Error("model check results not read; starting afresh", "err", err)
		}
	}
	now := mc.now()
	if !mc.st.ObservingSince.IsZero() && now.Sub(mc.st.LastSaved) > mcGap {
		// The check was off (or the gateway down) for a while: the 3 days of observing before
		// withholding count from now, not from a period long past (code review, 2026-11-20).
		log.Info("model check: off since its last save; observing starts again", "last_saved", mc.st.LastSaved)
		mc.st.ObservingSince = time.Time{}
	}
	if mc.st.ObservingSince.IsZero() {
		mc.st.ObservingSince = now.UTC()
	}
	mc.st.Thresholds, mc.st.Withhold = mc.th, cfg.Withhold
	mc.dirty = true
	return mc, nil
}

// withholding reports whether a failing node is held now: the switch is on and the check has
// observed long enough.
func (mc *modelChecker) withholding() bool {
	mc.mu.Lock()
	defer mc.mu.Unlock()
	return mc.withholdingLocked()
}

func (mc *modelChecker) withholdingLocked() bool {
	return mc.cfg.Withhold && !mc.now().Before(mc.st.ObservingSince.Add(mcObserveFirst))
}

// save writes the result file if anything changed.
func (mc *modelChecker) save() error {
	if mc == nil || mc.path == "" {
		return nil
	}
	mc.mu.Lock()
	now := mc.now()
	for id, s := range mc.st.Nodes { // also when nothing else changed: an old record goes
		if now.Sub(s.LastSeen) > mcKeepNodes && !mc.running[id] {
			delete(mc.st.Nodes, id)
			mc.dirty = true
		}
	}
	if !mc.dirty && now.Sub(mc.st.LastSaved) < time.Hour {
		mc.mu.Unlock()
		return nil
	}
	mc.st.LastSaved = now.UTC()
	b, err := json.MarshalIndent(mc.st, "", " ")
	mc.dirty = false
	mc.mu.Unlock()
	if err != nil {
		return err
	}
	tmp := mc.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		mc.markDirty()
		return err
	}
	if err := os.Rename(tmp, mc.path); err != nil {
		mc.markDirty()
		return err
	}
	return nil
}

func (mc *modelChecker) markDirty() {
	mc.mu.Lock()
	mc.dirty = true
	mc.mu.Unlock()
}

// snapshot is a deep copy of the state, for the operations page.
func (mc *modelChecker) snapshot() ModelCheckState {
	mc.mu.Lock()
	defer mc.mu.Unlock()
	s := mc.st
	s.Nodes = make(map[string]*ModelCheckNode, len(mc.st.Nodes))
	for id, n := range mc.st.Nodes {
		c := *n
		c.Checks = slices.Clone(n.Checks)
		c.Used = nil
		s.Nodes[id] = &c
	}
	s.Uses = nil
	return s
}

// expDelay is a random delay with the given mean (exponential), at least a second (or half the
// mean, when that is shorter: tests).
func (mc *modelChecker) expDelay(mean time.Duration) time.Duration {
	d := time.Duration(mc.rng.ExpFloat64() * float64(mean))
	return max(d, min(time.Second, mean/2))
}

func (mc *modelChecker) interval() time.Duration {
	if mc.cfg.Interval > 0 {
		return mc.cfg.Interval
	}
	return time.Duration(mc.cfg.IntervalSec) * time.Second
}

func (mc *modelChecker) recheck() time.Duration {
	if mc.cfg.Recheck > 0 {
		return mc.cfg.Recheck
	}
	return time.Duration(mc.cfg.RecheckSec) * time.Second
}

func (mc *modelChecker) admit() time.Duration {
	if mc.cfg.Admit > 0 {
		return mc.cfg.Admit
	}
	return time.Duration(mc.cfg.AdmitSec) * time.Second
}

// anonAdmitted reports whether the record s admits a node without an account (anon.go): anonAdmitChecks
// passed checks in its window, and its result is not fail. (Before admission one failed check holds it:
// mcFileLocked.)
func anonAdmitted(s *ModelCheckNode, th modelcheck.Thresholds) bool {
	if s == nil || !s.HeldAt.IsZero() {
		return false
	}
	if v, _, _ := s.Status(th); v == MCFail {
		return false
	}
	return passed(s) >= anonAdmitChecks
}

// passed counts the passed checks in s's window.
func passed(s *ModelCheckNode) int {
	c := 0
	for _, x := range s.Checks {
		if x.Verdict == modelcheck.Pass {
			c++
		}
	}
	return c
}

// admitted reports whether the check admits the node without an account id now.
func (mc *modelChecker) admitted(id string) bool {
	mc.mu.Lock()
	defer mc.mu.Unlock()
	return anonAdmitted(mc.st.Nodes[id], mc.th)
}

// passedInWindow counts node id's passed checks in its window.
func (mc *modelChecker) passedInWindow(id string) int {
	mc.mu.Lock()
	defer mc.mu.Unlock()
	if s := mc.st.Nodes[id]; s != nil {
		return passed(s)
	}
	return 0
}

// meanLocked is the mean wait before n's next probe prompt: shortest for a node without an account until
// it is admitted, short until a node has a result and while a failed check waits for its re-check, long
// otherwise.
func (mc *modelChecker) meanLocked(n *nodeConn) time.Duration {
	s := mc.st.Nodes[n.id]
	if n.anon && !anonAdmitted(s, mc.th) {
		return mc.admit()
	}
	if s == nil || len(s.Checks) == 0 || s.Checks[len(s.Checks)-1].Verdict != modelcheck.Pass {
		return mc.recheck()
	}
	return mc.interval()
}

// checkedModel is the model of n the check has a reference for, or "".
func (mc *modelChecker) checkedModel(n *nodeConn) string {
	for _, m := range n.models {
		if mc.refs[m.Name] != nil {
			return m.Name
		}
	}
	return ""
}

// checks reports whether the check is for node n at all: every node announcing a model with a
// reference.
func (mc *modelChecker) checks(n *nodeConn) bool {
	return mc != nil && mc.checkedModel(n) != ""
}

// coverFor reports whether a person's request to n also asks for the probes' logprobs: a share of
// the requests without tools to nodes the check is for and that have passed a check.
func (mc *modelChecker) coverFor(n *nodeConn, body map[string]any) bool {
	if mc == nil || mc.cover <= 0 || hasTools(body) || !mc.checks(n) {
		return false
	}
	mc.mu.Lock()
	defer mc.mu.Unlock()
	// Only a node that has passed a check, so its engine is known to answer with logprobs: one
	// that refused them would fail a person's request (code review, 2026-11-20).
	if s := mc.st.Nodes[n.id]; s == nil || s.Pass == 0 {
		return false
	}
	return mc.rng.Float64() < mc.cover
}

// pickPromptLocked chooses the next prompt for a node: one it has not been sent, among those sent
// least often over all nodes, at random. When the node has had them all, its list starts again.
func (mc *modelChecker) pickPromptLocked(s *ModelCheckNode, r *mcRef) string {
	used := map[string]bool{}
	for _, id := range s.Used {
		used[id] = true
	}
	inGroup := map[string]bool{} // the check in progress never has one prompt twice, also when the list starts again
	for _, a := range mc.pending[s.Node] {
		inGroup[a.id] = true
	}
	var free []string
	for _, id := range r.ids {
		if !used[id] {
			free = append(free, id)
		}
	}
	if len(free) == 0 {
		mc.log.Info("model check: a node has had every probe prompt; its list starts again", "node", s.Node, "prompts", len(r.ids))
		s.Used = nil
		for _, id := range r.ids {
			if !inGroup[id] {
				free = append(free, id)
			}
		}
	}
	least := math.MaxInt
	for _, id := range free {
		least = min(least, mc.st.Uses[id])
	}
	var pick []string
	for _, id := range free {
		if mc.st.Uses[id] == least {
			pick = append(pick, id)
		}
	}
	id := pick[mc.rng.IntN(len(pick))]
	s.Used = append(s.Used, id)
	mc.st.Uses[id]++
	return id
}

// nodeLocked is n's record, made on first sight.
func (mc *modelChecker) nodeLocked(n *nodeConn, model string, now time.Time) *ModelCheckNode {
	s := mc.st.Nodes[n.id]
	if s == nil {
		s = &ModelCheckNode{Node: n.id}
		mc.st.Nodes[n.id] = s
	}
	s.Operator, s.Model, s.LastSeen = n.operator, model, now.UTC()
	return s
}

// tick starts the probes that are due, one per node at most.
func (g *Gateway) mcTick() {
	mc := g.mc
	now := mc.now()
	g.mu.Lock()
	nodes := make([]*nodeConn, 0, len(g.node))
	for _, n := range g.node {
		nodes = append(nodes, n)
	}
	g.mu.Unlock()
	sort.Slice(nodes, func(i, j int) bool { return nodes[i].id < nodes[j].id })
	for _, n := range nodes { // nodes without an account: admitted, or no longer (a hold, a failing result)
		if n.anon {
			g.setAdmitted(n, mc.admitted(n.id))
		}
	}
	live := map[string]bool{}
	for _, n := range nodes {
		live[n.id] = true
		if !mc.checks(n) {
			continue
		}
		model := mc.checkedModel(n)
		g.mu.Lock()
		held := n.held
		g.mu.Unlock()
		mc.mu.Lock() // never under g.mu: mc.mu is not taken inside g.mu anywhere
		s := mc.nodeLocked(n, model, now)
		next, ok := mc.next[n.id]
		if !ok { // first sight since it connected: its first prompt comes at a random time
			mc.next[n.id] = now.Add(mc.expDelay(mc.meanLocked(n)))
			mc.dirty = true
			mc.mu.Unlock()
			continue
		}
		if !s.HeldAt.IsZero() {
			if held || mc.running[n.id] { // held, or its hold is being applied
				mc.mu.Unlock()
				continue
			}
			// The operator cleared the hold (`accounts node-clear`): the node starts afresh.
			mc.log.Info("model check: a held node was cleared; its checks start afresh", "node", n.id)
			s.HeldAt, s.Checks = time.Time{}, nil
			mc.pending[n.id] = nil
			mc.dirty = true
		}
		if mc.running[n.id] || now.Before(next) {
			mc.mu.Unlock()
			continue
		}
		mc.mu.Unlock()
		if !g.mcReserve(n, model) { // busy, paused, held or wanted by a waiting request: soon again
			mc.mu.Lock()
			mc.next[n.id] = now.Add(30*time.Second + time.Duration(mc.rng.Int64N(int64(60*time.Second))))
			mc.mu.Unlock()
			continue
		}
		mc.mu.Lock()
		r := mc.refs[model]
		id := mc.pickPromptLocked(s, r)
		mc.running[n.id] = true
		mc.dirty = true
		mc.mu.Unlock()
		g.bg.Add(1)
		go func() {
			defer g.bg.Done()
			g.mcProbe(n, model, r, id)
		}()
	}
	// A node that left starts afresh when it is back: its next prompt at a new random time, and
	// the answers of an unfinished check dropped (they would mix two sessions' engines).
	mc.mu.Lock()
	for id := range mc.next {
		if !live[id] && !mc.running[id] {
			delete(mc.next, id)
			delete(mc.pending, id)
		}
	}
	mc.mu.Unlock()
}

// mcReserve takes a free slot of n for a probe, as pick does for a request, unless n may not get
// requests now or a request is waiting for a node: probes never make a person (or science) wait. A node
// without an account not admitted yet serves no waiting request, so they do not hold its probes back.
func (g *Gateway) mcReserve(n *nodeConn, model string) bool {
	now := time.Now()
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.node[n.id] != n || n.held || n.registering || (len(g.waiting) > 0 && (!n.anon || n.admitted)) ||
		(n.pilot != "" && !now.Before(n.deadline)) {
		return false
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.paused || n.inflight >= n.slots(model) {
		return false
	}
	n.inflight++
	return true
}

// mcWanted reports whether a request waiting for a node could use n.
func (g *Gateway) mcWanted(n *nodeConn) bool {
	now := time.Now()
	g.mu.Lock()
	defer g.mu.Unlock()
	for w := range g.waiting {
		if w.usable(n, now, g.thresholdLocked(w, now)) {
			return true
		}
	}
	return false
}

// mcProbe sends one probe prompt to n on the slot mcReserve took, and files the answer.
func (g *Gateway) mcProbe(n *nodeConn, model string, r *mcRef, id string) {
	mc := g.mc
	res := r.byID[id]
	toks, ok, why := g.mcSend(n, model, r.ref.Request, res)
	now := mc.now()
	hold := ""
	mc.mu.Lock()
	if s := mc.st.Nodes[n.id]; s != nil { // nil: forgotten meanwhile (not seen for mcKeepNodes)
		hold = g.mcAnswerLocked(n, s, r, mcAnswer{id: id, model: model, toks: toks}, ok, why, now)
	}
	if hold == "" {
		mc.running[n.id] = false
	}
	mc.mu.Unlock()
	if hold != "" {
		// Outside mc.mu (holding takes other locks); the node counts as running until it is held,
		// so the scheduler does not take the hold as cleared meanwhile.
		g.holdNode(n, guardHit{rule: ruleModelCheck, why: hold}, "")
		mc.mu.Lock()
		mc.running[n.id] = false
		mc.mu.Unlock()
	}
}

// mcAnswerLocked files one probe answer (ok false: none came) and, once a group is complete, its
// check. It returns the reason to hold the node, or "". Caller holds mc.mu.
func (g *Gateway) mcAnswerLocked(n *nodeConn, s *ModelCheckNode, r *mcRef, a mcAnswer, ok bool, why string, now time.Time) string {
	mc := g.mc
	mc.dirty = true
	mc.next[n.id] = now.Add(mc.expDelay(mc.meanLocked(n)))
	switch {
	case !ok && strings.HasPrefix(why, mcNodeError):
		// The node answered with an error. An engine that refuses logprobs, or one that fails only
		// on probes, must not stay "unknown" or keep an old pass: it counts as a bad answer.
		a.toks, a.bad = nil, "answered with an error"
	case !ok: // no answer to judge: it left, stalled, or a person took the slot
		s.Dropped++
		mc.log.Debug("model check: probe prompt got no answer", "node", n.id, "why", why)
		return ""
	case len(a.toks) == 0: // an honest llama-server answers a probe with text and its logprobs
		a.bad = "an answer without logprobs"
	}
	// A group is one model's: answers to another reference's prompts (the node came back with
	// another model) are dropped.
	kept := mc.pending[n.id][:0]
	for _, p := range mc.pending[n.id] {
		if p.model == a.model {
			kept = append(kept, p)
		}
	}
	mc.pending[n.id] = append(kept, a)
	if len(mc.pending[n.id]) < mc.th.GroupSize {
		return ""
	}
	group := mc.pending[n.id]
	delete(mc.pending, n.id)
	hold := g.mcFileLocked(n, s, mc.evaluate(r, group, now))
	mc.next[n.id] = now.Add(mc.expDelay(mc.meanLocked(n))) // after a failed check, the re-check comes soon
	return hold
}

// mcFileLocked adds a check to the node's record and logs it. When withholding is in effect and
// the node's result is fail (also a node that was failing before withholding began), it marks the
// node held and returns why, for the caller to hold it; otherwise "". A node without an account
// (anon.go) is held whatever the switch: on any failed check before its admission, and on a fail
// result after it. Caller holds mc.mu.
func (g *Gateway) mcFileLocked(n *nodeConn, s *ModelCheckNode, c ModelCheckResult) string {
	mc := g.mc
	before, _, _ := s.Status(mc.th)
	wasAdmitted := anonAdmitted(s, mc.th)
	s.Checks = append(s.Checks, c)
	if len(s.Checks) > mcWindow {
		s.Checks = slices.Clone(s.Checks[len(s.Checks)-mcWindow:])
	}
	if c.Verdict == modelcheck.Pass {
		s.Pass++
	} else {
		s.Fail++
	}
	after, worst, flips := s.Status(mc.th)
	mc.log.Info("model check", "node", n.id, "operator", n.operator, "check", c.Verdict, "kl", fmt.Sprintf("%.4f", c.KL),
		"flips", c.Flips, "result", after, "worst_kl", fmt.Sprintf("%.4f", worst), "window_flips", flips)
	if n.anon {
		var why string
		switch {
		case !wasAdmitted && c.Verdict != modelcheck.Pass:
			why = "failed a model check before its admission (a node without an account)"
		case after == MCFail:
			why = fmt.Sprintf("failed the model check (a node without an account): largest KL %.4f (limit %.4f), %d confident flips in %d checks",
				worst, mc.th.MaxCheckKL, flips, len(s.Checks))
		default:
			if !wasAdmitted && anonAdmitted(s, mc.th) {
				mc.log.Info("model check: node without an account admitted", "node", n.id, "passed", passed(s))
			}
			return ""
		}
		if !s.HeldAt.IsZero() {
			return ""
		}
		s.HeldAt = mc.now().UTC()
		return why
	}
	if after != MCFail {
		return ""
	}
	if !mc.withholdingLocked() {
		if before != MCFail { // once, as it turns to fail
			mc.log.Warn("model check: node failed (observe-only: not held)", "node", n.id, "operator", n.operator,
				"reasons", strings.Join(c.Reasons, "; "))
		}
		return ""
	}
	if !s.HeldAt.IsZero() {
		return ""
	}
	s.HeldAt = mc.now().UTC()
	return fmt.Sprintf("failed the model check: largest KL %.4f (limit %.4f), %d confident flips in %d checks", worst,
		mc.th.MaxCheckKL, flips, len(s.Checks))
}

// ruleModelCheck is the hold rule of a node the model check stopped.
const ruleModelCheck = "model-check"

// evaluate compares a group of answers with the reference, by modelcheck.Compare. An answer
// without logprobs fails the check: an honest llama-server always returns them when asked.
func (mc *modelChecker) evaluate(r *mcRef, group []mcAnswer, now time.Time) ModelCheckResult {
	c := ModelCheckResult{At: now.UTC(), Verdict: modelcheck.Pass}
	for _, a := range group {
		c.Prompts = append(c.Prompts, a.id)
	}
	sub := *r.ref
	sub.Results = nil
	run := modelcheck.Reference{Format: modelcheck.Format, Kind: modelcheck.KindRun, Request: r.ref.Request}
	bad := 0
	var why []string
	for _, a := range group {
		ref := r.byID[a.id]
		if ref == nil { // not this reference's prompt: never compared (mcAnswerLocked keeps groups to one model)
			continue
		}
		if a.bad != "" {
			bad++
			if !slices.Contains(why, a.bad) {
				why = append(why, a.bad)
			}
			continue
		}
		sub.Results = append(sub.Results, *ref)
		toks := a.toks
		// The reference stops at max_tokens; the probe's answer goes on. Positions past the
		// reference's are not compared (one past it shows that the answer went on).
		if len(toks) > len(ref.Tokens) {
			cut := len(ref.Tokens)
			if len(ref.Tokens) < r.ref.Request.MaxTokens {
				cut++ // the reference ended by itself: going on is a difference
			}
			toks = toks[:min(cut, len(toks))]
		}
		run.Results = append(run.Results, modelcheck.Result{ID: a.id, Text: ref.Text, Tokens: toks})
	}
	if bad > 0 {
		c.Verdict = modelcheck.Fail
		c.Reasons = append(c.Reasons, fmt.Sprintf("%d of %d answers unusable: %s", bad, len(group), strings.Join(why, ", ")))
		return c
	}
	th := mc.th
	th.GroupSize = len(group)
	rep := modelcheck.Compare(&sub, &run, th)
	c.KL, c.Flips, c.Positions, c.Diverged = rep.CheckKLMax, rep.Flips, rep.Positions, rep.Diverged
	if rep.Verdict != modelcheck.Pass {
		// "could not check" is a fail here too: the answers were there, but not usable (positions
		// that are not log-probabilities, say).
		c.Verdict = modelcheck.Fail
		c.Reasons = append(c.Reasons, rep.Reasons...)
	}
	return c
}

// mcSend runs one probe prompt on n (the slot is reserved) and returns the answer's positions up
// to the reference's length plus one. ok is false when there is no answer to judge: the node
// failed it, left, stalled, or a person's request took the slot.
func (g *Gateway) mcSend(n *nodeConn, model string, rq modelcheck.Request, res *modelcheck.Result) (toks []modelcheck.Token, ok bool, why string) {
	body := map[string]any{
		"model": model, "stream": true, "stream_options": map[string]any{"include_usage": true},
		"messages":    []any{map[string]any{"role": "user", "content": res.Text}},
		"temperature": 0, "logprobs": true, "top_logprobs": rq.TopLogprobs,
		"chat_template_kwargs": map[string]any{"enable_thinking": rq.Thinking},
	}
	if g.cfg.DefaultMaxTokens > 0 { // what a request that sets no max_tokens gets (api.go)
		body["max_tokens"] = g.cfg.DefaultMaxTokens
	}
	raw, _ := json.Marshal(body)
	reqID := newID()
	p := newPendingReq()
	sr := g.addScience(n) // a person's request that waits for this node stops the probe (preempt.go)
	// Its operator's page lists it as any request in flight: a chat, or science on a pilot that
	// serves only science.
	class := ClassChat
	g.mu.Lock()
	if n.scienceOnly {
		class = ClassScience
	}
	g.mu.Unlock()
	n.mu.Lock()
	n.pending[reqID] = p
	if n.work == nil {
		n.work = map[string]nodeWork{}
	}
	n.work[reqID] = nodeWork{class: class, since: time.Now()}
	n.mu.Unlock()
	free := sync.OnceFunc(func() {
		n.mu.Lock()
		delete(n.work, reqID)
		n.inflight--
		n.mu.Unlock()
		g.dropScience(sr)
		g.signal()
	})
	stopping := false
	defer func() {
		close(p.done)
		if stopping {
			g.awaitStopped(n, reqID, p, free)
			return
		}
		n.mu.Lock()
		delete(n.pending, reqID)
		n.mu.Unlock()
		free()
	}()
	stop := func(reason string) (bool, string) {
		cancelOnNode(n, reqID)
		stopping = true
		return false, reason
	}
	sctx, scancel := context.WithTimeout(context.Background(), 15*time.Second)
	err := n.send(sctx, protocol.Msg{Type: protocol.TypeRequest, ReqID: reqID, Body: raw})
	scancel()
	if err != nil {
		return nil, false, "send: " + err.Error()
	}
	limit := len(res.Tokens) + 1
	chunks := 0
	// A waiting request of any class that could use this node stops the probe: preemption covers
	// people only, and science must not wait behind a probe either (code review, 2026-11-20).
	yield := time.NewTicker(time.Second)
	defer yield.Stop()
	broken := false // a logprob entry that is not one (no logprob, no alternatives)
	stall := time.NewTimer(time.Duration(g.cfg.FirstChunkTimeoutSec) * time.Second)
	defer stall.Stop()
	for {
		select {
		case <-stall.C:
			ok, why = stop("stalled")
			return nil, ok, why
		case <-sr.stop:
			ok, why = stop("a person's request took the slot")
			return nil, ok, why
		case <-yield.C:
			if g.mcWanted(n) {
				ok, why = stop("a waiting request wants the slot")
				return nil, ok, why
			}
		case <-g.stop:
			ok, why = stop("gateway stopping")
			return nil, ok, why
		case <-n.closed:
			return nil, false, "node left"
		case m := <-p.ch:
			stall.Reset(time.Duration(g.cfg.IdleTimeoutSec) * time.Second)
			switch m.Type {
			case protocol.TypeError:
				free()
				return nil, false, mcNodeError + errCategory(m.Error)
			case protocol.TypeDone:
				if broken {
					return nil, true, "" // judged as an answer without (usable) logprobs
				}
				return toks, true, ""
			case protocol.TypeChunk:
				chunks++
				if chunks > 2*mcMaxAnswer+256 {
					ok, why = stop("answer too long")
					return nil, ok, why
				}
				// Logprobs may come in a chunk of their own or several in one (llama-server holds
				// text back at an incomplete character): positions are counted from the entries.
				if len(toks) >= limit || broken {
					continue
				}
				t, err := modelcheck.ChunkTokens(m.Data, len(toks))
				if err != nil {
					broken = true
					continue
				}
				toks = append(toks, t...)
			}
		}
	}
}

// mcNodeError starts the reason of a probe the node answered with an error.
const mcNodeError = "node error: "

// mcStart starts the check's scheduler.
func (g *Gateway) mcStart() {
	tick := g.mc.cfg.Tick
	if tick <= 0 {
		tick = 5 * time.Second
	}
	g.every(tick, g.mcTick)
}
