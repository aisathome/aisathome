// Answer assembly, secret restoration, and safe error categories.
//
// SPDX-License-Identifier: Apache-2.0
package gateway

import (
	"encoding/json"
	"regexp"
	"strings"
	"time"

	"github.com/aisathome/aisathome/protocol"
)

// restoreAnswer puts redacted secrets back into the text of a non-streaming answer (never
// into tool arguments; see the restore policy in privacy.go).
func restoreAnswer(a map[string]any, red *redactor) map[string]any {
	if !red.restoring() {
		return a
	}
	choices, _ := a["choices"].([]any)
	for _, c := range choices {
		cm, _ := c.(map[string]any)
		msg, _ := cm["message"].(map[string]any)
		if msg == nil {
			continue
		}
		for _, f := range []string{"content", "reasoning_content"} {
			if s, ok := msg[f].(string); ok {
				msg[f] = red.restore(s)
			}
		}
	}
	return a
}

// assemble turns streamed chunks into one non-streaming chat.completion object.
func assemble(id, model string, chunks []json.RawMessage, u protocol.Usage) map[string]any {
	var content, reasoning strings.Builder
	finish := "stop"
	type call struct {
		id, typ, name string
		args          strings.Builder
	}
	var calls []*call // by stream index; arguments arrive in pieces
	for _, c := range chunks {
		var ch struct {
			Choices []struct {
				Delta struct {
					Content          string `json:"content"`
					ReasoningContent string `json:"reasoning_content"`
					ToolCalls        []struct {
						Index    int    `json:"index"`
						ID       string `json:"id"`
						Type     string `json:"type"`
						Function struct {
							Name      string `json:"name"`
							Arguments string `json:"arguments"`
						} `json:"function"`
					} `json:"tool_calls"`
				} `json:"delta"`
				FinishReason *string `json:"finish_reason"`
			} `json:"choices"`
		}
		if json.Unmarshal(c, &ch) != nil {
			continue
		}
		for _, choice := range ch.Choices {
			content.WriteString(choice.Delta.Content)
			reasoning.WriteString(choice.Delta.ReasoningContent)
			for _, tc := range choice.Delta.ToolCalls {
				if tc.Index < 0 || tc.Index > 255 {
					continue
				}
				for len(calls) <= tc.Index {
					calls = append(calls, &call{typ: "function"})
				}
				k := calls[tc.Index]
				if tc.ID != "" {
					k.id = tc.ID
				}
				if tc.Type != "" {
					k.typ = tc.Type
				}
				if tc.Function.Name != "" {
					k.name = tc.Function.Name
				}
				k.args.WriteString(tc.Function.Arguments)
			}
			if choice.FinishReason != nil {
				finish = *choice.FinishReason
			}
		}
	}
	msg := map[string]any{"role": "assistant", "content": content.String()}
	if reasoning.Len() > 0 {
		msg["reasoning_content"] = reasoning.String()
	}
	if len(calls) > 0 {
		var tcs []any
		for _, k := range calls {
			tcs = append(tcs, map[string]any{"id": k.id, "type": k.typ,
				"function": map[string]any{"name": k.name, "arguments": k.args.String()}})
		}
		msg["tool_calls"] = tcs
		if content.Len() == 0 {
			msg["content"] = nil // as OpenAI returns it for pure tool calls
		}
	}
	return map[string]any{
		"id": "chatcmpl-" + id, "object": "chat.completion", "created": time.Now().Unix(), "model": model,
		"choices": []any{map[string]any{"index": 0, "message": msg, "finish_reason": finish}},
		"usage": map[string]any{"prompt_tokens": u.PromptTokens, "completion_tokens": u.CompletionTokens,
			"total_tokens":          u.PromptTokens + u.CompletionTokens,
			"prompt_tokens_details": map[string]any{"cached_tokens": u.CachedTokens}},
	}
}

var engineStatusRe = regexp.MustCompile(`engine returned \d{3}`)

// errCategory reduces an error from a node to a fixed category. Node text is untrusted and
// may quote the prompt (older nodes forward engine response bodies), so it is never logged
// or passed on as is.
func errCategory(msg string) string {
	switch {
	case isContextOverflow(msg):
		return "context_length_exceeded"
	case engineStatusRe.MatchString(msg):
		return engineStatusRe.FindString(msg)
	case strings.Contains(msg, "engine unreachable"):
		return "engine unreachable"
	case strings.Contains(msg, "engine stalled"):
		return "engine stalled"
	case strings.Contains(msg, "ended before the answer was complete"):
		return "engine stream ended early"
	case msg == "node paused", msg == "node disconnected", msg == "node stalled", msg == "client too slow",
		msg == "node sent more output than requested", msg == "context canceled":
		return msg
	}
	return "node error"
}

// isContextOverflow recognises llama-server's context errors: before generation (HTTP 400,
// "exceed_context_size_error") and during it ("Context size has been exceeded").
func isContextOverflow(msg string) bool {
	m := strings.ToLower(msg)
	return strings.Contains(m, "context_length_exceeded") || strings.Contains(m, "exceed_context_size_error") ||
		strings.Contains(m, "exceeds the available context") ||
		strings.Contains(m, "context size has been exceeded")
}

func mustJSON(v any) []byte { b, _ := json.Marshal(v); return b }
