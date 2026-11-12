// Command aisathome-node runs an AIs@home node agent next to a local llama-server, with a
// local web UI on 127.0.0.1. `aisathome-node status` prints the running agent's status.
// On Windows it also runs as a service, started by the Service Control Manager, and
// `aisathome-node service install|uninstall|stop` registers it (the installer runs these).
// `aisathome-node diagnose` writes a zip for finding out why a node does not work.
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
	"path/filepath"
	"syscall"
	"time"

	"github.com/aisathome/aisathome/node"
	"github.com/aisathome/aisathome/node/engine"
	"github.com/aisathome/aisathome/node/localui"
	"github.com/aisathome/aisathome/node/setup"
	"github.com/aisathome/aisathome/node/setupflow"
)

// maxLogBytes caps the service's log file; one older file is kept beside it.
const maxLogBytes = 8 << 20

func main() {
	if len(os.Args) > 1 && os.Args[1] == "status" {
		os.Exit(localui.RunStatus(os.Args[2:], os.Stdout, os.Stderr))
	}
	if len(os.Args) > 1 && os.Args[1] == "diagnose" {
		os.Exit(runDiagnose(os.Args[2:], os.Stdout, os.Stderr))
	}
	if len(os.Args) > 1 && os.Args[1] == "service" {
		os.Exit(runServiceCommand(os.Args[2:], os.Stdout, os.Stderr))
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
			return run(ctx, *cfgPath, layout, w, layout.EngineLogFile)
		}))
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	os.Exit(run(ctx, *cfgPath, layout, os.Stderr, ""))
}

// run starts the agent and its local UI and serves until ctx ends. It returns the exit code.
// When the config has an engine, the agent runs it too, with its output to engineLog (a capped
// file; "" = the agent's log writer), and stops it before it returns. On a layout with a setup
// (Windows) the agent also runs without a complete config: then it serves the setup page and
// starts the node once the setup has written node.json (runWithSetup).
func run(ctx context.Context, cfgPath string, layout node.Layout, logw io.Writer, engineLog string) int {
	log := slog.New(slog.NewTextHandler(logw, nil))
	log.Info("starting", "version", node.Version)
	uiPort, err := localui.PortFromEnv(localui.DefaultPort)
	if err != nil {
		log.Error("local UI", "err", err)
		return 1
	}
	engineOut := logw
	if engineLog != "" {
		f, err := openLog(engineLog, maxLogBytes)
		if err != nil {
			log.Error("engine log", "err", err)
			return 1
		}
		defer f.Close()
		engineOut = f
	}
	r := &runner{cfgPath: cfgPath, layout: layout, log: log, engineOut: engineOut, uiPort: uiPort, pipe: localui.TrayPipe}
	r.runNode = r.node
	if layout.Setup && uiPort != 0 {
		return r.withSetup(ctx)
	}
	code, _ := r.node(ctx, nil)
	return code
}

// runner runs the node with one config at a time.
type runner struct {
	cfgPath   string
	layout    node.Layout
	log       *slog.Logger
	engineOut io.Writer
	uiPort    int
	// pipe is the tray's pipe (localui.TrayPipe); "" = none.
	pipe    string
	runNode func(ctx context.Context, ui *localui.Server) (int, error) // r.node; tests replace it
}

// errConfig is a config the node cannot run with (the setup page can fix it).
type errConfig struct{ error }

// node runs the agent (and its engine) with the config file until ctx ends or the agent fails.
// With ui the agent is shown there; without, it starts its own local UI. A config it cannot
// run with returns an errConfig.
func (r *runner) node(ctx context.Context, ui *localui.Server) (int, error) {
	log := r.log
	cfg, err := loadConfig(r.cfgPath, os.Getenv, r.layout)
	if err != nil {
		log.Error("config", "err", err)
		return 1, errConfig{err}
	}
	agentDir := ""
	if cfg.Engine != nil {
		if agentDir, err = ownDir(); err != nil {
			log.Error("cannot find the agent's own program", "err", err)
			return 1, nil
		}
	}
	es, err := engineSettings(&cfg, r.layout, agentDir)
	if err != nil {
		log.Error("config", "err", err)
		return 1, errConfig{err}
	}
	var agent *node.Agent
	var sup *engine.Supervisor
	if es != nil {
		sup = newEngine(es, r.engineOut, log, func(err error, in time.Duration) { agent.EngineExited(err, in) })
		cfg.EngineControl = sup
	}
	agent = node.New(cfg, log)
	if sup != nil {
		log.Info("running the engine", "program", es.Exe, "model", es.Model, "preset", cfg.Preset)
		// The engine stops with the agent: on ctx's end, or when the agent fails.
		ectx, cancel := context.WithCancel(ctx)
		done := runEngine(ectx, sup)
		defer func() {
			cancel()
			<-done
		}()
	}
	if ui != nil {
		ui.SetAgent(agent)
	} else if r.uiPort != 0 {
		ui := localui.New(agent, localui.Options{Port: r.uiPort, PasswordFile: passwordFile(os.Getenv, r.layout), Log: log})
		go func() {
			// The node keeps serving without its UI (e.g. the port is taken).
			if err := ui.ListenAndServe(ctx); err != nil {
				log.Error("local UI not available", "addr", localui.Addr(r.uiPort), "err", err)
			}
		}()
	}
	if err := agent.Run(ctx); err != nil && ctx.Err() == nil {
		log.Error("node stopped", "err", err)
		return 1, nil
	}
	return 0, nil
}

