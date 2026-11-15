// SPDX-License-Identifier: Apache-2.0
package setup

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// Speculative decoding: llama-server drafts a few tokens with a small model and checks them in
// one step of the big one, so a node writes more tokens per second. The output distribution is
// the model's own (the draft is only accepted where the model agrees). It costs GPU memory, so it
// is a per-node choice, off by default, and only on cards with room for it.
const (
	SpecOff     = "off"
	SpecMTP     = "mtp"     // Qwen3.8's multi-token-prediction head, as a sidecar GGUF
	SpecDFlash2 = "dflash2" // z-lab's DFlash2 block drafter
)

// Drafter is one way to speculate: the draft model's pinned file and llama-server's settings.
type Drafter struct {
	Mode     string
	SpecType string // --spec-type
	DraftMax int    // --spec-draft-n-max
	// ExtraMiB is the GPU memory it adds to the tier's MinFreeMiB: the draft model, its context
	// and buffers at the big tier's context (measured on an RTX 3090, see each entry).
	ExtraMiB int
	// MinBuild is the oldest llama.cpp release (its build number, b<N>) it may run on.
	MinBuild int
	Model    Model
}

// Drafters are the supported modes. Both run on the big tier only: on the standard tier
// (16 GB, 81,920 context) neither fits (not measured; +2.5 to +2.9 GB on a 14 GB peak).
var Drafters = map[string]Drafter{
	// MTP: Unsloth's sidecar of the network's model, same repository and revision. Two drafted
	// tokens a step (about 1.5 kept). Measured 2026-11-12 on an RTX 3090 (master 2a4daf0): decode
	// x1.8 on long agent steps, +2.5 GB. b12323 is the first release the node shipped with it.
	SpecMTP: {
		Mode: SpecMTP, SpecType: "draft-mtp", DraftMax: 2, ExtraMiB: 2500, MinBuild: 12323,
		Model: Model{
			URL:    hfBase + "MTP/mtp-Qwen3.8-27B-Q4_0.gguf",
			SHA256: "50d9ce5a6da381bbcfb31061cf73df94a90e6faf8efeddee379a9cb8f1501c6e",
			Size:   1369590656,
		},
	},
	// DFlash2: z-lab's drafter (Apache-2.0), seven drafted tokens a step (its block size is 8).
	// Measured 2026-11-12 on an RTX 3090: decode x2.27 on long agent steps with llama.cpp PR
	// #31533 (few-row MMQ for batch sizes 2-24), only x1.6 without it; +2.9 GB. #31533 is commit
	// 0fb8f15, first released as b12342: an older build refuses this mode.
	SpecDFlash2: {
		Mode: SpecDFlash2, SpecType: "draft-dflash", DraftMax: 7, ExtraMiB: 2900, MinBuild: 12342,
		Model: Model{
			URL:    "https://huggingface.co/z-lab/Qwen3.8-27B-DFlash2-GGUF/resolve/2d9571f8ce46e151f61c6499c99dee6079e1d610/Qwen3.8-27B-DFlash2-Q4_K_M.gguf",
			SHA256: "1a25c56858e1ebe93f2718ac1d49d1151f9323325c1bbfd6209370f4db131ebd",
			Size:   1143006816,
		},
	},
}

// SpecModes is the values a node's speculation setting takes, for messages.
const SpecModes = "off, mtp or dflash2"

// NormSpec is the setting's canonical value ("" is off), or an error for an unknown one.
func NormSpec(mode string) (string, error) {
	m := strings.ToLower(strings.TrimSpace(mode))
	switch m {
	case "", SpecOff:
		return SpecOff, nil
	case SpecMTP, SpecDFlash2:
		return m, nil
	}
	return "", fmt.Errorf("speculation %q: must be %s", mode, SpecModes)
}

// The build number in `llama-server --version`: "version: 0.9.0-dev (build 12323, commit
// 93cf736a8)" (b11146 to b12342 at least, Checked 2026-11-12/14), or the older "version: 12342
// (0fb8f157)"; or in a release tag ("b12342").
var (
	reBuild    = regexp.MustCompile(`\(build (\d+), commit `)
	reBuildOld = regexp.MustCompile(`(?m)^version: (\d+) \(`)
	reTag      = regexp.MustCompile(`^b(\d+)$`)
)

// LlamaBuildNumber is the llama.cpp build number in a release tag or `llama-server --version`
// output, or false. llama.cpp counts the commits of the checkout it was built from, so a build
// from a shallow clone says "build 1" (v0.5.0's Linux bundle did): numbers below 1000 are not a
// release's and count as unknown.
func LlamaBuildNumber(s string) (int, bool) {
	s = strings.TrimSpace(s)
	for _, re := range []*regexp.Regexp{reBuild, reBuildOld, reTag} {
		if m := re.FindStringSubmatch(s); m != nil {
			n, err := strconv.Atoi(m[1])
			return n, err == nil && n >= 1000
		}
	}
	return 0, false
}

// BuildInfoTag is llama_cpp_tag from a Linux bundle's BUILD-INFO text (scripts/build-llama.sh).
func BuildInfoTag(info string) string {
	for _, l := range strings.Split(info, "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(l), "llama_cpp_tag="); ok {
			return v
		}
	}
	return ""
}

// SpecFor is the drafter for mode on this tier with this llama.cpp build (0: not known), or why
// the node must run without it: a tier other than big, or a build older than the mode needs (an
// unknown build counts as too old). Off returns false and no error.
func (t Tier) SpecFor(mode string, build int) (Drafter, bool, error) {
	m, err := NormSpec(mode)
	if err != nil || m == SpecOff {
		return Drafter{}, false, err
	}
	d := Drafters[m]
	if t.Name != "big" {
		return Drafter{}, false, fmt.Errorf("speculation %s runs on the big tier (24 GB cards) only; this node is in the %s tier", m, t.Name)
	}
	if build < d.MinBuild {
		have := "an unknown llama.cpp build"
		if build > 0 {
			have = fmt.Sprintf("llama.cpp b%d", build)
		}
		return Drafter{}, false, fmt.Errorf("speculation %s needs llama.cpp b%d or newer; this engine is %s", m, d.MinBuild, have)
	}
	return d, true, nil
}

// MinFree is the free GPU memory the tier needs with drafter d (the zero Drafter: none).
func (t Tier) MinFree(d Drafter) int { return t.MinFreeMiB + d.ExtraMiB }
