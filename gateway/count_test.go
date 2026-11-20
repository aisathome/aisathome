// SPDX-License-Identifier: Apache-2.0
package gateway

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/aisathome/aisathome/protocol"
)

// The Go rendering of the chat template must equal jinja2's (scripts/template-fixture.py).
func TestRenderPromptMatchesTemplate(t *testing.T) {
	raw, err := os.ReadFile("testdata/qwen38-template-fixture.json")
	if err != nil {
		t.Fatal(err)
	}
	var f struct {
		Cases []struct {
			Body   map[string]any `json:"body"`
			Prompt string         `json:"prompt"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(raw, &f); err != nil || len(f.Cases) < 8 {
		t.Fatalf("fixture: %v", err)
	}
	for i, c := range f.Cases {
		// As the node gets it: through encoding/json (sorted keys, no HTML escaping issues).
		var body map[string]any
		json.Unmarshal(mustJSON(c.Body), &body)
		// jinja2 renders without llama-server's kwargs: preserve_thinking undefined, which the
		// template treats as true.
		kw, _ := body["chat_template_kwargs"].(map[string]any)
		if _, set := kw["preserve_thinking"]; !set {
			if kw == nil {
				kw = map[string]any{}
				body["chat_template_kwargs"] = kw
			}
			kw["preserve_thinking"] = true
		}
		got, _ := renderPrompt(body)
		if got != c.Prompt {
			t.Errorf("case %d:\n--- got\n%s\n--- want\n%s", i, got, c.Prompt)
		}
	}
}

// The prompt is counted as the node's llama-server renders it. The installer starts it with
// --no-reasoning-preserve, which in llama.cpp v0.5.0 sets the template's preserve_thinking to
// false unless the request sets chat_template_kwargs.preserve_thinking (used as given: only
// true preserves) or llama.cpp's own preserve_reasoning (a boolean replaces the server's).
func TestPromptFollowsEngineReasoningDefault(t *testing.T) {
	const earlier = "EARLIER-REASONING"
	for _, c := range []struct {
		kwargs   string
		preserve bool
	}{
		{``, false},
		{`"chat_template_kwargs":{"enable_thinking":true},`, false},
		{`"chat_template_kwargs":{"preserve_thinking":true},`, true},
		{`"chat_template_kwargs":{"preserve_thinking":false},`, false},
		{`"chat_template_kwargs":{"preserve_thinking":"yes"},`, false}, // "is true" needs a boolean
		{`"chat_template_kwargs":{"preserve_thinking":null},`, false},
		{`"chat_template_kwargs":{"preserve_reasoning":true},`, true},
		{`"chat_template_kwargs":{"preserve_reasoning":false},`, false},
		{`"chat_template_kwargs":{"preserve_reasoning":"x"},`, true}, // replaces the server's; not applied: template default
		{`"chat_template_kwargs":{"preserve_reasoning":true,"preserve_thinking":false},`, false},
	} {
		var body map[string]any
		raw := `{` + c.kwargs + `"messages":[{"role":"user","content":"first"},{"role":"assistant","content":"one","reasoning_content":"` + earlier + `"},{"role":"user","content":"second"}]}`
		if err := json.Unmarshal([]byte(raw), &body); err != nil {
			t.Fatal(err)
		}
		got, _ := renderPrompt(body)
		if strings.Contains(got, earlier) != c.preserve {
			t.Errorf("%s: earlier reasoning in the prompt = %v, want %v", c.kwargs, !c.preserve, c.preserve)
		}
		if !strings.Contains(got, "<|im_start|>assistant\none<|im_end|>") && !c.preserve {
			t.Errorf("%s: dropped reasoning must leave the bare answer: %q", c.kwargs, got)
		}
	}
}

func TestMeterOutput(t *testing.T) {
	m := &meter{args: templateArgs{thinking: true}}
	for _, c := range []string{
		`{"choices":[{"delta":{"reasoning_content":"Let me think."}}]}`,
		`{"choices":[{"delta":{"content":"Sure. "}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"name":"bash","arguments":"{\"cmd\":"}}]}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":" \"ls\"}"}}]}}]}`,
		`not json`,
		`{"choices":[{"delta":{"tool_calls":[{"index":999,"function":{"name":"x"}}]}}]}`,
	} {
		m.chunk([]byte(c))
	}
	gen, next := m.output()
	want := "Let me think.\n</think>\n\nSure. \n\n<tool_call>\n<function=bash>\n<parameter=cmd>\nls\n</parameter>\n</function>\n</tool_call>"
	if gen != want {
		t.Fatalf("output:\n%q\nwant\n%q", gen, want)
	}
	if !strings.HasPrefix(next, "Let me think.\n</think>\n\nSure.\n\n<tool_call>") || !strings.HasSuffix(next, "</tool_call><|im_end|>\n") {
		t.Fatalf("next-turn form: %q", next)
	}
	// Without thinking the answer is all there is.
	m = &meter{}
	m.chunk([]byte(`{"choices":[{"delta":{"content":"hi"}}]}`))
	if gen, _ := m.output(); gen != "hi" {
		t.Fatalf("no thinking: %q", gen)
	}
	// The kept text is bounded.
	m = &meter{}
	big := `{"choices":[{"delta":{"content":"` + strings.Repeat("x", 64<<10) + `"}}]}`
	for i := 0; i < 64; i++ {
		m.chunk([]byte(big))
	}
	if m.content.Len() > maxMetered+len(big) {
		t.Fatalf("kept %d bytes", m.content.Len())
	}
	// Logprobs do not count towards the bound: only the text kept does.
	m = &meter{}
	lp := `,"logprobs":{"content":[{"token":"x","logprob":-0.1,"top_logprobs":[` +
		strings.TrimSuffix(strings.Repeat(`{"token":"y","logprob":-2.5,"bytes":[121]},`, 20), ",") + `]}]}`
	for i := 0; i < 2000; i++ { // 2000 tokens with top_logprobs 20: about 2 MB of chunks
		m.chunk([]byte(`{"choices":[{"delta":{"content":"x"}` + lp + `}]}`))
	}
	if m.content.Len() != 2000 {
		t.Fatalf("with logprobs, %d of 2000 tokens metered", m.content.Len())
	}
}

func TestPyJSON(t *testing.T) {
	got := pyJSON(map[string]any{"b": []any{1.0, "x, y: z"}, "a": map[string]any{"<": "&"}})
	if want := `{"a": {"<": "&"}, "b": [1, "x, y: z"]}`; got != want {
		t.Fatalf("got %s, want %s", got, want)
	}
}

// The thinking limits change what the node generates, not the prompt it renders: the
// gateway's count of the prompt (and the conversation's hash for routing) stays the same.
// The forced budget message is generated output and streams as reasoning, so it is metered
// like the rest.
func TestReasoningBudgetLeavesPromptAlone(t *testing.T) {
	msgs := []any{map[string]any{"role": "user", "content": "hi"}}
	plain, _ := renderPrompt(map[string]any{"messages": msgs})
	with, _ := renderPrompt(map[string]any{"messages": msgs, "thinking_budget_tokens": 4096.0,
		"reasoning_budget_message": "I act now."})
	if plain != with {
		t.Fatalf("budget fields changed the prompt:\n%q\n%q", plain, with)
	}
}

// A request that ends with an assistant message has the engine continue that message
// (llama-server's prefill_assistant). llama.cpp b11146 streams the message back first, as if
// generated (opencode's step-limit notice: one token sampled, 166 counted); b12342 does not.
// Either way only what the engine generated after it is output.
func TestPrefillIsNotOutput(t *testing.T) {
	g := newTestGateway(t, Config{TokenizerFile: "testdata/qwen38-mini-tokenizer.json"})
	defer g.Close()
	const notice = "CRITICAL - MAXIMUM STEPS REACHED\n\nThe maximum number of steps allowed for this task has been reached. Tools are disabled until next user input. Respond with text only."
	history := `{"role":"user","content":"Read notes.txt."},` +
		`{"role":"assistant","content":"","tool_calls":[{"id":"c1","type":"function","function":{"name":"read","arguments":"{\"path\":\"notes.txt\"}"}}]},` +
		`{"role":"tool","tool_call_id":"c1","content":"The notes say: buy milk."}`
	delta := func(field, s string) string {
		return `{"choices":[{"delta":{` + mustJSONString(field) + `:` + mustJSONString(s) + `}}]}`
	}
	var credit float64
	var reasoning int
	count := func(last, outcome string, chunks ...string) int {
		t.Helper()
		var body map[string]any
		if err := json.Unmarshal([]byte(`{"tools":[{"type":"function","function":{"name":"read"}}],"messages":[`+history+last+`]}`), &body); err != nil {
			t.Fatal(err)
		}
		m := newMeter(body, nil, g.tok)
		<-m.ready
		sent := &delivered{}
		for _, c := range chunks {
			m.chunk([]byte(c))
			sent.chunk([]byte(c))
		}
		rec := &usageRecord{Outcome: outcome, Tokens: &creditTokens{Prompt: 100}}
		g.countOutput(rec, m, sent, protocol.Usage{}, 4096)
		credit, reasoning = rec.Credit, rec.facts.reasoning
		return rec.Tokens.Completion
	}
	asked := `,{"role":"assistant","content":` + mustJSONString(notice) + `}`
	// Only the end of the turn was sampled, streamed back (b11146) or not (b12342).
	if got := count(asked, "ok", delta("content", notice)); got != 1 {
		t.Errorf("the notice streamed back: %d tokens, want 1 (the end of the turn)", got)
	}
	if got := count(asked, "ok"); got != 1 {
		t.Errorf("the notice not streamed back: %d tokens, want 1", got)
	}
	// The engine went on after the prefill: the first chunk holds the prefill and the first
	// token. No end of thinking either: the prompt holds it.
	started := `,{"role":"assistant","content":"The notes say to buy"}`
	want := g.tok.Count(" milk.") + 1
	if got := count(started, "ok", delta("content", "The notes say to buy milk"), delta("content", ".")); got != want {
		t.Errorf("continued, streamed back: %d tokens, want %d", got, want)
	}
	if got := count(started, "ok", delta("content", " milk"), delta("content", ".")); got != want {
		t.Errorf("continued, not streamed back: %d tokens, want %d", got, want)
	}
	if got := count(started, "cancelled", delta("content", "The notes say to buy milk")); got != g.tok.Count(" milk") {
		t.Errorf("cancelled: %d tokens, want %d", got, g.tok.Count(" milk"))
	}
	// A client that got only the prefill back got nothing: no credit, not even for the prompt.
	if got := count(started, "cancelled", delta("content", "The notes say to buy")); got != 0 || credit != 0 {
		t.Errorf("cancelled after the prefill only: %d tokens, credit %v; want none", got, credit)
	}
	if count(started, "cancelled", delta("content", "The notes say to buy milk")); credit == 0 {
		t.Error("cancelled after a generated token: no credit")
	}
	// Text parts may come back joined by blank lines.
	parts := `,{"role":"assistant","content":[{"type":"text","text":"The notes"},{"type":"text","text":"say to buy"}]}`
	if got := count(parts, "ok", delta("content", "The notes\n\nsay to buy milk.")); got != want {
		t.Errorf("text parts streamed back: %d tokens, want %d", got, want)
	}
	// Reasoning alone is continued as reasoning: its end of thinking is generated.
	thought := `,{"role":"assistant","content":"","reasoning_content":"The user wants a summary."}`
	want = g.tok.Count(" Short.\n</think>\n\nMilk.") + 1
	if got := count(thought, "ok", delta("reasoning_content", "The user wants a summary. Short."), delta("content", "Milk.")); got != want {
		t.Errorf("reasoning continued: %d tokens, want %d", got, want)
	}
	if reasoning != g.tok.Count(" Short.") { // the reasoning budget check counts only what was generated
		t.Errorf("reasoning continued: %d reasoning tokens, want %d", reasoning, g.tok.Count(" Short."))
	}
	// A new answer counts as before.
	want = g.tok.Count("Fine.\n</think>\n\nMilk.") + 1
	if got := count("", "ok", delta("reasoning_content", "Fine."), delta("content", "Milk.")); got != want {
		t.Errorf("a new answer: %d tokens, want %d", got, want)
	}
}

func mustJSONString(s string) string { return string(mustJSON(s)) }
