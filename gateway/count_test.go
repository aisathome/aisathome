// SPDX-License-Identifier: Apache-2.0
package gateway

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
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
