// SPDX-License-Identifier: Apache-2.0
package modelcheck

import (
	"encoding/hex"
	"fmt"
	"math"
	"sort"
	"strings"
)

// Thresholds decide pass or fail.
//
// The check statistic is the KL divergence KL(reference || node) per position, summed over the
// aligned positions of a group of GroupSize consecutive prompts and divided by their number. A
// node passes when no group exceeds MaxCheckKL and it has at most MaxFlips confident flips. A
// confident flip is a position where the node's greedy token differs from the reference's while
// the reference's top-1 token led its top-2 by more than Margin nats.
//
// The defaults come from measurements of honest nodes (several GPU types and llama.cpp builds,
// with and without speculative decoding) against the big tier's reference: MaxCheckKL is twice
// the 99.9th percentile of their 4-prompt checks, and one confident flip in 100 prompts did occur
// on an honest node. A different quant of the same model fails most checks.
type Thresholds struct {
	MaxCheckKL float64 `json:"max_check_kl"`
	MaxFlips   int     `json:"max_confident_flips"`
	Margin     float64 `json:"flip_margin_nats"`
	GroupSize  int     `json:"group_size"`
}

// DefaultThresholds are the published limits.
var DefaultThresholds = Thresholds{MaxCheckKL: 0.0085, MaxFlips: 2, Margin: 0.5, GroupSize: 4}

// Verdicts.
const (
	Pass  = "pass"
	Fail  = "fail"
	Error = "error" // could not check: missing answers, other request settings, too few prompts
)

// PromptResult is one prompt's comparison.
type PromptResult struct {
	ID      string  `json:"id"`
	Aligned int     `json:"aligned"`   // positions compared: while both answers agree, plus the first difference
	KLSum   float64 `json:"kl_sum"`    // KL summed over those positions
	KLMax   float64 `json:"kl_max"`    // the largest single position's KL
	Diverge int     `json:"diverge"`   // position of the first different token, -1 if none
	Margin  float64 `json:"margin"`    // the reference's top-1 minus top-2 logprob there (-1: not known; 1000: one alternative only)
	Flip    bool    `json:"flip"`      // a confident flip
	RefText string  `json:"ref_token"` // the reference's and the node's token at the difference
	RunText string  `json:"node_token"`
}

// Report is the outcome of a comparison.
type Report struct {
	Verdict    string         `json:"verdict"`
	Reasons    []string       `json:"reasons,omitempty"` // why it failed or could not check
	Warnings   []string       `json:"warnings,omitempty"`
	Thresholds Thresholds     `json:"thresholds"`
	Prompts    int            `json:"prompts"`   // compared
	Positions  int            `json:"positions"` // aligned positions, all prompts
	Diverged   int            `json:"diverged"`  // prompts whose answer left the reference's
	Flips      int            `json:"confident_flips"`
	CheckKLMax float64        `json:"check_kl_max"` // the largest group statistic
	WorstGroup []string       `json:"worst_group"`  // its prompts
	KLMean     float64        `json:"kl_mean"`      // KL over all aligned positions / their number
	Groups     int            `json:"groups"`       // groups of prompts checked
	Results    []PromptResult `json:"prompt_results"`
}

