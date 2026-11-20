// Answer checks (node verification, BACKLOG 11c; review docs/reviews/2026-11-20-node-verification.md
// in the research repo): the gateway pays per token only for tokens it can show came from the model.
// Every credited answer (ok, cancelled or preempted) is checked against three rules that no honest
// node breaks, all from what the gateway itself saw:
//
//   - speed: more output tokens than speed_burst + speed_rate x the gateway's own duration of the
//     answer (from sending the request to the node to its end). A node that pads its answers without
//     running the model sends far faster than any GPU writes. The operator sets both from its own
//     nodes' measured speeds, with a margin for faster cards and speculative decoding; the defaults
//     here are deliberately generous, and the values a deployment uses belong in its private
//     configuration, not in public (an exact ceiling tells a cheater how fast it may pad).
//   - reasoning-budget: more reasoning than the budget in force plus the message forced at it. The
//     budget is the request's thinking_budget_tokens (or reasoning_budget_tokens), else the node's
//     default (node_reasoning_budget, the installer's 8,192); the node's llama-server stops thinking
//     there itself, so only a modified node goes past it.
//   - max-tokens: more output than the request's max_tokens. llama-server never exceeds it; output
//     beyond it already earned nothing (credit_http.go caps it), now the whole answer is in question.
//
// The counts are the gateway's own (its tokenizer), with a small allowance for counting the text
// again rather than the engine's tokens. Without a tokenizer only the speed rule runs, on the node's
// clamped count.
//
// mode: "observe" (the default) records a failed answer's rules in its usage record ("checks"),
// logs it and counts it per node, and changes no credit. "enforce" also voids that answer's credit:
// it earns nothing ("voided" in the record says what it would have earned), in the ledger and in a
// pilot's record alike. "off" checks nothing. A failed check is never a verdict on the operator by
// itself.
//
// SPDX-License-Identifier: Apache-2.0
package gateway

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"
	"time"
)

// Check modes.
const (
	VerifyObserve = "observe"
	VerifyEnforce = "enforce"
	VerifyOff     = "off"
)

// Rules an answer can fail (usageRecord.Checks).
const (
	checkSpeed     = "speed"
	checkReasoning = "reasoning-budget"
	checkMaxTokens = "max-tokens"
)

// Defaults: generous speed limits for a deployment that sets none (see above), and the
// installer's reasoning budget and message (node/setup/engine.go).
const (
	DefaultSpeedRate         = 500.0
	DefaultSpeedBurst        = 512
	DefaultNodeBudget        = 8192
	DefaultNodeBudgetMessage = "(Thinking budget reached; answering now.)"
)

// VerifyConfig sets the answer checks (Config.Verify). Zero values take the defaults.
type VerifyConfig struct {
	// Mode is "observe" (default: record, log and count), "enforce" (also void the answer's
	// credit) or "off".
	Mode string `json:"mode,omitempty"`
	// SpeedRate (tokens per second) and SpeedBurst (tokens): an answer fails the speed check
	// with more output tokens than SpeedBurst + SpeedRate x its duration as the gateway timed it.
	SpeedRate  float64 `json:"speed_rate,omitempty"`
	SpeedBurst int     `json:"speed_burst,omitempty"`
	// NodeReasoningBudget is the thinking budget nodes apply when a request sets none (the
	// installer's --reasoning-budget, 8192; -1 = none, the check then needs the request's), and
	// NodeBudgetMessage the text they force at it.
	NodeReasoningBudget int    `json:"node_reasoning_budget,omitempty"`
	NodeBudgetMessage   string `json:"node_budget_message,omitempty"`
}

