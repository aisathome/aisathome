// SPDX-License-Identifier: Apache-2.0
package setup

import (
	"errors"
	"fmt"
	"net"
	"regexp"
	"strconv"
	"time"

	"github.com/aisathome/aisathome/protocol"
)

// The engine's settings that are the same for every tier: install.sh's defaults (set_engine and
// the variables it reads). drift_test.go compares the whole command with install.sh's.
const (
	EngineHost = "127.0.0.1" // llama-server listens on loopback only
	EnginePort = 8000
	// EngineSlots is the requests one node serves at a time (install.sh's SLOTS default).
	EngineSlots = 1
	// ReasoningBudget is the thinking budget in tokens (-1 = unlimited). Unlimited thinking ran
	// for 8 minutes and filled the context; after the budget the model is told to answer.
	ReasoningBudget        = 8192
	ReasoningBudgetMessage = "(Thinking budget reached; answering now.)"
	// CacheRAMMaxMiB caps llama-server's host-RAM prompt cache (--cache-ram; its own default is
	// 8192, 0 = off): see CacheRAMFor.
	CacheRAMMaxMiB = 16384
	// CacheRAMUnknownMiB is the cache when the machine's RAM cannot be read.
	CacheRAMUnknownMiB = 2048
)

// CacheRAMFor is the prompt cache for a machine with totalMiB of RAM (install.sh's
// cache_ram_for): half the RAM, at most CacheRAMMaxMiB; CacheRAMUnknownMiB when totalMiB is 0
// (not known). One cached agent conversation of the hybrid model is 1-1.5 GiB at 19-44k tokens,
// and an entry larger than the cache is dropped, so 2048 held one conversation; 16384 raised
// the cache hits after a switch between two conversations from 29% to 88% and peaked at
// 9.3 GiB (measured, 2026-11-12). It is a ceiling that fills on a busy node, so it takes at
// most half the RAM: volunteers may run BOINC CPU work beside the node.
func CacheRAMFor(totalMiB int) int {
	if totalMiB <= 0 {
		return CacheRAMUnknownMiB
	}
	return min(totalMiB/2, CacheRAMMaxMiB)
}

// EngineRoles are the roles the node's model entry announces (install.sh's gen_node_config).
var EngineRoles = []string{"deep", "agent"}

// EngineSettings is one llama-server command: the tier's settings plus where the program and
// the model file are. The zero value is not usable; start from Tier.Engine.
type EngineSettings struct {
	Exe                    string // llama-server (llama-server.exe on Windows)
	Model                  string // the GGUF file
	Alias                  string // the model name it answers under (--alias)
	Host                   string
	Port                   int
	Slots                  int    // parallel requests (-np)
	CtxPerSlot             int    // context per request (--kv-unified-per-slot)
	KV                     string // KV cache type (-ctk, -ctv)
	ReasoningBudget        int
	ReasoningBudgetMessage string
	CacheRAMMiB            int
	// CUDADevice pins the engine to one card (CUDA_VISIBLE_DEVICES) when the machine has
	// several (Choice.Several): the card's UUID. Empty: no pin.
	CUDADevice string
}

// Engine is the tier's llama-server command with the program at exe and the model at model,
// with the prompt cache for this machine's RAM.
func (t Tier) Engine(exe, model string) EngineSettings {
	return EngineSettings{
		Exe: exe, Model: model, Alias: t.Alias, Host: EngineHost, Port: EnginePort,
		Slots: EngineSlots, CtxPerSlot: t.Ctx, KV: t.KV,
		ReasoningBudget: ReasoningBudget, ReasoningBudgetMessage: ReasoningBudgetMessage,
		CacheRAMMiB: CacheRAMFor(SystemRAMMiB()),
	}
}

