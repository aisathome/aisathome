// Package protocol defines the messages exchanged between an AIs@home gateway (project
// server) and a node agent over a WebSocket that the node opens outward to the gateway.
//
// Every frame is one JSON object (a Msg). See PROTOCOL.md in this directory for the full specification.
//
// SPDX-License-Identifier: Apache-2.0
package protocol

import "encoding/json"

// Version is the protocol version this build speaks: a small integer, raised by one when
// the messages change in a way the other side must know about. A node sends it in hello as
// protocol_version; a gateway accepts a configured range (by default this version and the
// one before it) and refuses others (CloseUnsupportedProtocol). PROTOCOL.md lists the
// versions.
const Version = 2

// Unversioned is the version of a hello without protocol_version: nodes released before
// protocol versions existed (v0.1.0 to v0.3.0) speak version 1.
const Unversioned = 1

// LegacyVersion is what the "protocol" field of hello and welcome has carried since v0.1. It
// is frozen: gateways before protocol versions refuse a node whose major differs from "0",
// so nodes keep sending it, and gateways keep sending it to old nodes. Versions are compared
// with protocol_version only.
const LegacyVersion = "0.1"

// CloseUnsupportedProtocol is the WebSocket close code a gateway refuses a node's protocol
// version with (instead of welcome). The close reason is a short text for the volunteer that
// names the versions the gateway supports. A private-use code (4000-4999, RFC 6455).
const CloseUnsupportedProtocol = 4001

// HelloVersion is the protocol version a hello announces (Unversioned if none).
func (m Msg) HelloVersion() int {
	if m.ProtocolVersion == 0 {
		return Unversioned
	}
	return m.ProtocolVersion
}

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
	NodeID   string `json:"node_id,omitempty"`
	Protocol string `json:"protocol,omitempty"` // LegacyVersion (hello and welcome)
	// ProtocolVersion (hello: the version the node speaks, absent = Unversioned; welcome: the
	// version the gateway speaks with this node). Added in protocol version 2.
	ProtocolVersion int         `json:"protocol_version,omitempty"`
	Agent           string      `json:"agent,omitempty"`  // node software version
	Models          []ModelInfo `json:"models,omitempty"` // what this node can serve

	// welcome
	GatewayID string `json:"gateway_id,omitempty"`
	// ProtocolMin and ProtocolMax: the protocol versions this gateway accepts (version 2).
	ProtocolMin int `json:"protocol_min,omitempty"`
	ProtocolMax int `json:"protocol_max,omitempty"`

	// heartbeat
	Active int `json:"active,omitempty"` // requests in flight on this node
	// Paused: the volunteer paused the node; it finishes requests in flight but takes no new
	// ones. Optional and added after v0.1 shipped: older gateways ignore it (the node then
	// answers new requests with an error, which those gateways retry on another node).
	Paused bool `json:"paused,omitempty"`

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
