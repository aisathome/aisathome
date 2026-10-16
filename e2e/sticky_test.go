// Sticky, cache-aware routing (v0.2.0 ticket B): a conversation goes back to the node that
// holds its prefix while that node is free, because each node's llama.cpp keeps its own
// prompt cache.
//
// SPDX-License-Identifier: Apache-2.0
package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aisathome/aisathome/gateway"
)

// cacheEngine imitates llama-server's prompt cache: it remembers the last few conversations
// it served (prompt and answer) and reports the longest common prefix with one of them as
// cached (timings.cache_n), at four characters a token. Its answer names the engine. A last
// user message "HOLD" blocks until hold is closed (held is signalled first).
type cacheEngine struct {
	*httptest.Server
	name       string
	keep       int // conversations kept: 1 = the slot alone, more = llama.cpp's RAM cache
	mu         sync.Mutex
	seen       []string
	hold, held chan struct{}
}

func newCacheEngine(t *testing.T, name string) *cacheEngine { return newCacheEngineKeep(t, name, 4) }

func newCacheEngineKeep(t *testing.T, name string, keep int) *cacheEngine {
	e := &cacheEngine{name: name, keep: keep, hold: make(chan struct{}), held: make(chan struct{}, 4)}
	e.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if health(w, r) {
			return
		}
		var body struct {
			Messages []struct{ Role, Content string }
		}
		json.NewDecoder(r.Body).Decode(&body)
		var b strings.Builder
		for _, m := range body.Messages {
			b.WriteString(m.Role + ": " + m.Content + "\n")
		}
		prompt := b.String()
		if n := len(body.Messages); n > 0 && body.Messages[n-1].Content == "HOLD" {
			e.held <- struct{}{}
			select {
			case <-e.hold:
			case <-r.Context().Done():
				return
			}
		}
		answer := "answered by " + name
		e.mu.Lock()
		cached := 0
		for _, s := range e.seen {
			k := 0
			for k < len(s) && k < len(prompt) && s[k] == prompt[k] {
				k++
			}
			cached = max(cached, k)
		}
		e.seen = append(e.seen, prompt+"assistant: "+answer+"\n")
		if len(e.seen) > e.keep {
			e.seen = e.seen[1:]
		}
		e.mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":%q},\"finish_reason\":null}]}\n\n", answer)
		fmt.Fprint(w, "data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n")
		fmt.Fprintf(w, "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":%d,\"completion_tokens\":3},\"timings\":{\"cache_n\":%d,\"prompt_n\":%d}}\n\n",
			len(prompt)/4, cached/4, (len(prompt)-cached)/4)
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	t.Cleanup(e.Close)
	return e
}

type msg struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// agentSession is one conversation: a long system prompt and turns that each add a tool
// result of a few KB, as a coding agent's do.
type agentSession struct {
	key  string
	msgs []msg
}

func newSession(key, topic string) *agentSession {
	return &agentSession{key: key, msgs: []msg{{"system", strings.Repeat("You are a coding agent working on "+topic+". ", 200)}}}
}

