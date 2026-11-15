// SPDX-License-Identifier: Apache-2.0
package modelcheck

import (
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
)

// Pick chooses the built-in reference for the model a server serves: the same model file (by
// name, else by llama.cpp's file type), recorded on the same llama.cpp build if there is one,
// else on the newest build (Differences then says so).
func Pick(refs []*Reference, p Props) (*Reference, error) {
	var cands []*Reference
	for _, r := range refs {
		if strings.EqualFold(r.Model.File, p.ModelFile()) {
			cands = append(cands, r)
		}
	}
	if len(cands) == 0 && p.ModelFtype != "" {
		for _, r := range refs {
			if r.Model.Ftype == p.ModelFtype {
				cands = append(cands, r)
			}
		}
	}
	if len(cands) == 0 {
		var names []string
		for _, r := range refs {
			names = append(names, r.Model.File)
		}
		return nil, fmt.Errorf("no built-in reference for the model %q (%s); built in: %s. Name one with -ref",
			p.ModelFile(), p.ModelFtype, strings.Join(names, ", "))
	}
	tag, _ := p.Build()
	sort.SliceStable(cands, func(i, j int) bool { return buildNum(cands[i].Engine.Build) > buildNum(cands[j].Engine.Build) })
	for _, r := range cands {
		if tag != "" && r.Engine.Build == tag {
			return r, nil
		}
	}
	return cands[0], nil
}

func buildNum(tag string) int {
	n, _ := strconv.Atoi(strings.TrimPrefix(tag, "b"))
	return n
}

// Differences are warnings about what differs between the reference and the run. None of them
// decides the verdict, but they say how to read a narrow result.
func Differences(ref, run *Reference) []string {
	var w []string
	if run.Model.File != "" && !strings.EqualFold(ref.Model.File, run.Model.File) {
		w = append(w, fmt.Sprintf("the reference is for %s, this engine serves %s: expect a failure unless it is the same file under another name", ref.Model.File, run.Model.File))
	} else if ref.Model.Ftype != "" && run.Model.Ftype != "" && ref.Model.Ftype != run.Model.Ftype {
		w = append(w, fmt.Sprintf("the reference's file type is %q, this engine's %q", ref.Model.Ftype, run.Model.Ftype))
	}
	if run.Engine.Build != ref.Engine.Build {
		have := run.Engine.Build
		if have == "" {
			have = "an unknown build (" + run.Engine.BuildInfo + ")"
		}
		w = append(w, fmt.Sprintf("the reference was recorded on llama.cpp %s, this engine is %s: another build moves the numbers a little, so a pass counts and a narrow failure is not conclusive", ref.Engine.Build, have))
	}
	if run.GPU.Name != "" && ref.GPU.Name != "" && run.GPU.Name != ref.GPU.Name {
		w = append(w, fmt.Sprintf("another GPU type (%s; the reference's: %s): small differences are expected and within the limits", run.GPU.Name, ref.GPU.Name))
	}
	return w
}

// WriteText prints the report for a person.
func WriteText(w io.Writer, ref, run *Reference, rep Report) {
	fmt.Fprintf(w, "Model check against %s\n", ref.Name)
	fmt.Fprintf(w, "  reference: %s, recorded %s\n", ref.Label(), ref.Created)
	if ref.Note != "" {
		fmt.Fprintf(w, "             %s\n", ref.Note)
	}
	fmt.Fprintf(w, "  this run:  %s\n", run.Label())
	for _, s := range rep.Warnings {
		fmt.Fprintf(w, "  note: %s\n", s)
	}
	th := rep.Thresholds
	if rep.Verdict != Error {
		fmt.Fprintf(w, "  %d prompts, %d positions compared; %d answers left the reference's\n", rep.Prompts, rep.Positions, rep.Diverged)
		fmt.Fprintf(w, "  KL divergence per group of %d prompts: largest %.4f (limit %.4f); over all positions %.4f\n",
			th.GroupSize, rep.CheckKLMax, th.MaxCheckKL, rep.KLMean)
		fmt.Fprintf(w, "  confident flips: %d (limit %d; a different token where the reference led by more than %.1f nats)\n",
			rep.Flips, th.MaxFlips, th.Margin)
		for _, p := range rep.Results {
			if p.Flip {
				fmt.Fprintf(w, "    %s at token %d: reference %s, this run %s (margin %.2f)\n", p.ID, p.Diverge, p.RefText, p.RunText, p.Margin)
			}
		}
		worst := append([]PromptResult(nil), rep.Results...)
		sort.SliceStable(worst, func(i, j int) bool { return mean(worst[i]) > mean(worst[j]) })
		var top []string
		for _, p := range worst[:min(3, len(worst))] {
			top = append(top, fmt.Sprintf("%s %.4f", p.ID, mean(p)))
		}
		fmt.Fprintf(w, "  furthest prompts (KL per position): %s\n", strings.Join(top, ", "))
	}
	for _, s := range rep.Reasons {
		fmt.Fprintf(w, "  %s\n", s)
	}
	switch rep.Verdict {
	case Pass:
		fmt.Fprintln(w, "PASS: this engine and model compute like the reference.")
	case Fail:
		fmt.Fprintln(w, "FAIL: this engine or model does not compute like the reference (see the notes above).")
	default:
		fmt.Fprintln(w, "COULD NOT CHECK (see above).")
	}
}

func mean(p PromptResult) float64 {
	if p.Aligned == 0 {
		return 0
	}
	return p.KLSum / float64(p.Aligned)
}
