// SPDX-License-Identifier: AGPL-3.0-or-later

package gateway

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// ---------------------------------------------------------------- identity stripping

// nodeParams are the request fields a node may see. Everything else (user, metadata,
// session or chat ids that clients add) stays at the gateway: nodes see what to compute,
// not who asked.
var nodeParams = map[string]bool{
	"model": true, "messages": true, "stream": true, "stream_options": true,
	"max_tokens": true, "max_completion_tokens": true, "temperature": true, "top_p": true,
	"top_k": true, "min_p": true, "typical_p": true, "stop": true, "seed": true,
	"presence_penalty": true, "frequency_penalty": true, "repeat_penalty": true,
	"repeat_last_n": true, "logit_bias": true, "logprobs": true, "top_logprobs": true, "n": true,
	"tools": true, "tool_choice": true, "parallel_tool_calls": true, "response_format": true,
	"chat_template_kwargs": true, "reasoning_effort": true, "grammar": true, "json_schema": true,
}

// messageFields are the per-message fields a node may see. "name" is kept only for tool
// results, where it names the tool; on user or system messages it is often a person's name.
var messageFields = map[string]bool{
	"role": true, "content": true, "tool_calls": true, "tool_call_id": true, "reasoning_content": true,
}

// forNode returns a copy of the request body with only the fields a node may see.
func forNode(body map[string]any) map[string]any {
	out := make(map[string]any, len(body))
	for k, v := range body {
		if nodeParams[k] {
			out[k] = v
		}
	}
	if msgs, ok := body["messages"].([]any); ok {
		clean := make([]any, 0, len(msgs))
		for _, m := range msgs {
			mm, ok := m.(map[string]any)
			if !ok {
				continue
			}
			c := map[string]any{}
			for k, v := range mm {
				if messageFields[k] || (k == "name" && (mm["role"] == "tool" || mm["role"] == "function")) {
					c[k] = v
				}
			}
			clean = append(clean, c)
		}
		out["messages"] = clean
	}
	return out
}

// ---------------------------------------------------------------- secret redaction

