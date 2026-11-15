// SPDX-License-Identifier: Apache-2.0
package main

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/aisathome/aisathome/node"
	"github.com/aisathome/aisathome/node/setup"
)

// With FAKE_LLAMA_SERVER set to a folder, this test binary stands in for llama-server: it
// writes its arguments and pid there, says hello on stderr and answers /health on the
// --host and --port it was given, until it is stopped.
func TestMain(m *testing.M) {
	if dir := os.Getenv("FAKE_LLAMA_SERVER"); dir != "" {
		fakeLlamaServer(dir, os.Args[1:])
		os.Exit(2)
	}
	os.Exit(m.Run())
}

func fakeLlamaServer(dir string, args []string) {
	os.WriteFile(filepath.Join(dir, "args"), []byte(strings.Join(args, "\n")), 0o600)
	os.WriteFile(filepath.Join(dir, "pid.tmp"), []byte(strconv.Itoa(os.Getpid())), 0o600)
	os.Rename(filepath.Join(dir, "pid.tmp"), filepath.Join(dir, "pid"))
	fmt.Fprintln(os.Stderr, "fake llama-server: GGML_NO_BACKTRACE="+os.Getenv("GGML_NO_BACKTRACE"))
	host, port := "", ""
	for i := 0; i+1 < len(args); i++ {
		switch args[i] {
		case "--host":
			host = args[i+1]
		case "--port":
			port = args[i+1]
		}
	}
	http.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, `{"status":"ok"}`) })
	http.ListenAndServe(net.JoinHostPort(host, port), nil)
}

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