var reAlias = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// Validate checks the settings as install.sh checks its environment overrides.
func (e EngineSettings) Validate() error {
	var errs []error
	if e.Exe == "" {
		errs = append(errs, errors.New("no llama-server program"))
	}
	if e.Model == "" {
		errs = append(errs, errors.New("no model file"))
	}
	if !reAlias.MatchString(e.Alias) {
		errs = append(errs, fmt.Errorf("model name %q: use letters, digits, '.', '_' and '-'", e.Alias))
	}
	if e.Host == "" || e.Port <= 0 || e.Port > 65535 {
		errs = append(errs, fmt.Errorf("bad engine address %s:%d", e.Host, e.Port))
	}
	if e.Slots < 1 || e.CtxPerSlot < 1 {
		errs = append(errs, fmt.Errorf("slots (%d) and context per slot (%d) must be positive", e.Slots, e.CtxPerSlot))
	}
	switch e.KV {
	case "q8_0", "q4_0", "f16":
	default:
		errs = append(errs, fmt.Errorf("KV type %q: must be q8_0, q4_0 or f16", e.KV))
	}
	if e.ReasoningBudget < -1 || e.CacheRAMMiB < 0 {
		errs = append(errs, fmt.Errorf("reasoning budget (%d) must be -1 or more, cache RAM (%d) 0 or more", e.ReasoningBudget, e.CacheRAMMiB))
	}
	return errors.Join(errs...)
}

// Args is llama-server's arguments (without the program), in install.sh's order. They are
// passed as separate arguments, never through a shell, so the budget message needs no quoting.
func (e EngineSettings) Args() []string {
	return []string{
		"-m", e.Model, "--alias", e.Alias,
		"--host", e.Host, "--port", strconv.Itoa(e.Port), "-ngl", "all", "-fa", "on",
		"-np", strconv.Itoa(e.Slots), "--kv-unified-per-slot", strconv.Itoa(e.CtxPerSlot),
		"--reasoning-budget", strconv.Itoa(e.ReasoningBudget),
		"--reasoning-budget-message", e.ReasoningBudgetMessage, "--no-reasoning-preserve",
		"-ctk", e.KV, "-ctv", e.KV, "--cache-prompt", "--cache-ram", strconv.Itoa(e.CacheRAMMiB), "--metrics",
	}
}

// Env is what the engine's environment adds to the agent's: no crash backtrace (on Linux, ggml
// forks a debugger for it, which can hang the process instead of ending it; the same setting
// as install.sh's unit) and the card pin, if any.
func (e EngineSettings) Env() []string {
	env := []string{"GGML_NO_BACKTRACE=1"}
	if e.CUDADevice != "" {
		env = append(env, "CUDA_VISIBLE_DEVICES="+e.CUDADevice)
	}
	return env
}

// Upstream is the engine's base URL for the agent ("http://127.0.0.1:8000").
func (e EngineSettings) Upstream() string { return fmt.Sprintf("http://%s:%d", e.Host, e.Port) }

// ModelInfo is the node's model entry for the gateway, as install.sh's gen_node_config writes it.
func (e EngineSettings) ModelInfo() protocol.ModelInfo {
	return protocol.ModelInfo{Name: e.Alias, Roles: append([]string(nil), EngineRoles...), Slots: e.Slots, Ctx: e.CtxPerSlot}
}

// Preset is the label the local UI shows for these settings, as install.sh's AISATHOME_PRESET:
// "standard: 1 x 81920 tokens, KV q4_0".
func (e EngineSettings) Preset(tier string) string {
	return fmt.Sprintf("%s: %d x %d tokens, KV %s", tier, e.Slots, e.CtxPerSlot, e.KV)
}

// CheckPort refuses when something already listens on the engine's port: the agent would
// otherwise send the node's requests to that program, whose /health may answer as well.
func (e EngineSettings) CheckPort() error {
	addr := net.JoinHostPort(e.Host, strconv.Itoa(e.Port))
	inUse := fmt.Errorf("port %d on %s is already in use by another program (another llama-server?); the engine needs it. Close that program, then start the engine again", e.Port, e.Host)
	if c, err := net.DialTimeout("tcp", addr, time.Second); err == nil {
		c.Close()
		return inUse
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return inUse
	}
	return ln.Close()
}
