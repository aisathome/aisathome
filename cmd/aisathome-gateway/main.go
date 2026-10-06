// Command aisathome-gateway runs an AIs@home project gateway.
//
// SPDX-License-Identifier: AGPL-3.0-or-later
package main

import (
	"encoding/json"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"time"

	"aisathome/internal/gateway"
)

func main() {
	addr := flag.String("addr", ":8080", "listen address")
	cfgPath := flag.String("config", "gateway.json", "config file (JSON)")
	flag.Parse()
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))

	b, err := os.ReadFile(*cfgPath)
	if err != nil {
		log.Error("reading config", "err", err)
		os.Exit(1)
	}
	var cfg gateway.Config
	if err := json.Unmarshal(b, &cfg); err != nil {
		log.Error("parsing config", "err", err)
		os.Exit(1)
	}
	g, err := gateway.New(cfg, log)
	if err != nil {
		log.Error("starting gateway", "err", err)
		os.Exit(1)
	}
	srv := &http.Server{Addr: *addr, Handler: g.Handler(), ReadHeaderTimeout: 10 * time.Second}
	log.Info("gateway listening", "addr", *addr, "id", cfg.GatewayID)
	if err := srv.ListenAndServe(); err != nil {
		log.Error("server stopped", "err", err)
		os.Exit(1)
	}
}
