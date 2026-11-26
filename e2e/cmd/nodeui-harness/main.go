// Command nodeui-harness serves the node's local page (package localui) with a real agent (package node)
// that is not connected anywhere, for the browser test of the release notice in e2e/browser. The latest
// release is -latest (no network, no gateway); -windows shows the Windows node (an agent that runs its
// engine, which its installer updates).
//
//	nodeui-harness -ui-port 7398 -password-file <file> -dir <empty folder> -version v0.7.0 -latest v0.8.0
//
// It prints "ready http://127.0.0.1:<port>/" once the page answers, and runs until killed.
//
// SPDX-License-Identifier: Apache-2.0
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"time"

	"github.com/aisathome/aisathome/node"
	"github.com/aisathome/aisathome/node/engine"
	"github.com/aisathome/aisathome/node/localui"
	"github.com/aisathome/aisathome/node/release"
	"github.com/aisathome/aisathome/protocol"
)

type known struct{ rel release.Release }

func (k known) Known() (release.Release, bool) { return k.rel, k.rel.Version != "" }

// runningEngine stands in for the engine process the Windows agent runs.
type runningEngine struct{ since time.Time }

func (runningEngine) Stop()    {}
func (runningEngine) Start()   {}
func (runningEngine) Restart() {}
func (e runningEngine) Status() engine.Status {
	return engine.Status{State: "running", PID: 4242, Since: e.since}
}

func main() {
	uiPort := flag.Int("ui-port", 7398, "the local page's port")
	pwFile := flag.String("password-file", "", "the local password's file (required)")
	dir := flag.String("dir", "", "a folder for the state file (required)")
	version := flag.String("version", "v0.7.0", "this agent's version")
	latest := flag.String("latest", "v0.8.0", `the latest release ("" = not known)`)
	windows := flag.Bool("windows", false, "the Windows node: the agent runs its engine")
	flag.Parse()
	if *pwFile == "" || *dir == "" {
		log.Fatal("usage: nodeui-harness -password-file <file> -dir <folder>")
	}
	node.Version = *version
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	cfg := node.Config{NodeID: "harness-4090", GatewayURL: "wss://api.aisathome.org/v1/node/connect", Upstream: "http://127.0.0.1:1",
		Models:    []protocol.ModelInfo{{Name: "qwen3.8-27b", Slots: 1, Ctx: 196608}},
		StateFile: filepath.Join(*dir, "node-state.json")}
	if *windows {
		cfg.EngineControl = runningEngine{since: time.Now()}
	}
	rel := release.Release{}
	if *latest != "" {
		rel = release.Release{Version: *latest, URL: release.ReleasesURL + "/tag/" + *latest}
	}
	ui := localui.New(node.New(cfg, logger), localui.Options{Port: *uiPort, PasswordFile: *pwFile, NvidiaSMI: "/nonexistent",
		Log: logger, Releases: known{rel}})
	go func() {
		if err := ui.ListenAndServe(ctx); err != nil {
			log.Fatal(err)
		}
	}()
	url := "http://" + localui.Addr(*uiPort) + "/"
	for i := 0; i < 100; i++ {
		if resp, err := http.Get(url + "api/status?gpu=0"); err == nil {
			resp.Body.Close()
			fmt.Println("ready " + url)
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	<-ctx.Done()
}
