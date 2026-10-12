// Self-registered nodes (decision 0007): signed-in accounts create node tokens on the account
// page, and the gateway routes chats to their nodes as untrusted volunteers. "Trusted" means
// operated by the maintainer (tokens in the gateway config), not secure.
//
// A node token is "nt-<node id>.<secret>": the gateway assigns the id ("n-<8 hex>"), so a
// node needs only its token, and the id cannot be chosen or squatted. Only a SHA-256 hash of
// the token is stored, in the accounts file. Revoking a token, banning its account or
// pausing its node takes effect within seconds (sweepNodes), without a restart.
//
// SPDX-License-Identifier: Apache-2.0
package gateway

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"

	"github.com/coder/websocket"
)

// Limits on self-registered nodes (research/node-registration, NOTES §4).
const (
	DefaultMaxNodes      = 10 // node tokens over all accounts, unless the accounts file says otherwise
	nodeTokensPerAccount = 3
	maxNodesPerIP        = 3 // connected account nodes per client address (IPv6: per /64)
	maxNodeName          = 40
)

// RoutingTrustedOnly is the emergency setting: every request goes to trusted nodes only.
const RoutingTrustedOnly = "trusted-only"

// Node preferences of an account, for its own chat and API requests.
const (
	NodePrefOwn  = ""     // prefer my own nodes, then any (the default)
	NodePrefOnly = "only" // only my own nodes
	NodePrefAny  = "any"  // no preference
)

// AccountNode is a node token of an account; only its hash is stored.
type AccountNode struct {
	ID      string    `json:"id"`             // "n-<8 hex>", assigned by the gateway
	Name    string    `json:"name,omitempty"` // the volunteer's label, shown only to them and the operator
	Hash    string    `json:"sha256"`
	Prefix  string    `json:"prefix"`
	Created time.Time `json:"created"`
}

// NodeHold records a node stopped by a guard (guard.go). It gets no requests until the
// operator clears it (`accounts node-clear`); the record is the evidence, without content.
type NodeHold struct {
	Node     string    `json:"node"`
	Operator string    `json:"operator"` // the account's pseudonym, or the config token's operator
	Rule     string    `json:"rule"`     // which guard fired, e.g. "download-and-execute"
	At       time.Time `json:"at"`
}

type nodeRef struct {
	acct *Account
	node *AccountNode
}

var (
	errNodesClosed = errors.New("New nodes are not being accepted right now. Please try again later.")
	errNetworkFull = errors.New("The network is full for now: no new nodes are being added. Please try again later.")
)

// splitNodeToken returns the node id of a token of the form "nt-<id>.<secret>".
func splitNodeToken(token string) (id string, ok bool) {
	rest, ok := strings.CutPrefix(token, "nt-")
	if !ok {
		return "", false
	}
	id, secret, ok := strings.Cut(rest, ".")
	return id, ok && validNodeID(id) && len(secret) >= 40
}

// validNodeID reports whether id has the form the gateway assigns: "n-" and 8 hex digits.
func validNodeID(id string) bool {
	h, ok := strings.CutPrefix(id, "n-")
	return ok && len(h) == 8 && strings.Trim(h, "0123456789abcdef") == ""
}

func newNodeToken(id string) (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return "nt-" + id + "." + base64.RawURLEncoding.EncodeToString(b), nil
}

// cleanNodeName keeps a label short and printable.
func cleanNodeName(s string) string {
	s = strings.Map(func(r rune) rune {
		if unicode.IsPrint(r) {
			return r
		}
		return -1
	}, strings.TrimSpace(s))
	if r := []rune(s); len(r) > maxNodeName {
		s = string(r[:maxNodeName])
	}
	return s
}

// nodeCount counts node tokens over all accounts.
func (f *AccountsFile) nodeCount() int {
	n := 0
	for _, a := range f.Accounts {
		n += len(a.Nodes)
	}
	return n
}

// MaxNodesOrDefault is the limit on node tokens over all accounts.
func (s AccountSettings) MaxNodesOrDefault() int {
	if s.MaxNodes > 0 {
		return s.MaxNodes
	}
	return DefaultMaxNodes
}

