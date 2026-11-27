// SPDX-License-Identifier: Apache-2.0
package pilot

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aisathome/aisathome/node/engine"
	"github.com/aisathome/aisathome/node/setup"
)

var (
	card4080 = Device{Name: "CUDA0", Desc: "NVIDIA GeForce RTX 4080", TotalMiB: 16375, FreeMiB: 15800}
	card3090 = Device{Name: "CUDA0", Desc: "NVIDIA GeForce RTX 3090", TotalMiB: 24135, FreeMiB: 23800}
)

func tierNamed(t *testing.T, name string) setup.Tier {
	t.Helper()
	tr, ok := setup.TierByName(name)
	if !ok {
		t.Fatalf("no tier %s", name)
	}
	return tr
}

// --list-devices' output as llama.cpp prints it: ggml's own lines on stderr, the list on stdout.
func TestParseDevices(t *testing.T) {
	good := "Available devices:\n  CUDA0: NVIDIA GeForce RTX 4080 (16375 MiB, 15800 MiB free)\n"
	d, err := parseDevices([]byte(good))
	if err != nil || d != card4080 {
		t.Fatalf("%+v %v", d, err)
	}
	two := "ggml_cuda_init: found 2 CUDA devices\nAvailable devices:\n  CUDA0: NVIDIA GeForce RTX 3090 (24135 MiB, 23800 MiB free)\n" +
		"  CUDA1: NVIDIA GeForce RTX 4080 (16375 MiB, 15800 MiB free)\n"
	if d, err := parseDevices([]byte(two)); err != nil || d != card3090 {
		t.Errorf("two devices: %+v %v, want the first", d, err)
	}
	for name, out := range map[string]string{
		"empty":          "",
		"no devices":     "Available devices:\n",
		"no header":      "  CUDA0: NVIDIA GeForce RTX 4080 (16375 MiB, 15800 MiB free)\n",
		"junk":           "Available devices:\n  CUDA0: NVIDIA GeForce RTX 4080 (lots of memory)\n",
		"number too big": "Available devices:\n  CUDA0: X (99999999999999999999999 MiB, 1 MiB free)\n",
	} {
		if d, err := parseDevices([]byte(out)); err == nil {
			t.Errorf("%s: %+v, want an error", name, d)
		}
	}
}

func TestPickTier(t *testing.T) {
	std, big := tierNamed(t, "standard"), tierNamed(t, "big")
	for _, c := range []struct {
		name string
		flag string
		size int64
		dev  Device
		want string
		how  string
	}{
		{"Q3 file", TierAuto, std.Model.Size, card3090, "standard", "file"}, // a 24 GB host that took a 16 GB task
		{"IQ4_XS file", TierAuto, big.Model.Size, card4080, "big", "file"},  // checkCard refuses it later
		{"other file, 16 GB card", TierAuto, 4, card4080, "standard", "card"},
		{"other file, 24 GB card", TierAuto, 4, card3090, "big", "card"},
		{"flag", "standard", big.Model.Size, card3090, "standard", "flag"},
	} {
		tr, how, err := pickTier(c.flag, c.size, c.dev)
		if err != nil || tr.Name != c.want || how != c.how {
			t.Errorf("%s: %s %s %v, want %s %s", c.name, tr.Name, how, err, c.want, c.how)
		}
	}
	if _, _, err := pickTier(TierAuto, 4, Device{Desc: "small", TotalMiB: 12000, FreeMiB: 11000}); err == nil {
		t.Error("a 12 GB card with an unknown file got a tier")
	}
	if _, _, err := pickTier("huge", 4, card3090); err == nil {
		t.Error("an unknown tier name was taken")
	}
}

