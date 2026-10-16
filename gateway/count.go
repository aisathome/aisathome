// Token counts by the gateway (credit, decision 0007): the prompt is the request the gateway
// forwarded, rendered with the model's chat template (Qwen3.8's chat_template.jinja, rewritten
// here in Go); the output is the text, reasoning and tool calls the gateway relayed. Nodes
// report their own counts too; with a tokenizer configured those are only recorded for
// comparison, never used for credit.
//
// The rendering follows the template closely but is not a Jinja engine: tool definitions are
// written with sorted keys (as the node receives them from the gateway), and images count as
// their three marker tokens. Small differences cost a token or two per message.
//
// SPDX-License-Identifier: Apache-2.0
package gateway

import (
	"bytes"
	"encoding/json"
	"strings"
)

const (
	effortXHigh = "Reasoning effort is set to xhigh. Please think carefully through the task, validate key assumptions, consider plausible alternatives, and prioritize correctness, consistency, and clarity in the final answer."
	effortLow   = "Reasoning effort is set to low. Keep your thinking brief and focused, moving directly to the conclusion without unnecessary elaboration."
	toolsHead   = "# Tools\n\nYou have access to the following functions:\n\n<tools>"
	toolsTail   = "\n\nIf you choose to call a function ONLY reply in the following format with NO suffix:\n\n<tool_call>\n<function=example_function_name>\n<parameter=example_parameter_1>\nvalue_1\n</parameter>\n<parameter=example_parameter_2>\nThis is the value for the second parameter\nthat can span\nmultiple lines\n</parameter>\n</function>\n</tool_call>\n\n<IMPORTANT>\nReminder:\n- Function calls MUST follow the specified format: an inner <function=...></function> block must be nested within <tool_call></tool_call> XML tags\n- Required parameters MUST be specified\n- You may provide optional reasoning for your function call in natural language BEFORE the function call, but NOT after\n- If there is no function call available, answer the question like normal with your current knowledge and do not tell the user about function calls\n</IMPORTANT>"
)

// templateArgs are the chat template's switches a request may set (chat_template_kwargs, or
// reasoning_effort at the top level).
type templateArgs struct {
	thinking, preserve bool
	effort             string
}

// argsOf reads the switches as the node's llama-server applies them. Earlier turns'
// reasoning: the installer starts llama-server with --no-reasoning-preserve,
// which in llama.cpp v0.5.0 sets the server's preserve_reasoning kwarg to false; a request's
// chat_template_kwargs replace the server's per key. A boolean preserve_reasoning becomes the
// template's preserve_thinking; a preserve_thinking in the request is passed as given and wins
// (the template preserves only on exactly true, or when it is undefined).
func argsOf(body map[string]any) templateArgs {
	a := templateArgs{thinking: true, preserve: false, effort: "xhigh"}
	if e, ok := body["reasoning_effort"].(string); ok {
		a.effort = e
	}
	if kw, ok := body["chat_template_kwargs"].(map[string]any); ok {
		if v, ok := kw["enable_thinking"].(bool); ok {
			a.thinking = v
		}
		if v, set := kw["preserve_thinking"]; set {
			a.preserve = v == true
		} else if v, set := kw["preserve_reasoning"]; set {
			b, isBool := v.(bool)
			a.preserve = b || !isBool // not a boolean: not applied, preserve_thinking stays undefined
		}
		if e, ok := kw["reasoning_effort"].(string); ok {
			a.effort = e
		}
	}
	return a
}

