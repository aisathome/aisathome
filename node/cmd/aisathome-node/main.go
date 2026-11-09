// Command aisathome-node runs an AIs@home node agent next to a local llama-server, with a
// local web UI on 127.0.0.1. `aisathome-node status` prints the running agent's status.
// On Windows it also runs as a service, started by the Service Control Manager.
//
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

	"github.com/aisathome/aisathome/node"
	"github.com/aisathome/aisathome/node/localui"
)

// maxLogBytes caps the service's log file; one older file is kept beside it.
const maxLogBytes = 8 << 20

func main() {
	if len(os.Args) > 1 && os.Args[1] == "status" {
		os.Exit(localui.RunStatus(os.Args[2:], os.Stdout, os.Stderr))
	}
	layout := node.DefaultLayout()
	cfgPath := flag.String("config", layout.Config, "config file (JSON)")
	showVersion := flag.Bool("version", false, "print the version and exit")
	flag.Parse()
	if *showVersion {
		fmt.Println("aisathome-node", node.Version)
		return
	}
	asService, err := runningAsService()
	if err != nil {
		fmt.Fprintln(os.Stderr, "cannot tell whether this runs as a service:", err)
		os.Exit(1)
	}
	if asService {
		// A service has no console: the log goes to a file in the agent's folder.
		os.Exit(runService(func(ctx context.Context) int {
			w, err := openLog(layout.LogFile, maxLogBytes)
			if err != nil {
				return 1
			}
			defer w.Close()
			return run(ctx, *cfgPath, layout, w)
		}))
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	os.Exit(run(ctx, *cfgPath, layout, os.Stderr))
}

// run starts the agent and its local UI and serves until ctx ends. It returns the exit code.
func run(ctx context.Context, cfgPath string, layout node.Layout, logw io.Writer) int {
	log := slog.New(slog.NewTextHandler(logw, nil))
	log.Info("starting", "version", node.Version)
	cfg, err := loadConfig(cfgPath, os.Getenv, layout)
	if err != nil {
		log.Error("config", "err", err)
		return 1
	}
	uiPort, err := localui.PortFromEnv(localui.DefaultPort)
	if err != nil {
		log.Error("local UI", "err", err)
		return 1
	}
	agent := node.New(cfg, log)
	if uiPort != 0 {
		ui := localui.New(agent, localui.Options{Port: uiPort, PasswordFile: passwordFile(os.Getenv, layout), Log: log})
		go func() {
			// The node keeps serving without its UI (e.g. the port is taken).
			if err := ui.ListenAndServe(ctx); err != nil {
				log.Error("local UI not available", "addr", localui.Addr(uiPort), "err", err)
			}
		}()
	}
	if err := agent.Run(ctx); err != nil && ctx.Err() == nil {
		log.Error("node stopped", "err", err)
		return 1
	}
	return 0
}
