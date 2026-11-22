// Package setupflow is the Windows node's first-run setup behind the local page's /setup: the
// card and driver check, the model download with progress and resume, the speed test with the
// tier's own engine, and the node token. It writes node.json once every step has passed, and
// the agent then starts as configured (Deps.Reload). The steps themselves are package setup's;
// this package runs them one at a time, keeps their progress for the page and never holds the
// token in its state. The token comes pasted, or by linking the node to an account in the browser
// (Link, package accountlink): the gateway hands it to the node once the person approves.
//
// SPDX-License-Identifier: Apache-2.0
package setupflow

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/aisathome/aisathome/node"
	"github.com/aisathome/aisathome/node/accountlink"
	"github.com/aisathome/aisathome/node/setup"
	"github.com/aisathome/aisathome/protocol"
)

// DefaultGatewayURL is where nodes connect unless the volunteer names another gateway.
const DefaultGatewayURL = "wss://api.aisathome.org/v1/node/connect"

// reGatewayURL is install.sh's check of --gateway-url.
var reGatewayURL = regexp.MustCompile(`^wss?://[A-Za-z0-9.-]+(:[0-9]+)?(/[A-Za-z0-9._~/-]*)?$`)

// Runner runs an engine until ctx ends (engine.Supervisor).
type Runner interface {
	Run(ctx context.Context) error
}

// Deps is what the flow works with; tests replace the card, the download and the engine.
type Deps struct {
	ConfigPath string // node.json, written at Start
	StateFile  string // the flow's last state, for `aisathome-node diagnose` ("" = not kept)
	Model      string // where the model goes (node.WindowsLayout().Model)
	Engine     string // the llama-server program
	Port       int    // the engine's port; default setup.EnginePort (node.json's engine.port)
	SMI        setup.SMI
	Driver     setup.DriverReq
	Tiers      []setup.Tier // default setup.Tiers
	Download   setup.DownloadOptions
	// NewEngine runs the tier's engine for the speed test; onExit reports that it ended on its
	// own (it did not load).
	NewEngine func(s setup.EngineSettings, onExit func(error, time.Duration)) Runner
	Speed     func(base string) setup.Engine // default setup.HTTPEngine
	MinTPS    float64                        // default setup.MinDecodeTPS; negative = 0 (measure only)
	Prompt    int                            // speed test prompt tokens; default setup.SpeedPromptTokens
	Predict   int                            // default setup.SpeedPredict
	Ready     time.Duration                  // how long the engine may take to load; default 15 min
	// Reload is called after Start wrote node.json, or when the volunteer leaves or enters the
	// setup: the agent then loads its config again.
	Reload func(setupMode bool)
	Log    *slog.Logger
	// NewLinker talks to the gateway at base (accountlink.APIBase) to link the node to an account;
	// default an accountlink.Client. LinkInfo is what the node reports about itself; default
	// accountlink.LocalInfo with SMI.
	NewLinker func(base string) Linker
	LinkInfo  func(ctx context.Context) protocol.LinkInfo
	// NewRegistrar registers the node without an account at the gateway at base (Anonymous); default an
	// accountlink.Client.
	NewRegistrar func(base string) Registrar
}

// Registrar registers a node without an account (accountlink.Client).
type Registrar interface {
	Register(ctx context.Context, acceptTerms bool) (protocol.Register, error)
}

// tokenSender is a Linker that can send a node without an account's token with its request for a code
// (accountlink.Client), so that linking moves that node to the account.
type tokenSender interface{ SetToken(token string) }

// Linker links the node to an account (accountlink.Client).
type Linker interface {
	Start(ctx context.Context, info protocol.LinkInfo) (protocol.LinkStart, error)
	Wait(ctx context.Context, s protocol.LinkStart) (protocol.LinkPoll, error)
}

