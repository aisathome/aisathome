// Health warnings for the operations page (theme A2): computed from a project's latest runner
// status (projstatus.go) and the gateway's own data each time they are shown, so a warning
// clears as soon as its cause is gone. No push: the page (/account/ops) shows them at the top,
// and the runner's daily summary lists them (GET /v1/projects/<id>/status).
//
// SPDX-License-Identifier: Apache-2.0
package gateway

import (
	"fmt"
	"math"
	"sort"
	"time"
)

// The rules' thresholds (owner, 2026-10-24, BACKLOG theme A2).
const (
	stallAfter       = 10 * time.Minute // a running attempt with no session event for this long
	slowFactor       = 2.0              // minutes per step above this times the task type's median
	slowMinSteps     = 5                // too few steps say nothing about the pace
	budgetUsedShare  = 0.8              // this share of the time budget used ...
	budgetStepsShare = 0.5              // ... with fewer than this share of the steps
	interruptWindow  = 2 * time.Hour
	interruptMax     = 2 // this many interruptions within the window
	ciBackoffMax     = 3 * time.Hour
	contractWarn     = 48 * time.Hour
	candidateWaitMax = 24 * time.Hour
	statusStaleAfter = 10 * time.Minute // no status from the runner for this long
)

// healthWarning is one active warning. Rule is a fixed name (tests and the summary use it).
type healthWarning struct {
	Rule    string `json:"rule"`
	Attempt string `json:"attempt,omitempty"`
	Text    string `json:"text"`
}

// Rule names.
const (
	ruleStalled      = "stalled"
	ruleSlow         = "slow"
	ruleBudget       = "budget"
	ruleInterrupted  = "interruptions"
	ruleCIBackoff    = "ci-backoff"
	ruleContract     = "contract"
	ruleNoNodes      = "no-nodes"
	ruleCandidate    = "candidate-waiting"
	ruleStatusStale  = "status-stale"
	ruleStatusAbsent = "no-status"
)

func mins(d time.Duration) string {
	if d >= 2*time.Hour {
		return fmt.Sprintf("%.1f h", d.Hours())
	}
	return fmt.Sprintf("%.0f min", math.Floor(d.Minutes()))
}

// health computes the active warnings from a stored status (nil: none received) and the
// number of nodes online, at now.
func health(s *storedStatus, nodesOnline int, now time.Time) []healthWarning {
	var out []healthWarning
	add := func(rule, attempt, format string, a ...any) {
		out = append(out, healthWarning{Rule: rule, Attempt: attempt, Text: fmt.Sprintf(format, a...)})
	}
	if nodesOnline == 0 {
		add(ruleNoNodes, "", "No node is online: nothing can answer chats or science.")
	}
	if s == nil || s.Status == nil {
		add(ruleStatusAbsent, "", "No status from the runner yet.")
		return out
	}
	if age := now.Sub(s.ReceivedAt); age >= statusStaleAfter {
		add(ruleStatusStale, "", "No status from the runner for %s (last at %s UTC): the numbers below are old.",
			mins(age), s.ReceivedAt.UTC().Format("15:04"))
	}
	st := s.Status
	// The runner's numbers were true at generated_at; the clock runs on since.
	for _, a := range st.Attempts {
		if a.State != "running" {
			continue
		}
		last := a.LastActivity
		if last == nil {
			last = a.StartedAt
		}
		if last != nil {
			if idle := now.Sub(*last); idle >= stallAfter {
				add(ruleStalled, a.Attempt, "%s: no step for %s (last activity %s UTC).", a.Attempt, mins(idle), last.UTC().Format("15:04"))
			}
		}
		if b, ok := st.Baselines[a.TaskType]; ok && b.MinPerStep > 0 && a.Steps >= slowMinSteps && a.AwakeMin > 0 {
			if pace := a.AwakeMin / float64(a.Steps); pace > slowFactor*b.MinPerStep {
				add(ruleSlow, a.Attempt, "%s: %.1f min per step, more than %.0f times the %s median (%.2f min, %d attempts).",
					a.Attempt, pace, slowFactor, a.TaskType, b.MinPerStep, b.Attempts)
			}
		}
		if a.BudgetMin > 0 && a.StepCap > 0 && a.UsedMin >= budgetUsedShare*a.BudgetMin && float64(a.Steps) < budgetStepsShare*float64(a.StepCap) {
			add(ruleBudget, a.Attempt, "%s: %.0f of %.0f budget minutes used (%.0f%%) with %d of %d steps.",
				a.Attempt, a.UsedMin, a.BudgetMin, 100*a.UsedMin/a.BudgetMin, a.Steps, a.StepCap)
		}
	}
	seen := map[time.Time]bool{}
	var recent []statusInterruption
	for _, x := range st.Interruptions {
		at := x.At.Truncate(time.Minute) // one stop interrupts every running attempt at once
		if now.Sub(x.At) <= interruptWindow && !seen[at] {
			seen[at] = true
			recent = append(recent, x)
		}
	}
	if len(recent) >= interruptMax {
		sort.Slice(recent, func(i, j int) bool { return recent[i].At.Before(recent[j].At) })
		add(ruleInterrupted, "", "%d interruptions in the last 2 h (spot stops or restarts; the latest at %s UTC, by %s).",
			len(recent), recent[len(recent)-1].At.UTC().Format("15:04"), recent[len(recent)-1].By)
	}
	if ci := st.CI; ci != nil && ci.Pause && ci.Since != nil {
		if d := now.Sub(*ci.Since); d > ciBackoffMax {
			reason := "TauCeti's CI"
			if len(ci.Reasons) > 0 {
				reason = ci.Reasons[0]
			}
			add(ruleCIBackoff, "", "CI back-off for %s (since %s UTC): %s.", mins(d), ci.Since.UTC().Format("01-02 15:04"), reason)
		}
	}
	for _, n := range st.Nodes {
		if n.ContractEnds == nil {
			continue
		}
		if left := n.ContractEnds.Sub(now); left < 0 {
			add(ruleContract, "", "Node %s: its contract ended %s UTC.", n.Node, n.ContractEnds.UTC().Format("2006-01-02 15:04"))
		} else if left < contractWarn {
			add(ruleContract, "", "Node %s: its contract ends in %s (%s UTC): re-rent it.", n.Node, mins(left), n.ContractEnds.UTC().Format("2006-01-02 15:04"))
		}
	}
	for _, c := range st.Candidates {
		if d := now.Sub(c.Since); d > candidateWaitMax {
			add(ruleCandidate, c.Attempt, "%s: a PR candidate waiting for the owner for %s.", c.Attempt, mins(d))
		}
	}
	return out
}

// healthOf is the warnings for project id now.
func (g *Gateway) healthOf(id string, now time.Time) []healthWarning {
	return health(g.pstatus.get(id), g.NodeCount(), now)
}
