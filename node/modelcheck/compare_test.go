// SPDX-License-Identifier: Apache-2.0
package modelcheck

import (
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"testing"
)

func alts(kv ...any) []Alt {
	var out []Alt
	for i := 0; i < len(kv); i += 2 {
		out = append(out, Alt{Key: kv[i].(string), LP: math.Log(kv[i+1].(float64))})
	}
	return out
}

// TestKL checks the divergence against values computed independently (the same definition in
// another implementation): union of both top lists, missing tokens filled from the tail, the
// rest of the tail as one bucket.
func TestKL(t *testing.T) {
	p := topOf(alts("61", 0.7, "62", 0.2, "63", 0.05))
	q := topOf(alts("61", 0.6, "64", 0.25, "62", 0.1))
	for _, c := range []struct {
		name string
		a, b []Alt
		want float64
	}{
		{"p||q", p, q, 0.22362764369421595},
		{"q||p", q, p, 0.39651246778216315},
		{"p||p", p, p, 0},
	} {
		if got := KL(c.a, c.b); math.Abs(got-c.want) > 1e-12 {
			t.Errorf("%s: KL %v, want %v", c.name, got, c.want)
		}
	}
	// Two entries with the same bytes are one token.
	m := topOf(alts("61", 0.35, "62", 0.2, "61", 0.35))
	if len(m) != 2 || m[0].Key != "61" || math.Abs(math.Exp(m[0].LP)-0.7) > 1e-12 {
		t.Errorf("merged %+v", m)
	}
}

// two builds a prompt of one position with two alternatives: x with probability px, y with 1-px;
// the chosen token is x unless chooseY.
func two(id string, px float64, chooseY bool) Result {
	k := "78"
	if chooseY {
		k = "79"
	}
	return Result{ID: id, Text: "prompt " + id, Tokens: []Token{{Key: k, Top: alts("78", px, "79", 1-px)}}}
}

func refOf(results ...Result) *Reference {
	return &Reference{Format: Format, Kind: KindReference, Name: "t", Request: DefaultRequest, Results: results,
		Engine: Engine{Build: "b1000"}, Model: Model{File: "m.gguf"}}
}

func runOf(results ...Result) *Reference {
	r := refOf(results...)
	r.Kind = KindRun
	return r
}

// klTwo is KL between two-token distributions (px vs qx), as KL computes it.
func klTwo(px, qx float64) float64 {
	return px*math.Log(px/qx) + (1-px)*math.Log((1-px)/(1-qx))
}

// TestThresholdKL: a group's KL exactly at the limit passes; just above it fails.
func TestThresholdKL(t *testing.T) {
	ref := refOf(two("a", 0.9, false), two("b", 0.9, false), two("c", 0.9, false), two("d", 0.9, false))
	run := runOf(two("a", 0.8, false), two("b", 0.9, false), two("c", 0.9, false), two("d", 0.9, false))
	stat := KL(topOf(ref.Results[0].Tokens[0].Top), topOf(run.Results[0].Tokens[0].Top)) / 4
	if want := klTwo(0.9, 0.8) / 4; math.Abs(stat-want) > 1e-12 {
		t.Fatalf("group KL %v, want %v", stat, want)
	}
	th := DefaultThresholds
	th.MaxCheckKL = stat
	rep := Compare(ref, run, th)
	if rep.Verdict != Pass || rep.CheckKLMax != stat || rep.Positions != 4 {
		t.Errorf("at the limit: %s %v %v (%d positions)", rep.Verdict, rep.CheckKLMax, rep.Reasons, rep.Positions)
	}
	th.MaxCheckKL = math.Nextafter(stat, 0)
	if rep := Compare(ref, run, th); rep.Verdict != Fail || len(rep.Reasons) != 1 || !strings.Contains(rep.Reasons[0], "KL divergence") {
		t.Errorf("above the limit: %s %v", rep.Verdict, rep.Reasons)
	}
}