// Compare compares a node's run with a reference. Only the reference's prompts that the run
// answered count (a run of the first N prompts is fine); every one of them must have an answer.
func Compare(ref, run *Reference, th Thresholds) Report {
	rep := Report{Verdict: Pass, Thresholds: th, Warnings: Differences(ref, run)}
	if th.GroupSize < 1 {
		th.GroupSize = DefaultThresholds.GroupSize
		rep.Thresholds = th
	}
	fail := func(verdict, format string, args ...any) {
		if verdict == Error || rep.Verdict == Pass {
			rep.Verdict = verdict
		}
		rep.Reasons = append(rep.Reasons, fmt.Sprintf(format, args...))
	}
	if ref.Request != run.Request {
		fail(Error, "the run's request settings %+v differ from the reference's %+v", run.Request, ref.Request)
		return rep
	}
	got := map[string]*Result{}
	for i := range run.Results {
		got[run.Results[i].ID] = &run.Results[i]
	}
	var errs []string
	for i := range ref.Results {
		r := &ref.Results[i]
		a, ok := got[r.ID]
		if !ok {
			continue
		}
		if a.Text != r.Text {
			fail(Error, "prompt %s: the run's text differs from the reference's", r.ID)
			continue
		}
		if r.Error != "" || len(r.Tokens) == 0 {
			fail(Error, "the reference has no answer for prompt %s", r.ID)
			continue
		}
		if err := checkTokens(r.Tokens); err != nil {
			fail(Error, "the reference's prompt %s: %v", r.ID, err)
			continue
		}
		msg := a.Error
		if msg == "" && len(a.Tokens) == 0 {
			msg = "no tokens"
		} else if err := checkTokens(a.Tokens); msg == "" && err != nil {
			msg = err.Error()
		}
		if msg != "" {
			errs = append(errs, r.ID+": "+msg)
			continue
		}
		rep.Results = append(rep.Results, comparePrompt(r, a, th.Margin))
	}
	if len(errs) > 0 {
		shown := errs
		if len(shown) > 3 {
			shown = append(shown[:3:3], fmt.Sprintf("and %d more", len(errs)-3))
		}
		fail(Error, "%d prompt(s) without an answer: %s", len(errs), strings.Join(shown, "; "))
	}
	if rep.Verdict == Error {
		return rep
	}
	rep.Prompts = len(rep.Results)
	if rep.Prompts < th.GroupSize {
		fail(Error, "%d prompt(s) compared; at least %d are needed", rep.Prompts, th.GroupSize)
		return rep
	}
	var klTotal float64
	for _, p := range rep.Results {
		rep.Positions += p.Aligned
		klTotal += p.KLSum
		if p.Diverge >= 0 {
			rep.Diverged++
		}
		if p.Flip {
			rep.Flips++
		}
	}
	if rep.Positions == 0 {
		fail(Error, "no positions compared")
		return rep
	}
	rep.KLMean = klTotal / float64(rep.Positions)
	// Groups of GroupSize consecutive prompts in the reference's order; the remainder joins
	// the last group.
	n := rep.Prompts / th.GroupSize
	rep.Groups = n
	for g := 0; g < n; g++ {
		lo, hi := g*th.GroupSize, (g+1)*th.GroupSize
		if g == n-1 {
			hi = rep.Prompts
		}
		var sum float64
		var pos int
		for _, p := range rep.Results[lo:hi] {
			sum += p.KLSum
			pos += p.Aligned
		}
		stat := 0.0
		if pos > 0 {
			stat = sum / float64(pos)
		}
		if g == 0 || stat > rep.CheckKLMax {
			rep.CheckKLMax = stat
			rep.WorstGroup = rep.WorstGroup[:0]
			for _, p := range rep.Results[lo:hi] {
				rep.WorstGroup = append(rep.WorstGroup, p.ID)
			}
		}
	}
	if rep.CheckKLMax > th.MaxCheckKL {
		fail(Fail, "KL divergence %.4f in a group of %d prompts is above the limit %.4f", rep.CheckKLMax, th.GroupSize, th.MaxCheckKL)
	}
	if rep.Flips > th.MaxFlips {
		fail(Fail, "%d confident flips are more than the limit %d", rep.Flips, th.MaxFlips)
	}
	return rep
}

// comparePrompt compares one prompt's answers position by position while they agree, plus the
// first position where they differ (its distribution still follows the same prefix).
func comparePrompt(ref, run *Result, margin float64) PromptResult {
	pr := PromptResult{ID: ref.ID, Diverge: -1}
	n := min(len(ref.Tokens), len(run.Tokens))
	for t := 0; t < n; t++ {
		rt, at := topOf(ref.Tokens[t].Top), topOf(run.Tokens[t].Top)
		k := KL(rt, at)
		pr.Aligned++
		pr.KLSum += k
		pr.KLMax = max(pr.KLMax, k)
		if ref.Tokens[t].Key != run.Tokens[t].Key {
			pr.Diverge = t
			m := marginOf(rt)
			pr.Margin, pr.Flip = finite(m), m > margin
			pr.RefText, pr.RunText = keyText(ref.Tokens[t].Key), keyText(run.Tokens[t].Key)
			return pr
		}
	}
	// One answer ended where the other went on (the end-of-text token is not in the answer):
	// they part at position n. When the node stopped early, the reference's distribution there
	// says how sure it was of going on; when the node went on, the reference's margin for
	// stopping is not known, so that difference counts but is not a confident flip.
	if len(ref.Tokens) != len(run.Tokens) {
		pr.Diverge = n
		pr.Margin = -1
		pr.RefText, pr.RunText = "(end)", "(end)"
		if n < len(ref.Tokens) {
			m := marginOf(topOf(ref.Tokens[n].Top))
			pr.Margin, pr.Flip = finite(m), m > margin
			pr.RefText = keyText(ref.Tokens[n].Key)
		} else {
			pr.RunText = keyText(run.Tokens[n].Key)
		}
	}
	return pr
}

