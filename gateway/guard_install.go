// The install-source rule: a package install from an index, registry or other source whose host
// is unknown blocks and holds the node, like download-and-execute. An unknown address alone only
// flags (guard.go), but installing from a node-chosen index runs that index's build and lifecycle
// scripts on the client (setup.py, npm's install scripts, build.rs) without any pattern of
// guardRules (code review, 2026-11-18). Known: in the request, a guard_known_addresses source, a
// local or private address, or one of the public registries a package manager uses by default
// (registryAddrs), which a node gains nothing by naming.
//
// The rule reads shell commands and scripts as commandRules do (commandText), and the text a write
// or edit tool puts into a package manager's own source file (.npmrc, pip.conf, requirements.txt,
// ...). In a command it reads only installs and the variables installers read (PIP_INDEX_URL=…): a
// word "pip install" or "npm add" in an echo, a comment or a commit message is not one, nor is an
// assignment printed or commented out, nor an address that is not given to an installer. Like the
// other rules it is a tripwire: a variable for the address, a here-document or Python's subprocess
// list get past it.
//
// SPDX-License-Identifier: Apache-2.0
package gateway

import (
	"encoding/json"
	"path"
	"regexp"
	"strconv"
	"strings"
)

const (
	ruleInstallSource = "install-source"
	whyInstallSource  = "a package install from an unknown index"
)

// registryDefaults are the public registries package managers use by default, and PyTorch's wheel
// index, which install guides name: known sources for this rule only.
var registryDefaults = []string{
	"pypi.org",
	"files.pythonhosted.org",
	"download.pytorch.org",
	"registry.npmjs.org",
	"registry.yarnpkg.com",
	"proxy.golang.org",
	"rubygems.org",
	"crates.io",
	"conda.anaconda.org",
	"repo.anaconda.com",
}

var registryAddrs = func() knownAddrs {
	k, err := parseKnownAddrs(registryDefaults)
	if err != nil {
		panic(err)
	}
	return k
}()

// installSeps end a simple command: a line, ; & && | || ( ) { } and a backtick. A quote does not
// protect them: `sh -c "pip install …; ls"` is two commands for the rule, as it is for the shell
// that sh starts.
var installSeps = regexp.MustCompile("&&|\\|\\||[\n;&|(){}`]")

