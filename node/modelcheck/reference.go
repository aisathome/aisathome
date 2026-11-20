// Package modelcheck checks that a node's engine and model compute like a published reference:
// it sends a fixed set of short prompts to the local llama-server (greedy, top-20 logprobs,
// thinking off), compares each position's top-20 distribution with the reference's by KL
// divergence, and counts confident flips (the greedy answer leaves the reference's at a token the
// reference was sure of). A reference is one such run, recorded on a known llama.cpp build, model
// file and GPU, and pinned by SHA-256. `aisathome-node modelcheck` is its command line.
//
// The check is for operators and the release process (after a llama.cpp upgrade, with speculative
// decoding on, on a new GPU type). The prompt set is public, so it is not an anti-cheat check.
//
// SPDX-License-Identifier: Apache-2.0
package modelcheck

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"strings"
	"time"
)

// Format names this file format and its version. A reader refuses other versions.
const Format = "aisathome-modelcheck/1"

// Kinds of file: a reference (recorded for publishing) or a node's run (saved with -out).
const (
	KindReference = "reference"
	KindRun       = "run"
)

// Reference is a recorded run: the settings it was recorded with and, per prompt, the greedy
// answer's tokens with their top-k logprobs. A node's run is saved in the same format.
type Reference struct {
	Format  string   `json:"format"`
	Kind    string   `json:"kind"`
	Name    string   `json:"name"`
	Created string   `json:"created"` // YYYY-MM-DD, UTC
	Note    string   `json:"note,omitempty"`
	Engine  Engine   `json:"engine"`
	Model   Model    `json:"model"`
	GPU     GPU      `json:"gpu"`
	Server  Server   `json:"server"`
	Request Request  `json:"request"`
	Results []Result `json:"results"`
}

// Engine is the llama.cpp build that produced the run.
type Engine struct {
	Name      string `json:"name"`             // "llama.cpp"
	Build     string `json:"build"`            // the release tag, "b12342" ("" when not known)
	Commit    string `json:"commit,omitempty"` // the commit, as long as known
	BuildInfo string `json:"build_info"`       // what llama-server's /props reported
}

// Model is the model file: its name, SHA-256 (empty when not hashed) and llama.cpp's file type.
type Model struct {
	File   string `json:"file"`
	SHA256 string `json:"sha256,omitempty"`
	Ftype  string `json:"ftype"`
}

// GPU is the card the run was made on (nvidia-smi's name and compute capability).
type GPU struct {
	Name string `json:"name"`
	Cap  string `json:"cap,omitempty"`
}

// Server is the engine's settings that can matter for the numbers.
type Server struct {
	Tier        string `json:"tier,omitempty"`
	Ctx         int    `json:"ctx,omitempty"`
	Slots       int    `json:"slots,omitempty"`
	KV          string `json:"kv,omitempty"`
	Speculation string `json:"speculation,omitempty"` // off, mtp, dflash2 ("" = not recorded)
}

// Request is the check request: greedy (temperature 0), MaxTokens tokens, the top TopLogprobs
// logprobs at each position, thinking on or off, no system prompt.
type Request struct {
	MaxTokens   int  `json:"max_tokens"`
	TopLogprobs int  `json:"top_logprobs"`
	Thinking    bool `json:"thinking"`
}

// DefaultRequest is the request every published reference uses.
var DefaultRequest = Request{MaxTokens: 32, TopLogprobs: 20, Thinking: false}

// Result is one prompt and its answer, or the error that prevented it.
type Result struct {
	ID     string  `json:"id"`
	Text   string  `json:"text"`
	Tokens []Token `json:"tokens"`
	Error  string  `json:"error,omitempty"`
}

// Token is one position of the answer: the token chosen (by its bytes, hex-encoded, so models
// with other vocabularies compare by text) and the top-k alternatives with their logprobs.
type Token struct {
	Key string `json:"k"`
	Top []Alt  `json:"top"`
}

// Alt is one alternative at a position; in JSON a pair ["<hex bytes>", logprob].
type Alt struct {
	Key string
	LP  float64
}

func (a Alt) MarshalJSON() ([]byte, error) { return json.Marshal([]any{a.Key, a.LP}) }

func (a *Alt) UnmarshalJSON(b []byte) error {
	var pair []json.RawMessage
	if err := json.Unmarshal(b, &pair); err != nil || len(pair) != 2 {
		return fmt.Errorf("alternative %.60s: want [key, logprob]", b)
	}
	if err := json.Unmarshal(pair[0], &a.Key); err != nil {
		return fmt.Errorf("alternative key: %w", err)
	}
	if err := json.Unmarshal(pair[1], &a.LP); err != nil {
		return fmt.Errorf("alternative logprob: %w", err)
	}
	return nil
}

