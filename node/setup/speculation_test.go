// SPDX-License-Identifier: Apache-2.0
package setup

import (
	"slices"
	"strings"
	"testing"
)

func TestLlamaBuildNumber(t *testing.T) {
	for in, want := range map[string]int{
		"b12342": 12342,
		"version: 0.9.0-dev (build 12323, commit 93cf736a8)\nbuilt with GNU 11.4.0 for Linux x86_64": 12323,
		"ggml_cuda_init: found 1 CUDA devices:\nversion: 0.5.0-dev (build 11146, commit 7fe450e19)":  11146,
		"version: 12342 (0fb8f157)\nbuilt with GNU 11.4.0 for Linux x86_64":                          12342,
		"ggml_cuda_init: found 1 CUDA devices:\nversion: 12323 (93cf736a)\n":                         12323,
		"  b12387\n": 12387,
	} {
		if n, ok := LlamaBuildNumber(in); !ok || n != want {
			t.Errorf("%q: %d %v, want %d", in, n, ok, want)
		}
	}
	for _, in := range []string{"", "v0.5.0", "llama-server: error while loading shared libraries", "version: unknown",
		"version: 0.9.0-dev (build 1, commit 93cf736)"} { // v0.5.0's bundle: a shallow clone counts 1 commit
		if n, ok := LlamaBuildNumber(in); ok {
			t.Errorf("%q: %d, want none", in, n)
		}
	}
}

func TestBuildInfoTag(t *testing.T) {
	info := "llama_cpp_tag=b12342\nllama_cpp_commit=0fb8f157702702f33f4e018de8701b2569a1bc28\ncuda_version=12.8\n"
	if got := BuildInfoTag(info); got != "b12342" {
		t.Errorf("tag %q", got)
	}
	if got := BuildInfoTag("cuda_version=12.8\n"); got != "" {
		t.Errorf("no tag: %q", got)
	}
}

func TestNormSpec(t *testing.T) {
	for in, want := range map[string]string{"": SpecOff, "off": SpecOff, "MTP": SpecMTP, " dflash2 ": SpecDFlash2} {
		if got, err := NormSpec(in); err != nil || got != want {
			t.Errorf("%q: %q %v, want %q", in, got, err, want)
		}
	}
	if _, err := NormSpec("eagle"); err == nil || !strings.Contains(err.Error(), SpecModes) {
		t.Errorf("unknown mode: %v", err)
	}
}

// SpecFor: big tier only; DFlash2 refuses a llama.cpp build without PR #31533 (b12342), and an
// unknown build is too old for either mode.
func TestSpecFor(t *testing.T) {
	big, _ := TierByName("big")
	std, _ := TierByName("standard")
	cases := []struct {
		tier  Tier
		mode  string
		build int
		on    bool
		err   string
	}{
		{big, "", 12342, false, ""},
		{big, "off", 0, false, ""},
		{big, "mtp", 12323, true, ""},
		{big, "mtp", 12342, true, ""},
		{big, "mtp", 11146, false, "needs llama.cpp b12323 or newer; this engine is llama.cpp b11146"},
		{big, "mtp", 0, false, "an unknown llama.cpp build"},
		{big, "dflash2", 12342, true, ""},
		{big, "dflash2", 12387, true, ""},
		{big, "dflash2", 12341, false, "needs llama.cpp b12342 or newer"},
		{big, "dflash2", 12323, false, "needs llama.cpp b12342 or newer; this engine is llama.cpp b12323"},
		{std, "mtp", 12342, false, "big tier (24 GB cards) only; this node is in the standard tier"},
		{std, "dflash2", 12342, false, "big tier"},
		{big, "eagle", 12342, false, "must be off, mtp or dflash2"},
	}
	for _, c := range cases {
		d, on, err := c.tier.SpecFor(c.mode, c.build)
		if on != c.on || (err == nil) != (c.err == "") || (err != nil && !strings.Contains(err.Error(), c.err)) {
			t.Errorf("%s %q b%d: on %v err %v; want on %v err %q", c.tier.Name, c.mode, c.build, on, err, c.on, c.err)
		}
		if on && d.Mode != strings.TrimSpace(strings.ToLower(c.mode)) {
			t.Errorf("%q: drafter %+v", c.mode, d)
		}
	}
}

// The drafters are pinned files (the network's revision for MTP) and the free-memory need grows
// by what each one adds.
func TestDrafters(t *testing.T) {
	big, _ := TierByName("big")
	for mode, d := range Drafters {
		if d.Mode != mode || d.SpecType == "" || d.DraftMax < 1 || d.ExtraMiB <= 0 || d.MinBuild < 12323 {
			t.Errorf("%s: %+v", mode, d)
		}
		if len(d.Model.SHA256) != 64 || d.Model.Size <= 0 || !strings.HasPrefix(d.Model.URL, "https://huggingface.co/") ||
			!strings.HasSuffix(d.Model.File(), ".gguf") {
			t.Errorf("%s: model pin %+v", mode, d.Model)
		}
		if big.MinFree(d) != big.MinFreeMiB+d.ExtraMiB {
			t.Errorf("%s: min free %d", mode, big.MinFree(d))
		}
	}
	if !strings.HasPrefix(Drafters[SpecMTP].Model.URL, hfBase) {
		t.Errorf("MTP sidecar is not from the model's pinned revision: %s", Drafters[SpecMTP].Model.URL)
	}
	if big.MinFree(Drafter{}) != big.MinFreeMiB {
		t.Error("no drafter must not change the free-memory need")
	}
}

func TestEngineWithSpec(t *testing.T) {
	big, _ := TierByName("big")
	plain := big.Engine("llama-server", "model.gguf")
	e := plain.WithSpec(Drafters[SpecMTP], "/m/mtp.gguf")
	if err := e.Validate(); err != nil {
		t.Fatal(err)
	}
	args := e.Args()
	if !slices.Equal(args[:len(plain.Args())], plain.Args()) {
		t.Errorf("speculation must only append flags: %q", args)
	}
	if tail := args[len(plain.Args()):]; !slices.Equal(tail, []string{"--spec-type", "draft-mtp", "-md", "/m/mtp.gguf", "--spec-draft-n-max", "2"}) {
		t.Errorf("speculation flags %q", tail)
	}
	if p := e.Preset("big"); p != "big: 1 x 196608 tokens, KV q4_0, speculation mtp" {
		t.Errorf("preset %q", p)
	}
	d := plain.WithSpec(Drafters[SpecDFlash2], "/m/df.gguf").Args()
	if !slices.Equal(d[len(d)-6:], []string{"--spec-type", "draft-dflash", "-md", "/m/df.gguf", "--spec-draft-n-max", "7"}) {
		t.Errorf("dflash2 flags %q", d)
	}
	bad := e
	bad.Draft = ""
	if err := bad.Validate(); err == nil {
		t.Error("speculation without a draft file must not validate")
	}
}
