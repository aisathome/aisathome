// SPDX-License-Identifier: Apache-2.0
package setupflow

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aisathome/aisathome/node/setup"
)

// smi answers the setup's nvidia-smi query with fixed CSV lines.
type smi string

func (s smi) Query(context.Context, ...string) (string, error) { return string(s), nil }

// fakeEngine is llama-server for the speed test: /health, /tokenize and /completion with
// timings at tps, on the settings' port, until ctx ends.
type fakeEngine struct {
	s      setup.EngineSettings
	tps    float64
	fail   bool // exit at once, as an engine that cannot load
	onExit func(error, time.Duration)
	starts *atomic.Int32
}

func (e fakeEngine) Run(ctx context.Context) error {
	e.starts.Add(1)
	if e.fail {
		e.onExit(fmt.Errorf("exit status 1"), 2*time.Second)
		<-ctx.Done()
		return ctx.Err()
	}
	ln, err := net.Listen("tcp", net.JoinHostPort(e.s.Host, strconv.Itoa(e.s.Port)))
	if err != nil {
		e.onExit(err, time.Second)
		<-ctx.Done()
		return ctx.Err()
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/health":
		case "/tokenize":
			fmt.Fprint(w, `{"tokens":[1,2,3,4,5]}`)
		case "/completion":
			fmt.Fprintf(w, `{"timings":{"prompt_n":%d,"prompt_ms":900,"predicted_n":16,"predicted_per_second":%g}}`, 64, e.tps)
		}
	})}
	go srv.Serve(ln)
	<-ctx.Done()
	srv.Close()
	return ctx.Err()
}

func freePort(t *testing.T) int {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

type env struct {
	f      *Flow
	dir    string
	cfg    string
	model  []byte
	tier   setup.Tier
	reload chan bool
	starts atomic.Int32
	tps    float64
	fail   bool
}

// newEnv is a flow over a fake card (16 GB), a tiny pinned "model" on a local TLS server
// and a fake engine; card is nvidia-smi's CSV line.
func newEnv(t *testing.T, card string) *env {
	e := &env{dir: t.TempDir(), model: []byte(strings.Repeat("gguf", 4096)), reload: make(chan bool, 4), tps: 30}
	sum := sha256.Sum256(e.model)
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.ServeContent(w, r, "m.gguf", time.Time{}, strings.NewReader(string(e.model)))
	}))
	t.Cleanup(srv.Close)
	e.tier = setup.Tiers[0]
	e.tier.Model = setup.Model{URL: srv.URL + "/m.gguf", SHA256: hex.EncodeToString(sum[:]), Size: int64(len(e.model))}
	e.cfg = filepath.Join(e.dir, "node.json")
	port := freePort(t)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	e.f = New(ctx, Deps{
		ConfigPath: e.cfg, StateFile: filepath.Join(e.dir, "setup-state.json"),
		Model: filepath.Join(e.dir, "models", "model.gguf"), Engine: "llama-server",
		SMI: smi(card), Driver: setup.WindowsCUDA124, Tiers: []setup.Tier{e.tier},
		Download: setup.DownloadOptions{Client: srv.Client(), Reserve: 1, RetryDelay: time.Millisecond},
		Port:     port,
		NewEngine: func(s setup.EngineSettings, onExit func(error, time.Duration)) Runner {
			return fakeEngine{s: s, tps: e.tps, fail: e.fail, onExit: onExit, starts: &e.starts}
		},
		Ready:  5 * time.Second,
		Reload: func(setupMode bool) { e.reload <- setupMode },
	}, "setup")
	return e
}

const card4080 = "0, GPU-aaa, NVIDIA GeForce RTX 4080, 16376, 15800, 566.36, 8.9\n"

func (e *env) step(t *testing.T, do func() error) State {
	t.Helper()
	if err := do(); err != nil {
		t.Fatal(err)
	}
	e.f.Wait()
	return e.f.State()
}

