//go:build !windows

// The drift guard: install.sh (the Linux installer, bash) and the Go tier table must agree.
// The test cuts install.sh's tier_* functions out of the script, runs them in bash and compares
// every answer with Tiers, so a tier change in one fails CI until the other follows. It also
// checks the speed test's numbers and prompt. Linux only: it runs the Linux installer's bash.
//
// SPDX-License-Identifier: Apache-2.0
package setup

import (
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
)

const installSh = "../packaging/linux/install.sh"

// shellFunc is the definition of the function name in script: a one-line "name() { ...; }" or
// from "name() {" to the next line that is just "}".
func shellFunc(t *testing.T, script, name string) string {
	t.Helper()
	lines := strings.Split(script, "\n")
	for i, l := range lines {
		if !strings.HasPrefix(l, name+"() {") {
			continue
		}
		if strings.HasSuffix(strings.TrimSpace(l), "}") {
			return l
		}
		for j := i + 1; j < len(lines); j++ {
			if lines[j] == "}" {
				return strings.Join(lines[i:j+1], "\n")
			}
		}
	}
	t.Fatalf("install.sh has no function %s()", name)
	return ""
}

func TestInstallShDrift(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("no bash")
	}
	b, err := os.ReadFile(installSh)
	if err != nil {
		t.Fatal(err)
	}
	script := string(b)

	// The tier names install.sh's --tier accepts must be the Go table's.
	m := regexp.MustCompile(`case "\$want_tier" in\s*\n\s*""\|([a-z|]+)\)`).FindStringSubmatch(script)
	if m == nil {
		t.Fatal(`install.sh: cannot find the --tier names (case "$want_tier" in ""|...)`)
	}
	shNames := strings.Split(m[1], "|")
	var goNames []string
	for _, tr := range Tiers {
		goNames = append(goNames, tr.Name)
	}
	slices.Sort(shNames)
	slices.Sort(goNames)
	if !slices.Equal(shNames, goNames) {
		t.Errorf("tier names: install.sh %v, Go %v", shNames, goNames)
	}

	var defs []string
	for _, f := range []string{"tier_ctx", "tier_kv", "tier_min_free", "tier_alias", "tier_model", "tier_text", "tier_for_total"} {
		defs = append(defs, shellFunc(t, script, f))
	}
	// Card sizes: every Go boundary and one below it, then a sweep, so a moved threshold or a
	// new tier in install.sh shows.
	totals := []int{0}
	for _, tr := range Tiers {
		totals = append(totals, tr.MinTotalMiB-1, tr.MinTotalMiB, tr.MinTotalMiB+1)
	}
	for mib := 0; mib <= 131072; mib += 128 {
		totals = append(totals, mib)
	}
	var sb strings.Builder
	sb.WriteString("set -eu\n" + strings.Join(defs, "\n") + "\n")
	for _, n := range goNames {
		fmt.Fprintf(&sb, "printf 'tier %%s|%%s|%%s|%%s|%%s|%%s|%%s\\n' %[1]s \"$(tier_ctx %[1]s)\" \"$(tier_kv %[1]s)\" \"$(tier_min_free %[1]s)\" \"$(tier_alias %[1]s)\" \"$(tier_model %[1]s)\" \"$(tier_text %[1]s)\"\n", n)
	}
	for _, mib := range totals {
		fmt.Fprintf(&sb, "printf 'total %%s|%%s\\n' %d \"$(tier_for_total %d)\"\n", mib, mib)
	}
	out, err := exec.Command(bash, "-c", sb.String()).CombinedOutput()
	if err != nil {
		t.Fatalf("bash: %v\n%s", err, out)
	}

	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		kind, rest, _ := strings.Cut(line, " ")
		f := strings.Split(rest, "|")
		switch kind {
		case "tier":
			tr, _ := TierByName(f[0])
			model := fmt.Sprintf("%s %s %d", tr.Model.URL, tr.Model.SHA256, tr.Model.Size)
			for _, c := range []struct{ what, sh, goVal string }{
				{"tier_ctx", f[1], strconv.Itoa(tr.Ctx)},
				{"tier_kv", f[2], tr.KV},
				{"tier_min_free", f[3], strconv.Itoa(tr.MinFreeMiB)},
				{"tier_alias", f[4], tr.Alias},
				{"tier_model", f[5], model},
				{"tier_text", f[6], tr.Text()},
			} {
				if c.sh != c.goVal {
					t.Errorf("%s %s: install.sh %q, Go %q", c.what, tr.Name, c.sh, c.goVal)
				}
			}
		case "total":
			mib, _ := strconv.Atoi(f[0])
			goTier, _ := TierForTotal(mib)
			if f[1] != goTier.Name {
				t.Errorf("tier_for_total %d: install.sh %q, Go %q", mib, f[1], goTier.Name)
			}
		default:
			t.Errorf("unexpected bash output %q", line)
		}
	}

	// The speed test: minimum, prompt length, tokens written and the prompt itself.
	if m := regexp.MustCompile(`min_tps="\$\{MIN_DECODE_TPS:-([0-9.]+)\}" speed_ctx=([0-9]+)`).FindStringSubmatch(script); m == nil {
		t.Error("install.sh: cannot find min_tps and speed_ctx")
	} else if m[1] != strconv.FormatFloat(MinDecodeTPS, 'f', -1, 64) || m[2] != strconv.Itoa(SpeedPromptTokens) {
		t.Errorf("speed test: install.sh min %s, %s tokens; Go %g, %d", m[1], m[2], MinDecodeTPS, SpeedPromptTokens)
	}
	probe := shellFunc(t, script, "speed_probe")
	if !strings.Contains(probe, fmt.Sprintf(`"n_predict": %d,`, SpeedPredict)) {
		t.Errorf("speed_probe does not write %d tokens", SpeedPredict)
	}
	tm := regexp.MustCompile(`(?s)\ntext = \((.*?)\)\n`).FindStringSubmatch(probe)
	if tm == nil {
		t.Fatal("speed_probe: cannot find its prompt text")
	}
	var text strings.Builder
	for _, lit := range regexp.MustCompile(`"([^"]*)"`).FindAllStringSubmatch(tm[1], -1) {
		text.WriteString(lit[1])
	}
	if text.String() != speedText {
		t.Errorf("speed prompt: install.sh %q, Go %q", text.String(), speedText)
	}
}
