// SPDX-License-Identifier: Apache-2.0
package gateway

import (
	"io"
	"log/slog"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/aisathome/aisathome/protocol"
)

func usageOf(ms int64) protocol.Usage { return protocol.Usage{DurationMs: ms} }

func TestVerifyDefaults(t *testing.T) {
	v, err := VerifyConfig{}.withDefaults()
	if err != nil || v.Mode != VerifyObserve || v.SpeedRate != DefaultSpeedRate || v.SpeedBurst != DefaultSpeedBurst ||
		v.NodeReasoningBudget != DefaultNodeBudget || v.NodeBudgetMessage != DefaultNodeBudgetMessage {
		t.Fatalf("%+v %v", v, err)
	}
	for _, bad := range []VerifyConfig{{Mode: "strict"}, {SpeedRate: -1}, {SpeedBurst: -5}, {NodeReasoningBudget: -2}} {
		if _, err := bad.withDefaults(); err == nil {
			t.Errorf("%+v accepted", bad)
		}
	}
	if v, err := (VerifyConfig{NodeReasoningBudget: -1}).withDefaults(); err != nil || v.NodeReasoningBudget != -1 {
		t.Fatalf("budget -1 (none): %+v %v", v, err)
	}
	if _, err := New(Config{Verify: VerifyConfig{Mode: "nope"}}, slog.New(slog.NewTextHandler(io.Discard, nil))); err == nil {
		t.Fatal("the gateway started with a bad verify.mode")
	}
}

func TestFailedChecks(t *testing.T) {
	v, _ := VerifyConfig{SpeedRate: 350, SpeedBurst: 256}.withDefaults()
	base := answerFacts{reasoning: -1, budget: -1, maxTokens: 16384}
	with := func(f func(*answerFacts)) answerFacts { a := base; f(&a); return a }
	cases := []struct {
		name string
		f    answerFacts
		want []string
	}{
		{"burst only, instant", with(func(a *answerFacts) { a.out = 256 }), nil},
		{"one over the burst, instant", with(func(a *answerFacts) { a.out = 257 }), []string{checkSpeed}},
		{"honest 3090 MTP, 86 tok/s", with(func(a *answerFacts) { a.out = 8600; a.durMs = 100_000 }), nil},
		{"at the line: 256 + 350 x 10 s", with(func(a *answerFacts) { a.out = 3756; a.durMs = 10_000 }), nil},
		{"over the line", with(func(a *answerFacts) { a.out = 3757; a.durMs = 10_000 }), []string{checkSpeed}},
		{"padding: 16,384 in 5 s", with(func(a *answerFacts) { a.out = 16384; a.durMs = 5000 }), []string{checkSpeed}},
		{"negative duration counts as 0", with(func(a *answerFacts) { a.out = 300; a.durMs = -5 }), []string{checkSpeed}},
		{"reasoning within budget + message + allowance", with(func(a *answerFacts) {
			a.durMs = 1e6
			a.out = 1000
			a.reasoning = 1000 + 12 + 32 + 20
			a.budget = 1000
			a.budgetMsg = 12
		}), nil},
		{"reasoning one past", with(func(a *answerFacts) {
			a.durMs = 1e6
			a.out = 1100
			a.reasoning = 1000 + 12 + 32 + 20 + 1
			a.budget = 1000
			a.budgetMsg = 12
		}), []string{checkReasoning}},
		{"reasoning not counted", with(func(a *answerFacts) { a.durMs = 1e6; a.reasoning = -1; a.budget = 0 }), nil},
		{"no budget in force", with(func(a *answerFacts) { a.durMs = 1e6; a.out = 9000; a.reasoning = 9000; a.budget = -1 }), nil},
		{"budget 0: only the message", with(func(a *answerFacts) {
			a.durMs = 1e6
			a.out = 100
			a.reasoning = 100
			a.budget = 0
			a.budgetMsg = 12
		}), []string{checkReasoning}},
		{"max_tokens + allowance", with(func(a *answerFacts) { a.durMs = 1e6; a.maxTokens = 1000; a.out = 1000 + 32 + 20 }), nil},
		{"past max_tokens", with(func(a *answerFacts) { a.durMs = 1e6; a.maxTokens = 1000; a.out = 1000 + 32 + 21 }), []string{checkMaxTokens}},
		{"all three, in order", with(func(a *answerFacts) {
			a.maxTokens = 100
			a.out = 5000
			a.reasoning = 4000
			a.budget = 10
		}), []string{checkSpeed, checkReasoning, checkMaxTokens}},
	}
	for _, c := range cases {
		if got := v.failedChecks(c.f); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
		}
	}
}

