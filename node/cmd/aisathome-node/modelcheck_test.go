// SPDX-License-Identifier: Apache-2.0
package main

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aisathome/aisathome/node/modelcheck"
)

type fakeSMI struct{ out string }

func (f fakeSMI) Query(context.Context, ...string) (string, error) { return f.out, nil }

// fakeLlama is a llama-server that answers each check prompt with a reference's recorded
// answer, optionally changed by alter (prompt index, positions), and reports props.
type fakeLlama struct {
	ref   *modelcheck.Reference
	props string
	alter func(i int, toks []modelcheck.Token) []modelcheck.Token
	mu    sync.Mutex
	asked int
}

type entry struct {
	ID      int     `json:"id"`
	Token   string  `json:"token"`
	Bytes   []int   `json:"bytes"`
	Logprob float64 `json:"logprob"`
}

func bytesOf(key string) []int {
	b, _ := hex.DecodeString(key)
	out := make([]int, len(b))
	for i, v := range b {
		out[i] = int(v)
	}
	return out
}

func (f *fakeLlama) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/props":
		fmt.Fprint(w, f.props)
	case "/v1/chat/completions":
		var body struct {
			Messages []struct{ Content string } `json:"messages"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		for i, res := range f.ref.Results {
			if res.Text != body.Messages[0].Content {
				continue
			}
			f.mu.Lock()
			f.asked++
			f.mu.Unlock()
			toks := append([]modelcheck.Token(nil), res.Tokens...)
			if f.alter != nil {
				toks = f.alter(i, toks)
			}
			var content []map[string]any
			for _, t := range toks {
				var top []entry
				lp := 0.0
				for _, a := range t.Top {
					e := entry{Bytes: bytesOf(a.Key), Logprob: a.LP}
					if id, ok := strings.CutPrefix(a.Key, "id:"); ok { // a token without bytes or text
						e.ID, _ = strconv.Atoi(id)
					}
					top = append(top, e)
					if a.Key == t.Key {
						lp = a.LP
					}
				}
				content = append(content, map[string]any{"id": 1, "token": "", "bytes": bytesOf(t.Key), "logprob": lp, "top_logprobs": top})
			}
			json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"logprobs": map[string]any{"content": content}}}})
			return
		}
		http.Error(w, "unknown prompt", http.StatusBadRequest)
	default:
		http.NotFound(w, r)
	}
}

const bigProps = `{"build_info":"b12342-0fb8f15","model_path":"/var/lib/aisathome/models/Qwen3.8-27B-UD-IQ4_XS.gguf",
	"model_ftype":"IQ4_XS - 4.25 bpw","total_slots":1,"default_generation_settings":{"n_ctx":196608}}`

func testEnv() mcEnv {
	return mcEnv{
		smi: fakeSMI{"0, GPU-test, NVIDIA GeForce RTX 3090, 24576, 24000, 580.95.05, 8.6"},
		now: func() time.Time { return time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC) },
		hash: func(string) (string, error) {
			return "40fac4050e940397dbf13087afd50f4734a11805bf9d65ef8ddd7483470e6199", nil
		},
		retry: time.Millisecond,
	}
}

func runMC(t *testing.T, env mcEnv, args ...string) (int, string, string) {
	t.Helper()
	var out, errb bytes.Buffer
	code := env.main(context.Background(), args, &out, &errb)
	return code, out.String(), errb.String()
}

// TestModelcheckEndToEnd runs the command against a fake llama-server that replays the big
// tier's b12342 reference: that built-in is picked by the served model file and build, the check
// passes, the saved run compares offline (also against the older b11146 reference, with a note
// on the build); changed answers fail.
func TestModelcheckEndToEnd(t *testing.T) {
	ref, err := modelcheck.LoadBuiltin("qwen3.8-27b-iq4xs-b12342-rtx4090")
	if err != nil {
		t.Fatal(err)
	}
	fake := &fakeLlama{ref: ref, props: bigProps}
	srv := httptest.NewServer(fake)
	defer srv.Close()
	env := testEnv()
	saved := filepath.Join(t.TempDir(), "run.json.gz")

	code, out, errs := runMC(t, env, "-server", srv.URL, "-out", saved)
	if code != mcPass || !strings.Contains(out, "PASS") || !strings.Contains(out, "against qwen3.8-27b-iq4xs-b12342-rtx4090") ||
		strings.Contains(out, "recorded on llama.cpp") || !strings.Contains(out, "largest 0.0000") || fake.asked != 100 {
		t.Fatalf("exit %d after %d prompts:\n%s\n%s", code, fake.asked, out, errs)
	}
	if code, out, _ := runMC(t, env, "compare", "-ref", "qwen3.8-27b-iq4xs-b11146-rtx4090", "-run", saved); code != mcPass ||
		!strings.Contains(out, "recorded on llama.cpp b11146, this engine is b12342") {
		t.Errorf("against the b11146 reference: %d\n%s", code, out)
	}
	// A b12323 engine (no reference of its own) gets the newest build's reference, with a note.
	fake.props = strings.Replace(bigProps, "b12342-0fb8f15", "b12323-93cf736", 1)
	if code, out, _ := runMC(t, env, "-server", srv.URL, "-limit", "4"); code != mcPass ||
		!strings.Contains(out, "against qwen3.8-27b-iq4xs-b12342-rtx4090") || !strings.Contains(out, "this engine is b12323") {
		t.Errorf("b12323 engine: %d\n%s", code, out)
	}
	fake.props = bigProps
	run, err := modelcheck.LoadFile(saved, "")
	if err != nil || run.Kind != modelcheck.KindRun || run.Engine.Build != "b12342" || run.GPU.Name != "NVIDIA GeForce RTX 3090" || len(run.Results) != 100 {
		t.Fatalf("saved run: %v %+v", err, run)
	}
	code, out, _ = runMC(t, env, "compare", "-ref", "qwen3.8-27b-iq4xs-b12342-rtx4090", "-run", saved, "-json")
	var js struct {
		Report modelcheck.Report `json:"report"`
	}
	if code != mcPass || json.Unmarshal([]byte(out), &js) != nil || js.Report.Verdict != modelcheck.Pass || js.Report.Prompts != 100 {
		t.Errorf("compare: exit %d\n%s", code, out)
	}

	// Confident flips: three prompts answer with their second-best first token, where the
	// reference was sure (margin > 0.5 nats).
	flipped := 0
	fake.alter = func(i int, toks []modelcheck.Token) []modelcheck.Token {
		top := toks[0].Top
		if flipped < 3 && len(top) > 1 && top[0].LP-top[1].LP > 1 {
			flipped++
			return []modelcheck.Token{{Key: top[1].Key, Top: top}}
		}
		return toks
	}
	code, out, _ = runMC(t, env, "-server", srv.URL)
	if code != mcFail || !strings.Contains(out, "FAIL") || !strings.Contains(out, "3 confident flips") {
		t.Errorf("3 flips: exit %d\n%s", code, out)
	}

	// Answers cut to their first token: they end where the reference goes on.
	fake.alter = func(i int, toks []modelcheck.Token) []modelcheck.Token { return toks[:1] }
	code, out, _ = runMC(t, env, "-server", srv.URL)
	if code != mcFail || !strings.Contains(out, "confident flips") {
		t.Errorf("truncated answers: exit %d\n%s", code, out)
	}

	// Another distribution: every logprob scaled, as a different model file would differ.
	fake.alter = func(i int, toks []modelcheck.Token) []modelcheck.Token {
		out := make([]modelcheck.Token, len(toks))
		for j, t := range toks {
			out[j] = modelcheck.Token{Key: t.Key}
			for _, a := range t.Top {
				out[j].Top = append(out[j].Top, modelcheck.Alt{Key: a.Key, LP: a.LP * 1.3})
			}
		}
		return out
	}
	code, out, _ = runMC(t, env, "-server", srv.URL, "-limit", "8")
	if code != mcFail || !strings.Contains(out, "KL divergence") {
		t.Errorf("scaled logprobs: exit %d\n%s", code, out)
	}
}

func TestModelcheckCannotCheck(t *testing.T) {
	env := testEnv()
	if code, _, errs := runMC(t, env, "-server", "http://192.0.2.1:8000"); code != mcCannot || !strings.Contains(errs, "not on this computer") {
		t.Errorf("remote: %d %s", code, errs)
	}
	srv := httptest.NewServer(http.NotFoundHandler())
	srv.Close() // nothing listens
	if code, _, errs := runMC(t, env, "-server", srv.URL); code != mcCannot || !strings.Contains(errs, "Is the node's engine") {
		t.Errorf("no engine: %d %s", code, errs)
	}
	other := httptest.NewServer(&fakeLlama{props: `{"model_path":"/m/other.gguf","model_ftype":"Q8_0"}`})
	defer other.Close()
	if code, _, errs := runMC(t, env, "-server", other.URL); code != mcCannot || !strings.Contains(errs, "no built-in reference") {
		t.Errorf("other model: %d %s", code, errs)
	}
	if code, _, errs := runMC(t, env, "-server", other.URL, "-ref", filepath.Join(t.TempDir(), "missing.json.gz")); code != mcCannot || !strings.Contains(errs, "reference") {
		t.Errorf("missing reference file: %d %s", code, errs)
	}
	if code, _, errs := runMC(t, env, "-server", other.URL, "-ref", "https://example.invalid/r.json.gz"); code != mcCannot || !strings.Contains(errs, "SHA-256") {
		t.Errorf("URL without a pin: %d %s", code, errs)
	}
}

// TestModelcheckRecord records a (short) reference from the fake server, checks its metadata,
// and checks the engine against it.
func TestModelcheckRecord(t *testing.T) {
	ref, _ := modelcheck.LoadBuiltin("qwen3.8-27b-iq4xs-b11146-rtx4090")
	fake := &fakeLlama{ref: ref, props: bigProps}
	srv := httptest.NewServer(fake)
	defer srv.Close()
	env := testEnv()
	path := filepath.Join(t.TempDir(), "new.json.gz")
	code, out, errs := runMC(t, env, "record", "-server", srv.URL, "-out", path, "-limit", "8", "-speculation", "mtp")
	if code != mcPass || !strings.Contains(out, `File: "new.json.gz"`) || !strings.Contains(out, "SHA256: \""+modelcheck.SHA256(mustRead(t, path))) {
		t.Fatalf("record: exit %d\n%s\n%s", code, out, errs)
	}
	// A plain JSON file is not what ships: no Builtins entry with a pin that would not match.
	plain := filepath.Join(t.TempDir(), "plain.json")
	if code, out, _ := runMC(t, env, "record", "-server", srv.URL, "-out", plain, "-limit", "4"); code != mcPass || strings.Contains(out, "SHA256:") || !strings.Contains(out, "gzip this file") {
		t.Errorf("record to .json: %d\n%s", code, out)
	}
	r, err := modelcheck.LoadFile(path, "")
	if err != nil {
		t.Fatal(err)
	}
	want := modelcheck.Server{Tier: "big", Ctx: 196608, Slots: 1, KV: "q4_0", Speculation: "mtp"}
	if r.Name != "qwen3.8-27b-iq4xs-b12342-rtx3090-mtp" || r.Kind != modelcheck.KindReference || r.Engine.Build != "b12342" ||
		r.Engine.Commit != "0fb8f15" || r.GPU != (modelcheck.GPU{Name: "NVIDIA GeForce RTX 3090", Cap: "8.6"}) ||
		r.Model.SHA256 != "40fac4050e940397dbf13087afd50f4734a11805bf9d65ef8ddd7483470e6199" || r.Server != want ||
		r.Created != "2026-11-15" || len(r.Results) != 8 {
		t.Errorf("recorded %s %+v %+v %+v %+v %s %d", r.Name, r.Engine, r.GPU, r.Model, r.Server, r.Created, len(r.Results))
	}
	if code, out, _ := runMC(t, env, "-server", srv.URL, "-ref", path); code != mcPass || !strings.Contains(out, "8 prompts") {
		t.Errorf("check against the recorded reference: %d\n%s", code, out)
	}

	// An engine that names no release needs -engine-build; a wrong one is refused.
	fake.props = strings.Replace(bigProps, "b12342-0fb8f15", "b1-0fb8f15", 1)
	if code, _, errs := runMC(t, env, "record", "-server", srv.URL, "-out", path); code != mcCannot || !strings.Contains(errs, "-engine-build") {
		t.Errorf("no release: %d %s", code, errs)
	}
	if code, _, _ := runMC(t, env, "record", "-server", srv.URL, "-out", path, "-engine-build", "b12342", "-limit", "4"); code != mcPass {
		t.Errorf("with -engine-build: %d", code)
	}
	fake.props = bigProps
	if code, _, errs := runMC(t, env, "record", "-server", srv.URL, "-out", path, "-engine-build", "b12323"); code != mcCannot || !strings.Contains(errs, "engine says") {
		t.Errorf("contradicting -engine-build: %d %s", code, errs)
	}
	// A failed prompt writes no reference.
	fake.alter = func(i int, toks []modelcheck.Token) []modelcheck.Token {
		if i == 2 {
			return nil
		}
		return toks
	}
	failed := filepath.Join(t.TempDir(), "failed.json.gz")
	if code, _, errs := runMC(t, env, "record", "-server", srv.URL, "-out", failed, "-limit", "4"); code != mcFail || !strings.Contains(errs, "no reference written") {
		t.Errorf("failed prompt: %d %s", code, errs)
	}
	if _, err := modelcheck.LoadFile(failed, ""); err == nil {
		t.Error("a reference was written despite a failed prompt")
	}
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestRefName(t *testing.T) {
	for _, c := range []struct{ file, build, gpu, spec, want string }{
		{"Qwen3.8-27B-UD-IQ4_XS.gguf", "b12342", "NVIDIA GeForce RTX 4090", "off", "qwen3.8-27b-iq4xs-b12342-rtx4090"},
		{"Qwen3.8-27B-UD-Q3_K_XL.gguf", "b12342", "NVIDIA GeForce RTX 5060 Ti", "", "qwen3.8-27b-q3kxl-b12342-rtx5060ti"},
		{"Qwen3.8-27B-UD-IQ4_XS.gguf", "b12342", "NVIDIA RTX A5000", "mtp", "qwen3.8-27b-iq4xs-b12342-rtxa5000-mtp"},
	} {
		if got := refName(c.file, c.build, c.gpu, c.spec); got != c.want {
			t.Errorf("%s %s: %s, want %s", c.file, c.gpu, got, c.want)
		}
	}
}