// nodeIDUsed reports whether id belongs, or belonged, to a node (ids are never reused, so
// a pause record or log line always means one node).
func (f *AccountsFile) nodeIDUsed(id string) bool {
	for _, a := range f.Accounts {
		for _, n := range a.Nodes {
			if n.ID == id {
				return true
			}
		}
	}
	for _, p := range f.NodeHolds {
		if p.Node == id {
			return true
		}
	}
	return false
}

// AddNode creates a node token for an active account and returns it (shown once) and the
// node id. valid checks the form's one-time token against NodeSeq, as AddKey does.
func (s *AccountStore) AddNode(id, name string, now time.Time, valid func(seq int) bool) (token, nodeID string, err error) {
	err = s.Update(func(f *AccountsFile) error {
		a := f.find(id)
		switch {
		case a == nil || a.Status != StatusActive:
			return errors.New("only active accounts can add nodes")
		case valid != nil && !valid(a.NodeSeq):
			return errStaleForm
		case f.Settings.NodesClosed:
			return errNodesClosed
		case len(a.Nodes) >= nodeTokensPerAccount:
			return fmt.Errorf("at most %d nodes per account; revoke one first", nodeTokensPerAccount)
		case f.nodeCount() >= f.Settings.MaxNodesOrDefault():
			return errNetworkFull
		}
		for nodeID == "" || f.nodeIDUsed(nodeID) {
			b := make([]byte, 4)
			rand.Read(b)
			nodeID = "n-" + hex.EncodeToString(b)
		}
		var terr error
		if token, terr = newNodeToken(nodeID); terr != nil {
			return terr
		}
		a.NodeSeq++
		t := now.UTC()
		if a.FirstNode == nil {
			a.FirstNode = &t
		}
		a.Nodes = append(a.Nodes, AccountNode{ID: nodeID, Name: cleanNodeName(name), Hash: keyHash(token),
			Prefix: token[:len("nt-")+len(nodeID)+1+4], Created: t})
		return nil
	})
	return token, nodeID, err
}

// RevokeNode deletes a node token. account "" finds the node in any account (operator).
func (s *AccountStore) RevokeNode(account, nodeID string) error {
	return s.Update(func(f *AccountsFile) error {
		for _, a := range f.Accounts {
			if account != "" && a.ID != account {
				continue
			}
			for i, n := range a.Nodes {
				if n.ID == nodeID {
					a.Nodes = append(a.Nodes[:i], a.Nodes[i+1:]...)
					return nil
				}
			}
		}
		return fmt.Errorf("no node %s", nodeID)
	})
}

// HoldNode stops a node until ClearNode (a guard fired on its answer).
func (s *AccountStore) HoldNode(p NodeHold) error {
	return s.Update(func(f *AccountsFile) error {
		for _, q := range f.NodeHolds {
			if q.Node == p.Node {
				return nil
			}
		}
		f.NodeHolds = append(f.NodeHolds, p)
		return nil
	})
}

// ClearNode lets a held node serve again (operator).
func (s *AccountStore) ClearNode(nodeID string) error {
	return s.Update(func(f *AccountsFile) error {
		for i, p := range f.NodeHolds {
			if p.Node == nodeID {
				f.NodeHolds = append(f.NodeHolds[:i], f.NodeHolds[i+1:]...)
				return nil
			}
		}
		return fmt.Errorf("node %s is not held", nodeID)
	})
}

// SetNodePref sets an account's node preference.
func (s *AccountStore) SetNodePref(id, pref string) error {
	if pref != NodePrefOwn && pref != NodePrefOnly && pref != NodePrefAny {
		return errors.New("unknown node preference")
	}
	return s.Update(func(f *AccountsFile) error {
		a := f.find(id)
		if a == nil || a.Status != StatusActive {
			return errors.New("only active accounts can set this")
		}
		a.NodePref = pref
		return nil
	})
}

// nodeToken returns the active account and node id that token belongs to.
func (s *AccountStore) nodeToken(token string) (account, nodeID string, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, found := s.byNode[keyHash(token)]
	if !found || r.acct.Status != StatusActive {
		return "", "", false
	}
	return r.acct.ID, r.node.ID, true
}

