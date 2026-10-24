// Logprobs reach the client in the OpenAI shape when the request asked for them, streamed or
// not (the model check needs them through the gateway), and not otherwise.
//
// SPDX-License-Identifier: Apache-2.0
package e2e

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// lpEntry is one token's logprobs as llama-server streams them (top_logprobs: two
// alternatives).
func lpEntry(tok string, lp float64) string {
	return fmt.Sprintf(`{"id":%d,"token":%q,"bytes":%s,"logprob":%g,"top_logprobs":[{"id":1,"token":%q,"bytes":%s,"logprob":%g},{"id":2,"token":"zz","bytes":[122,122],"logprob":-9.5}]}`,
		len(tok), tok, byteList(tok), lp, tok, byteList(tok), lp)
}

func byteList(s string) string {
	parts := make([]string, 0, len(s))
	for _, c := range []byte(s) {
		parts = append(parts, fmt.Sprint(c))
	}
	return "[" + strings.Join(parts, ",") + "]"
}

// logprobsEngine streams "Hello, world" like llama-server: with logprobs on every token chunk
// when the request asks for them (or always, as a misbehaving node might).
func logprobsEngine(t *testing.T, always bool) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if health(w, r) {
			return
		}
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		with := always || body["logprobs"] == true
		w.Header().Set("Content-Type", "text/event-stream")
		for i, piece := range []string{"Hello", ", ", "world"} {
			lp := ""
			if with {
				lp = `,"logprobs":{"content":[` + lpEntry(piece, -0.25*float64(i)) + `]}`
			}
			fmt.Fprintf(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":%q},\"finish_reason\":null%s}]}\n\n", piece, lp)
			w.(http.Flusher).Flush()
		}
		fmt.Fprint(w, "data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n")
		fmt.Fprint(w, "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":12,\"completion_tokens\":3}}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
}

type lpToken struct {
	Token       string  `json:"token"`
	Logprob     float64 `json:"logprob"`
	Bytes       []int   `json:"bytes"`
	TopLogprobs []struct {
		Token   string  `json:"token"`
		Logprob float64 `json:"logprob"`
	} `json:"top_logprobs"`
}

func checkTokens(t *testing.T, what string, got []lpToken) {
	t.Helper()
	want := []string{"Hello", ", ", "world"}
	if len(got) != len(want) {
		t.Fatalf("%s: %d logprob entries, want %d: %+v", what, len(got), len(want), got)
	}
	for i, g := range got {
		if g.Token != want[i] || g.Logprob != -0.25*float64(i) || len(g.Bytes) != len(want[i]) ||
			len(g.TopLogprobs) != 2 || g.TopLogprobs[0].Token != want[i] || g.TopLogprobs[1].Logprob != -9.5 {
			t.Fatalf("%s: entry %d is %+v", what, i, g)
		}
	}
}

const lpAsk = `"logprobs":true,"top_logprobs":2,"temperature":0,`

