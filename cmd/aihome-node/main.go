// Command aihome-node runs an ai@home node agent next to a local llama-server.
//
// SPDX-License-Identifier: Apache-2.0
package main

import (
	"context"
	"encoding/json"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"aihome/internal/node"
)

func main() {
	cfgPath := flag.String("config", "node.json", "config file (JSON)")
	flag.Parse()
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))

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
	if cfg.Token == "" {
		cfg.Token = os.Getenv("AIHOME_NODE_TOKEN") // keep secrets out of config files if preferred
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := node.New(cfg, log).Run(ctx); err != nil && ctx.Err() == nil {
		log.Error("node stopped", "err", err)
		os.Exit(1)
	}
}
