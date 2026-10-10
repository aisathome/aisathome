// SPDX-License-Identifier: Apache-2.0

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
//
// Placeholders keep the secret's format ("sk-proj-AISX7f3a01XXXX..." for "sk-proj-AbCd..."):
// a model handles them like any other key. Obviously marked placeholders ("[[SECRET_1]]")
// made the model refuse to repeat or write them (measured 2026-10-09).
type redactor struct {
	nonce     string
	byValue   map[string]string // secret → placeholder
	byPH      map[string]string // placeholder → secret
	maxLen    int
	overflow  bool           // more secrets than maxRedactions: the request must not be forwarded
	restoreOn bool           // KeyInfo.RestoreSecrets: put secrets back into answer text
	re        *regexp.Regexp // matches any placeholder of this request (built lazily)
}

// maxRedactions bounds the work per request (a request stuffed with fake keys would otherwise
// make every streamed chunk expensive).
const maxRedactions = 256

func newRedactor() *redactor {
	b := make([]byte, 2)
	rand.Read(b)
	return &redactor{nonce: hex.EncodeToString(b), byValue: map[string]string{}, byPH: map[string]string{}}
}

func (r *redactor) active() bool { return len(r.byPH) > 0 }

// restoring reports whether answers need rewriting at all.
func (r *redactor) restoring() bool { return r.restoreOn && r.active() }

// typePrefixRe matches the fixed marker that names a secret's kind. The placeholder keeps
// only this marker, never any of the random part.
var typePrefixRe = regexp.MustCompile(`^(?:sk-(?:proj-|ant-)?|gh[pousr]_|github_pat_|AKIA|ASIA|xox[abprs]-|[sr]k_live_|AIza|hf_|eyJ|ak-|nt-)`)

// typePrefix is the part of a secret that names its kind ("sk-proj-", "ghp_", "AKIA").
func typePrefix(s string) string { return typePrefixRe.FindString(s) }

func (r *redactor) placeholder(secret string) string {
	n := len(r.byPH) + 1
	if strings.HasPrefix(secret, "-----BEGIN") { // private key block: keep the armour lines
		lines := strings.Split(secret, "\n")
		return lines[0] + "\n" + fmt.Sprintf("AISX%s%02dREDACTEDKEY", r.nonce, n) + "\n" + lines[len(lines)-1]
	}
	ph := typePrefix(secret) + fmt.Sprintf("AISX%s%02d", r.nonce, n)
	if pad := len(secret) - len(ph); pad > 0 {
		ph += strings.Repeat("X", pad)
	}
	return ph
}

func (r *redactor) redactText(s string) string {
	for _, p := range secretPatterns {
		s = p.ReplaceAllStringFunc(s, func(secret string) string {
			if ph, ok := r.byValue[secret]; ok {
				return ph
			}
			if _, isPH := r.byPH[secret]; isPH || strings.Contains(secret, "AISX"+r.nonce) {
				return secret // already a placeholder (matched again by a later pattern)
			}
			if len(r.byPH) >= maxRedactions {
				r.overflow = true
				return secret
			}
			ph := r.placeholder(secret)
			r.byValue[secret], r.byPH[ph] = ph, secret
			r.maxLen = max(r.maxLen, len(ph))
			return ph
		})
	}
	return s
}

// Restore policy. A malicious node can place a placeholder where the client will send it
// somewhere: a link or image the chat UI loads, a tool call, or a command written as text.
// So by default nothing is restored (KeyInfo.RestoreSecrets). For keys that opt in,
// placeholders are never restored into tool arguments, and in a text field only if the
// whole field, before and after the placeholder, shows nothing URL-, path-, command- or
// markup-like. This is a second line of defence, not a guarantee; it fails closed.
var urlish = regexp.MustCompile(`(?i)[/\\?:%<>{}\[\]` + "`" + `@]|&#|www\.|[a-z0-9-]\.[a-z]{2,}|\d\.\d|[\x{FF0E}\x{3002}\x{FF61}]` +
	`|\b(?:0x[0-9a-f]{6,}|\d{8,10})\b|\b(?:curl|wget|nc|ncat|socat|telnet|ssh|scp|ftp|fetch|iwr|irm|invoke-\w+|http\w*)\b`)

// maxHeld bounds the text held back per field while waiting to decide; beyond it the
// placeholders simply stay.
const maxHeld = 64 << 10

// tailLen is how much emitted field text is kept to detect markers that span chunks.
const tailLen = 300

// matcher returns a regexp that matches this request's placeholders.
func (r *redactor) matcher() *regexp.Regexp {
	if r.re == nil {
		alts := make([]string, 0, len(r.byPH))
		for ph := range r.byPH {
			alts = append(alts, regexp.QuoteMeta(ph))
		}
		r.re = regexp.MustCompile(strings.Join(alts, "|"))
	}
	return r.re
}