// Validate checks what a comparison relies on.
func (r *Reference) Validate() error {
	if r.Format != Format {
		return fmt.Errorf("format %q: this program reads %q (a newer node release may read it)", r.Format, Format)
	}
	if r.Request.MaxTokens < 1 || r.Request.MaxTokens > 512 || r.Request.TopLogprobs < 2 || r.Request.TopLogprobs > 20 {
		return fmt.Errorf("request: max_tokens %d (1-512) and top_logprobs %d (2-20)", r.Request.MaxTokens, r.Request.TopLogprobs)
	}
	if len(r.Results) == 0 {
		return errors.New("no prompts")
	}
	seen := map[string]bool{}
	for i, p := range r.Results {
		if p.ID == "" || p.Text == "" {
			return fmt.Errorf("prompt %d: no id or text", i)
		}
		if seen[p.ID] {
			return fmt.Errorf("prompt %s twice", p.ID)
		}
		seen[p.ID] = true
		if err := checkTokens(p.Tokens); err != nil {
			return fmt.Errorf("prompt %s %w", p.ID, err)
		}
	}
	return nil
}

// checkTokens refuses positions a comparison cannot use: no token, no alternatives, a logprob
// that is not a log-probability (NaN, infinite, above 0), or alternatives whose probabilities
// add up to more than 1. KL on such numbers can come out as 0 and pass.
func checkTokens(toks []Token) error {
	for t, tok := range toks {
		if tok.Key == "" || len(tok.Top) == 0 {
			return fmt.Errorf("position %d: no token or no alternatives", t)
		}
		sum := 0.0
		for _, a := range tok.Top {
			if a.Key == "" || math.IsNaN(a.LP) || math.IsInf(a.LP, 0) || a.LP > 1e-6 {
				return fmt.Errorf("position %d: alternative %q has logprob %v", t, a.Key, a.LP)
			}
			sum += math.Exp(a.LP)
		}
		if sum > 1.001 {
			return fmt.Errorf("position %d: the alternatives' probabilities add up to %.4f", t, sum)
		}
	}
	return nil
}

// Label is a one-line description: "Qwen3.8-27B-UD-IQ4_XS.gguf, llama.cpp b11146, NVIDIA GeForce RTX 4090".
func (r *Reference) Label() string {
	eng := r.Engine.Build
	if eng == "" {
		eng = "build " + r.Engine.BuildInfo
	}
	s := fmt.Sprintf("%s, llama.cpp %s, %s", r.Model.File, eng, r.GPU.Name)
	if r.Server.Speculation != "" && r.Server.Speculation != "off" {
		s += ", speculation " + r.Server.Speculation
	}
	return s
}

// maxFile caps a reference file (gzipped or not) and its decompressed JSON.
const maxFile = 64 << 20

// Parse reads a reference from its bytes, gzipped or plain JSON, and validates it.
func Parse(b []byte) (*Reference, error) {
	var rd io.Reader = bytes.NewReader(b)
	if len(b) >= 2 && b[0] == 0x1f && b[1] == 0x8b {
		z, err := gzip.NewReader(rd)
		if err != nil {
			return nil, err
		}
		rd = z
	}
	plain, err := io.ReadAll(io.LimitReader(rd, maxFile+1))
	if err != nil {
		return nil, err
	}
	if len(plain) > maxFile {
		return nil, errors.New("larger than 64 MB")
	}
	var r Reference
	if err := json.Unmarshal(plain, &r); err != nil {
		return nil, err
	}
	if err := r.Validate(); err != nil {
		return nil, err
	}
	return &r, nil
}

