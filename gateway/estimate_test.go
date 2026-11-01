// SPDX-License-Identifier: Apache-2.0
package gateway

import (
	"encoding/json"
	"os"
	"sort"
	"strings"
	"testing"

	"github.com/aisathome/aisathome/protocol"
)

// realPrompts is testdata/real-prompts.json: sizes of real requests, generated from recorded
// sessions, without their text.
type realPrompts struct {
	Counted  [][2]int `json:"counted"` // the gateway's count, the node's prompt_tokens
	Sessions []struct {
		ToolsBytes int `json:"tools_bytes"`
		Rows       []struct {
			Real                                            int  // llama-server's prompt, cache included
			GoBytes                                         int  `json:"go_bytes"`
			System, User, Assistant, Calls, Reasoning, Tool int  // bytes of each field
			Tools                                           bool // it carried tool definitions
			Hidden                                          bool // the model saw none of the reasoning
		} `json:"rows"`
	} `json:"sessions"`
}

func loadRealPrompts(t *testing.T) realPrompts {
	t.Helper()
	raw, err := os.ReadFile("testdata/real-prompts.json")
	if err != nil {
		t.Fatal(err)
	}
	var r realPrompts
	if err := json.Unmarshal(raw, &r); err != nil {
		t.Fatal(err)
	}
	if len(r.Counted) < 700 || len(r.Sessions) != 4 {
		t.Fatalf("a short fixture: %d counts, %d sessions", len(r.Counted), len(r.Sessions))
	}
	return r
}

// With a tokenizer, on real requests of up to 114k tokens: never under what the node counted,
// and within the margin of it.
func TestEstimateNearRealCounts(t *testing.T) {
	for _, c := range loadRealPrompts(t).Counted {
		got, real := promptEstimate(&convPrint{ids: make([]int32, c[0])}), c[1]
		if got < real {
			t.Fatalf("estimate %d under the real %d: a request would overflow its node", got, real)
		}
		if got > real+real/80+64 {
			t.Fatalf("estimate %d, real %d: more than 1.25%% and 64 over", got, real)
		}
	}
}

// fieldBody is a request with a real request's sizes: what the gateway renders of it, though
// not its text. A request whose reasoning the model did not see ends on a user's message.
func fieldBody(toolsBytes, system, user, assistant, calls, reasoning, tool int, tools, hidden bool) map[string]any {
	pad := func(n int) string { return strings.Repeat("x", max(n, 0)) }
	msgs := []any{map[string]any{"role": "system", "content": pad(system)}, map[string]any{"role": "user", "content": pad(user)},
		map[string]any{"role": "assistant", "content": pad(assistant), "reasoning_content": pad(reasoning),
			"tool_calls": []any{map[string]any{"id": "c", "type": "function",
				"function": map[string]any{"name": "t", "arguments": `{"a":"` + pad(calls) + `"}`}}}},
		map[string]any{"role": "tool", "tool_call_id": "c", "content": pad(tool)}}
	if hidden {
		msgs = append(msgs, map[string]any{"role": "user", "content": "go on"})
	}
	body := map[string]any{"messages": msgs}
	if tools {
		body["tools"] = []any{map[string]any{"type": "function",
			"function": map[string]any{"name": "t", "description": pad(toolsBytes)}}}
	}
	return body
}

// Without a tokenizer, on real sessions: never under the real prompt, and the reasoning a
// summary request carries no longer counts. Each of these requests got a 400 from bytes/3 of
// the JSON on a node of 98,304, on the one request that must not fail.
func TestEstimateRealSessions(t *testing.T) {
	const ctx = 98304
	g := newTestGateway(t, Config{})
	defer g.Close()
	var ratios []float64
	hidden, wasRefused := 0, 0
	for _, s := range loadRealPrompts(t).Sessions {
		for _, r := range s.Rows {
			cp := g.pcache.print(forNode(fieldBody(s.ToolsBytes, r.System, r.User, r.Assistant, r.Calls, r.Reasoning, r.Tool, r.Tools, r.Hidden)))
			got := promptEstimate(cp)
			if got < r.Real {
				t.Fatalf("estimate %d under the real %d: a request would overflow its node", got, r.Real)
			}
			ratios = append(ratios, float64(got)/float64(r.Real))
			if !r.Hidden {
				continue
			}
			hidden++
			if float64(got)/float64(r.Real) > 1.3 {
				t.Fatalf("estimate %d, real %d: hidden reasoning still counts", got, r.Real)
			}
			if r.GoBytes/3+16+minOutputRoom > ctx {
				wasRefused++
				rt := route{}
				rt.setPrompt(cp, 16000)
				if !fits(protocol.ModelInfo{Ctx: ctx}, rt.needMin) {
					t.Fatalf("real %d is refused on %d: needs %d", r.Real, ctx, rt.needMin)
				}
			}
		}
	}
	if hidden < 9 || wasRefused == 0 {
		t.Fatalf("the fixture lost its point: %d hidden, %d refused before", hidden, wasRefused)
	}
	sort.Float64s(ratios)
	median, top := ratios[len(ratios)/2], ratios[len(ratios)-1]
	t.Logf("estimate x real: lowest %.2f, median %.2f, highest %.2f; %d refused before", ratios[0], median, top, wasRefused)
	if median > 1.25 || top > 1.45 {
		t.Fatalf("estimate x real: median %.2f, highest %.2f", median, top)
	}
}

// The route counts the rendered prompt with the tokenizer, once: credit gets the same tokens.
func TestEstimateCountsWithTokenizer(t *testing.T) {
	g := newTestGateway(t, Config{TokenizerFile: "testdata/qwen38-mini-tokenizer.json"})
	defer g.Close()
	c := &chatCaller{acct: KeyInfo{User: "alice"}, userKey: "alice", class: ClassChat, charge: []string{"alice"}}
	job := prepared(t, g, c, strings.Repeat(" the", 65000), map[string]any{"max_tokens": 32000})
	ids := g.tok.Encode(job.req.conv.prompt)
	if len(ids) < 65000 || len(ids) > 65300 {
		t.Fatalf("%d tokens: the prompt is not what this test means", len(ids))
	}
	want := len(ids) + len(ids)/100 + 16
	if job.rt.need != want+32000 || job.rt.needMin != want+minOutputRoom {
		t.Fatalf("need %d, needMin %d; want %d and room", job.rt.need, job.rt.needMin, want)
	}
	m := newMeter(nil, job.req.conv, g.tok)
	<-m.ready
	if len(m.promptIDs) != len(ids) || &m.promptIDs[0] != &job.req.conv.ids[0] {
		t.Fatalf("credit counted the prompt again, or differently: %d tokens", len(m.promptIDs))
	}
}