var unquote = strings.NewReplacer(`"`, "", "'", "", `\`, "")

// installWords are a simple command's words with quotes and backslashes taken out (the shell joins
// "ht"'tp's://… into one word), up to a comment.
func installWords(seg string) []string {
	var out []string
	for _, w := range strings.Fields(seg) {
		if strings.HasPrefix(w, "#") {
			break
		}
		if w = unquote.Replace(w); w != "" {
			out = append(out, w)
		}
	}
	return out
}

var assignRe = regexp.MustCompile(`^([A-Za-z_][A-Za-z0-9_]*)=(.*)$`)

// installEnv are environment variables that name an install source, upper case.
var installEnv = map[string]bool{
	"PIP_INDEX_URL": true, "PIP_EXTRA_INDEX_URL": true, "PIP_FIND_LINKS": true,
	"UV_INDEX_URL": true, "UV_EXTRA_INDEX_URL": true, "UV_INDEX": true, "UV_DEFAULT_INDEX": true, "UV_FIND_LINKS": true,
	"NPM_CONFIG_REGISTRY": true, "YARN_REGISTRY": true, "YARN_NPM_REGISTRY_SERVER": true,
}

// envSources: the sources an assignment of name sets. Go adds "https://" to a GOPROXY entry
// without a scheme.
func envSources(name, val string) []string {
	n := strings.ToUpper(name)
	switch {
	case n == "GOPROXY":
		var out []string
		for _, e := range strings.FieldsFunc(val, func(r rune) bool { return r == ',' || r == '|' }) {
			if e != "direct" && e != "off" && !strings.Contains(e, "://") && !strings.HasPrefix(e, "/") {
				e = "https://" + e
			}
			out = append(out, e)
		}
		return out
	case installEnv[n] || (strings.HasPrefix(n, "CARGO_REGISTRIES_") && strings.HasSuffix(n, "_INDEX")):
		return []string{val}
	}
	return nil
}

// commandName is a command word without its directory and ".exe", lower case.
func commandName(w string) string {
	w = strings.ToLower(w)
	if i := strings.LastIndexAny(w, `/\`); i >= 0 {
		w = w[i+1:]
	}
	return strings.TrimSuffix(w, ".exe")
}

var (
	pipName    = regexp.MustCompile(`^pip[0-9.]*$`)
	pythonName = regexp.MustCompile(`^(?:python[0-9.]*|py)$`)
)

func wordSet(ws ...string) map[string]bool {
	m := map[string]bool{}
	for _, w := range ws {
		m[w] = true
	}
	return m
}

// Install verbs per package manager: with one of them every address in the command is a source.
// Commands that run a package (npx, uvx, uv run) take only their source flags' values: their other
// arguments go to the package.
var (
	pipVerbs   = wordSet("install", "download", "wheel", "config", "sync", "compile", "inject", "upgrade")
	uvVerbs    = wordSet("add", "sync", "lock", "install", "upgrade")
	npmNames   = wordSet("npm", "pnpm", "yarn", "bun")
	npmVerbs   = wordSet("install", "i", "in", "ins", "inst", "insta", "instal", "isnt", "isnta", "isntal", "isntall", "add", "ci", "clean-install", "ic", "install-clean", "isntall-clean", "install-test", "it", "cit", "clean-install-test", "update", "up", "upgrade", "udpate")
	cargoVerbs = wordSet("install", "add")
	gemVerbs   = wordSet("install", "i", "update", "sources")
	condaNames = wordSet("conda", "mamba", "micromamba")
	condaVerbs = wordSet("install", "create", "update", "upgrade", "config", "env")
	runNames   = wordSet("uv", "poetry", "pipenv", "pdm", "hatch", "rye", "conda", "mamba", "micromamba")
	pipFlags   = []string{"--index-url", "-i", "--extra-index-url", "--find-links", "-f", "--index", "--default-index"}
	uvRunFlags = append(pipFlags[:len(pipFlags):len(pipFlags)], "--with", "--from")
	npxFlags   = []string{"--registry", "--package", "-p"}
)

// flagValues are the values of flags in args: "--flag v", "--flag=v", and "-fv" for a short flag.
func flagValues(args []string, flags []string) []string {
	var out []string
	for i, a := range args {
		for _, f := range flags {
			switch {
			case a == f && i+1 < len(args):
				out = append(out, args[i+1])
			case strings.HasPrefix(a, f+"="):
				out = append(out, a[len(f)+1:])
			case len(f) == 2 && len(a) > 2 && strings.HasPrefix(a, f) && !strings.HasPrefix(a, "--"):
				out = append(out, a[2:])
			}
		}
	}
	return out
}

func hasWord(args []string, set map[string]bool) bool {
	for _, a := range args {
		if set[strings.ToLower(a)] {
			return true
		}
	}
	return false
}

// installSources returns the install sources a simple command names (words from installWords or
// shellCommands): environment assignments before it or exported, and the sources of a package
// manager's install.
func installSources(w []string) []string { return installSourcesIn(w, 0) }

// maxShellDepth bounds how deep "sh -c" scripts are read inside each other: each level reads the
// script again.
const maxShellDepth = 4

func installSourcesIn(w []string, depth int) []string {
	var out []string
	export := false
	for len(w) > 0 {
		if a, n := psAssign(w); n > 0 {
			if m := assignRe.FindStringSubmatch(a); m != nil {
				out = append(out, envSources(m[1], m[2])...)
			}
			w = w[n:]
			continue
		}
		if m := assignRe.FindStringSubmatch(w[0]); m != nil {
			out = append(out, envSources(m[1], m[2])...)
			w = w[1:]
			continue
		}
		if export {
			w = w[1:] // export -n, declare -x
			continue
		}
		switch commandName(w[0]) {
		case "export", "declare", "typeset", "local", "readonly", "setenv":
			export = true
			w = w[1:]
			continue
		case "sudo", "exec", "env", "nohup", "command", "time", "nice", "then", "do", "else", "if", "while", "until", "!":
			w = w[1:]
			for len(w) > 0 && strings.HasPrefix(w[0], "-") {
				w = w[1:]
			}
			continue
		case "timeout":
			w = w[1:]
			for len(w) > 0 && strings.HasPrefix(w[0], "-") {
				w = w[1:]
			}
			if len(w) > 0 {
				w = w[1:]
			}
			continue
		case "sh", "bash", "zsh", "dash", "ksh":
			i := 1
			for i < len(w) && strings.HasPrefix(w[i], "-") && w[i] != "-c" {
				i++
			}
			if i < len(w) && w[i] == "-c" {
				// One word holding the whole script (shellCommands): read it as shell text.
				if i+1 < len(w) && strings.ContainsAny(w[i+1], " \t\n;&|(){}`") {
					if depth < maxShellDepth {
						for _, c := range shellCommands(w[i+1]) {
							out = append(out, installSourcesIn(c.words, depth+1)...)
						}
					}
					return out
				}
				w = w[i+1:]
				continue
			}
		}
		break
	}
	if len(w) == 0 {
		return out
	}
	name, args := commandName(w[0]), w[1:]
	urls := func() []string { return append(out, args...) } // every word: only those with "://" count
	switch {
	case pythonName.MatchString(name):
		for i, a := range args {
			if a == "-mpip" {
				return append(out, installSourcesIn(append([]string{"pip"}, args[i+1:]...), depth)...)
			}
			if a == "-m" && i+1 < len(args) && args[i+1] == "pip" {
				return append(out, installSourcesIn(append([]string{"pip"}, args[i+2:]...), depth)...)
			}
		}
	case pipName.MatchString(name), name == "pipx":
		if hasWord(args, pipVerbs) {
			return urls()
		}
		out = append(out, flagValues(args, pipFlags)...)
	case name == "uv" || name == "uvx":
		verb, rest := subcommand(args, uvCmds)
		switch {
		case name == "uv" && verb == "pip":
			return append(out, installSourcesIn(append([]string{"pip"}, rest...), depth)...)
		case name == "uv" && uvVerbs[verb]:
			return urls()
		case name == "uv" && verb == "tool":
			if v, _ := subcommand(rest, uvCmds); uvVerbs[v] {
				return urls()
			}
		}
		out = append(out, flagValues(args, uvRunFlags)...)
	case npmNames[name]:
		out = append(out, flagValues(args, []string{"--registry"})...)
		verb, rest := subcommand(args, npmCmds)
		if verb == "workspace" && len(rest) > 1 { // yarn workspace <name> add …
			verb, rest = subcommand(rest[1:], npmCmds)
		}
		if npmVerbs[verb] {
			return urls()
		}
		if verb == "set" || verb == "config" && hasWord(rest, wordSet("set")) {
			for _, a := range rest {
				if strings.Contains(strings.ToLower(a), "registry") {
					return urls()
				}
			}
		}
	case name == "npx" || name == "pnpx" || name == "bunx":
		return append(out, flagValues(args, npxFlags)...)
	case name == "cargo":
		if v, _ := subcommand(args, cargoCmds); cargoVerbs[v] { // not "cargo run -- install …"
			return urls()
		}
		out = append(out, flagValues(args, []string{"--index"})...)
	case name == "go" && len(args) > 1 && args[0] == "env" && args[1] == "-w":
		for _, a := range args[2:] {
			if m := assignRe.FindStringSubmatch(a); m != nil {
				out = append(out, envSources(m[1], m[2])...)
			}
		}
	case name == "gem":
		if v, _ := subcommand(args, gemCmds); gemVerbs[v] {
			return urls()
		}
	case condaNames[name]:
		if v, _ := subcommand(args, condaCmds); condaVerbs[v] {
			return urls()
		}
		out = append(out, flagValues(args, []string{"-c", "--channel"})...)
	}
	// "uv run pip install …", "poetry run pip install …": the command after run.
	if v, rest := subcommand(args, runCmds[name]); runNames[name] && v == "run" {
		for i := range rest {
			if n := commandName(rest[i]); pipName.MatchString(n) || pythonName.MatchString(n) || npmNames[n] {
				return append(out, installSourcesIn(rest[i:], depth)...)
			}
		}
	}
	return out
}

