// Guards on answers from untrusted nodes (decision 0007 addendum). A volunteer can read and
// change what its node answers; an agent such as opencode may run a tool call without a
// person reading it. So tool calls from untrusted nodes are held back until complete and
// checked before the client sees them: download-and-execute, credential files, encoded
// commands, uploads of files, and web addresses that appear nowhere in the request. A
// flagged tool call is never released, and the node is held (gets no requests) until the operator looks.
// Plain text with unknown addresses is only flagged (logged), not blocked.
//
// These are tripwires for crude attacks, not a proof that an answer is honest: a careful
// attacker writes a harmful command that matches no pattern. Answer checking comes later.
//
// SPDX-License-Identifier: Apache-2.0
package gateway

import (
	"encoding/json"
	"net/netip"
	"regexp"
	"strings"
)

// Guard rules, as logged and stored in pause records.
const (
	ruleDownloadExec = "download-and-execute"
	ruleCredentials  = "credential-path"
	ruleEncoded      = "encoded-command"
	ruleUpload       = "file-upload"
	ruleUnknownURL   = "unknown-address"
	ruleTextURL      = "unknown-address-in-text" // flagged only
)

// guardRules match dangerous tool calls. Credential paths are excused when the request
// itself names them ("put the key in .env"): the node cannot add text to the request.
var guardRules = []struct {
	rule string
	re   *regexp.Regexp
}{
	{ruleDownloadExec, regexp.MustCompile(`(?i)\b(?:curl|wget|fetch|iwr|irm|invoke-webrequest|invoke-restmethod)\b[^\n|;&]*\|\s*(?:sudo\s+(?:-\S+\s+)*)?(?:env\s+)?(?:(?:ba|z|da|k|c|tc|fi)?sh|python[0-9.]*|perl|ruby|node|php|iex|invoke-expression)\b`)},
	{ruleDownloadExec, regexp.MustCompile(`(?i)\b(?:(?:ba|z|da|k)?sh|source|\.)\s+(?:-c\s+)?["']?(?:<\(|\$\(|` + "`" + `)\s*(?:curl|wget|fetch)\b`)},
	{ruleDownloadExec, regexp.MustCompile(`(?i)\b(?:iex|invoke-expression)\b[^\n]*\b(?:iwr|irm|invoke-webrequest|invoke-restmethod|downloadstring)\b`)},
	{ruleCredentials, regexp.MustCompile(`(?i)(?:^|[\s"'=:(/~])\.(?:ssh|aws|gnupg|kube|docker)(?:/|\b)|\bid_(?:rsa|dsa|ecdsa|ed25519)\b|\.git-credentials\b|(?:^|[\s"'=:(/~])\.netrc\b|/etc/shadow\b|(?:^|[\s"'=:(/~])\.env(?:\.[a-z]+)?(?:$|[\s"'):;,])`)},
	{ruleEncoded, regexp.MustCompile(`(?i)\bbase64\s+(?:-d|-D|--decode)\b[^\n]*\|\s*(?:sudo\s+)?(?:(?:ba|z|da|k)?sh|python[0-9.]*|perl|eval)\b`)},
	{ruleEncoded, regexp.MustCompile(`(?i)\b(?:(?:ba|z|da|k)?sh|eval)\b[^\n]*(?:\$\(|` + "`" + `)[^\n]*\bbase64\s+(?:-d|-D|--decode)\b`)},
	{ruleEncoded, regexp.MustCompile(`(?i)\b(?:powershell|pwsh)(?:\.exe)?\b[^\n]*\s-(?:e|en|enc|encodedcommand)\s+[A-Za-z0-9+/=]{16,}`)},
	{ruleEncoded, regexp.MustCompile(`(?i)\b(?:exec|eval)\s*\(\s*(?:__import__\(\s*["']base64["']\s*\)\.|base64\.)?b64decode\b`)},
	{ruleUpload, regexp.MustCompile(`\bcurl\b[^\n]*\s(?:-d|--data(?:-binary|-raw|-urlencode)?|-F|--form)\s*["']?(?:[\w.-]+=)?@`)},
	{ruleUpload, regexp.MustCompile(`\bcurl\b[^\n]*\s(?:-T|--upload-file)\s`)},
	{ruleUpload, regexp.MustCompile(`\bwget\b[^\n]*--post-file`)},
	{ruleUpload, regexp.MustCompile(`\b(?:nc|ncat|netcat|socat)\b[^\n]*<\s*\S`)},
	{ruleUpload, regexp.MustCompile(`\b(?:scp|rsync)\b[^\n]*\s(?:[\w.-]+@)?[A-Za-z0-9.-]+\.[A-Za-z]{2,}:`)},
}

