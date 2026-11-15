// SPDX-License-Identifier: Apache-2.0
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/aisathome/aisathome/node"
	"github.com/aisathome/aisathome/node/engine"
	"github.com/aisathome/aisathome/node/setup"
	"github.com/aisathome/aisathome/protocol"
)

// engineSettings is the engine command for node.json's "engine" (nil when there is none): the
// tier's llama-server command, with the program beside the agent (exeDir) and the model in the
// layout's place unless the config names them. The engine also decides what the node offers:
// the agent's upstream, its model entry and the preset label come from it, so node.json must
// not set the first two.
func engineSettings(cfg *node.Config, layout node.Layout, exeDir string) (*setup.EngineSettings, error) {
	ec := cfg.Engine
	if ec == nil {
		return nil, nil
	}
	tier, ok := setup.TierByName(ec.Tier)
	if !ok {
		var names []string
		for _, t := range setup.Tiers {
			names = append(names, t.Name)
		}
		return nil, fmt.Errorf("engine.tier %q: must be %s", ec.Tier, strings.Join(names, " or "))
	}
	if cfg.Upstream != "" || len(cfg.Models) > 0 {
		return nil, errors.New(`with "engine", leave "upstream" and "models" out: the engine's tier sets them`)
	}
	// Configured paths may be relative to the agent's working directory; the engine runs in its
	// program's folder, so both become absolute here (the defaults already are).
	exe, model := ec.LlamaServer, ec.Model
	for _, p := range []*string{&exe, &model} {
		if *p != "" && !filepath.IsAbs(*p) {
			abs, err := filepath.Abs(*p)
			if err != nil {
				return nil, fmt.Errorf("engine: %w", err)
			}
			*p = abs
		}
	}
	if exe == "" {
		exe = filepath.Join(exeDir, layout.EngineProgram)
	}
	if model == "" {
		model = layout.Model
	}
	if model == "" {
		return nil, errors.New(`engine.model: no default model file on this platform; set it`)
	}
	s := tier.Engine(exe, model)
	if ec.Speculation != "" {
		build := engineBuild(exe)
		if ec.LlamaServer == "" && runtime.GOOS == "windows" {
			// The engine the installer ships beside the agent: the pinned upstream release.
			build, _ = setup.LlamaBuildNumber(setup.WindowsLlama.Tag)
		}
		d, on, err := tier.SpecFor(ec.Speculation, build)
		if err != nil {
			return nil, fmt.Errorf("engine.speculation: %w", err)
		}
		if on {
			draft := ec.DraftModel
			if draft == "" {
				draft = filepath.Join(filepath.Dir(model), d.Model.File())
			} else if !filepath.IsAbs(draft) {
				if draft, err = filepath.Abs(draft); err != nil {
					return nil, fmt.Errorf("engine: %w", err)
				}
			}
			if fi, err := os.Stat(draft); err != nil || fi.Size() != d.Model.Size {
				return nil, fmt.Errorf("engine.speculation %s: the draft model %s is missing or not the pinned file (%d bytes, sha256 %s, from %s)",
					d.Mode, draft, d.Model.Size, d.Model.SHA256, d.Model.URL)
			}
			// The drafter needs room on the card beyond the tier's (Linux: install.sh's pick_spec).
			// The node's own engine is not running yet, so the card's free memory is all there is.
			g, err := engineGPU(ec.CUDADevice)
			if err != nil {
				return nil, fmt.Errorf("engine.speculation %s: cannot read the GPU's free memory to check that the draft model fits: %w", d.Mode, err)
			}
			if need := tier.MinFree(d); g.FreeMiB < need {
				return nil, fmt.Errorf("engine.speculation %s: GPU %d (%s) has %d MiB free; the %s tier with %s needs %d MiB free (%d without it). Close programs that use the GPU, or set speculation off",
					d.Mode, g.Index, g.Name, g.FreeMiB, tier.Name, d.Mode, need, tier.MinFreeMiB)
			}
			s = s.WithSpec(d, draft)
		}
	}
	s.CUDADevice = ec.CUDADevice
	if ec.Port != 0 {
		s.Port = ec.Port
	}
	if err := s.Validate(); err != nil {
		return nil, fmt.Errorf("engine: %w", err)
	}
	cfg.Upstream = s.Upstream()
	cfg.Models = []protocol.ModelInfo{s.ModelInfo()}
	if cfg.Preset == "" {
		cfg.Preset = s.Preset(tier.Name)
	}
	return &s, nil
}

// engineBuild is the llama.cpp build number of the llama-server at exe: a Linux bundle's
// BUILD-INFO (bin/llama-server beside it, or one folder up), else its --version output (0 when
// neither tells). A variable for tests.
var engineBuild = func(exe string) int {
	for _, info := range []string{filepath.Join(filepath.Dir(exe), "BUILD-INFO"), filepath.Join(filepath.Dir(filepath.Dir(exe)), "BUILD-INFO")} {
		if b, err := os.ReadFile(info); err == nil {
			if n, ok := setup.LlamaBuildNumber(setup.BuildInfoTag(string(b))); ok {
				return n
			}
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	out, _ := exec.CommandContext(ctx, exe, "--version").CombinedOutput()
	n, _ := setup.LlamaBuildNumber(string(out))
	return n
}

// engineGPU is the card the engine will run on, as nvidia-smi reports it now: the one
// cuda_device names (its UUID or index), else the one the setup picks (the largest). A
// variable for tests.
var engineGPU = func(device string) (setup.GPU, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	gpus, err := setup.ReadGPUs(ctx, setup.ExecSMI{})
	if err != nil {
		return setup.GPU{}, err
	}
	return pickGPU(gpus, device)
}

// pickGPU is the card device names (a UUID or an index), or with no device the largest one (on
// a tie, the one with more free memory, as setup.ChooseCard).
func pickGPU(gpus []setup.GPU, device string) (setup.GPU, error) {
	if device == "" {
		c, err := setup.ChooseCard(gpus, nil)
		if len(gpus) == 0 {
			return setup.GPU{}, err
		}
		return c.GPU, nil
	}
	for _, g := range gpus {
		if g.UUID == device || strconv.Itoa(g.Index) == device {
			return g, nil
		}
	}
	return setup.GPU{}, fmt.Errorf("nvidia-smi lists no GPU %q (engine.cuda_device)", device)
}

// newEngine is the supervisor for the engine settings, with the engine's output to out.
func newEngine(s *setup.EngineSettings, out io.Writer, log *slog.Logger, onExit func(error, time.Duration)) *engine.Supervisor {
	return engine.New(engine.Command{Path: s.Exe, Args: s.Args(), Env: s.Env(), Dir: filepath.Dir(s.Exe)},
		engine.Options{Output: out, Log: log.With("part", "engine"), OnExit: onExit, PreStart: s.CheckPort})
}

// runEngine runs sup until ctx ends; the returned channel closes once the engine is gone.
func runEngine(ctx context.Context, sup *engine.Supervisor) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		sup.Run(ctx)
	}()
	return done
}
