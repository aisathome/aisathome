// Local status and controls of the node agent, for the local UI (package localui) and
// `aisathome-node status`: connection state, counters, recent events, pause and resume.
// Nothing here ever holds request content: no prompts, no answers, no engine response text.
//
// SPDX-License-Identifier: Apache-2.0
package node

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/aisathome/aisathome/node/engine"
	"github.com/aisathome/aisathome/protocol"
)

// States reported by Status.State.
const (
	StateServing      = "serving"      // connected, at least one request in flight
	StateConnected    = "connected"    // connected, idle
	StatePaused       = "paused"       // paused by the volunteer (connected or not)
	StateDisconnected = "disconnected" // not connected to the gateway
)

// Engine health as last checked.
const (
	EngineUnknown   = "unknown"
	EngineHealthy   = "healthy"
	EngineUnhealthy = "unhealthy"
	EngineStopped   = "stopped" // the agent runs the engine and it is stopped on request (GPU free)
)

// ErrPausedMsg is the error a paused node answers new requests with. The gateway retries such
// a request on another node (it fails before any output).
const ErrPausedMsg = "node paused"

// RestartCommand (platform_*.go) restarts the engine by hand when the agent may not.

// ErrRestartUnavailable means the agent has no way to restart the engine itself.
var ErrRestartUnavailable = errors.New("this agent cannot restart the engine; run: " + RestartCommand)

// ErrNoEngineControl means the agent does not run the engine itself, so it cannot stop or
// start it (Linux: systemd runs it).
var ErrNoEngineControl = errors.New("this agent does not run the engine itself")

// ErrRestartTooSoon limits engine restarts from the UI to one a minute.
var ErrRestartTooSoon = errors.New("an engine restart was requested less than a minute ago")

const (
	maxEvents       = 50
	restartInterval = time.Minute
)

// Event is one entry in the agent's recent history: what happened, never request content.
type Event struct {
	Time  time.Time `json:"time"`
	Kind  string    `json:"kind"` // started, connected, disconnected, refused, rejected, engine_unhealthy, engine_healthy, paused, resumed, request_failed, restart_requested, watchdog, engine_stopped, engine_started, engine_exited
	Text  string    `json:"text,omitempty"`
	Count int       `json:"count,omitempty"` // repeats folded into this entry (e.g. failed reconnects)
}

// Counters are the agent's own tally of requests (not the gateway's billing).
type Counters struct {
	Requests         int64 `json:"requests"` // completed
	Failed           int64 `json:"failed"`
	PromptTokens     int64 `json:"prompt_tokens"`
	CompletionTokens int64 `json:"completion_tokens"`
	CachedTokens     int64 `json:"cached_tokens"`
	BusyMs           int64 `json:"busy_ms"` // summed request durations (GPU time serving)
}

func (c *Counters) add(u protocol.Usage) {
	c.Requests++
	c.PromptTokens += int64(u.PromptTokens)
	c.CompletionTokens += int64(u.CompletionTokens)
	c.CachedTokens += int64(u.CachedTokens)
	c.BusyMs += u.DurationMs
}

// Status is a snapshot of the agent for the local UI.
type Status struct {
	State          string     `json:"state"`
	Connected      bool       `json:"connected"`
	Paused         bool       `json:"paused"`
	Engine         string     `json:"engine"`
	NodeID         string     `json:"node_id"`
	Gateway        string     `json:"gateway"`    // host of the gateway URL
	GatewayID      string     `json:"gateway_id"` // from welcome
	ConnectedSince *time.Time `json:"connected_since,omitempty"`
	StartedAt      time.Time  `json:"started_at"`
	Agent          string     `json:"agent"`
	Protocol       int        `json:"protocol"`          // the protocol version this agent speaks
	Refused        string     `json:"refused,omitempty"` // the gateway refused that version: its reason (until the node connects again)
	// Rejected is the gateway's reason when it last refused this node's token or hello ("HTTP
	// 401: unknown node token", "node id is reserved"), until the node connects again. Text
	// from the gateway: shown as text only.
	Rejected   string               `json:"rejected,omitempty"`
	Models     []protocol.ModelInfo `json:"models"`
	Preset     string               `json:"preset,omitempty"`
	SlotsInUse int                  `json:"slots_in_use"`
	SlotsTotal int                  `json:"slots_total"`
	Day        string               `json:"day"` // local date of Today
	Today      Counters             `json:"today"`
	Total      Counters             `json:"total"`
	Events     []Event              `json:"events"` // newest first
	CanRestart bool                 `json:"can_restart"`
	// EngineProcess is the engine process when the agent runs it itself (Config.EngineControl);
	// then the UI can also stop and start it.
	EngineProcess *engine.Status `json:"engine_process,omitempty"`
}

// persisted is what survives agent restarts (Config.StateFile): the pause switch, the
// counters and recent events. Counts only, no content.
type persisted struct {
	Paused bool     `json:"paused"`
	Day    string   `json:"day"`
	Today  Counters `json:"today"`
	Total  Counters `json:"total"`
	Events []Event  `json:"events"`
}