func TestCheckCard(t *testing.T) {
	std, big := tierNamed(t, "standard"), tierNamed(t, "big")
	if err := checkCard(std, card4080); err != nil {
		t.Errorf("4080, standard: %v", err)
	}
	if err := checkCard(big, card3090); err != nil {
		t.Errorf("3090, big: %v", err)
	}
	err := checkCard(big, card4080)
	if err == nil || !strings.Contains(err.Error(), "16375 MiB") || !strings.Contains(err.Error(), "big tier") {
		t.Errorf("4080, big: %v", err)
	}
	busy := card4080
	busy.FreeMiB = 13000 // a desktop of about 3 GB on the card
	if err := checkCard(std, busy); err == nil || !strings.Contains(err.Error(), "13000 MiB free") {
		t.Errorf("busy 4080, standard: %v", err)
	}
	edge := card4080
	edge.FreeMiB = std.MinFreeMiB
	if err := checkCard(std, edge); err != nil {
		t.Errorf("exactly the free memory needed: %v", err)
	}
}

// sparseModel makes a model file of the given size without writing it.
func sparseModel(t *testing.T, dir string, size int64) string {
	t.Helper()
	p := filepath.Join(dir, "model-sized.gguf")
	f, err := os.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(size); err != nil {
		t.Fatal(err)
	}
	f.Close()
	return p
}

// helloModel is the model entry the pilot announced to the gateway.
func helloModel(t *testing.T, g *gateway) map[string]any {
	t.Helper()
	g.mu.Lock()
	defer g.mu.Unlock()
	if len(g.frames) == 0 {
		t.Fatal("no hello")
	}
	var h struct {
		Models []map[string]any `json:"models"`
	}
	if err := json.Unmarshal(g.frames[0], &h); err != nil || len(h.Models) != 1 {
		t.Fatalf("hello %s: %v", g.frames[0], err)
	}
	return h.Models[0]
}

// A 16 GB card with the Q3 task serves the standard tier, under its own model name and context.
func TestStandardTierFromModelFile(t *testing.T) {
	r := newRig(t)
	r.dev = card4080
	r.o.Model = sparseModel(t, r.slot, tierNamed(t, "standard").Model.Size)
	r.o.IdleExit = 300 * time.Millisecond
	ch := r.run(context.Background())
	connected(t, r.gw)
	m := helloModel(t, r.gw)
	if m["name"] != "qwen3.8-27b-q3" || m["ctx"] != float64(81920) {
		t.Errorf("model entry %v, want qwen3.8-27b-q3 with 81920", m)
	}
	if res := wait(t, ch, 10*time.Second); res.Reason != ReasonIdle {
		t.Fatalf("result %+v", res)
	}
	if s := r.summary(); s.Tier != "standard" || s.SpeedTPS != 40 {
		t.Errorf("summary %+v", s)
	}
	if !strings.Contains(r.logs(), "chosen_by=file") {
		t.Errorf("log does not say how the tier was chosen:\n%s", r.logs())
	}
}

// A file that is no tier's takes the card's tier: the big one on a 24 GB card.
func TestTierFromCard(t *testing.T) {
	r := newRig(t)
	r.o.IdleExit = 300 * time.Millisecond
	ch := r.run(context.Background())
	connected(t, r.gw)
	if m := helloModel(t, r.gw); m["name"] != "qwen3.8-27b" || m["ctx"] != float64(196608) {
		t.Errorf("model entry %v", m)
	}
	wait(t, ch, 10*time.Second)
	if !strings.Contains(r.logs(), "chosen_by=card") {
		t.Errorf("log:\n%s", r.logs())
	}
}

// refused runs a pilot that must not serve: exit 1, never connected, the reason in its summary.
func refused(t *testing.T, r *rig, reason, says string) {
	t.Helper()
	res := wait(t, r.run(context.Background()), 15*time.Second)
	if res.Reason != reason || res.Code != 1 {
		t.Fatalf("result %+v, want %s, 1; log:\n%s", res, reason, r.logs())
	}
	select {
	case <-r.gw.ready:
		t.Error("the pilot connected")
	default:
	}
	if s := r.summary(); s.Reason != reason || !strings.Contains(s.Error, says) {
		t.Errorf("summary %+v, want reason %s and %q", s, reason, says)
	}
}

