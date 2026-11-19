// SPDX-License-Identifier: Apache-2.0
package pilot

import (
	"archive/tar"
	"bytes"
	"context"
	crand "crypto/rand"
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
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/ulikunitz/xz"

	"github.com/aisathome/aisathome/node"
	"github.com/aisathome/aisathome/node/engine"
	"github.com/aisathome/aisathome/node/setup"
	"github.com/aisathome/aisathome/protocol"
)

// gateway is a stand-in for the gateway's node endpoint: it welcomes one node at a time,
// records every frame and header it gets and the close reason, and sends requests on demand.
// Tests wait on what it records (until), never on sleeps: the pilot's Run can return before this
// side has read the close frame (seen in CI on 2026-11-18).
type gateway struct {
	srv *httptest.Server

	mu       sync.Mutex
	frames   [][]byte
	headers  []string
	reason   string
	closed   chan struct{} // closed when the node's connection ends, after reason is set
	conn     *websocket.Conn
	ready    chan struct{} // closed once a node is welcomed
	once     sync.Once
	done     map[string]string // request id → how it ended: "done" or "error"
	answered map[string]bool   // request ids the node sent any frame for
	paused   bool
	wake     chan struct{} // closed and replaced whenever a frame arrives
	reqSeen  atomic.Int32
}

func newGateway(t *testing.T) *gateway {
	g := &gateway{closed: make(chan struct{}), ready: make(chan struct{}), done: map[string]string{},
		answered: map[string]bool{}, wake: make(chan struct{})}
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
	if m.ReqID != "" {
		g.answered[m.ReqID] = true
	}
	switch m.Type {
	case protocol.TypeDone, protocol.TypeError:
		g.done[m.ReqID] = m.Type
	case protocol.TypeHeartbeat:
		if m.Paused {
			g.paused = true
		}
	}
	close(g.wake)
	g.wake = make(chan struct{})
}

// waitFor is how long a test waits for something the pilot should do within milliseconds: long
// enough for a loaded CI runner under -race, and still a failure rather than a hang.
const waitFor = 10 * time.Second

// until waits until cond, called with g.mu held, holds; at most waitFor.
func (g *gateway) until(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.After(waitFor)
	for {
		g.mu.Lock()
		ok, wake := cond(), g.wake
		g.mu.Unlock()
		if ok {
			return
		}
		select {
		case <-wake:
		case <-deadline:
			t.Fatalf("waited %s for %s", waitFor, what)
		}
	}
}