// load restores persisted state; a missing or broken file starts fresh.
func (a *Agent) load() {
	if a.cfg.StateFile == "" {
		return
	}
	b, err := os.ReadFile(a.cfg.StateFile)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			a.log.Warn("cannot read the state file; counters start at zero", "file", a.cfg.StateFile, "err", err)
		}
		return
	}
	var p persisted
	if err := json.Unmarshal(b, &p); err != nil {
		a.log.Warn("broken state file; counters start at zero", "file", a.cfg.StateFile, "err", err)
		return
	}
	if len(p.Events) > maxEvents {
		p.Events = p.Events[len(p.Events)-maxEvents:]
	}
	a.ps = p
}

// save writes the persisted state atomically. Caller must not hold a.mu.
func (a *Agent) save() {
	if a.cfg.StateFile == "" {
		return
	}
	a.saveMu.Lock()
	defer a.saveMu.Unlock()
	a.mu.Lock()
	b, err := json.Marshal(a.ps)
	a.mu.Unlock()
	if err != nil {
		return
	}
	tmp := a.cfg.StateFile + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		a.saveWarn(err)
		return
	}
	if err := os.Rename(tmp, a.cfg.StateFile); err != nil {
		a.saveWarn(err)
	}
}

func (a *Agent) saveWarn(err error) {
	if !a.saveFailed { // once, not on every request
		a.saveFailed = true
		a.log.Warn("cannot write the state file; counters will not survive a restart", "file", a.cfg.StateFile, "err", err)
	}
}

// rollLocked starts a new day's counters when the local date changed. Caller holds a.mu.
func (a *Agent) rollLocked() {
	if d := a.now().Format(time.DateOnly); a.ps.Day != d {
		a.ps.Day, a.ps.Today = d, Counters{}
	}
}

// eventLocked records an event, folding an exact repeat of the latest one. Caller holds a.mu.
func (a *Agent) eventLocked(kind, text string) {
	now := a.now()
	if n := len(a.ps.Events); n > 0 && a.ps.Events[n-1].Kind == kind && a.ps.Events[n-1].Text == text {
		e := &a.ps.Events[n-1]
		e.Time = now
		e.Count = max(e.Count, 1) + 1
		return
	}
	a.ps.Events = append(a.ps.Events, Event{Time: now, Kind: kind, Text: text})
	if len(a.ps.Events) > maxEvents {
		a.ps.Events = append([]Event(nil), a.ps.Events[len(a.ps.Events)-maxEvents:]...)
	}
}

// event records an event and saves the state.
func (a *Agent) event(kind, text string) {
	a.mu.Lock()
	a.eventLocked(kind, text)
	a.mu.Unlock()
	a.save()
}

// served counts a completed request.
func (a *Agent) served(u protocol.Usage) {
	a.mu.Lock()
	a.rollLocked()
	a.ps.Today.add(u)
	a.ps.Total.add(u)
	a.mu.Unlock()
	a.save()
}

// failed counts a failed request. msg is the agent's own error text (fixed phrases and local
// errors), never the engine's response body.
func (a *Agent) failed(msg string) {
	a.mu.Lock()
	a.rollLocked()
	a.ps.Today.Failed++
	a.ps.Total.Failed++
	a.eventLocked("request_failed", msg)
	a.mu.Unlock()
	a.save()
}

func (a *Agent) setEngine(state string) {
	a.mu.Lock()
	a.engine = state
	a.mu.Unlock()
}

// engineHeld reports whether the engine the agent runs is stopped on request: asked to stop
// (the supervisor may still be ending it) or found stopped.
func (a *Agent) engineHeld() bool {
	ctl := a.cfg.EngineControl
	if ctl == nil {
		return false
	}
	a.mu.Lock()
	held := a.held
	a.mu.Unlock()
	return held || ctl.Status().State == engine.StateStopped
}

func (a *Agent) engineState() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.engine
}

// setRejected records the gateway's reason for refusing the token or the hello, and an event.
func (a *Agent) setRejected(reason string) {
	if reason == "" {
		reason = "no reason given"
	}
	a.mu.Lock()
	a.rejected = reason
	a.eventLocked("rejected", reason)
	a.mu.Unlock()
	a.save()
}

// cleanReason makes the gateway's text safe to keep and show: one line of printable
// characters, at most 200 bytes.
func cleanReason(s string) string {
	s = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return ' '
		}
		return r
	}, s)
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > 200 {
		s = strings.ToValidUTF8(s[:200], "") + "..."
	}
	return s
}

// Paused reports whether the volunteer paused the node.
func (a *Agent) Paused() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.ps.Paused
}

// Pause stops taking new requests and tells the gateway at once; requests in flight finish.
// The switch survives restarts of the agent when a state file is configured.
func (a *Agent) Pause() { a.setPaused(true) }

// Resume takes requests again.
func (a *Agent) Resume() { a.setPaused(false) }