// TestThresholdFlips: a flip needs a margin above Margin; MaxFlips flips pass, one more fails.
func TestThresholdFlips(t *testing.T) {
	// Margin of a 0.9/0.1 reference: ln 9 = 2.197 nats.
	conf := math.Log(9)
	mk := func(flips int) (*Reference, *Reference) {
		var r, a []Result
		for i := 0; i < 8; i++ {
			id := fmt.Sprint(i)
			r = append(r, two(id, 0.9, false))
			a = append(a, two(id, 0.9, i < flips)) // same distribution, other token chosen
		}
		return refOf(r...), runOf(a...)
	}
	th := DefaultThresholds
	th.MaxCheckKL = 1
	ref, run := mk(2)
	rep := Compare(ref, run, th)
	if rep.Verdict != Pass || rep.Flips != 2 || rep.Diverged != 2 {
		t.Errorf("2 flips, limit 2: %s, %d flips, %d diverged, %v", rep.Verdict, rep.Flips, rep.Diverged, rep.Reasons)
	}
	if p := rep.Results[0]; !p.Flip || p.Diverge != 0 || math.Abs(p.Margin-conf) > 1e-9 || p.RefText != `"x"` || p.RunText != `"y"` {
		t.Errorf("flip %+v", p)
	}
	ref, run = mk(3)
	if rep := Compare(ref, run, th); rep.Verdict != Fail || rep.Flips != 3 || !strings.Contains(strings.Join(rep.Reasons, " "), "confident flips") {
		t.Errorf("3 flips, limit 2: %s %d %v", rep.Verdict, rep.Flips, rep.Reasons)
	}
	// A difference at exactly the margin is a near-tie, not a confident flip.
	th.Margin = conf
	th.MaxFlips = 0
	ref, run = mk(1)
	rep = Compare(ref, run, th)
	if rep.Verdict != Pass || rep.Flips != 0 || rep.Diverged != 1 {
		t.Errorf("margin at the limit: %s %d flips %d diverged", rep.Verdict, rep.Flips, rep.Diverged)
	}
	th.Margin = math.Nextafter(conf, 0)
	if rep := Compare(ref, run, th); rep.Verdict != Fail || rep.Flips != 1 {
		t.Errorf("margin above the limit: %s %d flips", rep.Verdict, rep.Flips)
	}
}

// TestGroups: groups of 4 in the reference's order, the remainder joining the last group; the
// worst group decides, so one far prompt is not averaged away over the whole set.
func TestGroups(t *testing.T) {
	var r, a []Result
	for i := 0; i < 10; i++ {
		id := fmt.Sprint(i)
		r = append(r, two(id, 0.9, false))
		px := 0.9
		if i == 5 {
			px = 0.6
		}
		a = append(a, two(id, px, false))
	}
	rep := Compare(refOf(r...), runOf(a...), DefaultThresholds)
	if rep.Groups != 2 {
		t.Errorf("%d groups, want 2 (4 + 6)", rep.Groups)
	}
	if want := klTwo(0.9, 0.6) / 6; math.Abs(rep.CheckKLMax-want) > 1e-12 {
		t.Errorf("worst group %v, want %v (the far prompt in a group of 6)", rep.CheckKLMax, want)
	}
	if strings.Join(rep.WorstGroup, ",") != "4,5,6,7,8,9" {
		t.Errorf("worst group %v", rep.WorstGroup)
	}
	if want := klTwo(0.9, 0.6) / 10; math.Abs(rep.KLMean-want) > 1e-12 {
		t.Errorf("mean %v, want %v", rep.KLMean, want)
	}
	if rep.Verdict != Fail { // 0.24/6 = 0.04, far above 0.0085
		t.Errorf("verdict %s", rep.Verdict)
	}
}

// TestCannotCheck: missing answers, other settings, other texts and too few prompts are not a
// verdict on the engine; a run of the first prompts only is fine.
func TestCannotCheck(t *testing.T) {
	ref := refOf(two("a", 0.9, false), two("b", 0.9, false), two("c", 0.9, false), two("d", 0.9, false), two("e", 0.9, false))
	ok := func() *Reference {
		return runOf(two("a", 0.9, false), two("b", 0.9, false), two("c", 0.9, false), two("d", 0.9, false), two("e", 0.9, false))
	}
	if rep := Compare(ref, ok(), DefaultThresholds); rep.Verdict != Pass || rep.Prompts != 5 || rep.CheckKLMax != 0 {
		t.Errorf("identical: %s %d %v", rep.Verdict, rep.Prompts, rep.Reasons)
	}
	first4 := ok()
	first4.Results = first4.Results[:4]
	if rep := Compare(ref, first4, DefaultThresholds); rep.Verdict != Pass || rep.Prompts != 4 {
		t.Errorf("first 4: %s %d %v", rep.Verdict, rep.Prompts, rep.Reasons)
	}
	for name, mutate := range map[string]func(r *Reference){
		"an error":      func(r *Reference) { r.Results[1].Tokens, r.Results[1].Error = nil, "HTTP 500" },
		"no tokens":     func(r *Reference) { r.Results[2].Tokens = nil },
		"other request": func(r *Reference) { r.Request.MaxTokens = 16 },
		"other text":    func(r *Reference) { r.Results[0].Text = "something else" },
		"too few":       func(r *Reference) { r.Results = r.Results[:3] },
		"error and far": func(r *Reference) {
			r.Results[0] = two("a", 0.5, true)
			r.Results[4].Error = "timeout"
		},
	} {
		run := ok()
		mutate(run)
		if rep := Compare(ref, run, DefaultThresholds); rep.Verdict != Error || len(rep.Reasons) == 0 {
			t.Errorf("%s: %s %v", name, rep.Verdict, rep.Reasons)
		}
	}
}

