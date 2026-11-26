// SPDX-License-Identifier: Apache-2.0
package gateway

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aisathome/aisathome/protocol"
	"github.com/coder/websocket"
)

// liveCall is a relayed call of task in project as commit hands it to countLive.
func (r *taskRig) liveCall(project, task, outcome string, u protocol.Usage) {
	r.g.countLive(&chatReq{class: ClassScience, project: project, task: task}, &usageRecord{Time: r.now, Outcome: outcome, Usage: u})
}

var liveUsage = protocol.Usage{PromptTokens: 1200, CachedTokens: 1000, CompletionTokens: 80, DurationMs: 2500}

// The live count: only ok calls of a running task of a public project count; a queued, unknown or
// finished task, another project's, a project without a public queue and a failed call do not. The
// public views show it while the task runs and not after; a heartbeat saves it.
func TestTaskLiveCount(t *testing.T) {
	r := v1Rig(t)
	for _, id := range []string{"t6-a", "t6-q"} {
		r.p2m("", map[string]any{"id": id, "kind": "p2m-v7", "created_by": "owner", "role": "hole", "spec": map[string]any{}})
	}
	lease := r.p2m("/claim", map[string]any{"worker": "w1", "task": "t6-a", "lease_seconds": 600})["lease"].(map[string]any)["id"]
	if w, _ := r.call("POST", "/v1/projects/lab/tasks", otherProjectKey, newTask("LAB-1", "p2m-v7")); w.Code != 201 {
		t.Fatalf("lab task: HTTP %d", w.Code)
	}
	if w, _ := r.call("POST", "/v1/projects/lab/tasks/claim", otherProjectKey, map[string]any{"worker": "w9", "task": "LAB-1"}); w.Code != 200 {
		t.Fatalf("lab claim: HTTP %d", w.Code)
	}

	r.liveCall("prove2me", "t6-a", "ok", liveUsage)
	r.now = r.now.Add(time.Minute)
	r.liveCall("prove2me", "t6-a", "ok", protocol.Usage{PromptTokens: 1300, CachedTokens: 5000, CompletionTokens: 20, DurationMs: 500})
	for _, o := range []string{"failed", "cancelled", outcomePreempted, outcomeBlocked} {
		r.liveCall("prove2me", "t6-a", o, liveUsage)
	}
	r.liveCall("prove2me", "t6-q", "ok", liveUsage)   // queued
	r.liveCall("prove2me", "nosuch", "ok", liveUsage) // unknown
	r.liveCall("lab", "LAB-1", "ok", liveUsage)       // running, but lab's queue is not public
	r.liveCall("lab", "t6-a", "ok", liveUsage)        // another project's task
	r.liveCall("prove2me", "", "ok", liveUsage)       // no header

	s := r.g.tasks
	want := taskLive{Requests: 2, PromptTokens: 2500, CachedTokens: 2300, CompletionTokens: 100, ModelMs: 3000, UpdatedAt: r.now}
	if got := s.get("prove2me", "t6-a").Live; got == nil || got.InFlight == nil || *got.InFlight != 0 || withoutInFlight(*got) != want {
		t.Fatalf("live %+v, want %+v", got, want)
	}
	for _, c := range [][2]string{{"prove2me", "t6-q"}, {"lab", "LAB-1"}} {
		if got := s.get(c[0], c[1]).Live; got != nil {
			t.Errorf("%s/%s counted: %+v", c[0], c[1], got)
		}
	}
	// A copy for a response is a copy.
	v := s.get("prove2me", "t6-a")
	v.Live.Requests = 99
	if s.get("prove2me", "t6-a").Live.Requests != 2 {
		t.Fatal("a view shares the live count")
	}

	// GET /v1/tasks: live for the running task, null for the queued one.
	_, body, page := r.v1("?project=prove2me")
	by := map[string]map[string]any{}
	for _, tk := range page.Tasks {
		by[tk["id"].(string)] = tk
	}
	live, _ := by["t6-a"]["live"].(map[string]any)
	if live == nil || live["requests"] != 2.0 || live["prompt_tokens"] != 2500.0 || live["cached_tokens"] != 2300.0 ||
		live["completion_tokens"] != 100.0 || live["model_s"] != 3.0 || live["updated_at"] != "2026-11-15T12:01:00Z" ||
		live["uncached_prompt_tokens"] != 200.0 || live["tokens_computed"] != 300.0 || live["tokens_with_cache"] != 2600.0 ||
		live["in_flight"] != 0.0 || len(live) != 10 {
		t.Fatalf("running task's live: %s", body)
	}
	if l, ok := by["t6-q"]["live"]; !ok || l != nil {
		t.Errorf("queued task's live: %v (present %v)", l, ok)
	}
	// The project's queue: the same fields.
	_, qbody, _ := r.publicQueue()
	if !strings.Contains(qbody, `"live":{"requests":2,"prompt_tokens":2500,"cached_tokens":2300,"completion_tokens":100,"uncached_prompt_tokens":200,"tokens_computed":300,"tokens_with_cache":2600,"model_s":3,"in_flight":0,"updated_at":"2026-11-15T12:01:00Z"}`) ||
		strings.Count(qbody, `"live"`) != 1 {
		t.Fatalf("public queue: %s", qbody)
	}

	// A heartbeat saves it; the end keeps it private and the public views drop it.
	if w := r.p2mLease("t6-a", "heartbeat", "w1", lease, nil); w != 200 {
		t.Fatalf("heartbeat: HTTP %d", w)
	}
	raw, _ := os.ReadFile(filepath.Join(r.g.cfg.StateDir, "tasks-prove2me", "t6-a.json"))
	if !strings.Contains(string(raw), `"live":{"requests":2,`) || strings.Contains(string(raw), "in_flight") {
		t.Fatalf("not saved with the heartbeat: %s", raw)
	}
	if w := r.p2mLease("t6-a", "complete", "w1", lease, map[string]any{"result": okResult}); w != 200 {
		t.Fatalf("complete: HTTP %d", w)
	}
	r.liveCall("prove2me", "t6-a", "ok", liveUsage) // finished: not counted
	if got := s.get("prove2me", "t6-a").Live; got == nil || got.Requests != 2 || got.InFlight != nil {
		t.Fatalf("a finished task's count changed: %+v", got)
	}
	r.freshV1()
	_, body, page = r.v1("?project=prove2me")
	for _, tk := range page.Tasks {
		if tk["live"] != nil {
			t.Errorf("%s: live after its end: %s", tk["id"], body)
		}
	}
}

