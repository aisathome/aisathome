//go:build !windows

// More of the drift guard (drift_test.go): the parts of install.sh that have a Go twin in this package
// and no drift test of their own. Each test cuts the bash functions out of install.sh, runs them with
// stand-ins for nvidia-smi, systemctl and df, and compares every answer with the Go function's:
//
//   - the card and its tier: pick_tier and ChooseCard (with ParseComputeApps for the node's own engine);
//   - whether the tier fits, and the advice when it does not: check_fit and fit_advice, CheckFit and FitAdvice;
//   - driver versions: ver_ge and VersionAtLeast;
//   - free disk space: check_disk and checkDisk.
//
// SPDX-License-Identifier: Apache-2.0
package setup

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// installShFuncs reads install.sh and returns the named functions' definitions, one after the other.
func installShFuncs(t *testing.T, names ...string) string {
	t.Helper()
	b, err := os.ReadFile(installSh)
	if err != nil {
		t.Fatal(err)
	}
	var defs []string
	for _, n := range names {
		defs = append(defs, shellFunc(t, string(b), n))
	}
	return strings.Join(defs, "\n") + "\n"
}

// runBash runs script in bash with only PATH set, and returns its output.
func runBash(t *testing.T, script string) string {
	t.Helper()
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("no bash")
	}
	cmd := exec.Command(bash, "-c", script)
	cmd.Env = []string{"PATH=" + os.Getenv("PATH")}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("bash: %v\n%s", err, out)
	}
	return string(out)
}

// The card the engine uses, its memory and its tier: pick_tier, given nvidia-smi's answers, must choose
// what ChooseCard chooses from the same cards, with the node's own engine's memory counted alike.
func TestInstallShPickTierDrift(t *testing.T) {
	type card struct {
		uuid         string
		total, free_ int
	}
	cases := []struct {
		name  string
		cards []card
		apps  string // --query-compute-apps output; the node's engine is pid 4242
	}{
		{"one 24 GB card", []card{{"GPU-a", 24576, 23000}}, ""},
		{"one 16 GB card", []card{{"GPU-a", 16376, 15800}}, ""},
		{"too small", []card{{"GPU-a", 12288, 12000}}, ""},
		{"just big enough for standard", []card{{"GPU-a", 15360, 15000}}, ""},
		{"one below standard", []card{{"GPU-a", 15359, 15000}}, ""},
		{"larger card wins", []card{{"GPU-a", 16376, 16000}, {"GPU-b", 24576, 1000}}, ""},
		{"larger card wins, listed first", []card{{"GPU-a", 24576, 1000}, {"GPU-b", 16376, 16000}}, ""},
		{"tie: more free wins", []card{{"GPU-a", 24576, 10000}, {"GPU-b", 24576, 20000}}, ""},
		{"tie, equal free: the first", []card{{"GPU-a", 24576, 20000}, {"GPU-b", 24576, 20000}}, ""},
		{"own engine's memory counts as free", []card{{"GPU-a", 24576, 2000}, {"GPU-b", 24576, 10000}},
			"GPU-a, 4242, 19000\nGPU-a, 999, 500\nGPU-b, 999, 300\n"},
		{"own engine on the other card", []card{{"GPU-a", 24576, 12000}, {"GPU-b", 24576, 10000}},
			"GPU-b, 4242, 3000\n"},
		{"own engine, two entries", []card{{"GPU-a", 24576, 2000}}, "GPU-a, 4242, 9000\nGPU-a, 4242, 1000\n"},
		{"apps it cannot read", []card{{"GPU-a", 24576, 2000}}, "GPU-a, 4242, [N/A]\n"},
		{"three cards", []card{{"GPU-a", 16376, 16000}, {"GPU-b", 24576, 20000}, {"GPU-c", 32607, 1000}}, ""},
	}
	var sb strings.Builder
	sb.WriteString("set -eu\n" + installShFuncs(t, "trim", "tier_ctx", "tier_model", "tier_text", "tier_for_total", "pick_tier"))
	for i, c := range cases {
		var csv strings.Builder
		for j, k := range c.cards {
			fmt.Fprintf(&csv, "%d, %s, NVIDIA Test %d, %d, %d\n", j, k.uuid, j, k.total, k.free_)
		}
		// nvidia-smi and systemctl as functions (command -v finds them), die as a line and an exit; the
		// stand-ins' answers in variables pick_tier does not declare local (bash scopes them dynamically)
		fmt.Fprintf(&sb, `(
fake_gpus='%s' fake_apps='%s'
nvidia-smi() { case "$*" in *query-gpu*) printf '%%s' "$fake_gpus" ;; *query-compute-apps*) printf '%%s' "$fake_apps" ;; esac; }
systemctl() { echo 4242; }
die() { echo "case %d|die|$*"; exit 0; }
gpu_idx="" gpu_uuid="" gpu_name="" gpu_total=0 gpu_free=0 gpu_own=0 gpu_count=0 tier="" gpu_pin=""
pick_tier 2>/dev/null
echo "case %d|$gpu_idx|$gpu_uuid|$gpu_total|$gpu_free|$gpu_own|$tier|$gpu_pin"
)
`, csv.String(), c.apps, i, i)
	}
	out := runBash(t, sb.String())
	seen := 0
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		f := strings.Split(line, "|")
		i, err := strconv.Atoi(strings.TrimPrefix(f[0], "case "))
		if err != nil || i >= len(cases) {
			t.Fatalf("unexpected bash output %q", line)
		}
		seen++
		c := cases[i]
		var gpus []GPU
		for j, k := range c.cards {
			gpus = append(gpus, GPU{Index: j, UUID: k.uuid, Name: fmt.Sprintf("NVIDIA Test %d", j), TotalMiB: k.total, FreeMiB: k.free_})
		}
		var own map[string]int
		if c.apps != "" {
			own = ParseComputeApps(c.apps, 4242)
		}
		ch, err := ChooseCard(gpus, own)
		if f[1] == "die" {
			if err == nil {
				t.Errorf("%s: install.sh refuses (%s), Go chooses GPU %d (%s)", c.name, f[2], ch.Index, ch.Tier.Name)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: Go refuses (%v), install.sh chooses GPU %s", c.name, err, f[1])
			continue
		}
		pin := ""
		if ch.Several {
			pin = "Environment=CUDA_VISIBLE_DEVICES=" + ch.UUID
		}
		goVals := []string{strconv.Itoa(ch.Index), ch.UUID, strconv.Itoa(ch.TotalMiB), strconv.Itoa(ch.Free()), strconv.Itoa(ch.OwnMiB), ch.Tier.Name, pin}
		for k, what := range []string{"card", "uuid", "total", "free", "own", "tier", "pin"} {
			if f[k+1] != goVals[k] {
				t.Errorf("%s: %s: install.sh %q, Go %q", c.name, what, f[k+1], goVals[k])
			}
		}
	}
	if seen != len(cases) {
		t.Errorf("bash answered %d cases, want %d", seen, len(cases))
	}
}

