// Package protocol defines the messages exchanged between an ai@home gateway (project
// server) and a node agent over a WebSocket that the node opens outward to the gateway.
//
// Every frame is one JSON object (a Msg). See docs/protocol.md for the full specification.
//
// SPDX-License-Identifier: Apache-2.0
package protocol

import "encoding/json"

// Version is the protocol version spoken by this build. A gateway rejects nodes whose
// major version differs.
const Version = "0.1"

// Message types.
const (
	TypeHello     = "hello"     // node → gateway, first frame: who I am and what I serve
	TypeWelcome   = "welcome"   // gateway → node: accepted
	TypeHeartbeat = "heartbeat" // node → gateway, periodic liveness and load
	TypeRequest   = "request"   // gateway → node: run one chat completion
	TypeCancel    = "cancel"    // gateway → node: client went away, stop work
	TypeChunk     = "chunk"     // node → gateway: one streamed OpenAI chunk
	TypeDone      = "done"      // node → gateway: request finished, with metered usage
	TypeError     = "error"     // node → gateway: request failed
)

// Msg is the single envelope for all frames; only the fields relevant to Type are set.
type Msg struct {
	Type  string `json:"type"`
	ReqID string `json:"req_id,omitempty"`

	// hello
	NodeID   string      `json:"node_id,omitempty"`
	Protocol string      `json:"protocol,omitempty"`
	Agent    string      `json:"agent,omitempty"`  // node software version
	Models   []ModelInfo `json:"models,omitempty"` // what this node can serve

	// welcome
	GatewayID string `json:"gateway_id,omitempty"`

	// heartbeat
	Active int `json:"active,omitempty"` // requests in flight on this node

	// request: an OpenAI-compatible /v1/chat/completions body; "model" holds the local
	// model name chosen by the gateway, and "stream" is always true towards nodes.
	Body json.RawMessage `json:"body,omitempty"`

	// chunk: one OpenAI chat.completion.chunk object, passed through unchanged
	Data json.RawMessage `json:"data,omitempty"`

	// done
	Usage *Usage `json:"usage,omitempty"`

	// error
	Error string `json:"error,omitempty"`
}

// ModelInfo describes one model a node serves.
type ModelInfo struct {
	Name  string   `json:"name"`            // model id as the node's engine knows it
	Roles []string `json:"roles,omitempty"` // e.g. "fast", "deep", "agent"
	Slots int      `json:"slots"`           // concurrent requests this node accepts
	Ctx   int      `json:"ctx,omitempty"`   // max context tokens per request
}

// Usage is the metered result of one request, reported by the node and recorded by the
// gateway. Cached prompt tokens are reported separately because they are cheap to serve.
type Usage struct {
	PromptTokens     int   `json:"prompt_tokens"`
	CompletionTokens int   `json:"completion_tokens"`
	CachedTokens     int   `json:"cached_tokens"`
	DurationMs       int64 `json:"duration_ms"`
}
