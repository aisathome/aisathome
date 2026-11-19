// Official installers: a download piped into a shell is how rustup, uv, nvm, Deno, Bun, Homebrew
// and Lean's elan tell people to install them, and a model suggests just that. Such an answer from
// an untrusted node is still stopped (and retried on a trusted node when nothing went out yet), but
// the node is not held: a hold for an official installer cost half the network's capacity for
// 3.5 hours (2026-11-19).
//
// The answer is stopped all the same because the guard reads one command, not what the installer
// does with its arguments and environment (RUSTUP_DIST_SERVER, an installer's own download URL),
// and a node that reads the list can build a command from it. So the list decides only whether a
// node is held, never what reaches a client: an entry wrongly listed costs a hold, not a client.
//
// A command counts as an official install only if every word of the download command is either an
// https:// address of the list (one plain shell word, as for guard_known_addresses) or one of a few
// flags that only quieten or follow redirects (installerFlags); any other word (a second address,
// a proxy, a config file, -k) and the node is held as before. Read for the first two patterns of
// download-and-execute: "curl … | sh" and "sh -c "$(curl …)"" (or "bash <(curl …)").
//
// SPDX-License-Identifier: Apache-2.0
package gateway

import (
	"regexp"
	"strings"
)

// defaultInstallers are the install scripts of the projects' own install pages, as host or
// host/path entries (parseKnownAddrs; a path entry matches that path and below):
//   - https://rust-lang.org/tools/install (rustup: https://sh.rustup.rs)
//   - https://docs.astral.sh/uv/getting-started/installation/ (https://astral.sh/uv/install.sh, also
//     versioned as /uv/<version>/install.sh), and ruff's https://astral.sh/ruff/install.sh
//   - https://github.com/nvm-sh/nvm#installing-and-updating (raw.githubusercontent.com/nvm-sh/nvm/…)
//   - https://docs.deno.com/runtime/getting_started/installation/ (https://deno.land/install.sh, .ps1)
//   - https://bun.sh/docs/installation (https://bun.sh/install, https://bun.sh/install.ps1)
//   - https://brew.sh (raw.githubusercontent.com/Homebrew/install/HEAD/install.sh; GitHub's owner and
//     repository names do not depend on case, and paths compare in lower case)
//   - https://lean-lang.org/install/manual/ (elan: https://elan.lean-lang.org/elan-init.sh, and the
//     older raw.githubusercontent.com/leanprover/elan/master/elan-init.sh)
//   - https://python-poetry.org/docs/#installing-with-the-official-installer (https://install.python-poetry.org)
//
// Config.GuardOfficialInstallers replaces the list.
var defaultInstallers = []string{
	"sh.rustup.rs",
	"astral.sh/uv",
	"astral.sh/ruff",
	"raw.githubusercontent.com/nvm-sh/nvm",
	"deno.land/install.sh",
	"deno.land/install.ps1",
	"bun.sh/install",
	"bun.sh/install.ps1",
	"raw.githubusercontent.com/homebrew/install",
	"elan.lean-lang.org",
	"raw.githubusercontent.com/leanprover/elan",
	"install.python-poetry.org",
}

// installerFlags are the flags an official install command may give its downloader, lower case:
// output to standard output ("-o -", "-O-"; any other value of -o is no address, so the command
// is not one), quiet, fail on errors, follow redirects, https only (curl's
// "--proto '=https' --tlsv1.2", as rustup's page writes it) and PowerShell's -UseBasicParsing.
// installerShort: curl's short flags of these, combined ("-fsSL", "-LsSf", "-sSf").
var (
	installerFlags = wordSet("-o-", "-qo-", "-o", "-", "--proto", "=https", "--proto==https", "--tlsv1.2", "--tlsv1.3",
		"--fail", "--silent", "--show-error", "--location", "--quiet", "-q", "-useb", "-usebasicparsing")
	installerShort = regexp.MustCompile(`^-[fsSL]+$`)
	// downloadWord is the downloader a shell runs in "sh -c "$(curl …)"": the match ends with it.
	downloadWord = regexp.MustCompile(`(?i)(?:curl|wget|fetch)$`)
)

// officialInstall: the download-and-execute match text[m0:m1] runs only installers of the list
// (see the top of this file).
func officialInstall(text string, m0, m1 int, list knownAddrs) bool {
	if len(list) == 0 {
		return false
	}
	seg := text[m0:m1]
	if i := strings.IndexByte(seg, '|'); i >= 0 { // curl … | sh
		seg = seg[:i]
	} else { // sh -c "$(curl …)": the download command runs from the match's last word to the end of the substitution
		w := downloadWord.FindStringIndex(seg)
		if w == nil {
			return false
		}
		rest := text[m1:]
		if j := strings.IndexAny(rest, ")`|;&\n"); j >= 0 {
			rest = rest[:j]
		}
		// A quote inside the substitution could hide its end, or more words, from this reading; the
		// installers' own commands have none (code review, 2026-11-19).
		if strings.ContainsAny(rest, `"'\`) {
			return false
		}
		seg = seg[w[0]:] + rest
	}
	words := strings.Fields(seg)
	if len(words) < 2 {
		return false
	}
	urls := 0
	for _, w := range words[1:] {
		w = unquote.Replace(w) // the shell joins quoted pieces into one word
		switch {
		case w == "":
		case strings.HasPrefix(w, "-") || strings.HasPrefix(w, "="):
			if !installerFlags[strings.ToLower(w)] && !installerShort.MatchString(w) {
				return false
			}
		case officialURL(w, list):
			urls++
		default:
			return false
		}
	}
	return urls > 0
}

// officialURL: w is an https:// address of the list, as one plain word (knownAddrs.has): no user
// information, nothing joined to the host.
func officialURL(w string, list knownAddrs) bool {
	if len(w) < len("https://") || !strings.EqualFold(w[:len("https://")], "https://") {
		return false
	}
	rest := w[len("https://"):]
	end := strings.IndexAny(rest, "/?#:")
	if end < 0 {
		end = len(rest)
	}
	host := strings.ToLower(rest[:end])
	if strings.Contains(host, "@") || strings.Contains(rest, "@") {
		return false
	}
	return list.has(host, rest[end:], true)
}
