// Package tray is the platform-independent part of the Windows tray app (aisathome-tray):
// what the tray shows for the agent's status (FromStatus), its icons (Icon), and a client for
// the agent's local page (Client). The Windows-only part is node/cmd/aisathome-tray.
//
// SPDX-License-Identifier: Apache-2.0
package tray

import (
	"fmt"
	"strings"
	"unicode/utf16"

	"github.com/aisathome/aisathome/node"
	"github.com/aisathome/aisathome/node/engine"
	"github.com/aisathome/aisathome/node/localui"
	"github.com/aisathome/aisathome/node/setupflow"
)

// State is the tray's state, one icon each.
type State string

const (
	Down      State = "down"      // the agent does not answer (its service is not running)
	Setup     State = "setup"     // not set up yet, or the volunteer runs the setup again
	Problem   State = "problem"   // needs the volunteer: the gateway refused the node, the engine keeps ending, node.json unusable
	Stopped   State = "stopped"   // the engine is stopped on request (GPU free)
	Paused    State = "paused"    // paused on the page
	Starting  State = "starting"  // not connected yet: the engine loads the model, or the node connects
	Connected State = "connected" // connected, no request right now
	Serving   State = "serving"   // connected, answering a request
)

// States lists every state, for the icons and tests.
var States = []State{Down, Setup, Problem, Stopped, Paused, Starting, Connected, Serving}

// View is what the tray shows.
type View struct {
	State    State
	Title    string // the menu's first line
	Tooltip  string // at most maxTooltip UTF-16 units (Windows' limit, with its terminating zero)
	CanStop  bool   // Stop engine applies
	CanStart bool   // Start engine applies
}

const maxTooltip = 127

// FromStatus is the tray's view of the agent's /api/status, or of the error getting it.
func FromStatus(st *localui.StatusResponse, err error) View {
	if err != nil || st == nil {
		return view(Down, "the agent is not running", "Its service ("+node.ServiceName+") is stopped or starting.")
	}
	ep := st.EngineProcess
	if s := st.Setup; s != nil && (!s.Configured || s.Mode == "setup") {
		if s.StartError != "" {
			return view(Problem, "needs the setup again", s.StartError)
		}
		detail := "Open the node page to set it up."
		switch s.Busy {
		case setupflow.BusyDownload:
			detail = "Downloading the model."
		case setupflow.BusySpeed:
			detail = "Testing the card's speed."
		case setupflow.BusyCheck:
			detail = "Checking the card."
		}
		if s.Configured {
			return view(Setup, "setup", detail)
		}
		return view(Setup, "not set up yet", detail)
	}
	var v View
	switch {
	case st.Rejected != "":
		v = view(Problem, "the gateway refused this node", st.Rejected)
	case st.Refused != "":
		v = view(Problem, "update the node agent", st.Refused)
	case ep != nil && ep.State == engine.StateStopped:
		v = view(Stopped, "engine stopped", "The GPU is free and the node is away. Start engine brings it back.")
	case ep != nil && ep.State == engine.StateRestarting:
		v = view(Problem, "the engine keeps ending", engineEnded(ep))
	case st.Paused:
		v = view(Paused, "paused", "Resume on the node page.")
	case st.Connected && st.State == node.StateServing:
		v = view(Serving, "serving", today(st))
	case st.Connected:
		v = view(Connected, "connected", today(st))
	case st.Engine != node.EngineHealthy:
		v = view(Starting, "starting", "The engine is loading the model (up to a few minutes).")
	default:
		v = view(Starting, "connecting", "Connecting to "+st.Gateway+".")
	}
	if ep != nil {
		v.CanStop = ep.State != engine.StateStopped
		v.CanStart = ep.State == engine.StateStopped
	}
	return v
}

func view(s State, title, detail string) View {
	return View{State: s, Title: "AIs@home node: " + title, Tooltip: clip("AIs@home node: " + title + "\n" + oneLine(detail))}
}

func today(st *localui.StatusResponse) string {
	if st.Today.Requests == 1 {
		return "1 request today."
	}
	return fmt.Sprintf("%d requests today.", st.Today.Requests)
}

func engineEnded(ep *engine.Status) string {
	s := fmt.Sprintf("Restarts: %d.", ep.Restarts)
	if ep.LastExit != "" {
		s += " Last exit: " + ep.LastExit + "."
	}
	return s
}

// oneLine keeps text from the gateway or a log on one line.
func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

// clip shortens s to maxTooltip UTF-16 units, with an ellipsis when it cut.
func clip(s string) string {
	if len(utf16.Encode([]rune(s))) <= maxTooltip {
		return s
	}
	r := []rune(s)
	for len(utf16.Encode(r)) > maxTooltip-1 {
		r = r[:len(r)-1]
	}
	return string(r) + "…"
}