// Without "engine" (the Linux node) nothing changes; with it, the tier's command, and the
// node's upstream, model entry and preset follow from it.
func TestEngineSettings(t *testing.T) {
	cfg := node.Config{NodeID: "n1", Upstream: "http://127.0.0.1:8000", Models: nil}
	if es, err := engineSettings(&cfg, node.LinuxLayout, "/opt"); es != nil || err != nil || cfg.Upstream != "http://127.0.0.1:8000" {
		t.Fatalf("no engine: %v %v %+v", es, err, cfg)
	}

	cfg = node.Config{Engine: &node.EngineConfig{Tier: "big", CUDADevice: "GPU-abc"}}
	es, err := engineSettings(&cfg, winLayout, "/opt/agent")
	if err != nil {
		t.Fatal(err)
	}
	if es.Exe != filepath.Join("/opt/agent", `engine\llama-server.exe`) || es.Model != `C:\ProgramData\AIsAtHome\models\model.gguf` {
		t.Errorf("default paths: %q %q", es.Exe, es.Model)
	}
	big, _ := setup.TierByName("big")
	want := big.Engine(es.Exe, es.Model)
	if !slices.Equal(es.Args(), want.Args()) || !slices.Contains(es.Env(), "CUDA_VISIBLE_DEVICES=GPU-abc") {
		t.Errorf("engine %+v", es)
	}
	if cfg.Upstream != "http://127.0.0.1:8000" || len(cfg.Models) != 1 || cfg.Models[0].Name != "qwen3.8-27b" ||
		cfg.Models[0].Ctx != 196608 || cfg.Preset != "big: 1 x 196608 tokens, KV q4_0" {
		t.Errorf("config from the engine: %+v", cfg)
	}

	// Absolute paths are used as they are. They come from TempDir: "/x/ls" is not absolute on
	// Windows (it has no drive), so there it would rightly become "D:\x\ls".
	absExe, absModel := filepath.Join(t.TempDir(), "x", "ls"), filepath.Join(t.TempDir(), "m.gguf")
	cfg = node.Config{Preset: "mine", Engine: &node.EngineConfig{Tier: "standard", LlamaServer: absExe, Model: absModel, Port: 8123}}
	if es, err = engineSettings(&cfg, node.LinuxLayout, "/opt"); err != nil || es.Exe != absExe || es.Model != absModel ||
		cfg.Upstream != "http://127.0.0.1:8123" || cfg.Preset != "mine" {
		t.Errorf("configured paths and port: %+v %v %+v", es, err, cfg)
	}

	// A rooted path without a drive (`\x\ls` on Windows) is made absolute too: on every
	// platform the engine gets absolute paths only.
	cfg = node.Config{Engine: &node.EngineConfig{Tier: "standard", LlamaServer: filepath.FromSlash("/x/ls"), Model: filepath.FromSlash("/m.gguf")}}
	if es, err = engineSettings(&cfg, node.LinuxLayout, "/opt"); err != nil || !filepath.IsAbs(es.Exe) || !filepath.IsAbs(es.Model) {
		t.Errorf("rooted paths: %+v %v", es, err)
	}

	// Relative paths in node.json are the agent's working directory's: the engine runs in its
	// program's folder, so a relative path would be looked up there instead (Codex review).
	cfg = node.Config{Engine: &node.EngineConfig{Tier: "standard", LlamaServer: filepath.Join("llama", "ls"), Model: "m.gguf"}}
	wd, _ := os.Getwd()
	if es, err = engineSettings(&cfg, node.LinuxLayout, "/opt"); err != nil || es.Exe != filepath.Join(wd, "llama", "ls") ||
		es.Model != filepath.Join(wd, "m.gguf") {
		t.Errorf("relative paths: %+v %v", es, err)
	}

	for name, c := range map[string]node.Config{
		"unknown tier":     {Engine: &node.EngineConfig{Tier: "huge", Model: "/m"}},
		"upstream too":     {Upstream: "http://x", Engine: &node.EngineConfig{Tier: "standard", Model: "/m"}},
		"no model (Linux)": {Engine: &node.EngineConfig{Tier: "standard"}},
		"bad port":         {Engine: &node.EngineConfig{Tier: "standard", Model: "/m", Port: 99999}},
	} {
		if _, err := engineSettings(&c, node.LinuxLayout, "/opt"); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// engine.speculation: the drafter's flags on the big tier when the build is new enough and the
// pinned draft file is in place (by default beside the model); otherwise the agent refuses,
// and says why, instead of starting a different engine than node.json asks for.
func TestEngineSpeculation(t *testing.T) {
	build := 12342
	old := engineBuild
	engineBuild = func(string) int { return build }
	t.Cleanup(func() { engineBuild = old })
	card := setup.GPU{Index: 0, UUID: "GPU-3090", Name: "NVIDIA GeForce RTX 3090", TotalMiB: 24576, FreeMiB: 24126}
	var cardErr error
	var asked []string
	oldGPU := engineGPU
	engineGPU = func(device string) (setup.GPU, error) { asked = append(asked, device); return card, cardErr }
	t.Cleanup(func() { engineGPU = oldGPU })

	dir := t.TempDir()
	model, exe := filepath.Join(dir, "model.gguf"), filepath.Join(dir, "llama-server")
	mtp := setup.Drafters[setup.SpecMTP]
	sized := func(path string, n int64) {
		t.Helper()
		f, err := os.Create(path)
		if err != nil {
			t.Fatal(err)
		}
		if err := f.Truncate(n); err != nil {
			t.Fatal(err)
		}
		f.Close()
	}
	sized(filepath.Join(dir, mtp.Model.File()), mtp.Model.Size)

	cfg := node.Config{Engine: &node.EngineConfig{Tier: "big", Model: model, Speculation: "mtp"}}
	es, err := engineSettings(&cfg, node.LinuxLayout, dir)
	if err != nil {
		t.Fatal(err)
	}
	args := es.Args()
	if i := slices.Index(args, "-md"); i < 0 || args[i+1] != filepath.Join(dir, mtp.Model.File()) ||
		!slices.Contains(args, "draft-mtp") || cfg.Preset != "big: 1 x 196608 tokens, KV q4_0, speculation mtp" {
		t.Errorf("mtp: %q, preset %q", args, cfg.Preset)
	}

	// "off" is the plain command.
	cfg = node.Config{Engine: &node.EngineConfig{Tier: "big", Model: model, Speculation: "off"}}
	if es, err = engineSettings(&cfg, node.LinuxLayout, dir); err != nil || slices.Contains(es.Args(), "--spec-type") {
		t.Errorf("off: %q %v", es.Args(), err)
	}

	// A configured draft file is used; a wrong size (not the pinned file) is refused.
	other := filepath.Join(dir, "other.gguf")
	sized(other, mtp.Model.Size)
	cfg = node.Config{Engine: &node.EngineConfig{Tier: "big", Model: model, Speculation: "mtp", DraftModel: other}}
	if es, err = engineSettings(&cfg, node.LinuxLayout, dir); err != nil || !slices.Contains(es.Args(), other) {
		t.Errorf("configured draft: %v", err)
	}
	sized(other, 1000)
	cfg = node.Config{Engine: &node.EngineConfig{Tier: "big", Model: model, Speculation: "mtp", DraftModel: other}}
	if _, err = engineSettings(&cfg, node.LinuxLayout, dir); err == nil || !strings.Contains(err.Error(), "not the pinned file") {
		t.Errorf("wrong draft file: %v", err)
	}

	for name, c := range map[string]struct {
		ec    node.EngineConfig
		build int
		want  string
	}{
		// An explicit llama_server: on Windows the default one is the pinned release, whatever
		// engineBuild says (Codex review 2026-11-14: these cases failed there).
		"dflash2, b12323 (no #31533)": {node.EngineConfig{Tier: "big", Model: model, LlamaServer: exe, Speculation: "dflash2"}, 12323, "needs llama.cpp b12342"},
		"dflash2, build unknown":      {node.EngineConfig{Tier: "big", Model: model, LlamaServer: exe, Speculation: "dflash2"}, 0, "unknown llama.cpp build"},
		"dflash2, no draft file":      {node.EngineConfig{Tier: "big", Model: model, LlamaServer: exe, Speculation: "dflash2"}, 12342, "missing"},
		"standard tier":               {node.EngineConfig{Tier: "standard", Model: model, LlamaServer: exe, Speculation: "mtp"}, 12342, "big tier"},
		"unknown mode":                {node.EngineConfig{Tier: "big", Model: model, LlamaServer: exe, Speculation: "eagle3"}, 12342, "off, mtp or dflash2"},
	} {
		build = c.build
		ec := c.ec
		cfg := node.Config{Engine: &ec}
		if _, err := engineSettings(&cfg, node.LinuxLayout, dir); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v, want %q", name, err, c.want)
		}
	}

	// The card must have the tier's free memory plus the drafter's (Codex review 2026-11-15:
	// MinFree was never checked, so a node short of room started an engine that failed to
	// allocate). Exactly enough runs; one MiB less, or no reading of the card, is refused.
	build = 12342
	big, _ := setup.TierByName("big")
	asked = nil
	card.FreeMiB = big.MinFree(mtp)
	cfg = node.Config{Engine: &node.EngineConfig{Tier: "big", Model: model, Speculation: "mtp", CUDADevice: "GPU-3090"}}
	if _, err = engineSettings(&cfg, node.LinuxLayout, dir); err != nil || !slices.Equal(asked, []string{"GPU-3090"}) {
		t.Errorf("exactly enough free memory: %v (asked %q)", err, asked)
	}
	card.FreeMiB = big.MinFree(mtp) - 1
	cfg = node.Config{Engine: &node.EngineConfig{Tier: "big", Model: model, Speculation: "mtp"}}
	if _, err = engineSettings(&cfg, node.LinuxLayout, dir); err == nil || !strings.Contains(err.Error(), fmt.Sprintf("has %d MiB free", card.FreeMiB)) ||
		!strings.Contains(err.Error(), "set speculation off") {
		t.Errorf("too little free memory: %v", err)
	}
	card.FreeMiB, cardErr = 24126, setup.ErrNoSMI
	cfg = node.Config{Engine: &node.EngineConfig{Tier: "big", Model: model, Speculation: "mtp"}}
	if _, err = engineSettings(&cfg, node.LinuxLayout, dir); err == nil || !strings.Contains(err.Error(), "cannot read the GPU's free memory") {
		t.Errorf("no nvidia-smi: %v", err)
	}
	// Off never asks for the card.
	asked = nil
	cfg = node.Config{Engine: &node.EngineConfig{Tier: "big", Model: model, Speculation: "off"}}
	if _, err = engineSettings(&cfg, node.LinuxLayout, dir); err != nil || len(asked) != 0 {
		t.Errorf("off: %v (asked %q)", err, asked)
	}
}

// The card the engine runs on: cuda_device's (by UUID or index), else the largest, on a tie the
// one with more free memory.
func TestPickGPU(t *testing.T) {
	gpus := []setup.GPU{
		{Index: 0, UUID: "GPU-a", TotalMiB: 16376, FreeMiB: 16000},
		{Index: 1, UUID: "GPU-b", TotalMiB: 24576, FreeMiB: 20000},
		{Index: 2, UUID: "GPU-c", TotalMiB: 24576, FreeMiB: 23000},
	}
	for device, want := range map[string]string{"": "GPU-c", "GPU-a": "GPU-a", "1": "GPU-b"} {
		if g, err := pickGPU(gpus, device); err != nil || g.UUID != want {
			t.Errorf("%q: %s %v, want %s", device, g.UUID, err, want)
		}
	}
	if _, err := pickGPU(gpus, "GPU-x"); err == nil || !strings.Contains(err.Error(), "no GPU") {
		t.Errorf("unknown device: %v", err)
	}
	if _, err := pickGPU(nil, ""); err == nil {
		t.Error("no GPU: no error")
	}
}

// The build comes from a Linux bundle's BUILD-INFO first (its llama-server says "build 1": a
// shallow clone), else from --version; a build number below 1000 is unknown.
func TestEngineBuild(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "bin", "llama-server")
	os.MkdirAll(filepath.Dir(exe), 0o755)
	if n := engineBuild(exe); n != 0 {
		t.Errorf("no program, no BUILD-INFO: %d", n)
	}
	os.WriteFile(filepath.Join(dir, "BUILD-INFO"), []byte("llama_cpp_tag=b12323\nllama_cpp_commit=93cf736a8d1d46aa922e27eb64aa712bcd5ebc30\n"), 0o644)
	if n := engineBuild(exe); n != 12323 {
		t.Errorf("BUILD-INFO one folder up: %d", n)
	}
}

