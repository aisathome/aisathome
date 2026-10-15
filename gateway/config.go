// Gateway configuration and node credentials.
//
// SPDX-License-Identifier: Apache-2.0
package gateway

import (
	"encoding/json"
	"time"
)

// Config holds gateway settings. In M1 keys and tokens come from a config file; accounts and
// payments come later.
type Config struct {
	GatewayID  string               `json:"gateway_id"`
	APIKeys    map[string]string    `json:"api_keys"`    // API key → user id (no limits)
	Keys       map[string]KeyInfo   `json:"keys"`        // API key → user and limits
	NodeTokens map[string]NodeToken `json:"node_tokens"` // node token → operator (and node id)
	UsageLog   string               `json:"usage_log"`   // JSONL file, one line per request
	// FrontendAddr is a private listener (e.g. ":8081") for the trusted front end (Open
	// WebUI). Keys with forward_users work only there, never on the public listener, so a
	// leaked front-end key cannot speak for other users. Empty = no front end.
	FrontendAddr string `json:"frontend_addr"`
	// KeysFile holds per-user keys (stored hashed) managed with `aisathome-gateway keys`;
	// changes apply within a few seconds, without a reload. Empty = none.
	KeysFile string `json:"keys_file"`
	// StateDir keeps state that must survive a restart (daily token budgets), so a restart
	// does not hand every key a fresh day. Empty = memory only.
	StateDir string `json:"state_dir"`
	// Accounts enables GitHub and/or Google sign-in, self-service API keys and the waitlist
	// (see accounts.go).
	Accounts *AccountsConfig `json:"accounts,omitempty"`
	// QueueTimeoutSec is how long a request waits for a free slot, or for a node serving a
	// recently seen model to (re)connect, before failing with 503. Default 30.
	QueueTimeoutSec int `json:"queue_timeout_seconds"`
	// MaxQueued caps the requests waiting at once; more fail at once with 503. Default 64.
	MaxQueued int `json:"max_queued"`
	// MaxNodesPerToken caps simultaneous node connections per node token. Default 8.
	MaxNodesPerToken int `json:"max_nodes_per_token"`
	// DefaultMaxTokens is applied when a request sets no max_tokens/max_completion_tokens,
	// so one request cannot generate (or think) until a volunteer's context is full.
	// Default 16384; -1 disables it.
	DefaultMaxTokens int `json:"default_max_tokens"`
	// MaxMaxTokens caps what a client may request. Default 32768.
	MaxMaxTokens int `json:"max_max_tokens"`
	// FirstChunkTimeoutSec and IdleTimeoutSec end a request whose node stalls (before the
	// first chunk, and between chunks); before any output it is retried on another node.
	// Slightly longer than the node agent's own engine timeouts. Defaults 330 and 150.
	FirstChunkTimeoutSec int `json:"first_chunk_timeout_seconds"`
	IdleTimeoutSec       int `json:"idle_timeout_seconds"`
	// TokenizerFile is the model's tokenizer.json (Qwen3.8's; README "Credit"). With it the
	// gateway counts the tokens of each answer itself, for credit; without it credit uses the
	// node's reported counts, clamped, and the usage log says counted "node".
	TokenizerFile string `json:"tokenizer_file"`
	// TokenizerSHA256, if set, must match the tokenizer file (hex), or the gateway won't start.
	TokenizerSHA256 string `json:"tokenizer_sha256"`
	// HeartbeatTimeout drops nodes that stay silent for longer than this.
	HeartbeatTimeout time.Duration `json:"-"`
	// StatsRefresh, if set, replaces how often the leaderboard (a minute) and the BOINC-format
	// exports (an hour) are rebuilt. For tests.
	StatsRefresh time.Duration `json:"-"`
}

// NodeToken authorises node connections. In JSON it is either a plain operator id (any
// node id may use the token) or {"operator": "...", "node_id": "..."} to bind it to one node.
type NodeToken struct {
	Operator string `json:"operator"`
	NodeID   string `json:"node_id,omitempty"`
	// Trust "trusted" marks the maintainer's own nodes (not "secure"): they serve MinTrust
	// keys, requests carrying personal data, and agents without consent. Volunteers, and
	// every node registered from the account page, are untrusted.
	Trust string `json:"trust,omitempty"`
}

// UnmarshalJSON accepts both the plain-string and the object form.
func (t *NodeToken) UnmarshalJSON(b []byte) error {
	var s string
	if json.Unmarshal(b, &s) == nil {
		*t = NodeToken{Operator: s}
		return nil
	}
	type plain NodeToken
	return json.Unmarshal(b, (*plain)(t))
}