// restore puts secrets back into a complete text field, if the field allows it.
func (r *redactor) restore(s string) string { return r.restoreField(false, "", s) }

// restoreField restores the placeholders in s, the rest of a text field, unless the field
// (seen: its earlier text already had a marker; tail: the end of its earlier text) has
// anything URL-like.
func (r *redactor) restoreField(seen bool, tail, s string) string {
	if !r.restoreOn || !r.active() || seen || !strings.Contains(s, "AISX"+r.nonce) || urlish.MatchString(tail+s) {
		return s
	}
	return r.matcher().ReplaceAllStringFunc(s, func(ph string) string { return r.byPH[ph] })
}

// bodyTextFields are the request fields whose strings a node sees.
var bodyTextFields = []string{"messages", "tools", "stop", "response_format", "grammar", "json_schema", "chat_template_kwargs"}

// redactBody replaces secrets in every string a node will see: all message fields
// (content, earlier reasoning, tool calls and results), tool definitions, stop sequences and
// response formats. It modifies body in place.
func (r *redactor) redactBody(body map[string]any) {
	for _, k := range bodyTextFields {
		if v, ok := body[k]; ok {
			body[k] = r.redactValue(v)
		}
	}
}

func (r *redactor) redactValue(v any) any {
	switch x := v.(type) {
	case string:
		return r.redactText(x)
	case []any:
		for i := range x {
			x[i] = r.redactValue(x[i])
		}
	case map[string]any:
		for k := range x {
			x[k] = r.redactValue(x[k])
		}
	}
	return v
}

// partialStart returns the index from which s ends in what could still become one of this
// request's placeholders (so it must be held back until the next chunk), or len(s).
func (r *redactor) partialStart(s string) int {
	for i := max(0, len(s)-r.maxLen+1); i < len(s); i++ {
		t := s[i:]
		for ph := range r.byPH {
			if len(t) < len(ph) && strings.HasPrefix(ph, t) {
				return i
			}
		}
	}
	return len(s)
}

// restorer restores placeholders in a stream of chat.completion.chunk objects (text fields
// only). A placeholder can be split across chunks, so text that may be the start of one is
// held back per field until it is complete or the stream ends.
type restorer struct {
	r       *redactor
	pending map[string]string // "choice/field" → held-back text
	seen    map[string]bool   // URL-like text has appeared in the field
	tail    map[string]string // end of the field's emitted text
}

func newRestorer(r *redactor) *restorer {
	return &restorer{r: r, pending: map[string]string{}, seen: map[string]bool{}, tail: map[string]string{}}
}

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
	}
	out, err := json.Marshal(c)
	if err != nil {
		return raw
	}
	return out
}

// take appends text to the field's held-back text and returns what can be released now.
// Once a placeholder appears, the rest of the field is held until it ends (or exceeds
// maxHeld): whether it may be restored depends on what follows it too.
func (s *restorer) take(key, text string, final bool) string {
	raw := s.pending[key] + text
	if final {
		delete(s.pending, key)
		return s.r.restoreField(s.seen[key], s.tail[key], raw)
	}
	cut := s.r.partialStart(raw) // hold back a possible placeholder start
	if m := s.r.matcher().FindStringIndex(raw); m != nil && m[0] < cut {
		cut = m[0]
	}
	if len(raw)-cut > maxHeld { // give up: release it all with the placeholders
		s.seen[key], cut = true, len(raw)
	}
	s.pending[key] = raw[cut:]
	out := raw[:cut]
	s.seen[key] = s.seen[key] || urlish.MatchString(s.tail[key]+out)
	if tail := s.tail[key] + out; len(tail) > tailLen {
		s.tail[key] = tail[len(tail)-tailLen:]
	} else {
		s.tail[key] = tail
	}
	return out
}

// flush returns a chunk carrying all held-back text, or nil (for the end of a stream
// without a finish_reason).
func (s *restorer) flush() []byte {
	if len(s.pending) == 0 {
		return nil
	}
	deltas := map[string]map[string]any{}
	for key, rest := range s.pending {
		parts := strings.SplitN(key, "/", 2)
		d := deltas[parts[0]]
		if d == nil {
			d = map[string]any{}
			deltas[parts[0]] = d
		}
		d[parts[1]] = s.r.restoreField(s.seen[key], s.tail[key], rest)
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
	var walk func(v any)
	walk = func(v any) {
		switch x := v.(type) {
		case string:
			f(x)
		case []any:
			for _, e := range x {
				walk(e)
			}
		case map[string]any:
			for _, e := range x {
				walk(e)
			}
		}
	}
	for _, k := range bodyTextFields {
		walk(body[k])
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
