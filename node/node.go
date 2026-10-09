// Package node is the AIs@home node agent: it connects outward to a gateway, receives
// requests, runs them on a local OpenAI-compatible engine (llama-server) and streams the
// results back with metered usage.
//
// SPDX-License-Identifier: Apache-2.0
package node

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/aisathome/aisathome/protocol"
)

// AgentVersion is reported to the gateway in hello.
const AgentVersion = "aisathome-node/0.1.0"

// Config holds node settings.
type Config struct {
	NodeID     string               `json:"node_id"`
	GatewayURL string               `json:"gateway_url"` // ws(s)://host/v1/node/connect
	Token      string               `json:"token"`       // node token issued by the gateway operator
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
}

// Agent is a running node agent.
type Agent struct {
	cfg    Config
	log    *slog.Logger
	client *http.Client

	mu      sync.Mutex
	cancels map[string]context.CancelFunc
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
	return &Agent{cfg: cfg, log: log, client: &http.Client{}, cancels: map[string]context.CancelFunc{}}
}

// Run connects to the gateway and serves until ctx is cancelled, reconnecting with backoff.
func (a *Agent) Run(ctx context.Context) error {
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
		a.log.Warn("disconnected from gateway, reconnecting", "err", err, "in", wait)
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
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

// waitHealthy blocks until the engine is healthy.
func (a *Agent) waitHealthy(ctx context.Context) error {
	logged := false
	for !a.healthy(ctx) {
		if !logged {
			a.log.Warn("engine not healthy; staying away from the gateway until it is", "upstream", a.cfg.Upstream)
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
		b, _ := json.Marshal(m)
		wmu.Lock()
		defer wmu.Unlock()
		wctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		return c.Write(wctx, websocket.MessageText, b)
	}

	if err := send(protocol.Msg{Type: protocol.TypeHello, NodeID: a.cfg.NodeID, Protocol: protocol.Version,
		Agent: AgentVersion, Models: a.cfg.Models}); err != nil {
		return err
	}
	var welcome protocol.Msg
	if err := read(ctx, c, &welcome); err != nil || welcome.Type != protocol.TypeWelcome {
		return fmt.Errorf("no welcome from gateway: %v", err)
	}
	a.log.Info("connected to gateway", "gateway", welcome.GatewayID)

	sctx, stop := context.WithCancel(ctx)
	defer stop()
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
				a.mu.Lock()
				active := len(a.cancels)
				a.mu.Unlock()
				if send(protocol.Msg{Type: protocol.TypeHeartbeat, Active: active}) != nil {
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
			rctx, cancel := context.WithCancel(sctx)
			a.mu.Lock()
			a.cancels[m.ReqID] = cancel
			a.mu.Unlock()
			go func(m protocol.Msg) {
				defer func() {
					a.mu.Lock()
					delete(a.cancels, m.ReqID)
					a.mu.Unlock()
					cancel()
				}()
				a.handle(rctx, m, send)
			}(m)
		case protocol.TypeCancel:
			a.mu.Lock()
			if cancel := a.cancels[m.ReqID]; cancel != nil {
				cancel()
			}
			a.mu.Unlock()
		}
	}
}

// handle runs one request against the local engine and streams chunks back.
func (a *Agent) handle(ctx context.Context, m protocol.Msg, send func(protocol.Msg) error) {
	fail := func(err error) {
		_ = send(protocol.Msg{Type: protocol.TypeError, ReqID: m.ReqID, Error: err.Error()})
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
		fail(fmt.Errorf("engine returned %d: %s", resp.StatusCode, truncate(b.String(), 300)))
		return
	}

	var u protocol.Usage
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
		if data == "[DONE]" {
			break
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
	_ = send(protocol.Msg{Type: protocol.TypeDone, ReqID: m.ReqID, Usage: &u})
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

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
