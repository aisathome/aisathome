// Which program an interpreter at the end of "curl … | python3" runs. Piped into a shell, a
// download is the shell's program; piped into python, node, ruby, perl or php it is the program
// only if the interpreter gets none from its arguments: "curl … | python3 -c 'import sys, re; …'"
// or "curl … | python3 parse.py" reads the download as data on standard input, as "curl … | jq"
// does. Agents fetch a page and clean it up like that all the time, and the old pattern, which
// stopped at the interpreter's name, held the owner's RTX 4090 node for five hours for one
// (2026-11-21).
//
// So for an interpreter the guard reads the rest of its command as the shell would (words,
// quotes, redirections, substitutions) and lets the pipe pass only if the program is an argument:
// python's -c or -m, node's -e/-p/--eval/--print, ruby's and perl's -e (perl -E), php's -r/-R/-F/-f,
// or a script path. Everything else still holds: no program ("| python3", "| node"), "-" or
// /dev/stdin, an interactive flag (-i, node --interactive, php -a), a module that reads code from
// standard input (python -m code, pdb), an inline program that itself runs code (exec, eval,
// system, subprocess, child_process, ...: "python3 -c 'exec(sys.stdin.read())'"), a command or
// process substitution where the program goes ("python3 <(curl …)", "-c "$(curl …)""), a
// here-document, an option it does not know whose value might be the "script", and a later stage
// of the same pipeline that is a shell or such an interpreter ("| python3 -c '…' | sh"). When the
// command cannot be read to its end, it holds too.
//
// Shells are not read this way: "| sh", "| bash -s -- args" and "| sudo bash" hold whatever follows.
//
// SPDX-License-Identifier: Apache-2.0
package gateway

import (
	"regexp"
	"strings"
)

