// SPDX-License-Identifier: Apache-2.0
package setup

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeEngine answers as llama-server's /tokenize and /completion do, with a set decode speed.
func fakeEngine(t *testing.T, tps float64, got *map[string]any) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		switch r.URL.Path {
		case "/tokenize":
			if body["content"] != speedText {
				http.Error(w, "unexpected text", http.StatusBadRequest)
				return
			}
			json.NewEncoder(w).Encode(map[string]any{"tokens": []int{785, 14796, 6290, 8604, 1181, 7424, 304}})
		case "/completion":
			*got = body
			n := len(body["prompt"].([]any))
			json.NewEncoder(w).Encode(map[string]any{"content": "...", "timings": map[string]any{
				"prompt_n": n, "prompt_ms": 41234.5, "predicted_n": body["n_predict"], "predicted_per_second": tps}})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestSpeedTestHTTP(t *testing.T) {
	var body map[string]any
	srv := fakeEngine(t, 28.94, &body)
	s, err := SpeedTest(context.Background(), HTTPEngine{BaseURL: srv.URL + "/"}, SpeedPromptTokens, SpeedPredict)
	if err != nil {
		t.Fatal(err)
	}
	if s.TPS != 28.94 || s.PromptTokens != SpeedPromptTokens || s.PromptSeconds != 41.2345 {
		t.Errorf("got %+v", s)
	}
	prompt := body["prompt"].([]any)
	if len(prompt) != 32000 || prompt[0] != 785.0 || prompt[7] != 785.0 || prompt[31999] != prompt[31999%7] {
		t.Errorf("prompt: %d tokens, %v %v", len(prompt), prompt[0], prompt[7])
	}
	if body["n_predict"] != 256.0 || body["cache_prompt"] != false || body["ignore_eos"] != true || body["temperature"] != 0.0 {
		t.Errorf("completion request %v", body)
	}
	g := GPU{Index: 0, Name: "NVIDIA GeForce RTX 5070 Ti"}
	if err := CheckSpeed(g, s, MinDecodeTPS); err != nil {
		t.Errorf("28.9 tokens/s: %v", err)
	}
}

func TestCheckSpeed(t *testing.T) {
	g := GPU{Index: 0, Name: "NVIDIA GeForce RTX 5070 Ti"}
	err := CheckSpeed(g, Speed{TPS: 16.6, PromptTokens: 32000}, MinDecodeTPS)
	if err == nil {
		t.Fatal("16.6 tokens/s passed")
	}
	for _, want := range []string{"writes 16.6 tokens/s after a 32000-token prompt; the network needs at least 20", "BOINC GPU tasks", "clocks or power limit are locked"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal lacks %q: %v", want, err)
		}
	}
	if err := CheckSpeed(g, Speed{TPS: 3}, 0); err != nil {
		t.Errorf("minimum 0 must never refuse: %v", err)
	}
	if err := CheckSpeed(g, Speed{TPS: 20}, 20); err != nil {
		t.Errorf("exactly the minimum: %v", err)
	}
}

type stubEngine struct {
	toks    []int
	tokErr  error
	timings Timings
	err     error
	prompt  []int
}

func (s *stubEngine) Tokenize(context.Context, string) ([]int, error) { return s.toks, s.tokErr }
func (s *stubEngine) Complete(_ context.Context, p []int, _ int) (Timings, error) {
	s.prompt = p
	return s.timings, s.err
}

func TestSpeedTestFailures(t *testing.T) {
	ctx := context.Background()
	if _, err := SpeedTest(ctx, &stubEngine{tokErr: errors.New("connection refused")}, 100, 8); err == nil || !strings.Contains(err.Error(), "tokenize: connection refused") {
		t.Errorf("tokenize: %v", err)
	}
	if _, err := SpeedTest(ctx, &stubEngine{}, 100, 8); err == nil {
		t.Error("no tokens: no error")
	}
	if _, err := SpeedTest(ctx, &stubEngine{toks: []int{1, 2, 3}, err: errors.New("HTTP 500")}, 100, 8); err == nil {
		t.Error("completion error: no error")
	}
	if _, err := SpeedTest(ctx, &stubEngine{toks: []int{1, 2, 3}}, 100, 8); err == nil || !strings.Contains(err.Error(), "no decode speed") {
		t.Errorf("no timings: %v", err)
	}
	e := &stubEngine{toks: []int{1, 2, 3}, timings: Timings{PredictedPerSecond: 30}}
	if _, err := SpeedTest(ctx, e, 10, 8); err != nil || len(e.prompt) != 10 || e.prompt[9] != 1 {
		t.Errorf("prompt %v, %v", e.prompt, err)
	}
	// The real client: an engine that answers without timings, and one that is down.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/tokenize" {
			w.Write([]byte(`{"tokens":[1,2]}`))
			return
		}
		w.Write([]byte(`{"content":"x"}`))
	}))
	defer srv.Close()
	if _, err := SpeedTest(ctx, HTTPEngine{BaseURL: srv.URL}, 10, 8); err == nil || !strings.Contains(err.Error(), "no timings") {
		t.Errorf("no timings over HTTP: %v", err)
	}
	srv.Close()
	if _, err := SpeedTest(ctx, HTTPEngine{BaseURL: srv.URL}, 10, 8); err == nil {
		t.Error("engine down: no error")
	}
}