// withoutInFlight is l without its in-flight count, to compare the rest.
func withoutInFlight(l taskLive) taskLive {
	l.InFlight = nil
	return l
}

func (r *taskRig) p2mLease(task, op, worker string, lease any, extra map[string]any) int {
	r.t.Helper()
	body := map[string]any{"worker": worker, "lease": lease}
	for k, v := range extra {
		body[k] = v
	}
	w, _ := r.call("POST", p2mTasks+"/"+task+"/"+op, testProjectKey, body)
	return w.Code
}

// A claim that continues an interrupted try goes on counting; one that starts a new try starts at 0.
func TestTaskLiveTries(t *testing.T) {
	r := v1Rig(t)
	r.p2m("", map[string]any{"id": "t6-a", "kind": "p2m-v7", "created_by": "owner", "spec": map[string]any{}})
	lease := r.p2m("/claim", map[string]any{"worker": "w1", "task": "t6-a", "lease_seconds": 30, "hold_seconds": 0})["lease"].(map[string]any)["id"]
	r.liveCall("prove2me", "t6-a", "ok", liveUsage)
	if w := r.p2mLease("t6-a", "fail", "w1", lease, map[string]any{"requeue": true, "reason": "spot stop"}); w != 200 {
		t.Fatalf("give back: HTTP %d", w)
	}
	r.liveCall("prove2me", "t6-a", "ok", liveUsage) // queued again: not counted
	r.p2m("/claim", map[string]any{"worker": "w2", "task": "t6-a", "lease_seconds": 30, "hold_seconds": 0})
	r.liveCall("prove2me", "t6-a", "ok", liveUsage)
	if tk := r.g.tasks.get("prove2me", "t6-a"); tk.Tries != 1 || tk.Live == nil || tk.Live.Requests != 2 {
		t.Fatalf("continued try: tries %d, live %+v", tk.Tries, tk.Live)
	}
	// The lease expires and another worker claims it: a new try.
	r.now = r.now.Add(time.Minute)
	r.p2m("/claim", map[string]any{"worker": "w3", "task": "t6-a", "lease_seconds": 30})
	if tk := r.g.tasks.get("prove2me", "t6-a"); tk.Tries != 2 || tk.Live != nil {
		t.Fatalf("new try: tries %d, live %+v", tk.Tries, tk.Live)
	}
}

