// SPDX-License-Identifier: Apache-2.0
package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/aisathome/aisathome/protocol"
)

func TestEngineErrorKind(t *testing.T) {
	for chunk, want := range map[string]string{
		`{"choices":[{"index":0,"delta":{"content":"an \"error\" in the text"}}]}`:                                       "",
		`{"choices":[{"index":0,"delta":{"content":"x"}}]}`:                                                              "",
		`{"error":{"code":400,"message":"Context size has been exceeded.","type":"exceed_context_size_error"}}`:          "context_length_exceeded",
		`{"error":{"code":500,"message":"Failed to parse input at pos 17: <tool_call>{\"name\"","type":"server_error"}}`: "tool_call_parse",
		`{"error":{"code":500,"message":"something else","type":"server_error"}}`:                                        "engine_error",
		`{"error":null}`:   "",
		`not json "error"`: "",
	} {
		if got := engineErrorKind([]byte(chunk)); got != want {
			t.Errorf("%s: %q, want %q", chunk, got, want)
		}
	}
}

// The counts are per UTC day, node and model; they survive a save and a load, old days go, and a day
// holds at most failuresMaxRows rows (the rest under "(other)").
func TestFailureCountsKeepAndBound(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 11, 28, 23, 59, 0, 0, time.UTC)
	f := newFailureCounts()
	f.path = filepath.Join(dir, FailuresFile)
	f.add("n-a", "qwen3.8-27b-q3", 81920, now, kindOK, kindBadToolCalls)
	f.add("n-a", "qwen3.8-27b-q3", 81920, now, kindFailedPrefix+"context_length_exceeded")
	f.add("n-a", "qwen3.8-27b-q3", 81920, now.Add(2*time.Minute), kindOK) // the next UTC day
	f.add("n-old", "qwen3.8-27b", 196608, now.AddDate(0, 0, -failuresKeepDays-1), kindOK)
	for i := 0; i < failuresMaxRows+5; i++ {
		f.add(fmt.Sprintf("n-%04d", i), "qwen3.8-27b", 196608, now, kindOK)
	}
	if err := f.save(); err != nil {
		t.Fatal(err)
	}
	g := newFailureCounts()
	g.path = f.path
	g.load(slog.New(slog.NewTextHandler(io.Discard, nil)), now)
	days := g.snapshotLocked().Days
	if len(days) != 2 || days[0].Day != "2026-11-28" || days[1].Day != "2026-11-29" {
		t.Fatalf("days %+v", days)
	}
	var a, other *FailureRow
	for _, r := range days[0].Rows {
		switch r.Node {
		case "n-a":
			a = r
		case failuresOther:
			other = r
		case "n-old":
			t.Fatal("a day past failuresKeepDays was kept")
		}
	}
	if len(days[0].Rows) != failuresMaxRows+1 || a == nil || a.Ctx != 81920 || a.Kinds[kindOK] != 1 ||
		a.Kinds[kindBadToolCalls] != 1 || a.Kinds["failed:context_length_exceeded"] != 1 {
		t.Fatalf("%d rows, n-a %+v", len(days[0].Rows), a)
	}
	if other == nil || other.Kinds[kindOK] != 6 { // n-a took a row before the 1000 others filled the day
		t.Fatalf("other %+v", other)
	}
	var nilCounts *failureCounts // a gateway part that is off
	nilCounts.add("n", "m", 1, now, kindOK)
	if nilCounts.save() != nil {
		t.Fatal("nil save")
	}
}