func (a *Agent) setPaused(p bool) {
	a.mu.Lock()
	if a.ps.Paused == p {
		a.mu.Unlock()
		return
	}
	a.ps.Paused = p
	a.linkAt = a.now() // the watchdog counts from here, not from before the pause
	if p {
		a.eventLocked("paused", "")
	} else {
		a.eventLocked("resumed", "")
	}
	push := a.push
	a.mu.Unlock()
	a.save()
	if p {
		a.log.Info("paused: no new requests; requests in flight finish")
	} else {
		a.log.Info("resumed")
	}
	if push != nil {
		go push() // a heartbeat now, not at the next tick
	}
}

// RestartEngine restarts llama-server. When the agent runs the engine itself
// (Config.EngineControl) it restarts it at once; requests in flight end. Otherwise it asks
// systemd by touching Config.RestartTrigger, which a root-owned systemd path unit watches (the
// agent itself has no privileges); restarting llama-server restarts the agent too (PartOf).
func (a *Agent) RestartEngine() error {
	ctl := a.cfg.EngineControl
	if ctl == nil && a.cfg.RestartTrigger == "" {
		return ErrRestartUnavailable
	}
	a.mu.Lock()
	if !a.lastRestart.IsZero() && a.now().Sub(a.lastRestart) < restartInterval {
		a.mu.Unlock()
		return ErrRestartTooSoon
	}
	a.lastRestart = a.now()
	a.eventLocked("restart_requested", "")
	a.mu.Unlock()
	a.save()
	a.log.Warn("engine restart requested from the local UI")
	if ctl != nil {
		a.mu.Lock()
		a.held = false
		a.mu.Unlock()
		ctl.Restart()
		return nil
	}
	return os.WriteFile(a.cfg.RestartTrigger, []byte(a.now().UTC().Format(time.RFC3339)+"\n"), 0o600)
}

// StopEngine stops the engine the agent runs, which frees the GPU, until StartEngine. The node
// leaves the gateway at once; requests in flight end.
func (a *Agent) StopEngine() error {
	ctl := a.cfg.EngineControl
	if ctl == nil {
		return ErrNoEngineControl
	}
	a.event("engine_stopped", "stopped from the local UI")
	a.log.Warn("engine stop requested from the local UI")
	ctl.Stop()
	a.mu.Lock()
	a.engine, a.held = EngineStopped, true
	leave := a.leave
	a.mu.Unlock()
	if leave != nil { // leave the gateway now, not after three failed health checks
		leave()
	}
	return nil
}

// StartEngine starts the engine the agent runs again, after StopEngine (or at once when it is
// waiting to restart after a crash).
func (a *Agent) StartEngine() error {
	ctl := a.cfg.EngineControl
	if ctl == nil {
		return ErrNoEngineControl
	}
	a.mu.Lock()
	a.held = false
	a.mu.Unlock()
	a.event("engine_started", "started from the local UI")
	a.log.Info("engine start requested from the local UI")
	ctl.Start()
	return nil
}

// EngineExited records that the engine the agent runs ended on its own (a crash, or it did not
// start) and when it starts again. err is the process's exit, never its output.
func (a *Agent) EngineExited(err error, restartIn time.Duration) {
	a.event("engine_exited", fmt.Sprintf("%v; starting it again in %v", err, restartIn.Round(time.Second)))
}

// Status returns a snapshot for the local UI.
func (a *Agent) Status() Status {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.rollLocked()
	s := Status{
		Connected:  a.connected,
		Paused:     a.ps.Paused,
		Engine:     a.engine,
		NodeID:     a.cfg.NodeID,
		Gateway:    gatewayHost(a.cfg.GatewayURL),
		GatewayID:  a.gatewayID,
		StartedAt:  a.started,
		Agent:      AgentVersion(),
		Protocol:   protocol.Version,
		Refused:    a.refused,
		Rejected:   a.rejected,
		Models:     append([]protocol.ModelInfo(nil), a.cfg.Models...),
		Preset:     a.cfg.Preset,
		SlotsInUse: len(a.cancels),
		Day:        a.ps.Day,
		Today:      a.ps.Today,
		Total:      a.ps.Total,
		CanRestart: a.cfg.RestartTrigger != "" || a.cfg.EngineControl != nil,
	}
	if ctl := a.cfg.EngineControl; ctl != nil {
		st := ctl.Status()
		s.EngineProcess = &st
	}
	if a.connected {
		t := a.connectedAt
		s.ConnectedSince = &t
	}
	for _, m := range a.cfg.Models {
		s.SlotsTotal += m.Slots
	}
	switch {
	case a.ps.Paused:
		s.State = StatePaused
	case !a.connected:
		s.State = StateDisconnected
	case s.SlotsInUse > 0:
		s.State = StateServing
	default:
		s.State = StateConnected
	}
	s.Events = make([]Event, 0, len(a.ps.Events))
	for i := len(a.ps.Events) - 1; i >= 0; i-- {
		s.Events = append(s.Events, a.ps.Events[i])
	}
	return s
}

// gatewayHost shows where the node connects without anything that could be a credential.
func gatewayHost(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return ""
	}
	return u.Host
}
