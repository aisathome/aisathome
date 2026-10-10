// Command aisathome-node runs an AIs@home node agent next to a local llama-server.
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
)

// version is set at build time: go build -ldflags "-X main.version=v1.2.3".
var version = "dev"

func main() {
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
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := node.New(cfg, log).Run(ctx); err != nil && ctx.Err() == nil {
		log.Error("node stopped", "err", err)
		os.Exit(1)
	}
}