// withDefaults fills in the defaults, and refuses what makes no sense.
func (v VerifyConfig) withDefaults() (VerifyConfig, error) {
	switch v.Mode {
	case "":
		v.Mode = VerifyObserve
	case VerifyObserve, VerifyEnforce, VerifyOff:
	default:
		return v, fmt.Errorf("verify.mode %q: observe, enforce or off", v.Mode)
	}
	if v.SpeedRate == 0 {
		v.SpeedRate = DefaultSpeedRate
	}
	if v.SpeedBurst == 0 {
		v.SpeedBurst = DefaultSpeedBurst
	}
	if v.NodeReasoningBudget == 0 {
		v.NodeReasoningBudget = DefaultNodeBudget
	}
	if v.NodeBudgetMessage == "" {
		v.NodeBudgetMessage = DefaultNodeBudgetMessage
	}
	if v.SpeedRate < 0 || math.IsNaN(v.SpeedRate) || math.IsInf(v.SpeedRate, 0) || v.SpeedBurst < 0 || v.NodeReasoningBudget < -1 {
		return v, fmt.Errorf("verify: speed_rate and speed_burst must be positive, node_reasoning_budget -1 or more")
	}
	return v, nil
}

// answerFacts is what the checks read about one answer, all as the gateway counted or timed it.
type answerFacts struct {
	out       int   // output tokens (reasoning, answer, tool calls), before the max_tokens cap
	reasoning int   // reasoning tokens; -1 = not counted (no tokenizer, or thinking off)
	durMs     int64 // from sending the request to the node to the answer's end
	maxTokens int   // the request's max_tokens
	budget    int   // the reasoning budget in force; -1 = none
	budgetMsg int   // tokens of the text forced at the budget
}

// allowance is the room given for counting text again instead of taking the engine's tokens
// (a re-tokenized text can come out a little longer), and for the end-of-turn token.
func allowance(n int) int { return 32 + n/50 }

// failedChecks returns the rules f breaks under v, in a fixed order (none: nil).
func (v VerifyConfig) failedChecks(f answerFacts) []string {
	var out []string
	secs := float64(max(f.durMs, 0)) / 1000
	if float64(f.out) > float64(v.SpeedBurst)+v.SpeedRate*secs {
		out = append(out, checkSpeed)
	}
	if f.reasoning >= 0 && f.budget >= 0 && f.reasoning > f.budget+f.budgetMsg+allowance(f.budget) {
		out = append(out, checkReasoning)
	}
	if f.maxTokens > 0 && f.out > f.maxTokens+allowance(f.maxTokens) {
		out = append(out, checkMaxTokens)
	}
	return out
}

// requestBudget is the reasoning budget a request asked for (thinking_budget_tokens or
// reasoning_budget_tokens; api.go checkReasoningBudget validated it), or -1 for the node's own.
func requestBudget(body map[string]any) int {
	for _, k := range []string{"reasoning_budget_tokens", "thinking_budget_tokens"} { // llama.cpp prefers the first
		if f, ok := body[k].(float64); ok && f >= 0 {
			return int(f)
		}
	}
	return -1
}

// credited reports whether an outcome can earn credit (relay.commit).
func credited(outcome string) bool {
	return outcome == "ok" || outcome == "cancelled" || outcome == outcomePreempted
}

// checkAnswer runs the checks on rec (relay.commit, before its credit is granted): it sets
// rec.Checks and, in enforce mode, voids the credit. It returns whether the answer failed.
func (g *Gateway) checkAnswer(rec *usageRecord, body map[string]any, maxTokens int) bool {
	v := g.cfg.Verify
	if v.Mode == VerifyOff || !credited(rec.Outcome) || rec.Tokens == nil {
		return false
	}
	f := rec.facts
	f.durMs, f.maxTokens = rec.Usage.DurationMs, maxTokens
	f.budget = requestBudget(body)
	msg, _ := body["reasoning_budget_message"].(string)
	if f.budget < 0 {
		f.budget = v.NodeReasoningBudget
	}
	if msg == "" {
		msg = v.NodeBudgetMessage
	}
	if g.tok != nil {
		f.budgetMsg = g.tok.Count(msg) + 2 // and the </think> after it
	} else {
		f.reasoning = -1
	}
	rec.Checks = v.failedChecks(f)
	if len(rec.Checks) == 0 {
		return false
	}
	if v.Mode == VerifyEnforce && rec.Credit > 0 {
		rec.Voided, rec.Credit = rec.Credit, 0
	}
	g.log.Warn("answer failed a check", "node", rec.Node, "operator", rec.Operator, "checks", strings.Join(rec.Checks, ","),
		"mode", v.Mode, "req", rec.ReqID, "tokens", f.out, "reasoning", f.reasoning, "ms", f.durMs, "max_tokens", maxTokens,
		"budget", f.budget, "voided", rec.Voided)
	return true
}

