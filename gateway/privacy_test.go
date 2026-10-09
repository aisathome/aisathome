// SPDX-License-Identifier: Apache-2.0

package gateway

import (
	"encoding/json"
	"strings"
	"testing"
)

const fakeKey = "sk-proj-AbCdEfGhIjKlMnOpQrStUvWxYz0123456789"

func TestForNodeStripsIdentity(t *testing.T) {
	body := map[string]any{
		"model": "deep", "temperature": 0.2, "user": "alice@example.com",
		"metadata": map[string]any{"chat_id": "c1"}, "session_id": "s1",
		"messages": []any{
			map[string]any{"role": "user", "content": "hi", "name": "Alice", "id": "m1"},
			map[string]any{"role": "tool", "content": "42", "name": "calc", "tool_call_id": "t1"},
		},
	}
	got := forNode(body)
	b, _ := json.Marshal(got)
	for _, leak := range []string{"alice@example.com", "chat_id", "session_id", "Alice", `"id":"m1"`} {
		if strings.Contains(string(b), leak) {
			t.Fatalf("node body leaks %q: %s", leak, b)
		}
	}
	if got["temperature"] != 0.2 || !strings.Contains(string(b), `"name":"calc"`) {
		t.Fatalf("needed fields dropped: %s", b)
	}
}

func TestRedactAndRestore(t *testing.T) {
	r := newRedactor()
	body := map[string]any{"messages": []any{
		map[string]any{"role": "user", "content": "my key is " + fakeKey + " and again " + fakeKey},
		map[string]any{"role": "assistant", "tool_calls": []any{map[string]any{"function": map[string]any{
			"name": "write", "arguments": `{"text":"OPENAI_API_KEY=` + fakeKey + `"}`}}}},
		map[string]any{"role": "user", "content": []any{map[string]any{"type": "text", "text": "-----BEGIN OPENSSH PRIVATE KEY-----\nabc\n-----END OPENSSH PRIVATE KEY-----"}}},
	}}
	r.redactBody(body)
	b, _ := json.Marshal(body)
	// the armour lines stay (the model should recognise the format), the key material goes
	if strings.Contains(string(b), fakeKey) || strings.Contains(string(b), `\nabc\n`) {
		t.Fatalf("secret left in node body: %s", b)
	}
	if len(r.byPH) != 2 {
		t.Fatalf("want 2 distinct placeholders (same key twice -> one), got %d", len(r.byPH))
	}
	ph := r.byValue[fakeKey]
	if got := r.restore("write " + ph + " to .env"); got != "write "+fakeKey+" to .env" {
		t.Fatalf("restore: %q", got)
	}
}

// Streamed answers: the placeholder arrives split across chunks of any size.
func TestRestorerSplitPlaceholders(t *testing.T) {
	for _, size := range []int{1, 2, 3, 5, 7, 40} {
		r := newRedactor()
		r.redactText(fakeKey)
		ph := r.byValue[fakeKey]
		text := "Set OPENAI_API_KEY=" + ph + " in .env, keep [brackets] and [[other]] text."
		args := `{"path":".env","text":"KEY=` + ph + `"}`
		s := newRestorer(r)
		var gotText, gotArgs strings.Builder
		emit := func(raw []byte) {
			var c struct {
				Choices []struct {
					Delta struct {
						Content   string `json:"content"`
						ToolCalls []struct {
							Function struct{ Arguments string } `json:"function"`
						} `json:"tool_calls"`
					} `json:"delta"`
				} `json:"choices"`
			}
			json.Unmarshal(raw, &c)
			for _, ch := range c.Choices {
				gotText.WriteString(ch.Delta.Content)
				for _, tc := range ch.Delta.ToolCalls {
					gotArgs.WriteString(tc.Function.Arguments)
				}
			}
		}
		for i := 0; i < len(text); i += size {
			piece := text[i:min(i+size, len(text))]
			emit(s.chunk(mustJSON(map[string]any{"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"content": piece}}}})))
		}
		for i := 0; i < len(args); i += size {
			piece := args[i:min(i+size, len(args))]
			emit(s.chunk(mustJSON(map[string]any{"choices": []any{map[string]any{"index": 0, "delta": map[string]any{
				"tool_calls": []any{map[string]any{"index": 0, "function": map[string]any{"arguments": piece}}}}}}})))
		}
		emit(s.chunk(mustJSON(map[string]any{"choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": "stop"}}})))
		if f := s.flush(); f != nil {
			emit(f)
		}
		want := strings.ReplaceAll(text, ph, fakeKey)
		if gotText.String() != want {
			t.Fatalf("size %d: text\n got %q\nwant %q", size, gotText.String(), want)
		}
		if gotArgs.String() != args { // tool arguments pass through with the placeholder
			t.Fatalf("size %d: args %q", size, gotArgs.String())
		}
	}
}

func TestPIIDetection(t *testing.T) {
	msg := func(s string) map[string]any {
		return map[string]any{"messages": []any{map[string]any{"role": "user", "content": s}}}
	}
	// a syntactically valid Norwegian national identity number, constructed for the test
	fnr := ""
	for a := 0; a < 10 && fnr == ""; a++ {
		for b := 0; b < 10; b++ {
			if c := "010180123" + itoa(a) + itoa(b); fodselsnummer(c) {
				fnr = c
				break
			}
		}
	}
	if fnr == "" {
		t.Fatal("could not construct a test identity number")
	}
	for _, c := range []struct {
		text string
		want bool
	}{
		{"mail me at ola.nordmann@example.no", true},
		{"card 4111 1111 1111 1111 please", true},
		{"card 4111 1111 1111 1112 please", false}, // fails Luhn
		{"IBAN NO93 8601 1117 947", true},
		{"number " + fnr, true},
		{"order 12345678901 shipped", fodselsnummer("12345678901")},
		{"plain text with numbers 2026 and 42", false},
	} {
		if got := hasPII(msg(c.text)); got != c.want {
			t.Errorf("%q: got %v want %v", c.text, got, c.want)
		}
	}
}

func itoa(n int) string { b, _ := json.Marshal(n); return string(b) }

// The placeholder must not reveal any of the secret's random part.
func TestPlaceholderKeepsOnlyTypeMarker(t *testing.T) {
	r := newRedactor()
	for _, s := range []string{"sk-proj-Ab_cdefghijklmnopqrstuvwxyz0123", "ghp_" + strings.Repeat("Q", 36), "AKIAZZZZZZZZZZZZZZZZ"} {
		ph := r.placeholder(s)
		rnd := strings.TrimPrefix(s, typePrefix(s))
		if strings.Contains(ph, rnd[:2]) && !strings.HasPrefix(rnd[:2], "X") {
			t.Fatalf("placeholder %q reveals part of %q", ph, s)
		}
		if !strings.HasPrefix(ph, typePrefix(s)) {
			t.Fatalf("placeholder %q lost the type marker of %q", ph, s)
		}
	}
}