// turn sends the conversation with one more user message and returns which engine answered.
func (s *agentSession) turn(t *testing.T, r *rig, user string) string {
	t.Helper()
	got, err := s.try(t, r, user)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

// try is turn for other goroutines than the test's.
func (s *agentSession) try(t *testing.T, r *rig, user string) (string, error) {
	msgs := append(append([]msg{}, s.msgs...), msg{"user", user})
	b, _ := json.Marshal(map[string]any{"model": "deep", "messages": msgs})
	resp := r.do(t, "POST", "/v1/chat/completions", s.key, string(b))
	defer resp.Body.Close()
	var ans struct {
		Choices []struct{ Message msg }
	}
	if err := json.NewDecoder(resp.Body).Decode(&ans); err != nil || resp.StatusCode != 200 || len(ans.Choices) == 0 {
		return "", fmt.Errorf("answer: %d %v", resp.StatusCode, err)
	}
	s.msgs = append(msgs, msg{"assistant", ans.Choices[0].Message.Content})
	return strings.TrimPrefix(ans.Choices[0].Message.Content, "answered by "), nil
}

func toolResult(i int) string {
	return fmt.Sprintf("tool result %d: %s", i, strings.Repeat("output line\n", 200))
}

// stickyRig starts a gateway with one node (slots 1) per cacheEngine, named node-<engine>.
func stickyRig(t *testing.T, mod func(*gateway.Config), engines ...*cacheEngine) *rig {
	r := newRigCfg(t, mod)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	for _, e := range engines {
		r.startNode(ctx, "node-"+e.name, "node-token", e.Server, 1)
	}
	r.waitNodes(t, len(engines))
	return r
}

// With two free nodes, every turn of a conversation lands on the node that served the
// first, and that node reports the conversation as cached.
func TestStickyConversation(t *testing.T) {
	a, b := newCacheEngine(t, "a"), newCacheEngine(t, "b")
	r := stickyRig(t, nil, a, b)
	s := newSession("user-key", "the parser")
	home := s.turn(t, r, toolResult(0))
	for i := 1; i <= 6; i++ {
		if got := s.turn(t, r, toolResult(i)); got != home {
			t.Fatalf("turn %d went to %s; the conversation is on %s", i, got, home)
		}
	}
	u := waitUsage(t, r, 7)
	for i, rec := range u[1:] {
		us := rec["usage"].(map[string]any)
		if c, p := us["cached_tokens"].(float64), us["prompt_tokens"].(float64); c < p*3/4 {
			t.Fatalf("turn %d: %v of %v prompt tokens cached", i+1, c, p)
		}
	}
}

// When the node holding the conversation is busy, the next turn goes to the other free node
// at once rather than waiting; another requester's identical conversation and the same
// requester's other conversation are not drawn to a node by it.
func TestStickyBusyAndSeparate(t *testing.T) {
	a, b := newCacheEngine(t, "a"), newCacheEngine(t, "b")
	r := stickyRig(t, func(c *gateway.Config) { c.QueueTimeoutSec = 20 }, a, b)
	s := newSession("user-key", "the parser")
	home := s.turn(t, r, toolResult(0))
	s.turn(t, r, toolResult(1))
	eng := map[string]*cacheEngine{"a": a, "b": b}[home]

	// A parallel request of the same conversation (a subagent, say) is sticky too and holds
	// home's one slot.
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
		t.Fatal("the parallel request did not reach the conversation's node")
	}
	t0 := time.Now()
	away := s.turn(t, r, toolResult(2))
	if away == home || time.Since(t0) > 3*time.Second {
		t.Fatalf("with %s busy the turn went to %s after %v", home, away, time.Since(t0))
	}
	close(eng.hold)
	if got := <-done; got != home {
		t.Fatalf("the parallel request was answered by %s", got)
	}
	// The conversation now follows its longest copy, on the other node.
	if got := s.turn(t, r, toolResult(3)); got != away {
		t.Fatalf("turn 3 went to %s; %s holds the longer prefix", got, away)
	}

	// Another conversation of the same requester starts by round-robin, and stays where it
	// started while both are interleaved.
	o := newSession("user-key", "the linker")
	other := o.turn(t, r, toolResult(0))
	for i := 1; i <= 2; i++ {
		if got := s.turn(t, r, toolResult(3+i)); got != away {
			t.Fatalf("first conversation moved to %s", got)
		}
		if got := o.turn(t, r, toolResult(i)); got != other {
			t.Fatalf("second conversation moved from %s to %s", other, got)
		}
	}
	// Round-robin points at the other node now (sticky picks do not move it): another
	// requester sending the very same conversation goes there, not where the conversation is.
	c := &agentSession{key: "agent-key", msgs: append([]msg{}, o.msgs...)}
	if got := c.turn(t, r, toolResult(9)); got == other {
		t.Fatalf("another requester's copy of the conversation went to its node %s", got)
	}
}

// Placement policy comes first: a turn that carries personal data goes to the trusted node,
// although the volunteer node holds the conversation and is free.
func TestStickyPolicyFirst(t *testing.T) {
	vol, home := newCacheEngine(t, "vol"), newCacheEngine(t, "home")
	r := newRigCfg(t, func(c *gateway.Config) {
		c.NodeTokens["home-token"] = gateway.NodeToken{Operator: "owner", Trust: "trusted"}
	})
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	r.startNode(ctx, "node-vol", "node-token", vol.Server, 1)
	r.waitNodes(t, 1)
	s := newSession("user-key", "the parser")
	for i := 0; i < 2; i++ {
		if got := s.turn(t, r, toolResult(i)); got != "vol" {
			t.Fatalf("turn %d: %s", i, got)
		}
	}
	r.startNode(ctx, "node-home", "home-token", home.Server, 1)
	r.waitNodes(t, 2)
	if got := s.turn(t, r, "write to jane.doe@example.com about it"); got != "home" {
		t.Fatalf("a turn with personal data went to %s", got)
	}
}

// Measurement: one agent session, and two at once, on two one-slot nodes, as the engines
// report their cache: with the slot's cache alone, and with a RAM cache of four
// conversations (the generous case for switching nodes, if the earlier copy still fits). Logged
// for the ticket; run with -v. Each turn adds a fresh tool result, so even a perfect cache
// re-reads about 15% of the prompt tokens.
func TestStickyCacheShare(t *testing.T) {
	const turns = 10
	for _, keep := range []int{1, 4} {
		for _, sessions := range [][]string{{"user-key"}, {"user-key", "agent-key"}} {
			name := fmt.Sprintf("keep %d, %d sessions", keep, len(sessions))
			t.Run(name, func(t *testing.T) {
				a, b := newCacheEngineKeep(t, "a", keep), newCacheEngineKeep(t, "b", keep)
				r := stickyRig(t, nil, a, b)
				var wg sync.WaitGroup
				for i, key := range sessions {
					wg.Add(1)
					go func() {
						defer wg.Done()
						s := newSession(key, fmt.Sprintf("project %d", i))
						for k := 0; k < turns; k++ {
							if _, err := s.try(t, r, toolResult(k)); err != nil {
								t.Error(err)
								return
							}
						}
					}()
				}
				wg.Wait()
				var cached, prompt float64
				for _, rec := range waitUsage(t, r, turns*len(sessions)) {
					us := rec["usage"].(map[string]any)
					cached += us["cached_tokens"].(float64)
					prompt += us["prompt_tokens"].(float64)
				}
				share := cached / prompt
				t.Logf("%s: %d turns each, %.0f of %.0f prompt tokens cached (%.0f%%), %.0f re-read",
					name, turns, cached, prompt, 100*share, prompt-cached)
				if share < 0.8 {
					t.Fatalf("cache share %.2f", share)
				}
			})
		}
	}
}