// Calls counted from many goroutines while the public views are built: every call counts once (and
// -race finds no race).
func TestTaskLiveConcurrent(t *testing.T) {
	r := v1Rig(t)
	r.p2m("", map[string]any{"id": "t6-a", "kind": "p2m-v7", "created_by": "owner", "spec": map[string]any{}})
	r.p2m("/claim", map[string]any{"worker": "w1", "task": "t6-a"})
	const workers, calls = 8, 50
	var wg sync.WaitGroup
	for range workers {
		wg.Add(2)
		go func() {
			defer wg.Done()
			for range calls {
				r.g.countLive(&chatReq{project: "prove2me", task: "t6-a"}, &usageRecord{Time: time.Now(), Outcome: "ok", Usage: liveUsage})
			}
		}()
		go func() {
			defer wg.Done()
			for range 5 {
				if _, err := r.g.tasks.publicTasks([]string{"prove2me"}); err != nil {
					t.Error(err)
				}
				if _, _, err := r.g.tasks.publicList("prove2me"); err != nil {
					t.Error(err)
				}
				_ = r.g.tasks.get("prove2me", "t6-a")
			}
		}()
	}
	wg.Wait()
	l := r.g.tasks.get("prove2me", "t6-a").Live
	if n := int64(workers * calls); l == nil || l.Requests != n || l.PromptTokens != n*1200 || l.ModelMs != n*2500 {
		t.Fatalf("live %+v after %d calls", l, workers*calls)
	}
}

// inFlightOf is a task's in-flight count in the project's own list and in the public queue (-1: no
// live there, -2: no in_flight in it).
func (r *taskRig) inFlightOf(id string) (own, pub int64) {
	r.t.Helper()
	own, pub = -1, -1
	ts, _ := r.g.tasks.list("prove2me", nil, nil, nil, "")
	for _, t := range ts {
		if t.ID == id && t.Live != nil {
			own = -2
			if t.Live.InFlight != nil {
				own = *t.Live.InFlight
			}
		}
	}
	pl, _, err := r.g.tasks.publicList("prove2me")
	if err != nil {
		r.t.Fatal(err)
	}
	for _, t := range pl {
		if t.ID == id && t.Live != nil {
			pub = t.Live.InFlight
		}
	}
	return own, pub
}

