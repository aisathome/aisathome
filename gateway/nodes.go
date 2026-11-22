// Self-registered nodes (decision 0007): signed-in accounts create node tokens on the account
// page, and the gateway routes chats to their nodes as untrusted volunteers. Every node is untrusted,
// config tokens' too (decision 0007, addendum 2026-11-20); only an account's own nodes may get its
// personal data (routing.go).
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
	"slices"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/coder/websocket"
)

// Limits on self-registered nodes.
const (
	// DefaultMaxNodes and DefaultMaxNodesPerAccount apply unless the accounts file says
	// otherwise. Owner, 2026-10-22: 25, 10 and 10 per address, up from 10, 3 and 3. The share
	// cap (overShareLocked) limits how much of the traffic one operator sees, however many
	// nodes it has; low node limits only kept volunteers with several GPUs out.
	DefaultMaxNodes           = 25 // node tokens over all accounts
	DefaultMaxNodesPerAccount = 10 // node tokens per account
	maxNodesPerIP             = 10 // connected account nodes per client address (IPv6: per /64)
	maxNodeName               = 40
)

// RoutingOwnOnly is the emergency setting: every request goes only to its sender's own nodes, so no
// stranger's node serves anything (science neither: a project has no nodes of its own).
// routingTrustedOnly is its name before trusted nodes were removed (2026-11-20), still read from an
// accounts file: it sent every request to trusted nodes only, which with none means the same now.
const (
	RoutingOwnOnly     = "own-only"
	routingTrustedOnly = "trusted-only"
)

// setOwnOnly records the emergency setting as read now (routeFor, sweepNodes), for requests that wait
// (route.ownOnly); turned off, they look for a node again.
func (g *Gateway) setOwnOnly(on bool) {
	if g.ownOnlyNow.Swap(on) && !on {
		g.signal()
	}
}

// OwnOnly reports whether the emergency setting is on.
func (s AccountSettings) OwnOnly() bool {
	return s.Routing == RoutingOwnOnly || s.Routing == routingTrustedOnly
}

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
	// Via says how the node came: ViaNodeLink (approved on /link, nodelink.go), or "" (the account page's
	// "Add a node").
	Via string `json:"via,omitempty"`
	// Confirmed is when the account confirmed on its account page that a linked node is its own machine
	// ("This is my machine"). Until then a linked node is not the account's own node (unconfirmed).
	Confirmed *time.Time `json:"confirmed,omitempty"`
	// LastSeen is when a node without an account (anon.go) was last connected, for its retention.
	LastSeen *time.Time `json:"last_seen,omitempty"`
}

// unconfirmed reports whether n came by a link (a code someone may have sent the account's owner) and
// its owner has not yet confirmed that it is their machine. Such a node is not the account's own: it gets
// none of the account's personal data, and serves the account's requests only as a stranger's node would
// (route.own). Owner, 2026-11-22.
func (n AccountNode) unconfirmed() bool { return n.Via == ViaNodeLink && n.Confirmed == nil }

