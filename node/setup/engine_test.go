// SPDX-License-Identifier: Apache-2.0
package setup

import (
	"fmt"
	"net"
	"slices"
	"strings"
	"testing"
)

// The command from the tier: install.sh's flags (drift_test.go compares them on Linux), here
// the parts that come from the tier and the node's model entry, on every platform.
func TestEngineFromTier(t *testing.T) {
	tr, _ := TierByName("standard")
	e := tr.Engine(`C:\Program Files\AIsAtHome\llama-server.exe`, `C:\ProgramData\AIsAtHome\models\model.gguf`)
	if err := e.Validate(); err != nil {
		t.Fatal(err)
	}
	args := e.Args()
	for _, want := range [][]string{
		{"-m", `C:\ProgramData\AIsAtHome\models\model.gguf`}, {"--alias", "qwen3.8-27b-q3"},
		{"--host", "127.0.0.1"}, {"--port", "8000"}, {"--kv-unified-per-slot", "81920"},
		{"-ctk", "q4_0"}, {"-ctv", "q4_0"}, {"-np", "1"},
		{"--reasoning-budget", "8192"}, {"--reasoning-budget-message", "(Thinking budget reached; answering now.)"},
		{"--cache-ram", "2048"},
	} {
		i := slices.Index(args, want[0])
		if i < 0 || i+1 >= len(args) || args[i+1] != want[1] {
			t.Errorf("%s: want %q in %q", want[0], want[1], args)
		}
	}
	if e.Upstream() != "http://127.0.0.1:8000" {
		t.Errorf("upstream %q", e.Upstream())
	}
	m := e.ModelInfo()
	if m.Name != "qwen3.8-27b-q3" || m.Slots != 1 || m.Ctx != 81920 || !slices.Equal(m.Roles, []string{"deep", "agent"}) {
		t.Errorf("model entry %+v", m)
	}
	if p := e.Preset("standard"); p != "standard: 1 x 81920 tokens, KV q4_0" {
		t.Errorf("preset %q", p)
	}
	if env := e.Env(); !slices.Equal(env, []string{"GGML_NO_BACKTRACE=1"}) {
		t.Errorf("env %q", env)
	}
	e.CUDADevice = "GPU-1234"
	if env := e.Env(); !slices.Contains(env, "CUDA_VISIBLE_DEVICES=GPU-1234") {
		t.Errorf("pinned env %q", env)
	}
	big, _ := TierByName("big")
	if b := big.Engine("x", "y"); b.CtxPerSlot != 196608 || b.Alias != "qwen3.8-27b" {
		t.Errorf("big tier engine %+v", b)
	}
}

func TestEngineValidate(t *testing.T) {
	tr, _ := TierByName("big")
	for name, change := range map[string]func(*EngineSettings){
		"no program": func(e *EngineSettings) { e.Exe = "" },
		"no model":   func(e *EngineSettings) { e.Model = "" },
		"alias":      func(e *EngineSettings) { e.Alias = "a b" },
		"port":       func(e *EngineSettings) { e.Port = 70000 },
		"slots":      func(e *EngineSettings) { e.Slots = 0 },
		"ctx":        func(e *EngineSettings) { e.CtxPerSlot = 0 },
		"kv":         func(e *EngineSettings) { e.KV = "q5_1" },
		"budget":     func(e *EngineSettings) { e.ReasoningBudget = -2 },
		"cache":      func(e *EngineSettings) { e.CacheRAMMiB = -1 },
	} {
		e := tr.Engine("llama-server", "model.gguf")
		change(&e)
		if err := e.Validate(); err == nil {
			t.Errorf("%s: accepted %+v", name, e)
		}
	}
	e := tr.Engine("llama-server", "model.gguf")
	e.ReasoningBudget, e.CacheRAMMiB = -1, 0 // unlimited thinking, no RAM cache: allowed
	if err := e.Validate(); err != nil {
		t.Error(err)
	}
	if !strings.Contains(strings.Join(e.Args(), " "), "--reasoning-budget -1") {
		t.Errorf("unlimited budget not passed: %q", e.Args())
	}
}

// The engine's port must be free before it starts: a program already on it would get the
// node's requests (its /health may answer too).
func TestCheckPort(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	e := Tiers[0].Engine("llama-server", "m.gguf")
	e.Port = port
	if err := e.CheckPort(); err == nil || !strings.Contains(err.Error(), fmt.Sprintf("port %d", port)) {
		t.Fatalf("taken port: %v", err)
	}
	ln.Close()
	if err := e.CheckPort(); err != nil {
		t.Fatalf("free port: %v", err)
	}
}
