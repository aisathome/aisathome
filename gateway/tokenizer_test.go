// SPDX-License-Identifier: Apache-2.0
package gateway

import (
	"encoding/json"
	"flag"
	"os"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// The reference ids come from Hugging Face `tokenizers` (scripts/tokenizer-fixture.py) over
// Qwen3.8's tokenizer.json. testdata/qwen38-mini-tokenizer.json is that file cut down to the
// merges the fixture strings use (in their original order), so CI checks real merges without
// the 12 MB file: BPE picks the lowest-ranked pair present, and every pair it picks is kept.
// With AISATHOME_TOKENIZER=/path/to/tokenizer.json the full file is checked too, and
// -update-mini rebuilds the cut-down file from it.
var updateMini = flag.Bool("update-mini", false, "rebuild testdata/qwen38-mini-tokenizer.json from $AISATHOME_TOKENIZER")

type tokFixture struct {
	SHA256 string `json:"tokenizer_sha256"`
	Cases  []struct {
		Text string  `json:"text"`
		IDs  []int32 `json:"ids"`
	} `json:"cases"`
}

func loadFixture(t *testing.T) tokFixture {
	t.Helper()
	raw, err := os.ReadFile("testdata/qwen38-tokenizer-fixture.json")
	if err != nil {
		t.Fatal(err)
	}
	var f tokFixture
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatal(err)
	}
	if len(f.Cases) < 20 {
		t.Fatal("fixture too small")
	}
	return f
}

func checkFixture(t *testing.T, tk *Tokenizer, f tokFixture) {
	t.Helper()
	for _, c := range f.Cases {
		got := tk.Encode(c.Text)
		if len(got) == 0 && len(c.IDs) == 0 {
			continue
		}
		if !reflect.DeepEqual(got, c.IDs) {
			t.Errorf("%q:\n got  %v\n want %v", c.Text, got, c.IDs)
		}
		if n := tk.Count(c.Text); n != len(c.IDs) {
			t.Errorf("%q: Count %d, want %d", c.Text, n, len(c.IDs))
		}
		bytes := 0
		for _, id := range got {
			bytes += tk.TokenBytes(id)
		}
		if bytes != len(c.Text) {
			t.Errorf("%q: token bytes add up to %d, want %d", c.Text, bytes, len(c.Text))
		}
	}
}

func TestTokenizerMatchesReference(t *testing.T) {
	f := loadFixture(t)
	tk, err := LoadTokenizer("testdata/qwen38-mini-tokenizer.json", "")
	if err != nil {
		t.Fatal(err)
	}
	checkFixture(t, tk, f)
}

func TestTokenizerFullFile(t *testing.T) {
	path := os.Getenv("AISATHOME_TOKENIZER")
	if path == "" {
		t.Skip("set AISATHOME_TOKENIZER to Qwen3.8's tokenizer.json to check the full file")
	}
	f := loadFixture(t)
	if _, err := LoadTokenizer(path, strings.Repeat("0", 64)); err == nil {
		t.Fatal("a wrong pinned SHA-256 was accepted")
	}
	tk, err := LoadTokenizer(path, f.SHA256)
	if err != nil {
		t.Fatal(err)
	}
	checkFixture(t, tk, f)
	if *updateMini {
		writeMini(t, path, tk, f)
	}
}