// urlHostRe finds the host of a web address; bareHostRe a host given to curl or wget without
// a scheme.
var (
	urlHostRe  = regexp.MustCompile(`(?i)\b(?:https?|ftp|wss?)://(?:[^\s/@"'<>]*@)?(\[[0-9a-f:.]+\]|[^\s/"'<>?#:\\)\]]+)`)
	bareHostRe = regexp.MustCompile(`(?i)\b(?:curl|wget)\s+(?:-\S+\s+)*([a-z0-9-]+(?:\.[a-z0-9-]+)+)(?:[:/\s"']|$)`)
)

// unknownHost returns the first web address host in s that appears nowhere in the request
// (nor a parent domain of it), ignoring local and private addresses.
func unknownHost(s, req string) string {
	var hosts []string
	for _, m := range urlHostRe.FindAllStringSubmatch(s, -1) {
		hosts = append(hosts, m[1])
	}
	for _, m := range bareHostRe.FindAllStringSubmatch(s, -1) {
		hosts = append(hosts, m[1])
	}
	for _, h := range hosts {
		h = strings.ToLower(strings.Trim(strings.TrimRight(h, ".,;"), "[]"))
		if h == "" || localHost(h) || hostInRequest(h, req) {
			continue
		}
		return h
	}
	return ""
}

func localHost(h string) bool {
	if h == "localhost" || strings.HasSuffix(h, ".localhost") {
		return true
	}
	a, err := netip.ParseAddr(h)
	return err == nil && (a.IsLoopback() || a.IsPrivate() || a.IsUnspecified() || a.IsLinkLocalUnicast())
}

// hostInRequest: the host, or a parent domain with at least two labels, occurs in the
// request text (e.g. "raw.githubusercontent.com" is fine if the user wrote
// "githubusercontent.com").
func hostInRequest(h, req string) bool {
	for {
		if strings.Contains(req, h) {
			return true
		}
		_, parent, ok := strings.Cut(h, ".")
		if !ok || !strings.Contains(parent, ".") {
			return false
		}
		h = parent
	}
}

// requestText is all text of a request a node sees, lowercased, for the address check.
func requestText(body map[string]any) string {
	var b strings.Builder
	eachText(body, func(s string) { b.WriteString(strings.ToLower(s)); b.WriteByte('\n') })
	return b.String()
}

// checkToolCall returns the rule a tool call breaks, or "". Arguments are JSON; the check
// reads their string values (unescaped), so quoting cannot hide a pattern.
func checkToolCall(name, args, req string) string {
	text := name + "\n" + args
	var v any
	if json.Unmarshal([]byte(args), &v) == nil {
		var b strings.Builder
		var walk func(any)
		walk = func(x any) {
			switch y := x.(type) {
			case string:
				b.WriteString(y)
				b.WriteByte('\n')
			case []any:
				for _, e := range y {
					walk(e)
				}
			case map[string]any:
				for k, e := range y {
					b.WriteString(k)
					b.WriteByte('\n')
					walk(e)
				}
			}
		}
		walk(v)
		text += "\n" + b.String()
	}
	for _, r := range guardRules {
		for _, m := range r.re.FindAllString(text, -1) {
			if r.rule == ruleCredentials && strings.Contains(req, strings.ToLower(strings.Trim(m, " \t\n\"'=:(/~);,"))) {
				continue
			}
			return r.rule
		}
	}
	if unknownHost(text, req) != "" {
		return ruleUnknownURL
	}
	return ""
}

// maxGuardText bounds the answer text kept for the address check, and maxHeldArgs the tool
// call arguments held back per request.
const (
	maxGuardText = 256 << 10
	maxHeldArgs  = 2 << 20
)

// guard follows one streamed answer of an untrusted node.
type guard struct {
	req   string // requestText of the request
	calls []*heldCall
	held  int      // bytes of arguments held
	early [][]byte // chunks before any content (the role): held, so that an answer made only of
	// a flagged tool call has sent nothing yet and can be retried on a trusted node
	begun  bool   // content went out: nothing more is held early
	finish []byte // the chunk with finish_reason, held until the calls are released
	meta   map[string]any
	text   strings.Builder
}

type heldCall struct {
	id, typ, name string
	args          strings.Builder
}

func newGuard(req string) *guard { return &guard{req: req} }

// filter takes one streamed chunk and returns what may go to the client now. Tool-call
// deltas are taken out and held; a chunk with finish_reason is held while tool calls are
// pending (release puts it back after them; check first). tooLarge reports held arguments
// beyond maxHeldArgs.
func (gd *guard) filter(raw []byte) (outs [][]byte, tooLarge bool) {
	out, tooLarge := gd.filter1(raw)
	if out == nil || tooLarge {
		return nil, tooLarge
	}
	if !gd.begun && len(gd.early) < 8 && emptyChunk(out) {
		gd.early = append(gd.early, out)
		return nil, false
	}
	gd.begun = true
	outs, gd.early = append(gd.early, out), nil
	return outs, false
}