func TestDifferences(t *testing.T) {
	ref := refOf(two("a", 0.9, false))
	ref.GPU.Name = "NVIDIA GeForce RTX 4090"
	run := runOf(two("a", 0.9, false))
	if w := Differences(ref, run); len(w) != 0 {
		t.Errorf("same: %v", w)
	}
	run.Engine.Build, run.GPU.Name, run.Model.File = "b12342", "NVIDIA GeForce RTX 3090", "other.gguf"
	w := strings.Join(Differences(ref, run), "\n")
	for _, s := range []string{"b12342", "RTX 3090", "other.gguf"} {
		if !strings.Contains(w, s) {
			t.Errorf("no %q in %s", s, w)
		}
	}
}

// TestTruncated: an answer that ends where the reference's goes on parts from it there. With a
// sure reference that is a confident flip, so an engine that stops early cannot pass on the
// positions it did write; a node that goes on where the reference ended differs, but not
// confidently (the reference's margin for ending is not known).
func TestTruncated(t *testing.T) {
	long := func(id string) Result {
		r := two(id, 0.9, false)
		r.Tokens = append(r.Tokens, r.Tokens[0], r.Tokens[0])
		return r
	}
	var ref, cut, more []Result
	for i := 0; i < 4; i++ {
		id := fmt.Sprint(i)
		ref = append(ref, long(id))
		cut = append(cut, two(id, 0.9, false)) // the first of three tokens only
		more = append(more, long(id))
	}
	rep := Compare(refOf(ref...), runOf(cut...), DefaultThresholds)
	if rep.Verdict != Fail || rep.Flips != 4 || rep.Diverged != 4 || rep.Results[0].Diverge != 1 || rep.Results[0].RunText != "(end)" {
		t.Errorf("early end: %s, %d flips, %+v", rep.Verdict, rep.Flips, rep.Results[0])
	}
	more[0].Tokens = append(more[0].Tokens, more[0].Tokens[0])
	rep = Compare(refOf(ref...), runOf(more...), DefaultThresholds)
	if rep.Verdict != Pass || rep.Flips != 0 || rep.Diverged != 1 || rep.Results[0].Diverge != 3 || rep.Results[0].Margin != -1 {
		t.Errorf("went on: %s, %d flips, %+v", rep.Verdict, rep.Flips, rep.Results[0])
	}
	if _, err := json.Marshal(rep); err != nil {
		t.Errorf("report as JSON: %v", err)
	}
}

// TestBadNumbers: a reference without answers, and logprobs that are not log-probabilities,
// make the check impossible instead of passing it.
func TestBadNumbers(t *testing.T) {
	four := func() []Result {
		return []Result{two("a", 0.9, false), two("b", 0.9, false), two("c", 0.9, false), two("d", 0.9, false)}
	}
	ref := refOf(four()...)
	ref.Results[0].Tokens, ref.Results[0].Error = nil, "HTTP 500"
	if rep := Compare(ref, runOf(four()...), DefaultThresholds); rep.Verdict != Error || !strings.Contains(strings.Join(rep.Reasons, " "), "reference has no answer") {
		t.Errorf("reference without an answer: %s %v", rep.Verdict, rep.Reasons)
	}
	for name, lp := range map[string]float64{"zero probability sum": 0, "positive": 0.5, "NaN": math.NaN(), "-Inf": math.Inf(-1)} {
		run := runOf(four()...)
		run.Results[1].Tokens[0].Top[1].LP = lp // with "zero": 0.9 + 1.0 > 1
		if rep := Compare(refOf(four()...), run, DefaultThresholds); rep.Verdict != Error {
			t.Errorf("run logprob %s: %s %v", name, rep.Verdict, rep.Reasons)
		}
		b, _ := run.Encode(false)
		if name != "NaN" && name != "-Inf" { // JSON has neither
			if _, err := Parse(b); err == nil {
				t.Errorf("logprob %s parsed", name)
			}
		}
	}
}