// writeMini keeps the merges the fixture strings use, the byte tokens and every added token.
func writeMini(t *testing.T, path string, tk *Tokenizer, f tokFixture) {
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var full map[string]json.RawMessage
	json.Unmarshal(raw, &full)
	var model struct {
		Vocab  map[string]int32 `json:"vocab"`
		Merges []string         `json:"merges"` // "a b" in Qwen3.8's file
	}
	if err := json.Unmarshal(full["model"], &model); err != nil {
		t.Fatal(err)
	}
	used := map[int32]bool{}
	tk.trace = func(rank int32) { used[rank] = true }
	tk.cache = map[string][]int32{}
	for _, c := range f.Cases {
		tk.Encode(c.Text)
	}
	tk.trace = nil
	ranks := make([]int, 0, len(used))
	for r := range used {
		ranks = append(ranks, int(r))
	}
	sort.Ints(ranks)
	vocab := map[string]int32{}
	b2u := bytesToUnicode()
	for _, r := range b2u {
		vocab[string(r)] = model.Vocab[string(r)]
	}
	merges := [][2]string{}
	for _, r := range ranks {
		a, b, _ := strings.Cut(model.Merges[r], " ")
		m := [2]string{a, b}
		merges = append(merges, m)
		for _, s := range []string{m[0], m[1], m[0] + m[1]} {
			vocab[s] = model.Vocab[s]
		}
	}
	out := map[string]any{
		"_comment":      "Cut down from Qwen3.8's tokenizer.json (sha256 " + tk.SHA256 + ") by tokenizer_test.go -update-mini: only the merges testdata/qwen38-tokenizer-fixture.json uses.",
		"added_tokens":  json.RawMessage(full["added_tokens"]),
		"pre_tokenizer": json.RawMessage(full["pre_tokenizer"]),
		"model":         map[string]any{"type": "BPE", "vocab": vocab, "merges": merges},
	}
	b, _ := json.MarshalIndent(out, "", " ")
	if err := os.WriteFile("testdata/qwen38-mini-tokenizer.json", append(b, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Logf("wrote %d merges, %d vocab entries", len(merges), len(vocab))
}

// The pre-tokenizer on its own, for cases that read better as pieces than as ids.
func TestPretokenize(t *testing.T) {
	cases := map[string][]string{
		"Hello world":      {"Hello", " world"},
		"  two":            {" ", " two"},
		"a  \n\n b":        {"a", "  \n\n", " b"},
		"x   ":             {"x", "   "},
		"I'LL go":          {"I", "'LL", " go"},
		"it's":             {"it", "'s"},
		"12ab":             {"1", "2", "ab"},
		"hi!!\n\nyo":       {"hi", "!!\n\n", "yo"},
		" ?x":              {" ?", "x"},
		"\u0301a":          {"\u0301a"},
		"नमस्ते":           {"नमस्ते"},
		"a\u00a0b":         {"a", "\u00a0b"},
		"\t\tx":            {"\t", "\tx"},
		"tab\t":            {"tab", "\t"},
		"\xff\xfe":         {"\ufffd\ufffd"},
		"emoji 👩‍💻":        {"emoji", " 👩\u200d💻"},
		"end\r\n":          {"end", "\r\n"},
		"'x":               {"'x"},
		"a.\nb":            {"a", ".\n", "b"},
		"under_score-dash": {"under", "_score", "-dash"},
		"$100":             {"$", "1", "0", "0"},
		"(\n":              {"(\n"},
		"  ":               {"  "},
		"\n":               {"\n"},
		"\u3000\u3000word": {"\u3000", "\u3000word"},
		"ſ'ſ":              {"ſ", "'ſ"},
		"don'T":            {"don", "'T"},
		"x 'reword":        {"x", " '", "reword"},
	}
	for in, want := range cases {
		var got []string
		pretokenize(in, func(p string) { got = append(got, p) })
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%q: got %q, want %q", in, got, want)
		}
	}
}

// A long run of one letter must not take quadratic time (the merge heap).
func TestBPELongRun(t *testing.T) {
	tk, err := LoadTokenizer("testdata/qwen38-mini-tokenizer.json", "")
	if err != nil {
		t.Fatal(err)
	}
	s := strings.Repeat("a", 200000)
	if n := tk.Count(s); n <= 0 || n > len(s) {
		t.Fatalf("count %d", n)
	}
}

func TestTokenizerRejectsOtherPreTokenizers(t *testing.T) {
	raw, err := os.ReadFile("testdata/qwen38-mini-tokenizer.json")
	if err != nil {
		t.Fatal(err)
	}
	other := strings.Replace(string(raw), `[\\p{L}\\p{M}]+`, `\\p{L}+`, 1)
	if other == string(raw) {
		t.Fatal("test did not change the pattern")
	}
	if _, err := parseTokenizer([]byte(other)); err == nil || !strings.Contains(err.Error(), "pre-tokenizer") {
		t.Fatalf("a different pre-tokenizer was accepted: %v", err)
	}
}
