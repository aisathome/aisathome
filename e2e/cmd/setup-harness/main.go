// Command setup-harness serves the Windows node's local page (package localui) with its setup
// (package setupflow) on any platform, against stand-ins: one NVIDIA card (an RTX 4080 16 GB,
// driver 566.36), a small "model" served over local TLS, an engine that answers the speed test
// at 31.5 tokens/s, and a gateway that accepts only the token in -good-token and refuses every
// other one with 401 "unknown node token", as the real one does. The browser tests in the ops
// repository drive it; the page, the setup steps and the agent are the real code.
//
//	setup-harness -ui-port 7399 -password-file <file> -dir <empty folder>
//
// It prints "ready http://127.0.0.1:<port>/" once the page answers, and runs until killed.
//
// SPDX-License-Identifier: Apache-2.0
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/aisathome/aisathome/node"
	"github.com/aisathome/aisathome/node/engine"
	"github.com/aisathome/aisathome/node/localui"
	"github.com/aisathome/aisathome/node/setup"
	"github.com/aisathome/aisathome/node/setupflow"
	"github.com/aisathome/aisathome/protocol"
)

func main() {
	uiPort := flag.Int("ui-port", 7399, "the local page's port")
	pwFile := flag.String("password-file", "", "the local password's file (required)")
	dir := flag.String("dir", "", "an empty folder for node.json, the model and the state (required)")
	good := flag.String("good-token", "nt-n-0000e2e0.good-secret", "the one token the stand-in gateway accepts")
	modelMB := flag.Int("model-mb", 24, "the stand-in model's size in MB (downloaded at about 8 MB/s)")
	flag.Parse()
	if *pwFile == "" || *dir == "" {
		log.Fatal("usage: setup-harness -password-file <file> -dir <folder> [-ui-port 7399]")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))

	// The model: deterministic bytes, pinned like the real ones, served slowly enough that the
	// page shows progress.
	model := make([]byte, *modelMB<<20)
	for i := range model {
		model[i] = byte(i * 31 >> 7)
	}
	sum := sha256.Sum256(model)
	files := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.ServeContent(slowWriter{w}, r, "model.gguf", time.Time{}, strings.NewReader(string(model)))
	}))
	defer files.Close()
	tier := setup.Tiers[0]
	tier.Model = setup.Model{URL: files.URL + "/model.gguf", SHA256: hex.EncodeToString(sum[:]), Size: int64(len(model))}

	gw := httptest.NewServer(gateway(*good))
	defer gw.Close()
	gwURL := "ws" + strings.TrimPrefix(gw.URL, "http") + "/v1/node/connect"

	enginePort := freePort()
	eng := &fakeEngine{port: enginePort}
	cfgPath := filepath.Join(*dir, "node.json")
	modelPath := filepath.Join(*dir, "models", "model.gguf")

	var ui *localui.Server
	var mu sync.Mutex
	var cancelNode context.CancelFunc
	// startNode runs the agent with node.json as the setup wrote it, against the stand-in
	// gateway (the gateway address on the page is only checked for its form) and engine.
	startNode := func() {
		mu.Lock()
		defer mu.Unlock()
		if cancelNode != nil {
			cancelNode()
		}
		var cfg node.Config
		b, _ := os.ReadFile(cfgPath)
		json.Unmarshal(b, &cfg)
		cfg.GatewayURL = gwURL
		cfg.Upstream = fmt.Sprintf("http://127.0.0.1:%d", enginePort)
		cfg.Models = []protocol.ModelInfo{{Name: tier.Alias, Roles: setup.EngineRoles, Slots: 1, Ctx: tier.Ctx}}
		cfg.Engine, cfg.EngineControl = nil, eng
		cfg.HealthInterval = 500 * time.Millisecond
		a := node.New(cfg, logger)
		eng.Start()
		nctx, cancel := context.WithCancel(ctx)
		cancelNode = cancel
		ui.SetAgent(a)
		go a.Run(nctx)
	}
	flow := setupflow.New(ctx, setupflow.Deps{
		ConfigPath: cfgPath, StateFile: filepath.Join(*dir, "setup-state.json"), Model: modelPath,
		Engine: "llama-server", Port: enginePort,
		SMI:    smi("0, GPU-e2e-4080, NVIDIA GeForce RTX 4080, 16376, 15700, 566.36, 8.9\n"),
		Driver: setup.WindowsCUDA124, Tiers: []setup.Tier{tier},
		Download: setup.DownloadOptions{Client: files.Client(), Reserve: 1},
		NewEngine: func(s setup.EngineSettings, _ func(error, time.Duration)) setupflow.Runner {
			return runner{eng}
		},
		Reload: func(setupMode bool) {
			if setupMode {
				mu.Lock()
				if cancelNode != nil {
					cancelNode()
					cancelNode = nil
				}
				mu.Unlock()
				eng.Stop()
				ui.SetAgent(node.New(node.Config{}, logger))
				return
			}
			startNode()
		},
		Log: logger,
	}, "setup")
	ui = localui.New(node.New(node.Config{}, logger), localui.Options{Port: *uiPort, PasswordFile: *pwFile, Log: logger, Setup: flow,
		NvidiaSMI: "true"})
	ln, err := net.Listen("tcp", localui.Addr(*uiPort))
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("ready http://%s/\n", ln.Addr())
	if err := ui.Serve(ctx, ln); err != nil {
		log.Fatal(err)
	}
}

