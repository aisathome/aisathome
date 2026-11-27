// SPDX-License-Identifier: Apache-2.0
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/aisathome/aisathome/node/pilot"
	"github.com/aisathome/aisathome/node/setup"
)

// runPilot is `aisathome-node pilot`: the agent as a BOINC task (package pilot), started by
// BOINC's wrapper with boinc/job.xml. Its log goes to stderr, which BOINC uploads with the
// result; llama-server's output goes to a capped file in the task's folder instead.
func runPilot(args []string, stderr io.Writer) int {
	fs := flag.NewFlagSet("pilot", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var o pilot.Options
	fs.StringVar(&o.Gateway, "gateway", "", "the gateway's node URL (wss://host/v1/node/connect)")
	fs.StringVar(&o.CredentialFile, "credential-file", "", "file holding the node credential")
	fs.StringVar(&o.Model, "model", "", "the GGUF model file")
	fs.StringVar(&o.Tier, "tier", pilot.TierAuto, "engine settings tier: standard, big, or auto (the model file's tier, else the card's)")
	fs.StringVar(&o.EngineArchive, "engine-archive", "", "the node's Linux bundle (.tar.xz), unpacked once under --engine-dir")
	fs.StringVar(&o.EngineDir, "engine-dir", "", "where the bundle is unpacked (the BOINC project folder)")
	fs.StringVar(&o.LlamaServer, "llama-server", "", "run this llama-server instead of the bundle's")
	fs.DurationVar(&o.MaxServe, "max-serve", 0, "serve at most this long (default 4h)")
	fs.DurationVar(&o.IdleExit, "idle-exit", 0, "end after this long without a request (default 30m)")
	fs.StringVar(&o.Checkpoint, "checkpoint", "", "job.xml's checkpoint file")
	fs.StringVar(&o.FractionDone, "fraction-done", "", "job.xml's fraction-done file")
	fs.StringVar(&o.Summary, "summary", "", "the task's output file (JSON summary)")
	minTPS := fs.Float64("min-tps", setup.MinDecodeTPS, "speed floor: decode tokens/s after a 32k-token prompt (0: measure, never refuse)")
	engineLog := fs.String("engine-log", "engine.log", "llama-server's output (a capped file; \"\" discards it)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "pilot: unexpected arguments %q\n", fs.Args())
		return 2
	}
	if *minTPS < 0 {
		fmt.Fprintln(stderr, "pilot: --min-tps must not be negative")
		return 2
	}
	o.MinTPS = *minTPS
	if o.MinTPS == 0 {
		o.MinTPS = -1 // measure, never refuse
	}
	log := slog.New(slog.NewTextHandler(stderr, nil))
	var engineOut io.Writer
	if *engineLog != "" {
		f, err := openLog(*engineLog, maxLogBytes)
		if err != nil {
			log.Error("engine log", "err", err)
			return 1
		}
		defer f.Close()
		engineOut = f
	}
	// BOINC's wrapper asks the task to end with SIGTERM and kills it 10 s later.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return pilot.Run(ctx, o, pilot.Deps{Log: log, EngineOutput: engineOut}).Code
}
