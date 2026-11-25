// Guards on answers from every node but the requester's own. A
// volunteer can read and change what its node answers; an agent such as opencode may run a tool
// call without a person reading it. So tool calls from such nodes are held back until complete and
// checked before the client sees them: download-and-execute, credential files, encoded
// commands and uploads of files. From a node without an account, a config token's node or a
// pilot, a tool call that breaks one of these rules is never released, and the node is held
// (gets no requests) until the operator looks, unless it only runs an official installer
// (guard_official.go). From an account's node these rules only flag (guard.flagOnly): the
// guard cannot tell an honest model reading a credential file from an attack, stopping such
// answers could hold every node of the network on one agent's retries, and the defence is the
// client's own command approval. Each stop, hold and flag keeps its evidence, a fragment
// of the node's answer (guard_evidence.go). Web addresses that appear
// nowhere in the request and are not well-known sources (knownAddrs) are only flagged (logged),
// in tool calls as in plain text: honest coding agents write such addresses all the time
// (package indexes, documentation, git clones), and holding a node for one stopped all chat
// for hours (2026-11-18). An address used to download and run something still blocks, through
// download-and-execute, and so does a package install from an unknown index (guard_install.go).
//
// These are tripwires for crude attacks, not a proof that an answer is honest: a careful
// attacker writes a harmful command that matches no pattern. Answer checking comes later.
//
// SPDX-License-Identifier: Apache-2.0
package gateway

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Guard rules, as logged and stored in pause records.
const (
	ruleDownloadExec = "download-and-execute"
	ruleCredentials  = "credential-path"
	ruleEncoded      = "encoded-command"
	ruleUpload       = "file-upload"
	ruleUnknownURL   = "unknown-address"         // flagged only
	ruleTextURL      = "unknown-address-in-text" // flagged only
	// Streams no honest engine sends, built to make the guard and the client see different
	// calls: tool-call pieces after the calls were completed and released, or keys the guard
	// reads exactly while a client's decoder may match them in another case, or a chunk the guard
	// cannot decode or that carries calls in another shape than a client reads (whyBadChunk).
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
// what a hold record keeps to say which pattern fired: fixed text, never the call's own (the
// evidence, guard_evidence.go, keeps a fragment of the call). installer: the pattern can run an
// official installer (guard_official.go), which stops the answer without holding the node.
// refine, if set, reads a match further in the shell's view of the call (callTexts; pipedExec,
// guard_interp.go): it can drop the match or move its end.
var guardRules = []struct {
	rule, why string
	installer bool
	re        *regexp.Regexp
	refine    func(shell string, m []int) (end int, hold bool, from []int)
}{
	{ruleDownloadExec, "a download piped into a shell or interpreter", true, regexp.MustCompile(`(?i)\b(?:curl|wget|fetch|iwr|irm|invoke-webrequest|invoke-restmethod)\b[^\n|;&]*\|\s*(?:sudo\s+(?:(?-i:-[ugCDhpRrTU])\s+[^\s|;&-]\S*\s+|-\S+\s+)*)?(?:env\s+)?(?:(?:ba|z|da|k|c|tc|fi)?sh|python[0-9.]*|perl|ruby|node|php|iex|invoke-expression)\b`), pipedExec},
	{ruleDownloadExec, "a shell running a download", true, regexp.MustCompile(`(?i)\b(?:(?:ba|z|da|k)?sh|source|\.)\s+(?:-c\s+)?["']?(?:<\(|\$\(|` + "`" + `)\s*(?:curl|wget|fetch)\b`), nil},
	// "python3 <(curl …)", "node -e "$(curl …)"": the download is the program. Only options may come
	// before it ("python3 parse.py <(curl …)" reads it as data), with the values of python's -W and
	// -X, ruby's -r, -I, -C, perl's -M and -I.
	{ruleDownloadExec, "an interpreter running a download", true, regexp.MustCompile(`(?i)\b(?:python[0-9.]*|perl|ruby|node|php)\s+(?:(?-i:-[WXIrCM])\s+[^\s<$` + "`" + `'"]\S*\s+|-[^\s'"]+\s+)*["']?(?:<\(|\$\(|` + "`" + `)\s*(?:curl|wget|fetch)\b`), nil},
	{ruleDownloadExec, "PowerShell running a download", false, regexp.MustCompile(`(?i)\b(?:iex|invoke-expression)\b[^\n]*\b(?:iwr|irm|invoke-webrequest|invoke-restmethod|downloadstring)\b`), nil},
	{ruleCredentials, "a credential file or folder the request does not name", false, regexp.MustCompile(`(?i)(?:^|[\s"'=:(/~])\.(?:ssh|aws|gnupg|kube|docker)(?:/|\b)|\bid_(?:rsa|dsa|ecdsa|ed25519)\b|\.git-credentials\b|(?:^|[\s"'=:(/~])\.netrc\b|/etc/shadow\b|(?:^|[\s"'=:(/~])\.env(?:\.[a-z]+)?(?:$|[\s"'):;,])`), nil},
	{ruleEncoded, "base64 decoded into a shell or interpreter", false, regexp.MustCompile(`(?i)\bbase64\s+(?:-d|-D|--decode)\b[^\n]*\|\s*(?:sudo\s+)?(?:(?:ba|z|da|k)?sh|python[0-9.]*|perl|eval)\b`), nil},
	{ruleEncoded, "a shell running base64-decoded text", false, regexp.MustCompile(`(?i)\b(?:(?:ba|z|da|k)?sh|eval)\b[^\n]*(?:\$\(|` + "`" + `)[^\n]*\bbase64\s+(?:-d|-D|--decode)\b`), nil},
	{ruleEncoded, "PowerShell with an encoded command", false, regexp.MustCompile(`(?i)\b(?:powershell|pwsh)(?:\.exe)?\b[^\n]*\s-(?:e|en|enc|encodedcommand)\s+[A-Za-z0-9+/=]{16,}`), nil},
	{ruleEncoded, "exec or eval of base64-decoded code", false, regexp.MustCompile(`(?i)\b(?:exec|eval)\s*\(\s*(?:__import__\(\s*["']base64["']\s*\)\.|base64\.)?b64decode\b`), nil},
	{ruleUpload, "curl sending a file as form or data", false, regexp.MustCompile(`\bcurl\b[^\n]*\s(?:-d|--data(?:-binary|-raw|-urlencode)?|-F|--form)\s*["']?(?:[\w.-]+=)?@`), nil},
	{ruleUpload, "curl uploading a file", false, regexp.MustCompile(`\bcurl\b[^\n]*\s(?:-T|--upload-file)\s`), nil},
	{ruleUpload, "wget posting a file", false, regexp.MustCompile(`\bwget\b[^\n]*--post-file`), nil},
	{ruleUpload, "socat sending a file", false, regexp.MustCompile(`\bsocat\s(?:[^\n|;&<]|\d*>&|&>)*<\s*\S`), nil},
	{ruleUpload, "scp or rsync to a remote host", false, regexp.MustCompile(`\b(?:scp|rsync)\b[^\n]*\s(?:[\w.-]+@)?[A-Za-z0-9.-]+\.[A-Za-z]{2,}:`), nil},
}

// urlHostRe finds the host of a web address; bareHostRe a host given to curl or wget without
// a scheme. User information ends at the first '/', '?', '#' or '\\': in
// "https://evil.example#@pypi.org" or "https://evil.example\\@pypi.org" the host a client
// contacts is evil.example (code review, 2026-11-18).
var (
	urlHostRe  = regexp.MustCompile(`(?i)\b(?:https?|ftp|wss?)://(?:[^\s/?#\\@"'<>]*@)?(\[[0-9a-f:.]+\]|[^\s/"'<>?#:\\)\]]+)`)
	bareHostRe = regexp.MustCompile(`(?i)\b(?:curl|wget)\s+(?:-\S+\s+)*([a-z0-9-]+(?:\.[a-z0-9-]+)+)(?:[:/\s"']|$)`)
)

// unknownHost returns the first web address host in s that the request does not name (nor a
// parent domain of it) and is not a known source, ignoring local and private addresses.
// The work is linear in s and req: the request's host words are collected once, each host is
// looked up among them, and a known-source check reads at most maxAddrTail bytes after its host.
func unknownHost(s, req string, known knownAddrs) string {
	h, _ := unknownAddr(s, req, known)
	return h
}

// unknownAddr is unknownHost with where the host is in s (the evidence's match).
func unknownAddr(s, req string, known knownAddrs) (string, [2]int) {
	type addr struct{ host, end int } // the host is s[host:end]
	var addrs []addr
	for _, re := range []*regexp.Regexp{urlHostRe, bareHostRe} {
		for _, m := range re.FindAllStringSubmatchIndex(s, -1) {
			addrs = append(addrs, addr{m[2], m[3]})
		}
	}
	excused := map[string]bool{} // host → local or named in the request
	var named map[string]bool    // requestHosts(req), when first needed
	for _, a := range addrs {
		h := strings.ToLower(strings.Trim(strings.TrimRight(s[a.host:a.end], ".,;"), "[]"))
		if h == "" {
			continue
		}
		ok, seen := excused[h]
		if !seen {
			ok = localHost(h)
			if !ok {
				if named == nil {
					named = requestHosts(req)
				}
				ok = hostNamed(h, named)
			}
			excused[h] = ok
		}
		if ok {
			continue
		}
		if tail, whole := addrTail(s, a.end); known.has(h, tail, whole) {
			continue
		}
		return h, [2]int{a.host, a.end}
	}
	return "", [2]int{}
}

// maxAddrTail bounds what a known-source check reads after a host: the rest of its word, up to
// the next white space. A longer word is never a known source.
const maxAddrTail = 1024

// addrTail is the rest of the word after a host that ends at end: up to white space or the end
// of s, at most maxAddrTail bytes; ok is false if the word is longer.
func addrTail(s string, end int) (tail string, ok bool) {
	t := s[end:min(len(s), end+maxAddrTail+1)]
	if i := strings.IndexAny(t, " \t\r\n\v\f"); i >= 0 {
		return t[:i], true
	}
	return t, len(t) <= maxAddrTail
}

// defaultKnownAddrs are the sources Lean work fetches by itself, so an agent repeats them in
// its tool calls: Lake's git dependencies (Mathlib and its dependencies, Lean's own
// repositories), Mathlib's build cache, its documentation and Lean's package registry. A
// long agent conversation loses what the request once showed: its client compacts the history
// into a summary, and an address read from a file hours before is gone from the request (a
// prove2me task presumably repeated Mathlib's git address from its own lakefile after two
// compactions, and the guard held a node). Config.GuardKnownAddresses replaces the list.
var defaultKnownAddrs = []string{
	"github.com/leanprover-community",
	"github.com/leanprover",
	"leanprover-community.github.io",
	"lean-lang.org",
	"cache.mathlib.org",
	"lakecache.blob.core.windows.net",
}

// knownAddr is a known source: a host with its subdomains (path ""), or one host and the
// paths under path ("/leanprover-community": that account on a host shared by everyone).
type knownAddr struct{ host, path string }

type knownAddrs []knownAddr

var (
	knownHostRe = regexp.MustCompile(`^[a-z0-9-]+(?:\.[a-z0-9-]+)+$`)
	knownSegRe  = regexp.MustCompile(`^[a-z0-9_.~-]+$`)
)

// maxKnownAddrs bounds the list: it names a few sources, not the web.
const maxKnownAddrs = 64

// parseKnownAddrs reads entries "host" or "host/path/...", lower case, without a scheme.
func parseKnownAddrs(list []string) (knownAddrs, error) {
	return parseAddrList("guard_known_addresses", list)
}

// parseAddrList reads a list of the config setting key as parseKnownAddrs does.
func parseAddrList(key string, list []string) (knownAddrs, error) {
	if len(list) > maxKnownAddrs {
		return nil, fmt.Errorf("%s: at most %d entries", key, maxKnownAddrs)
	}
	var out knownAddrs
	for _, e := range list {
		host, path, _ := strings.Cut(e, "/")
		if !knownHostRe.MatchString(host) {
			return nil, fmt.Errorf("%s: %q: give a host name in lower case, then optionally /path, without a scheme", key, e)
		}
		k := knownAddr{host: host}
		if path != "" {
			for _, seg := range strings.Split(strings.TrimRight(path, "/"), "/") {
				if !knownSegRe.MatchString(seg) || seg == "." || seg == ".." {
					return nil, fmt.Errorf("%s: %q: path segments of lower-case letters, digits and _ . ~ -", key, e)
				}
				k.path += "/" + seg
			}
		}
		out = append(out, k)
	}
	return out, nil
}

// has: host, with the rest of its word (port, path, query, closing quotes), is a known source.
// The shell joins quoted and escaped pieces, expansions and continued lines into one word, so
// a word that does that after a known host (lean-lang.org"".evil.example, lean-lang.org\.x,
// x$y.lean-lang.org) names another host: the host must be a plain name and the rest of the
// word free of quotes (but closing ones), backslashes, $, ` and braces. A path entry needs the
// host itself and a plain path below it: no "." or ".." segment and no percent-escape, which a
// client could resolve to another account's path.
func (k knownAddrs) has(host string, tail string, whole bool) bool {
	if len(k) == 0 || !whole || !knownHostRe.MatchString(host) {
		return false
	}
	tail = strings.TrimRight(tail, wordClosers)
	if strings.ContainsAny(tail, wordSplices) {
		return false
	}
	path, plain := addrPath(tail)
	for _, a := range k {
		if a.path == "" {
			if host == a.host || strings.HasSuffix(host, "."+a.host) {
				return true
			}
			continue
		}
		if host == a.host && plain && (path == a.path || strings.HasPrefix(path, a.path+"/")) {
			return true
		}
	}
	return false
}

// wordClosers may end the word of an address: closing quotes and brackets, and punctuation of
// the text around it. wordSplices, anywhere before them, join more text into the host or path.
const (
	wordClosers = "'\")].,;:!"
	wordSplices = "'\"\\$`{}"
)

// addrPath is the path of an address from what follows its host: after an optional port, up to
// a query or fragment, lower case. plain is false if the path could resolve elsewhere.
func addrPath(rest string) (path string, plain bool) {
	if strings.HasPrefix(rest, ":") {
		rest = strings.TrimLeft(rest[1:], "0123456789")
	}
	if !strings.HasPrefix(rest, "/") {
		return "", rest == "" || strings.IndexByte("?#", rest[0]) >= 0
	}
	if i := strings.IndexAny(rest, "?#"); i >= 0 {
		rest = rest[:i]
	}
	path = strings.ToLower(rest)
	if strings.ContainsAny(path, "%\\") {
		return "", false
	}
	for _, seg := range strings.Split(path, "/") {
		if seg == "." || seg == ".." {
			return "", false
		}
	}
	return path, true
}

func localHost(h string) bool {
	if h == "localhost" || strings.HasSuffix(h, ".localhost") {
		return true
	}
	a, err := netip.ParseAddr(h)
	return err == nil && (a.IsLoopback() || a.IsPrivate() || a.IsUnspecified() || a.IsLinkLocalUnicast())
}

// requestHosts are the host-like words of a request text: each maximal run of letters, digits,
// '.', '-' and '_' with a dot in it, without dots at its ends, and each IPv6 address (a run of hex
// digits, ':' and '.' that parses as one), in its canonical form. A host is named only as a whole
// word: "hub.com" is not named by "github.com", nor "pkgs.example.net" by "my_pkgs.example.net"
// (code review, 2026-11-18: a substring let a registrable host pass). Letters and digits may be
// any script's (an internationalized name), but other non-ASCII characters end a word: a host in
// curly quotes or Chinese punctuation is still named (code review, 2026-11-18).
func requestHosts(req string) map[string]bool {
	m := map[string]bool{}
	runs(req, hostRune, func(w string) {
		if w = strings.Trim(w, "."); strings.Contains(w, ".") {
			m[w] = true
		}
	})
	runs(req, func(r rune) bool { return r >= '0' && r <= '9' || r >= 'a' && r <= 'f' || r == ':' || r == '.' }, func(w string) {
		if strings.Count(w, ":") < 2 || len(w) > maxIPv6Text {
			return
		}
		for _, c := range []string{w, strings.TrimRight(w, ".:")} {
			if a, err := netip.ParseAddr(c); err == nil && a.Is6() {
				m[a.String()] = true
				return
			}
		}
	})
	return m
}

// maxIPv6Text is the longest text of an IPv6 address: eight groups, or six and an IPv4 address.
const maxIPv6Text = len("ffff:ffff:ffff:ffff:ffff:ffff:255.255.255.255")

// hostRune: r can be part of a host name in a request (requestHosts). Invalid UTF-8 counts, as
// before: it ends no word.
func hostRune(r rune) bool {
	if r < utf8.RuneSelf {
		return r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '.' || r == '-' || r == '_'
	}
	return r == utf8.RuneError || unicode.IsLetter(r) || unicode.IsDigit(r) || unicode.IsMark(r)
}

// runs calls f with each maximal run of s whose runes satisfy in.
func runs(s string, in func(rune) bool, f func(string)) {
	start := -1
	for i := 0; i < len(s); {
		r, n := rune(s[i]), 1
		if r >= utf8.RuneSelf {
			r, n = utf8.DecodeRuneInString(s[i:])
		}
		if in(r) {
			if start < 0 {
				start = i
			}
		} else if start >= 0 {
			f(s[start:i])
			start = -1
		}
		i += n
	}
	if start >= 0 {
		f(s[start:])
	}
}

// maxHostLen is the longest host name DNS can resolve (253 bytes, so at most 127 labels).
const maxHostLen = 253

// hostNamed: the host, or a parent domain with at least two labels, is a word of the request
// (requestHosts); e.g. "raw.githubusercontent.com" is fine if the user wrote
// "githubusercontent.com". An IPv6 address is compared in its canonical form. A host longer than
// DNS allows is never named: looking up each parent of a long host would hash each suffix, work
// quadratic in its length (code review, 2026-11-18); within the bound it is at most 127 lookups
// of at most 253 bytes.
func hostNamed(h string, named map[string]bool) bool {
	if strings.Contains(h, ":") {
		a, err := netip.ParseAddr(h)
		return err == nil && named[a.String()]
	}
	if len(h) > maxHostLen {
		return false
	}
	for {
		if named[h] {
			return true
		}
		_, parent, ok := strings.Cut(h, ".")
		if !ok || !strings.Contains(parent, ".") {
			return false
		}
		h = parent
	}
}

// requestText is the text of a request that the client wrote, lowercased, for the address
// check: every field a node sees (bodyTextFields), but of the messages only those in a role the
// client writes (clientRoles). An earlier assistant message, its content, reasoning and tool-call
// arguments, came from a node: a host a node planted in its answer must not count as named by
// the user in the next request (code review, 2026-11-18). Tool results stay: the client's own
// tool runs produced them. A message in any other role, or no role, names nothing.
func requestText(body map[string]any) string {
	client := make(map[string]any, len(bodyTextFields))
	for _, k := range bodyTextFields {
		client[k] = body[k]
	}
	var kept []any
	msgs, _ := body["messages"].([]any)
	for _, m := range msgs {
		if mm, ok := m.(map[string]any); ok {
			if r, _ := mm["role"].(string); clientRoles[r] {
				kept = append(kept, mm)
			}
		}
	}
	client["messages"] = kept
	var b strings.Builder
	eachText(client, func(s string) { b.WriteString(strings.ToLower(s)); b.WriteByte('\n') })
	return b.String()
}

// clientRoles are the message roles whose text the client wrote or its own tools produced.
var clientRoles = map[string]bool{"system": true, "developer": true, "user": true, "tool": true, "function": true}

// guardHit is what stopped or flagged an answer: the rule and, for the operator's hold record, why
// it fired, in fixed words that name the pattern (guardRules) and hold nothing of the call or the
// request; and where it matched, for the evidence (guard_evidence.go): text[at[0]:at[1]] of the
// text the rule read, which is the node's answer, never the request. rule "" means nothing did.
type guardHit struct {
	rule, why string
	text      string // what the rule read: a tool call's text, the answer's text or a stream chunk
	at        [2]int // the match in text
	where     string // "tool call <name>", "answer text" or "stream"
	host      string // the host of the address the match names, if any
	official  bool   // an official installer (guard_official.go): stop the answer, do not hold the node
}

// blocks: the hit stops the answer and holds the node (unless official). An unknown address is
// only flagged.
func (h guardHit) blocks() bool { return h.rule != "" && h.rule != ruleUnknownURL }

// Reasons of the rules that are not a pattern of guardRules.
const (
	whyUnknownURL  = "a web address that is not in the request"
	whyTextURL     = "a web address in the answer's text that is not in the request"
	whyAfterFinish = "tool-call pieces after the calls were complete"
	whyOddKey      = "a stream key in an unusual case"
	whyBadChunk    = "a stream chunk that is not one plain JSON object of the stream's shape"
)

// streamHit is the hit of a stream rule (guard.violation) in chunk, the node's chunk that broke it.
func streamHit(rule string, chunk []byte) guardHit {
	h := guardHit{rule: rule, text: string(chunk), at: [2]int{0, len(chunk)}, where: "stream"}
	switch rule {
	case ruleAfterFinish:
		h.why = whyAfterFinish
	case ruleOddKey:
		h.why = whyOddKey
	}
	return h
}

var (
	// flatLines makes a text one line, joining shell line continuations first: rules that stop
	// at a line end then see all of a command, and all of a script as the JSON text used to show it.
	// A continuation goes without a trace, as in the shell: "lean-lang.org\<new line>.evil.example"
	// is one host.
	flatLines = strings.NewReplacer("\\\r\n", "", "\\\n", "", "\r\n", " ", "\n", " ", "\r", " ")
	// flatEscapes does the same for arguments that are not valid JSON.
	flatEscapes = strings.NewReplacer(`\\\r\n`, "", `\\\n`, "", `\\n`, " ", `\r`, " ", `\n`, " ", `\t`, " ", `\"`, `"`)
)

// callText is the text the rules read: every key, string and number of the arguments, decoded,
// in order (a key given twice shows both values, whichever one a client takes), as one line.
// Not the JSON itself: in `"cd x\ncat y"` the escape puts the words "ncat" and "nc" where the
// command has none (2 of 4,535 recorded calls, one being Python's `\nc = open(...)`), and
// quoting could hide a pattern. Arguments that do not parse are read as they came.
func callText(name, args string) string {
	text, _ := callTexts(name, args)
	return text
}

// shellLines is flatLines keeping line ends, each as one "\n" where flatLines puts its space, so
// the two texts have the same length and offsets.
var shellLines = strings.NewReplacer("\\\r\n", "", "\\\n", "", "\r\n", "\n", "\n", "\n", "\r", "\n")

// callTexts is callText and, at the same offsets, the text as the shell reads it: line ends kept,
// and each key, string and number of the arguments ending in a line end instead of a space, so a
// command ends with its string: the description after it is no argument of its last command
// (code review, 2026-11-21). shell is "" for arguments that do not parse.
func callTexts(name, args string) (text, shell string) {
	if !json.Valid([]byte(args)) {
		return name + "\n" + flatEscapes.Replace(args), ""
	}
	var b, sh strings.Builder
	d := json.NewDecoder(strings.NewReader(args))
	d.UseNumber()
	for {
		t, err := d.Token()
		if err != nil {
			break
		}
		v := ""
		switch x := t.(type) {
		case string:
			v = x
		case json.Number:
			v = x.String()
		default:
			continue
		}
		b.WriteString(v)
		b.WriteByte(' ')
		sh.WriteString(shellLines.Replace(v))
		sh.WriteByte('\n')
	}
	text = name + "\n" + flatLines.Replace(b.String())
	shell = name + "\n" + sh.String()
	if len(shell) != len(text) {
		return text, "" // cannot happen: the replacers match the same places
	}
	return text, shell
}

// checkToolCall returns what a tool call breaks; hit.rule is "" if nothing. A hit that blocks
// comes before ruleUnknownURL, which only flags (guardHit.blocks). The rules read a
// write or edit tool's file text as well as shell commands: a script written now can run in the
// next call, and in 1,032 recorded write and edit calls no rule fired on such text.
func checkToolCall(name, args, req string, known knownAddrs) guardHit {
	return checkCall(name, args, req, known, nil)
}

// checkCall is checkToolCall with the official installers (guard_official.go): a download-and-
// execute match that runs only those is an official hit, which every other blocking hit in the
// call goes before.
func checkCall(name, args, req string, known, installers knownAddrs) guardHit {
	where := "tool call " + name
	text, shell := callTexts(name, args)
	var official guardHit
	for _, r := range guardRules {
		read := map[int]bool{} // where refine read on from and found nothing to stop
		for _, m := range r.re.FindAllStringIndex(text, -1) {
			if r.rule == ruleCredentials && strings.Contains(req, strings.ToLower(strings.Trim(text[m[0]:m[1]], " \t\n\"'=:(/~);,"))) {
				continue
			}
			if r.refine != nil {
				if read[m[1]] {
					continue
				}
				end, hold, from := r.refine(shell, m)
				if !hold {
					for _, i := range from {
						read[i] = true
					}
					continue
				}
				m = []int{m[0], end}
			}
			h := guardHit{rule: r.rule, why: r.why, text: text, at: [2]int{m[0], m[1]}, where: where, host: matchHost(text, m, r.installer)}
			if r.installer && officialInstall(text, m[0], m[1], installers) {
				if official.rule == "" {
					h.official = true
					official = h
				}
				continue
			}
			return h
		}
	}
	cmds := commandText(name, args)
	for _, r := range commandRules {
		if m := r.re.FindStringIndex(cmds); m != nil {
			return guardHit{rule: r.rule, why: r.why, text: cmds, at: [2]int{m[0], m[1]}, where: where, host: matchHost(cmds, m, false)}
		}
	}
	if host, src := installSourceHit(name, args, req, known); host != "" {
		h := guardHit{rule: ruleInstallSource, why: whyInstallSource, where: where, host: host, text: src, at: [2]int{0, len(src)}}
		if i := strings.Index(strings.ToLower(text), host); i >= 0 && len(strings.ToLower(text)) == len(text) {
			h.text, h.at = text, [2]int{i, i + len(host)} // the install in the call, if its host shows as is
		}
		return h
	}
	if official.rule != "" {
		return official
	}
	if host, at := unknownAddr(text, req, known); host != "" {
		return guardHit{rule: ruleUnknownURL, why: whyUnknownURL, text: text, at: at, where: where, host: host}
	}
	return guardHit{}
}

// matchHost is the host of the first web address in the match m of text; for a pattern that can run
// an installer, also in the rest of its line ("sh -c "$(curl … https://…)"": the match ends at curl).
func matchHost(text string, m []int, installer bool) string {
	end := m[1]
	if installer {
		end = min(len(text), end+maxFragment)
		if i := strings.IndexByte(text[m[1]:end], '\n'); i >= 0 {
			end = m[1] + i
		}
	}
	return hostIn(text[m[0]:end])
}

// cmdLines joins shell line continuations and keeps each line its own, so a command word
// stays at a line start.
var cmdLines = strings.NewReplacer("\\\r\n", " ", "\\\n", " ", "\r\n", "\n", "\r", "\n")

// shellShebang is a first line that runs a shell (not Python's "#!/usr/bin/env python3").
var shellShebang = regexp.MustCompile(`^#!\s*(?:\S*/)?(?:env\s+(?:-\S+\s+)*)?(?:ba|z|da|k|c|tc|fi|a)?sh\b|^#!\s*(?:\S*/)?(?:env\s+)?pwsh\b`)

// stringsIn lists the strings in a JSON value: the value itself, or those in an array or object,
// in order.
func stringsIn(v any) []string { return appendStrings(nil, v) }

// appendStrings appends the strings in v to out: one slice for all levels, so deep nesting costs
// no copies of what lower levels found.
func appendStrings(out []string, v any) []string {
	switch x := v.(type) {
	case string:
		out = append(out, x)
	case []any:
		for _, e := range x {
			out = appendStrings(out, e)
		}
	case map[string]any:
		for _, e := range x {
			out = appendStrings(out, e)
		}
	}
	return out
}

// argValues maps each key of a JSON object of arguments to its strings (stringsIn), every value
// of a key given twice; ok is false if args is not a JSON object.
func argValues(args string) (vals map[string][]string, ok bool) {
	vals = map[string][]string{}
	d := json.NewDecoder(strings.NewReader(args))
	if t, err := d.Token(); err != nil || t != json.Delim('{') {
		return nil, false
	}
	for d.More() {
		k, err := d.Token()
		var raw json.RawMessage
		if err != nil || d.Decode(&raw) != nil {
			return nil, false
		}
		// Numbers as written: a number beyond a float64's range (1e400) is valid JSON, and it
		// must not make the strings beside it in the same value invisible.
		var v any
		vd := json.NewDecoder(bytes.NewReader(raw))
		vd.UseNumber()
		if key, isKey := k.(string); isKey && vd.Decode(&v) == nil {
			vals[key] = append(vals[key], stringsIn(v)...)
		}
	}
	return vals, true
}

// commandText is what commandRules read: the call's "command", "cmd" and "script" strings, and
// the text of a file written as a shell script (a ".sh", ".bash", ".zsh" or ".ps1" path, or a "#!"
// first line), one per line. Every value of a key given twice counts, whichever one a client
// takes. Arguments that are not a JSON object are read whole, as they came.
func commandText(name, args string) string {
	vals, ok := argValues(args)
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

// guard follows one streamed answer of a node that is not the requester's own.
type guard struct {
	req        string     // requestText of the request
	known      knownAddrs // sources known without the request (defaultKnownAddrs)
	installers knownAddrs // official installers (guard_official.go)
	// flagOnly: an account's node, and a requester whose tool calls a person approves (not a
	// project's or an unattended key: KeyInfo.Unattended). The command, credential, install and
	// address rules only flag (logged with their evidence; the answer goes out, the node serves
	// on). The guard cannot
	// tell an honest model reading a credential file from an attack, and stopping such answers
	// could hold every node of the network; the client's own command approval is the defence.
	// Stream rules (violation) still stop the answer.
	flagOnly bool
	calls    []*heldCall
	legacy   *heldCall // a legacy "function_call" (one call, no index), held as tool calls are
	flag     guardHit  // a flag-only rule a checked tool call broke (ruleUnknownURL)
	held     int       // bytes of arguments held
	early    [][]byte  // chunks before any content (the role): held, so that an answer made only of
	// a flagged tool call has sent nothing yet (the client gets a clean 502)
	begun  bool   // content went out: nothing more is held early
	finish []byte // the chunk with finish_reason, held until the calls are released
	meta   map[string]any
	text   strings.Builder
	// released: tool calls were checked and passed on; more pieces of tool calls after that
	// would be checked apart from what the client joins them to.
	released  bool
	violation string // a stream rule broken (ruleAfterFinish, ruleOddKey): stop the answer
	violating []byte // the chunk that broke it, for the evidence
	// violationWhy: the reason of a malformed chunk (whyOddKey, whyBadChunk); "" for the rule's own.
	violationWhy string
}

type heldCall struct {
	id, typ, name string
	args          strings.Builder
}

func newGuard(req string, known knownAddrs) *guard { return &guard{req: req, known: known} }

// flagOnlyNode: an account's node (n-), whose answers the command rules only flag. A node without
// an account, a config token's node and a pilot still stop and hold.
func flagOnlyNode(n *nodeConn) bool { return n.account != "" && !n.anon && n.pilot == "" }

// violationHit is the hit of the stream rule broken.
func (gd *guard) violationHit() guardHit {
	h := streamHit(gd.violation, gd.violating)
	if gd.violationWhy != "" {
		h.why = gd.violationWhy
	}
	return h
}

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

// filter1 is filter without the early chunks. Every chunk is decoded once, strictly
// (decodeChunk); one that does not decode, or whose tool-call pieces have another shape than a
// client reads, is a stream rule broken (ruleOddKey): it never goes out as it came, so the guard
// and the client cannot read different calls from it. A chunk the guard changed (it took tool
// calls out) goes out as the gateway writes it; any other as the node wrote it, which decodes to
// the same in every conforming decoder (no key twice, no key in another case, one object).
func (gd *guard) filter1(raw []byte) (out []byte, tooLarge bool) {
	c, err := decodeChunk(raw)
	if err != nil || callKeyElsewhere(c) {
		gd.violate(whyBadChunk, raw)
		return nil, false
	}
	if oddKey(c) {
		gd.violate(whyOddKey, raw)
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
	var choices []any
	switch x := c["choices"].(type) {
	case nil:
	case []any:
		choices = x
	default:
		gd.violate(whyBadChunk, raw)
		return nil, false
	}
	changed, finished, empty := false, false, true
	for _, ch := range choices {
		cm, ok := ch.(map[string]any)
		if !ok {
			gd.violate(whyBadChunk, raw)
			return nil, false
		}
		var delta map[string]any
		switch x := cm["delta"].(type) {
		case nil:
		case map[string]any:
			delta = x
		default:
			gd.violate(whyBadChunk, raw)
			return nil, false
		}
		if t, ok := delta["content"].(string); ok && gd.text.Len() < maxGuardText {
			gd.text.WriteString(t)
		}
		for _, key := range []string{"tool_calls", "function_call"} {
			v, has := delta[key]
			if !has {
				continue
			}
			if gd.released && pieces(v) > 0 {
				gd.violation, gd.violating = ruleAfterFinish, raw
				return nil, false
			}
			if gd.holdCalls(key, v) != nil {
				gd.violate(whyBadChunk, raw)
				return nil, false
			}
			delete(delta, key)
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
	if finished && gd.holding() {
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

// violate records a malformed chunk (ruleOddKey) with why, the reason for the hold record.
func (gd *guard) violate(why string, raw []byte) {
	gd.violation, gd.violationWhy, gd.violating = ruleOddKey, why, raw
}

// holding reports tool calls held back, not yet released.
func (gd *guard) holding() bool { return len(gd.calls) > 0 || gd.legacy != nil }

// errChunk is a streamed chunk the guard cannot read as a client would (decodeChunk, holdCalls).
var errChunk = errors.New("malformed chunk")

// maxChunkDepth bounds the nesting of a streamed chunk (an honest one nests about six deep).
const maxChunkDepth = 32

// decodeChunk decodes a streamed chunk: exactly one JSON object, numbers as written (a number
// beyond a float64's range, such as 1e400, is valid JSON that other decoders take), and no key
// twice in one object (decoders differ in which one they keep). Anything else is errChunk.
func decodeChunk(raw []byte) (map[string]any, error) {
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	v, err := decodeValue(d, 0)
	if err != nil {
		return nil, errChunk
	}
	if _, err := d.Token(); err != io.EOF { // something after the object
		return nil, errChunk
	}
	c, ok := v.(map[string]any)
	if !ok {
		return nil, errChunk
	}
	return c, nil
}

func decodeValue(d *json.Decoder, depth int) (any, error) {
	if depth > maxChunkDepth {
		return nil, errChunk
	}
	t, err := d.Token()
	if err != nil {
		return nil, err
	}
	switch t {
	case json.Delim('{'):
		m := map[string]any{}
		for d.More() {
			kt, err := d.Token()
			if err != nil {
				return nil, err
			}
			k, _ := kt.(string)
			if _, dup := m[k]; dup {
				return nil, errChunk
			}
			if m[k], err = decodeValue(d, depth+1); err != nil {
				return nil, err
			}
		}
		_, err = d.Token() // '}'
		return m, err
	case json.Delim('['):
		a := []any{}
		for d.More() {
			v, err := decodeValue(d, depth+1)
			if err != nil {
				return nil, err
			}
			a = append(a, v)
		}
		_, err = d.Token() // ']'
		return a, err
	case json.Delim('}'), json.Delim(']'):
		return nil, errChunk
	}
	return t, nil // a string, json.Number, bool or nil
}

// callKeys are the keys under which an answer carries tool calls: "tool_calls", and the legacy
// "function_call" (one call), which OpenAI's clients still read.
var callKeys = map[string]bool{"tool_calls": true, "function_call": true}

// callKeyElsewhere reports a call key (callKeys) anywhere in a chunk but directly in a choice's
// delta, where the guard holds them: in a "message", say, which a lenient client may read from a
// stream too. No engine streams them anywhere else.
func callKeyElsewhere(c map[string]any) bool {
	for k, v := range c {
		chs, isList := v.([]any)
		if k != "choices" || !isList {
			if callKeys[k] || hasCallKey(v) {
				return true
			}
			continue
		}
		for _, ch := range chs {
			cm, _ := ch.(map[string]any)
			for ck, cv := range cm {
				if d, ok := cv.(map[string]any); ck == "delta" && ok {
					for dk, dv := range d {
						if !callKeys[dk] && hasCallKey(dv) {
							return true
						}
					}
					continue
				}
				if callKeys[ck] || hasCallKey(cv) {
					return true
				}
			}
		}
	}
	return false
}

// hasCallKey reports a call key (callKeys) anywhere in v.
func hasCallKey(v any) bool {
	switch x := v.(type) {
	case map[string]any:
		for k, e := range x {
			if callKeys[k] || hasCallKey(e) {
				return true
			}
		}
	case []any:
		for _, e := range x {
			if hasCallKey(e) {
				return true
			}
		}
	}
	return false
}

// pieces counts the tool-call pieces in a delta's call key: the list's entries, or 1 for a
// legacy function_call.
func pieces(v any) int {
	switch x := v.(type) {
	case []any:
		return len(x)
	case map[string]any:
		return 1
	}
	return 0
}

// holdCalls holds the pieces of a delta's "tool_calls" (a list, each piece naming its call by
// index) or "function_call" (the legacy single call, which has no index). errChunk: a piece of
// another shape than the one a client reads (an index that is not a whole number from 0 to 255,
// a name or arguments that are not a string): the pieces held so far no longer show the call.
func (gd *guard) holdCalls(key string, v any) error {
	if v == nil {
		return nil
	}
	if key == "function_call" {
		fn, ok := v.(map[string]any)
		if !ok {
			return errChunk
		}
		if gd.legacy == nil {
			gd.legacy = &heldCall{}
		}
		return gd.holdFunction(gd.legacy, fn)
	}
	tcs, ok := v.([]any)
	if !ok {
		return errChunk
	}
	for _, t := range tcs {
		tc, ok := t.(map[string]any)
		if !ok {
			return errChunk
		}
		i := 0
		if x := tc["index"]; x != nil {
			num, _ := x.(json.Number)
			n, err := num.Int64()
			if err != nil || n < 0 || n > 255 {
				return errChunk
			}
			i = int(n)
		}
		id, okID := optString(tc, "id")
		typ, okType := optString(tc, "type")
		if !okID || !okType {
			return errChunk
		}
		for len(gd.calls) <= i {
			gd.calls = append(gd.calls, &heldCall{typ: "function"})
		}
		k := gd.calls[i]
		if id != "" {
			k.id = id
		}
		if typ != "" {
			k.typ = typ
		}
		switch fn := tc["function"].(type) {
		case nil:
		case map[string]any:
			if err := gd.holdFunction(k, fn); err != nil {
				return err
			}
		default:
			return errChunk
		}
	}
	return nil
}

// holdFunction adds a piece's function name and arguments to the call k.
func (gd *guard) holdFunction(k *heldCall, fn map[string]any) error {
	name, okName := optString(fn, "name")
	args, okArgs := optString(fn, "arguments")
	if !okName || !okArgs {
		return errChunk
	}
	if name != "" {
		k.name = name
	}
	k.args.WriteString(args)
	gd.held += len(args)
	return nil
}

// optString is m[k] if it is a string or absent (""); ok is false for any other value.
func optString(m map[string]any, k string) (s string, ok bool) {
	switch v := m[k].(type) {
	case nil:
		return "", true
	case string:
		return v, true
	}
	return "", false
}

// check returns what the held tool calls break that blocks; hit.rule is "" if nothing. A rule
// that only flags is kept for flagged. A call that is not official goes before an official one.
// With flagOnly nothing blocks: the first blocking hit is kept for flagged instead.
func (gd *guard) check() guardHit {
	var official guardHit
	calls := gd.calls
	if gd.legacy != nil {
		calls = append(calls[:len(calls):len(calls)], gd.legacy)
	}
	for _, k := range calls {
		h := checkCall(k.name, k.args.String(), gd.req, gd.known, gd.installers)
		if !gd.stops(h) {
			continue
		}
		if h.blocks() && !h.official {
			return h
		}
		if h.official && official.rule == "" {
			official = h
		}
		if h.rule != "" && !h.blocks() && gd.flag.rule == "" {
			gd.flag = h
		}
	}
	return official
}

// stops reports whether h may stop the answer. With flagOnly it never does: a blocking hit
// becomes the flag (over a flag-only one, so the log names the rule that matters).
func (gd *guard) stops(h guardHit) bool {
	if !gd.flagOnly || !h.blocks() {
		return true
	}
	if gd.flag.rule == "" || !gd.flag.blocks() {
		gd.flag = h
	}
	return false
}

// flagged returns the hit to log for an answer that passed: ruleUnknownURL if a tool call named
// a web address absent from the request, else ruleTextURL if the answer text did; rule "" if none.
func (gd *guard) flagged() guardHit {
	if gd.flag.rule != "" {
		return gd.flag
	}
	text := gd.text.String()
	if host, at := unknownAddr(text, gd.req, gd.known); host != "" {
		return guardHit{rule: ruleTextURL, why: whyTextURL, text: text, at: at, where: "answer text", host: host}
	}
	return guardHit{}
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
		out = append(out, gd.chunkOf(map[string]any{"tool_calls": tcs}))
	}
	if k := gd.legacy; k != nil { // in the shape it came in: a client that reads it reads this one
		out = append(out, gd.chunkOf(map[string]any{"function_call": map[string]any{"name": k.name, "arguments": k.args.String()}}))
	}
	if gd.finish != nil {
		out = append(out, gd.finish)
	}
	gd.released = gd.released || gd.holding()
	gd.calls, gd.legacy, gd.finish, gd.held = nil, nil, nil, 0
	return out
}

// chunkOf is a chunk of the answer (its id, object, created and model) with one choice and delta.
func (gd *guard) chunkOf(delta map[string]any) []byte {
	c := map[string]any{}
	for k, v := range gd.meta {
		c[k] = v
	}
	c["choices"] = []any{map[string]any{"index": 0, "delta": delta, "finish_reason": nil}}
	b, _ := json.Marshal(c)
	return b
}

// streamKeys are the chunk keys the guard and the answer assembly read. encoding/json matches
// struct fields case-insensitively, and clients may too, while the guard reads exact keys.
var streamKeys = []string{"choices", "delta", "message", "tool_calls", "function_call", "function", "arguments", "name",
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

// checkAssembled returns what a tool call in an assembled (non-streaming) answer breaks that
// blocks; hit.rule is "" if nothing; an official installer only if no call breaks a rule
// otherwise. flag is a flag-only rule a call broke. The client gets exactly these calls, however
// the stream was cut into pieces.
func checkAssembled(ans map[string]any, req string, known, installers knownAddrs) (hit, flag guardHit) {
	choices, _ := ans["choices"].([]any)
	for _, ch := range choices {
		cm, _ := ch.(map[string]any)
		msg, _ := cm["message"].(map[string]any)
		calls, _ := msg["tool_calls"].([]any)
		if fc, ok := msg["function_call"].(map[string]any); ok { // assemble writes none; checked all the same
			calls = append(calls[:len(calls):len(calls)], map[string]any{"function": fc})
		}
		for _, c := range calls {
			cm, _ := c.(map[string]any)
			fn, _ := cm["function"].(map[string]any)
			name, _ := fn["name"].(string)
			args, _ := fn["arguments"].(string)
			h := checkCall(name, args, req, known, installers)
			switch {
			case h.blocks() && !h.official:
				return h, guardHit{}
			case h.official:
				if hit.rule == "" {
					hit = h
				}
			case h.rule != "" && flag.rule == "":
				flag = h
			}
		}
	}
	if hit.rule != "" {
		return hit, guardHit{}
	}
	return guardHit{}, flag
}

// guardError is a tool call stopped by a guard.
type guardError struct{ rule string }

func (e *guardError) Error() string { return "answer stopped by a guard: " + e.rule }