// Link is the link to an account as the page shows it: the code and where to approve it, then how it
// ended. It never holds the device code or the token.
type Link struct {
	Status  string    `json:"status"`            // LinkWaiting, LinkLinked or LinkFailed
	Code    string    `json:"code,omitempty"`    // the code the person checks on the approval page
	URL     string    `json:"url,omitempty"`     // the approval page with the code filled in
	Page    string    `json:"page,omitempty"`    // the approval page, where the code can be typed
	Expires time.Time `json:"expires,omitempty"` // when the code stops working
	NodeID  string    `json:"node_id,omitempty"` // linked: the node's id on the account
	Error   string    `json:"error,omitempty"`   // failed: why (denied, expired, the gateway)
}

// Link states.
const (
	LinkWaiting = "waiting"
	LinkLinked  = "linked"
	LinkFailed  = "failed"
)

// Card is the chosen card as the page shows it.
type Card struct {
	Index    int    `json:"index"`
	Name     string `json:"name"`
	TotalMiB int    `json:"total_mib"`
	FreeMiB  int    `json:"free_mib"`
	Driver   string `json:"driver"`
	Cap      string `json:"compute_cap"`
	Tier     string `json:"tier,omitempty"`
	TierText string `json:"tier_text,omitempty"`
	Model    string `json:"model,omitempty"` // the tier's model name (alias)
	Size     int64  `json:"model_bytes,omitempty"`
	Several  bool   `json:"several"` // more cards: the engine is pinned to this one
	uuid     string
}

// Progress is the download's.
type Progress struct {
	Done     int64  `json:"done"`
	Total    int64  `json:"total"`
	Checking bool   `json:"checking"` // hashing bytes already on disk
	Complete bool   `json:"complete"` // the file is there and its SHA-256 matches
	Error    string `json:"error,omitempty"`
}

// Speed is the speed test's result.
type Speed struct {
	TPS          float64 `json:"tps,omitempty"`
	PromptTokens int     `json:"prompt_tokens,omitempty"`
	MinTPS       float64 `json:"min_tps"`
	Passed       bool    `json:"passed"`
	Phase        string  `json:"phase,omitempty"` // loading, measuring
	Error        string  `json:"error,omitempty"`
}

// State is the setup as the page shows it. It never holds the token.
type State struct {
	Mode       string    `json:"mode"`       // "setup" (the agent waits for the setup) or "run"
	Configured bool      `json:"configured"` // node.json has a token and the engine's model exists
	Busy       string    `json:"busy,omitempty"`
	Card       *Card     `json:"card,omitempty"`
	CardError  string    `json:"card_error,omitempty"`
	Download   Progress  `json:"download"`
	Speed      Speed     `json:"speed"`
	StartError string    `json:"start_error,omitempty"`
	Gateway    string    `json:"gateway"`           // the configured or default gateway URL
	NodeID     string    `json:"node_id,omitempty"` // from the configured token
	ModelPath  string    `json:"model_path"`        // where the model is downloaded
	Updated    time.Time `json:"updated"`           // when this state last changed
	Message    string    `json:"message,omitempty"` // the last step's outcome, for the page
	Link       *Link     `json:"link,omitempty"`    // the last link to an account (Flow.Link)
}

// Busy steps.
const (
	BusyCheck    = "check"
	BusyDownload = "download"
	BusySpeed    = "speed"
)

// ErrBusy: another step runs.
var ErrBusy = errors.New("another setup step is running; wait for it or cancel it")

// Flow is the setup. Its methods are safe for concurrent use.
type Flow struct {
	d   Deps
	ctx context.Context

	mu         sync.Mutex
	st         State
	tier       setup.Tier
	cancel     context.CancelFunc // the running step's
	linkCancel context.CancelFunc // the waiting link's
}