// The whole setup: card, download, speed test, then the token starts the node with node.json
// written as the agent reads it.
func TestSetupFlow(t *testing.T) {
	e := newEnv(t, card4080)
	if st := e.f.State(); st.Configured || st.Mode != "setup" || st.Gateway != DefaultGatewayURL {
		t.Fatalf("fresh state %+v", st)
	}
	if err := e.f.Start("nt-n-0000000a.secret", ""); err == nil {
		t.Fatal("started before the card check")
	}
	st := e.step(t, e.f.CheckCard)
	if st.CardError != "" || st.Card == nil || st.Card.Tier != "standard" || st.Card.Several {
		t.Fatalf("card %+v %q", st.Card, st.CardError)
	}
	if err := e.f.SpeedTest(); err == nil {
		t.Fatal("speed test before the download")
	}
	st = e.step(t, e.f.Download)
	if !st.Download.Complete || st.Download.Error != "" {
		t.Fatalf("download %+v", st.Download)
	}
	if b, _ := os.ReadFile(e.f.d.Model); string(b) != string(e.model) {
		t.Fatal("model file not in place")
	}
	if err := e.f.Start("nt-n-0000000a.secret", ""); err == nil {
		t.Fatal("started before the speed test")
	}
	st = e.step(t, e.f.SpeedTest)
	if !st.Speed.Passed || st.Speed.TPS != 30 || st.Speed.Error != "" {
		t.Fatalf("speed %+v", st.Speed)
	}
	for _, bad := range []string{"", "secret", "nt-n-0000000a.se cret", "nt-xyz.abc"} {
		if err := e.f.Start(bad, ""); err == nil {
			t.Errorf("token %q accepted", bad)
		}
	}
	if err := e.f.Start("nt-n-0000000a.secret", "https://evil.example/"); err == nil {
		t.Error("an https gateway URL accepted")
	}
	os.WriteFile(e.cfg, []byte(`{"upstream":"http://x","models":[],"preset":"keep me"}`), 0o600)
	if err := e.f.Start(" nt-n-0000000a.secret\n", "wss://staging.example.org/v1/node/connect"); err != nil {
		t.Fatal(err)
	}
	if mode := <-e.reload; mode {
		t.Fatal("Start reloaded into setup mode")
	}
	var cfg map[string]any
	b, _ := os.ReadFile(e.cfg)
	if err := json.Unmarshal(b, &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg["token"] != "nt-n-0000000a.secret" || cfg["node_id"] != "n-0000000a" || cfg["gateway_url"] != "wss://staging.example.org/v1/node/connect" ||
		cfg["preset"] != "keep me" || cfg["upstream"] != nil || cfg["models"] != nil {
		t.Fatalf("node.json %s", b)
	}
	if eng, _ := cfg["engine"].(map[string]any); eng["tier"] != "standard" || eng["cuda_device"] != nil {
		t.Fatalf("engine %v", cfg["engine"])
	}
	st = e.f.State()
	if !st.Configured || st.Mode != "run" || st.NodeID != "n-0000000a" {
		t.Fatalf("after start %+v", st)
	}
	// The state kept for diagnostics never holds the token.
	if b, _ := os.ReadFile(e.f.d.StateFile); !strings.Contains(string(b), `"tier": "standard"`) || strings.Contains(string(b), "secret") {
		t.Fatalf("setup state file %s", b)
	}
	// In run mode the steps wait for "Run the setup again".
	if err := e.f.CheckCard(); err == nil {
		t.Fatal("a setup step ran while the node runs")
	}
	if err := e.f.ChangeToken("nt-n-0000000b.other", ""); err != nil {
		t.Fatal(err)
	}
	<-e.reload
	b, _ = os.ReadFile(e.cfg)
	if !strings.Contains(string(b), `"nt-n-0000000b.other"`) || !strings.Contains(string(b), `"tier": "standard"`) ||
		!strings.Contains(string(b), DefaultGatewayURL) {
		t.Fatalf("after the token change: %s", b)
	}
}

// Two cards: the larger is chosen and the engine pinned to it; a card below every tier, an old
// driver and too little free memory are refused with the reason.
func TestCardChecks(t *testing.T) {
	e := newEnv(t, "0, GPU-small, RTX 3060, 12288, 12000, 566.36, 8.6\n1, GPU-big, RTX 4080, 16376, 15800, 566.36, 8.9\n")
	st := e.step(t, e.f.CheckCard)
	if st.CardError != "" || st.Card.Index != 1 || !st.Card.Several {
		t.Fatalf("two cards: %+v %q", st.Card, st.CardError)
	}
	e.step(t, e.f.Download)
	e.step(t, e.f.SpeedTest)
	if err := e.f.Start("nt-n-0000000a.secret", ""); err != nil {
		t.Fatal(err)
	}
	<-e.reload
	if b, _ := os.ReadFile(e.cfg); !strings.Contains(string(b), `"cuda_device": "GPU-big"`) {
		t.Fatalf("node.json %s", b)
	}
	for line, want := range map[string]string{
		"0, GPU-x, RTX 3060, 12288, 12000, 566.36, 8.6\n": "16 GB or more",
		"0, GPU-x, RTX 4080, 16376, 15800, 512.15, 8.9\n": "too old",
		"0, GPU-x, RTX 4080, 16376, 9000, 566.36, 8.9\n":  "MiB free",
	} {
		e := newEnv(t, line)
		if st := e.step(t, e.f.CheckCard); !strings.Contains(st.CardError, want) {
			t.Errorf("%q: card error %q, want %q", line, st.CardError, want)
		}
		if err := e.f.Download(); err == nil {
			t.Errorf("%q: download allowed after a failed card check", line)
		}
	}
}

// A slow card is refused; an engine that cannot load ends the test with that reason at once
// (no restarts), and the engine is gone when the step ends; a taken port is refused before
// the engine starts.
func TestSpeedTestFailures(t *testing.T) {
	e := newEnv(t, card4080)
	e.tps = 12
	e.step(t, e.f.CheckCard)
	e.step(t, e.f.Download)
	st := e.step(t, e.f.SpeedTest)
	if st.Speed.Passed || !strings.Contains(st.Speed.Error, "12.0 tokens/s") {
		t.Fatalf("slow card: %+v", st.Speed)
	}
	if err := e.f.Start("nt-n-0000000a.secret", ""); err == nil {
		t.Fatal("started after a failed speed test")
	}
	e.fail = true
	start := time.Now()
	st = e.step(t, e.f.SpeedTest)
	if st.Speed.Passed || !strings.Contains(st.Speed.Error, "ended while loading") || time.Since(start) > 3*time.Second {
		t.Fatalf("engine that cannot load: %+v after %v", st.Speed, time.Since(start))
	}
	// The engine's port taken by another program.
	ln, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(e.f.d.Port)))
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	n := e.starts.Load()
	st = e.step(t, e.f.SpeedTest)
	if !strings.Contains(st.Speed.Error, fmt.Sprintf("port %d", e.f.d.Port)) || e.starts.Load() != n {
		t.Fatalf("taken port: %+v, engine starts %d -> %d", st.Speed, n, e.starts.Load())
	}
}