// NodeHold records a node stopped by a guard (guard.go). It gets no requests until the
// operator clears it (`accounts node-clear`).
type NodeHold struct {
	Node     string `json:"node"`
	Operator string `json:"operator"` // the account's pseudonym, or the config token's operator
	Rule     string `json:"rule"`     // which guard fired, e.g. "download-and-execute"
	// Reason says which pattern of the rule fired, in the guard's own fixed words (guardRules),
	// e.g. "netcat sending a file to a host and port": nothing of the request or the answer.
	// Empty in holds recorded before it existed.
	Reason string    `json:"reason,omitempty"`
	At     time.Time `json:"at"`
	Req    string    `json:"req,omitempty"` // the request whose answer was stopped (its usage record)
	// Evidence is what matched in the node's answer (guard_evidence.go): a fragment, secrets
	// redacted, never the request. Dropped EvidenceKeep after At; the hold stays.
	Evidence *GuardEvidence `json:"evidence,omitempty"`
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

// validNodeID reports whether id has a form the gateway assigns: "n-" (an account's node) or "a-" (a node
// without an account, anon.go), and 8 hex digits.
func validNodeID(id string) bool {
	h, ok := strings.CutPrefix(id, "n-")
	if !ok {
		h, ok = strings.CutPrefix(id, anonNodePrefix)
	}
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

// nodeCount counts node tokens over all accounts; nodes without an account have their own pool
// (anonCount).
func (f *AccountsFile) nodeCount() int {
	n := 0
	for _, a := range f.Accounts {
		if !isAnon(a.ID) {
			n += len(a.Nodes)
		}
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

// MaxNodesPerAccountOrDefault is the limit on node tokens per account.
func (s AccountSettings) MaxNodesPerAccountOrDefault() int {
	if s.MaxNodesPerAccount > 0 {
		return s.MaxNodesPerAccount
	}
	return DefaultMaxNodesPerAccount
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
	token, nodeID, _, err = s.addNode(id, name, now, valid, false)
	return token, nodeID, err
}

// addNode is AddNode; with terms (the form showed the line that creating the node accepts the current
// operator terms) it records that acceptance in the same write, as acceptOnNodeCreate would
// (chatrules.go), and reports whether it did.
func (s *AccountStore) addNode(id, name string, now time.Time, valid func(seq int) bool, terms bool) (token, nodeID string, recorded bool, err error) {
	return s.addNodeVia(id, name, now, valid, terms, "")
}

// addNodeVia is addNode for a node that came by via (AccountNode.Via): ViaNodeLink for an approved link,
// whose acceptance of the operator terms, if it records one, says so (ViaNodeLinked).
func (s *AccountStore) addNodeVia(id, name string, now time.Time, valid func(seq int) bool, terms bool, via string) (token, nodeID string, recorded bool, err error) {
	err = s.Update(func(f *AccountsFile) error {
		a := f.find(id)
		switch {
		case a == nil || a.Status != StatusActive:
			return errors.New("only active accounts can add nodes")
		case valid != nil && !valid(a.NodeSeq):
			return errStaleForm
		case f.Settings.NodesClosed:
			return errNodesClosed
		case len(a.Nodes) >= f.Settings.MaxNodesPerAccountOrDefault():
			return fmt.Errorf("at most %d nodes per account; revoke one first", f.Settings.MaxNodesPerAccountOrDefault())
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
			Prefix: token[:len("nt-")+len(nodeID)+1+4], Created: t, Via: via})
		how := ViaNodeCreated
		if via == ViaNodeLink {
			how = ViaNodeLinked
		}
		recorded = terms && a.acceptOnNodeCreate(t, how)
		return nil
	})
	if err != nil {
		recorded = false
	}
	return token, nodeID, recorded, err
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
	_, err := s.clearHold(nodeID, time.Time{})
	return err
}

// errHoldChanged: the node's hold is not the one the operator saw (cleared and held again since).
var errHoldChanged = errors.New("this node's hold changed since the page was loaded: reload it and look again")

// clearHold is ClearNode returning the hold it lifted; with a non-zero at only the hold made
// then, the one the operator saw (the ops page), so a newer hold is never cleared unseen.
func (s *AccountStore) clearHold(nodeID string, at time.Time) (NodeHold, error) {
	var lifted NodeHold
	err := s.Update(func(f *AccountsFile) error {
		for i, p := range f.NodeHolds {
			if p.Node == nodeID {
				if !at.IsZero() && !p.At.Equal(at) {
					return errHoldChanged
				}
				lifted = p
				f.NodeHolds = append(f.NodeHolds[:i], f.NodeHolds[i+1:]...)
				return nil
			}
		}
		return fmt.Errorf("node %s is not held in the accounts file (a hold the gateway could not write "+
			"yet is written within seconds once the file is writable: then clear it)", nodeID)
	})
	return lifted, err
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

// nodeToken returns the account and node id that token belongs to: an active account's, or a node
// without an account's (anon.go).
func (s *AccountStore) nodeToken(token string) (account, nodeID string, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, found := s.byNode[keyHash(token)]
	if !found || !r.acct.nodesWork() {
		return "", "", false
	}
	return r.acct.ID, r.node.ID, true
}

// nodeLive reports whether the node token with hash still exists for an active account (or a node without
// an account), when that account's probation started, and whether the node is a linked one its owner has
// not confirmed yet.
func (s *AccountStore) nodeLive(tokenHash string) (first time.Time, unconfirmed, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, found := s.byNode[tokenHash]
	if !found || !r.acct.nodesWork() {
		return time.Time{}, false, false
	}
	if r.acct.FirstNode != nil {
		first = *r.acct.FirstNode
	}
	return first, r.node.unconfirmed(), true
}

// ConfirmNode records that account id confirmed its linked node nodeID as its own machine (once; a second
// confirmation keeps the first time).
func (s *AccountStore) ConfirmNode(id, nodeID string, now time.Time) error {
	return s.Update(func(f *AccountsFile) error {
		a := f.find(id)
		if a == nil || a.Status != StatusActive {
			return errors.New("only active accounts can confirm nodes")
		}
		for i := range a.Nodes {
			if n := &a.Nodes[i]; n.ID == nodeID {
				if n.Confirmed == nil {
					t := now.UTC()
					n.Confirmed = &t
				}
				return nil
			}
		}
		return fmt.Errorf("no node %s", nodeID)
	})
}

// confirmedNodes reports whether account id has registered nodes that are its own (not a linked node
// waiting for its owner's confirmation).
func (s *AccountStore) confirmedNodes(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if a := s.f.find(id); a != nil {
		for _, n := range a.Nodes {
			if !n.unconfirmed() {
				return true
			}
		}
	}
	return false
}

// isHeld reports whether a guard holds the node.
func (s *AccountStore) isHeld(nodeID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.held[nodeID]
}

// nodeHolds lists the holds in the accounts file.
func (s *AccountStore) nodeHolds() []NodeHold {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.f.NodeHolds)
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
	account, id, ok := g.accounts.nodeToken(token) // a node without an account's too (anon.go)
	if !ok {
		return NodeToken{}, "", false
	}
	return NodeToken{Operator: g.pseudonym(account), NodeID: id}, account, true
}

// nodesFromLocked counts connected nodes from address ip, except node id: account nodes, or with anon nodes
// without an account (they have their own limit, maxAnonPerIP). Caller holds g.mu.
func (g *Gateway) nodesFromLocked(ip, id string, anon bool) int {
	c := 0
	for _, n := range g.node {
		if n.account != "" && n.anon == anon && n.ip == ip && n.id != id {
			c++
		}
	}
	return c
}

// holds lists every hold on record, oldest first, the node connected or not: a hold nobody
// looked at must not vanish because the node left. A hold the accounts file has not recorded
// yet counts too.
func (g *Gateway) holds() []NodeHold {
	byNode := map[string]NodeHold{}
	if g.accounts != nil {
		for _, p := range g.unrec.list() {
			byNode[p.Node] = p
		}
		for _, p := range g.accounts.nodeHolds() {
			byNode[p.Node] = p
		}
	} else {
		g.held.Range(func(_, v any) bool {
			p := v.(NodeHold)
			byNode[p.Node] = p
			return true
		})
	}
	out := make([]NodeHold, 0, len(byNode))
	for _, p := range byNode {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].At.Equal(out[j].At) {
			return out[i].At.Before(out[j].At)
		}
		return out[i].Node < out[j].Node
	})
	return out
}

// nodeHeld reports whether a guard holds node id (accounts file, or memory without one). A
// hold the accounts file has not recorded yet counts too: the file alone would lift it.
func (g *Gateway) nodeHeld(id string) bool {
	if g.accounts != nil {
		return g.accounts.isHeld(id) || g.unrec.has(id)
	}
	_, p := g.held.Load(id)
	return p
}

// holdNode stops n after a guard fired on its answer. The log line holds no content; the record
// and the guard log keep the evidence (guard_evidence.go).
func (g *Gateway) holdNode(n *nodeConn, hit guardHit, reqID string) {
	msg := "guard stopped a node's answer; node held"
	if hit.rule == ruleModelCheck {
		msg = "model check failed; node held"
	}
	g.log.Warn(msg, "node", n.id, "operator", n.operator, "rule", hit.rule,
		"reason", hit.why, "req", reqID)
	ev := guardEvent(n, hit, reqID, GuardHeld, time.Now().UTC())
	g.glog.add(ev)
	evidence := ev.GuardEvidence
	p := NodeHold{Node: n.id, Operator: n.operator, Rule: hit.rule, Reason: hit.why, At: ev.At, Req: reqID, Evidence: &evidence}
	if g.accounts != nil {
		if err := g.accounts.HoldNode(p); err != nil {
			g.log.Error("HOLD NOT RECORDED: writing the node's hold to the accounts file failed; it is held in "+
				"memory and retried until recorded, and a restart before then lets it serve again", "node", n.id, "err", err)
			if !g.unrec.add(p) {
				g.log.Error("HOLD NOT RECORDED: too many unrecorded holds; every node is held until the gateway restarts",
					"max", maxUnrecorded)
			}
		}
	} else {
		g.held.LoadOrStore(n.id, p) // the first hold stands, as in the accounts file
	}
	g.mu.Lock()
	was := n.held
	n.held = true
	g.mu.Unlock()
	if !was {
		g.press.hold(time.Now())
	}
	if n.pilot != "" {
		why := "guard: " + hit.rule
		if hit.rule == ruleModelCheck {
			why = "model check"
		}
		g.withholdPilotCredit(n.pilot, why)
	}
}

// maxUnrecorded bounds the holds kept in memory while the accounts file cannot be written.
const maxUnrecorded = 1024

// unrecordedHolds are guard holds whose write to the accounts file failed. They hold their
// node like recorded ones (sweepNodes ORs them with the file), and recordHolds retries the
// write; each is dropped only once the file has it. So a write failure never lifts a hold:
// only the operator does, with `accounts node-clear`, once the hold is in the file. A
// restart forgets the ones still unrecorded, which is logged loudly until they are recorded.
type unrecordedHolds struct {
	mu      sync.Mutex
	m       map[string]NodeHold
	full    bool      // more than maxUnrecorded: every node held, until restart (fail closed)
	lastLog time.Time // of the last failed retry, to log each minute rather than each try
}

// add keeps p unless its node already has an unrecorded hold; false if the set is full.
func (u *unrecordedHolds) add(p NodeHold) bool {
	u.mu.Lock()
	defer u.mu.Unlock()
	if _, ok := u.m[p.Node]; ok {
		return true
	}
	if len(u.m) >= maxUnrecorded {
		u.full = true
		return false
	}
	if u.m == nil {
		u.m = map[string]NodeHold{}
	}
	u.m[p.Node] = p
	return true
}

func (u *unrecordedHolds) has(id string) bool {
	u.mu.Lock()
	defer u.mu.Unlock()
	_, ok := u.m[id]
	return ok || u.full
}

func (u *unrecordedHolds) list() []NodeHold {
	u.mu.Lock()
	defer u.mu.Unlock()
	l := make([]NodeHold, 0, len(u.m))
	for _, p := range u.m {
		l = append(l, p)
	}
	return l
}

// recorded drops p once the file has it (not a newer hold of the same node).
func (u *unrecordedHolds) recorded(p NodeHold) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if q, ok := u.m[p.Node]; ok && q.At.Equal(p.At) && q.Rule == p.Rule {
		delete(u.m, p.Node)
	}
}