func TestRequestBudget(t *testing.T) {
	for _, c := range []struct {
		body map[string]any
		want int
	}{
		{map[string]any{}, -1},
		{map[string]any{"thinking_budget_tokens": 512.0}, 512},
		{map[string]any{"reasoning_budget_tokens": 0.0}, 0},
		{map[string]any{"thinking_budget_tokens": -1.0}, -1},
		{map[string]any{"thinking_budget_tokens": 100.0, "reasoning_budget_tokens": 200.0}, 200},
		{map[string]any{"thinking_budget_tokens": "9"}, -1},
	} {
		if got := requestBudget(c.body); got != c.want {
			t.Errorf("%v: %d, want %d", c.body, got, c.want)
		}
	}
}

func checkGateway(t *testing.T, mode string) *Gateway {
	t.Helper()
	v, err := VerifyConfig{Mode: mode}.withDefaults()
	if err != nil {
		t.Fatal(err)
	}
	return &Gateway{cfg: Config{Verify: v}, log: slog.New(slog.NewTextHandler(io.Discard, nil))}
}

func fastRecord(outcome string) *usageRecord {
	return &usageRecord{Node: "n-1", Outcome: outcome, Credit: 250, Tokens: &creditTokens{Completion: 2000},
		Usage: usageOf(100), facts: answerFacts{out: 2000, reasoning: -1}}
}

func TestCheckAnswerModes(t *testing.T) {
	rec := fastRecord("ok")
	if !checkGateway(t, VerifyObserve).checkAnswer(rec, map[string]any{}, 16384) || rec.Credit != 250 || rec.Voided != 0 ||
		!reflect.DeepEqual(rec.Checks, []string{checkSpeed}) {
		t.Fatalf("observe: %+v", rec)
	}
	rec = fastRecord("cancelled")
	if !checkGateway(t, VerifyEnforce).checkAnswer(rec, map[string]any{}, 16384) || rec.Credit != 0 || rec.Voided != 250 {
		t.Fatalf("enforce: %+v", rec)
	}
	rec = fastRecord("ok")
	if checkGateway(t, VerifyOff).checkAnswer(rec, map[string]any{}, 16384) || rec.Checks != nil || rec.Credit != 250 {
		t.Fatalf("off: %+v", rec)
	}
	for _, o := range []string{"failed", outcomeBlocked} { // they earn nothing anyway
		rec = fastRecord(o)
		if checkGateway(t, VerifyEnforce).checkAnswer(rec, map[string]any{}, 16384) || rec.Checks != nil {
			t.Fatalf("%s checked: %+v", o, rec)
		}
	}
	// Without a tokenizer the reasoning rule cannot run, whatever the facts say.
	rec = fastRecord("ok")
	rec.facts = answerFacts{out: 10, reasoning: 99999}
	if checkGateway(t, VerifyEnforce).checkAnswer(rec, map[string]any{"thinking_budget_tokens": 0.0}, 16384) || rec.Credit != 250 {
		t.Fatalf("reasoning checked without a tokenizer: %+v", rec)
	}
}

func TestNodeChecksCounts(t *testing.T) {
	var c nodeChecks
	now := time.Now()
	c.add(&usageRecord{Node: "a", Operator: "op", Outcome: "ok", Time: now, Checks: []string{checkSpeed, checkMaxTokens}, Voided: 3,
		Task: "t1", Signals: &answerSignals{Finish: "length", ToolCalls: 2, BadCalls: 1, Degenerate: true}})
	c.add(&usageRecord{Node: "a", Outcome: "failed", Time: now})
	c.add(&usageRecord{Node: "b", Outcome: "ok", Time: now})
	c.discarded("b", now)
	s := c.snapshot()
	if len(s) != 2 || s[0].Node != "a" || s[0].Answers != 1 || s[0].Failed != 1 || s[0].Voided != 3 || s[0].Length != 1 ||
		s[0].BadCalls != 1 || s[0].ToolCalls != 2 || s[0].Degenerate != 1 || s[0].Tasks != 1 || s[0].Operator != "op" ||
		s[0].rules() != "max-tokens 1, speed 1" || s[1].Discarded != 1 || s[1].Failed != 0 {
		t.Fatalf("%+v", s)
	}
	s[0].ByRule["speed"] = 99 // a copy
	if c.snapshot()[0].ByRule["speed"] != 1 {
		t.Fatal("snapshot shares its maps")
	}
	var big nodeChecks
	for i := 0; i < maxCheckNodes+10; i++ {
		big.add(&usageRecord{Node: strings.Repeat("x", 1) + string(rune('a'+i%26)) + time.Duration(i).String(), Outcome: "ok",
			Time: now.Add(time.Duration(i) * time.Second)})
	}
	if n := len(big.snapshot()); n != maxCheckNodes {
		t.Fatalf("%d nodes kept, want %d", n, maxCheckNodes)
	}
}
