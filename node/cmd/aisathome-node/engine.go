// SPDX-License-Identifier: Apache-2.0
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
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
