// Package node is the AIs@home node agent: it connects outward to a gateway, receives
// requests, runs them on a local OpenAI-compatible engine (llama-server) and streams the
// results back with metered usage.
//
// SPDX-License-Identifier: Apache-2.0
package node

import (
	"bufio"
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/aisathome/aisathome/protocol"
)

// Version is the release this agent was built as, set at build time by
// scripts/build-release.sh: go build -ldflags "-X github.com/aisathome/aisathome/node.Version=v1.2.3".
// Other builds are "dev".
var Version = "dev"

// AgentVersion names this agent and its version ("aisathome-node/1.2.3") for the gateway's
// hello, the status events and the local UI.
func AgentVersion() string { return "aisathome-node/" + strings.TrimPrefix(Version, "v") }

// Config holds node settings.
type Config struct {
	// NodeID names the node. A token created on the account page ("nt-<id>.<secret>")
	// carries the id the gateway assigned, and that id is used; only tokens from the gateway's
	// config need a node_id here.
	NodeID     string               `json:"node_id"`
	GatewayURL string               `json:"gateway_url"` // ws(s)://host/v1/node/connect
	Token      string               `json:"token"`       // node token from the account page (or the operator)
	Upstream   string               `json:"upstream"`    // local engine, e.g. http://127.0.0.1:8000
	Models     []protocol.ModelInfo `json:"models"`
	// Heartbeat interval; defaults to 10 s.
	Heartbeat time.Duration `json:"-"`
	// HealthInterval is how often the engine's /health is checked; after 3 failures in a
	// row the node leaves the gateway until the engine is healthy again. Default 5 s.
	HealthInterval time.Duration `json:"-"`
	// FirstChunkTimeout and IdleTimeout end a request whose engine stalls: before the first
	// streamed chunk (prefill of a long prompt can take a minute) and between chunks.
	// Defaults 300 s and 120 s.
	FirstChunkTimeout time.Duration `json:"-"`
	IdleTimeout       time.Duration `json:"-"`
	// CancelWait bounds how long a request the gateway cancelled waits for the engine to let go
	// of it before the node ends it (the gateway keeps its slot until then). Default 90 s,
	// below the gateway's own limit (120 s).
	CancelWait time.Duration `json:"-"`
	// RefusedRetry is how long the node waits before it tries again after the gateway refused
	// its protocol version (the volunteer has to update the agent, or the gateway its range).
	// Default 10 min.
	RefusedRetry time.Duration `json:"-"`

	// Local UI and state (see status.go). StateFile keeps the pause switch, counters and
	// events across restarts ("" = memory only). RestartTrigger is a file that a root-owned
	// systemd path unit watches to restart the engine ("" = the agent cannot restart it).
	// Preset is a label for the engine settings, shown in the UI.
	StateFile      string `json:"state_file,omitempty"`
	RestartTrigger string `json:"restart_trigger,omitempty"`
	Preset         string `json:"preset,omitempty"`
}

// Agent is a running node agent.
type Agent struct {
	cfg    Config
	log    *slog.Logger
	client *http.Client

	mu       sync.Mutex
	cancels  map[string]context.CancelCauseFunc
	upstream int // requests whose engine request is still open

	// Local status (status.go), guarded by mu.
	ps          persisted // saved to cfg.StateFile
	connected   bool
	gatewayID   string
	connectedAt time.Time
	refused     string // the gateway's reason when it last refused this node's protocol version; "" once connected
	engine      string
	started     time.Time
	lastRestart time.Time
	push        func() // sends a heartbeat now; nil while disconnected
	now         func() time.Time

	saveMu     sync.Mutex
	saveFailed bool
}

