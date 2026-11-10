// SPDX-License-Identifier: Apache-2.0
package setup

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Engine is what the speed test needs from llama-server. Tests use a fake.
type Engine interface {
	Tokenize(ctx context.Context, text string) ([]int, error)
	// Complete writes nPredict tokens after prompt, with no prompt cache, and returns the
	// engine's own timings.
	Complete(ctx context.Context, prompt []int, nPredict int) (Timings, error)
}

// Timings is the part of llama-server's /completion "timings" the speed test reads.
type Timings struct {
	PromptN            int     `json:"prompt_n"`
	PromptMS           float64 `json:"prompt_ms"`
	PredictedN         int     `json:"predicted_n"`
	PredictedPerSecond float64 `json:"predicted_per_second"`
}

// HTTPEngine is llama-server's native API at BaseURL ("http://127.0.0.1:8000").
type HTTPEngine struct {
	BaseURL string
	Client  *http.Client // default: http.DefaultClient; requests carry the caller's context
}

func (e HTTPEngine) post(ctx context.Context, path string, body, out any) error {
	b, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(e.BaseURL, "/")+path, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	c := e.Client
	if c == nil {
		c = http.DefaultClient
	}
	resp, err := c.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		msg := strings.TrimSpace(string(data))
		if len(msg) > 200 {
			msg = msg[:200]
		}
		return fmt.Errorf("%s: HTTP %d: %s", path, resp.StatusCode, msg)
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("%s: %v", path, err)
	}
	return nil
}

// Tokenize is POST /tokenize.
func (e HTTPEngine) Tokenize(ctx context.Context, text string) ([]int, error) {
	var r struct {
		Tokens []int `json:"tokens"`
	}
	if err := e.post(ctx, "/tokenize", map[string]any{"content": text}, &r); err != nil {
		return nil, err
	}
	return r.Tokens, nil
}

// Complete is POST /completion with a token prompt, greedy, ignoring end of text.
func (e HTTPEngine) Complete(ctx context.Context, prompt []int, nPredict int) (Timings, error) {
	var r struct {
		Timings *Timings `json:"timings"`
	}
	body := map[string]any{"prompt": prompt, "n_predict": nPredict, "temperature": 0, "ignore_eos": true, "cache_prompt": false}
	if err := e.post(ctx, "/completion", body, &r); err != nil {
		return Timings{}, err
	}
	if r.Timings == nil {
		return Timings{}, errors.New("/completion: no timings in the answer")
	}
	return *r.Timings, nil
}

// speedText is the speed test's prompt, repeated to length as tokens: decode speed depends on
// the context's length, not its content. The same text as install.sh's speed_probe.
const speedText = "The river town kept its records in a stone house by the bridge. Every spring the clerks " +
	"counted boats, bales of wool, barrels of salt and the names of travellers, and every autumn " +
	"they argued about the numbers. A merchant from the coast once paid in copper coins stamped " +
	"with a heron; nobody could say where they were minted. "

// Speed is one speed test's result.
type Speed struct {
	TPS           float64 // decode tokens per second after the prompt
	PromptTokens  int
	PromptSeconds float64
}

// SpeedTest measures decode speed after a promptTokens-long prompt (SpeedPromptTokens), writing
// nPredict tokens (SpeedPredict), from the engine's own timings. Tokenize gets a minute; the
// completion up to 20 minutes, unless ctx ends sooner.
func SpeedTest(ctx context.Context, e Engine, promptTokens, nPredict int) (Speed, error) {
	tctx, cancel := context.WithTimeout(ctx, time.Minute)
	toks, err := e.Tokenize(tctx, speedText)
	cancel()
	if err != nil {
		return Speed{}, fmt.Errorf("tokenize: %w", err)
	}
	if len(toks) == 0 {
		return Speed{}, errors.New("tokenize: no tokens")
	}
	prompt := make([]int, 0, promptTokens)
	for len(prompt) < promptTokens {
		prompt = append(prompt, toks[:min(len(toks), promptTokens-len(prompt))]...)
	}
	cctx, cancel := context.WithTimeout(ctx, 20*time.Minute)
	defer cancel()
	t, err := e.Complete(cctx, prompt, nPredict)
	if err != nil {
		return Speed{}, err
	}
	if t.PredictedPerSecond <= 0 {
		return Speed{}, fmt.Errorf("the engine reported no decode speed (%d tokens written)", t.PredictedN)
	}
	return Speed{TPS: t.PredictedPerSecond, PromptTokens: t.PromptN, PromptSeconds: t.PromptMS / 1000}, nil
}

// CheckSpeed refuses a card that writes slower than minTPS after the long prompt (MinDecodeTPS;
// 0 measures and never refuses, for testing). The refusal names what usually causes it.
func CheckSpeed(g GPU, s Speed, minTPS float64) error {
	if s.TPS >= minTPS {
		return nil
	}
	return fmt.Errorf("GPU %d (%s) writes %.1f tokens/s after a %d-token prompt; the network needs at least %g, so the setup stopped the node (users would wait too long on long prompts). "+
		"If other programs used the GPU during the test (BOINC GPU tasks, games, video), stop them; if the card's clocks or power limit are locked or lowered (a mining or overclocking tool such as MSI Afterburner, or nvidia-smi -lgc / -pl), undo that. Then run the setup again.",
		g.Index, g.Name, s.TPS, s.PromptTokens, minTPS)
}
