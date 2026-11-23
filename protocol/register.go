// Nodes without an account (PROTOCOL.md, "A node without an account"): the installer registers the machine
// with no sign-in and gets a node token that serves science only; the node's local page asks the gateway
// what it knows of the node.
//
// SPDX-License-Identifier: Apache-2.0
package protocol

import "time"

// Paths on the gateway's HTTPS address (the connect URL's host).
const (
	RegisterPath   = "/v1/node/register"
	SelfPath       = "/v1/node/self"
	TokenKnownPath = "/v1/node/token-known"
)

// TokenKnownRequest is the body of POST TokenKnownPath: the SHA-256 (hex) of a node token, never the token.
// The gateway keeps only these hashes; a hash authenticates nowhere, so an installer may ask any gateway.
type TokenKnownRequest struct {
	SHA256 string `json:"sha256"`
}

// TokenKnown is the answer: whether a node token with that hash connects to this gateway now.
type TokenKnown struct {
	Known bool `json:"known"`
}

// RegisterRequest is the body of POST RegisterPath.
type RegisterRequest struct {
	// AcceptTerms: the person running the installer was shown that they must be 18 or older and that
	// running the node accepts the terms, and went on. Required.
	AcceptTerms bool   `json:"accept_terms"`
	Agent       string `json:"agent,omitempty"` // the node software, e.g. "aisathome-node/0.9.0"
}

// Register is the answer to POST RegisterPath: the node's token, given this once, and the node id it
// carries ("a-<8 hex>").
type Register struct {
	Token  string `json:"token"`
	NodeID string `json:"node_id"`
}

// NodeSelf is the answer to GET SelfPath (Authorization: Bearer <node token>).
type NodeSelf struct {
	NodeID       string `json:"node_id"`
	Anonymous    bool   `json:"anonymous"`              // a node without an account
	ScienceOnly  bool   `json:"science_only,omitempty"` // it serves science only (every node without an account)
	State        string `json:"state"`                  // SelfChecking, SelfAdmitted, SelfHeld, SelfOffline, SelfNoCheck or SelfServing
	ChecksPassed int    `json:"checks_passed"`          // model checks passed in its recent window
	ChecksNeeded int    `json:"checks_needed"`          // passed checks a node without an account needs before it gets work
	Message      string `json:"message,omitempty"`      // the state in a sentence, for people; never which probe or check failed
	// DeleteAt: a held node without an account is deleted then (24 hours after its hold) unless it is
	// reinstalled or linked before.
	DeleteAt *time.Time `json:"delete_at,omitempty"`
}

// NodeSelf.State values.
const (
	SelfChecking = "checking" // without an account: probe prompts only, until enough model checks passed
	SelfAdmitted = "admitted" // without an account: admitted, gets science work
	SelfHeld     = "held"     // held until the operator clears it
	SelfOffline  = "offline"  // not connected
	SelfNoCheck  = "no_check" // without an account: the gateway cannot check its model, so it gets no work
	SelfServing  = "serving"  // an account's node, connected
)
