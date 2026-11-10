// Package setup is the node's setup logic in Go: the tier table, the card choice from
// nvidia-smi, the free-memory and driver checks, the model download and the speed test. It
// is pure or HTTP-only, so it is tested without a GPU on Linux and Windows. The Windows node
// uses it; the Linux installer (node/packaging/linux/install.sh) has its own copy in bash, and
// drift_test.go fails when the two tier tables disagree.
//
// SPDX-License-Identifier: Apache-2.0
package setup

import (
	"fmt"
	"strings"
)

// Model is a tier's model file: a pinned URL, its SHA-256 and its size in bytes.
type Model struct {
	URL    string
	SHA256 string
	Size   int64
}

// File is the model's file name, the last element of its URL.
func (m Model) File() string {
	u := m.URL
	if i := strings.IndexAny(u, "?#"); i >= 0 {
		u = u[:i]
	}
	return u[strings.LastIndex(u, "/")+1:]
}

// Tier is one class of card: the file, context and model name its nodes serve. A tier never
// adapts to the machine: when it does not fit, the setup stops with the reason.
type Tier struct {
	Name        string // "standard" or "big", as install.sh's --tier
	MinTotalMiB int    // the smallest card (memory.total) in this tier
	Quant       string // the file's quantisation, for messages ("UD-Q3_K_XL")
	Ctx         int    // context per request (llama-server --kv-unified-per-slot)
	KV          string // KV cache type (-ctk, -ctv)
	MinFreeMiB  int    // free memory the card must have before the engine starts
	Alias       string // the model name the tier's nodes answer under
	Model       Model
}

// Text is the tier's file and context for messages, as install.sh's tier_text:
// "UD-Q3_K_XL, 80k context".
func (t Tier) Text() string { return fmt.Sprintf("%s, %dk context", t.Quant, t.Ctx/1024) }

// hfBase is the Hugging Face repository of the network's models, pinned to a commit.
const hfBase = "https://huggingface.co/unsloth/Qwen3.8-27B-GGUF/resolve/4ca720788d1e01f1bff70c033e0d0028fd02e502/"

// Tiers is the tier table, smallest cards first. It must match install.sh's tier_* functions
// (drift_test.go); see install.sh for the measurements behind each number. One model file per
// tier: the model check cannot tell close quants apart, so a tier never mixes files.
var Tiers = []Tier{
	{
		Name: "standard", MinTotalMiB: 15500, Quant: "UD-Q3_K_XL",
		Ctx: 81920, KV: "q4_0", MinFreeMiB: 14300, Alias: "qwen3.8-27b-q3",
		Model: Model{
			URL:    hfBase + "Qwen3.8-27B-UD-Q3_K_XL.gguf",
			SHA256: "8c2a45ff85e7674ca185ec8eb6cdeab0e617ed9d8018caed0b64380eb2a67a5e",
			Size:   13146393504,
		},
	},
	{
		Name: "big", MinTotalMiB: 23000, Quant: "UD-IQ4_XS",
		Ctx: 196608, KV: "q4_0", MinFreeMiB: 18500, Alias: "qwen3.8-27b",
		Model: Model{
			URL:    hfBase + "Qwen3.8-27B-UD-IQ4_XS.gguf",
			SHA256: "40fac4050e940397dbf13087afd50f4734a11805bf9d65ef8ddd7483470e6199",
			Size:   14252845984,
		},
	},
}

// The speed test (install.sh's speed_probe and min_tps): decode speed after a long prompt,
// because a short one misranks cards. Measured at 32k: RTX 4080 35.6 tokens/s, RTX 3090 35.1,
// RTX 5070 Ti 28.9, RTX 5060 Ti 13.8; a 5070 Ti with locked clocks 16.6.
const (
	SpeedPromptTokens = 32000
	SpeedPredict      = 256
	MinDecodeTPS      = 20.0
)

// TierByName is the tier called name, or false.
func TierByName(name string) (Tier, bool) {
	for _, t := range Tiers {
		if t.Name == name {
			return t, true
		}
	}
	return Tier{}, false
}

// TierForTotal is the tier for a card with totalMiB of memory: the largest tier whose
// MinTotalMiB it reaches, or false when the card is too small for any.
func TierForTotal(totalMiB int) (Tier, bool) {
	var best Tier
	ok := false
	for _, t := range Tiers {
		if totalMiB >= t.MinTotalMiB && (!ok || t.MinTotalMiB > best.MinTotalMiB) {
			best, ok = t, true
		}
	}
	return best, ok
}

// TooSmall is the refusal for a card below every tier.
func TooSmall(g GPU) error {
	return fmt.Errorf("GPU %d (%s) has %d MiB of memory; the network needs a card with 16 GB or more (standard tier: 80k context on 16 GB cards; big tier: 192k on 24 GB and more)",
		g.Index, g.Name, g.TotalMiB)
}
