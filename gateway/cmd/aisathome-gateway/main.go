// Command aisathome-gateway runs an AIs@home project gateway.
//
// SPDX-License-Identifier: AGPL-3.0-or-later
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/aisathome/aisathome/gateway"
)

func main() {
	addr := flag.String("addr", ":8080", "listen address")
	cfgPath := flag.String("config", "gateway.json", "config file (JSON)")
	flag.Parse()
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))

	cfg, err := loadConfig(*cfgPath)
	if err != nil {
		log.Error("config", "err", err)
		os.Exit(1)
	}
	g, err := gateway.New(cfg, log)
	if err != nil {
		log.Error("starting gateway", "err", err)
		os.Exit(1)
	}

	// SIGHUP reloads API keys and node tokens without dropping node connections
	// (systemctl reload). Revoked node tokens disconnect their nodes.
	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	go func() {
		for range hup {
			c, err := loadConfig(*cfgPath)
			if err != nil {
				log.Error("reload failed, keeping the old config", "err", err)
				continue
			}
			g.Reload(c)
			log.Info("config reloaded", "api_keys", len(c.APIKeys)+len(c.Keys), "node_tokens", len(c.NodeTokens))
		}
	}()
	srv := &http.Server{Addr: *addr, Handler: g.Handler(), ReadHeaderTimeout: 10 * time.Second}
	log.Info("gateway listening", "addr", *addr, "id", cfg.GatewayID)
	if err := srv.ListenAndServe(); err != nil {
		log.Error("server stopped", "err", err)
		os.Exit(1)
	}
}

func loadConfig(path string) (gateway.Config, error) {
	var cfg gateway.Config
	b, err := os.ReadFile(path)
	if err != nil {
		return cfg, fmt.Errorf("reading %s: %w", path, err)
	}
	if err := json.Unmarshal(b, &cfg); err != nil {
		return cfg, fmt.Errorf("parsing %s: %w", path, err)
	}
	return cfg, nil
}
