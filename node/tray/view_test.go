// SPDX-License-Identifier: Apache-2.0
package tray

import (
	"errors"
	"strings"
	"testing"
	"unicode/utf16"

	"github.com/aisathome/aisathome/node"
	"github.com/aisathome/aisathome/node/engine"
	"github.com/aisathome/aisathome/node/localui"
	"github.com/aisathome/aisathome/node/setupflow"
)

// running is a connected node whose agent runs its engine (Windows), configured.
func running() *localui.StatusResponse {
	return &localui.StatusResponse{
		Status: node.Status{State: node.StateConnected, Connected: true, Engine: node.EngineHealthy, Gateway: "api.aisathome.org",
			Today: node.Counters{Requests: 12}, EngineProcess: &engine.Status{State: engine.StateRunning}},
		Setup: &setupflow.State{Mode: "run", Configured: true},
	}
}

func TestFromStatus(t *testing.T) {
	cases := []struct {
		name      string
		edit      func(*localui.StatusResponse)
		err       error
		want      State
		stop, sta bool
		tip       string // part of the tooltip
	}{
		{"agent not answering", nil, errors.New("connection refused"), Down, false, false, "not running"},
		{"fresh install", func(s *localui.StatusResponse) {
			s.Setup = &setupflow.State{Mode: "setup"}
			s.EngineProcess, s.Connected, s.State = nil, false, node.StateDisconnected
		}, nil, Setup, false, false, "not set up yet"},
		{"setup downloading", func(s *localui.StatusResponse) {
			s.Setup = &setupflow.State{Mode: "setup", Busy: setupflow.BusyDownload}
			s.EngineProcess = nil
		}, nil, Setup, false, false, "Downloading"},
		{"setup again on a set-up node", func(s *localui.StatusResponse) { s.Setup.Mode = "setup"; s.EngineProcess = nil }, nil, Setup, false, false, "setup"},
		{"node.json unusable", func(s *localui.StatusResponse) {
			s.Setup = &setupflow.State{Mode: "setup", Configured: true, StartError: "node.json cannot be used: unknown tier"}
		}, nil, Problem, false, false, "unknown tier"},
		{"token refused", func(s *localui.StatusResponse) {
			s.Connected, s.State, s.Rejected = false, node.StateDisconnected, "HTTP 401: unknown node token"
		}, nil, Problem, true, false, "HTTP 401: unknown node token"},
		{"protocol refused", func(s *localui.StatusResponse) {
			s.Connected, s.State, s.Refused = false, node.StateDisconnected, "node protocol version 2 not supported"
		}, nil, Problem, true, false, "update the node agent"},
		{"engine stopped", func(s *localui.StatusResponse) {
			s.Connected, s.State, s.Engine = false, node.StateDisconnected, node.EngineStopped
			s.EngineProcess.State = engine.StateStopped
		}, nil, Stopped, false, true, "GPU is free"},
		{"engine crash loop", func(s *localui.StatusResponse) {
			s.Connected, s.State, s.Engine = false, node.StateDisconnected, node.EngineUnhealthy
			s.EngineProcess = &engine.Status{State: engine.StateRestarting, Restarts: 3, LastExit: "exit status 0xc0000005"}
		}, nil, Problem, true, false, "Restarts: 3. Last exit: exit status 0xc0000005."},
		{"paused", func(s *localui.StatusResponse) { s.Paused, s.State = true, node.StatePaused }, nil, Paused, true, false, "Resume"},
		{"serving", func(s *localui.StatusResponse) { s.State = node.StateServing }, nil, Serving, true, false, "12 requests today"},
		{"connected", nil, nil, Connected, true, false, "12 requests today"},
		{"loading the model", func(s *localui.StatusResponse) {
			s.Connected, s.State, s.Engine = false, node.StateDisconnected, node.EngineUnhealthy
		}, nil, Starting, true, false, "loading the model"},
		{"connecting", func(s *localui.StatusResponse) { s.Connected, s.State = false, node.StateDisconnected }, nil, Starting, true, false, "Connecting to api.aisathome.org"},
		{"Linux-style agent (no engine process, no setup)", func(s *localui.StatusResponse) {
			s.EngineProcess, s.Setup = nil, nil
		}, nil, Connected, false, false, "connected"},
	}
	for _, c := range cases {
		st := running()
		if c.edit != nil {
			c.edit(st)
		}
		if c.err != nil {
			st = nil
		}
		v := FromStatus(st, c.err)
		if v.State != c.want || v.CanStop != c.stop || v.CanStart != c.sta {
			t.Errorf("%s: %s stop=%v start=%v, want %s stop=%v start=%v", c.name, v.State, v.CanStop, v.CanStart, c.want, c.stop, c.sta)
		}
		if !strings.Contains(v.Tooltip, c.tip) || !strings.HasPrefix(v.Tooltip, "AIs@home node: ") || !strings.HasPrefix(v.Title, "AIs@home node: ") {
			t.Errorf("%s: tooltip %q, title %q; want it to name %q", c.name, v.Tooltip, v.Title, c.tip)
		}
	}
}

// Windows cuts a tooltip at 127 UTF-16 units; a long reason from the gateway is clipped with
// an ellipsis, also when it is not ASCII, and stays on one line after the title.
func TestTooltipLimit(t *testing.T) {
	st := running()
	st.Connected, st.Rejected = false, "HTTP 403:\n"+strings.Repeat("ä😀 ", 80)
	v := FromStatus(st, nil)
	if n := len(utf16.Encode([]rune(v.Tooltip))); n > maxTooltip {
		t.Fatalf("tooltip is %d UTF-16 units", n)
	}
	if !strings.HasSuffix(v.Tooltip, "…") || strings.Count(v.Tooltip, "\n") != 1 {
		t.Fatalf("tooltip %q", v.Tooltip)
	}
	if got := clip("short"); got != "short" {
		t.Fatalf("clip changed a short text: %q", got)
	}
}
