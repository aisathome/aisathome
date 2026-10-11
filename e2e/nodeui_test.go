// Pause and resume from the node's local UI, through the real gateway: a paused node gets
// no new requests, finishes those in flight, and the UI never shows request content.
//
// SPDX-License-Identifier: Apache-2.0
package e2e

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aisathome/aisathome/node"
	"github.com/aisathome/aisathome/node/localui"
	"github.com/aisathome/aisathome/protocol"
)

// counted wraps an engine and counts the completion requests that reach it.
func counted(t *testing.T, inner *httptest.Server) (*httptest.Server, *atomic.Int32) {
	var hits atomic.Int32
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/health" {
			hits.Add(1)
		}
		inner.Config.Handler.ServeHTTP(w, r)
	}))
	t.Cleanup(proxy.Close)
	return proxy, &hits
}

// startAgent runs a node agent and returns it, so the test can pause it.
func (r *rig) startAgent(ctx context.Context, id string, engine *httptest.Server, slots int, state string) *node.Agent {
	cfg := node.Config{NodeID: id, Token: "node-token", Upstream: engine.URL, StateFile: state,
		GatewayURL:     "ws" + strings.TrimPrefix(r.gw.URL, "http") + "/v1/node/connect",
		HealthInterval: 50 * time.Millisecond,
		Models:         []protocol.ModelInfo{{Name: "qwen3.8-27b-q4", Roles: []string{"deep"}, Slots: slots, Ctx: 32768}}}
	a := node.New(cfg, quiet())
	go a.Run(ctx)
	return a
}

// waitPaused waits until the gateway counts n paused nodes (public stats).
func waitPaused(t *testing.T, r *rig, n int) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		s, _ := getStats(t, r.gw.URL)
		if s["nodes"].(map[string]any)["paused"].(float64) == float64(n) {
			return
		}
	}
	t.Fatalf("the gateway never counted %d paused node(s)", n)
}