// Subcommands of the package managers, install or not: a word that follows a flag is the flag's
// value unless it is one of them (subcommand).
var (
	npmCmds = func() map[string]bool {
		m := wordSet("run", "run-script", "rum", "urn", "exec", "x", "dlx", "test", "t", "tst", "start", "stop", "restart",
			"pkg", "config", "c", "set", "get", "workspace", "workspaces", "why", "ls", "list", "view", "info", "publish",
			"pack", "init", "create", "audit", "outdated", "remove", "rm", "uninstall", "un", "unlink", "link", "cache",
			"dedupe", "prune", "rebuild", "help", "version")
		for v := range npmVerbs {
			m[v] = true
		}
		return m
	}()
	uvCmds = wordSet("pip", "add", "sync", "lock", "install", "upgrade", "tool", "run", "remove", "venv", "python",
		"init", "export", "tree", "build", "publish", "cache", "self", "version", "help", "format")
	cargoCmds = wordSet("install", "add", "run", "r", "build", "b", "check", "c", "test", "t", "bench", "fetch", "update",
		"publish", "search", "new", "init", "clean", "doc", "d", "tree", "remove", "rm", "vendor", "metadata", "fmt",
		"clippy", "help", "login", "owner", "yank", "package", "generate-lockfile", "fix", "uninstall", "version")
	gemCmds = wordSet("install", "i", "update", "sources", "uninstall", "list", "build", "push", "fetch", "help", "query",
		"search", "info", "cleanup", "env", "contents", "pristine", "specification", "which", "owner", "outdated")
	condaCmds = wordSet("install", "create", "update", "upgrade", "config", "env", "run", "remove", "uninstall", "list",
		"activate", "deactivate", "info", "search", "clean", "init", "compare", "doctor", "export", "notices", "package", "rename")
	runCmds = map[string]map[string]bool{"uv": uvCmds, "conda": condaCmds, "mamba": condaCmds, "micromamba": condaCmds}
)