// The conversation is remembered for its node, and the node's slot freed, before the client
// gets [DONE]: a next turn sent at that moment lands on the same node, with one slot per node
// (as all real nodes run) and with two. The client's connection runs the next turn from inside
// the write of [DONE], so the order is fixed, not a race.
func TestStickyNextTurnOnDone(t *testing.T) {
	for _, slots := range []int{1, 2} {
		t.Run(fmt.Sprintf("%d slots", slots), func(t *testing.T) {
			a, b := newCacheEngine(t, "a"), newCacheEngine(t, "b")
			r := newRigCfg(t, nil)
			ctx, cancel := context.WithCancel(context.Background())
			t.Cleanup(cancel)
			for _, e := range []*cacheEngine{a, b} {
				r.startNode(ctx, "node-"+e.name, "node-token", e.Server, slots)
			}
			r.waitNodes(t, 2)
			nextTurnOnDone(t, r)
		})
	}
}

func nextTurnOnDone(t *testing.T, r *rig) {
	s := newSession("user-key", "the parser")
	first := append(append([]msg{}, s.msgs...), msg{"user", toolResult(0)})
	var seen strings.Builder
	home, next := "", ""
	h := cutClient(t, r.g.Handler(), false, func(p []byte) bool {
		seen.Write(p)
		if !strings.Contains(string(p), "[DONE]") {
			return false
		}
		for _, line := range strings.Split(seen.String(), "\n") {
			var c struct {
				Choices []struct{ Delta struct{ Content string } }
			}
			if json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &c) == nil && len(c.Choices) > 0 {
				home += c.Choices[0].Delta.Content
			}
		}
		home = strings.TrimPrefix(home, "answered by ")
		s.msgs = append(first, msg{"assistant", "answered by " + home})
		got, err := s.try(t, r, toolResult(1))
		if err != nil {
			t.Error(err)
		}
		next = got
		return false
	})
	body, _ := json.Marshal(map[string]any{"model": "deep", "stream": true, "messages": first})
	req, _ := http.NewRequest("POST", h.URL+"/v1/chat/completions", strings.NewReader(string(body)))
	req.Header.Set("Authorization", "Bearer user-key")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if home == "" || next != home {
		t.Fatalf("the first turn was answered by %q, the turn sent on its [DONE] by %q", home, next)
	}
}

// The share cap counts conversations, not requests (owner, 2026-10-18): an agent session of
// many turns on one volunteer's node stays there, while other requesters' new conversations
// still spread under the cap, the session's node getting its share of them. Counting every
// request would put that operator over a third and move the session off its cache.
func TestStickySessionShareCap(t *testing.T) {
	ops := []string{"a", "b", "c"}
	engines := map[string]*cacheEngine{}
	r := newRigCfg(t, func(c *gateway.Config) {
		c.APIKeys["bob-key"], c.APIKeys["carol-key"] = "bob", "carol"
		for _, op := range ops {
			c.NodeTokens["tok-"+op] = gateway.NodeToken{Operator: "op-" + op} // three untrusted volunteers
		}
	})
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	for _, op := range ops {
		engines[op] = newCacheEngine(t, op)
		r.startNode(ctx, "node-"+op, "tok-"+op, engines[op].Server, 1)
	}
	r.waitNodes(t, len(ops))
	s := newSession("user-key", "the parser")
	home := s.turn(t, r, "step 0") // short turns: 30 of them fit the test nodes' context
	convs := map[string]int{home: 1}
	for i := 1; i <= 30; i++ {
		if got := s.turn(t, r, fmt.Sprintf("step %d", i)); got != home {
			t.Fatalf("turn %d of the session went to %s; it is on %s (conversations so far %v)", i, got, home, convs)
		}
		if i%3 == 0 {
			for _, key := range []string{"bob-key", "carol-key"} {
				convs[newSession(key, fmt.Sprintf("%s %d", key, i)).turn(t, r, "step 0")]++
			}
		}
	}
	// 21 conversations: 7 each, give or take one.
	for _, op := range ops {
		if c := convs[op]; c < 6 || c > 8 {
			t.Fatalf("conversations per node %v (the session's node is %s)", convs, home)
		}
	}
}