func chat(t *testing.T, r *rig, body string) (int, string) {
	t.Helper()
	resp := r.do(t, "POST", "/v1/chat/completions", "user-key", body)
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// A paused node gets no new requests while another node serves; after resume it does again.
func TestPausedNodeGetsNoRequests(t *testing.T) {
	r := newRigCfg(t, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	engA, hitsA := counted(t, mockEngine(t, false))
	engB, hitsB := counted(t, mockEngine(t, false))
	a := r.startAgent(ctx, "node-a", engA, 2, "")
	r.startAgent(ctx, "node-b", engB, 2, "")
	r.waitNodes(t, 2)

	a.Pause()
	waitPaused(t, r, 1)
	for i := 0; i < 6; i++ {
		if code, b := chat(t, r, chatBody); code != 200 {
			t.Fatalf("request %d while node-a is paused: %d %s", i, code, b)
		}
	}
	if hitsA.Load() != 0 || hitsB.Load() != 6 {
		t.Fatalf("paused node-a served %d, node-b %d", hitsA.Load(), hitsB.Load())
	}
	if st := a.Status(); st.State != node.StatePaused || !st.Connected {
		t.Fatalf("node-a status: %+v", st)
	}

	a.Resume()
	waitPaused(t, r, 0)
	for i := 0; i < 6; i++ {
		chat(t, r, chatBody)
	}
	if hitsA.Load() == 0 {
		t.Fatal("node-a got no requests after resume")
	}
}

// With its only node paused, a request waits (as for a busy node) instead of failing, and is
// served once the node resumes.
func TestPausedOnlyNodeQueuesUntilResume(t *testing.T) {
	r := newRigCfg(t, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	eng, hits := counted(t, mockEngine(t, false))
	a := r.startAgent(ctx, "solo", eng, 2, "")
	r.waitNodes(t, 1)
	a.Pause()
	waitPaused(t, r, 1)

	type result struct {
		code int
		body string
	}
	done := make(chan result, 1)
	go func() { c, b := chat(t, r, chatBody); done <- result{c, b} }()
	select {
	case res := <-done:
		t.Fatalf("request finished while the only node was paused: %d %s", res.code, res.body)
	case <-time.After(400 * time.Millisecond):
	}
	if hits.Load() != 0 {
		t.Fatal("the paused node's engine got a request")
	}
	a.Resume()
	select {
	case res := <-done:
		if res.code != 200 || !strings.Contains(res.body, "[DONE]") {
			t.Fatalf("after resume: %d %s", res.code, res.body)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the waiting request was not served after resume")
	}
}

// Pausing does not cut a request in flight: it streams to the end.
func TestPauseFinishesRequestsInFlight(t *testing.T) {
	r := newRigCfg(t, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a := r.startAgent(ctx, "solo", slowEngine(t, 10, 30*time.Millisecond), 1, "")
	r.waitNodes(t, 1)

	resp := r.do(t, "POST", "/v1/chat/completions", "user-key", chatBody)
	defer resp.Body.Close()
	sc := bufio.NewScanner(resp.Body)
	chunks, paused, sawDone := 0, false, false
	for sc.Scan() {
		line := strings.TrimPrefix(sc.Text(), "data: ")
		if line == "[DONE]" {
			sawDone = true
			break
		}
		if strings.Contains(line, `"content":"t`) {
			chunks++
			if !paused {
				a.Pause() // after the first token
				paused = true
				if st := a.Status(); st.State != node.StatePaused || st.SlotsInUse != 1 {
					t.Errorf("paused with one in flight: %+v", st)
				}
			}
		}
	}
	if resp.StatusCode != 200 || chunks != 10 || !sawDone {
		t.Fatalf("in-flight request cut by pause: status %d, %d of 10 chunks, done=%v", resp.StatusCode, chunks, sawDone)
	}
	waitPaused(t, r, 1)
	for i := 0; i < 100 && a.Status().Today.Requests != 1; i++ {
		time.Sleep(10 * time.Millisecond)
	}
	if st := a.Status(); st.Today.Requests != 1 || st.Today.CompletionTokens != 10 || st.SlotsInUse != 0 {
		t.Fatalf("counters after the request: %+v", st)
	}
}

// The local UI drives pause and resume end to end, keeps them across an agent restart, and
// its status (counters, events) never contains prompts, answers or engine error bodies.
func TestLocalUIEndToEnd(t *testing.T) {
	const secret = "SECRET-PROMPT-4f9c"
	r := newRigCfg(t, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// An engine that answers the first request and fails the second with a body that quotes
	// the prompt (llama-server does this for some errors).
	var n atomic.Int32
	ok := mockEngine(t, false)
	eng := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if health(w, req) {
			return
		}
		if n.Add(1) == 2 {
			b, _ := io.ReadAll(req.Body)
			http.Error(w, "error near: "+string(b), http.StatusInternalServerError)
			return
		}
		ok.Config.Handler.ServeHTTP(w, req)
	}))
	defer eng.Close()
	dir := t.TempDir()
	state := filepath.Join(dir, "node-state.json")
	pwFile := filepath.Join(dir, "ui-password")
	os.WriteFile(pwFile, []byte("local-ui-password-123\n"), 0o640)

	actx, stopAgent := context.WithCancel(ctx)
	a := r.startAgent(actx, "ui-node", eng, 2, state)
	ui := httptest.NewServer(localui.New(a, localui.Options{PasswordFile: pwFile, NvidiaSMI: "/nonexistent"}).Handler())
	defer ui.Close()
	r.waitNodes(t, 1)

	body := `{"model":"deep","messages":[{"role":"user","content":"` + secret + `"}]}`
	if code, b := chat(t, r, body); code != 200 || !strings.Contains(b, "Hello, world") {
		t.Fatalf("first request: %d %s", code, b)
	}
	if code, _ := chat(t, r, body); code == 200 {
		t.Fatal("second request should fail")
	}

	post := func(path, body string, cookie *http.Cookie) *http.Response {
		req, _ := http.NewRequest("POST", ui.URL+path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Origin", ui.URL)
		req.Header.Set("Sec-Fetch-Site", "same-origin")
		if cookie != nil {
			req.AddCookie(cookie)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp
	}
	status := func() (localui.StatusResponse, string) {
		resp, err := http.Get(ui.URL + "/api/status")
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		var st localui.StatusResponse
		json.Unmarshal(b, &st)
		return st, string(b)
	}

	if resp := post("/api/pause", "{}", nil); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("pause without password: %d", resp.StatusCode)
	}
	login := post("/api/login", `{"password":"local-ui-password-123"}`, nil)
	var cookie *http.Cookie
	for _, c := range login.Cookies() {
		cookie = c
	}
	if login.StatusCode != 200 || cookie == nil {
		t.Fatalf("login: %d", login.StatusCode)
	}
	if resp := post("/api/pause", "{}", cookie); resp.StatusCode != 200 {
		t.Fatalf("pause: %d", resp.StatusCode)
	}
	waitPaused(t, r, 1)

	st, raw := status()
	if st.State != node.StatePaused || st.Today.Requests != 1 || st.Today.Failed != 1 || st.Today.PromptTokens != 12 ||
		st.GPU.Note == "" || st.NodeID != "ui-node" {
		t.Fatalf("status: %s", raw)
	}
	for _, leak := range []string{secret, "Hello", "world", "error near", "node-token"} {
		if strings.Contains(raw, leak) {
			t.Fatalf("the local UI status contains %q: %s", leak, raw)
		}
	}
	kinds := map[string]bool{}
	for _, e := range st.Events {
		kinds[e.Kind] = true
	}
	for _, k := range []string{"started", "connected", "request_failed", "paused"} {
		if !kinds[k] {
			t.Errorf("no %q event: %s", k, raw)
		}
	}
	if b, _ := os.ReadFile(state); strings.Contains(string(b), secret) || strings.Contains(string(b), "error near") {
		t.Fatalf("the state file holds request content: %s", b)
	}

	// The agent restarts (e.g. with the engine): it comes back paused, and the gateway knows.
	stopAgent()
	r.waitNodes(t, 0)
	a2 := r.startAgent(ctx, "ui-node", eng, 2, state)
	r.waitNodes(t, 1)
	waitPaused(t, r, 1)
	if st := a2.Status(); !st.Paused || st.Total.Requests != 1 {
		t.Fatalf("after restart: %+v", st)
	}
	a2.Resume()
	waitPaused(t, r, 0)
}