// withSetup serves the local UI with the setup page for the agent's whole life, and runs the
// node whenever node.json is complete and the volunteer is not running the setup. A new config
// from the setup restarts the node in this process; the engine is stopped between configs, so
// the setup's card check and speed test have the GPU to themselves.
func (r *runner) withSetup(ctx context.Context) int {
	log := r.log
	reload := make(chan bool, 1)
	mode := "run"
	if !setupflow.Configured(r.cfgPath, r.layout.Model) {
		mode = "setup"
	}
	dir, err := ownDir()
	if err != nil {
		log.Error("cannot find the agent's own program", "err", err)
		return 1
	}
	flow := setupflow.New(ctx, setupflow.Deps{
		ConfigPath: r.cfgPath, StateFile: r.layout.SetupStateFile, Model: r.layout.Model,
		Engine: filepath.Join(dir, r.layout.EngineProgram), SMI: setup.ExecSMI{}, Driver: setup.WindowsLlama.Driver,
		NewEngine: func(s setup.EngineSettings, onExit func(error, time.Duration)) setupflow.Runner {
			return newEngine(&s, r.engineOut, log, onExit)
		},
		Reload: func(setupMode bool) {
			select {
			case <-reload: // only the latest counts
			default:
			}
			reload <- setupMode
		},
		Log: log.With("part", "setup"),
	}, mode)
	pw := passwordFile(os.Getenv, r.layout)
	ui := localui.New(r.waiting(), localui.Options{Port: r.uiPort, PasswordFile: pw, Log: log, Setup: flow, Pipe: r.pipe})
	// The UI ends with this function, also when the node fails and the agent must exit for the
	// service's recovery to restart it.
	uictx, stopUI := context.WithCancel(ctx)
	uiDone := make(chan struct{})
	go func() {
		defer close(uiDone)
		if err := ui.ListenAndServe(uictx); err != nil {
			log.Error("local UI not available: the setup cannot run", "addr", localui.Addr(r.uiPort), "err", err)
		}
	}()
	defer func() {
		stopUI()
		<-uiDone
	}()
	setupMode := mode == "setup"
	for {
		if setupMode {
			ui.SetAgent(r.waiting())
			log.Info("waiting for the setup; the node and its engine are stopped", "page", "http://"+localui.Addr(r.uiPort)+"/setup")
			select {
			case <-ctx.Done():
				return 0
			case setupMode = <-reload:
			}
			continue
		}
		nctx, cancel := context.WithCancel(ctx)
		res := make(chan [2]any, 1)
		go func() {
			code, err := r.runNode(nctx, ui)
			res <- [2]any{code, err}
		}()
		select {
		case out := <-res:
			cancel()
			if err, _ := out[1].(error); err != nil {
				// A config the node cannot run with: the setup page fixes it, not a restart.
				flow.Hold("node.json cannot be used: " + err.Error())
				setupMode = true
				continue
			}
			return out[0].(int) // the agent failed (e.g. the watchdog): the service restarts it
		case setupMode = <-reload:
			cancel()
			<-res
			log.Info("node.json changed or the setup started; restarting the node")
		case <-ctx.Done():
			cancel()
			return (<-res)[0].(int)
		}
	}
}

// waiting is the agent the page shows while the node waits for its setup: the config as far as
// it goes (node id, gateway), with the state file's history; it never connects.
func (r *runner) waiting() *node.Agent {
	cfg, _ := loadConfig(r.cfgPath, os.Getenv, r.layout)
	cfg.Engine, cfg.EngineControl = nil, nil
	return node.New(cfg, r.log)
}

// ownDir is the folder of the agent's own program (the engine is beside it).
func ownDir() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	return filepath.Dir(exe), nil
}
