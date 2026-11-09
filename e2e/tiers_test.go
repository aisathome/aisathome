// Two tiers in one network (owner, 2026-11-09): 24 GB nodes serve qwen3.8-27b (UD-IQ4_XS) and
// 16 GB nodes qwen3.8-27b-q3 (UD-Q3_K_XL), both for the roles "deep" and "agent". A session stays
// on the model it started on, and answers name the model that answered.
//
// SPDX-License-Identifier: Apache-2.0
package e2e

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/aisathome/aisathome/gateway"
	"github.com/aisathome/aisathome/node"
	"github.com/aisathome/aisathome/protocol"
)

var tierModel = map[string]string{"big": "qwen3.8-27b", "std": "qwen3.8-27b-q3"}

// tierTurn sends s's conversation with one more user message for role "agent" and returns the
// answering engine, the answer's model and the HTTP status (the session goes on only on 200).
func (s *agentSession) tierTurn(t *testing.T, r *rig, user string) (engine, model string, code int, body string) {
	t.Helper()
	msgs := append(append([]msg{}, s.msgs...), msg{"user", user})
	b, _ := json.Marshal(map[string]any{"model": "agent", "messages": msgs})
	resp := r.do(t, "POST", "/v1/chat/completions", s.key, string(b))
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var ans struct {
		Model   string
		Choices []struct{ Message msg }
	}
	if resp.StatusCode != 200 || json.Unmarshal(raw, &ans) != nil || len(ans.Choices) == 0 {
		return "", "", resp.StatusCode, string(raw)
	}
	s.msgs = append(msgs, msg{"assistant", ans.Choices[0].Message.Content})
	return strings.TrimPrefix(ans.Choices[0].Message.Content, "answered by "), ans.Model, 200, ""
}

func TestTiersSessionStaysOnItsModel(t *testing.T) {
	big, std := newCacheEngine(t, "big"), newCacheEngine(t, "std")
	r := newRigCfg(t, func(c *gateway.Config) { c.QueueTimeoutSec = 3 })
	ctxs := map[string]context.CancelFunc{}
	for _, e := range []*cacheEngine{big, std} {
		ctx, cancel := context.WithCancel(context.Background())
		t.Cleanup(cancel)
		ctxs[e.name] = cancel
		r.startNodeCfg(ctx, node.Config{NodeID: "node-" + e.name, Token: "node-token", Upstream: e.URL,
			Models: []protocol.ModelInfo{{Name: tierModel[e.name], Roles: []string{"deep", "agent"}, Slots: 1, Ctx: 32768}}})
	}
	r.waitNodes(t, 2)

	s := newSession("user-key", "the parser")
	home, model, code, body := s.tierTurn(t, r, toolResult(0))
	if code != 200 || model != tierModel[home] {
		t.Fatalf("first turn: HTTP %d %s, answered by %q as model %q", code, body, home, model)
	}
	other := map[string]string{"big": "std", "std": "big"}[home]
	for i := 1; i <= 3; i++ {
		if got, m, code, body := s.tierTurn(t, r, toolResult(i)); got != home || m != tierModel[home] {
			t.Fatalf("turn %d: HTTP %d %s, answered by %q (%q); the session is on %s", i, code, body, got, m, home)
		}
	}

	// Its model's node busy (a parallel request of the same session holds it): the next turn
	// waits for it instead of moving to the free node of the other model.
	eng := map[string]*cacheEngine{"big": big, "std": std}[home]
	par := &agentSession{key: s.key, msgs: append([]msg{}, s.msgs...)}
	done := make(chan string, 1)
	go func() {
		got, err := par.try(t, r, "HOLD")
		if err != nil {
			t.Error(err)
		}
		done <- got
	}()
	select {
	case <-eng.held:
	case <-time.After(5 * time.Second):
		t.Fatal("the parallel request did not reach the session's node")
	}
	go func() { time.Sleep(500 * time.Millisecond); close(eng.hold) }()
	t0 := time.Now()
	if got, _, code, body := s.tierTurn(t, r, toolResult(4)); got != home || time.Since(t0) < 400*time.Millisecond {
		t.Fatalf("with its node busy the turn went to %q after %v (HTTP %d %s)", got, time.Since(t0), code, body)
	}
	if got := <-done; got != home {
		t.Fatalf("the parallel request was answered by %s", got)
	}

	// Its model's node gone: the turn fails after the wait, with the reason, and the other model
	// never sees the conversation. A new session is served by the other node.
	ctxs[home]()
	r.waitNodes(t, 1)
	t0 = time.Now()
	if got, _, code, body := s.tierTurn(t, r, toolResult(5)); code != 503 ||
		!strings.Contains(body, `this conversation started on \"`+tierModel[home]+`\"`) || time.Since(t0) < 2*time.Second {
		t.Fatalf("with its model offline: HTTP %d after %v, answered by %q: %s", code, time.Since(t0), got, body)
	}
	n := newSession("user-key", "the linker")
	if got, m, code, body := n.tierTurn(t, r, toolResult(0)); got != other || m != tierModel[other] {
		t.Fatalf("a new session: HTTP %d %s, answered by %q (%q)", code, body, got, m)
	}
	for _, rec := range r.usage(t) {
		if rec["node"] == "node-"+other && rec["requested"] == "agent" && rec["model"] != tierModel[other] {
			t.Fatalf("usage record: %v", rec)
		}
	}
}

// A streamed answer names the model that answered in every chunk, also when the engine names
// none (the client asked for a role).
func TestTiersStreamNamesModel(t *testing.T) {
	std := newCacheEngine(t, "std")
	r := newRigCfg(t, nil)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	r.startNodeCfg(ctx, node.Config{NodeID: "node-std", Token: "node-token", Upstream: std.URL,
		Models: []protocol.ModelInfo{{Name: tierModel["std"], Roles: []string{"deep", "agent"}, Slots: 1, Ctx: 32768}}})
	r.waitNodes(t, 1)
	resp := r.do(t, "POST", "/v1/chat/completions", "user-key",
		`{"model":"deep","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	defer resp.Body.Close()
	chunks := 0
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		data, ok := strings.CutPrefix(sc.Text(), "data: ")
		if !ok || data == "[DONE]" {
			continue
		}
		var c struct{ Model string }
		if json.Unmarshal([]byte(data), &c) != nil || c.Model != tierModel["std"] {
			t.Fatalf("chunk names model %q, want %q: %s", c.Model, tierModel["std"], data)
		}
		chunks++
	}
	if resp.StatusCode != 200 || chunks < 2 {
		t.Fatalf("HTTP %d, %d chunks", resp.StatusCode, chunks)
	}
}
