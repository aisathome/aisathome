// Usage records, token bounds, and budget weights.
//
// SPDX-License-Identifier: Apache-2.0
package gateway

import (
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/aisathome/aisathome/protocol"
)

// clampUsage bounds node-reported usage to what is plausible for the request: nodes are
// untrusted, and inflated numbers would exhaust shared budgets (or inflate credit).
// promptBytes is the size of the request sent to the node; tokens are never more than bytes.
func clampUsage(u protocol.Usage, promptBytes, maxTokens, relayed int) (protocol.Usage, bool) {
	orig := u
	u.PromptTokens = min(max(u.PromptTokens, 0), promptBytes+4096)
	u.CachedTokens = min(max(u.CachedTokens, 0), u.PromptTokens)
	u.CompletionTokens = min(max(u.CompletionTokens, 0), maxTokens, 8*relayed+64)
	return u, u != orig
}

// weighted is the cost of a request for budgets and credit: new prompt tokens count fully,
// cached prompt tokens a tenth (they cost the node little), generated tokens fully.
func weighted(u protocol.Usage) int64 {
	cached := min(u.CachedTokens, u.PromptTokens)
	return int64(u.PromptTokens-cached) + int64(cached/10) + int64(u.CompletionTokens)
}

// outcomeBlocked: a guard stopped the answer (guard.go).
const outcomeBlocked = "blocked"

type usageRecord struct {
	Time      time.Time      `json:"time"`
	ReqID     string         `json:"req_id"`
	User      string         `json:"user"`
	Node      string         `json:"node"`
	Operator  string         `json:"operator"`
	Model     string         `json:"model"`
	Requested string         `json:"requested"`
	Outcome   string         `json:"outcome"`        // ok, cancelled (client left), failed or blocked
	Relayed   int            `json:"relayed_chunks"` // text chunks the gateway relayed itself
	Estimated bool           `json:"estimated,omitempty"`
	Clamped   bool           `json:"clamped,omitempty"` // node reported implausible usage
	Usage     protocol.Usage `json:"usage"`             // node-reported, or estimated from relayed chunks
	// SelfServed: the node's operator is the requesting account (no credit for serving oneself).
	SelfServed bool `json:"self_served,omitempty"`
	// Tools: the request carried tool definitions (an agent request); never which ones.
	Tools bool   `json:"tools,omitempty"`
	Guard string `json:"guard,omitempty"` // a guard's rule: the answer was stopped, or (text) flagged
	// Counted says who counted Tokens: "gateway" (its tokenizer) or "node" (reported counts,
	// clamped). Cached tokens are always the gateway's decision. Credit is what the node earned.
	Counted string        `json:"counted,omitempty"`
	Tokens  *creditTokens `json:"tokens,omitempty"`
	Credit  float64       `json:"credit,omitempty"`
}

// textLen is the number of generated characters in a streamed chunk (for budget bounds).
// Generated output is text, reasoning and tool-call arguments: all of it costs GPU time and
// counts against budgets.
func textLen(chunk []byte) int {
	var c struct {
		Choices []struct {
			Delta struct {
				Content          string `json:"content"`
				ReasoningContent string `json:"reasoning_content"`
				ToolCalls        []struct {
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"delta"`
		} `json:"choices"`
	}
	if json.Unmarshal(chunk, &c) != nil {
		return 0
	}
	n := 0
	for _, ch := range c.Choices {
		n += len(ch.Delta.Content) + len(ch.Delta.ReasoningContent)
		for _, tc := range ch.Delta.ToolCalls {
			n += len(tc.Function.Name) + len(tc.Function.Arguments)
		}
	}
	return n
}

// hasText reports whether a streamed chunk carries generated text (answer or reasoning).
func hasText(chunk []byte) bool { return textLen(chunk) > 0 }

type usageWriter struct {
	mu sync.Mutex
	f  *os.File
}

func newUsageWriter(path string) (*usageWriter, error) {
	if path == "" {
		return &usageWriter{}, nil
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, fmt.Errorf("usage log: %w", err)
	}
	return &usageWriter{f: f}, nil
}

func (u *usageWriter) record(r usageRecord) {
	if u.f == nil {
		return
	}
	b, _ := json.Marshal(r)
	u.mu.Lock()
	defer u.mu.Unlock()
	u.f.Write(append(b, '\n'))
}
