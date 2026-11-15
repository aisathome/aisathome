// SPDX-License-Identifier: Apache-2.0
package modelcheck

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aisathome/aisathome/node/setup"
)

// TestBuiltins: every shipped reference matches its pin, parses, uses the published request and
// prompt set, and names a tier's pinned model file, so a reference cannot drift from the tiers.
func TestBuiltins(t *testing.T) {
	prompts := Prompts()
	if len(prompts) != 100 {
		t.Fatalf("%d prompts, want 100", len(prompts))
	}
	files, _ := filepath.Glob("references/*")
	if len(files) != len(Builtins) {
		t.Errorf("%d files in references/, %d in Builtins: ship each file with its pin", len(files), len(Builtins))
	}
	for _, b := range Builtins {
		r, err := LoadBuiltin(b.Name)
		if err != nil {
			t.Fatal(err)
		}
		if r.Name != b.Name || r.Kind != KindReference || r.Request != DefaultRequest || r.Engine.Build == "" || r.GPU.Name == "" || r.Created == "" {
			t.Errorf("%s: name %q kind %q request %+v engine %+v gpu %+v", b.Name, r.Name, r.Kind, r.Request, r.Engine, r.GPU)
		}
		if len(r.Results) != len(prompts) {
			t.Errorf("%s: %d prompts", b.Name, len(r.Results))
		}
		for i, p := range r.Results {
			if i < len(prompts) && (p.ID != prompts[i].ID || p.Text != prompts[i].Text) {
				t.Errorf("%s: prompt %d is %s, the prompt set's %s", b.Name, i, p.ID, prompts[i].ID)
				break
			}
		}
		tier, ok := setup.TierByName(r.Server.Tier)
		if !ok || tier.Model.File() != r.Model.File || tier.Model.SHA256 != r.Model.SHA256 || tier.KV != r.Server.KV {
			t.Errorf("%s: model %+v server %+v is not a tier's pinned file and KV", b.Name, r.Model, r.Server)
		}
	}
	if _, err := LoadBuiltin("nope"); err == nil {
		t.Error("an unknown name loaded")
	}
}

func TestEncodeParse(t *testing.T) {
	ref := refOf(two("a", 0.9, false), two("b", 0.7, true))
	for _, gz := range []bool{true, false} {
		b, err := ref.Encode(gz)
		if err != nil {
			t.Fatal(err)
		}
		got, err := Parse(b)
		if err != nil {
			t.Fatalf("gz %v: %v", gz, err)
		}
		if rep := Compare(ref, got, DefaultThresholds); len(got.Results) != 2 || got.Results[1].Tokens[0].Top[1].LP != ref.Results[1].Tokens[0].Top[1].LP || rep.Verdict != Error {
			t.Errorf("gz %v: round trip %+v (%s)", gz, got.Results, rep.Verdict) // 2 prompts: too few to check
		}
	}
	for name, mutate := range map[string]func(r *Reference){
		"format":    func(r *Reference) { r.Format = "aisathome-modelcheck/2" },
		"no prompt": func(r *Reference) { r.Results = nil },
		"twice":     func(r *Reference) { r.Results[1].ID = "a" },
		"top":       func(r *Reference) { r.Results[0].Tokens[0].Top = nil },
		"request":   func(r *Reference) { r.Request.TopLogprobs = 50 },
	} {
		r := refOf(two("a", 0.9, false), two("b", 0.7, true))
		mutate(r)
		b, _ := r.Encode(false)
		if _, err := Parse(b); err == nil {
			t.Errorf("%s: parsed", name)
		}
	}
}

func TestLoadAndFetchPinned(t *testing.T) {
	b, _ := refOf(two("a", 0.9, false)).Encode(true)
	sum := SHA256(b)
	path := filepath.Join(t.TempDir(), "r.json.gz")
	os.WriteFile(path, b, 0o644)
	if _, err := LoadFile(path, sum); err != nil {
		t.Error(err)
	}
	if _, err := LoadFile(path, strings.Repeat("0", 64)); err == nil || !strings.Contains(err.Error(), "SHA-256") {
		t.Errorf("wrong pin: %v", err)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/r.json.gz" {
			http.NotFound(w, r)
			return
		}
		w.Write(b)
	}))
	defer srv.Close()
	ctx := context.Background()
	if r, err := Fetch(ctx, srv.Client(), srv.URL+"/r.json.gz", strings.ToUpper(sum)); err != nil || r.Name != "t" {
		t.Errorf("fetch: %v", err)
	}
	if _, err := Fetch(ctx, srv.Client(), srv.URL+"/r.json.gz", ""); err == nil {
		t.Error("fetched without a pin")
	}
	if _, err := Fetch(ctx, srv.Client(), srv.URL+"/r.json.gz", strings.Repeat("a", 64)); err == nil {
		t.Error("fetched with a wrong pin")
	}
	if _, err := Fetch(ctx, srv.Client(), srv.URL+"/other", sum); err == nil || !strings.Contains(err.Error(), "404") {
		t.Errorf("404: %v", err)
	}
}