// New returns the flow; ctx bounds every step (the agent's life). mode is "setup" or "run".
func New(ctx context.Context, d Deps, mode string) *Flow {
	if d.Tiers == nil {
		d.Tiers = setup.Tiers
	}
	if d.MinTPS == 0 {
		d.MinTPS = setup.MinDecodeTPS
	} else if d.MinTPS < 0 {
		d.MinTPS = 0
	}
	if d.Prompt == 0 {
		d.Prompt = setup.SpeedPromptTokens
	}
	if d.Predict == 0 {
		d.Predict = setup.SpeedPredict
	}
	if d.Ready == 0 {
		d.Ready = 15 * time.Minute
	}
	if d.Speed == nil {
		d.Speed = func(base string) setup.Engine { return setup.HTTPEngine{BaseURL: base} }
	}
	if d.Log == nil {
		d.Log = slog.New(slog.DiscardHandler)
	}
	if d.Reload == nil {
		d.Reload = func(bool) {}
	}
	if d.NewLinker == nil {
		d.NewLinker = func(base string) Linker { return &accountlink.Client{Base: base} }
	}
	if d.NewRegistrar == nil {
		d.NewRegistrar = func(base string) Registrar { return &accountlink.Client{Base: base} }
	}
	if d.LinkInfo == nil {
		smi := d.SMI
		d.LinkInfo = func(ctx context.Context) protocol.LinkInfo { return accountlink.LocalInfo(ctx, smi) }
	}
	f := &Flow{d: d, ctx: ctx}
	f.st = State{Mode: mode, ModelPath: d.Model, Speed: Speed{MinTPS: d.MinTPS}}
	f.refreshConfig()
	return f
}

// Configured reports whether node.json is complete: the agent can run without the setup.
func Configured(cfgPath, model string) bool {
	cfg, err := readConfig(cfgPath)
	if err != nil || strings.TrimSpace(cfg.Token) == "" {
		return false
	}
	if cfg.Engine != nil {
		m := cfg.Engine.Model
		if m == "" {
			m = model
		}
		if fi, err := os.Stat(m); err != nil || !fi.Mode().IsRegular() {
			return false
		}
	}
	return true
}

func readConfig(path string) (node.Config, error) {
	var cfg node.Config
	b, err := os.ReadFile(path)
	if err != nil {
		return cfg, err
	}
	return cfg, json.Unmarshal(b, &cfg)
}

// refreshConfig reads what the page shows from node.json: whether it is complete, the gateway
// and the node id.
func (f *Flow) refreshConfig() {
	cfg, _ := readConfig(f.d.ConfigPath)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.st.Configured = Configured(f.d.ConfigPath, f.d.Model)
	f.st.Gateway = cfg.GatewayURL
	if f.st.Gateway == "" {
		f.st.Gateway = DefaultGatewayURL
	}
	f.st.NodeID = ""
	if id, ok := node.TokenNodeID(cfg.Token); ok {
		f.st.NodeID = id
	}
}

// State returns a snapshot.
func (f *Flow) State() State {
	f.mu.Lock()
	defer f.mu.Unlock()
	st := f.st
	if st.Card != nil {
		c := *st.Card
		st.Card = &c
	}
	return st
}

// changed saves the state for diagnostics. Caller holds f.mu.
func (f *Flow) changedLocked() {
	f.st.Updated = time.Now().UTC()
	if f.d.StateFile == "" {
		return
	}
	saved := f.st
	if saved.Link != nil { // a live code could be approved by whoever reads it: not in a file for diagnostics
		l := *saved.Link
		l.Code, l.URL = "", ""
		saved.Link = &l
	}
	b, err := json.MarshalIndent(saved, "", "  ")
	if err == nil {
		writeAtomic(f.d.StateFile, b)
	}
}

// begin marks step as running and returns its context, or ErrBusy.
func (f *Flow) begin(step string) (context.Context, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.st.Busy != "" {
		return nil, ErrBusy
	}
	if f.st.Mode != "setup" {
		return nil, errors.New("the node runs; choose \"Run the setup again\" first (it stops the node)")
	}
	ctx, cancel := context.WithCancel(f.ctx)
	f.st.Busy, f.cancel = step, cancel
	f.st.Message = ""
	f.changedLocked()
	return ctx, nil
}