// secretPatterns match credentials in well-known formats. They are replaced by placeholders
// before a request reaches a node and restored in the answer, so the node never sees them.
// Deliberately format-based (high precision): generic "password = ..." rules would also hit
// ordinary code and degrade answers.
var secretPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?s)-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----.*?-----END [A-Z0-9 ]*PRIVATE KEY-----`),
	regexp.MustCompile(`\b(?:AKIA|ASIA)[0-9A-Z]{16}\b`),                                         // AWS access key id
	regexp.MustCompile(`\b(?:gh[pousr]_[A-Za-z0-9]{36,255}|github_pat_[A-Za-z0-9_]{22,255})\b`), // GitHub
	regexp.MustCompile(`\bsk-(?:ant-|proj-)?[A-Za-z0-9_\-]{20,}`),                               // OpenAI, Anthropic and similar
	regexp.MustCompile(`\bxox[abprs]-[A-Za-z0-9\-]{10,}`),                                       // Slack
	regexp.MustCompile(`\b(?:sk|rk)_live_[A-Za-z0-9]{16,}\b`),                                   // Stripe
	regexp.MustCompile(`\bAIza[0-9A-Za-z_\-]{35}\b`),                                            // Google API key
	regexp.MustCompile(`\bhf_[A-Za-z0-9]{30,}\b`),                                               // Hugging Face
	regexp.MustCompile(`\beyJ[A-Za-z0-9_\-]{10,}\.eyJ[A-Za-z0-9_\-]{10,}\.[A-Za-z0-9_\-]{10,}`), // JWT
	regexp.MustCompile(`\b(?:ak|nt)-[A-Za-z0-9_\-]{40,}`),                                       // AIs@home API keys and node tokens
}

// redactor replaces secrets with per-request placeholders and restores them. Its mapping
// lives only in memory for one request and is never logged or sent to a node.
type redactor struct {
	nonce   string
	byValue map[string]string // secret → placeholder
	byPH    map[string]string // placeholder → secret
	re      *regexp.Regexp    // matches this request's placeholders
	prefix  string            // "[[SECRET_<nonce>_"
}

func newRedactor() *redactor {
	b := make([]byte, 3)
	rand.Read(b)
	nonce := hex.EncodeToString(b)
	return &redactor{nonce: nonce, byValue: map[string]string{}, byPH: map[string]string{},
		re: regexp.MustCompile(`\[\[SECRET_` + nonce + `_\d+\]\]`), prefix: "[[SECRET_" + nonce + "_"}
}

func (r *redactor) active() bool { return len(r.byPH) > 0 }

func (r *redactor) redactText(s string) string {
	for _, p := range secretPatterns {
		s = p.ReplaceAllStringFunc(s, func(secret string) string {
			if ph, ok := r.byValue[secret]; ok {
				return ph
			}
			ph := fmt.Sprintf("%s%d]]", r.prefix, len(r.byValue)+1)
			r.byValue[secret], r.byPH[ph] = ph, secret
			return ph
		})
	}
	return s
}

func (r *redactor) restore(s string) string {
	if !r.active() || !strings.Contains(s, "[[SECRET_") {
		return s
	}
	return r.re.ReplaceAllStringFunc(s, func(ph string) string {
		if v, ok := r.byPH[ph]; ok {
			return v
		}
		return ph
	})
}

// redactBody replaces secrets in every message's text and in earlier tool-call arguments
// (which may hold restored secrets from previous turns). It modifies body in place.
func (r *redactor) redactBody(body map[string]any) {
	msgs, _ := body["messages"].([]any)
	for _, m := range msgs {
		mm, ok := m.(map[string]any)
		if !ok {
			continue
		}
		switch c := mm["content"].(type) {
		case string:
			mm["content"] = r.redactText(c)
		case []any: // content parts: [{"type":"text","text":"..."}, ...]
			for _, part := range c {
				if pm, ok := part.(map[string]any); ok {
					if t, ok := pm["text"].(string); ok {
						pm["text"] = r.redactText(t)
					}
				}
			}
		}
		if calls, ok := mm["tool_calls"].([]any); ok {
			for _, tc := range calls {
				if fn, ok := tc.(map[string]any)["function"].(map[string]any); ok {
					if args, ok := fn["arguments"].(string); ok {
						fn["arguments"] = r.redactText(args)
					}
				}
			}
		}
	}
}

// partialStart returns the index from which s ends in what could still become one of this
// request's placeholders (so it must be held back until the next chunk), or len(s).
func (r *redactor) partialStart(s string) int {
	maxLen := len(r.prefix) + 12
	start := len(s) - maxLen
	if start < 0 {
		start = 0
	}
	for i := start; i < len(s); i++ {
		if s[i] == '[' && r.couldBePlaceholder(s[i:]) {
			return i
		}
	}
	return len(s)
}

// couldBePlaceholder reports whether t is a proper prefix of some placeholder.
func (r *redactor) couldBePlaceholder(t string) bool {
	if len(t) <= len(r.prefix) {
		return strings.HasPrefix(r.prefix, t)
	}
	if !strings.HasPrefix(t, r.prefix) {
		return false
	}
	rest := t[len(r.prefix):]
	if strings.HasSuffix(rest, "]]") {
		return false // complete: restore() handles it
	}
	for _, c := range strings.TrimSuffix(rest, "]") {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// restorer restores placeholders in a stream of chat.completion.chunk objects. A
// placeholder can be split across chunks, so text that may be the start of one is held
// back per field until it is complete or the stream ends.
type restorer struct {
	r       *redactor
	pending map[string]string // "choice/field[/tool]" → held-back text
}

func newRestorer(r *redactor) *restorer { return &restorer{r: r, pending: map[string]string{}} }

// chunk rewrites one streamed chunk. Unparseable chunks pass through unchanged.
func (s *restorer) chunk(raw []byte) []byte {
	var c map[string]any
	if json.Unmarshal(raw, &c) != nil {
		return raw
	}
	choices, _ := c["choices"].([]any)
	for _, ch := range choices {
		cm, ok := ch.(map[string]any)
		if !ok {
			continue
		}
		idx := fmt.Sprint(cm["index"])
		delta, _ := cm["delta"].(map[string]any)
		final := cm["finish_reason"] != nil
		if delta == nil && final {
			delta = map[string]any{}
			cm["delta"] = delta
		}
		if delta == nil {
			continue
		}
		for _, f := range []string{"content", "reasoning_content"} {
			key := idx + "/" + f
			if v, ok := delta[f].(string); ok || (final && s.pending[key] != "") {
				if out := s.take(key, v, final); out != "" || ok {
					delta[f] = out
				}
			}
		}
		if calls, ok := delta["tool_calls"].([]any); ok {
			for _, tc := range calls {
				tm, _ := tc.(map[string]any)
				fn, _ := tm["function"].(map[string]any)
				if fn == nil {
					continue
				}
				if args, ok := fn["arguments"].(string); ok {
					fn["arguments"] = s.take(idx+"/tool/"+fmt.Sprint(tm["index"]), args, final)
				}
			}
		}
		if final { // flush tool arguments that are still held back
			for key, rest := range s.pending {
				if strings.HasPrefix(key, idx+"/tool/") && rest != "" {
					ti, _ := strconv.Atoi(strings.TrimPrefix(key, idx+"/tool/"))
					calls, _ := delta["tool_calls"].([]any)
					delta["tool_calls"] = append(calls, map[string]any{"index": ti,
						"function": map[string]any{"arguments": s.r.restore(rest)}})
					delete(s.pending, key)
				}
			}
		}
	}
	out, err := json.Marshal(c)
	if err != nil {
		return raw
	}
	return out
}

// take appends text to the field's held-back text, restores complete placeholders and
// returns what can be released now (everything when final).
func (s *restorer) take(key, text string, final bool) string {
	t := s.r.restore(s.pending[key] + text)
	if final {
		delete(s.pending, key)
		return t
	}
	cut := s.r.partialStart(t)
	s.pending[key] = t[cut:]
	return t[:cut]
}

// flush returns a chunk carrying all held-back text, or nil (for the end of a stream
// without a finish_reason).
func (s *restorer) flush() []byte {
	if len(s.pending) == 0 {
		return nil
	}
	deltas := map[string]map[string]any{}
	for key, rest := range s.pending {
		parts := strings.SplitN(key, "/", 3)
		d := deltas[parts[0]]
		if d == nil {
			d = map[string]any{}
			deltas[parts[0]] = d
		}
		if len(parts) == 2 {
			d[parts[1]] = s.r.restore(rest)
		} else {
			ti, _ := strconv.Atoi(parts[2])
			calls, _ := d["tool_calls"].([]any)
			d["tool_calls"] = append(calls, map[string]any{"index": ti, "function": map[string]any{"arguments": s.r.restore(rest)}})
		}
	}
	s.pending = map[string]string{}
	var choices []any
	for idx, d := range deltas {
		i, _ := strconv.Atoi(idx)
		choices = append(choices, map[string]any{"index": i, "delta": d})
	}
	out, _ := json.Marshal(map[string]any{"choices": choices})
	return out
}

// ---------------------------------------------------------------- personal data

// piiPatterns flag requests that carry personal data; such requests go to trusted nodes
// only (the text itself is not changed: names and addresses cannot be found reliably, and
// rewriting them would degrade answers).
var (
	emailRe = regexp.MustCompile(`\b[A-Za-z0-9._%+\-]+@[A-Za-z0-9.\-]+\.[A-Za-z]{2,}\b`)
	digitRe = regexp.MustCompile(`\b\d(?:[ \-]?\d){10,18}\b`)
	ibanRe  = regexp.MustCompile(`\b[A-Z]{2}\d{2}(?: ?[A-Z0-9]{4}){2,7}(?: ?[A-Z0-9]{1,4})?\b`)
)

// hasPII reports whether any message text contains an email address, a payment card number
// (Luhn), an IBAN (mod 97) or a Norwegian national identity number (mod 11).
func hasPII(body map[string]any) bool {
	found := false
	eachText(body, func(s string) {
		if found {
			return
		}
		if emailRe.MatchString(s) {
			found = true
			return
		}
		for _, m := range digitRe.FindAllString(s, -1) {
			d := strings.NewReplacer(" ", "", "-", "").Replace(m)
			if (len(d) >= 13 && len(d) <= 19 && luhn(d)) || (len(d) == 11 && fodselsnummer(d)) {
				found = true
				return
			}
		}
		for _, m := range ibanRe.FindAllString(s, -1) {
			if iban(strings.ReplaceAll(m, " ", "")) {
				found = true
				return
			}
		}
	})
	return found
}

func eachText(body map[string]any, f func(string)) {
	msgs, _ := body["messages"].([]any)
	for _, m := range msgs {
		mm, _ := m.(map[string]any)
		switch c := mm["content"].(type) {
		case string:
			f(c)
		case []any:
			for _, part := range c {
				if pm, ok := part.(map[string]any); ok {
					if t, ok := pm["text"].(string); ok {
						f(t)
					}
				}
			}
		}
	}
}

func luhn(d string) bool {
	sum, alt := 0, false
	for i := len(d) - 1; i >= 0; i-- {
		n := int(d[i] - '0')
		if alt {
			if n *= 2; n > 9 {
				n -= 9
			}
		}
		sum, alt = sum+n, !alt
	}
	return sum%10 == 0
}

// fodselsnummer checks the two mod-11 control digits of a Norwegian national identity number.
func fodselsnummer(d string) bool {
	n := make([]int, 11)
	for i := range n {
		n[i] = int(d[i] - '0')
	}
	ctrl := func(w []int, upto int) int {
		s := 0
		for i := 0; i < upto; i++ {
			s += w[i] * n[i]
		}
		c := 11 - s%11
		if c == 11 {
			c = 0
		}
		return c
	}
	k1 := ctrl([]int{3, 7, 6, 1, 8, 9, 4, 5, 2}, 9)
	k2 := ctrl([]int{5, 4, 3, 2, 7, 6, 5, 4, 3, 2}, 10)
	return k1 != 10 && k2 != 10 && k1 == n[9] && k2 == n[10]
}

func iban(s string) bool {
	if len(s) < 15 || len(s) > 34 {
		return false
	}
	r := s[4:] + s[:4]
	mod := 0
	for _, c := range r {
		switch {
		case c >= '0' && c <= '9':
			mod = (mod*10 + int(c-'0')) % 97
		case c >= 'A' && c <= 'Z':
			v := int(c-'A') + 10
			mod = (mod*100 + v) % 97
		default:
			return false
		}
	}
	return mod == 1
}