// failed reports whether a failed retry should be logged now (once a minute).
func (u *unrecordedHolds) failed(now time.Time) bool {
	u.mu.Lock()
	defer u.mu.Unlock()
	if now.Sub(u.lastLog) < time.Minute {
		return false
	}
	u.lastLog = now
	return true
}

// recordHolds retries writing the unrecorded holds to the accounts file.
func (g *Gateway) recordHolds() {
	l := g.unrec.list()
	for i, p := range l {
		if err := g.accounts.HoldNode(p); err != nil {
			if g.unrec.failed(time.Now()) {
				g.log.Error("HOLD NOT RECORDED: guard holds are still not in the accounts file; they hold in memory "+
					"only, and a restart now lets those nodes serve again", "holds", len(l)-i, "err", err)
			}
			return
		}
		g.unrec.recorded(p)
		g.log.Warn("guard hold recorded in the accounts file", "node", p.Node, "rule", p.Rule)
	}
}

// sweepNodes applies the accounts file to connected nodes: nodes whose token was revoked or
// whose account is no longer active are dropped (their requests in flight fail over), and
// pauses set or cleared by the operator take effect. Sweeps run one at a time: one that read the
// accounts file earlier must not publish its older state over a newer sweep's (a withdrawal of the
// operator terms sweeps at once, while the periodic sweep may be running).
func (g *Gateway) sweepNodes() {
	g.sweepMu.Lock()
	defer g.sweepMu.Unlock()
	if g.accounts != nil {
		g.setOwnOnly(g.accounts.Settings().OwnOnly()) // the emergency switch reaches waiting requests too
	}
	g.mu.Lock()
	conns := make([]*nodeConn, 0, len(g.node))
	for _, n := range g.node {
		conns = append(conns, n)
	}
	g.mu.Unlock()
	type state struct {
		live        bool
		first       time.Time
		unconfirmed bool
		scienceOnly bool
		rules       nodeRules
	}
	st := make([]state, len(conns))
	for i, n := range conns {
		st[i].live = true
		switch {
		case n.pilot != "": // its account and connector as they are now (connector.go)
			var chatOff bool
			st[i].first, chatOff, st[i].live = g.accounts.pilotAccountState(n.account, n.connector, n.extUser)
			c := g.connector(n.connector)
			st[i].live = st[i].live && c != nil
			st[i].scienceOnly = c == nil || pilotScienceOnly(c, n.extUser, chatOff)
		case n.account != "":
			st[i].first, st[i].unconfirmed, st[i].live = g.accounts.nodeLive(keyHash(n.token))
		}
		if n.account != "" { // the chat rules: the account may have accepted or withdrawn (chatrules.go)
			st[i].rules = g.rulesFor(n.account, NodeToken{})
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
		if n.account != "" && n.pilot == "" {
			cleared = cleared || (n.unconfirmed && !st[i].unconfirmed) // confirmed: its owner's waiting requests may go to it
			n.unconfirmed = st[i].unconfirmed
		}
		if n.pilot != "" {
			cleared = cleared || (n.scienceOnly && !st[i].scienceOnly)
			n.scienceOnly = st[i].scienceOnly
		}
		if n.account != "" {
			r := st[i].rules
			cleared = cleared || (r.terms && !n.rules.terms)
			n.rules = r
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

// The share cap: no operator gets more than 1/maxShare of the conversations placed
// in the last shareWindow while other nodes are free (a soft cap: when only its nodes are
// free, they serve rather than let the request wait). It limits how many people's
// conversations one bad volunteer sees. A conversation counts once per window, not once per
// request: an agent session's many requests stick to the node holding its prompt cache,
// and counting each would push that operator over its share and move the session away.
const (
	shareWindow     = 10 * time.Minute
	maxShare        = 3
	maxShareEntries = 4096
)

// shareEntry is one conversation placed on an operator's node: when, and its convKey
// (0 = too short to recognise later; such requests each count).
type shareEntry struct {
	at   time.Time
	op   string
	conv uint64
}

// overShareLocked prunes the window and reports, per operator, whether one more conversation
// would put it over its share (over), and whether conversation conv was already counted
// for it in the window (seen). Caller holds g.mu.
func (g *Gateway) overShareLocked(now time.Time, conv uint64) (over, seen func(op string) bool) {
	i := 0
	for i < len(g.shares) && (now.Sub(g.shares[i].at) > shareWindow || len(g.shares)-i > maxShareEntries) {
		i++
	}
	g.shares = append(g.shares[:0], g.shares[i:]...)
	count := map[string]int{}
	var had map[string]bool
	for _, e := range g.shares {
		count[e.op]++
		if conv != 0 && e.conv == conv {
			if had == nil {
				had = map[string]bool{}
			}
			had[e.op] = true
		}
	}
	total := len(g.shares)
	return func(op string) bool { return (count[op]+1)*maxShare > total+1 },
		func(op string) bool { return had[op] }
}

// routeFor builds the placement policy of a chat request.
func (g *Gateway) routeFor(want string, body map[string]any, acct KeyInfo, reqAcct string, consent bool) *route {
	rt := &route{want: want, operator: acct.OnlyOperator, consent: consent, exclude: map[*nodeConn]bool{}}
	if hasTools(body) {
		rt.tools = true
	}
	rt.pii = piiKind(body) // personal data: only the requester's own nodes, or refused (routing.go)
	rt.rules = g.chatRulesOn()
	if g.accounts != nil {
		g.setOwnOnly(g.accounts.Settings().OwnOnly())
		rt.ownOnlyNow = &g.ownOnlyNow
		if reqAcct != "" {
			rt.self, rt.acct = g.pseudonym(reqAcct), reqAcct
			switch g.accounts.nodePref(reqAcct) {
			case NodePrefOnly: // on top of a front end's operator: if they differ, no node fits
				rt.onlySelf = true
			case NodePrefAny:
			default:
				rt.preferSelf = true
			}
		}
	}
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

// nodeVersion is the software version that account's node id reported when it connected,
// without the "aisathome-node/" prefix ("" if it reported none), and the protocol version
// it speaks; "" and 0 while it is offline.
func (g *Gateway) nodeVersion(account, id string) (string, int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	n := g.node[id]
	if n == nil || n.account != account {
		return "", 0
	}
	return strings.TrimPrefix(n.agent, "aisathome-node/"), n.proto
}

// nodeStatus is the state of an account's node, with a generic reason only.
func (g *Gateway) nodeStatus(account, id string) string {
	if g.nodeHeld(id) {
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