// Encode is the file's bytes: gzipped JSON when gz, else indented JSON.
func (r *Reference) Encode(gz bool) ([]byte, error) {
	if !gz {
		b, err := json.MarshalIndent(r, "", " ")
		return append(b, '\n'), err
	}
	plain, err := json.Marshal(r)
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	z, _ := gzip.NewWriterLevel(&buf, gzip.BestCompression)
	if _, err := z.Write(plain); err != nil {
		return nil, err
	}
	if err := z.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// SHA256 is the hex SHA-256 of b.
func SHA256(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// checkPin compares b's SHA-256 with want (any case); want "" means no pin.
func checkPin(b []byte, want string) error {
	if want == "" {
		return nil
	}
	if got := SHA256(b); !strings.EqualFold(got, want) {
		return fmt.Errorf("SHA-256 is %s, want %s", got, want)
	}
	return nil
}

// LoadFile reads a reference file; with a non-empty sha256 the file must have it.
func LoadFile(path, sha256 string) (*Reference, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if err := checkPin(b, sha256); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	r, err := Parse(b)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return r, nil
}

// Fetch downloads a reference, which must have the given SHA-256 (required: a downloaded
// reference is only as good as its pin).
func Fetch(ctx context.Context, client *http.Client, url, sha256 string) (*Reference, error) {
	if len(sha256) != 64 {
		return nil, errors.New("a reference from the network needs its SHA-256 (64 hex digits)")
	}
	if client == nil {
		client = &http.Client{Timeout: 2 * time.Minute}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: HTTP %d", url, resp.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxFile+1))
	if err != nil {
		return nil, err
	}
	if len(b) > maxFile {
		return nil, fmt.Errorf("%s: larger than 64 MB", url)
	}
	if err := checkPin(b, sha256); err != nil {
		return nil, fmt.Errorf("%s: %w", url, err)
	}
	r, err := Parse(b)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", url, err)
	}
	return r, nil
}

//go:embed prompts.json references/*.json.gz
var files embed.FS

// Builtin is a published reference shipped inside the node program, pinned by SHA-256.
type Builtin struct {
	Name   string
	File   string // in references/
	SHA256 string
}

// Builtins are the references this release ships. Add one per llama.cpp build and model file
// at release time (docs/modelcheck.md); a test checks every pin and the metadata.
var Builtins = []Builtin{
	{Name: "qwen3.8-27b-iq4xs-b12342-rtx4090", File: "qwen3.8-27b-iq4xs-b12342-rtx4090.json.gz",
		SHA256: "51a432cbac8ddc5ca9da86af7b9884db89599a56a1b28ebb47435665ca9db9f1"},
	{Name: "qwen3.8-27b-iq4xs-b11146-rtx4090", File: "qwen3.8-27b-iq4xs-b11146-rtx4090.json.gz",
		SHA256: "54aeab626227e0ee3170d69926ed6a6c724d200907dc837fa6ee1414d067c982"},
	{Name: "qwen3.8-27b-q3kxl-b11146-rtx4090", File: "qwen3.8-27b-q3kxl-b11146-rtx4090.json.gz",
		SHA256: "548f2506bb72173674b25b84869ef3fd56d64e7982c853939b1f651f181917d0"},
}

// LoadBuiltin reads the built-in reference called name and checks its pin.
func LoadBuiltin(name string) (*Reference, error) {
	for _, b := range Builtins {
		if b.Name != name {
			continue
		}
		data, err := files.ReadFile("references/" + b.File)
		if err != nil {
			return nil, err
		}
		if err := checkPin(data, b.SHA256); err != nil {
			return nil, fmt.Errorf("built-in reference %s: %w", name, err)
		}
		r, err := Parse(data)
		if err != nil {
			return nil, fmt.Errorf("built-in reference %s: %w", name, err)
		}
		return r, nil
	}
	return nil, fmt.Errorf("no built-in reference %q", name)
}

// Prompt is one check prompt.
type Prompt struct {
	ID   string `json:"id"`
	Text string `json:"text"`
}

// maxPrompts bounds a private prompt set (LoadPrompts): a reference of that many prompts stays
// well under maxFile.
const maxPrompts = 5000

// LoadPrompts reads a prompt set from a JSON file, a list of {"id", "text"}: a private set, such as
// the gateway's secret model-check pool (docs/modelcheck.md), recorded with `modelcheck record
// -prompts`. Ids must be unique and neither field empty.
func LoadPrompts(path string) ([]Prompt, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var ps []Prompt
	if err := json.Unmarshal(b, &ps); err != nil {
		return nil, fmt.Errorf("%s: want a JSON list of {\"id\", \"text\"}: %w", path, err)
	}
	if len(ps) == 0 || len(ps) > maxPrompts {
		return nil, fmt.Errorf("%s: %d prompts (1-%d)", path, len(ps), maxPrompts)
	}
	seen := map[string]bool{}
	for i, p := range ps {
		if p.ID == "" || strings.TrimSpace(p.Text) == "" {
			return nil, fmt.Errorf("%s: prompt %d has no id or text", path, i)
		}
		if seen[p.ID] {
			return nil, fmt.Errorf("%s: prompt id %s twice", path, p.ID)
		}
		seen[p.ID] = true
	}
	return ps, nil
}

// Prompts is the check prompt set new references are recorded with: 100 short, generated
// prompts whose greedy answers are mostly stable, the same set as the published references.
func Prompts() []Prompt {
	b, err := files.ReadFile("prompts.json")
	if err != nil {
		panic(err)
	}
	var ps []Prompt
	if err := json.Unmarshal(b, &ps); err != nil {
		panic(err)
	}
	return ps
}
