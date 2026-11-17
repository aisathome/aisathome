// SPDX-License-Identifier: Apache-2.0
package pilot

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/aisathome/aisathome/node"
	"github.com/aisathome/aisathome/node/engine"
	"github.com/aisathome/aisathome/node/setup"
	"github.com/aisathome/aisathome/protocol"
)

// gateway is a stand-in for the gateway's node endpoint: it welcomes one node at a time,
// records every frame and header it gets and the close reason, and sends requests on demand.
type gateway struct {
	srv *httptest.Server

	mu      sync.Mutex
	frames  [][]byte
	headers []string
	reason  string
	closed  chan struct{} // closed when the node's connection ends
	conn    *websocket.Conn
	ready   chan struct{} // closed once a node is welcomed
	once    sync.Once
	done    map[string]bool
	paused  bool
	reqSeen atomic.Int32
}

func newGateway(t *testing.T) *gateway {
	g := &gateway{closed: make(chan struct{}), ready: make(chan struct{}), done: map[string]bool{}}
	g.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		g.mu.Lock()
		for k, v := range r.Header {
			g.headers = append(g.headers, k+": "+strings.Join(v, ","))
		}
		g.mu.Unlock()
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		ctx := r.Context()
		_, b, err := c.Read(ctx)
		if err != nil {
			return
		}
		g.record(b)
		welcome, _ := json.Marshal(protocol.Msg{Type: protocol.TypeWelcome, GatewayID: "gw-test", ProtocolVersion: protocol.Version})
		if c.Write(ctx, websocket.MessageText, welcome) != nil {
			return
		}
		g.mu.Lock()
		g.conn = c
		g.mu.Unlock()
		g.once.Do(func() { close(g.ready) })
		for {
			_, b, err := c.Read(ctx)
			if err != nil {
				var ce websocket.CloseError
				g.mu.Lock()
				if errors.As(err, &ce) {
					g.reason = ce.Reason
				}
				g.mu.Unlock()
				select {
				case <-g.closed:
				default:
					close(g.closed)
				}
				return
			}
			g.record(b)
		}
	}))
	t.Cleanup(g.srv.Close)
	return g
}

func (g *gateway) url() string {
	return "ws" + strings.TrimPrefix(g.srv.URL, "http") + "/v1/node/connect"
}

func (g *gateway) record(b []byte) {
	var m protocol.Msg
	json.Unmarshal(b, &m)
	g.mu.Lock()
	defer g.mu.Unlock()
	g.frames = append(g.frames, append([]byte(nil), b...))
	switch m.Type {
	case protocol.TypeDone, protocol.TypeError:
		g.done[m.ReqID] = true
	case protocol.TypeHeartbeat:
		if m.Paused {
			g.paused = true
		}
	}
}

// request sends one chat request to the node.
func (g *gateway) request(t *testing.T, id string) {
	t.Helper()
	g.mu.Lock()
	c := g.conn
	g.mu.Unlock()
	b, _ := json.Marshal(protocol.Msg{Type: protocol.TypeRequest, ReqID: id,
		Body: json.RawMessage(`{"model":"m","stream":true,"messages":[{"role":"user","content":"hi"}]}`)})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.Write(ctx, websocket.MessageText, b); err != nil {
		t.Errorf("request %s: %v", id, err)
	}
	g.reqSeen.Add(1)
}

func (g *gateway) closeReason() string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.reason
}

func (g *gateway) isDone(id string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.done[id]
}

func (g *gateway) sawPause() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.paused
}

// wire is everything the gateway received: frames and headers.
func (g *gateway) wire() string {
	g.mu.Lock()
	defer g.mu.Unlock()
	var b bytes.Buffer
	for _, h := range g.headers {
		b.WriteString(h + "\n")
	}
	for _, f := range g.frames {
		b.Write(f)
		b.WriteByte('\n')
	}
	return b.String()
}

// fakeEngine stands in for llama-server: it serves /health and a streamed completion on the
// port the pilot chose, taking chunkDelay between its chunks.
type fakeEngine struct {
	port       int
	chunks     int
	chunkDelay time.Duration
	failRuns   int // call onExit this many times right away (an engine that keeps crashing)
	onExit     func(error, time.Duration)
	stopped    atomic.Bool
}