// renderPrompt renders the messages and tools of body as the node's engine does, with the
// generation prompt for the assistant's answer.
func renderPrompt(body map[string]any) (string, templateArgs) {
	a := argsOf(body)
	var b strings.Builder
	instr := ""
	if a.thinking {
		switch a.effort {
		case "low":
			instr = effortLow
		case "medium":
		default:
			instr = effortXHigh
		}
	}
	msgs, _ := body["messages"].([]any)
	msg := func(i int) map[string]any { m, _ := msgs[i].(map[string]any); return m }
	role := func(i int) string { r, _ := msg(i)["role"].(string); return r }
	firstSystem := len(msgs) > 0 && role(0) == "system"
	sysContent := ""
	if firstSystem {
		sysContent = strings.TrimSpace(renderContent(msg(0)["content"]))
	}
	if tools, _ := body["tools"].([]any); len(tools) > 0 {
		b.WriteString("<|im_start|>system\n")
		if instr != "" {
			b.WriteString(instr + "\n\n")
		}
		b.WriteString(toolsHead)
		for _, t := range tools {
			b.WriteString("\n" + pyJSON(t))
		}
		b.WriteString("\n</tools>" + toolsTail)
		if sysContent != "" {
			b.WriteString("\n\n" + sysContent)
		}
		b.WriteString("<|im_end|>\n")
	} else if sysContent != "" {
		b.WriteString("<|im_start|>system\n")
		if instr != "" {
			b.WriteString(instr + "\n\n")
		}
		b.WriteString(sysContent + "<|im_end|>\n")
	} else if instr != "" {
		b.WriteString("<|im_start|>system\n" + instr + "<|im_end|>\n")
	}
	lastQuery := len(msgs) - 1
	for i := len(msgs) - 1; i >= 0; i-- {
		if role(i) == "user" {
			c := strings.TrimSpace(renderContent(msg(i)["content"]))
			if !(strings.HasPrefix(c, "<tool_response>") && strings.HasSuffix(c, "</tool_response>")) {
				lastQuery = i
				break
			}
		}
	}
	for i := range msgs {
		m := msg(i)
		content := strings.TrimSpace(renderContent(m["content"]))
		switch role(i) {
		case "user":
			b.WriteString("<|im_start|>user\n" + content + "<|im_end|>\n")
		case "assistant":
			reasoning, _ := m["reasoning_content"].(string)
			if a.preserve || i > lastQuery {
				b.WriteString("<|im_start|>assistant\n<think>\n" + strings.TrimSpace(reasoning) + "\n</think>\n\n" + content)
			} else {
				b.WriteString("<|im_start|>assistant\n" + content)
			}
			calls, _ := m["tool_calls"].([]any)
			var cs []toolCall
			for _, c := range calls {
				cm, _ := c.(map[string]any)
				if f, ok := cm["function"].(map[string]any); ok {
					cm = f
				}
				name, _ := cm["name"].(string)
				args, _ := cm["arguments"].(string)
				if args == "" {
					if o, ok := cm["arguments"].(map[string]any); ok {
						args = string(mustJSON(o))
					}
				}
				cs = append(cs, toolCall{name: name, args: args})
			}
			b.WriteString(renderCalls(content, cs))
			b.WriteString("<|im_end|>\n")
		case "tool":
			if i > 0 && role(i-1) != "tool" {
				b.WriteString("<|im_start|>user")
			}
			b.WriteString("\n<tool_response>\n" + content + "\n</tool_response>")
			if i == len(msgs)-1 || role(i+1) != "tool" {
				b.WriteString("<|im_end|>\n")
			}
		}
	}
	b.WriteString("<|im_start|>assistant\n")
	if a.thinking {
		b.WriteString("<think>\n")
	} else {
		b.WriteString("<think>\n\n</think>\n\n")
	}
	return b.String(), a
}

// renderContent is the template's render_content: a string, or the text of content parts.
func renderContent(c any) string {
	switch v := c.(type) {
	case string:
		return v
	case []any:
		var b strings.Builder
		for _, p := range v {
			pm, _ := p.(map[string]any)
			typ, _ := pm["type"].(string)
			switch {
			case pm["image"] != nil || pm["image_url"] != nil || typ == "image" || typ == "image_url":
				b.WriteString("<|vision_start|><|image_pad|><|vision_end|>")
			case pm["video"] != nil || typ == "video":
				b.WriteString("<|vision_start|><|video_pad|><|vision_end|>")
			default:
				t, _ := pm["text"].(string)
				b.WriteString(t)
			}
		}
		return b.String()
	}
	return ""
}

type toolCall struct{ name, args string }

// renderCalls writes tool calls as the template does: one <parameter> per argument, strings
// as they are and other values as JSON.
func renderCalls(content string, calls []toolCall) string {
	var b strings.Builder
	for i, c := range calls {
		switch {
		case i > 0:
			b.WriteString("\n<tool_call>\n<function=" + c.name + ">\n")
		case strings.TrimSpace(content) != "":
			b.WriteString("\n\n<tool_call>\n<function=" + c.name + ">\n")
		default:
			b.WriteString("<tool_call>\n<function=" + c.name + ">\n")
		}
		for _, kv := range orderedObject(c.args) {
			v := kv.raw
			var s string
			if json.Unmarshal(v, &s) == nil {
				v = []byte(s)
			} else {
				v = []byte(pyJSONRaw(v))
			}
			b.WriteString("<parameter=" + kv.key + ">\n" + string(v) + "\n</parameter>\n")
		}
		b.WriteString("</function>\n</tool_call>")
	}
	return b.String()
}

type keyRaw struct {
	key string
	raw json.RawMessage
}

