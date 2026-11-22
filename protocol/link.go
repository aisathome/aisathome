// Linking a node to an account without pasting a token (PROTOCOL.md, "Linking a node to an account"): two
// HTTPS calls before the node has a token, a device code flow in the style of RFC 8628.
//
// SPDX-License-Identifier: Apache-2.0
package protocol

// Paths of the link calls, on the gateway's HTTPS address (the connect URL's host).
const (
	LinkPath     = "/v1/node/link"
	LinkPollPath = "/v1/node/link/poll"
)

// LinkInfo is what a node says about itself when it asks for a link code (POST LinkPath). The person who
// approves sees it; the gateway checks none of it.
type LinkInfo struct {
	Name    string `json:"name,omitempty"`     // a suggested node name (the host name), which the person may change
	GPU     string `json:"gpu,omitempty"`      // e.g. "NVIDIA GeForce RTX 4090"
	VRAMMiB int    `json:"vram_mib,omitempty"` // the card's memory
	OS      string `json:"os,omitempty"`       // e.g. "Ubuntu 24.04.1 LTS", "Windows 11 (26100)"
	Agent   string `json:"agent,omitempty"`    // the node software, e.g. "aisathome-node/0.9.0"
}

// LinkStart is the answer to POST LinkPath (RFC 8628 §3.2's names). The node keeps DeviceCode to itself
// and shows the person UserCode and VerificationURIComplete (VerificationURI is where to type the code).
type LinkStart struct {
	DeviceCode              string `json:"device_code"`
	UserCode                string `json:"user_code"`
	VerificationURI         string `json:"verification_uri"`
	VerificationURIComplete string `json:"verification_uri_complete"`
	ExpiresIn               int    `json:"expires_in"` // seconds
	Interval                int    `json:"interval"`   // seconds between polls
}

// LinkPollRequest is the body of POST LinkPollPath.
type LinkPollRequest struct {
	DeviceCode string `json:"device_code"`
}

// LinkPoll is the answer to POST LinkPollPath.
type LinkPoll struct {
	Status   string `json:"status"`             // LinkPending, LinkSlowDown, LinkApproved, LinkDenied or LinkExpired
	Interval int    `json:"interval,omitempty"` // seconds to wait before the next poll (pending, slow_down)
	Token    string `json:"token,omitempty"`    // approved: the node token, given this once
	NodeID   string `json:"node_id,omitempty"`  // approved: the node id the token carries
}

// LinkPoll.Status values.
const (
	LinkPending  = "pending"   // not approved yet: poll again after Interval
	LinkSlowDown = "slow_down" // polled too fast: Interval grew
	LinkApproved = "approved"  // Token is the node's token; the link is used up
	LinkDenied   = "denied"    // the person denied it
	LinkExpired  = "expired"   // unknown, expired or already used: ask for a new code
)
