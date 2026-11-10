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

// commandRules read only what a call runs: shell commands (the "command", "cmd" and "script"
// arguments, strings in arrays too) and files written as shell scripts (a ".sh", ".bash", ".zsh"
// or ".ps1" path, or a shell's "#!" line). Netcat is the case: as a command, netcat fed from a
// file, a here-document or a pipe is a transfer whatever the order of its words ("nc < f host
// 443", "nc host < f 443", "cat f | nc host 443"), while in a Lean or Python file "nc" is a name
// and "<" a comparison, which held nodes for ordinary proofs (2026-10-31; code review
// 2026-11-10). A command word is at a line start, after ; & | ( { ! ` $(, after then, do, else
// or time, after a quote that opens "sh -c", and after variable assignments and sudo, exec, env,
// nohup, command or timeout.
const (
	ncLead = `(?:^|[;&|({!` + "`" + `]|\$\(|\b(?:then|do|else|time)\s|\b(?:ba|z|da|k)?sh\s+(?:-\w+\s+)*-c\s+["'])\s*`
	ncProg = `(?:\w+=\S*\s+|(?:sudo|exec|env|nohup|command|timeout\s+\S+)\s+(?:-\S+\s+)*)*(?:\S*/)?(?:nc|ncat|netcat)(?:\.exe)?\b`
)

var commandRules = []struct {
	rule, why string
	re        *regexp.Regexp
}{
	{ruleUpload, "netcat sending a file to a host and port", regexp.MustCompile(`(?m)` + ncLead + ncProg + `(?:[^;&|\n]|&>|>&)*<`)},
	{ruleUpload, "netcat sending piped input to a host", regexp.MustCompile(`\|&?\s*` + ncProg + `\s+\S`)},
}

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
	cmds := commandText(name, args)
	for _, r := range commandRules {
		if r.re.MatchString(cmds) {
			return guardHit{r.rule, r.why}
		}
	}
	if unknownHost(text, req) != "" {
		return guardHit{ruleUnknownURL, whyUnknownURL}
	}
	return guardHit{}
}

// cmdLines joins shell line continuations and keeps each line its own, so a command word
// stays at a line start.
var cmdLines = strings.NewReplacer("\\\r\n", " ", "\\\n", " ", "\r\n", "\n", "\r", "\n")

// shellShebang is a first line that runs a shell (not Python's "#!/usr/bin/env python3").
var shellShebang = regexp.MustCompile(`^#!\s*(?:\S*/)?(?:env\s+(?:-\S+\s+)*)?(?:ba|z|da|k|c|tc|fi|a)?sh\b|^#!\s*(?:\S*/)?(?:env\s+)?pwsh\b`)

// stringsIn lists the strings in a JSON value: the value itself, or those in an array or object,
// in order.
func stringsIn(v any) []string {
	switch x := v.(type) {
	case string:
		return []string{x}
	case []any:
		var out []string
		for _, e := range x {
			out = append(out, stringsIn(e)...)
		}
		return out
	case map[string]any:
		var out []string
		for _, e := range x {
			out = append(out, stringsIn(e)...)
		}
		return out
	}
	return nil
}

// commandText is what commandRules read: the call's "command", "cmd" and "script" strings, and
// the text of a file written as a shell script (a ".sh", ".bash", ".zsh" or ".ps1" path, or a "#!"
// first line), one per line. Every value of a key given twice counts, whichever one a client
// takes. Arguments that are not a JSON object are read whole, as they came.
func commandText(name, args string) string {
	vals := map[string][]string{}
	d := json.NewDecoder(strings.NewReader(args))
	ok := false
	if t, err := d.Token(); err == nil && t == json.Delim('{') {
		ok = true
		for d.More() {
			k, err := d.Token()
			var raw json.RawMessage
			if err != nil || d.Decode(&raw) != nil {
				ok = false
				break
			}
			var v any
			if key, isKey := k.(string); isKey && json.Unmarshal(raw, &v) == nil {
				vals[key] = append(vals[key], stringsIn(v)...)
			}
		}
	}
	if !ok {
		return cmdLines.Replace(strings.ReplaceAll(args, `\n`, "\n"))
	}
	var b strings.Builder
	add := func(keys ...string) {
		for _, k := range keys {
			for _, v := range vals[k] {
				b.WriteString(v)
				b.WriteByte('\n')
			}
		}
	}
	add("command", "cmd", "script")
	script := false
	for _, k := range []string{"filePath", "path", "file_path"} {
		for _, p := range vals[k] {
			p = strings.ToLower(p)
			for _, ext := range []string{".sh", ".bash", ".zsh", ".ps1"} {
				script = script || strings.HasSuffix(p, ext)
			}
		}
	}
	for _, k := range []string{"content", "newString", "new_string"} {
		for _, c := range vals[k] {
			if script || shellShebang.MatchString(c) {
				b.WriteString(c)
				b.WriteByte('\n')
			}
		}
	}
	return cmdLines.Replace(b.String())
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