// Calls in flight: counted from the request's start to its end, whatever the end, for a running
// task of a public project only; shown while the first call is still in flight, never saved, and
// gone once the task ended.
func TestTaskLiveInFlight(t *testing.T) {
	r := v1Rig(t)
	for _, id := range []string{"t6-a", "t6-q"} {
		r.p2m("", map[string]any{"id": id, "kind": "p2m-v7", "created_by": "owner", "role": "hole", "spec": map[string]any{}})
	}
	lease := r.p2m("/claim", map[string]any{"worker": "w1", "task": "t6-a", "lease_seconds": 600})["lease"].(map[string]any)["id"]
	if w, _ := r.call("POST", "/v1/projects/lab/tasks", otherProjectKey, newTask("LAB-1", "p2m-v7")); w.Code != 201 {
		t.Fatalf("lab task: HTTP %d", w.Code)
	}
	if w, _ := r.call("POST", "/v1/projects/lab/tasks/claim", otherProjectKey, map[string]any{"worker": "w9", "task": "LAB-1"}); w.Code != 200 {
		t.Fatalf("lab claim: HTTP %d", w.Code)
	}
	start := func(project, task string) func() {
		return r.g.liveStart(&chatReq{class: ClassScience, project: project, task: task})
	}
	if own, pub := r.inFlightOf("t6-a"); own != -1 || pub != -1 {
		t.Fatalf("before any call: %d %d", own, pub)
	}

	// The first call in flight: live shows with no call counted and no updated_at.
	end1 := start("prove2me", "t6-a")
	if own, pub := r.inFlightOf("t6-a"); own != 1 || pub != 1 {
		t.Fatalf("first call in flight: own %d, public %d", own, pub)
	}
	_, body, page := r.v1("?project=prove2me")
	var live map[string]any
	for _, tk := range page.Tasks {
		if tk["id"] == "t6-a" {
			live, _ = tk["live"].(map[string]any)
		}
	}
	if _, has := live["updated_at"]; live == nil || live["in_flight"] != 1.0 || live["requests"] != 0.0 || has {
		t.Fatalf("GET /v1/tasks while the first call is in flight: %s", body)
	}
	if got := r.g.tasks.get("prove2me", "t6-a").Live; got == nil || got.InFlight == nil || *got.InFlight != 1 || got.Requests != 0 {
		t.Fatalf("the project's task: %+v", got)
	}
	w, _ := r.call("GET", p2mTasks+"/t6-a", testProjectKey, nil)
	if !strings.Contains(w.Body.String(), `"live":{"requests":0,"prompt_tokens":0,"cached_tokens":0,"completion_tokens":0,"model_ms":0,"in_flight":1}`) {
		t.Fatalf("GET the task: %s", w.Body)
	}

	// A second one beside it; the first ends ok and counts, its end called twice ends it once.
	end2 := start("prove2me", "t6-a")
	r.liveCall("prove2me", "t6-a", "ok", liveUsage)
	end1()
	end1()
	if own, pub := r.inFlightOf("t6-a"); own != 1 || pub != 1 {
		t.Fatalf("one of two ended: own %d, public %d", own, pub)
	}
	// The other fails: not counted, but no longer in flight.
	r.liveCall("prove2me", "t6-a", "failed", liveUsage)
	end2()
	if own, pub := r.inFlightOf("t6-a"); own != 0 || pub != 0 {
		t.Fatalf("none in flight: own %d, public %d", own, pub)
	}
	if got := r.g.tasks.get("prove2me", "t6-a").Live; got.Requests != 1 || !got.UpdatedAt.Equal(r.now) {
		t.Fatalf("live %+v", got)
	}

	// A queued task, an unknown one, a private project's running task and a request without a
	// task: nothing shows; only the public project's names are counted at all.
	for _, c := range [][2]string{{"prove2me", "t6-q"}, {"prove2me", "nosuch"}, {"lab", "LAB-1"}, {"prove2me", ""}} {
		defer start(c[0], c[1])()
	}
	if own, pub := r.inFlightOf("t6-q"); own != -1 || pub != -1 {
		t.Fatalf("queued task: own %d, public %d", own, pub)
	}
	if got := r.g.tasks.get("lab", "LAB-1").Live; got != nil {
		t.Fatalf("private project's task: %+v", got)
	}
	r.g.tasks.liveMu.Lock()
	if n := len(r.g.tasks.inflight); n != 2 {
		t.Errorf("entries %v, want t6-q and nosuch", r.g.tasks.inflight)
	}
	r.g.tasks.liveMu.Unlock()

	// A heartbeat while a call is in flight does not save it; the end hides it.
	end3 := start("prove2me", "t6-a")
	defer end3()
	if w := r.p2mLease("t6-a", "heartbeat", "w1", lease, nil); w != 200 {
		t.Fatalf("heartbeat: HTTP %d", w)
	}
	raw, _ := os.ReadFile(filepath.Join(r.g.cfg.StateDir, "tasks-prove2me", "t6-a.json"))
	if strings.Contains(string(raw), "in_flight") {
		t.Fatalf("saved: %s", raw)
	}
	if w := r.p2mLease("t6-a", "complete", "w1", lease, map[string]any{"result": okResult}); w != 200 {
		t.Fatalf("complete: HTTP %d", w)
	}
	if own, pub := r.inFlightOf("t6-a"); own != -2 || pub != -1 {
		t.Fatalf("finished task: own %d, public %d", own, pub)
	}
}