// slowWriter sends the model at about 8 MB/s.
type slowWriter struct{ http.ResponseWriter }

func (w slowWriter) Write(p []byte) (int, error) {
	n := 0
	for len(p) > 0 {
		c := min(len(p), 64<<10)
		m, err := w.ResponseWriter.Write(p[:c])
		n += m
		if err != nil {
			return n, err
		}
		if f, ok := w.ResponseWriter.(http.Flusher); ok {
			f.Flush()
		}
		time.Sleep(8 * time.Millisecond)
		p = p[c:]
	}
	return n, nil
}

// smi answers the setup's nvidia-smi query.
type smi string

func (s smi) Query(context.Context, ...string) (string, error) { return string(s), nil }

// gateway accepts the good token and refuses others as the real gateway does (401 before the
// upgrade, with its text).
func gateway(good string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ") != good {
			http.Error(w, "unknown node token", http.StatusUnauthorized)
			return
		}
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer c.CloseNow()
		if _, _, err := c.Read(r.Context()); err != nil { // hello
			return
		}
		b, _ := json.Marshal(protocol.Msg{Type: protocol.TypeWelcome, GatewayID: "e2e-gateway", Protocol: protocol.LegacyVersion,
			ProtocolVersion: protocol.Version})
		c.Write(r.Context(), websocket.MessageText, b)
		for {
			if _, _, err := c.Read(r.Context()); err != nil {
				return
			}
		}
	})
}

// fakeEngine is llama-server for the speed test and the running node: /health, /tokenize,
// /completion with timings. It is also the node's engine control (Stop engine, Start engine).
type fakeEngine struct {
	port int
	mu   sync.Mutex
	srv  *http.Server
	pid  int
}

func (e *fakeEngine) Start() {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.srv != nil {
		return
	}
	ln, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(e.port)))
	if err != nil {
		log.Printf("fake engine: %v", err)
		return
	}
	e.srv = &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/health":
		case "/tokenize":
			fmt.Fprint(w, `{"tokens":[1,2,3,4,5,6,7,8]}`)
		case "/completion":
			time.Sleep(time.Second)
			fmt.Fprint(w, `{"timings":{"prompt_n":32000,"prompt_ms":21000,"predicted_n":256,"predicted_per_second":31.5}}`)
		default:
			http.NotFound(w, r)
		}
	})}
	e.pid++
	go e.srv.Serve(ln)
}

func (e *fakeEngine) Stop() {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.srv != nil {
		e.srv.Close()
		e.srv = nil
	}
}

func (e *fakeEngine) Restart() { e.Stop(); e.Start() }

func (e *fakeEngine) Status() engine.Status {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.srv == nil {
		return engine.Status{State: engine.StateStopped}
	}
	return engine.Status{State: engine.StateRunning, PID: 1000 + e.pid}
}

// runner runs the fake engine for the speed test: loading takes a moment, as a real one's.
type runner struct{ e *fakeEngine }

func (r runner) Run(ctx context.Context) error {
	select {
	case <-time.After(1500 * time.Millisecond):
	case <-ctx.Done():
		return ctx.Err()
	}
	r.e.Start()
	<-ctx.Done()
	r.e.Stop()
	return ctx.Err()
}

func freePort() int {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		log.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}