// nodeLive reports whether the node token with hash still exists for an active account, and
// when that account's probation started.
func (s *AccountStore) nodeLive(tokenHash string) (first time.Time, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, found := s.byNode[tokenHash]
	if !found || r.acct.Status != StatusActive {
		return time.Time{}, false
	}
	if r.acct.FirstNode != nil {
		first = *r.acct.FirstNode
	}
	return first, true
}

// isHeld reports whether a guard holds the node.
func (s *AccountStore) isHeld(nodeID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.held[nodeID]
}

// nodeTotal counts node tokens over all accounts.
func (s *AccountStore) nodeTotal() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.f.nodeCount()
}

// Settings returns the operator's switches.
func (s *AccountStore) Settings() AccountSettings {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.f.Settings
}

// nodePref returns an account's node preference.
func (s *AccountStore) nodePref(id string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if a := s.f.find(id); a != nil {
		return a.NodePref
	}
	return NodePrefOwn
}

// ---------------------------------------------------------------- the gateway's side

// nodeAuth resolves a node token: one from the config, or an account's ("nt-...").
func (g *Gateway) nodeAuth(token string) (tok NodeToken, account string, ok bool) {
	g.mu.Lock()
	tok, ok = g.tokens[token]
	g.mu.Unlock()
	if ok {
		return tok, "", true
	}
	if _, form := splitNodeToken(token); !form || g.accounts == nil {
		return NodeToken{}, "", false
	}
	account, id, ok := g.accounts.nodeToken(token)
	if !ok {
		return NodeToken{}, "", false
	}
	return NodeToken{Operator: g.pseudonym(account), NodeID: id}, account, true
}

// nodesFromLocked counts connected account nodes from address ip, except node id. Caller
// holds g.mu.
func (g *Gateway) nodesFromLocked(ip, id string) int {
	c := 0
	for _, n := range g.node {
		if n.account != "" && n.ip == ip && n.id != id {
			c++
		}
	}
	return c
}

// nodeHeld reports whether a guard holds node id (accounts file, or memory without one).
func (g *Gateway) nodeHeld(id string) bool {
	if g.accounts != nil {
		return g.accounts.isHeld(id)
	}
	_, p := g.held.Load(id)
	return p
}

// holdNode stops n after a guard fired on its answer. The record holds no content.
func (g *Gateway) holdNode(n *nodeConn, rule, reqID string) {
	g.log.Warn("guard stopped a node's answer; node held", "node", n.id, "operator", n.operator, "rule", rule, "req", reqID)
	if g.accounts != nil {
		if err := g.accounts.HoldNode(NodeHold{Node: n.id, Operator: n.operator, Rule: rule, At: time.Now().UTC()}); err != nil {
			g.log.Error("holding node in the accounts file failed; it is held only until the next sweep", "node", n.id, "err", err)
		}
	} else {
		g.held.Store(n.id, true)
	}
	g.mu.Lock()
	n.held = true
	g.mu.Unlock()
}

// sweepNodes applies the accounts file to connected nodes: nodes whose token was revoked or
// whose account is no longer active are dropped (their requests in flight fail over), and
// pauses set or cleared by the operator take effect.
func (g *Gateway) sweepNodes() {
	g.mu.Lock()
	conns := make([]*nodeConn, 0, len(g.node))
	for _, n := range g.node {
		conns = append(conns, n)
	}
	g.mu.Unlock()
	type state struct {
		live  bool
		first time.Time
	}
	st := make([]state, len(conns))
	for i, n := range conns {
		st[i].live = true
		if n.account != "" {
			st[i].first, st[i].live = g.accounts.nodeLive(keyHash(n.token))
		}
	}
	var drop []*nodeConn
	cleared := false
	g.mu.Lock()
	for i, n := range conns {
		if !st[i].live {
			drop = append(drop, n)
			continue
		}
		p := g.nodeHeld(n.id) // read under g.mu: a pause set meanwhile is not undone
		cleared = cleared || (n.held && !p)
		n.held = p
		if n.account != "" {
			n.probEnd = st[i].first.Add(g.acfg.probation)
		}
	}
	g.mu.Unlock()
	for _, n := range drop {
		g.log.Info("node token revoked or account closed, disconnecting", "node", n.id)
		n.conn.Close(websocket.StatusPolicyViolation, "node token revoked")
	}
	if cleared {
		g.signal()
	}
}

