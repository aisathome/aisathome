// SPDX-License-Identifier: Apache-2.0
package setup

import (
	"fmt"
	"net"
	"runtime"
	"slices"
	"strconv"
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
		{"--cache-ram", strconv.Itoa(CacheRAMFor(SystemRAMMiB()))},
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

// The prompt cache: half the machine's RAM, at most 16384 MiB; 2048 when the RAM is unknown.
func TestCacheRAMFor(t *testing.T) {
	for _, c := range []struct{ total, want int }{
		{0, 2048}, {-1, 2048}, {4096, 2048}, {8192, 4096}, {16384, 8192}, {24576, 12288},
		{32104, 16052}, {32768, 16384}, {65536, 16384}, {131072, 16384},
	} {
		if got := CacheRAMFor(c.total); got != c.want {
			t.Errorf("CacheRAMFor(%d) = %d, want %d", c.total, got, c.want)
		}
	}
}

func TestMemTotalMiB(t *testing.T) {
	for in, want := range map[string]int{
		"MemTotal:       32874352 kB\nMemFree:         1234 kB\n": 32103,
		"MemFree: 1 kB\nMemTotal: 134217728 kB\n":                 131072,
		"MemFree: 1 kB\n": 0, "": 0, "MemTotal: x kB\n": 0,
	} {
		if got := memTotalMiB(in); got != want {
			t.Errorf("memTotalMiB(%q) = %d, want %d", in, got, want)
		}
	}
	if runtime.GOOS == "linux" && SystemRAMMiB() <= 0 {
		t.Error("SystemRAMMiB: no RAM size on Linux")
	}
}
