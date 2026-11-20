// SPDX-License-Identifier: Apache-2.0
package modelcheck

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// A llama-server answer with logprobs, in its shape (b12342): bytes as a list of numbers.
const chatJSON = `{"choices":[{"message":{"content":"Hi"},"logprobs":{"content":[
 {"id":5,"token":"Hi","bytes":[72,105],"logprob":-0.01,"top_logprobs":[
   {"id":5,"token":"Hi","bytes":[72,105],"logprob":-0.01},{"id":6,"token":"Hey","bytes":[72,101,121],"logprob":-4.7}]},
 {"id":7,"token":"!","logprob":-0.3,"top_logprobs":[{"id":7,"token":"!","logprob":-0.3},{"id":8,"token":"","logprob":-1.5}]}
]}}]}`

func TestAnswer(t *testing.T) {
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" || r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		json.NewDecoder(r.Body).Decode(&body)
		fmt.Fprint(w, chatJSON)
	}))
	defer srv.Close()
	c := &Client{BaseURL: srv.URL + "/"}
	toks, err := c.Answer(context.Background(), "Say hi", DefaultRequest)
	if err != nil {
		t.Fatal(err)
	}
	want := []Token{
		{Key: "4869", Top: []Alt{{"4869", -0.01}, {"486579", -4.7}}},
		{Key: "21", Top: []Alt{{"21", -0.3}, {"id:8", -1.5}}},
	}
	if fmt.Sprint(toks) != fmt.Sprint(want) {
		t.Errorf("tokens %v, want %v", toks, want)
	}
	msgs, _ := json.Marshal(body["messages"])
	kw, _ := json.Marshal(body["chat_template_kwargs"])
	if string(msgs) != `[{"content":"Say hi","role":"user"}]` || body["temperature"] != 0.0 || body["max_tokens"] != 32.0 ||
		body["logprobs"] != true || body["top_logprobs"] != 20.0 || body["stream"] != false || string(kw) != `{"enable_thinking":false}` {
		t.Errorf("request %v", body)
	}
}

func TestRetriesAndErrors(t *testing.T) {
	var calls atomic.Int32
	mode := "flaky"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		switch {
		case mode == "flaky" && n == 1:
			http.Error(w, "loading", http.StatusServiceUnavailable)
		case mode == "bad":
			http.Error(w, "bad request", http.StatusBadRequest)
		case mode == "null":
			fmt.Fprint(w, strings.Replace(chatJSON, `"bytes":[72,101,121],"logprob":-4.7`, `"bytes":[72,101,121],"logprob":null`, 1))
		case mode == "positive":
			fmt.Fprint(w, strings.Replace(chatJSON, `"logprob":-4.7`, `"logprob":0.7`, 1))
		case mode == "nologprobs":
			fmt.Fprint(w, `{"choices":[{"message":{"content":"Hi"}}]}`)
		default:
			fmt.Fprint(w, chatJSON)
		}
	}))
	defer srv.Close()
	c := &Client{BaseURL: srv.URL, Retry: time.Millisecond}
	ctx := context.Background()
	if _, err := c.Answer(ctx, "x", DefaultRequest); err != nil || calls.Load() != 2 {
		t.Errorf("a 503 is retried: %v after %d calls", err, calls.Load())
	}
	mode = "bad"
	calls.Store(0)
	if _, err := c.Answer(ctx, "x", DefaultRequest); err == nil || calls.Load() != 1 || !strings.Contains(err.Error(), "HTTP 400") {
		t.Errorf("a 400 is final: %v after %d calls", err, calls.Load())
	}
	mode = "null"
	if _, err := c.Answer(ctx, "x", DefaultRequest); err == nil || !strings.Contains(err.Error(), "without a logprob") {
		t.Errorf("null logprob: %v", err)
	}
	mode = "positive"
	if _, err := c.Answer(ctx, "x", DefaultRequest); err == nil || !strings.Contains(err.Error(), "unusable logprobs") {
		t.Errorf("positive logprob: %v", err)
	}
	mode = "nologprobs"
	if _, err := c.Answer(ctx, "x", DefaultRequest); err == nil || !strings.Contains(err.Error(), "no logprobs") {
		t.Errorf("no logprobs: %v", err)
	}
	// Run keeps going after a failed prompt and records its error.
	mode = "bad"
	res := c.Run(ctx, []Prompt{{"a", "x"}, {"b", "y"}}, DefaultRequest, nil)
	if len(res) != 2 || res[0].Error == "" || res[1].Error == "" || res[1].ID != "b" {
		t.Errorf("run %+v", res)
	}
}

