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
		got, _ := renderPrompt(body)
		if got != c.Prompt {
			t.Errorf("case %d:\n--- got\n%s\n--- want\n%s", i, got, c.Prompt)
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