// New creates an agent.
func New(cfg Config, log *slog.Logger) *Agent {
	if cfg.Heartbeat == 0 {
		cfg.Heartbeat = 10 * time.Second
	}
	if cfg.HealthInterval == 0 {
		cfg.HealthInterval = 5 * time.Second
	}
	if cfg.FirstChunkTimeout == 0 {
		cfg.FirstChunkTimeout = 300 * time.Second
	}
	if cfg.IdleTimeout == 0 {
		cfg.IdleTimeout = 120 * time.Second
	}
	if cfg.CancelWait == 0 {
		cfg.CancelWait = 90 * time.Second
	}
	if cfg.RefusedRetry == 0 {
		cfg.RefusedRetry = 10 * time.Minute
	}
	if id, ok := TokenNodeID(cfg.Token); ok && cfg.NodeID != id {
		if cfg.NodeID != "" {
			log.Warn("the node token belongs to another node id than node_id in the config; using the token's", "node", id)
		}
		cfg.NodeID = id
	}
	a := &Agent{cfg: cfg, log: log, client: &http.Client{}, cancels: map[string]context.CancelCauseFunc{},
		engine: EngineUnknown, now: time.Now}
	a.started = a.now()
	a.load()
	if a.ps.Paused {
		log.Info("starting paused (resume in the local UI)")
	}
	return a
}

// TokenNodeID returns the node id carried by a token from the account page:
// "nt-<id>.<secret>", where the id is "n-" and 8 hex digits.
func TokenNodeID(token string) (string, bool) {
	rest, ok := strings.CutPrefix(token, "nt-")
	if !ok {
		return "", false
	}
	id, _, ok := strings.Cut(rest, ".")
	h, isID := strings.CutPrefix(id, "n-")
	if !ok || !isID || len(h) != 8 || strings.Trim(h, "0123456789abcdef") != "" {
		return "", false
	}
	return id, true
}

// Run connects to the gateway and serves until ctx is cancelled, reconnecting with backoff.
func (a *Agent) Run(ctx context.Context) error {
	a.event("started", AgentVersion())
	backoff := time.Second
	for {
		// Only offer capacity while the engine answers: a hung engine (e.g. after a CUDA
		// fault, 2026-10-09) must not keep receiving requests through the gateway.
		if err := a.waitHealthy(ctx); err != nil {
			return err
		}
		start := time.Now()
		err := a.session(ctx)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if time.Since(start) > time.Minute {
			backoff = time.Second // the session was healthy; start over
		}
		// Jitter (±20%) so many nodes do not reconnect in lockstep after a gateway restart.
		wait := time.Duration(float64(backoff) * (0.8 + 0.4*rand.Float64()))
		var refused *RefusedError
		if errors.As(err, &refused) {
			wait = a.cfg.RefusedRetry
			a.log.Error("the gateway refused this node agent's protocol version",
				"reason", refused.Reason, "protocol", protocol.Version, "agent", AgentVersion(), "retry_in", wait)
			a.mu.Lock()
			a.refused = refused.Reason
			a.eventLocked("refused", refused.Reason)
			a.mu.Unlock()
			a.save()
		} else {
			a.log.Warn("disconnected from gateway, reconnecting", "err", err, "in", wait)
			a.event("disconnected", errText(err))
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(wait):
		}
		// Capped at 15 s: after an outage a node is back within seconds of the gateway
		// (a 1-minute cap kept nodes away for up to a minute on 2026-10-08).
		if backoff < 15*time.Second {
			backoff = min(backoff*2, 15*time.Second)
		}
	}
}

// healthy reports whether the engine answers /health with 200 within 3 s.
func (a *Agent) healthy(ctx context.Context) bool {
	hctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(hctx, http.MethodGet, strings.TrimRight(a.cfg.Upstream, "/")+"/health", nil)
	if err != nil {
		return false
	}
	resp, err := a.client.Do(req)
	ok := err == nil && resp.StatusCode == http.StatusOK
	if err == nil {
		resp.Body.Close()
	}
	if ctx.Err() == nil {
		a.setEngine(map[bool]string{true: EngineHealthy, false: EngineUnhealthy}[ok])
	}
	return ok
}

// waitHealthy blocks until the engine is healthy.
func (a *Agent) waitHealthy(ctx context.Context) error {
	logged := false
	for !a.healthy(ctx) {
		if !logged {
			a.log.Warn("engine not healthy; staying away from the gateway until it is", "upstream", a.cfg.Upstream)
			a.event("engine_unhealthy", "waiting for the engine before connecting")
			logged = true
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(a.cfg.HealthInterval):
		}
	}
	if logged {
		a.log.Info("engine healthy again")
		a.event("engine_healthy", "")
	}
	return nil
}