var (
	// pipeInterp is the interpreter a match of the pipe pattern ends with (not a shell).
	pipeInterp = regexp.MustCompile(`(?i)(?:python[0-9.]*|perl|ruby|node|php)$`)
	// interpName and shellName are the command words of a later pipeline stage: the pipe pattern's
	// interpreters and shells, by base name.
	interpName = regexp.MustCompile(`(?i)^(?:python[0-9.]*|perl|ruby|node|php)$`)
	shellName  = regexp.MustCompile(`(?i)^(?:(?:ba|z|da|k|c|tc|fi)?sh|iex|invoke-expression)$`)
	assignWord = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*=`)
	// stdinPath is a script path that is standard input itself.
	stdinPath = regexp.MustCompile(`^(?:/dev/stdin|/dev/fd/0|/proc/[^/]+/fd/0)$`)
	// runsCode: an inline program that runs other code (its standard input, say), per language.
	runsCode = map[string]*regexp.Regexp{
		"python": regexp.MustCompile(`\b(?:exec|eval|execfile|system|popen|Popen|spawn[lv]?p?e?|exec[lv]p?e?|interact|startfile)\s*\(|\b(?:subprocess|runpy|pty|InteractiveConsole)\b`),
		"node":   regexp.MustCompile(`\b(?:eval|execSync|execFile|execFileSync|spawn|spawnSync|fork|runInThisContext|runInNewContext|runInContext|compileFunction)\b|\bchild_process\b|\bFunction\s*\(`),
		"ruby":   regexp.MustCompile("\\b(?:eval|instance_eval|class_eval|module_eval|instance_exec|system|exec|spawn|syscall|Open3)\\b|(?i:popen)|%x[^\\w\\s]|`"),
		"perl":   regexp.MustCompile("\\b(?:eval|system|exec)\\b|\\bqx\\s*[^\\w\\s]|`"),
		"php":    regexp.MustCompile("(?i)\\b(?:eval|assert|system|exec|shell_exec|passthru|proc_open|popen|pcntl_exec|create_function)\\b|php://stdin|`"),
	}
	// pythonDenyModules read Python code from standard input: python -m code, pdb's commands.
	pythonDenyModules = wordSet("code", "pdb", "idlelib", "IPython", "ipython", "bpython", "ptpython", "runpy", "trace", "profile", "cProfile", "pydoc")
)

// maxSubstDepth bounds nested substitutions; deeper text counts as unreadable.
const maxSubstDepth = 32

// pipedExec reads a match m of the pipe pattern ("a download piped into a shell or interpreter").
// hold says whether it runs a download; end is where the hit ends: m[1], or the command word of a
// later stage that runs it. walked is how far the reading went when it holds nothing: a later
// match that starts before it is a stage (or a quoted word) of the same pipeline, already read.
func pipedExec(text string, m []int) (end int, hold bool, walked int) {
	loc := pipeInterp.FindStringIndex(text[m[0]:m[1]])
	if loc == nil {
		return m[1], true, 0 // a shell or PowerShell
	}
	// The interpreter's name must be the whole command word ("php-cgi", "python3-config" are other
	// programs, but were held as before).
	if m[1] < len(text) && !strings.ContainsRune(" \t\n|;&<>)", rune(text[m[1]])) {
		return m[1], true, 0
	}
	lang := langOf(text[m[0]+loc[0] : m[1]])
	return walkPipe(text, m[1], lang)
}

// langOf is the language of an interpreter's command word: python, node, ruby, perl or php.
func langOf(name string) string {
	n := strings.ToLower(name)
	if strings.HasPrefix(n, "python") {
		return "python"
	}
	return n
}

// walkPipe reads an interpreter's stage from i (after its name), then the stages that follow it
// in the pipeline; see pipedExec.
func walkPipe(text string, i int, lang string) (end int, hold bool, walked int) {
	for {
		words, op, j, ok := stageWords(text, i)
		if !ok || !programInArgs(lang, words) {
			return i, true, 0
		}
		if op != "|" && op != "|&" {
			return j, false, j
		}
		// The next stage: past assignments and sudo, env, exec, nohup, command or time, its command word.
		cmd, k, ok := commandWord(text, j)
		if !ok {
			return j, true, 0
		}
		base := commandName(cmd.word)
		switch {
		case cmd.subst:
			return k, true, 0
		case shellName.MatchString(base):
			return k, true, 0
		case interpName.MatchString(base):
			lang, i = langOf(base), k
		default: // a filter such as grep, tee or head: read past it to the next stage
			lang, i = "", k
		}
	}
}

// commandWord is the command word of the stage at i, after assignments and wrappers that run
// the command after them (sudo, env, nice, timeout, ...) with their options; ok is false if there
// is none.
func commandWord(text string, i int) (shTok, int, bool) {
	wrapper := ""
	for {
		t, j, ok := shLex(text, i, 0)
		if !ok || t.op != "" || t.bad {
			return t, j, false
		}
		i = j
		switch {
		case t.subst:
			return t, j, true
		case assignWord.MatchString(t.word):
		case wrapper != "" && strings.HasPrefix(t.word, "-"):
			if wrapper == "env" && (t.word == "-S" || strings.HasPrefix(t.word, "--split-string")) {
				return t, j, false // env -S splits a string into a command: not read
			}
			if strings.Contains(wrapperValued[wrapper], " "+t.word+" ") {
				if _, j, ok = shLex(text, i, 0); !ok { // the option's value
					return t, j, false
				}
				i = j
			}
		case (wrapper == "timeout" || wrapper == "nice") && durationWord.MatchString(t.word):
		case wrapperValued[t.word] != "":
			wrapper = t.word
		default:
			return t, j, true
		}
	}
}

// wrapperValued are the commands that run the command after them, each with its options that take
// a value as the next word.
var wrapperValued = map[string]string{
	"sudo": " -u -g -C -D -h -p -R -r -T -U ", "env": " -u -C --unset --chdir ", "exec": " -a ",
	"nohup": " ", "command": " ", "time": " -f -o ", "nice": " -n ", "timeout": " -s -k --signal --kill-after ",
	"stdbuf": " -i -o -e ", "doas": " -u -C ",
}

// durationWord is the niceness or time limit of nice and timeout.
var durationWord = regexp.MustCompile(`^[+-]?[0-9.]+[smhd]?$`)

// stageWords lists the words of the command at i up to the operator that ends it (op, "" at the
// end of the text), without redirections; j is after that operator. ok is false if the stage
// cannot be read: a here-document, or an operator no command takes ("(").
func stageWords(text string, i int) (words []shTok, op string, j int, ok bool) {
	for {
		t, k, more := shLex(text, i, 0)
		if !more {
			return words, "", len(text), true
		}
		i = k
		switch t.op {
		case "":
			if t.bad {
				return nil, "", i, false
			}
			words = append(words, t)
		case "|", "|&", "||", "&&", ";", ";;", "&", ")", "\n":
			return words, t.op, i, true
		case "<<", "<<-", "(":
			return nil, t.op, i, false
		default: // a redirection: its target is no argument
			w, k, more := shLex(text, i, 0)
			if !more || w.op != "" || w.bad {
				return nil, t.op, i, false
			}
			i = k
		}
	}
}

// programInArgs: the interpreter of lang, with these arguments, gets its program from them, so its
// standard input is data. lang "" is a command that is no interpreter (a filter): true.
func programInArgs(lang string, words []shTok) bool {
	switch lang {
	case "":
		return true
	case "python":
		return pythonProgram(words)
	case "node":
		return nodeProgram(words)
	case "ruby", "perl":
		return rubyPerlProgram(lang, words)
	case "php":
		return phpProgram(words)
	}
	return false
}

// argReader walks an interpreter's arguments.
type argReader struct {
	words []shTok
	i     int
}

// next returns the next argument; ok is false at the end or at a substitution, which could be anything.
func (a *argReader) next() (string, bool) {
	if a.i >= len(a.words) {
		return "", false
	}
	w := a.words[a.i]
	a.i++
	return w.word, !w.subst
}

// value is an option's value: the rest of its word, else the next argument.
func (a *argReader) value(rest string) (string, bool) {
	if rest != "" {
		return rest, true
	}
	return a.next()
}

// script: w is a script path that is not standard input.
func script(w string) bool { return w != "-" && !stdinPath.MatchString(w) }

// inline: code, an inline program of lang, does not run other code.
func inline(lang, code string) bool { return !runsCode[lang].MatchString(code) }

// pythonProgram: python [options] (-c code | -m module | script | -) [args].
func pythonProgram(words []shTok) bool {
	a := &argReader{words: words}
	for {
		w, ok := a.next()
		if !ok {
			return false // no program, or a substitution: standard input or anything
		}
		switch {
		case w == "--":
			s, ok := a.next()
			return ok && script(s)
		case strings.HasPrefix(w, "--"):
			name, _, hasVal := strings.Cut(w, "=")
			switch name {
			case "--check-hash-based-pycs":
				if !hasVal {
					if _, ok := a.next(); !ok {
						return false
					}
				}
			default: // --version, --help: nothing runs, but nothing to tell either
				return false
			}
		case strings.HasPrefix(w, "-") && len(w) > 1:
			for k := 1; k < len(w); k++ {
				switch c := w[k]; {
				case c == 'c':
					code, ok := a.value(w[k+1:])
					return ok && inline("python", code)
				case c == 'm':
					mod, ok := a.value(w[k+1:])
					mod, _, _ = strings.Cut(mod, ".")
					return ok && !pythonDenyModules[mod]
				case c == 'W' || c == 'X':
					if _, ok := a.value(w[k+1:]); !ok {
						return false
					}
					k = len(w)
				case strings.IndexByte("bBdEhIOPqsSuvVxR", c) >= 0:
				default: // -i reads standard input after the program; unknown flags
					return false
				}
			}
		default:
			return script(w)
		}
	}
}

// nodeProgram: node [options] (-e code | -p code | script | -) [args]. An option it does not know
// may take the next word as its value, so a script must come after another word then.
func nodeProgram(words []shTok) bool {
	a := &argReader{words: words}
	valued := wordSet("--require", "-r", "--import", "--loader", "--experimental-loader", "--conditions", "-C",
		"--title", "--env-file", "--env-file-if-exists", "--input-type", "--inspect-port", "--debug-port",
		"--redirect-warnings", "--stack-trace-limit", "--max-old-space-size", "--watch-path", "--test-reporter",
		"--test-reporter-destination", "--disable-warning", "--experimental-default-type", "--diagnostic-dir", "--report-dir")
	flags := wordSet("--no-warnings", "--no-deprecation", "--trace-warnings", "--trace-deprecation", "--trace-uncaught",
		"--enable-source-maps", "--experimental-strip-types", "--experimental-transform-types", "--experimental-vm-modules",
		"--experimental-modules", "--experimental-json-modules", "--harmony", "--use-strict", "--abort-on-uncaught-exception",
		"--preserve-symlinks", "--preserve-symlinks-main", "--expose-gc", "--inspect", "--inspect-brk", "--watch",
		"--pending-deprecation", "--throw-deprecation", "--no-experimental-fetch", "--frozen-intrinsics", "-v", "--version",
		"-h", "--help", "-c", "--check", "--test")
	unsure := false // the last option might have taken the word after it
	for {
		w, ok := a.next()
		if !ok {
			return false
		}
		name, _, hasVal := strings.Cut(w, "=")
		switch {
		case w == "--":
			s, ok := a.next()
			return ok && script(s)
		case w == "-":
			return false
		case name == "-e" || name == "--eval" || name == "-p" || name == "--print" || name == "-pe" || name == "-ep":
			code, ok := "", true
			if hasVal {
				_, code, _ = strings.Cut(w, "=")
			} else {
				code, ok = a.next()
			}
			return ok && inline("node", code)
		case name == "-i" || name == "--interactive":
			return false
		case valued[name]:
			if !hasVal {
				if _, ok := a.next(); !ok {
					return false
				}
			}
			unsure = false
		case flags[name] || hasVal && strings.HasPrefix(w, "--"):
			unsure = false
		case strings.HasPrefix(w, "-"):
			unsure = true
		case unsure:
			unsure = false // perhaps the option's value
		default:
			return script(w)
		}
	}
}

// rubyPerlProgram: ruby or perl [switches] (-e code | script | -) [args]. Their short switches
// combine ("-ne", "-lane", "-rjson -e"); some take a value, attached or as the next word, some an
// optional attached one.
func rubyPerlProgram(lang string, words []shTok) bool {
	code, valued, attached, flags := "e", "IrCEFX", "0xTWK", "nplaswvdcyUhS"
	if lang == "perl" {
		code, valued, attached, flags = "eE", "IMm", "0xliCdDVF", "npaswWXtTcuUvhS"
	}
	a := &argReader{words: words}
	for {
		w, ok := a.next()
		if !ok {
			return false
		}
		switch {
		case w == "--":
			s, ok := a.next()
			return ok && script(s)
		case strings.HasPrefix(w, "--"):
			name, _, hasVal := strings.Cut(w, "=")
			if !hasVal && name != "--version" && name != "--help" && name != "--verbose" && name != "--yjit" && name != "--jit" && name != "--copyright" {
				return false
			}
		case strings.HasPrefix(w, "-") && len(w) > 1:
			for k := 1; k < len(w); k++ {
				c := w[k]
				switch {
				case strings.IndexByte(code, c) >= 0:
					prog, ok := a.value(w[k+1:])
					return ok && inline(lang, prog)
				case strings.IndexByte(valued, c) >= 0:
					if _, ok := a.value(w[k+1:]); !ok {
						return false
					}
					k = len(w)
				case strings.IndexByte(attached, c) >= 0:
					k = len(w)
				case strings.IndexByte(flags, c) >= 0:
				default:
					return false
				}
			}
		default:
			return script(w)
		}
	}
}

// phpProgram: php [options] (-r code | -R code | -F file | -f file | file) [args]. php with no file
// runs standard input, also after "--".
func phpProgram(words []shTok) bool {
	long := map[string]byte{"--run": 'r', "--process-code": 'R', "--process-file": 'F', "--file": 'f',
		"--interactive": 'a', "--define": 'd', "--php-ini": 'c', "--no-php-ini": 'n', "--process-begin": 'B',
		"--process-end": 'E', "--zend-extension": 'z', "--no-header": 'q'}
	a := &argReader{words: words}
	for {
		w, ok := a.next()
		if !ok || w == "--" || w == "-" {
			return false
		}
		if !strings.HasPrefix(w, "-") {
			return script(w)
		}
		opt := w[1:]
		if strings.HasPrefix(w, "--") {
			name, val, hasVal := strings.Cut(w, "=")
			c, known := long[name]
			if !known {
				return false
			}
			opt = string(c)
			if hasVal {
				opt += val
			}
		}
		for k := 0; k < len(opt); k++ {
			switch c := opt[k]; {
			case c == 'r' || c == 'R':
				prog, ok := a.value(opt[k+1:])
				return ok && inline("php", prog)
			case c == 'F' || c == 'f':
				s, ok := a.value(opt[k+1:])
				return ok && script(s)
			case c == 'B' || c == 'E':
				prog, ok := a.value(opt[k+1:])
				if !ok || !inline("php", prog) {
					return false
				}
				k = len(opt)
			case c == 'd' || c == 'c' || c == 'z':
				if _, ok := a.value(opt[k+1:]); !ok {
					return false
				}
				k = len(opt)
			case strings.IndexByte("nqhilmsvwHeC", c) >= 0:
			default: // -a is interactive
				return false
			}
		}
	}
}

// shTok is a shell token: an operator, or a word as the shell passes it on (quotes and escapes
// removed; expansions kept as written).
type shTok struct {
	op    string // "|", "&&", ">", "2>", "<<", ... or "" for a word
	word  string
	subst bool // a command or process substitution, or a backquote, is part of the word
	bad   bool // the word does not end (an open quote or substitution) or nests too deep
}

// shLex reads the token at i, after blanks; more is false if only blanks are left. depth counts
// the substitutions it is inside.
func shLex(s string, i, depth int) (t shTok, j int, more bool) {
	for i < len(s) && (s[i] == ' ' || s[i] == '\t') {
		i++
	}
	if i >= len(s) {
		return t, i, false
	}
	if op := shOp(s, i); op != "" {
		return shTok{op: op}, i + len(op), true
	}
	var b strings.Builder
	digits := true
	for i < len(s) {
		c := s[i]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '|' || c == '&' || c == ';' || c == '(' || c == ')':
			return shTok{word: b.String(), subst: t.subst}, i, true
		case (c == '<' || c == '>') && i+1 < len(s) && s[i+1] == '(':
			end := skipParens(s, i+2, depth+1)
			if end < 0 {
				return shTok{word: b.String(), subst: true, bad: true}, len(s), true
			}
			b.WriteString(s[i:end])
			t.subst, i = true, end
		case c == '<' || c == '>':
			if digits && b.Len() > 0 { // "2>/dev/null": the digits are part of the redirection
				op := b.String() + shOp(s, i)
				return shTok{op: op}, i + len(op) - b.Len(), true
			}
			return shTok{word: b.String(), subst: t.subst}, i, true
		case c == '\\':
			if i+1 < len(s) {
				b.WriteByte(s[i+1])
				i += 2
			} else {
				b.WriteByte(c)
				i++
			}
		case c == '\'':
			end := strings.IndexByte(s[i+1:], '\'')
			if end < 0 {
				b.WriteString(s[i+1:])
				return shTok{word: b.String(), subst: t.subst, bad: true}, len(s), true
			}
			b.WriteString(s[i+1 : i+1+end])
			i += end + 2
		case c == '"':
			i++
			for {
				if i >= len(s) {
					return shTok{word: b.String(), subst: t.subst, bad: true}, len(s), true
				}
				d := s[i]
				if d == '"' {
					i++
					break
				}
				switch {
				case d == '\\' && i+1 < len(s) && strings.IndexByte("$`\"\\\n", s[i+1]) >= 0:
					b.WriteByte(s[i+1])
					i += 2
				case d == '$' && i+1 < len(s) && s[i+1] == '(' || d == '`':
					end := skipSubst(s, i, depth+1)
					if end < 0 {
						return shTok{word: b.String(), subst: true, bad: true}, len(s), true
					}
					b.WriteString(s[i:end])
					t.subst, i = true, end
				default:
					b.WriteByte(d)
					i++
				}
			}
		case c == '$' && i+1 < len(s) && s[i+1] == '(' || c == '`':
			end := skipSubst(s, i, depth+1)
			if end < 0 {
				return shTok{word: b.String(), subst: true, bad: true}, len(s), true
			}
			b.WriteString(s[i:end])
			t.subst, i = true, end
		case c == '$' && i+1 < len(s) && s[i+1] == '\'': // $'…', ANSI-C quoting
			k := i + 2
			for k < len(s) && s[k] != '\'' {
				if s[k] == '\\' {
					k++
				}
				k++
			}
			if k >= len(s) {
				b.WriteString(s[i+2:])
				return shTok{word: b.String(), subst: t.subst, bad: true}, len(s), true
			}
			b.WriteString(s[i+2 : k])
			i = k + 1
		default:
			b.WriteByte(c)
			i++
		}
		if c < '0' || c > '9' {
			digits = false
		}
	}
	return shTok{word: b.String(), subst: t.subst}, i, true
}