// A 16 GB node's attempts end in each way the counts tell apart; a request too long for any node is
// refused before one. The counts reach the state dir, name no prompt, error text or user, and the CLI
// reads them (cmd/aisathome-gateway).
func TestFailureCountsFromRequests(t *testing.T) {
	dir := t.TempDir()
	g := newTestGateway(t, Config{StateDir: dir, DefaultMaxTokens: 100, MaxMaxTokens: 1000,
		Keys:       map[string]KeyInfo{"user-key": {User: "alice"}},
		NodeTokens: map[string]NodeToken{"node-token": {Operator: "op1", OperatorTerms: OperatorTermsVersion}}})
	closed := false
	defer func() {
		if !closed {
			g.Close()
		}
	}()
	srv := httptest.NewServer(g.Handler())
	defer srv.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h := http.Header{}
	h.Set("Authorization", "Bearer node-token")
	c, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http")+"/v1/node/connect", &websocket.DialOptions{HTTPHeader: h})
	if err != nil {
		t.Fatal(err)
	}
	write := func(m protocol.Msg) { b, _ := json.Marshal(m); c.Write(ctx, websocket.MessageText, b) }
	write(protocol.Msg{Type: protocol.TypeHello, NodeID: "n-small", Protocol: protocol.LegacyVersion,
		Models: []protocol.ModelInfo{{Name: "qwen3.8-27b-q3", Roles: []string{"deep"}, Slots: 1, Ctx: 81920}}})
	chunk := func(id, data string) { write(protocol.Msg{Type: protocol.TypeChunk, ReqID: id, Data: []byte(data)}) }
	text := `{"choices":[{"index":0,"delta":{"content":"PROMPT-ANSWER-TEXT"}}]}`
	finish := func(r string) string {
		return `{"choices":[{"index":0,"delta":{},"finish_reason":"` + r + `"}]}`
	}
	done := func(id string, completion int) {
		write(protocol.Msg{Type: protocol.TypeDone, ReqID: id, Usage: &protocol.Usage{PromptTokens: 5, CompletionTokens: completion}})
	}
	// What the node does with each request, by the word the user sent.
	go func() {
		for {
			_, b, err := c.Read(ctx)
			if err != nil {
				return
			}
			var m protocol.Msg
			json.Unmarshal(b, &m)
			if m.Type != protocol.TypeRequest {
				continue
			}
			var body struct {
				Messages []struct {
					Content string `json:"content"`
				} `json:"messages"`
			}
			json.Unmarshal(m.Body, &body)
			id := m.ReqID
			switch body.Messages[0].Content {
			case "ok":
				chunk(id, text)
				chunk(id, finish("stop"))
				done(id, 1)
			case "maxtokens": // the request's max_tokens (100)
				chunk(id, text)
				chunk(id, finish("length"))
				done(id, 100)
			case "fullctx": // the node's context ran out first
				chunk(id, text)
				chunk(id, finish("length"))
				done(id, 7)
			case "overflow": // before any output
				write(protocol.Msg{Type: protocol.TypeError, ReqID: id, Error: "context_length_exceeded: the request exceeds this node's context"})
			case "parse": // llama-server's error event in the stream, then the stream ends
				chunk(id, text)
				chunk(id, `{"error":{"code":500,"message":"Failed to parse input at pos 3: SECRET-MODEL-OUTPUT","type":"server_error"}}`)
				write(protocol.Msg{Type: protocol.TypeError, ReqID: id, Error: "engine stream ended before the answer was complete"})
			case "badcall":
				chunk(id, `{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"c1","type":"function","function":{"name":"f","arguments":"not json"}}]}}]}`)
				chunk(id, finish("tool_calls"))
				done(id, 3)
			case "crash":
				write(protocol.Msg{Type: protocol.TypeError, ReqID: id, Error: "engine returned 500"})
			}
		}
	}()
	waitFor(t, func() bool { g.mu.Lock(); defer g.mu.Unlock(); return len(g.node) == 1 })
	ask := func(content string, maxTokens int) int {
		t.Helper()
		body, _ := json.Marshal(map[string]any{"model": "deep", "max_tokens": maxTokens,
			"messages": []any{map[string]any{"role": "user", "content": content}}})
		req, _ := http.NewRequest("POST", srv.URL+"/v1/chat/completions", strings.NewReader(string(body)))
		req.Header.Set("Authorization", "Bearer user-key")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		return resp.StatusCode
	}
	for _, w := range []string{"ok", "ok", "maxtokens", "fullctx", "overflow", "parse", "badcall", "crash"} {
		ask(w, 100)
	}
	if code := ask(strings.Repeat("too long ", 60000), 100); code != http.StatusBadRequest { // ~120k tokens > 81,920
		t.Fatalf("too long: HTTP %d", code)
	}

	g.fails.mu.Lock()
	st := g.fails.snapshotLocked()
	g.fails.mu.Unlock()
	if len(st.Days) != 1 {
		t.Fatalf("days %+v", st.Days)
	}
	rows := map[string]*FailureRow{}
	for _, r := range st.Days[0].Rows {
		rows[r.Node] = r
	}
	small := rows["n-small"]
	want := map[string]int{kindOK: 3, kindLengthMax: 1, kindLengthContext: 1, kindBadToolCalls: 1,
		"failed:context_length_exceeded": 1, "failed:tool_call_parse": 1, "failed:engine returned 500": 1}
	if small == nil || small.Model != "qwen3.8-27b-q3" || small.Ctx != 81920 || fmt.Sprint(small.Kinds) != fmt.Sprint(want) {
		t.Fatalf("n-small %+v, want kinds %v", small, want)
	}
	if r := rows[failuresNoNode]; r == nil || r.Model != "deep" || r.Kinds["refused:too_long"] != 1 {
		t.Fatalf("refusals %+v", r)
	}

	closed = true
	if err := g.Close(); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, FailuresFile))
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{"PROMPT", "SECRET", "too long", "alice", "user-key", "op1", "Failed to parse", "exceeds this node"} {
		if strings.Contains(string(raw), s) {
			t.Fatalf("the counts name %q: %s", s, raw)
		}
	}
	if days, err := ReadFailures(filepath.Join(dir, FailuresFile)); err != nil || len(days) != 1 || len(days[0].Rows) != 2 {
		t.Fatalf("read back %v %+v", err, days)
	}
}

// attemptKinds tells the client's leaving, preemption, a guard stop and the gateway's own busy state
// apart from the node's failures.
func TestAttemptKinds(t *testing.T) {
	gone, cancel := context.WithCancel(context.Background())
	cancel()
	live := context.Background()
	for _, c := range []struct {
		ctx  context.Context
		err  error
		want string
	}{
		{live, errPreempted, kindPreempted},
		{live, &guardError{}, kindBlocked},
		{live, errGatewayBusy, kindGatewayBusy},
		{live, errClientSlow, kindCancelled},
		{gone, errors.New("node disconnected"), kindCancelled},
		{live, errors.New("node disconnected"), "failed:node disconnected"},
		{live, errors.New("node stalled"), "failed:node stalled"},
		{live, errors.New("free text from a node: PROMPT"), "failed:node error"},
	} {
		rl := &relay{ctx: c.ctx}
		if got := rl.attemptKinds(c.err); len(got) != 1 || got[0] != c.want {
			t.Errorf("%v: %v, want %s", c.err, got, c.want)
		}
	}
}