func (a *Agent) session(ctx context.Context) error {
	h := http.Header{}
	h.Set("Authorization", "Bearer "+a.cfg.Token)
	c, _, err := websocket.Dial(ctx, a.cfg.GatewayURL, &websocket.DialOptions{HTTPHeader: h})
	if err != nil {
		return fmt.Errorf("dial: %w", err)
	}
	defer c.CloseNow()
	c.SetReadLimit(16 << 20)

	var wmu sync.Mutex
	send := func(m protocol.Msg) error {
		b, err := json.Marshal(m)
		if err != nil { // never put an empty frame on the wire
			return err
		}
		wmu.Lock()
		defer wmu.Unlock()
		wctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		return c.Write(wctx, websocket.MessageText, b)
	}

	if err := send(protocol.Msg{Type: protocol.TypeHello, NodeID: a.cfg.NodeID, Protocol: protocol.LegacyVersion,
		ProtocolVersion: protocol.Version, Agent: AgentVersion(), Models: a.cfg.Models}); err != nil {
		return err
	}
	var welcome protocol.Msg
	if err := read(ctx, c, &welcome); err != nil || welcome.Type != protocol.TypeWelcome {
		var ce websocket.CloseError
		if errors.As(err, &ce) && ce.Code == protocol.CloseUnsupportedProtocol {
			return &RefusedError{Reason: ce.Reason}
		}
		return fmt.Errorf("no welcome from gateway: %v", err)
	}
	// A gateway from before protocol versions sends none: it speaks version 1 with everyone.
	a.log.Info("connected to gateway", "gateway", welcome.GatewayID, "protocol", cmp.Or(welcome.ProtocolVersion, protocol.Unversioned))

	sctx, stop := context.WithCancel(ctx)
	defer stop()
	heartbeat := func() error {
		a.mu.Lock()
		m := protocol.Msg{Type: protocol.TypeHeartbeat, Active: len(a.cancels), Paused: a.ps.Paused}
		a.mu.Unlock()
		return send(m)
	}
	a.mu.Lock()
	a.connected, a.gatewayID, a.connectedAt, a.refused = true, welcome.GatewayID, a.now(), ""
	a.push = func() { _ = heartbeat() }
	a.eventLocked("connected", welcome.GatewayID)
	paused := a.ps.Paused
	a.mu.Unlock()
	a.save()
	defer func() {
		a.mu.Lock()
		a.connected, a.push = false, nil
		a.mu.Unlock()
	}()
	if paused { // tell the gateway before it routes anything here
		if err := heartbeat(); err != nil {
			return err
		}
	}
	go func() { // engine health: leave the gateway after 3 failed checks in a row
		fails := 0
		for {
			select {
			case <-sctx.Done():
				return
			case <-time.After(a.cfg.HealthInterval):
			}
			if a.healthy(sctx) {
				fails = 0
				continue
			}
			if fails++; fails >= 3 && sctx.Err() == nil {
				a.log.Error("engine unhealthy, leaving the gateway", "upstream", a.cfg.Upstream)
				a.event("engine_unhealthy", "3 failed health checks; leaving the gateway")
				c.Close(websocket.StatusGoingAway, "engine unhealthy")
				stop()
				return
			}
		}
	}()
	go func() { // heartbeats
		t := time.NewTicker(a.cfg.Heartbeat)
		defer t.Stop()
		for {
			select {
			case <-sctx.Done():
				return
			case <-t.C:
				if heartbeat() != nil {
					stop()
					return
				}
			}
		}
	}()

	for {
		var m protocol.Msg
		if err := read(sctx, c, &m); err != nil {
			return err
		}
		switch m.Type {
		case protocol.TypeRequest:
			a.mu.Lock()
			if a.ps.Paused { // the gateway did not know yet (or predates "paused"): it retries elsewhere
				a.mu.Unlock()
				go send(protocol.Msg{Type: protocol.TypeError, ReqID: m.ReqID, Error: ErrPausedMsg})
				continue
			}
			rctx, cancel := context.WithCancelCause(sctx)
			a.cancels[m.ReqID] = cancel
			a.mu.Unlock()
			go func(m protocol.Msg) {
				defer func() {
					a.mu.Lock()
					delete(a.cancels, m.ReqID)
					a.mu.Unlock()
					cancel(nil)
				}()
				a.handle(sctx, rctx, m, send)
			}(m)
		case protocol.TypeCancel:
			a.mu.Lock()
			if cancel := a.cancels[m.ReqID]; cancel != nil {
				cancel(errCancelled)
			}
			a.mu.Unlock()
		}
	}
}

