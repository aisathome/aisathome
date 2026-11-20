// SPDX-License-Identifier: Apache-2.0
package modelcheck

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// Client talks to a llama-server's OpenAI-compatible API.
type Client struct {
	BaseURL string       // "http://127.0.0.1:8000"
	HTTP    *http.Client // nil: a client with a 5-minute timeout per request
	Retry   time.Duration
}

// IsLocal says whether a base URL points at this computer (a loopback address or localhost).
func IsLocal(base string) bool {
	u, err := url.Parse(base)
	if err != nil {
		return false
	}
	h := u.Hostname()
	if strings.EqualFold(h, "localhost") {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && ip.IsLoopback()
}

func (c *Client) http() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return &http.Client{Timeout: 5 * time.Minute}
}

// errFinal is an answer that a retry would not change.
type errFinal struct{ error }

func (c *Client) call(ctx context.Context, path string, body any, out any) error {
	var last error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			wait := c.Retry
			if wait == 0 {
				wait = 2 * time.Second
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(wait):
			}
		}
		last = c.once(ctx, path, body, out)
		var fin errFinal
		if last == nil || errors.As(last, &fin) || ctx.Err() != nil {
			return last
		}
	}
	return last
}

func (c *Client) once(ctx context.Context, path string, body any, out any) error {
	method, rd := http.MethodGet, io.Reader(nil)
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return errFinal{err}
		}
		method, rd = http.MethodPost, bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(c.BaseURL, "/")+path, rd)
	if err != nil {
		return errFinal{err}
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		err := fmt.Errorf("%s: HTTP %d: %.300s", path, resp.StatusCode, b)
		if resp.StatusCode < 500 {
			return errFinal{err}
		}
		return err
	}
	if err := json.Unmarshal(b, out); err != nil {
		return errFinal{fmt.Errorf("%s: %w", path, err)}
	}
	return nil
}

// Props is what llama-server's /props says about the engine and the model.
type Props struct {
	BuildInfo  string `json:"build_info"` // "b12342-0fb8f15"
	ModelPath  string `json:"model_path"`
	ModelFtype string `json:"model_ftype"` // "IQ4_XS - 4.25 bpw"
	TotalSlots int    `json:"total_slots"`
	Gen        struct {
		NCtx int `json:"n_ctx"`
	} `json:"default_generation_settings"`
}

// ModelFile is the model file's name, without its folder (either separator).
func (p Props) ModelFile() string {
	return p.ModelPath[strings.LastIndexAny(p.ModelPath, `/\`)+1:]
}

var reBuildInfo = regexp.MustCompile(`^b(\d+)-([0-9a-f]{7,40})$`)

// Build is the llama.cpp release tag and commit in build_info ("b12342-0fb8f15"). A build
// number below 1000 is not a release's (a shallow clone says "b1"): then the tag is "".
func (p Props) Build() (tag, commit string) {
	m := reBuildInfo.FindStringSubmatch(strings.TrimSpace(p.BuildInfo))
	if m == nil {
		return "", ""
	}
	if len(m[1]) < 4 {
		return "", m[2]
	}
	return "b" + m[1], m[2]
}

// Props reads /props.
func (c *Client) Props(ctx context.Context) (Props, error) {
	var p Props
	err := c.call(ctx, "/props", nil, &p)
	return p, err
}

type chatEntry struct {
	ID      int      `json:"id"`
	Token   string   `json:"token"`
	Bytes   []int    `json:"bytes"`
	Logprob *float64 `json:"logprob"` // nil: missing or null, which is an error
}

type logprobEntry struct {
	chatEntry
	Top []chatEntry `json:"top_logprobs"`
}

type chatResponse struct {
	Choices []struct {
		Logprobs struct {
			Content []logprobEntry `json:"content"`
		} `json:"logprobs"`
	} `json:"choices"`
}

// keyOf is a token's identity across vocabularies: the hex of its bytes.
func keyOf(e chatEntry) string {
	if len(e.Bytes) > 0 {
		b := make([]byte, len(e.Bytes))
		for i, v := range e.Bytes {
			b[i] = byte(v)
		}
		return hex.EncodeToString(b)
	}
	if e.Token != "" {
		return hex.EncodeToString([]byte(e.Token))
	}
	return fmt.Sprintf("id:%d", e.ID)
}

// tokensOf turns llama-server's logprob entries into positions, refusing an entry without a
// logprob or without alternatives.
func tokensOf(entries []logprobEntry, from int) ([]Token, error) {
	var toks []Token
	for _, e := range entries {
		t := Token{Key: keyOf(e.chatEntry)}
		for _, a := range e.Top {
			if a.Logprob == nil {
				return nil, fmt.Errorf("position %d: an alternative without a logprob", from+len(toks))
			}
			t.Top = append(t.Top, Alt{Key: keyOf(a), LP: *a.Logprob})
		}
		if len(t.Top) == 0 {
			return nil, fmt.Errorf("position %d: no top logprobs", from+len(toks))
		}
		toks = append(toks, t)
	}
	return toks, nil
}

// ChunkTokens returns the positions one streamed chat.completion.chunk carries
// (choices[0].logprobs.content, as llama-server streams them; usually one), in Answer's form.
// A chunk without logprobs gives none and no error. from numbers the positions in errors.
func ChunkTokens(data []byte, from int) ([]Token, error) {
	var c chatResponse
	if err := json.Unmarshal(data, &c); err != nil {
		return nil, err
	}
	if len(c.Choices) == 0 {
		return nil, nil
	}
	return tokensOf(c.Choices[0].Logprobs.Content, from)
}

// Answer sends one check prompt and returns the greedy answer's positions.
func (c *Client) Answer(ctx context.Context, text string, rq Request) ([]Token, error) {
	body := map[string]any{
		"messages":             []map[string]string{{"role": "user", "content": text}},
		"temperature":          0,
		"max_tokens":           rq.MaxTokens,
		"logprobs":             true,
		"top_logprobs":         rq.TopLogprobs,
		"stream":               false,
		"chat_template_kwargs": map[string]bool{"enable_thinking": rq.Thinking},
	}
	var r chatResponse
	if err := c.call(ctx, "/v1/chat/completions", body, &r); err != nil {
		return nil, err
	}
	if len(r.Choices) == 0 || len(r.Choices[0].Logprobs.Content) == 0 {
		return nil, errors.New("the answer has no logprobs (llama-server answers with them; another engine may not)")
	}
	toks, err := tokensOf(r.Choices[0].Logprobs.Content, 0)
	if err != nil {
		return nil, err
	}
	if err := checkTokens(toks); err != nil {
		return nil, fmt.Errorf("unusable logprobs: %w", err)
	}
	return toks, nil
}

// Run sends each prompt in order and returns the results; a prompt that fails carries its
// error, so one failure does not end the run. progress (may be nil) sees each finished prompt.
func (c *Client) Run(ctx context.Context, prompts []Prompt, rq Request, progress func(done, total int)) []Result {
	out := make([]Result, 0, len(prompts))
	for i, p := range prompts {
		res := Result{ID: p.ID, Text: p.Text}
		if toks, err := c.Answer(ctx, p.Text, rq); err != nil {
			res.Error = err.Error()
		} else {
			res.Tokens = toks
		}
		out = append(out, res)
		if progress != nil {
			progress(i+1, len(prompts))
		}
		if ctx.Err() != nil {
			break
		}
	}
	return out
}
