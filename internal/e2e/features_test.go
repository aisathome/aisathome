// Tests for default output limits, tool calls in non-streaming answers, context errors and
// context length in /v1/models.
//
// SPDX-License-Identifier: Apache-2.0
package e2e

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// captureEngine answers like mockEngine and remembers the last request body.
func captureEngine(t *testing.T, last *atomic.Value) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		last.Store(body)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\n")
		fmt.Fprint(w, "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":1}}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
}

func TestDefaultMaxTokens(t *testing.T) {
	var last atomic.Value
	r := newRig(t, captureEngine(t, &last))
	for _, c := range []struct {
		body string
		want float64
	}{
		{`{"model":"deep","messages":[{"role":"user","content":"hi"}]}`, 16384},
		{`{"model":"deep","max_tokens":50,"messages":[{"role":"user","content":"hi"}]}`, 50},
	} {
		resp := r.do(t, "POST", "/v1/chat/completions", "user-key", c.body)
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		got := last.Load().(map[string]any)["max_tokens"]
		if got != c.want {
			t.Fatalf("body %s: engine saw max_tokens=%v, want %v", c.body, got, c.want)
		}
	}
}

// toolEngine streams a tool call whose arguments arrive in pieces, like llama-server.
func toolEngine(t *testing.T) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for _, d := range []string{
			`{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"get_weather","arguments":"{"}}]}`,
			`{"tool_calls":[{"index":0,"function":{"arguments":"\"city\":\""}}]}`,
			`{"tool_calls":[{"index":0,"function":{"arguments":"Oslo\"}"}}]}`,
		} {
			fmt.Fprintf(w, "data: {\"choices\":[{\"index\":0,\"delta\":%s,\"finish_reason\":null}]}\n\n", d)
		}
		fmt.Fprint(w, "data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\n")
		fmt.Fprint(w, "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":20,\"completion_tokens\":9}}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
}

func TestNonStreamingToolCalls(t *testing.T) {
	r := newRig(t, toolEngine(t))
	resp := r.do(t, "POST", "/v1/chat/completions", "user-key",
		`{"model":"deep","messages":[{"role":"user","content":"weather?"}],"tools":[{"type":"function","function":{"name":"get_weather"}}]}`)
	defer resp.Body.Close()
	var out struct {
		Choices []struct {
			Message struct {
				Content   *string `json:"content"`
				ToolCalls []struct {
					ID       string `json:"id"`
					Type     string `json:"type"`
					Function struct{ Name, Arguments string }
				} `json:"tool_calls"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil || resp.StatusCode != 200 {
		t.Fatalf("status %d err %v", resp.StatusCode, err)
	}
	m := out.Choices[0].Message
	if len(m.ToolCalls) != 1 || m.ToolCalls[0].ID != "call_1" || m.ToolCalls[0].Type != "function" ||
		m.ToolCalls[0].Function.Name != "get_weather" || m.ToolCalls[0].Function.Arguments != `{"city":"Oslo"}` ||
		m.Content != nil || out.Choices[0].FinishReason != "tool_calls" {
		t.Fatalf("bad tool call assembly: %+v", out)
	}
}

// overflowEngine answers like llama-server when the prompt does not fit the context.
var overflowHits atomic.Int32

func overflowEngine(t *testing.T) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		overflowHits.Add(1)
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `{"error":{"code":400,"message":"the request exceeds the available context size, try increasing it","type":"exceed_context_size_error","n_prompt_tokens":70000,"n_ctx":65536}}`)
	}))
}

func TestContextOverflowIsClientError(t *testing.T) {
	overflowHits.Store(0)
	r := newRig(t, overflowEngine(t), overflowEngine(t))
	resp := r.do(t, "POST", "/v1/chat/completions", "user-key", chatBody)
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 400 || !strings.Contains(string(b), `"code":"context_length_exceeded"`) {
		t.Fatalf("got %d %s, want 400 context_length_exceeded", resp.StatusCode, b)
	}
	if n := overflowHits.Load(); n != 1 {
		t.Fatalf("an oversized request was sent to %d nodes; it must not be retried", n)
	}
}

func TestModelsReportContextLength(t *testing.T) {
	r := newRig(t, mockEngine(t, false))
	resp := r.do(t, "GET", "/v1/models", "user-key", "")
	defer resp.Body.Close()
	var out struct {
		Data []struct {
			ID            string `json:"id"`
			ContextLength int    `json:"context_length"`
		} `json:"data"`
	}
	json.NewDecoder(resp.Body).Decode(&out)
	if len(out.Data) != 2 {
		t.Fatalf("want model and role, got %+v", out.Data)
	}
	for _, m := range out.Data {
		if m.ContextLength != 32768 {
			t.Fatalf("%s: context_length %d, want 32768", m.ID, m.ContextLength)
		}
	}
}