// Whether the tier fits the card's free memory: check_fit and CheckFit at the tier's line and on each side
// of it, and the advice when it does not (fit_advice and FitAdvice: worded for the installer and for the
// setup page, the same advice for the same tier).
func TestInstallShFitDrift(t *testing.T) {
	var sb strings.Builder
	sb.WriteString("set -eu\n" + installShFuncs(t, "tier_ctx", "tier_model", "tier_text", "tier_min_free", "fit_advice", "check_fit"))
	type fc struct {
		tier string
		free int
	}
	var cases []fc
	for _, tr := range Tiers {
		for _, d := range []int{-1000, -1, 0, 1} {
			cases = append(cases, fc{tr.Name, tr.MinFreeMiB + d})
		}
	}
	for i, c := range cases {
		fmt.Fprintf(&sb, `(
die() { echo "case %d|refused|$*"; exit 0; }
tier=%s gpu_free=%d gpu_own=0 gpu_idx=0 gpu_name=Test custom=0
check_fit 2>/dev/null
echo "case %d|fits|"
)
`, i, c.tier, c.free, i)
	}
	for _, tr := range Tiers {
		fmt.Fprintf(&sb, "tier=%s; echo \"advice %s|$(fit_advice)\"\n", tr.Name, tr.Name)
	}
	out := runBash(t, sb.String())
	seen := 0
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		head, rest, _ := strings.Cut(line, "|")
		if name, ok := strings.CutPrefix(head, "advice "); ok {
			tr, _ := TierByName(name)
			// the same advice, worded for each: the installer's "installer" is the setup page's "setup"
			sh, goAdvice := rest, FitAdvice(tr)
			for _, mark := range []string{"16 GB card", "light desktop", "integrated GPU", "2 GB", "motherboard's graphics", "run the"} {
				if strings.Contains(sh, mark) != strings.Contains(goAdvice, mark) {
					t.Errorf("fit advice for %s: %q in one only\n install.sh %q\n Go         %q", name, mark, sh, goAdvice)
				}
			}
			seen++
			continue
		}
		i, err := strconv.Atoi(strings.TrimPrefix(head, "case "))
		if err != nil || i >= len(cases) {
			t.Fatalf("unexpected bash output %q", line)
		}
		seen++
		c := cases[i]
		tr, _ := TierByName(c.tier)
		goErr := CheckFit(Choice{GPU: GPU{Name: "Test", FreeMiB: c.free}, Tier: tr})
		verdict, msg, _ := strings.Cut(rest, "|")
		if (verdict == "refused") != (goErr != nil) {
			t.Errorf("%s tier, %d MiB free: install.sh %s, Go %v", c.tier, c.free, verdict, goErr)
		}
		if goErr != nil && verdict == "refused" {
			// the same numbers in both refusals
			for _, n := range []string{strconv.Itoa(c.free) + " MiB free", fmt.Sprintf("needs %d MiB free", tr.MinFreeMiB), tr.Text()} {
				if !strings.Contains(msg, n) || !strings.Contains(goErr.Error(), n) {
					t.Errorf("%s tier, %d MiB free: %q not in both\n install.sh %q\n Go         %q", c.tier, c.free, n, msg, goErr)
				}
			}
		}
	}
	if seen != len(cases)+len(Tiers) {
		t.Errorf("bash answered %d lines, want %d", seen, len(cases)+len(Tiers))
	}
}

