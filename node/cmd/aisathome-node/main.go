// Command aisathome-node runs an AIs@home node agent next to a local llama-server, with a
// local web UI on 127.0.0.1. `aisathome-node status` prints the running agent's status.
//
// SPDX-License-Identifier: Apache-2.0
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/aisathome/aisathome/node"
	"github.com/aisathome/aisathome/node/localui"
)

// version is set at build time: go build -ldflags "-X main.version=v1.2.3".
var version = "dev"

func main() {
	if len(os.Args) > 1 && os.Args[1] == "status" {
		os.Exit(localui.RunStatus(os.Args[2:], os.Stdout, os.Stderr))
	}
	cfgPath := flag.String("config", "node.json", "config file (JSON)")
	showVersion := flag.Bool("version", false, "print the version and exit")
	flag.Parse()
	if *showVersion {
		fmt.Println("aisathome-node", version)
		return
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	log.Info("starting", "version", version)

	b, err := os.ReadFile(*cfgPath)
	if err != nil {
		log.Error("reading config", "err", err)
		os.Exit(1)
	}
	var cfg node.Config
	if err := json.Unmarshal(b, &cfg); err != nil {
		log.Error("parsing config", "err", err)
		os.Exit(1)
	}
	// The environment wins over the config file, so an installer-provided token is never
	// silently replaced by a token left in an example config.
	if tok := os.Getenv("AISATHOME_NODE_TOKEN"); tok != "" {
		cfg.Token = tok
	}
	// Local UI and state, set by the installer's systemd drop-in (local-ui.conf).
	for env, field := range map[string]*string{"AISATHOME_STATE_FILE": &cfg.StateFile,
		"AISATHOME_RESTART_TRIGGER": &cfg.RestartTrigger, "AISATHOME_PRESET": &cfg.Preset} {
		if v := os.Getenv(env); v != "" {
			*field = v
		}
	}
	uiPort, err := localui.PortFromEnv(localui.DefaultPort)
	if err != nil {
		log.Error("local UI", "err", err)
		os.Exit(1)
	}
	pwFile := os.Getenv("AISATHOME_UI_PASSWORD_FILE")
	if pwFile == "" {
		pwFile = localui.DefaultPasswordFile
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	agent := node.New(cfg, log)
	if uiPort != 0 {
		ui := localui.New(agent, localui.Options{Port: uiPort, PasswordFile: pwFile, Log: log})
		go func() {
			// The node keeps serving without its UI (e.g. the port is taken).
			if err := ui.ListenAndServe(ctx); err != nil {
				log.Error("local UI not available", "addr", localui.Addr(uiPort), "err", err)
			}
		}()
	}
	if err := agent.Run(ctx); err != nil && ctx.Err() == nil {
		log.Error("node stopped", "err", err)
		os.Exit(1)
	}
}