// syncBuffer is a log writer the test may read while the agent writes.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}
func (s *syncBuffer) String() string { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

// The agent with an engine in node.json starts it with the tier's command, sends its output
// to the engine log, and stops it when the agent stops.
func TestRunStartsAndStopsEngine(t *testing.T) {
	dir := t.TempDir()
	fake := filepath.Join(dir, "fake")
	os.Mkdir(fake, 0o700)
	t.Setenv("FAKE_LLAMA_SERVER", fake)
	t.Setenv("AISATHOME_UI_PORT", "off")
	exe, _ := os.Executable()
	port := freePort(t)
	model := filepath.Join(dir, "model.gguf")
	cfgPath := writeConfig(t, fmt.Sprintf(`{"node_id":"n1","gateway_url":"ws://127.0.0.1:1/v1/node/connect","token":"t",
		"engine":{"tier":"standard","llama_server":%q,"model":%q,"port":%d}}`, exe, model, port))
	engineLog := filepath.Join(dir, "logs", "engine.log")

	ctx, cancel := context.WithCancel(context.Background())
	var logw syncBuffer
	code := make(chan int, 1)
	go func() { code <- run(ctx, cfgPath, node.LinuxLayout, &logw, engineLog) }()

	deadline := time.Now().Add(15 * time.Second)
	var pid int
	for pid == 0 && time.Now().Before(deadline) {
		b, _ := os.ReadFile(filepath.Join(fake, "pid"))
		pid, _ = strconv.Atoi(string(b))
		time.Sleep(10 * time.Millisecond)
	}
	if pid == 0 {
		cancel()
		t.Fatalf("the engine did not start; agent log:\n%s", logw.String())
	}
	b, _ := os.ReadFile(filepath.Join(fake, "args"))
	tier, _ := setup.TierByName("standard")
	want := tier.Engine(exe, model)
	want.Port = port
	if got := strings.Split(string(b), "\n"); !slices.Equal(got, want.Args()) {
		t.Errorf("engine arguments\n got %q\nwant %q", got, want.Args())
	}
	// The agent finds it healthy at its next health check, 5 s on (it then tries the gateway,
	// which is not there).
	for !strings.Contains(logw.String(), "engine healthy") && !strings.Contains(logw.String(), "disconnected from gateway") && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	select {
	case c := <-code:
		if c != 0 {
			t.Errorf("exit code %d; log:\n%s", c, logw.String())
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the agent did not stop")
	}
	// run returns only after the engine is gone.
	if p, err := os.FindProcess(pid); err == nil && processAlive(p) {
		t.Error("the engine outlived the agent")
	}
	if lb, _ := os.ReadFile(engineLog); !strings.Contains(string(lb), "fake llama-server: GGML_NO_BACKTRACE=1") {
		t.Errorf("engine log: %q", lb)
	}
	if !strings.Contains(logw.String(), "running the engine") {
		t.Errorf("agent log:\n%s", logw.String())
	}
}

// processAlive reports whether p still runs (Unix; on Windows FindProcess already fails for a
// process that is gone, and Signal(0) is not supported).
func processAlive(p *os.Process) bool { return p.Signal(syscall.Signal(0)) == nil }