// waitAnswered waits until the node answers request id (its first chunk, or an error).
func (g *gateway) waitAnswered(t *testing.T, id string) {
	t.Helper()
	g.until(t, "an answer to request "+id, func() bool { return g.answered[id] })
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

// closeReason is the reason in the node's close frame. It waits for the connection to end: the
// node's close handshake completes when this side replies, which is before it returns the
// reason from Read.
func (g *gateway) closeReason(t *testing.T) string {
	t.Helper()
	select {
	case <-g.closed:
	case <-time.After(waitFor):
		t.Fatalf("the node's connection did not end within %s", waitFor)
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.reason
}

// ended is how request id ended: "done", "error" or "" (not yet).
func (g *gateway) ended(id string) string {
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
	stuck      chan struct{} // when set, Run returns only once it is closed: an engine slow to stop
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
	if f.stuck != nil {
		<-f.stuck
	}
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
	skew atomic.Int64 // added to the pilot's clock (Deps.Now): jump moves it on
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
		Now:  func() time.Time { return time.Now().Add(time.Duration(r.skew.Load())) },
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

// jump moves the pilot's clock on by d, so that a time rule fires at a moment the test chose
// rather than after a sleep.
func (r *rig) jump(d time.Duration) { r.skew.Add(int64(d)) }

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
	if got := r.gw.closeReason(t); got != "pilot: idle" {
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
	ch := r.run(context.Background())
	connected(t, r.gw)
	r.gw.request(t, "long")
	r.gw.waitAnswered(t, "long")
	r.jump(r.o.MaxServe) // the time limit passes while the request is in flight
	res := wait(t, ch, 15*time.Second)
	if res.Reason != ReasonTime || res.Code != 0 {
		t.Fatalf("result %+v, want time, 0", res)
	}
	if !r.gw.sawPause() {
		t.Error("the gateway never saw a paused heartbeat")
	}
	if got := r.gw.ended("long"); got != protocol.TypeDone {
		t.Errorf("the request in flight ended with %q before the pilot left, want %q", got, protocol.TypeDone)
	}
	if got := r.gw.closeReason(t); got != "pilot: time" {
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
	r.gw.waitAnswered(t, "endless") // in flight: the stop drains it, briefly
	start := time.Now()
	sigterm()
	res := wait(t, ch, 10*time.Second)
	if took := time.Since(start); took > 8*time.Second {
		t.Errorf("stop took %s, more than the 8 s budget", took)
	}
	if res.Reason != ReasonSigterm || res.Code != 0 {
		t.Fatalf("result %+v, want sigterm, 0", res)
	}
	if got := r.gw.closeReason(t); got != "pilot: sigterm" {
		t.Errorf("close reason %q", got)
	}
	if !r.eng.stopped.Load() {
		t.Error("the engine is still running")
	}
}

// A SIGTERM during the drain at the time limit still stops the pilot within the stop budget,
// not after the drain, even with an engine slow to stop: the budget is wall-clock time, however
// far the pilot's clock has moved on.
func TestSigtermDuringDrain(t *testing.T) {
	r := newRig(t)
	r.eng.chunks, r.eng.chunkDelay = 100000, 50*time.Millisecond
	r.eng.stuck = make(chan struct{})
	t.Cleanup(func() { close(r.eng.stuck) })
	r.o.DrainWait, r.o.StopBudget = time.Minute, 4*time.Second
	ctx, sigterm := context.WithCancel(context.Background())
	ch := r.run(ctx)
	connected(t, r.gw)
	r.gw.request(t, "endless")
	r.gw.waitAnswered(t, "endless")
	r.jump(r.o.MaxServe)
	r.gw.until(t, "a paused heartbeat at the time limit", func() bool { return r.gw.paused })
	start := time.Now()
	sigterm()
	res := wait(t, ch, 10*time.Second)
	// The pilot gives the stuck engine the whole budget, then writes its summary and returns.
	if took := time.Since(start); took > r.o.StopBudget+time.Second {
		t.Errorf("stop took %s, more than the %s budget", took, r.o.StopBudget)
	}
	if got := r.gw.closeReason(t); res.Reason != ReasonSigterm || got != "pilot: sigterm" {
		t.Errorf("result %+v, close reason %q; want sigterm", res, got)
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

// In a BOINC slot the credential is a soft-link file (or a symlink) to its physical file in the
// project folder. The pilot follows it itself, and the physical name, which is also the file's
// download URL, appears neither in its log nor in its summary, even when the file is missing.
func TestCredentialSoftLink(t *testing.T) {
	const phys = "pc-p-test-0123456789abcdef0123456789abcdef"
	for _, kind := range []string{"soft link", "symlink"} {
		t.Run(kind, func(t *testing.T) {
			if kind == "symlink" {
				linuxBundle(t) // BOINC makes symlinks on Unix only
			}
			r := newRig(t)
			project := filepath.Join(filepath.Dir(r.slot), "projects", "example.org_x")
			os.MkdirAll(project, 0o755)
			rel, _ := filepath.Rel(r.slot, filepath.Join(project, phys))
			link := filepath.Join(r.slot, "credential")
			os.Remove(link)
			if kind == "symlink" {
				os.Symlink(rel, link)
			} else {
				os.WriteFile(link, []byte("<soft_link>"+rel+"</soft_link>\n"), 0o644)
			}
			os.WriteFile(filepath.Join(project, phys), []byte("nt-n-0000beef.linkedsecret\n"), 0o600)
			r.o.IdleExit = 500 * time.Millisecond
			ch := r.run(context.Background())
			connected(t, r.gw)
			wait(t, ch, 10*time.Second)
			if !strings.Contains(r.gw.wire(), "Authorization: Bearer nt-n-0000beef.linkedsecret\n") {
				t.Errorf("the pilot did not authenticate with the linked credential:\n%s", r.gw.wire())
			}
			os.Remove(filepath.Join(project, phys)) // deleted, or not downloaded
			if res := wait(t, r.run(context.Background()), 5*time.Second); res.Code != 1 {
				t.Errorf("missing linked file: %+v", res)
			}
			summary, _ := os.ReadFile(r.o.Summary)
			for what, s := range map[string]string{"log": r.logs(), "summary": string(summary)} {
				if strings.Contains(s, phys) || strings.Contains(s, "linkedsecret") {
					t.Errorf("the credential's physical name or value appears in the %s:\n%s", what, s)
				}
			}
		})
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

// entry is one file of a test archive: a regular file, or a symlink when link is set.
type entry struct {
	name, body, link string
	mode             int64
}

// writeArchive writes a .tar.xz of entries in Go, so that the tests need no tar or xz program.
func writeArchive(t *testing.T, path string, entries []entry) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	xw, err := xz.NewWriter(f)
	if err != nil {
		t.Fatal(err)
	}
	tw := tar.NewWriter(xw)
	for _, e := range entries {
		h := &tar.Header{Name: e.name, Mode: e.mode, Size: int64(len(e.body)), Typeflag: tar.TypeReg}
		if e.link != "" {
			h.Typeflag, h.Linkname, h.Size = tar.TypeSymlink, e.link, 0
		}
		if h.Mode == 0 {
			h.Mode = 0o644
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(e.body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := xw.Close(); err != nil {
		t.Fatal(err)
	}
}

const bundleDir = "aisathome-node-bundle-linux-amd64"

func bundleEntries() []entry {
	return []entry{
		{name: bundleDir + "/bin/llama-server", body: "#!/bin/sh\n", mode: 0o755},
		{name: bundleDir + "/lib/libggml-cuda.so", body: "ELF"},
		{name: bundleDir + "/lib/libggml-cuda.so.0", link: "libggml-cuda.so"},
	}
}

// linuxBundle skips a test of the engine bundle, which is Linux's (with library symlinks).
func linuxBundle(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the engine bundle is Linux's")
	}
}

// The bundle is unpacked once and reused, with its modes and library symlinks, and without any
// program from PATH: BOINC's wrapper starts the pilot with only job.xml's variables. An archive
// without llama-server, or with a path or link out of the archive, is refused.
func TestUnpack(t *testing.T) {
	linuxBundle(t)
	src, dir := t.TempDir(), t.TempDir()
	archive := filepath.Join(src, "bundle_abc123.tar.xz")
	writeArchive(t, archive, bundleEntries())
	t.Setenv("PATH", "")
	exe, lib, err := Unpack(context.Background(), archive, dir)
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(dir, "bundle_abc123", bundleDir)
	if exe != filepath.Join(want, "bin", "llama-server") || lib != filepath.Join(want, "lib") {
		t.Fatalf("exe %s lib %s", exe, lib)
	}
	if fi, err := os.Stat(exe); err != nil || fi.Mode().Perm() != 0o755 {
		t.Errorf("llama-server: %v %v, want mode 0755", fi, err)
	}
	if target, err := os.Readlink(filepath.Join(lib, "libggml-cuda.so.0")); err != nil || target != "libggml-cuda.so" {
		t.Errorf("library symlink: %q %v", target, err)
	}
	os.Remove(archive) // the second pilot must not need it
	if exe2, _, err := Unpack(context.Background(), archive, dir); err != nil || exe2 != exe {
		t.Fatalf("reuse: %s %v", exe2, err)
	}
	ents, _ := os.ReadDir(dir)
	if len(ents) != 1 {
		t.Errorf("left behind: %v", ents)
	}

	for name, entries := range map[string][]entry{
		"no llama-server":  {{name: "x", body: "x"}},
		"parent path":      append(bundleEntries(), entry{name: "../escaped", body: "x"}),
		"absolute path":    append(bundleEntries(), entry{name: "/tmp/escaped", body: "x"}),
		"absolute symlink": append(bundleEntries(), entry{name: bundleDir + "/lib/evil.so", link: "/etc/passwd"}),
		"symlink out":      append(bundleEntries(), entry{name: bundleDir + "/lib/evil.so", link: "../../../escaped"}),
		"through symlink": append(bundleEntries(), entry{name: "out", link: "."},
			entry{name: "up", link: "out/.."}, entry{name: "up/escaped", body: "x"}),
	} {
		bad := filepath.Join(src, strings.ReplaceAll(name, " ", "-")+".tar.xz")
		writeArchive(t, bad, entries)
		if _, _, err := Unpack(context.Background(), bad, dir); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(dir), "escaped")); err == nil {
		t.Error("an entry was written outside the engine folder")
	}
	if ents, _ := os.ReadDir(dir); len(ents) != 1 {
		t.Errorf("refused archives left behind: %v", ents)
	}
}

// The release's bundle comes from GNU tar and multi-threaded xz -9 (scripts/build-release.sh):
// several xz blocks, owner 0/0, the library symlinks. Unpack reads such an archive too.
func TestUnpackReleaseArchive(t *testing.T) {
	if _, err := exec.LookPath("xz"); err != nil || runtime.GOOS != "linux" {
		t.Skip("needs GNU tar and xz")
	}
	src, dir := t.TempDir(), t.TempDir()
	b := filepath.Join(src, bundleDir)
	os.MkdirAll(filepath.Join(b, "bin"), 0o755)
	os.MkdirAll(filepath.Join(b, "lib"), 0o755)
	os.WriteFile(filepath.Join(b, "bin", "llama-server"), []byte("#!/bin/sh\n"), 0o755)
	os.WriteFile(filepath.Join(b, "lib", "big.so"), bytes.Repeat([]byte("0123456789abcdef"), 1<<16), 0o644)
	os.Symlink("big.so", filepath.Join(b, "lib", "big.so.0"))
	archive := filepath.Join(src, "bundle.tar.xz")
	sh := `tar --sort=name --owner=0 --group=0 --numeric-owner --mode='u+rwX,go+rX,go-w' -C "$1" -cf - "$2" | xz -T2 -9 --block-size=64KiB > "$3"`
	if out, err := exec.Command("sh", "-c", sh, "sh", src, bundleDir, archive).CombinedOutput(); err != nil {
		t.Fatalf("packing: %v %s", err, out)
	}
	exe, lib, err := Unpack(context.Background(), archive, dir)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(filepath.Join(lib, "big.so.0")); err != nil || len(got) != 16<<16 {
		t.Errorf("library through its symlink: %d bytes, %v", len(got), err)
	}
	if fi, err := os.Stat(exe); err != nil || fi.Mode()&0o111 == 0 {
		t.Errorf("llama-server not executable: %v %v", fi, err)
	}
}

// slowReader hands out a few bytes at a time, slowly: an unpack that takes minutes.
type slowReader struct{ f io.ReadCloser }

func (s slowReader) Read(p []byte) (int, error) {
	time.Sleep(20 * time.Millisecond)
	return s.f.Read(p[:min(len(p), 256)])
}
func (s slowReader) Close() error { return s.f.Close() }

// A SIGTERM during a slow first unpack ends the pilot within its stop budget: it writes its
// summary, and leaves no partial unpack behind.
func TestSigtermDuringUnpack(t *testing.T) {
	linuxBundle(t)
	r := newRig(t)
	random := make([]byte, 1<<20) // does not compress: about 80 s at the slow reader's pace
	crand.Read(random)
	archive := filepath.Join(r.slot, "bundle_slow.tar.xz")
	writeArchive(t, archive, append([]entry{{name: bundleDir + "/lib/big.so", body: string(random)}}, bundleEntries()...))
	r.o.LlamaServer, r.o.EngineArchive, r.o.EngineDir = "", archive, filepath.Join(r.slot, "engine")
	orig := openArchive
	openArchive = func(p string) (io.ReadCloser, error) {
		f, err := os.Open(p)
		return slowReader{f}, err
	}
	t.Cleanup(func() { openArchive = orig })
	ctx, sigterm := context.WithCancel(context.Background())
	ch := r.run(ctx)
	deadline := time.Now().Add(5 * time.Second)
	for {
		if m, _ := filepath.Glob(filepath.Join(r.o.EngineDir, ".unpack-*")); len(m) > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the pilot never started unpacking")
		}
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(100 * time.Millisecond)
	start := time.Now()
	sigterm()
	res := wait(t, ch, 10*time.Second)
	if took := time.Since(start); took > 2*time.Second {
		t.Errorf("stop took %s", took)
	}
	if res.Reason != ReasonSigterm || res.Code != 0 {
		t.Errorf("result %+v, want sigterm, 0", res)
	}
	if s := r.summary(); s.Reason != ReasonSigterm {
		t.Errorf("summary %+v", s)
	}
	if ents, _ := os.ReadDir(r.o.EngineDir); len(ents) != 0 {
		t.Errorf("left behind: %v", ents)
	}
}

// A pilot that unpacks its engine needs no PATH (the wrapper gives it only job.xml's variables).
func TestPilotUnpacksWithoutPath(t *testing.T) {
	linuxBundle(t)
	r := newRig(t)
	archive := filepath.Join(r.slot, "bundle_abc123.tar.xz")
	writeArchive(t, archive, bundleEntries())
	r.o.LlamaServer, r.o.EngineArchive, r.o.EngineDir = "", archive, filepath.Join(r.slot, "engine")
	r.o.IdleExit = 500 * time.Millisecond
	t.Setenv("PATH", "")
	ch := r.run(context.Background())
	connected(t, r.gw)
	if res := wait(t, ch, 10*time.Second); res.Reason != ReasonIdle {
		t.Errorf("result %+v; log:\n%s", res, r.logs())
	}
}
