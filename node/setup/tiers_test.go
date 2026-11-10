// SPDX-License-Identifier: Apache-2.0
package setup

import (
	"regexp"
	"strings"
	"testing"
)

func TestTierTable(t *testing.T) {
	seen := map[string]bool{}
	for i, tr := range Tiers {
		if seen[tr.Name] {
			t.Errorf("tier %q twice", tr.Name)
		}
		seen[tr.Name] = true
		if i > 0 && tr.MinTotalMiB <= Tiers[i-1].MinTotalMiB {
			t.Errorf("tiers not ordered by card size: %s after %s", tr.Name, Tiers[i-1].Name)
		}
		if tr.MinFreeMiB >= tr.MinTotalMiB {
			t.Errorf("%s: needs %d MiB free on a card of %d", tr.Name, tr.MinFreeMiB, tr.MinTotalMiB)
		}
		if !strings.HasPrefix(tr.Model.URL, hfBase) || !strings.HasSuffix(tr.Model.File(), "-"+tr.Quant+".gguf") {
			t.Errorf("%s: model %s is not the pinned repository's %s file", tr.Name, tr.Model.URL, tr.Quant)
		}
		if !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(tr.Model.SHA256) || tr.Model.Size <= 0 {
			t.Errorf("%s: bad pin %q %d", tr.Name, tr.Model.SHA256, tr.Model.Size)
		}
		if !regexp.MustCompile(`^[A-Za-z0-9._-]+$`).MatchString(tr.Alias) {
			t.Errorf("%s: bad alias %q", tr.Name, tr.Alias)
		}
		if got, ok := TierByName(tr.Name); !ok || got.Name != tr.Name {
			t.Errorf("TierByName(%q) = %v, %v", tr.Name, got.Name, ok)
		}
	}
	if _, ok := TierByName("huge"); ok {
		t.Error("TierByName(huge) found a tier")
	}
	if got := Tiers[0].Text(); got != "UD-Q3_K_XL, 80k context" {
		t.Errorf("Text() = %q", got)
	}
	if got := Tiers[1].Model.File(); got != "Qwen3.8-27B-UD-IQ4_XS.gguf" {
		t.Errorf("File() = %q", got)
	}
}

func TestTierForTotal(t *testing.T) {
	for _, c := range []struct {
		mib  int
		want string
	}{
		{0, ""}, {8192, ""}, {12288, ""}, {15499, ""},
		{15500, "standard"}, {15839, "standard"}, {16303, "standard"}, {16376, "standard"}, {22999, "standard"},
		{23000, "big"}, {24564, "big"}, {32607, "big"}, {81920, "big"},
	} {
		got, ok := TierForTotal(c.mib)
		if got.Name != c.want || ok != (c.want != "") {
			t.Errorf("TierForTotal(%d) = %q, %v; want %q", c.mib, got.Name, ok, c.want)
		}
	}
}