func TestProps(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"build_info":"b12342-0fb8f15","model_path":"C:\\AIsAtHome\\models\\Qwen3.8-27B-UD-IQ4_XS.gguf",
			"model_ftype":"IQ4_XS - 4.25 bpw","total_slots":1,"default_generation_settings":{"n_ctx":196608}}`)
	}))
	defer srv.Close()
	p, err := (&Client{BaseURL: srv.URL}).Props(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if p.ModelFile() != "Qwen3.8-27B-UD-IQ4_XS.gguf" || p.Gen.NCtx != 196608 || p.TotalSlots != 1 {
		t.Errorf("props %+v, file %q", p, p.ModelFile())
	}
	for info, want := range map[string]string{"b12342-0fb8f15": "b12342 0fb8f15", "b1-7fe450e": " 7fe450e",
		"b999-0fb8f15": " 0fb8f15", "unknown": " ", "": " "} {
		tag, commit := Props{BuildInfo: info}.Build()
		if tag+" "+commit != want {
			t.Errorf("%q: %q %q, want %q", info, tag, commit, want)
		}
	}
}

func TestIsLocal(t *testing.T) {
	for u, want := range map[string]bool{"http://127.0.0.1:8000": true, "http://localhost:8000": true, "http://[::1]:8000": true,
		"http://127.0.0.2": true, "http://192.168.1.5:8000": false, "https://example.org": false, "::": false} {
		if IsLocal(u) != want {
			t.Errorf("IsLocal(%q) = %v", u, !want)
		}
	}
}

func TestPick(t *testing.T) {
	mk := func(file, ftype, build string) *Reference {
		r := refOf(two("a", 0.9, false))
		r.Model, r.Engine.Build, r.Name = Model{File: file, Ftype: ftype}, build, file+build
		return r
	}
	refs := []*Reference{mk("big.gguf", "IQ4_XS", "b11146"), mk("big.gguf", "IQ4_XS", "b12342"), mk("small.gguf", "Q3_K", "b11146")}
	for _, c := range []struct {
		path, ftype, build, want string
	}{
		{"/m/big.gguf", "", "b11146-7fe450e", "big.ggufb11146"},
		{"/m/big.gguf", "", "b12400-abcdef0", "big.ggufb12342"}, // newest when none matches the build
		{`C:\m\SMALL.gguf`, "", "b12342-0fb8f15", "small.ggufb11146"},
		{"/m/renamed.gguf", "Q3_K", "b1-0fb8f15", "small.ggufb11146"}, // by file type
	} {
		r, err := Pick(refs, Props{ModelPath: c.path, ModelFtype: c.ftype, BuildInfo: c.build})
		if err != nil || r.Name != c.want {
			t.Errorf("%s %s: %v %v, want %s", c.path, c.build, r, err, c.want)
		}
	}
	if _, err := Pick(refs, Props{ModelPath: "/m/other.gguf", ModelFtype: "Q8_0"}); err == nil {
		t.Error("picked a reference for another model")
	}
}

// TestChunkTokensRealStream parses a streamed answer of llama-server b12342 (UD-IQ4_XS, a rented
// RTX 4090, 2026-11-20) to the first public prompt, asked as the gateway's probes ask (stream,
// max_tokens 16384, greedy, top-20 logprobs, thinking off): its first 32 positions are the
// published reference's, recorded non-streamed with max_tokens 32 on another 4090.
func TestChunkTokensRealStream(t *testing.T) {
	b, err := os.ReadFile("testdata/stream-b12342-rtx4090.sse")
	if err != nil {
		t.Fatal(err)
	}
	var toks []Token
	for _, line := range strings.Split(string(b), "\n") {
		data, ok := strings.CutPrefix(strings.TrimRight(line, "\r"), "data: ")
		if !ok || data == "[DONE]" {
			continue
		}
		got, err := ChunkTokens([]byte(data), len(toks))
		if err != nil {
			t.Fatal(err)
		}
		toks = append(toks, got...)
	}
	ref, err := LoadBuiltin("qwen3.8-27b-iq4xs-b12342-rtx4090")
	if err != nil {
		t.Fatal(err)
	}
	want := ref.Results[0]
	if len(toks) <= len(want.Tokens) {
		t.Fatalf("%d positions streamed, want more than the reference's %d", len(toks), len(want.Tokens))
	}
	run := &Reference{Format: Format, Kind: KindRun, Request: ref.Request,
		Results: []Result{{ID: want.ID, Text: want.Text, Tokens: toks[:len(want.Tokens)]}}}
	sub := *ref
	sub.Results = []Result{want}
	rep := Compare(&sub, run, Thresholds{MaxCheckKL: DefaultThresholds.MaxCheckKL, MaxFlips: 0, Margin: 0.5, GroupSize: 1})
	if rep.Verdict != Pass || rep.KLMean > 1e-6 || rep.Diverged != 0 {
		t.Errorf("the stream against the reference: %+v", rep)
	}
	// A chunk without logprobs gives none; one with a broken entry is an error.
	if got, err := ChunkTokens([]byte(`{"choices":[{"delta":{"content":"x"}}]}`), 0); err != nil || len(got) != 0 {
		t.Errorf("no logprobs: %v %v", got, err)
	}
	if _, err := ChunkTokens([]byte(`{"choices":[{"logprobs":{"content":[{"token":"x","logprob":-1,"top_logprobs":[{"token":"x"}]}]}}]}`), 0); err == nil {
		t.Error("an alternative without a logprob was accepted")
	}
	if _, err := ChunkTokens([]byte(`{"choices":[{"logprobs":{"content":[{"token":"x","logprob":-1,"top_logprobs":[]}]}}]}`), 0); err == nil {
		t.Error("a position without alternatives was accepted")
	}
}