func (f *fakeEngine) Stop()    {}
func (f *fakeEngine) Start()   {}
func (f *fakeEngine) Restart() {}
func (f *fakeEngine) Status() engine.Status {
	return engine.Status{State: engine.StateRunning}
}

func (f *fakeEngine) Run(ctx context.Context) error {
	for i := 0; i < f.failRuns; i++ {
		f.onExit(errors.New("exit status 1"), time.Millisecond)
	}
	l, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(f.port)))
	if err != nil {
		return err
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/health":
			fmt.Fprint(w, `{"status":"ok"}`)
			return
		case "/slots":
			http.Error(w, "no slots", http.StatusNotImplemented)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		for i := 0; i < f.chunks; i++ {
			select {
			case <-r.Context().Done():
				return
			case <-time.After(f.chunkDelay):
			}
			fmt.Fprint(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"x\"},\"finish_reason\":null}]}\n\n")
			w.(http.Flusher).Flush()
		}
		fmt.Fprint(w, "data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n")
		fmt.Fprint(w, "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":5,\"completion_tokens\":2}}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	})}
	go srv.Serve(l)
	<-ctx.Done()
	srv.Close()
	f.stopped.Store(true)
	return nil
}

// rig is one pilot run against a gateway and a fake engine, in a BOINC-like slot folder.
type rig struct {
	t    *testing.T
	gw   *gateway
	eng  *fakeEngine
	slot string
	o    Options
	d    Deps
	log  *bytes.Buffer
	logm sync.Mutex
}

type lockedWriter struct {
	mu *sync.Mutex
	w  io.Writer
}

func (l lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

func newRig(t *testing.T) *rig {
	r := &rig{t: t, gw: newGateway(t), eng: &fakeEngine{chunks: 2, chunkDelay: 5 * time.Millisecond}, slot: t.TempDir(), log: &bytes.Buffer{}}
	write := func(name, s string) string {
		p := filepath.Join(r.slot, name)
		if err := os.WriteFile(p, []byte(s), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	r.o = Options{
		Gateway:        r.gw.url(),
		CredentialFile: write("credential", "nt-n-0000beef.s3cretcredential\n"),
		Model:          write("model.gguf", "GGUF"),
		LlamaServer:    write("llama-server", "#!/bin/sh\n"),
		IdleExit:       time.Minute, MaxServe: time.Hour,
		Checkpoint:   filepath.Join(r.slot, "pilot_checkpoint"),
		FractionDone: filepath.Join(r.slot, "pilot_fraction"),
		Summary:      filepath.Join(r.slot, "summary.json"),
	}
	r.d = Deps{
		Log:  slog.New(slog.NewTextHandler(lockedWriter{&r.logm, r.log}, nil)),
		Tick: 10 * time.Millisecond,
		NewEngine: func(cmd engine.Command, s setup.EngineSettings, onExit func(error, time.Duration)) Engine {
			r.eng.port, r.eng.onExit = s.Port, onExit
			return r.eng
		},
		AgentConfig: func(c *node.Config) {
			c.HealthInterval = 20 * time.Millisecond
			c.Heartbeat = 50 * time.Millisecond
		},
	}
	return r
}

// run starts the pilot; the result arrives on the channel.
func (r *rig) run(ctx context.Context) <-chan Result {
	ch := make(chan Result, 1)
	go func() { ch <- Run(ctx, r.o, r.d) }()
	return ch
}

func (r *rig) logs() string {
	r.logm.Lock()
	defer r.logm.Unlock()
	return r.log.String()
}

func (r *rig) summary() Summary {
	r.t.Helper()
	var s Summary
	b, err := os.ReadFile(r.o.Summary)
	if err != nil {
		r.t.Fatalf("summary: %v", err)
	}
	if err := json.Unmarshal(b, &s); err != nil {
		r.t.Fatalf("summary %q: %v", b, err)
	}
	return s
}

func wait(t *testing.T, ch <-chan Result, d time.Duration) Result {
	t.Helper()
	select {
	case res := <-ch:
		return res
	case <-time.After(d):
		t.Fatalf("the pilot did not end within %s", d)
	}
	return Result{}
}

func connected(t *testing.T, g *gateway) {
	t.Helper()
	select {
	case <-g.ready:
	case <-time.After(10 * time.Second):
		t.Fatal("the pilot did not connect")
	}
}

// A pilot nobody sends work to ends after the idle time, leaves with "pilot: idle", exits 0
// and says so in its summary.
func TestIdleExit(t *testing.T) {
	r := newRig(t)
	r.o.IdleExit = 400 * time.Millisecond
	res := wait(t, r.run(context.Background()), 10*time.Second)
	if res.Reason != ReasonIdle || res.Code != 0 {
		t.Fatalf("result %+v, want idle, 0", res)
	}
	if got := r.gw.closeReason(); got != "pilot: idle" {
		t.Errorf("close reason %q, want %q", got, "pilot: idle")
	}
	if s := r.summary(); s.Reason != ReasonIdle || !s.Connected || s.Requests != 0 {
		t.Errorf("summary %+v", s)
	}
	if b, _ := os.ReadFile(r.o.FractionDone); strings.TrimSpace(string(b)) != "1.0000" {
		t.Errorf("fraction done %q, want 1.0000 at the end", b)
	}
	if !r.eng.stopped.Load() {
		t.Error("the engine is still running")
	}
}

// Requests keep a pilot alive: it ends only once they stop for the idle time.
func TestRequestsDeferIdleExit(t *testing.T) {
	r := newRig(t)
	r.o.IdleExit = 300 * time.Millisecond
	start := time.Now()
	ch := r.run(context.Background())
	connected(t, r.gw)
	var busy time.Duration
	for i := 0; i < 8; i++ {
		r.gw.request(t, "r"+strconv.Itoa(i))
		busy = time.Since(start)
		time.Sleep(100 * time.Millisecond)
	}
	res := wait(t, ch, 10*time.Second)
	if res.Reason != ReasonIdle {
		t.Fatalf("result %+v, want idle", res)
	}
	if took := time.Since(start); took < busy+r.o.IdleExit {
		t.Errorf("ended %s after start, but the last request came at %s: the idle time must count from it", took, busy)
	}
	if s := r.summary(); s.Requests != 8 || s.CompletionTokens != 16 {
		t.Errorf("summary %+v, want 8 requests and 16 completion tokens", s)
	}
}

// At the time limit the pilot pauses (the gateway routes nothing new), lets the request in
// flight finish, then leaves with "pilot: time".
func TestTimeLimitDrains(t *testing.T) {
	r := newRig(t)
	r.eng.chunks, r.eng.chunkDelay = 20, 50*time.Millisecond // about 1 s
	r.o.MaxServe = 1500 * time.Millisecond
	ch := r.run(context.Background())
	connected(t, r.gw)
	time.Sleep(time.Second)
	r.gw.request(t, "long")
	res := wait(t, ch, 15*time.Second)
	if res.Reason != ReasonTime || res.Code != 0 {
		t.Fatalf("result %+v, want time, 0", res)
	}
	if !r.gw.sawPause() {
		t.Error("the gateway never saw a paused heartbeat")
	}
	if !r.gw.isDone("long") {
		t.Error("the request in flight did not finish before the pilot left")
	}
	if got := r.gw.closeReason(); got != "pilot: time" {
		t.Errorf("close reason %q", got)
	}
}

// SIGTERM (ctx's end) with a request that never finishes: the pilot still leaves with
// "pilot: sigterm" and returns well within the wrapper's 10 s.
func TestSigtermStopsInTime(t *testing.T) {
	r := newRig(t)
	r.eng.chunks, r.eng.chunkDelay = 100000, 50*time.Millisecond
	ctx, sigterm := context.WithCancel(context.Background())
	ch := r.run(ctx)
	connected(t, r.gw)
	r.gw.request(t, "endless")
	time.Sleep(200 * time.Millisecond)
	start := time.Now()
	sigterm()
	res := wait(t, ch, 10*time.Second)
	if took := time.Since(start); took > 8*time.Second {
		t.Errorf("stop took %s, more than the 8 s budget", took)
	}
	if res.Reason != ReasonSigterm || res.Code != 0 {
		t.Fatalf("result %+v, want sigterm, 0", res)
	}
	if got := r.gw.closeReason(); got != "pilot: sigterm" {
		t.Errorf("close reason %q", got)
	}
	if !r.eng.stopped.Load() {
		t.Error("the engine is still running")
	}
}

// When the wrapper dies without a SIGTERM (it was killed), the pilot notices that its parent
// changed and stops as for SIGTERM.
func TestParentGone(t *testing.T) {
	r := newRig(t)
	var gone atomic.Bool
	r.d.Parent = func() int {
		if gone.Load() {
			return 1
		}
		return 4242
	}
	ch := r.run(context.Background())
	connected(t, r.gw)
	gone.Store(true)
	if res := wait(t, ch, 10*time.Second); res.Reason != ReasonSigterm {
		t.Fatalf("result %+v, want sigterm", res)
	}
}

// An engine that keeps ending on its own ends the pilot with an error (BOINC reports it).
func TestEngineFailure(t *testing.T) {
	r := newRig(t)
	r.eng.failRuns = engineFailures
	res := wait(t, r.run(context.Background()), 10*time.Second)
	if res.Reason != ReasonEngine || res.Code != 1 {
		t.Fatalf("result %+v, want engine, 1", res)
	}
}

// The checkpoint rule: past a slice boundary and idle long enough; never while busy.
func TestCheckpointDue(t *testing.T) {
	t0 := time.Date(2026, 11, 17, 0, 0, 0, 0, time.UTC)
	slice, yield := time.Hour, 5*time.Minute
	for _, c := range []struct {
		name     string
		now      time.Duration
		idle     time.Duration
		due      bool
		nextFrom time.Duration
	}{
		{"before the first boundary", 59 * time.Minute, time.Hour, false, time.Hour},
		{"at the boundary but busy", 61 * time.Minute, time.Minute, false, time.Hour},
		{"past the boundary and idle", 66 * time.Minute, 6 * time.Minute, true, 2 * time.Hour},
		{"two boundaries late", 150 * time.Minute, 10 * time.Minute, true, 3 * time.Hour},
	} {
		due, next := checkpointDue(t0.Add(c.now), t0.Add(time.Hour), c.idle, slice, yield)
		if due != c.due || !next.Equal(t0.Add(c.nextFrom)) {
			t.Errorf("%s: due %v next %s, want %v %s", c.name, due, next.Sub(t0), c.due, c.nextFrom)
		}
	}
}

// The checkpoint file exists from the start (the wrapper counts changes, not appearance) and
// is touched when an idle pilot passes a slice boundary.
func TestCheckpointTouchedWhenIdle(t *testing.T) {
	r := newRig(t)
	r.o.IdleExit, r.o.Slice, r.o.YieldIdle = 3*time.Second, 300*time.Millisecond, 100*time.Millisecond
	ch := r.run(context.Background())
	connected(t, r.gw)
	fi, err := os.Stat(r.o.Checkpoint)
	if err != nil {
		t.Fatalf("no checkpoint file at start: %v", err)
	}
	first := fi.ModTime()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if fi, err := os.Stat(r.o.Checkpoint); err == nil && fi.ModTime().After(first) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the idle pilot never touched its checkpoint file")
		}
		time.Sleep(20 * time.Millisecond)
	}
	wait(t, ch, 10*time.Second)
}

// BOINC's init_data.xml in the task's folder holds the volunteer's account key. The pilot never
// reads it, so the key reaches neither the gateway (frames, headers) nor the log BOINC uploads
// nor the output file. The credential itself goes only into the Authorization header.
func TestAccountKeyNeverLeaves(t *testing.T) {
	r := newRig(t)
	const key = "0123456789abcdef0123456789abcdef"
	os.WriteFile(filepath.Join(r.slot, "init_data.xml"), []byte("<app_init_data>\n<authenticator>"+key+
		"</authenticator>\n<user_name>volunteer</user_name>\n</app_init_data>\n"), 0o600)
	r.o.IdleExit = 500 * time.Millisecond
	ch := r.run(context.Background())
	connected(t, r.gw)
	r.gw.request(t, "r1")
	wait(t, ch, 10*time.Second)
	summary, _ := os.ReadFile(r.o.Summary)
	for what, s := range map[string]string{"gateway": r.gw.wire(), "log": r.logs(), "summary": string(summary)} {
		if strings.Contains(s, key) {
			t.Errorf("the account key appears in the %s", what)
		}
		if what != "gateway" && strings.Contains(s, "s3cretcredential") {
			t.Errorf("the credential appears in the %s", what)
		}
	}
	if !strings.Contains(r.gw.wire(), "Authorization: Bearer nt-n-0000beef.s3cretcredential") {
		t.Error("the pilot did not authenticate with its credential")
	}
}

func TestCredentialFile(t *testing.T) {
	dir := t.TempDir()
	for name, content := range map[string]string{"empty": "", "two words": "a b", "two lines": "a\nb\n"} {
		p := filepath.Join(dir, name)
		os.WriteFile(p, []byte(content), 0o600)
		if _, err := readCredential(p); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, err := readCredential(filepath.Join(dir, "missing")); err == nil {
		t.Error("missing file accepted")
	}
	p := filepath.Join(dir, "ok")
	os.WriteFile(p, []byte("nt-n-0000beef.secret\n"), 0o600)
	if c, err := readCredential(p); err != nil || c != "nt-n-0000beef.secret" {
		t.Errorf("got %q, %v", c, err)
	}
}

// A pilot that cannot start (no credential) exits 1 with an error summary, before any engine.
func TestStartErrors(t *testing.T) {
	r := newRig(t)
	r.o.CredentialFile = filepath.Join(r.slot, "missing")
	res := wait(t, r.run(context.Background()), 5*time.Second)
	if res.Code != 1 {
		t.Fatalf("result %+v, want code 1", res)
	}
	if s := r.summary(); s.Error == "" {
		t.Errorf("summary %+v has no error", s)
	}
	r.o.CredentialFile = filepath.Join(r.slot, "credential")
	r.o.Gateway = "https://example.org/v1/node/connect"
	if res := wait(t, r.run(context.Background()), 5*time.Second); res.Code != 1 {
		t.Fatalf("http gateway URL: result %+v, want code 1", res)
	}
}

// The bundle is unpacked once and reused; an archive without llama-server is refused.
func TestUnpack(t *testing.T) {
	if _, err := exec.LookPath("xz"); err != nil {
		t.Skip("no xz")
	}
	src, dir := t.TempDir(), t.TempDir()
	b := filepath.Join(src, "aisathome-node-bundle-linux-amd64")
	os.MkdirAll(filepath.Join(b, "bin"), 0o755)
	os.MkdirAll(filepath.Join(b, "lib"), 0o755)
	os.WriteFile(filepath.Join(b, "bin", "llama-server"), []byte("#!/bin/sh\n"), 0o755)
	archive := filepath.Join(src, "bundle_abc123.tar.xz")
	if out, err := exec.Command("tar", "-cJf", archive, "-C", src, "aisathome-node-bundle-linux-amd64").CombinedOutput(); err != nil {
		t.Fatalf("tar: %v %s", err, out)
	}
	exe, lib, err := Unpack(archive, dir)
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(dir, "bundle_abc123", "aisathome-node-bundle-linux-amd64")
	if exe != filepath.Join(want, "bin", "llama-server") || lib != filepath.Join(want, "lib") {
		t.Fatalf("exe %s lib %s", exe, lib)
	}
	os.Remove(archive) // the second pilot must not need it
	if exe2, _, err := Unpack(archive, dir); err != nil || exe2 != exe {
		t.Fatalf("reuse: %s %v", exe2, err)
	}
	ents, _ := os.ReadDir(dir)
	if len(ents) != 1 {
		t.Errorf("left behind: %v", ents)
	}

	bad := filepath.Join(src, "bad.tar.xz")
	os.WriteFile(filepath.Join(src, "x"), []byte("x"), 0o644)
	exec.Command("tar", "-cJf", bad, "-C", src, "x").Run()
	if _, _, err := Unpack(bad, dir); err == nil {
		t.Error("an archive without llama-server was accepted")
	}
}