// shOp is the operator at s[i], or "".
func shOp(s string, i int) string {
	for _, op := range shOps {
		if strings.HasPrefix(s[i:], op) {
			if (op == "<" || op == ">") && i+1 < len(s) && s[i+1] == '(' {
				return "" // a process substitution, part of a word
			}
			return op
		}
	}
	return ""
}

// shOps, longest first.
var shOps = []string{"&>>", "<<<", "<<-", "||", "|&", "&&", ";;", "&>", "<<", "<&", "<>", ">>", ">&", ">|", "|", "&", ";", "(", ")", "<", ">", "\n"}

// skipSubst returns the end of the command substitution at s[i] ("$(…)" or "`…`"), or -1.
func skipSubst(s string, i, depth int) int {
	if s[i] == '`' {
		for k := i + 1; k < len(s); k++ {
			switch s[k] {
			case '\\':
				k++
			case '`':
				return k + 1
			}
		}
		return -1
	}
	return skipParens(s, i+2, depth)
}

// skipParens returns the index after the ")" that closes a "(" ending at i, or -1.
func skipParens(s string, i, depth int) int {
	if depth > maxSubstDepth {
		return -1
	}
	open := 1
	for {
		t, j, more := shLex(s, i, depth)
		if !more || t.bad {
			return -1
		}
		i = j
		switch t.op {
		case "(":
			open++
		case ")":
			if open--; open == 0 {
				return i
			}
		}
	}
}
