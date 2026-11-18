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
// ...). In a command it reads only installs: a word "pip install" or "npm add" in an echo, a
// comment or a commit message is not one, nor is an address that is not given to an installer.
// Like the other rules it is a tripwire: a variable for the address, a here-document or Python's
// subprocess list get past it.
//
// SPDX-License-Identifier: Apache-2.0
package gateway

import (
	"path"
	"regexp"
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

// installEnvRe finds an assignment of an install-source variable anywhere in shell text, its value
// whole even when quoted: GOPROXY="https://a|https://b" holds a '|' that installSeps would split
// (code review, 2026-11-18). Also PowerShell's $env:GOPROXY="…".
var installEnvRe = regexp.MustCompile(`(?i)\b(GOPROXY|PIP_INDEX_URL|PIP_EXTRA_INDEX_URL|PIP_FIND_LINKS|UV_INDEX_URL|UV_EXTRA_INDEX_URL|UV_INDEX|UV_DEFAULT_INDEX|UV_FIND_LINKS|NPM_CONFIG_REGISTRY|YARN_REGISTRY|YARN_NPM_REGISTRY_SERVER|CARGO_REGISTRIES_\w+_INDEX)\s*=\s*("[^"]*"|'[^']*'|[^\s;&|()]+)`)

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

// installSources returns the install sources a simple command names (words from installWords):
// environment assignments before it or exported, and the sources of a package manager's install.
func installSources(w []string) []string {
	var out []string
	export := false
	for len(w) > 0 {
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
				return append(out, installSources(append([]string{"pip"}, args[i+1:]...))...)
			}
			if a == "-m" && i+1 < len(args) && args[i+1] == "pip" {
				return append(out, installSources(append([]string{"pip"}, args[i+2:]...))...)
			}
		}
	case pipName.MatchString(name), name == "pipx":
		if hasWord(args, pipVerbs) {
			return urls()
		}
		out = append(out, flagValues(args, pipFlags)...)
	case name == "uv" || name == "uvx":
		verb, rest := firstVerb(args)
		switch {
		case name == "uv" && verb == "pip":
			return append(out, installSources(append([]string{"pip"}, rest...))...)
		case name == "uv" && uvVerbs[verb]:
			return urls()
		case name == "uv" && verb == "tool":
			if v, _ := firstVerb(rest); uvVerbs[v] {
				return urls()
			}
		}
		out = append(out, flagValues(args, uvRunFlags)...)
	case npmNames[name]:
		out = append(out, flagValues(args, []string{"--registry"})...)
		verb, rest := npmVerb(args)
		if verb == "workspace" && len(rest) > 1 { // yarn workspace <name> add …
			verb, rest = npmVerb(rest[1:])
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
		if v, _ := firstVerb(args); cargoVerbs[v] { // not "cargo run -- install …"
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
		if v, _ := firstVerb(args); gemVerbs[v] {
			return urls()
		}
	case condaNames[name]:
		if v, _ := firstVerb(args); condaVerbs[v] {
			return urls()
		}
		out = append(out, flagValues(args, []string{"-c", "--channel"})...)
	}
	// "uv run pip install …", "poetry run pip install …": the command after run.
	if v, _ := firstVerb(args); runNames[name] && v == "run" {
		for i := 1; i < len(args); i++ {
			if n := commandName(args[i]); pipName.MatchString(n) || pythonName.MatchString(n) || npmNames[n] {
				return append(out, installSources(args[i:])...)
			}
		}
	}
	return out
}

// npmValueFlags take the next word as their value when given without "=".
var npmValueFlags = wordSet("--registry", "--prefix", "-C", "--workspace", "-w", "--userconfig", "--cache", "--loglevel", "--filter", "-F", "--dir", "--cwd")

// npmVerb is npm's, pnpm's, yarn's or bun's subcommand: the first word that is neither a flag nor
// a flag's value (not "update" in "npm run update -- …"), lower case, and the words after it.
func npmVerb(args []string) (string, []string) {
	for i := 0; i < len(args); i++ {
		if strings.HasPrefix(args[i], "-") {
			if npmValueFlags[args[i]] {
				i++
			}
			continue
		}
		return strings.ToLower(args[i]), args[i+1:]
	}
	return "", nil
}

// firstVerb is the first word of args that is not a flag, and the words after it.
func firstVerb(args []string) (string, []string) {
	for i, a := range args {
		if !strings.HasPrefix(a, "-") {
			return a, args[i+1:]
		}
	}
	return "", nil
}

// Kinds of package source files (installConfigFile).
const (
	configNone  = iota
	configLines // every address outside comments is a source (.npmrc, pip.conf, requirements.txt)
	configJSON  // npm's package files: every "key": "value" address but the project's own links
)

// installConfigFile: how a package manager reads install sources from the file at p.
func installConfigFile(p string) int {
	p = strings.ToLower(strings.ReplaceAll(p, `\`, "/"))
	base := path.Base(p)
	switch base {
	case ".npmrc", "npmrc", ".yarnrc", ".yarnrc.yml", "pip.conf", "pip.ini", ".condarc", "condarc", ".gemrc", "uv.toml",
		"yarn.lock", "pnpm-lock.yaml":
		return configLines
	case "package.json", "package-lock.json", "npm-shrinkwrap.json":
		return configJSON
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

// jsonPairRe is a "key": "value" pair of JSON text, a whole file or an edit's piece of one.
var jsonPairRe = regexp.MustCompile(`"((?:[^"\\]|\\.)*)"\s*:\s*"((?:[^"\\]|\\.)*)"`)

// npmLinkKeys hold the project's own links in npm's package files, which npm never installs from.
var npmLinkKeys = wordSet("homepage", "url", "funding", "$schema", "bugs", "repository", "author", "email", "web")

// jsonSources are the values of npm package-file text that npm can install from: dependency specs
// ("x": "https://…/x.tgz", "git+https://…"), "resolved" and "version" addresses of a lock file.
func jsonSources(text string) []string {
	var out []string
	for _, m := range jsonPairRe.FindAllStringSubmatch(text, -1) {
		if !npmLinkKeys[strings.ToLower(m[1])] {
			out = append(out, m[2])
		}
	}
	return out
}

// installSourceHit: the call installs from, or makes a package manager use, a source whose host is
// not known (see the top of this file). An install source is a word with "://"; the host after it
// is checked as unknownHost checks an address, whatever the scheme (git+https, sparse+https). All
// sources go to one unknownHost call, which looks each host up in the request once: the check
// stays linear in the call's size however many sources it names.
func installSourceHit(name, args, req string, known knownAddrs) bool {
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
	for _, m := range installEnvRe.FindAllStringSubmatch(cmds, -1) {
		add(envSources(m[1], unquote.Replace(m[2])))
	}
	if vals, ok := argValues(args); ok {
		lines, js := false, false
		for _, k := range []string{"filePath", "path", "file_path"} {
			for _, p := range vals[k] {
				switch installConfigFile(p) {
				case configLines:
					lines = true
				case configJSON:
					js = true
				}
			}
		}
		for _, k := range []string{"content", "newString", "new_string"} {
			for _, c := range vals[k] {
				if lines {
					add(configSources(c))
				}
				if js {
					add(jsonSources(c))
				}
			}
		}
	}
	if srcs.Len() == 0 {
		return false
	}
	return unknownHost(srcs.String(), req, append(known[:len(known):len(known)], registryAddrs...)) != ""
}
