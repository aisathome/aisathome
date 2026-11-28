// SPDX-License-Identifier: Apache-2.0
package gateway

import (
	"strings"
	"testing"
)

func TestMaskEmail(t *testing.T) {
	for in, want := range map[string]string{
		"alice@example.com": "al•••@example.com", "bob@example.com": "b•••@example.com", "x@example.com": "x•••@example.com",
		"not-an-address": "",
	} {
		if got := maskEmail(in); got != want {
			t.Fatalf("maskEmail(%q) = %q, want %q", in, got, want)
		}
	}
}

// Every ASCII character that Markdown or HTML could read as markup is escaped; letters, digits,
// spaces and other scripts stay as they are.
func TestMarkdownText(t *testing.T) {
	if got := markdownText(`a <b>&"'</b> *x* _y_ [l](u) #1 ~z~ |t| ` + "`c`" + ` 1. 2) ü`); got !=
		`a &lt;b&gt;&amp;&quot;&#39;&lt;\/b&gt; \*x\* \_y\_ \[l\]\(u\) \#1 \~z\~ \|t\| \`+"`"+`c\`+"`"+` 1\. 2\) ü` {
		t.Fatalf("markdownText: %s", got)
	}
	for c := byte('!'); c <= '~'; c++ {
		s := string(c)
		if strings.Contains("&<>\"'", s) || 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' || '0' <= c && c <= '9' {
			continue
		}
		if got := markdownText(s); got != `\`+s {
			t.Fatalf("%q is not escaped: %q", s, got)
		}
	}
}

func TestCleanNotice(t *testing.T) {
	if s, err := CleanNotice("  Planned   maintenance  today "); err != nil || s != "Planned maintenance today" {
		t.Fatalf("%q %v", s, err)
	}
	for _, bad := range []string{"", " ", "a\nb", "a\rb", "a\x00b", "a\u200bb", "\xff", strings.Repeat("a", 301)} {
		if _, err := CleanNotice(bad); err == nil {
			t.Fatalf("%q accepted", bad)
		}
	}
}