// orderedObject returns the members of a JSON object in their order (nil if s is not one).
func orderedObject(s string) []keyRaw {
	d := json.NewDecoder(strings.NewReader(s))
	if t, err := d.Token(); err != nil || t != json.Delim('{') {
		return nil
	}
	var out []keyRaw
	for d.More() {
		t, err := d.Token()
		k, ok := t.(string)
		if err != nil || !ok {
			return out
		}
		var v json.RawMessage
		if d.Decode(&v) != nil {
			return out
		}
		out = append(out, keyRaw{k, v})
	}
	return out
}

// pyJSON writes v as Jinja's tojson does (Python's separators ", " and ": ", no HTML
// escaping), with keys sorted as encoding/json sends them to the node.
func pyJSON(v any) string {
	var buf bytes.Buffer
	e := json.NewEncoder(&buf)
	e.SetEscapeHTML(false)
	e.Encode(v)
	return pyJSONRaw(bytes.TrimSpace(buf.Bytes()))
}

// pyJSONRaw rewrites compact JSON with Python's separators, keeping key order.
func pyJSONRaw(raw []byte) string {
	var b strings.Builder
	inStr, esc := false, false
	for _, c := range raw {
		if inStr {
			b.WriteByte(c)
			switch {
			case esc:
				esc = false
			case c == '\\':
				esc = true
			case c == '"':
				inStr = false
			}
			continue
		}
		switch c {
		case '"':
			inStr = true
			b.WriteByte(c)
		case ',':
			b.WriteString(", ")
		case ':':
			b.WriteString(": ")
		case ' ', '\n', '\r', '\t':
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}

// ---------------------------------------------------------------- the meter of one request

// maxMetered bounds the output text kept per request for counting (far above any real
// answer: max_tokens is capped at 32,768).
const maxMetered = 1 << 20

// meter collects what one request costs the node: the rendered prompt (tokenized in the
// background while the node works) and the output the gateway relays.
type meter struct {
	ready     chan struct{}
	prompt    string
	args      templateArgs
	hashes    []uint64 // the prompt's block hashes, when rendered before routing (nil = not)
	promptIDs []int32  // with a tokenizer

	reasoning, content strings.Builder
	calls              []*toolCall
	size               int
}

// newMeter starts metering nb; cp is its conversation if already rendered (nil = render it).
func newMeter(nb map[string]any, cp *convPrint, tok *Tokenizer) *meter {
	m := &meter{ready: make(chan struct{})}
	if cp != nil {
		m.prompt, m.args, m.hashes = cp.prompt, cp.args, cp.hashes
	}
	go func() {
		defer close(m.ready)
		if cp == nil {
			m.prompt, m.args = renderPrompt(nb)
		}
		if tok != nil {
			m.promptIDs = tok.Encode(m.prompt)
		}
	}()
	return m
}

// chunk takes the generated text of one streamed chunk.
func (m *meter) chunk(data []byte) {
	if m.size > maxMetered {
		return
	}
	m.size += len(data)
	var c struct {
		Choices []struct {
			Delta struct {
				Content          string `json:"content"`
				ReasoningContent string `json:"reasoning_content"`
				ToolCalls        []struct {
					Index    int `json:"index"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"delta"`
		} `json:"choices"`
	}
	if json.Unmarshal(data, &c) != nil {
		return
	}
	for _, ch := range c.Choices {
		m.content.WriteString(ch.Delta.Content)
		m.reasoning.WriteString(ch.Delta.ReasoningContent)
		for _, tc := range ch.Delta.ToolCalls {
			if tc.Index < 0 || tc.Index > 255 {
				continue
			}
			for len(m.calls) <= tc.Index {
				m.calls = append(m.calls, &toolCall{})
			}
			m.calls[tc.Index].name += tc.Function.Name
			m.calls[tc.Index].args += tc.Function.Arguments
		}
	}
}

// output is what the model generated after the prompt (reasoning, the end of thinking, the
// answer and its tool calls), and next is how the answer reappears in the next prompt of the
// conversation (for the cache).
func (m *meter) output() (gen, next string) {
	calls := make([]toolCall, len(m.calls))
	for i, c := range m.calls {
		calls[i] = *c
	}
	content := m.content.String()
	tc := renderCalls(content, calls)
	think, thinkNext := "", ""
	if m.args.thinking {
		think = m.reasoning.String() + "\n</think>\n\n"
		thinkNext = strings.TrimSpace(m.reasoning.String()) + "\n</think>\n\n"
	}
	return think + content + tc, thinkNext + strings.TrimSpace(content) + tc + "<|im_end|>\n"
}