// finite is a margin JSON can carry: an infinite one (a single alternative) becomes 1000 nats.
func finite(m float64) float64 {
	if math.IsInf(m, 1) {
		return 1000
	}
	return m
}

// eps keeps logarithms finite.
const eps = 1e-12

// topOf merges alternatives with the same bytes (their probabilities add up) and sorts them by
// logprob, highest first (stable, so equal logprobs keep the server's order).
func topOf(alts []Alt) []Alt {
	idx := map[string]int{}
	var out []Alt
	var probs []float64
	for _, a := range alts {
		if math.IsNaN(a.LP) {
			continue
		}
		if i, ok := idx[a.Key]; ok {
			probs[i] += math.Exp(a.LP)
			continue
		}
		idx[a.Key] = len(out)
		out = append(out, Alt{Key: a.Key})
		probs = append(probs, math.Exp(a.LP))
	}
	for i := range out {
		out[i].LP = math.Log(math.Max(probs[i], eps))
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].LP > out[j].LP })
	return out
}

// marginOf is top-1 minus top-2 logprob (infinite with one alternative).
func marginOf(top []Alt) float64 {
	if len(top) < 2 {
		return math.Inf(1)
	}
	return top[0].LP - top[1].LP
}

// filled is a distribution over keys: top's probabilities, and for keys missing from top
// min(top's smallest probability, tail / (missing + 1)) each, taken from the tail; what is left
// of the tail is one "other" bucket.
func filled(top []Alt, keys []string) (map[string]float64, []string, float64) {
	probs := map[string]float64{}
	order := make([]string, 0, len(keys))
	sum, smallest := 0.0, math.Inf(1)
	for _, a := range top {
		p := math.Exp(a.LP)
		probs[a.Key] = p
		order = append(order, a.Key)
		sum += p
		smallest = math.Min(smallest, p)
	}
	tail := math.Max(0, 1-sum)
	var missing []string
	for _, k := range keys {
		if _, ok := probs[k]; !ok {
			missing = append(missing, k)
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		fill := math.Min(smallest, tail/float64(len(missing)+1))
		for _, k := range missing {
			probs[k] = fill
			order = append(order, k)
		}
		tail -= fill * float64(len(missing))
	}
	return probs, order, math.Max(tail, 0)
}

// KL is KL(p || q) over the union of both top lists, with the missing mass filled in (see
// filled) and the rest of each tail as one bucket. Both are sorted top lists (topOf).
func KL(p, q []Alt) float64 {
	var keys []string
	seen := map[string]bool{}
	for _, l := range [][]Alt{p, q} {
		for _, a := range l {
			if !seen[a.Key] {
				seen[a.Key] = true
				keys = append(keys, a.Key)
			}
		}
	}
	pp, order, pOther := filled(p, keys)
	qq, _, qOther := filled(q, keys)
	s := 0.0
	for _, k := range order {
		if pv := pp[k]; pv > 0 {
			s += pv * math.Log(math.Max(pv, eps)/math.Max(qq[k], eps))
		}
	}
	if pOther > 0 {
		s += pOther * math.Log(math.Max(pOther, eps)/math.Max(qOther, eps))
	}
	return math.Max(s, 0)
}

// keyText shows a token key (hex of its bytes) as text, quoted.
func keyText(k string) string {
	b, err := hex.DecodeString(k)
	if err != nil {
		return k
	}
	return fmt.Sprintf("%q", b)
}