// emptyChunk: a chunk whose deltas carry nothing but a role (no text, no finish, no usage).
func emptyChunk(raw []byte) bool {
	var c struct {
		Choices []struct {
			Delta        map[string]any `json:"delta"`
			FinishReason *string        `json:"finish_reason"`
		} `json:"choices"`
		Usage any `json:"usage"`
	}
	if json.Unmarshal(raw, &c) != nil || c.Usage != nil || len(c.Choices) == 0 {
		return false
	}
	for _, ch := range c.Choices {
		if ch.FinishReason != nil {
			return false
		}
		for k, v := range ch.Delta {
			if k != "role" && v != nil && v != "" {
				return false
			}
		}
	}
	return true
}

func (gd *guard) filter1(raw []byte) (out []byte, tooLarge bool) {
	var c map[string]any
	if json.Unmarshal(raw, &c) != nil {
		return raw, false
	}
	if gd.meta == nil {
		gd.meta = map[string]any{}
		for _, k := range []string{"id", "object", "created", "model"} {
			if v, ok := c[k]; ok {
				gd.meta[k] = v
			}
		}
	}
	choices, _ := c["choices"].([]any)
	changed, finished, empty := false, false, true
	for _, ch := range choices {
		cm, _ := ch.(map[string]any)
		if cm == nil {
			continue
		}
		delta, _ := cm["delta"].(map[string]any)
		if t, ok := delta["content"].(string); ok && gd.text.Len() < maxGuardText {
			gd.text.WriteString(t)
		}
		if tcs, ok := delta["tool_calls"].([]any); ok {
			gd.hold(tcs)
			delete(delta, "tool_calls")
			changed = true
		}
		if f, _ := cm["finish_reason"].(string); f != "" {
			finished = true
		}
		for k, v := range delta {
			if k != "role" && v != nil && v != "" {
				empty = false
			}
		}
	}
	if gd.held > maxHeldArgs {
		return nil, true
	}
	if finished && len(gd.calls) > 0 {
		gd.finish, _ = json.Marshal(c)
		return nil, false
	}
	if !changed {
		return raw, false
	}
	if empty && !finished && c["usage"] == nil && len(choices) > 0 {
		return nil, false // only tool-call pieces: nothing to send yet
	}
	out, _ = json.Marshal(c)
	return out, false
}

func (gd *guard) hold(tcs []any) {
	for _, t := range tcs {
		tc, _ := t.(map[string]any)
		if tc == nil {
			continue
		}
		i := 0
		if f, ok := tc["index"].(float64); ok {
			i = int(f)
		}
		if i < 0 || i > 255 {
			continue
		}
		for len(gd.calls) <= i {
			gd.calls = append(gd.calls, &heldCall{typ: "function"})
		}
		k := gd.calls[i]
		if s, _ := tc["id"].(string); s != "" {
			k.id = s
		}
		if s, _ := tc["type"].(string); s != "" {
			k.typ = s
		}
		if fn, _ := tc["function"].(map[string]any); fn != nil {
			if s, _ := fn["name"].(string); s != "" {
				k.name = s
			}
			if s, _ := fn["arguments"].(string); s != "" {
				k.args.WriteString(s)
				gd.held += len(s)
			}
		}
	}
}

// check returns the rule the held tool calls break, or "".
func (gd *guard) check() string {
	for _, k := range gd.calls {
		if r := checkToolCall(k.name, k.args.String(), gd.req); r != "" {
			return r
		}
	}
	return ""
}

// textFlag returns ruleTextURL if the answer text names a web address absent from the request.
func (gd *guard) textFlag() string {
	if unknownHost(gd.text.String(), gd.req) != "" {
		return ruleTextURL
	}
	return ""
}

// release returns the held chunks, the tool calls as one chunk, then the held finish chunk,
// and clears them. Call it only after check passed.
func (gd *guard) release() [][]byte {
	out := gd.early
	gd.early = nil
	if len(gd.calls) > 0 {
		var tcs []any
		for i, k := range gd.calls {
			tcs = append(tcs, map[string]any{"index": i, "id": k.id, "type": k.typ,
				"function": map[string]any{"name": k.name, "arguments": k.args.String()}})
		}
		c := map[string]any{}
		for k, v := range gd.meta {
			c[k] = v
		}
		c["choices"] = []any{map[string]any{"index": 0, "delta": map[string]any{"tool_calls": tcs}, "finish_reason": nil}}
		b, _ := json.Marshal(c)
		out = append(out, b)
	}
	if gd.finish != nil {
		out = append(out, gd.finish)
	}
	gd.calls, gd.finish, gd.held = nil, nil, 0
	return out
}

// guardError is a tool call stopped by a guard.
type guardError struct{ rule string }

func (e *guardError) Error() string { return "answer stopped by a guard: " + e.rule }