// Refused before the engine starts: the big tier's file on a 16 GB card, too little free memory,
// no GPU listed.
func TestCardRefusals(t *testing.T) {
	t.Run("big file on 16 GB", func(t *testing.T) {
		r := newRig(t)
		r.dev = card4080
		r.o.Model = sparseModel(t, r.slot, tierNamed(t, "big").Model.Size)
		refused(t, r, ReasonEngine, "needs a card with at least 23000 MiB")
	})
	t.Run("not enough free", func(t *testing.T) {
		r := newRig(t)
		r.dev = card4080
		r.dev.FreeMiB = 12000
		r.o.Model = sparseModel(t, r.slot, tierNamed(t, "standard").Model.Size)
		refused(t, r, ReasonEngine, "12000 MiB free")
	})
	t.Run("no GPU", func(t *testing.T) {
		r := newRig(t)
		r.d.Devices = func(context.Context, engine.Command) (Device, error) {
			return Device{}, errors.New("the engine lists no GPU")
		}
		refused(t, r, ReasonEngine, "lists no GPU")
	})
	t.Run("a task that served before exits 0", func(t *testing.T) {
		r := newRig(t)
		r.dev.FreeMiB = 1000
		os.WriteFile(r.o.Checkpoint+".served", nil, 0o644)
		if res := wait(t, r.run(context.Background()), 15*time.Second); res.Code != 0 {
			t.Errorf("result %+v, want code 0", res)
		}
	})
}

// Below the speed floor: reason slow, exit 1, the speed in the summary, never connected, and the
// engine stopped.
func TestSpeedFloor(t *testing.T) {
	r := newRig(t)
	r.o.EngineDir = filepath.Join(r.slot, "engine")
	os.MkdirAll(r.o.EngineDir, 0o755)
	r.speedHook = func(context.Context, string) (setup.Speed, error) {
		return setup.Speed{TPS: 13.8, PromptTokens: 32000}, nil
	}
	refused(t, r, ReasonSlow, "13.8 tokens/s")
	if s := r.summary(); s.SpeedTPS != 13.8 {
		t.Errorf("summary %+v", s)
	}
	if !r.eng.stopped.Load() {
		t.Error("the engine still runs")
	}
	if _, err := os.Stat(filepath.Join(r.o.EngineDir, speedCacheFile)); err == nil {
		t.Error("a failed speed test was remembered")
	}

	// A negative floor measures and never refuses.
	r2 := newRig(t)
	r2.o.MinTPS, r2.o.IdleExit = -1, 300*time.Millisecond
	r2.speedHook = r.speedHook
	if res := wait(t, r2.run(context.Background()), 10*time.Second); res.Reason != ReasonIdle {
		t.Errorf("no floor: %+v", res)
	}
}

// A pass is remembered per card, tier and engine for a week.
func TestSpeedCache(t *testing.T) {
	r := newRig(t)
	r.o.EngineDir = filepath.Join(r.slot, "engine")
	os.MkdirAll(r.o.EngineDir, 0o755)
	r.o.IdleExit = 200 * time.Millisecond
	run := func(what string, wantRuns int32) {
		t.Helper()
		r.gw = newGateway(t)
		r.o.Gateway = r.gw.url()
		if res := wait(t, r.run(context.Background()), 10*time.Second); res.Reason != ReasonIdle {
			t.Fatalf("%s: %+v", what, res)
		}
		if got := r.speedRuns.Load(); got != wantRuns {
			t.Errorf("%s: %d speed tests, want %d", what, got, wantRuns)
		}
	}
	run("first", 1)
	run("again", 1)
	r.dev.Desc = "NVIDIA GeForce RTX 4090"
	run("another card", 2)
	r.o.Tier = "standard"
	r.dev.FreeMiB = 23800
	run("another tier", 3)
	os.WriteFile(r.o.LlamaServer, []byte("#!/bin/sh\n# another build\n"), 0o600)
	run("another engine", 4)
	run("same again", 4)
	r.jump(speedCacheFor + time.Minute)
	run("a week later", 5)

	b, _ := os.ReadFile(filepath.Join(r.o.EngineDir, speedCacheFile))
	var m map[string]speedPass
	if err := json.Unmarshal(b, &m); err != nil || len(m) != 1 {
		t.Errorf("cache file %s: %v (expired entries should be gone)", b, err)
	}
}