// ---------------------------------------------------------------- per node, since the start

// maxCheckNodes bounds the nodes counted (each pilot is a node id of its own): the least recently
// seen goes first.
const maxCheckNodes = 4096

// nodeChecks counts each node's answers, failed checks and signals since the gateway started,
// for the operations page. The usage log keeps them for good (`accounts node-checks`).
type nodeChecks struct {
	mu sync.Mutex
	by map[string]*NodeCheckStats
}

// NodeCheckStats is one node's counts: answers that could earn credit, those that failed a check
// (by rule), the credit voided, and the signals (signals.go).
type NodeCheckStats struct {
	Node       string
	Operator   string
	Answers    int
	Failed     int
	ByRule     map[string]int
	Voided     float64
	Length     int // answers that ended at max_tokens
	ToolCalls  int
	BadCalls   int
	Degenerate int
	Discarded  int
	Tasks      int // answers to a request that named its task
	Last       time.Time
}

func (c *nodeChecks) get(node, operator string, now time.Time) *NodeCheckStats {
	if c.by == nil {
		c.by = map[string]*NodeCheckStats{}
	}
	s := c.by[node]
	if s == nil {
		if len(c.by) >= maxCheckNodes {
			var oldest string
			for k, x := range c.by {
				if oldest == "" || x.Last.Before(c.by[oldest].Last) {
					oldest = k
				}
			}
			delete(c.by, oldest)
		}
		s = &NodeCheckStats{Node: node, ByRule: map[string]int{}}
		c.by[node] = s
	}
	if operator != "" {
		s.Operator = operator
	}
	s.Last = now
	return s
}

// add counts one metered answer.
func (c *nodeChecks) add(rec *usageRecord) {
	c.mu.Lock()
	defer c.mu.Unlock()
	s := c.get(rec.Node, rec.Operator, rec.Time)
	if credited(rec.Outcome) {
		s.Answers++
	}
	if len(rec.Checks) > 0 {
		s.Failed++
		for _, r := range rec.Checks {
			s.ByRule[r]++
		}
	}
	s.Voided += rec.Voided
	if rec.Task != "" {
		s.Tasks++
	}
	if sg := rec.Signals; sg != nil {
		if sg.Finish == "length" {
			s.Length++
		}
		s.ToolCalls += sg.ToolCalls
		s.BadCalls += sg.BadCalls
		if sg.Degenerate {
			s.Degenerate++
		}
	}
}

// discarded counts an answer of node that its requester discarded (discard.go).
func (c *nodeChecks) discarded(node string, now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.get(node, "", now).Discarded++
}

// snapshot lists the nodes, those with failed checks first, then by answers.
func (c *nodeChecks) snapshot() []NodeCheckStats {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]NodeCheckStats, 0, len(c.by))
	for _, s := range c.by {
		cp := *s
		cp.ByRule = make(map[string]int, len(s.ByRule))
		for k, v := range s.ByRule {
			cp.ByRule[k] = v
		}
		out = append(out, cp)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Failed != out[j].Failed {
			return out[i].Failed > out[j].Failed
		}
		if out[i].Answers != out[j].Answers {
			return out[i].Answers > out[j].Answers
		}
		return out[i].Node < out[j].Node
	})
	return out
}

// rules is s's failed rules as "speed 2, max-tokens 1".
func (s NodeCheckStats) rules() string {
	keys := make([]string, 0, len(s.ByRule))
	for k := range s.ByRule {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = fmt.Sprintf("%s %d", k, s.ByRule[k])
	}
	return strings.Join(parts, ", ")
}