// A real request through the relay: in flight from its start to its end, also when the node fails
// it, the client goes away or no node serves it; afterwards nothing is left in the map.
func TestTaskLiveInFlightRelayed(t *testing.T) {
	g := newTestGateway(t, Config{StateDir: t.TempDir(), QueueTimeoutSec: 1, CancelAckTimeoutSec: 1,
		NodeTokens: map[string]NodeToken{"node-token": {Operator: "op1", OperatorTerms: OperatorTermsVersion}},
		Projects:   []Project{{ID: "prove2me", KeySHA256: []string{keyHash(testProjectKey)}, PublicQueue: true, Kinds: []string{"p2m-v7"}}}})
	r := &taskRig{t: t, g: g, now: time.Now()}
	defer g.Close()
	srv := httptest.NewServer(g.Handler())
	defer srv.Close()
	r.p2m("", map[string]any{"id": "t6-a", "kind": "p2m-v7", "created_by": "owner", "role": "hole", "spec": map[string]any{}})
	r.p2m("/claim", map[string]any{"worker": "w1", "task": "t6-a", "lease_seconds": 600})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h := http.Header{}
	h.Set("Authorization", "Bearer node-token")
	c, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http")+"/v1/node/connect", &websocket.DialOptions{HTTPHeader: h})
	if err != nil {
		t.Fatal(err)
	}
	write := func(m protocol.Msg) { b, _ := json.Marshal(m); c.Write(ctx, websocket.MessageText, b) }
	write(protocol.Msg{Type: protocol.TypeHello, NodeID: "n1", Protocol: protocol.LegacyVersion,
		Models: []protocol.ModelInfo{{Name: "qwen3.8-27b-q4", Roles: []string{"deep"}, Slots: 1}}})
	reqs := make(chan string, 4) // the requests the node got
	go func() {
		for {
			_, b, err := c.Read(ctx)
			if err != nil {
				return
			}
			var m protocol.Msg
			json.Unmarshal(b, &m)
			switch m.Type {
			case protocol.TypeRequest:
				reqs <- m.ReqID
			case protocol.TypeCancel:
				write(protocol.Msg{Type: protocol.TypeError, ReqID: m.ReqID, Error: "cancelled"})
			}
		}
	}()
	for deadline := time.Now().Add(5 * time.Second); g.NodeCount() != 1; time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the node did not connect")
		}
	}
	chat := func(ctx context.Context, model string) <-chan int {
		done := make(chan int, 1)
		go func() {
			req, _ := http.NewRequestWithContext(ctx, "POST", srv.URL+"/v1/chat/completions", strings.NewReader(`{"model":"`+model+`","messages":[{"role":"user","content":"hi"}]}`))
			req.Header.Set("Authorization", "Bearer "+testProjectKey)
			req.Header.Set(taskHeader, "t6-a")
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				done <- 0
				return
			}
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			done <- resp.StatusCode
		}()
		return done
	}
	wantInFlight := func(what string, n int64) {
		t.Helper()
		for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(10 * time.Millisecond) {
			if own, pub := r.inFlightOf("t6-a"); (own == n || (n == 0 && own == -1)) && (pub == n || (n == 0 && pub == -1)) {
				return
			} else if time.Now().After(deadline) {
				t.Fatalf("%s: in flight own %d, public %d, want %d", what, own, pub, n)
			}
		}
	}
	got := func(what string) string {
		t.Helper()
		select {
		case id := <-reqs:
			return id
		case <-time.After(5 * time.Second):
			t.Fatalf("%s: the node got no request", what)
			return ""
		}
	}

	// Ends ok: counted, no longer in flight.
	done := chat(context.Background(), "deep")
	id := got("ok")
	wantInFlight("first call, before its answer", 1)
	write(protocol.Msg{Type: protocol.TypeChunk, ReqID: id, Data: []byte(`{"choices":[{"index":0,"delta":{"content":"x"}}]}`)})
	write(protocol.Msg{Type: protocol.TypeDone, ReqID: id, Usage: &protocol.Usage{PromptTokens: 5, CompletionTokens: 1}})
	if code := <-done; code != http.StatusOK {
		t.Fatalf("ok: HTTP %d", code)
	}
	wantInFlight("after ok", 0)
	if l := g.tasks.get("prove2me", "t6-a").Live; l == nil || l.Requests != 1 {
		t.Fatalf("live %+v", l)
	}

	// The node fails it (and there is no other node to retry on).
	done = chat(context.Background(), "deep")
	id = got("node error")
	wantInFlight("before the node's error", 1)
	write(protocol.Msg{Type: protocol.TypeError, ReqID: id, Error: "engine crashed"})
	if code := <-done; code == http.StatusOK {
		t.Fatalf("node error: HTTP %d", code)
	}
	wantInFlight("after the node's error", 0)

	// The client goes away.
	cctx, ccancel := context.WithCancel(context.Background())
	done = chat(cctx, "deep")
	got("cancel")
	wantInFlight("before the cancel", 1)
	ccancel()
	<-done
	wantInFlight("after the cancel", 0)

	// No node serves the model: refused at once.
	if code := <-chat(context.Background(), "nobody-serves-this"); code != http.StatusNotFound {
		t.Fatalf("unknown model: HTTP %d", code)
	}
	wantInFlight("after a refusal", 0)
	if l := g.tasks.get("prove2me", "t6-a").Live; l == nil || l.Requests != 1 {
		t.Fatalf("live %+v", l)
	}
	g.tasks.liveMu.Lock()
	defer g.tasks.liveMu.Unlock()
	if len(g.tasks.inflight) != 0 {
		t.Fatalf("left in the map: %v", g.tasks.inflight)
	}
}