// The share cap: no untrusted operator gets more than 1/maxShare of the requests placed in
// the last shareWindow while other nodes are free (a soft cap: when only its nodes are free,
// they serve rather than let the request wait). It limits what one bad volunteer sees.
const (
	shareWindow     = 10 * time.Minute
	maxShare        = 3
	maxShareEntries = 4096
)

type shareEntry struct {
	at time.Time
	op string
}

// overShareLocked prunes the window and reports, per operator, whether one more request
// would put it over its share. Caller holds g.mu.
func (g *Gateway) overShareLocked(now time.Time) func(op string) bool {
	i := 0
	for i < len(g.shares) && (now.Sub(g.shares[i].at) > shareWindow || len(g.shares)-i > maxShareEntries) {
		i++
	}
	g.shares = append(g.shares[:0], g.shares[i:]...)
	count := map[string]int{}
	for _, e := range g.shares {
		count[e.op]++
	}
	total := len(g.shares)
	return func(op string) bool { return (count[op]+1)*maxShare > total+1 }
}

// routeFor builds the placement policy of a chat request.
func (g *Gateway) routeFor(want string, body map[string]any, acct KeyInfo, reqAcct string, consent bool) *route {
	rt := &route{want: want, trusted: acct.MinTrust == "trusted", operator: acct.OnlyOperator, consent: consent,
		exclude: map[*nodeConn]bool{}}
	if hasTools(body) {
		rt.tools = true
	}
	if hasPII(body) {
		if !rt.trusted {
			rt.why = "requests that look like they contain personal data go only to trusted nodes"
		}
		rt.trusted = true
	}
	if g.accounts != nil {
		rt.trustedOnly = g.accounts.Settings().Routing == RoutingTrustedOnly
		if reqAcct != "" {
			rt.self = g.pseudonym(reqAcct)
			switch g.accounts.nodePref(reqAcct) {
			case NodePrefOnly: // on top of a front end's operator: if they differ, no node fits
				rt.onlySelf = true
			case NodePrefAny:
			default:
				rt.preferSelf = true
			}
		}
	}
	// Context: about 3 bytes per token of what the node sees (English text averages nearer
	// 4, so this errs towards more tokens; CJK text is about 3 bytes per token).
	prompt := len(mustJSON(forNode(body)))/3 + 16
	maxTok := requestMaxTokens(body, g.cfg.DefaultMaxTokens)
	rt.need, rt.needMin = prompt+maxTok, prompt+min(maxTok, minOutputRoom)
	return rt
}

// requestMaxTokens is the request's max_tokens (or max_completion_tokens), else def, else 16384.
func requestMaxTokens(body map[string]any, def int) int {
	maxTokens := def
	for _, k := range []string{"max_tokens", "max_completion_tokens"} {
		if v, ok := body[k].(float64); ok && v > 0 {
			maxTokens = int(v)
		} else if v, ok := body[k].(int); ok && v > 0 {
			maxTokens = v
		}
	}
	if maxTokens <= 0 {
		maxTokens = 16384
	}
	return maxTokens
}

// Node states shown to the volunteer.
const (
	nodeOnline    = "online"
	nodeBusy      = "busy"
	nodeOffline   = "offline"
	nodeProbation = "probation"
	nodeOnHold    = "held"   // stopped by a guard until the operator clears it
	nodePaused    = "paused" // paused by the volunteer (local node UI)
)

// nodeStatus is the state of an account's node, with a generic reason only.
func (g *Gateway) nodeStatus(account, id string) string {
	if g.accounts.isHeld(id) {
		return nodeOnHold
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	n := g.node[id]
	if n == nil || n.account != account {
		return nodeOffline
	}
	slots := 0
	for _, m := range n.models {
		slots += m.Slots
	}
	n.mu.Lock()
	busy, paused := n.inflight >= slots, n.paused
	n.mu.Unlock()
	if paused {
		return nodePaused
	}
	if time.Now().Before(n.probEnd) {
		return nodeProbation
	}
	if busy {
		return nodeBusy
	}
	return nodeOnline
}