// RefusedError: the gateway does not accept this agent's protocol version. Reason is the
// gateway's text for the volunteer (untrusted; shown as text only).
type RefusedError struct{ Reason string }

func (e *RefusedError) Error() string { return "refused by the gateway: " + e.Reason }

// errCancelled is the cause of a request the gateway cancelled.
var errCancelled = errors.New("cancelled")

// handle runs one request against the local engine and streams chunks back (session ends with
// the connection). It always ends
// the request with done or error (unless the connection is gone): after a cancel, only once
// the engine has let go of it, so the gateway knows when the slot is free again.
func (a *Agent) handle(session, ctx context.Context, m protocol.Msg, send func(protocol.Msg) error) {
	parent := ctx
	a.mu.Lock()
	a.upstream++
	a.mu.Unlock()
	upstreamDone := sync.OnceFunc(func() { a.mu.Lock(); a.upstream--; a.mu.Unlock() })
	defer upstreamDone()
	fail := func(err error) {
		if context.Cause(parent) == errCancelled {
			upstreamDone()
			a.engineStopped(session)
			err = errCancelled
		}
		_ = send(protocol.Msg{Type: protocol.TypeError, ReqID: m.ReqID, Error: err.Error()})
		if parent.Err() == nil { // not a cancel by the gateway or the end of the session
			a.failed(err.Error())
		}
	}
	// Stall watchdog, running from the start: cancels the request if the engine sends
	// nothing for too long, before the headers or between chunks.
	ctx, cancelReq := context.WithCancel(ctx)
	defer cancelReq()
	stalled := make(chan struct{})
	alive := make(chan struct{}, 1)
	go func() {
		limit := a.cfg.FirstChunkTimeout
		for {
			select {
			case <-ctx.Done():
				return
			case <-alive:
				limit = a.cfg.IdleTimeout
			case <-time.After(limit):
				close(stalled)
				cancelReq()
				return
			}
		}
	}()
	isStalled := func() bool {
		select {
		case <-stalled:
			return true
		default:
			return false
		}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		strings.TrimRight(a.cfg.Upstream, "/")+"/v1/chat/completions", bytes.NewReader(m.Body))
	if err != nil {
		fail(err)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	t0 := time.Now()
	resp, err := a.client.Do(req)
	if err != nil {
		if isStalled() {
			fail(errors.New("engine stalled: no response within the timeout"))
		} else {
			fail(fmt.Errorf("engine unreachable: %w", err))
		}
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		var b bytes.Buffer
		b.ReadFrom(resp.Body)
		// Only a category goes back: the engine's response body can quote the prompt, and the
		// gateway must never receive (or log) prompt text in errors.
		if strings.Contains(b.String(), "exceed_context_size_error") || strings.Contains(b.String(), "exceeds the available context") {
			fail(errors.New("context_length_exceeded: the request exceeds this node's context"))
		} else {
			fail(fmt.Errorf("engine returned %d", resp.StatusCode))
		}
		return
	}

	var u protocol.Usage
	finished := false
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64<<10), 8<<20)
	for sc.Scan() {
		select {
		case alive <- struct{}{}:
		default:
		}
		line := sc.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		data := strings.TrimPrefix(line, "data: ")
		if data != "[DONE]" && !json.Valid([]byte(data)) {
			continue // e.g. the half line left when a cancel cuts the engine's stream
		}
		if data == "[DONE]" {
			finished = true
			break
		}
		if hasFinishReason(data) {
			finished = true // a completion marker: the stream did not just stop
		}
		meterChunk([]byte(data), &u)
		if err := send(protocol.Msg{Type: protocol.TypeChunk, ReqID: m.ReqID, Data: json.RawMessage(data)}); err != nil {
			return // gateway gone; the session will reconnect
		}
	}
	if isStalled() {
		fail(errors.New("engine stalled: no output within the timeout"))
		return
	}
	if err := sc.Err(); err != nil {
		fail(fmt.Errorf("reading engine stream: %w", err))
		return
	}
	u.DurationMs = time.Since(t0).Milliseconds()
	if !finished { // a clean EOF without [DONE] or finish_reason is a truncated answer, not a success
		fail(errors.New("engine stream ended before the answer was complete"))
		return
	}
	if send(protocol.Msg{Type: protocol.TypeDone, ReqID: m.ReqID, Usage: &u}) == nil {
		a.served(u)
	}
}