func TestSpeedCacheBounded(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	for i := range speedCacheMax + 5 {
		if err := rememberPass(dir, strings.Repeat("k", i+1), speedPass{TPS: 30, At: now.Add(time.Duration(i) * time.Second)}); err != nil {
			t.Fatal(err)
		}
	}
	if n := len(readSpeedCache(dir)); n != speedCacheMax {
		t.Errorf("%d entries", n)
	}
	if _, ok := cachedPass(dir, "k", now); ok {
		t.Error("the oldest entry was kept")
	}
	os.WriteFile(filepath.Join(dir, speedCacheFile), []byte("junk"), 0o644)
	if _, ok := cachedPass(dir, "kk", now); ok {
		t.Error("a junk file gave a pass")
	}
	if _, ok := cachedPass("", "k", now); ok {
		t.Error("no folder gave a pass")
	}
}

// SIGTERM during the speed test stops the pilot within its stop budget, as a pilot that served
// nothing (exit 0).
func TestSigtermDuringSpeedTest(t *testing.T) {
	r := newRig(t)
	started := make(chan struct{})
	r.speedHook = func(ctx context.Context, _ string) (setup.Speed, error) {
		close(started)
		<-ctx.Done()
		return setup.Speed{}, ctx.Err()
	}
	ctx, cancel := context.WithCancel(context.Background())
	ch := r.run(ctx)
	select {
	case <-started:
	case <-time.After(10 * time.Second):
		t.Fatal("no speed test")
	}
	t0 := time.Now()
	cancel()
	res := wait(t, ch, 10*time.Second)
	if res.Reason != ReasonSigterm || res.Code != 0 {
		t.Fatalf("result %+v", res)
	}
	if took := time.Since(t0); took > 8*time.Second {
		t.Errorf("stop took %s", took)
	}
	if !r.eng.stopped.Load() {
		t.Error("the engine still runs")
	}
}

// An engine that never finishes loading ends the pilot as an engine failure, without a speed test.
func TestEngineNeverLoads(t *testing.T) {
	r := newRig(t)
	r.eng.loading.Store(true)
	r.o.EngineWait = 300 * time.Millisecond
	refused(t, r, ReasonEngine, "did not load the model")
	if r.speedRuns.Load() != 0 {
		t.Error("a speed test ran")
	}
}

// A pass counts only under the floor in force now, and never when the floor is off (measure).
func TestSpeedCacheUnderAnotherFloor(t *testing.T) {
	r := newRig(t)
	r.o.EngineDir = filepath.Join(r.slot, "engine")
	os.MkdirAll(r.o.EngineDir, 0o755)
	r.o.IdleExit = 200 * time.Millisecond
	r.speedHook = func(context.Context, string) (setup.Speed, error) {
		return setup.Speed{TPS: 15, PromptTokens: 32000}, nil
	}
	r.o.MinTPS = 10
	if res := wait(t, r.run(context.Background()), 10*time.Second); res.Reason != ReasonIdle {
		t.Fatalf("floor 10: %+v", res)
	}
	r.o.MinTPS = 20
	r.gw = newGateway(t)
	r.o.Gateway = r.gw.url()
	if res := wait(t, r.run(context.Background()), 10*time.Second); res.Reason != ReasonSlow {
		t.Errorf("floor 20 after a pass at 15 under 10: %+v", res)
	}
	r.o.MinTPS = -1
	r.gw = newGateway(t)
	r.o.Gateway = r.gw.url()
	wait(t, r.run(context.Background()), 10*time.Second)
	if n := r.speedRuns.Load(); n != 3 {
		t.Errorf("%d speed tests, want 3", n)
	}
}