// Cancel stops a download; the part stays and the next download resumes from it.
func TestDownloadCancelResumes(t *testing.T) {
	e := newEnv(t, card4080)
	e.step(t, e.f.CheckCard)
	e.f.mu.Lock()
	e.f.st.Busy = "x" // a step runs
	e.f.mu.Unlock()
	if err := e.f.Download(); err != ErrBusy {
		t.Fatalf("second step: %v", err)
	}
	e.f.mu.Lock()
	e.f.st.Busy = ""
	e.f.mu.Unlock()
	part := e.f.d.Model + ".part"
	os.MkdirAll(filepath.Dir(part), 0o755)
	os.WriteFile(part, e.model[:1000], 0o644)
	st := e.step(t, e.f.Download)
	if !st.Download.Complete {
		t.Fatalf("resume: %+v", st.Download)
	}
}

// Configured: a token, and the engine's model on disk when there is an engine.
func TestConfigured(t *testing.T) {
	dir := t.TempDir()
	cfg, model := filepath.Join(dir, "node.json"), filepath.Join(dir, "model.gguf")
	for body, want := range map[string]bool{
		"":                                    false,
		`{"token":""}`:                        false,
		`{"token":"t","upstream":"http://x"}`: true,
		`{"token":"t","engine":{"tier":"standard"}}`: false, // no model yet
		`{broken`: false,
	} {
		os.Remove(cfg)
		if body != "" {
			os.WriteFile(cfg, []byte(body), 0o600)
		}
		if got := Configured(cfg, model); got != want {
			t.Errorf("%q: %v", body, got)
		}
	}
	os.WriteFile(model, []byte("x"), 0o600)
	os.WriteFile(cfg, []byte(`{"token":"t","engine":{"tier":"standard"}}`), 0o600)
	if !Configured(cfg, model) {
		t.Error("with the model")
	}
}