func (f *Flow) end(msg string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.cancel != nil {
		f.cancel()
	}
	f.st.Busy, f.cancel, f.st.Message = "", nil, msg
	f.changedLocked()
}

// Cancel stops the running step (a download keeps its part for later).
func (f *Flow) Cancel() {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.cancel != nil {
		f.cancel()
	}
}

// Wait blocks until no step runs (tests).
func (f *Flow) Wait() {
	for {
		f.mu.Lock()
		busy := f.st.Busy
		f.mu.Unlock()
		if busy == "" {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// CheckCard reads the cards, chooses one and its tier, and checks the driver and the free
// memory. The agent's own engine is not running in setup mode, so the free memory is what other
// programs leave (Windows reports no per-process memory).
func (f *Flow) CheckCard() error {
	ctx, err := f.begin(BusyCheck)
	if err != nil {
		return err
	}
	go func() {
		card, tier, err := f.checkCard(ctx)
		f.mu.Lock()
		f.st.Card, f.tier, f.st.CardError = card, tier, ""
		f.st.Speed = Speed{MinTPS: f.d.MinTPS}
		if err != nil {
			f.st.CardError = err.Error()
		}
		f.mu.Unlock()
		if err != nil {
			f.end("")
			return
		}
		f.end(fmt.Sprintf("GPU %d (%s): the %s tier (%s).", card.Index, card.Name, card.Tier, card.TierText))
		f.checkModel()
	}()
	return nil
}

func (f *Flow) checkCard(ctx context.Context) (*Card, setup.Tier, error) {
	gpus, err := setup.ReadGPUs(ctx, f.d.SMI)
	if err != nil {
		return nil, setup.Tier{}, err
	}
	c, err := chooseCard(gpus, f.d.Tiers)
	card := &Card{Index: c.Index, Name: c.Name, TotalMiB: c.TotalMiB, FreeMiB: c.FreeMiB, Driver: c.Driver,
		Cap: c.Cap, Several: c.Several, uuid: c.UUID}
	if err != nil {
		return card, setup.Tier{}, err
	}
	card.Tier, card.TierText, card.Model, card.Size = c.Tier.Name, c.Tier.Text(), c.Tier.Alias, c.Tier.Model.Size
	if err := setup.CheckDriver(c.GPU, f.d.Driver); err != nil {
		return card, c.Tier, err
	}
	if err := setup.CheckFit(c); err != nil {
		return card, c.Tier, err
	}
	return card, c.Tier, nil
}

// chooseCard is setup.ChooseCard over tiers (tests use their own table).
func chooseCard(gpus []setup.GPU, tiers []setup.Tier) (setup.Choice, error) {
	c, err := setup.ChooseCard(gpus, nil)
	if err != nil && c.TotalMiB == 0 {
		return c, err
	}
	var best setup.Tier
	ok := false
	for _, t := range tiers {
		if c.TotalMiB >= t.MinTotalMiB && (!ok || t.MinTotalMiB > best.MinTotalMiB) {
			best, ok = t, true
		}
	}
	if !ok {
		return c, setup.TooSmall(c.GPU)
	}
	c.Tier = best
	return c, nil
}

// checkModel marks the download complete when the model file is there with the tier's size
// (its SHA-256 is checked again by the download step, which the page skips then).
func (f *Flow) checkModel() {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.st.Card == nil || f.tier.Name == "" {
		return
	}
	fi, err := os.Stat(f.d.Model)
	f.st.Download = Progress{Total: f.tier.Model.Size}
	if err == nil && fi.Size() == f.tier.Model.Size {
		f.st.Download.Done = f.tier.Model.Size
	}
	f.changedLocked()
}

// ready reports the tier when the card check passed, or an error for the page.
func (f *Flow) readyTier() (setup.Tier, *Card, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.st.Card == nil || f.tier.Name == "" || f.st.CardError != "" {
		return setup.Tier{}, nil, errors.New("check the card first")
	}
	c := *f.st.Card
	return f.tier, &c, nil
}

// Download fetches the tier's model (resumed from its .part, SHA-256 checked) with progress.
func (f *Flow) Download() error {
	tier, _, err := f.readyTier()
	if err != nil {
		return err
	}
	ctx, err := f.begin(BusyDownload)
	if err != nil {
		return err
	}
	go func() {
		o := f.d.Download
		o.Progress = func(done, total int64) {
			f.mu.Lock()
			f.st.Download = Progress{Done: done, Total: total}
			f.mu.Unlock()
		}
		o.HashProgress = func(done, total int64) {
			f.mu.Lock()
			f.st.Download = Progress{Done: done, Total: total, Checking: true}
			f.mu.Unlock()
		}
		f.mu.Lock()
		f.st.Download = Progress{Total: tier.Model.Size}
		f.mu.Unlock()
		err := setup.Download(ctx, tier.Model, f.d.Model, o)
		f.mu.Lock()
		switch {
		case err == nil:
			f.st.Download = Progress{Done: tier.Model.Size, Total: tier.Model.Size, Complete: true}
		case ctx.Err() != nil:
			f.st.Download.Checking = false
			f.st.Download.Error = "cancelled; the downloaded part is kept and the download continues from it"
		default:
			f.st.Download.Checking = false
			f.st.Download.Error = err.Error()
		}
		f.mu.Unlock()
		if err != nil {
			f.d.Log.Warn("setup: model download", "err", err)
			f.end("")
			return
		}
		f.end("The model is downloaded and checked.")
	}()
	return nil
}

// SpeedTest starts the tier's engine on the downloaded model, waits until it has loaded, and
// measures its decode speed after a long prompt; the engine stops again afterwards.
func (f *Flow) SpeedTest() error {
	tier, card, err := f.readyTier()
	if err != nil {
		return err
	}
	f.mu.Lock()
	complete := f.st.Download.Complete
	f.mu.Unlock()
	if !complete {
		return errors.New("download the model first")
	}
	ctx, err := f.begin(BusySpeed)
	if err != nil {
		return err
	}
	go func() {
		sp, err := f.speedTest(ctx, tier, card)
		sp.MinTPS = f.d.MinTPS
		if err != nil {
			sp.Error = err.Error()
			if ctx.Err() != nil && f.ctx.Err() == nil {
				sp.Error = "cancelled"
			}
		}
		sp.Passed = err == nil
		sp.Phase = ""
		f.mu.Lock()
		f.st.Speed = sp
		f.mu.Unlock()
		if err != nil {
			f.d.Log.Warn("setup: speed test", "err", err)
			f.end("")
			return
		}
		f.end(fmt.Sprintf("%.1f tokens/s after a %d-token prompt.", sp.TPS, sp.PromptTokens))
	}()
	return nil
}

func (f *Flow) phase(p string) {
	f.mu.Lock()
	f.st.Speed = Speed{MinTPS: f.d.MinTPS, Phase: p}
	f.mu.Unlock()
}

func (f *Flow) speedTest(ctx context.Context, tier setup.Tier, card *Card) (Speed, error) {
	s := tier.Engine(f.d.Engine, f.d.Model)
	if f.d.Port != 0 {
		s.Port = f.d.Port
	}
	if card.Several {
		s.CUDADevice = card.uuid
	}
	if err := s.CheckPort(); err != nil {
		return Speed{}, err
	}
	f.phase("loading")
	ectx, stop := context.WithCancel(ctx)
	exited := make(chan error, 1)
	r := f.d.NewEngine(s, func(err error, _ time.Duration) {
		select {
		case exited <- err:
		default:
		}
		stop() // one try: the setup does not restart an engine that did not load
	})
	done := make(chan struct{})
	go func() {
		defer close(done)
		r.Run(ectx)
	}()
	defer func() {
		stop()
		<-done // the engine is gone (the GPU free) before the step ends
	}()
	if err := waitHealth(ectx, s.Upstream(), f.d.Ready, exited); err != nil {
		return Speed{}, err
	}
	f.phase("measuring")
	got, err := setup.SpeedTest(ectx, f.d.Speed(s.Upstream()), f.d.Prompt, f.d.Predict)
	if err != nil {
		select {
		case e := <-exited:
			return Speed{}, fmt.Errorf("the engine ended during the speed test (%v); its log is engine.log in the node's logs folder", e)
		default:
		}
		return Speed{}, err
	}
	sp := Speed{TPS: got.TPS, PromptTokens: got.PromptTokens}
	g := setup.GPU{Index: card.Index, Name: card.Name}
	return sp, setup.CheckSpeed(g, got, f.d.MinTPS)
}

// waitHealth polls the engine's /health until it answers 200, the engine ends, or limit passes.
func waitHealth(ctx context.Context, base string, limit time.Duration, exited <-chan error) error {
	ctx, cancel := context.WithTimeout(ctx, limit)
	defer cancel()
	client := &http.Client{Timeout: 3 * time.Second}
	for {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, base+"/health", nil)
		if resp, err := client.Do(req); err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
		}
		ended := func(err error) error {
			return fmt.Errorf("the engine ended while loading the model (%v); its log is engine.log in the node's logs folder", err)
		}
		select {
		case err := <-exited:
			return ended(err)
		case <-ctx.Done():
			select { // the engine's end cancels ctx too: report it, not the cancel
			case err := <-exited:
				return ended(err)
			default:
			}
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				return fmt.Errorf("the engine did not load the model within %v", limit)
			}
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
}

// CheckToken checks a node token's form: one from the account page, which carries the node id.
func CheckToken(token string) (nodeID string, err error) {
	token = strings.TrimSpace(token)
	if token == "" {
		return "", errors.New("paste the node token from your account page")
	}
	id, ok := node.TokenNodeID(token)
	if !ok || len(token) > 256 || strings.ContainsFunc(token, func(r rune) bool { return r <= ' ' || r > '~' }) {
		return "", errors.New("this is not a node token: copy the whole token from your account page (it starts with nt-n-)")
	}
	if node.Anonymous(token) && len(token) < len("nt-a-00000000.")+40 {
		return "", errors.New("this is not a whole node token")
	}
	return id, nil
}

// CheckGateway checks a gateway URL as install.sh does; "" is the default.
func CheckGateway(u string) (string, error) {
	u = strings.TrimSpace(u)
	if u == "" {
		return DefaultGatewayURL, nil
	}
	if !reGatewayURL.MatchString(u) {
		return "", errors.New("the gateway must look like wss://host/v1/node/connect")
	}
	return u, nil
}

// Start writes node.json with the token, the gateway and the engine of the checked tier, once
// the card, the download and the speed test have passed, and starts the node.
func (f *Flow) Start(token, gateway string) error {
	id, err := CheckToken(token)
	if err != nil {
		return err
	}
	gw, err := CheckGateway(gateway)
	if err != nil {
		return err
	}
	tier, card, err := f.readyTier()
	if err != nil {
		return err
	}
	f.mu.Lock()
	busy, dl, speed := f.st.Busy, f.st.Download.Complete, f.st.Speed.Passed
	f.mu.Unlock()
	switch {
	case busy != "":
		return ErrBusy
	case !dl:
		return errors.New("download the model first")
	case !speed:
		return errors.New("run the speed test first")
	}
	eng := map[string]any{"tier": tier.Name}
	if card.Several {
		eng["cuda_device"] = card.uuid
	}
	if err := f.writeConfig(map[string]any{"token": strings.TrimSpace(token), "node_id": id, "gateway_url": gw, "engine": eng},
		"upstream", "models"); err != nil {
		return err
	}
	f.d.Log.Info("setup: node.json written; starting the node", "node", id, "tier", tier.Name, "gateway", gw)
	f.leave()
	return nil
}

// ChangeToken replaces the token (and gateway) of a configured node and restarts it.
func (f *Flow) ChangeToken(token, gateway string) error {
	id, err := CheckToken(token)
	if err != nil {
		return err
	}
	gw, err := CheckGateway(gateway)
	if err != nil {
		return err
	}
	if !Configured(f.d.ConfigPath, f.d.Model) {
		return errors.New("the node is not set up yet: finish the setup steps first")
	}
	if err := f.writeConfig(map[string]any{"token": strings.TrimSpace(token), "node_id": id, "gateway_url": gw}); err != nil {
		return err
	}
	f.d.Log.Info("setup: token changed; restarting the node", "node", id, "gateway", gw)
	f.leave()
	return nil
}

// Link links the node to an account in the browser, in place of a pasted token: it asks the gateway
// (gateway, as for Start; "" = the default) for a code and returns once the page can show it (State's
// Link); the node then waits for the approval, and on it starts with the token the gateway hands over
// (Start, or ChangeToken on a node already set up). A node not set up yet must have passed the steps
// that Start needs first. A second Link replaces a waiting one.
func (f *Flow) Link(gateway string) error {
	gw, err := CheckGateway(gateway)
	if err != nil {
		return err
	}
	configured := Configured(f.d.ConfigPath, f.d.Model)
	if !configured {
		if _, _, err := f.readyTier(); err != nil {
			return err
		}
		f.mu.Lock()
		busy, dl, speed := f.st.Busy, f.st.Download.Complete, f.st.Speed.Passed
		f.mu.Unlock()
		switch {
		case busy != "":
			return ErrBusy
		case !dl:
			return errors.New("download the model first")
		case !speed:
			return errors.New("run the speed test first")
		}
	}
	base, err := accountlink.APIBase(gw)
	if err != nil {
		return err
	}
	f.LinkCancel()
	ctx, cancel := context.WithCancel(f.ctx)
	sctx, scancel := context.WithTimeout(ctx, 30*time.Second)
	defer scancel()
	l := f.d.NewLinker(base)
	if cfg, err := readConfig(f.d.ConfigPath); err == nil && configured && node.Anonymous(cfg.Token) {
		if ts, ok := l.(tokenSender); ok {
			ts.SetToken(cfg.Token) // a node without an account: linking moves it to the account
		}
	}
	st, err := l.Start(sctx, f.d.LinkInfo(sctx))
	if err != nil {
		cancel()
		return err
	}
	f.mu.Lock()
	f.linkCancel = cancel
	f.st.Link = &Link{Status: LinkWaiting, Code: st.UserCode, URL: st.VerificationURIComplete, Page: st.VerificationURI,
		Expires: time.Now().Add(time.Duration(st.ExpiresIn) * time.Second).UTC()}
	f.changedLocked()
	f.mu.Unlock()
	f.d.Log.Info("setup: waiting for the link to an account to be approved", "gateway", gw)
	go func() {
		defer cancel()
		p, err := l.Wait(ctx, st)
		if ctx.Err() != nil {
			return // cancelled, or replaced by another link
		}
		if err == nil {
			if configured {
				err = f.ChangeToken(p.Token, gw)
			} else {
				err = f.Start(p.Token, gw)
			}
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.st.Link == nil || f.st.Link.Code != st.UserCode {
			return
		}
		if err != nil {
			f.d.Log.Warn("setup: link to an account", "err", err)
			f.st.Link = &Link{Status: LinkFailed, Error: err.Error()}
		} else {
			f.d.Log.Info("setup: linked to an account", "node", p.NodeID)
			f.st.Link = &Link{Status: LinkLinked, NodeID: p.NodeID}
		}
		f.changedLocked()
	}()
	return nil
}

// Anonymous starts the node without an account, in place of a token or a link: the gateway registers it
// (science only, no credit; accepted is the person's tick under the 18+ and terms line) and the node
// starts with the token it hands over, as Start does. Only in setup, once the steps Start needs passed,
// and only for a node that has no token yet.
func (f *Flow) Anonymous(gateway string, accepted bool) error {
	if !accepted {
		return errors.New("tick that you are 18 or older and accept the terms")
	}
	gw, err := CheckGateway(gateway)
	if err != nil {
		return err
	}
	if cfg, err := readConfig(f.d.ConfigPath); err == nil && strings.TrimSpace(cfg.Token) != "" {
		return errors.New("this node has a token already; it keeps it")
	}
	if _, _, err := f.readyTier(); err != nil {
		return err
	}
	f.mu.Lock()
	busy, dl, speed := f.st.Busy, f.st.Download.Complete, f.st.Speed.Passed
	f.mu.Unlock()
	switch {
	case busy != "":
		return ErrBusy
	case !dl:
		return errors.New("download the model first")
	case !speed:
		return errors.New("run the speed test first") // a machine that cannot serve never registers
	}
	base, err := accountlink.APIBase(gw)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(f.ctx, 30*time.Second)
	defer cancel()
	r, err := f.d.NewRegistrar(base).Register(ctx, true)
	if err != nil {
		return err
	}
	if err := f.Start(r.Token, gw); err != nil {
		return fmt.Errorf("registered as node %s, but starting failed: %w", r.NodeID, err)
	}
	f.d.Log.Info("setup: started without an account (science only)", "node", r.NodeID)
	return nil
}

// LinkCancel stops waiting for a link's approval; the code the page showed is then of no use.
func (f *Flow) LinkCancel() {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.linkCancel != nil {
		f.linkCancel()
		f.linkCancel = nil
	}
	if f.st.Link != nil && f.st.Link.Status == LinkWaiting {
		f.st.Link = nil
		f.changedLocked()
	}
}

// Enter switches to setup mode: the agent stops serving and its engine stops until Start or
// Leave.
func (f *Flow) Enter() error {
	f.mu.Lock()
	if f.st.Mode == "setup" {
		f.mu.Unlock()
		return nil
	}
	f.st.Mode = "setup"
	f.changedLocked()
	f.mu.Unlock()
	f.d.Reload(true)
	return nil
}

// Hold switches to setup mode without a reload, because the node could not run: reason says
// why, on the page.
func (f *Flow) Hold(reason string) {
	f.refreshConfig()
	f.mu.Lock()
	defer f.mu.Unlock()
	f.st.Mode, f.st.StartError = "setup", reason
	f.changedLocked()
}

// Leave goes back to running the node, when node.json is complete.
func (f *Flow) Leave() error {
	if !Configured(f.d.ConfigPath, f.d.Model) {
		return errors.New("the node is not set up yet: finish the setup steps first")
	}
	f.leave()
	return nil
}

func (f *Flow) leave() {
	f.Cancel()
	f.Wait()
	f.refreshConfig()
	f.mu.Lock()
	f.st.Mode, f.st.StartError = "run", ""
	f.changedLocked()
	f.mu.Unlock()
	f.d.Reload(false)
}

// writeConfig sets keys in node.json and deletes drop, keeping everything else. The file is
// written next to itself and renamed over it, so it inherits the data folder's permissions
// (the installer's: administrators and the service account only).
func (f *Flow) writeConfig(set map[string]any, drop ...string) error {
	m := map[string]json.RawMessage{}
	if b, err := os.ReadFile(f.d.ConfigPath); err == nil {
		if err := json.Unmarshal(b, &m); err != nil {
			return fmt.Errorf("%s is not valid JSON (%v); fix or delete it, then run the setup again", f.d.ConfigPath, err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	for k, v := range set {
		b, err := json.Marshal(v)
		if err != nil {
			return err
		}
		m[k] = b
	}
	for _, k := range drop {
		delete(m, k)
	}
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(f.d.ConfigPath), 0o755); err != nil {
		return err
	}
	return writeAtomic(f.d.ConfigPath, append(b, '\n'))
}

// writeAtomic writes b to path through a temporary file in the same folder.
func writeAtomic(path string, b []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}