func TestLogprobsNonStreaming(t *testing.T) {
	r := newRig(t, logprobsEngine(t, false))
	resp := r.do(t, "POST", "/v1/chat/completions", "user-key",
		`{"model":"deep",`+lpAsk+`"messages":[{"role":"user","content":"hi"}]}`)
	defer resp.Body.Close()
	var out struct {
		Choices []struct {
			Message  struct{ Content string } `json:"message"`
			Logprobs *struct {
				Content []lpToken `json:"content"`
			} `json:"logprobs"`
		} `json:"choices"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil || resp.StatusCode != 200 || len(out.Choices) != 1 {
		t.Fatalf("status %d, %+v, %v", resp.StatusCode, out, err)
	}
	if out.Choices[0].Message.Content != "Hello, world" || out.Choices[0].Logprobs == nil {
		t.Fatalf("no logprobs in the answer: %+v", out)
	}
	checkTokens(t, "non-streaming", out.Choices[0].Logprobs.Content)
}

// streamLogprobs reads a streamed answer and returns its text and the logprob entries.
func streamLogprobs(t *testing.T, resp *http.Response) (string, []lpToken, bool) {
	t.Helper()
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	var text strings.Builder
	var toks []lpToken
	sawKey := false
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		data, ok := strings.CutPrefix(sc.Text(), "data: ")
		if !ok || data == "[DONE]" {
			continue
		}
		sawKey = sawKey || strings.Contains(data, `"logprobs"`)
		var c struct {
			Choices []struct {
				Delta    struct{ Content string } `json:"delta"`
				Logprobs *struct {
					Content []lpToken `json:"content"`
				} `json:"logprobs"`
			} `json:"choices"`
		}
		if json.Unmarshal([]byte(data), &c) != nil {
			continue
		}
		for _, ch := range c.Choices {
			text.WriteString(ch.Delta.Content)
			if ch.Logprobs != nil {
				toks = append(toks, ch.Logprobs.Content...)
			}
		}
	}
	return text.String(), toks, sawKey
}

func TestLogprobsStreaming(t *testing.T) {
	r := newRig(t, logprobsEngine(t, false))
	text, toks, _ := streamLogprobs(t, r.do(t, "POST", "/v1/chat/completions", "user-key",
		`{"model":"deep","stream":true,`+lpAsk+`"messages":[{"role":"user","content":"hi"}]}`))
	if text != "Hello, world" {
		t.Fatalf("text %q", text)
	}
	checkTokens(t, "streaming", toks)
}

// A request that did not ask for logprobs gets none, even from a node that sends them.
func TestLogprobsOnlyWhenAsked(t *testing.T) {
	r := newRig(t, logprobsEngine(t, true))
	resp := r.do(t, "POST", "/v1/chat/completions", "user-key", `{"model":"deep","messages":[{"role":"user","content":"hi"}]}`)
	body := readAll(t, resp)
	resp.Body.Close()
	if resp.StatusCode != 200 || !strings.Contains(body, "Hello, world") || strings.Contains(body, "logprob") {
		t.Fatalf("non-streaming, not asked: %d %s", resp.StatusCode, body)
	}
	text, _, saw := streamLogprobs(t, r.do(t, "POST", "/v1/chat/completions", "user-key",
		`{"model":"deep","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	if text != "Hello, world" || saw {
		t.Fatalf("streaming, not asked: %q, logprobs passed on: %v", text, saw)
	}
}

// A tool call is held back until the guard has checked it whole; the logprobs of its tokens
// would show it before that. The guard drops chunks that carry only held pieces, with their
// logprobs, so they are not streamed (the text's are).
func TestLogprobsOfHeldToolCallsNotStreamed(t *testing.T) {
	eng := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if health(w, r) {
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		lp := func(tok string) string { return `,"logprobs":{"content":[` + lpEntry(tok, -0.5) + `]}` }
		fmt.Fprintf(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"Sure\"},\"finish_reason\":null%s}]}\n\n", lp("Sure"))
		for _, a := range []string{`{\"city\":`, `\"Oslo\"}`} {
			fmt.Fprintf(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_1\",\"type\":\"function\",\"function\":{\"name\":\"get_weather\",\"arguments\":\"%s\"}}]},\"finish_reason\":null%s}]}\n\n",
				a, lp("SECRETARG"))
		}
		fmt.Fprint(w, "data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\n")
		fmt.Fprint(w, "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":20,\"completion_tokens\":3}}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	t.Cleanup(eng.Close)
	r := newRig(t, eng)
	resp := r.do(t, "POST", "/v1/chat/completions", "agent-key",
		`{"model":"deep","stream":true,`+lpAsk+`"messages":[{"role":"user","content":"weather?"}],"tools":[{"type":"function","function":{"name":"get_weather"}}]}`)
	body := readAll(t, resp)
	resp.Body.Close()
	if resp.StatusCode != 200 || !strings.Contains(body, `"token":"Sure"`) || !strings.Contains(body, "get_weather") ||
		strings.Contains(body, "SECRETARG") {
		t.Fatalf("status %d: want the text's logprobs and the tool call, without the tool call's logprobs:\n%s", resp.StatusCode, body)
	}
}