// Driver versions as nvidia-smi and the bundle's BUILD-INFO write them (numbers and dots: check_bundle
// refuses anything else): ver_ge and VersionAtLeast must agree.
func TestInstallShVerGeDrift(t *testing.T) {
	// Not compared: a version without a dot (check_bundle refuses it) or one that differs from another
	// only by trailing zero parts ("570.0" and "570.0.0"): sort -V puts the shorter first, so ver_ge
	// says "570.0" < "570.0.0" where VersionAtLeast counts the missing part as 0. Drivers do not
	// write versions like that.
	versions := []string{"535.183.01", "550.54.14", "550.54.15", "565.57.01", "570.26", "570.65", "570.86.10",
		"570.124.04", "570.124.06", "570.133.07", "575.51.02", "576.02", "580.65.06", "590.12", "1000.1"}
	var sb strings.Builder
	sb.WriteString(installShFuncs(t, "ver_ge"))
	for _, a := range versions {
		for _, b := range versions {
			fmt.Fprintf(&sb, "if ver_ge %s %s; then echo '%s %s 1'; else echo '%s %s 0'; fi\n", a, b, a, b, a, b)
		}
	}
	out := runBash(t, sb.String())
	n := 0
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		f := strings.Fields(line)
		if len(f) != 3 {
			t.Fatalf("unexpected bash output %q", line)
		}
		n++
		if sh, goVal := f[2] == "1", VersionAtLeast(f[0], f[1]); sh != goVal {
			t.Errorf("%s >= %s: install.sh %v, Go %v", f[0], f[1], sh, goVal)
		}
	}
	if n != len(versions)*len(versions) {
		t.Errorf("bash compared %d pairs, want %d", n, len(versions)*len(versions))
	}
}

// Free disk space for the model download: check_disk (df, in KiB) and checkDisk must refuse the same
// needs, around the free space, and measure the same folder: the nearest one that exists.
func TestInstallShDiskDrift(t *testing.T) {
	root := t.TempDir()
	missing := filepath.Join(root, "models", "not", "yet")
	const freeKiB = 20_000_000 // about 20.5 GB
	free := int64(freeKiB) * 1024
	needs := []int64{0, 1, free - 1, free, free + 1, free + 1024, 2 * free}
	var sb strings.Builder
	sb.WriteString("set -eu\n" + installShFuncs(t, "gb", "check_disk"))
	// df as a function: POSIX output (-P, 1024-byte blocks), and the folder it was asked about
	fmt.Fprintf(&sb, "df() { printf 'Filesystem 1024-blocks Used Available Capacity Mounted on\\n/dev/x 40000000 0 %d 0%% /\\n'; echo \"df ${@: -1}\" >&2; }\n", freeKiB)
	for i, need := range needs {
		// the first run reports which folder df was asked about, the second whether it refused (die
		// writes to fd 3, the script's stdout)
		fmt.Fprintf(&sb, "( die() { echo \"case %d|refused|$*\" >&3; exit 0; }; check_disk %q %d 2>&1 >/dev/null | grep '^df ' | sed 's/^/case %d|asked|/'; check_disk %q %d >/dev/null 2>&1; echo \"case %d|ok|\" ) 3>&1\n",
			i, missing, need, i, missing, need, i)
	}
	out := runBash(t, sb.String())
	var asked []string
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		f := strings.SplitN(line, "|", 3)
		if len(f) != 3 {
			t.Fatalf("unexpected bash output %q", line)
		}
		i, err := strconv.Atoi(strings.TrimPrefix(f[0], "case "))
		if err != nil || i >= len(needs) {
			t.Fatalf("unexpected bash output %q", line)
		}
		need := needs[i]
		var goAsked string
		goErr := checkDisk(missing, need, func(d string) (uint64, error) { goAsked = d; return uint64(free), nil })
		switch f[1] {
		case "asked":
			asked = append(asked, f[2])
			if f[2] != "df "+goAsked {
				t.Errorf("need %d: install.sh asks df about %q, Go about %q", need, strings.TrimPrefix(f[2], "df "), goAsked)
			}
		case "refused":
			if goErr == nil {
				t.Errorf("need %d bytes, %d free: install.sh refuses, Go does not", need, free)
			} else {
				for _, n := range []string{"on " + root + ":", fmt.Sprintf("need %.1f GB", float64(need)/1e9), fmt.Sprintf("%.1f GB free", float64(free)/1e9)} {
					if !strings.Contains(f[2], n) || !strings.Contains(goErr.Error(), n) {
						t.Errorf("need %d: %q not in both\n install.sh %q\n Go         %q", need, n, f[2], goErr)
					}
				}
			}
		case "ok":
			if goErr != nil {
				t.Errorf("need %d bytes, %d free: install.sh accepts, Go refuses: %v", need, free, goErr)
			}
		}
	}
	if len(asked) == 0 {
		t.Error("check_disk never asked df")
	}
}
