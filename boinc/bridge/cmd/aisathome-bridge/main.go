// Command aisathome-bridge joins a BOINC project to an AIs@home gateway (boinc/README.md, "The
// bridge"). It runs on the BOINC project's host, as the project's user, next to the stock server.
//
//	aisathome-bridge -config bridge.json           # run until SIGTERM
//	aisathome-bridge -config bridge.json -check    # check the config, the database and the gateway, then exit
//
// SPDX-License-Identifier: Apache-2.0
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/aisathome/aisathome/boinc/bridge"
)

func main() {
	cfgPath := flag.String("config", "bridge.json", "config file (JSON)")
	check := flag.Bool("check", false, "check the config, the project's database and the gateway, then exit")
	flag.Parse()
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	if err := run(*cfgPath, *check, log); err != nil {
		log.Error("aisathome-bridge", "err", err)
		os.Exit(1)
	}
}

func run(cfgPath string, check bool, log *slog.Logger) error {
	cfg, err := bridge.LoadConfig(cfgPath)
	if err != nil {
		return err
	}
	key, err := bridge.ReadSecret(cfg.KeyFile)
	if err != nil {
		return fmt.Errorf("key_file: %w", err)
	}
	invite := ""
	if cfg.InviteCodeFile != "" {
		if invite, err = bridge.ReadSecret(cfg.InviteCodeFile); err != nil {
			return fmt.Errorf("invite_code_file: %w", err)
		}
	}
	db, err := bridge.OpenMySQL(cfg)
	if err != nil {
		return err
	}
	defer db.Close()
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	pctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	err = db.Ping(pctx)
	cancel()
	if err != nil {
		return fmt.Errorf("project database: %w", err)
	}
	tools := bridge.NewExecTools(cfg)
	b, err := bridge.New(cfg, key, db, tools, bridge.NewWebRPC(cfg.WebURL, invite), log)
	if err != nil {
		return err
	}
	// Never stage a credential where the download directory is listed.
	lctx, lcancel := context.WithTimeout(ctx, 30*time.Second)
	err = tools.CheckNotListed(lctx)
	lcancel()
	if err != nil {
		return err
	}
	if check {
		cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		if err := b.Check(cctx); err != nil {
			return err
		}
		fmt.Println("ok: config, project database and gateway")
		return nil
	}
	log.Info("aisathome-bridge started", "gateway", cfg.Gateway, "connector", cfg.Connector, "app", cfg.App,
		"batch", cfg.Batch, "target_host", cfg.TargetHost, "zero_credit", cfg.ZeroCredit)
	b.Run(ctx)
	log.Info("aisathome-bridge stopped")
	return nil
}