// subcommand is a command's subcommand in args, lower case, and the words after it: the first word
// that is not a flag (nor a Rustup "+toolchain"). A word right after a flag given without "=" may be
// that flag's value ("npm --tag beta install …", "uv --directory app add …"): it is the subcommand
// only if it is one of cmds, else the next such word is (code review, 2026-11-18). So "npm run
// update -- …" is "run".
func subcommand(args []string, cmds map[string]bool) (string, []string) {
	if cmds == nil {
		cmds = wordSet("run")
	}
	afterFlag := false
	for i, a := range args {
		switch {
		case strings.HasPrefix(a, "+"):
			afterFlag = false
		case strings.HasPrefix(a, "-") && len(a) > 1:
			afterFlag = a != "--" && !strings.Contains(a, "=")
		case afterFlag && !cmds[strings.ToLower(a)]:
			afterFlag = false
		default:
			return strings.ToLower(a), args[i+1:]
		}
	}
	return "", nil
}

// Kinds of package source files (installConfigFile).
const (
	configNone    = iota
	configLines   // every address outside comments is a source (.npmrc, pip.conf, requirements.txt)
	configPackage // package.json: every address but the project's own links at the top level
	configLock    // package-lock.json: every address but package metadata (packageLockSources)
)

// installConfigFile: how a package manager reads install sources from the file at p.
func installConfigFile(p string) int {
	p = strings.ToLower(strings.ReplaceAll(p, `\`, "/"))
	base := path.Base(p)
	switch base {
	case ".npmrc", "npmrc", ".yarnrc", ".yarnrc.yml", "pip.conf", "pip.ini", ".condarc", "condarc", ".gemrc", "uv.toml",
		"yarn.lock", "pnpm-lock.yaml":
		return configLines
	case "package.json":
		return configPackage
	case "package-lock.json", "npm-shrinkwrap.json":
		return configLock
	}
	if strings.HasSuffix(p, ".cargo/config") || strings.HasSuffix(p, ".cargo/config.toml") {
		return configLines
	}
	if (strings.HasPrefix(base, "requirements") || strings.HasPrefix(base, "constraints")) &&
		(strings.HasSuffix(base, ".txt") || strings.HasSuffix(base, ".in")) {
		return configLines
	}
	return configNone
}

// configWords are the words of a source file's text with quotes taken out, but not backslashes:
// the file is no shell text, and a client may read "\\" in an address as '/' (WHATWG URLs).
var configWords = strings.NewReplacer(`"`, " ", "'", " ")

// configSources are the words of a package manager's source file text, without comment lines and
// comments after " #".
func configSources(text string) []string {
	var out []string
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
			continue
		}
		if i := strings.Index(line, " #"); i >= 0 {
			line = line[:i]
		}
		out = append(out, strings.Fields(configWords.Replace(line))...)
	}
	return out
}

// unescapeText decodes the escapes of JSON, TOML and YAML strings anywhere in text (\/, \\, \",
// \uXXXX, \UXXXXXXXX, \xXX); other backslashes stay. A client decodes "https:\/\/…" or "\u002f" in
// a quoted value to an address the raw text hides (code review, 2026-11-18), so the rule reads a
// source file's text both raw and decoded.
func unescapeText(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] != '\\' || i+1 == len(s) {
			b.WriteByte(s[i])
			continue
		}
		switch c := s[i+1]; c {
		case '/', '\\', '"':
			b.WriteByte(c)
			i++
			continue
		case 'u', 'U', 'x':
			n := map[byte]int{'u': 4, 'U': 8, 'x': 2}[c]
			if i+2+n <= len(s) {
				if r, err := strconv.ParseUint(s[i+2:i+2+n], 16, 32); err == nil {
					b.WriteRune(rune(r))
					i += 1 + n
					continue
				}
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// The keys of npm's package files that npm never installs from (code review, 2026-11-18: they
// count only where npm reads them as such, never inside a dependency map, where a package may be
// named "url" or "repository"). pkgMetaKeys: the top level of package.json, the project's own
// description and links. lockTopKeys and lockEntryKeys: the top level of a lock file and a
// package's entry in it ("funding" links of every package; "resolved" and "version" are read).
var (
	pkgMetaKeys = wordSet("name", "version", "description", "keywords", "homepage", "bugs", "license", "licenses",
		"author", "contributors", "maintainers", "funding", "repository", "$schema", "readme", "private")
	lockTopKeys   = wordSet("name", "version", "lockfileVersion", "requires")
	lockEntryKeys = wordSet("name", "license", "funding", "engines", "bin", "integrity", "deprecated", "cpu", "os",
		"libc", "dev", "optional", "devOptional", "peer", "inBundle", "extraneous", "hasInstallScript", "hasShrinkwrap")
)

// packageJSONSources are the strings of a whole package.json (decoded JSON) npm can install from:
// all but those of pkgMetaKeys at its top level. Dependency maps, overrides, resolutions and
// everything else are read whole.
func packageJSONSources(v any) []string {
	top, ok := v.(map[string]any)
	if !ok {
		return stringsIn(v)
	}
	var out []string
	for k, x := range top {
		if !pkgMetaKeys[k] {
			out = appendStrings(out, x)
		}
	}
	return out
}

// packageLockSources are the strings of a whole package-lock.json or npm-shrinkwrap.json (decoded
// JSON) npm can install from: every string but those of lockTopKeys at the top level and of
// lockEntryKeys in a package's entry ("packages", and "dependencies" of lock version 1, which
// nests).
func packageLockSources(v any) []string {
	top, ok := v.(map[string]any)
	if !ok {
		return stringsIn(v)
	}
	var out []string
	for k, x := range top {
		switch {
		case k == "packages" || k == "dependencies":
			out = lockDeps(out, x)
		case !lockTopKeys[k]:
			out = appendStrings(out, x)
		}
	}
	return out
}

// lockDeps appends what a map of a lock file holds to out: an object value is a package's entry,
// any other value a dependency spec. One slice for all levels: a lock file of version 1 nests
// entries as deep as JSON allows, and copying what each level found would cost the depth times the
// file (code review, 2026-11-18).
func lockDeps(out []string, v any) []string {
	m, ok := v.(map[string]any)
	if !ok {
		return appendStrings(out, v)
	}
	for _, x := range m {
		e, ok := x.(map[string]any)
		if !ok {
			out = appendStrings(out, x)
			continue
		}
		for k, y := range e {
			switch {
			case k == "dependencies":
				out = lockDeps(out, y)
			case !lockEntryKeys[k]:
				out = appendStrings(out, y)
			}
		}
	}
	return out
}

// shellCommand is a simple command of shell text: its words as the shell makes them, and whether
// its output goes into a pipe.
type shellCommand struct {
	words []string
	piped bool
}

// shellCommands splits shell text into simple commands as the shell does, unlike installSeps:
// quotes and backslashes join a word, separators (a line end, ; & | ( ) { } and a backtick) end a
// command only outside quotes, and '#' at the start of a word comments out the rest of the line.
// So GOPROXY="https://a|https://b" is one word, and an assignment in a comment none.
func shellCommands(s string) []shellCommand {
	var (
		out   []shellCommand
		words []string
		w     strings.Builder
		inW   bool
	)
	endWord := func() {
		if inW {
			words = append(words, w.String())
			w.Reset()
			inW = false
		}
	}
	endCmd := func(piped bool) {
		endWord()
		if len(words) > 0 {
			out = append(out, shellCommand{words, piped})
			words = nil
		}
	}
	for i := 0; i < len(s); i++ {
		switch c := s[i]; {
		case c == '\\' && i+1 < len(s):
			i++
			w.WriteByte(s[i])
			inW = true
		case c == '\'':
			j := strings.IndexByte(s[i+1:], '\'')
			if j < 0 {
				j = len(s) - i - 1
			}
			w.WriteString(s[i+1 : i+1+j])
			i += j + 1
			inW = true
		case c == '"':
			for i++; i < len(s) && s[i] != '"'; i++ {
				if s[i] == '\\' && i+1 < len(s) && strings.IndexByte("\"\\$`", s[i+1]) >= 0 {
					i++
				}
				w.WriteByte(s[i])
			}
			inW = true
		case c == '#' && !inW:
			if j := strings.IndexByte(s[i:], '\n'); j >= 0 {
				i += j - 1
			} else {
				i = len(s)
			}
		case c == ' ' || c == '\t' || c == '\r':
			endWord()
		case c == '|' && !(i+1 < len(s) && s[i+1] == '|') && !(i > 0 && s[i-1] == '|'):
			endCmd(true)
		case strings.IndexByte("\n;&|(){}`", c) >= 0:
			endCmd(false)
		default:
			w.WriteByte(c)
			inW = true
		}
	}
	endCmd(false)
	return out
}

// psAssign reads PowerShell's $env:NAME=value at the start of w, also with spaces around '=': the
// assignment as NAME=value and how many words it took (0: none).
func psAssign(w []string) (string, int) {
	if len(w) == 0 || !strings.HasPrefix(strings.ToLower(w[0]), "$env:") {
		return "", 0
	}
	a, n := w[0][len("$env:"):], 1
	if !strings.Contains(a, "=") && n < len(w) && strings.HasPrefix(w[n], "=") {
		a += w[n]
		n++
	}
	if strings.HasSuffix(a, "=") && n < len(w) {
		a += w[n]
		n++
	}
	return a, n
}

// envAssigned are the install sources set by assignment words anywhere in a command (shell words
// from shellCommands): before it, exported, or passed on ("make PIP_INDEX_URL=… install", "env",
// "docker run -e", "--env=PIP_INDEX_URL=…"). Not in a comment, and not printed: an echo or printf only counts when its
// output goes to a file or a pipe ("echo 'export PIP_INDEX_URL=…' >> ~/.bashrc"), and then its
// words are read as shell text (code review, 2026-11-18: printing an unfamiliar PIP_INDEX_URL held
// a node).
func envAssigned(c shellCommand, depth int) []string {
	w := c.words
	if n := commandName(w[0]); n == "echo" || n == "printf" {
		redirected := c.piped
		for _, x := range w[1:] {
			redirected = redirected || strings.Contains(x, ">")
		}
		var out []string
		if redirected && depth < maxShellDepth {
			for _, sc := range shellCommands(strings.Join(w[1:], " ")) {
				out = append(out, envAssigned(sc, depth+1)...)
			}
		}
		return out
	}
	var out []string
	for i := 0; i < len(w); i++ {
		if a, n := psAssign(w[i:]); n > 0 {
			if m := assignRe.FindStringSubmatch(a); m != nil {
				out = append(out, envSources(m[1], m[2])...)
			}
			i += n - 1
			continue
		}
		a := w[i]
		if _, v, ok := strings.Cut(a, "="); ok && strings.HasPrefix(a, "-") {
			a = v
		}
		if m := assignRe.FindStringSubmatch(a); m != nil {
			out = append(out, envSources(m[1], m[2])...)
		}
	}
	return out
}

// editTexts are the new texts of a call's edits: "newString", "new_string", and those of an
// "edits" list (a multi-edit tool).
func editTexts(args string, vals map[string][]string) []string {
	out := append(vals["newString"][:len(vals["newString"]):len(vals["newString"])], vals["new_string"]...)
	if len(vals["edits"]) == 0 {
		return out
	}
	var v struct {
		Edits []map[string]any `json:"edits"`
	}
	if json.Unmarshal([]byte(args), &v) != nil {
		return append(out, vals["edits"]...) // every string, the old ones too
	}
	for _, e := range v.Edits {
		for _, k := range []string{"newString", "new_string"} {
			out = appendStrings(out, e[k])
		}
	}
	return out
}

// installSourceHit: the call installs from, or makes a package manager use, a source whose host is
// not known (see the top of this file). An install source is a word with "://"; the host after it
// is checked as unknownHost checks an address, whatever the scheme (git+https, sparse+https). All
// sources go to one unknownHost call, which looks each host up in the request once: the check
// stays linear in the call's size however many sources it names.
//
// Shell text is read twice: split at every separator, quoted or not (installSeps: "sh -c" and
// Python's os.system strings hold commands), and as the shell splits it (shellCommands: a quoted
// value may hold a separator). A package source file's text is read raw and with its escapes
// decoded (unescapeText). A whole package.json or lock file that is valid JSON is decoded and read
// by its structure; any other text of these files, such as an edit's new text, has no context
// that shows it is a project's own link, so every address in it counts (code review, 2026-11-18:
// an edit of a dependency's address alone holds no key).
//
// It returns the unknown host and the source that names it ("" if none), for the evidence.
func installSourceHit(name, args, req string, known knownAddrs) (host, src string) {
	var srcs strings.Builder
	add := func(words []string) {
		for _, w := range words {
			if i := strings.Index(w, "://"); i >= 0 {
				srcs.WriteString("https://")
				srcs.WriteString(w[i+3:])
				srcs.WriteByte(' ')
			}
		}
	}
	cmds := commandText(name, args)
	for _, seg := range installSeps.Split(cmds, -1) {
		add(installSources(installWords(seg)))
	}
	for _, c := range shellCommands(cmds) {
		add(installSources(c.words))
		add(envAssigned(c, 0))
	}
	if vals, ok := argValues(args); ok {
		var lines, pkg, lock bool
		for _, k := range []string{"filePath", "path", "file_path"} {
			for _, p := range vals[k] {
				switch installConfigFile(p) {
				case configLines:
					lines = true
				case configPackage:
					pkg = true
				case configLock:
					lock = true
				}
			}
		}
		read := func(c string, whole bool) {
			texts := []string{c}
			if u := unescapeText(c); u != c {
				texts = append(texts, u)
			}
			if lines {
				for _, t := range texts {
					add(configSources(t))
				}
			}
			if !pkg && !lock {
				return
			}
			var v any
			if whole && json.Unmarshal([]byte(c), &v) == nil {
				if pkg {
					add(packageJSONSources(v))
				}
				if lock {
					add(packageLockSources(v))
				}
				return
			}
			for _, t := range texts {
				add(strings.Fields(t))
			}
		}
		if lines || pkg || lock {
			for _, c := range vals["content"] {
				read(c, true)
			}
			for _, c := range editTexts(args, vals) {
				read(c, false)
			}
		}
	}
	if srcs.Len() == 0 {
		return "", ""
	}
	all := srcs.String()
	host, at := unknownAddr(all, req, append(known[:len(known):len(known)], registryAddrs...))
	if host == "" {
		return "", ""
	}
	start := strings.LastIndexByte(all[:at[0]], ' ') + 1 // the source's word in all
	end := at[1] + strings.IndexByte(all[at[1]:], ' ')   // every source ends with a space
	return host, all[start:end]
}