// engineStopped waits, at most CancelWait, until the engine works on no more requests than
// this node still has open with it: a cancelled request's HTTP request ends at once, but the
// engine may go on with it for a while. llama-server reports its busy slots at /slots
// (enabled by default); an engine without it counts as stopped once the request has ended.
func (a *Agent) engineStopped(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, a.cfg.CancelWait)
	defer cancel()
	for {
		busy, err := a.engineBusy(ctx)
		if errors.Is(err, errNoSlots) {
			return
		}
		a.mu.Lock()
		open := a.upstream
		a.mu.Unlock()
		if err == nil && busy <= open {
			return
		}
		select {
		case <-ctx.Done():
			a.log.Warn("engine still busy after a cancel; ending the request anyway", "busy_slots", busy, "wait", a.cfg.CancelWait)
			return
		case <-time.After(200 * time.Millisecond):
		}
	}
}

// errNoSlots: the engine has no /slots endpoint (another engine, or llama-server --no-slots).
var errNoSlots = errors.New("engine reports no slots")

// engineBusy returns how many of the engine's slots are processing a request. Another error
// than errNoSlots (a timeout: llama-server answers between batches) means ask again.
func (a *Agent) engineBusy(ctx context.Context) (int, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(a.cfg.Upstream, "/")+"/slots", nil)
	if err != nil {
		return 0, errNoSlots
	}
	resp, err := a.client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusNotImplemented {
		return 0, errNoSlots
	}
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("engine /slots returned %d", resp.StatusCode)
	}
	var slots []struct {
		IsProcessing bool `json:"is_processing"`
	}
	if json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&slots) != nil {
		return 0, errNoSlots
	}
	busy := 0
	for _, s := range slots {
		if s.IsProcessing {
			busy++
		}
	}
	return busy, nil
}

// errText is an error's message, or "" for nil.
func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// meterChunk extracts token counts from an OpenAI usage object or llama.cpp timings.
func meterChunk(data []byte, u *protocol.Usage) {
	var c struct {
		Usage *struct {
			PromptTokens        int `json:"prompt_tokens"`
			CompletionTokens    int `json:"completion_tokens"`
			PromptTokensDetails *struct {
				CachedTokens int `json:"cached_tokens"`
			} `json:"prompt_tokens_details"`
		} `json:"usage"`
		Timings *struct {
			CacheN  int `json:"cache_n"`
			PromptN int `json:"prompt_n"`
		} `json:"timings"`
	}
	if json.Unmarshal(data, &c) != nil {
		return
	}
	if c.Usage != nil {
		u.PromptTokens = c.Usage.PromptTokens
		u.CompletionTokens = c.Usage.CompletionTokens
		if c.Usage.PromptTokensDetails != nil {
			u.CachedTokens = c.Usage.PromptTokensDetails.CachedTokens
		}
	}
	if c.Timings != nil && c.Timings.CacheN > 0 {
		u.CachedTokens = c.Timings.CacheN
	}
}

func read(ctx context.Context, c *websocket.Conn, m *protocol.Msg) error {
	_, b, err := c.Read(ctx)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, m)
}

// hasFinishReason reports whether a streamed chunk carries a finish_reason for any choice
// (parsed, so JSON spacing does not matter).
func hasFinishReason(data string) bool {
	var c struct {
		Choices []struct {
			FinishReason *string `json:"finish_reason"`
		} `json:"choices"`
	}
	if json.Unmarshal([]byte(data), &c) != nil {
		return false
	}
	for _, ch := range c.Choices {
		if ch.FinishReason != nil && *ch.FinishReason != "" {
			return true
		}
	}
	return false
}