// A speed test that fails while the engine restarts is tried again; three failures end the pilot.
func TestSpeedTestRetried(t *testing.T) {
	r := newRig(t)
	r.o.IdleExit = 200 * time.Millisecond
	fails := atomic.Int32{}
	fails.Store(1)
	r.speedHook = func(context.Context, string) (setup.Speed, error) {
		if fails.Add(-1) >= 0 {
			return setup.Speed{}, errors.New("connection reset")
		}
		return setup.Speed{TPS: 40, PromptTokens: 32000}, nil
	}
	if res := wait(t, r.run(context.Background()), 10*time.Second); res.Reason != ReasonIdle {
		t.Fatalf("one failure: %+v", res)
	}
	r2 := newRig(t)
	r2.speedHook = func(context.Context, string) (setup.Speed, error) {
		return setup.Speed{}, errors.New("connection reset")
	}
	refused(t, r2, ReasonEngine, "connection reset")
	if n := r2.speedRuns.Load(); n != engineFailures {
		t.Errorf("%d tries, want %d", n, engineFailures)
	}
}

// The wrapper going away during the card check is noticed: the pilot never serves as an orphan.
func TestParentGoneDuringChecks(t *testing.T) {
	r := newRig(t)
	gone := atomic.Bool{}
	r.d.Parent = func() int {
		if gone.Load() {
			return 1
		}
		return 4242
	}
	dev := r.d.Devices
	r.d.Devices = func(ctx context.Context, c engine.Command) (Device, error) {
		gone.Store(true)
		return dev(ctx, c)
	}
	res := wait(t, r.run(context.Background()), 10*time.Second)
	if res.Reason != ReasonSigterm {
		t.Fatalf("result %+v", res)
	}
	select {
	case <-r.gw.ready:
		t.Error("the pilot connected")
	default:
	}
}

func TestSpeedCacheNull(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, speedCacheFile), []byte("null\n"), 0o644)
	if err := rememberPass(dir, "k", speedPass{TPS: 30, At: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if _, ok := cachedPass(dir, "k", time.Now()); !ok {
		t.Error("no pass after a null file")
	}
}

// The real --list-devices call: the list from stdout; a failing engine's messages (which may name
// local paths) go to the engine's log, not into the error the public log shows.
func TestListDevices(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell scripts")
	}
	dir := t.TempDir()
	script := func(name, body string) engine.Command {
		p := filepath.Join(dir, name)
		os.WriteFile(p, []byte("#!/bin/sh\n"+body), 0o755)
		return engine.Command{Path: p, Env: []string{"CUDA_VISIBLE_DEVICES=1"}, Dir: dir}
	}
	var out bytes.Buffer
	ok := script("ok", `[ "$1" = --list-devices ] && [ "$CUDA_VISIBLE_DEVICES" = 1 ] || exit 3
echo "ggml_cuda_init: found 1 CUDA devices" >&2
printf 'Available devices:\n  CUDA0: NVIDIA GeForce RTX 4080 (16375 MiB, 15800 MiB free)\n'
`)
	if d, err := listDevices(context.Background(), ok, &out); err != nil || d != card4080 {
		t.Fatalf("%+v %v", d, err)
	}
	if !strings.Contains(out.String(), "found 1 CUDA devices") {
		t.Errorf("engine log %q", out.String())
	}
	out.Reset()
	bad := script("bad", `echo "/home/alice/boinc/projects/x/llama-server: error while loading shared libraries" >&2; exit 127
`)
	_, err := listDevices(context.Background(), bad, &out)
	if err == nil || strings.Contains(err.Error(), "alice") || !strings.Contains(out.String(), "alice") {
		t.Errorf("err %v, log %q", err, out.String())
	}
	if _, err := listDevices(context.Background(), engine.Command{Path: filepath.Join(dir, "none")}, nil); err == nil || strings.Contains(err.Error(), dir) {
		t.Errorf("missing program: %v", err)
	}
	none := script("none", "echo 'Available devices:'\n")
	if _, err := listDevices(context.Background(), none, nil); err == nil {
		t.Error("no device listed, no error")
	}
}
