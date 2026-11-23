// Nodes without an account (owner, 2026-11-22). The installer registers the machine with no sign-in
// (POST /v1/node/register); the gateway makes it an unclaimed account ("ax:<hex>") holding one node ("a-<8
// hex>") and hands the node its token once, stored hashed like any node token.
//
// What such a node may do, as hard rules (route.allows), not settings:
//   - Science only. No chat, API or agent request of a person ever goes to it, whatever the chat rules or
//     their config switch say: its operator accepted no operator terms, and there is nobody to accept them.
//   - No science until the model check admitted it: its first work is probe prompts only, from the admission
//     set (model_check.admit_references), never the secret set of the ongoing checks, and it is admitted
//     after anonAdmitChecks passed checks (modelcheck.go). A failed check before then holds it. Admitted, it
//     gets the secret set at random like any node. Without the model check, or with no admission set for its
//     model, it is never admitted (and registering is refused when the check is off).
//   - No credit: what it serves is not counted in the credit ledger (relay.go), so nothing moves when it is
//     linked to an account; credit starts with the account's own node.
//   - Not listed anywhere public.
//
// Linking it to an account later (nodelink.go): `aisathome-node link` on the node sends its token with the
// link request; approving gives the account a new node of its own, as any link does, and once the node has
// picked up its new token the anonymous identity is deleted. A linked node is the account's own only after
// "This is my machine".
//
// Limits, cheapest first. Per address (IPv4, or IPv6 /64) at most anonRegPerAddr registrations a day, and
// over the gateway anonRegNetwork an hour: both in memory only, as the privacy notice promises for
// addresses (a restart forgets them). A pool of max_anon_nodes (default 25) apart from the accounts'
// max_nodes; anon_closed (and nodes_closed) stop new registrations. At most maxAnonPerIP connected per
// address. Retention: a node that never connected is deleted anonUnconnected after it registered, one not
// seen for anonKeep is deleted, and a held one anonHeldKeep after its hold (sweepAnon).
//
// SPDX-License-Identifier: Apache-2.0
package gateway

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/aisathome/aisathome/protocol"
	"github.com/coder/websocket"
)

const (
	anonAccountPrefix = "ax:"
	anonNodePrefix    = "a-"
	// DefaultMaxAnonNodes is the pool of nodes without an account unless the accounts file says
	// otherwise (owner, 2026-11-22).
	DefaultMaxAnonNodes = 25
	maxAnonPerIP        = 2                    // connected nodes without an account per client address
	anonUnconnected     = 24 * time.Hour       // a registered node that never connected is deleted after this
	anonKeep            = 90 * 24 * time.Hour  // one not seen for this long is deleted
	anonHeldKeep        = 24 * time.Hour       // a held one is deleted this long after its hold, unless linked
	anonSweepEvery      = 10 * time.Minute     // how often sweepAnon runs
	anonAdmitChecks     = 3                    // passed model checks before a node without an account gets science
	anonRegPruneEvery   = time.Minute          // old registration counts are forgotten
	anonSeenEvery       = 6 * time.Hour        // how often the connected nodes' last-seen times are written
	anonSeenMin         = time.Hour            // a last-seen time is not rewritten sooner than this
	viaAnon             = "anonymous"          // AccountNode.Via of a node without an account
	anonHeldMsg         = "This node is held." // what a held account node is told (no reason)
)

// Registration limits (rule, events: email.go), in memory only.
var (
	anonRegPerAddr = []rule{{3, 24 * time.Hour}} // owner, 2026-11-22
	anonRegNetwork = []rule{{30, time.Hour}}
)

var (
	errAnonClosed = errors.New("nodes without an account are not being accepted right now; sign in to add a node, or try again later")
	errAnonFull   = errors.New("the pool for nodes without an account is full; sign in to add a node, or try again later")
	errAnonBusy   = errors.New("too many nodes registered from this address today; sign in to add more nodes")
	errAnonRush   = errors.New("too many nodes are registering right now; try again in an hour")
	errAnonOff    = errors.New("this gateway does not take nodes without an account (it has no model check to admit them); sign in to add a node")
)

// isAnon reports whether account is a node without an account's (anon.go).
func isAnon(account string) bool { return strings.HasPrefix(account, anonAccountPrefix) }

// nodesWork reports whether a's node tokens connect: an active account's, or a node without an account's.
func (a *Account) nodesWork() bool {
	return a.Status == StatusActive || (a.Status == StatusUnclaimed && isAnon(a.ID))
}

// MaxAnonNodesOrDefault is the pool of nodes without an account.
func (s AccountSettings) MaxAnonNodesOrDefault() int {
	if s.MaxAnonNodes > 0 {
		return s.MaxAnonNodes
	}
	return DefaultMaxAnonNodes
}

// anonCount counts the nodes without an account.
func (f *AccountsFile) anonCount() int {
	n := 0
	for _, a := range f.Accounts {
		if isAnon(a.ID) {
			n += len(a.Nodes)
		}
	}
	return n
}

// RegisterAnon makes a node without an account: an unclaimed "ax:" account with one node, and the node's
// token (shown once).
func (s *AccountStore) RegisterAnon(now time.Time) (token, nodeID string, err error) {
	err = s.Update(func(f *AccountsFile) error {
		switch {
		case f.Settings.NodesClosed || f.Settings.AnonClosed:
			return errAnonClosed
		case f.anonCount() >= f.Settings.MaxAnonNodesOrDefault():
			return errAnonFull
		}
		for nodeID == "" || f.nodeIDUsed(nodeID) {
			b := make([]byte, 4)
			rand.Read(b)
			nodeID = anonNodePrefix + hex.EncodeToString(b)
		}
		var terr error
		if token, terr = newNodeToken(nodeID); terr != nil {
			return terr
		}
		id := ""
		for id == "" || f.find(id) != nil {
			b := make([]byte, 16)
			rand.Read(b)
			id = anonAccountPrefix + hex.EncodeToString(b)
		}
		t := now.UTC()
		f.Accounts = append(f.Accounts, &Account{ID: id, Status: StatusUnclaimed, Created: t, FirstNode: &t, NodeSeq: 1,
			Nodes: []AccountNode{{ID: nodeID, Hash: keyHash(token), Prefix: token[:len("nt-")+len(nodeID)+1+4], Created: t, Via: viaAnon}}})
		return nil
	})
	if err != nil {
		return "", "", err
	}
	return token, nodeID, nil
}

// anonSeen records when the node without an account nodeID of account was last connected. A time written
// less than anonSeenMin ago is kept, so a node that reconnects often does not rewrite the file each time.
func (s *AccountStore) anonSeen(account, nodeID string, now time.Time) error {
	s.mu.Lock()
	fresh := false
	if r, ok := s.byNodeID(nodeID); ok && r.node.LastSeen != nil && now.Sub(*r.node.LastSeen) < anonSeenMin {
		fresh = true
	}
	s.mu.Unlock()
	if fresh {
		return nil
	}
	return s.Update(func(f *AccountsFile) error {
		if a := f.find(account); a != nil && isAnon(a.ID) {
			for i := range a.Nodes {
				if a.Nodes[i].ID == nodeID {
					t := now.UTC()
					a.Nodes[i].LastSeen = &t
				}
			}
		}
		return nil
	})
}

// removeAnon deletes a node without an account (its account and token): linked to an account, or expired.
func (s *AccountStore) removeAnon(account string) error {
	if !isAnon(account) {
		return errors.New("not a node without an account")
	}
	return s.Update(func(f *AccountsFile) error {
		for i, a := range f.Accounts {
			if a.ID == account {
				f.Accounts = append(f.Accounts[:i], f.Accounts[i+1:]...)
				return nil
			}
		}
		return nil
	})
}

// anonExpired reports whether the node without an account a is past its retention at now: it never
// connected within anonUnconnected of registering, or it was not seen for anonKeep, or it has no node left
// (revoked), or a hold (held: node id to when) is anonHeldKeep old: a held node would otherwise keep its
// pool slot (owner, 2026-11-22; linked to an account before then, it is no longer anonymous). A connected
// node (live) is not expired for being unseen. Banned ones stay, as banned accounts do.
func anonExpired(a *Account, now time.Time, live func(id string) bool, held map[string]time.Time) bool {
	if !isAnon(a.ID) || a.Status != StatusUnclaimed {
		return false
	}
	if len(a.Nodes) == 0 {
		return true
	}
	for _, n := range a.Nodes {
		if at, ok := held[n.ID]; ok && now.Sub(at) >= anonHeldKeep {
			return true
		}
	}
	for _, n := range a.Nodes {
		switch {
		case live(n.ID):
			return false
		case n.LastSeen == nil && now.Sub(n.Created) < anonUnconnected:
			return false
		case n.LastSeen != nil && now.Sub(*n.LastSeen) < anonKeep:
			return false
		}
	}
	return true
}

// expireAnon deletes the nodes without an account past their retention, and returns their node ids.
func (s *AccountStore) expireAnon(now time.Time, live func(id string) bool) ([]string, error) {
	heldAt := func(f *AccountsFile) map[string]time.Time {
		m := map[string]time.Time{}
		for _, h := range f.NodeHolds {
			m[h.Node] = h.At
		}
		return m
	}
	s.mu.Lock()
	any := false
	held := heldAt(&s.f)
	for _, a := range s.f.Accounts {
		any = any || anonExpired(a, now, live, held)
	}
	s.mu.Unlock()
	if !any { // nothing to delete: no write
		return nil, nil
	}
	var gone []string
	err := s.Update(func(f *AccountsFile) error {
		keep := f.Accounts[:0]
		held := heldAt(f)
		for _, a := range f.Accounts {
			if anonExpired(a, now, live, held) {
				for _, n := range a.Nodes {
					gone = append(gone, n.ID)
				}
				if len(a.Nodes) == 0 {
					gone = append(gone, a.ID)
				}
				continue
			}
			keep = append(keep, a)
		}
		f.Accounts = keep
		f.NodeHolds = dropHolds(f.NodeHolds, gone)
		return nil
	})
	return gone, err
}

// dropHolds removes the holds of nodes that are gone (deleted nodes without an account): nothing of them
// is kept, and no warning is left for a node that no longer exists.
func dropHolds(holds []NodeHold, gone []string) []NodeHold {
	g := map[string]bool{}
	for _, id := range gone {
		g[id] = true
	}
	keep := holds[:0]
	for _, h := range holds {
		if !g[h.Node] {
			keep = append(keep, h)
		}
	}
	return keep
}

// ---------------------------------------------------------------- the gateway's side

// anonRegs counts registrations per address and over the gateway, in memory only.
type anonRegs struct {
	mu sync.Mutex
	ev events // "c:<address>", "*"
}

// take counts a registration from client now, unless a limit is reached; undo gives it back (the
// registration failed).
func (r *anonRegs) take(client string, now time.Time) (undo func(), err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.ev == nil {
		r.ev = events{}
	}
	switch {
	case !r.ev.allows("c:"+client, now, anonRegPerAddr):
		return nil, errAnonBusy
	case !r.ev.allows("*", now, anonRegNetwork):
		return nil, errAnonRush
	}
	r.ev.add("c:"+client, now)
	r.ev.add("*", now)
	return func() {
		r.mu.Lock()
		defer r.mu.Unlock()
		r.ev.drop("c:"+client, now)
		r.ev.drop("*", now)
	}, nil
}

func (r *anonRegs) prune(now time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ev.prune(now)
}

func (g *Gateway) anonNow() time.Time {
	if g.cfg.AnonNow != nil {
		return g.cfg.AnonNow()
	}
	return time.Now()
}

// handleNodeRegister serves POST /v1/node/register (protocol.RegisterRequest): a node without an account.
// The installer calls it once its speed test passed.
func (g *Gateway) handleNodeRegister(w http.ResponseWriter, r *http.Request) {
	var body protocol.RegisterRequest
	if !decodeLinkBody(w, r, &body) { // at most maxLinkBody
		return
	}
	if !body.AcceptTerms {
		apiError(w, http.StatusBadRequest, "accept_terms: running a node means accepting the terms (aisathome.org/about/terms/); you must be 18 or older")
		return
	}
	if g.mc == nil {
		apiError(w, http.StatusServiceUnavailable, errAnonOff.Error())
		return
	}
	now := g.anonNow()
	undo, err := g.anonReg.take(g.clientIP(r), now)
	if err != nil {
		w.Header().Set("Retry-After", "3600")
		apiError(w, http.StatusTooManyRequests, err.Error())
		return
	}
	token, nodeID, err := g.accounts.RegisterAnon(now)
	if err != nil {
		undo()
		if errors.Is(err, errAnonClosed) || errors.Is(err, errAnonFull) {
			w.Header().Set("Retry-After", "3600")
			apiError(w, http.StatusServiceUnavailable, err.Error())
			return
		}
		g.log.Error("node without an account: accounts file not written", "err", err)
		apiError(w, http.StatusServiceUnavailable, "accounts unavailable, try again")
		return
	}
	g.log.Info("node without an account registered", "node", nodeID, "agent", cleanAgent(body.Agent))
	w.Header().Set("Cache-Control", "no-store") // a token must not end up in a cache
	writeJSON(w, http.StatusOK, protocol.Register{Token: token, NodeID: nodeID})
}

// sweepAnon deletes the nodes without an account past their retention.
//
// It holds sweepMu throughout, as sweepNodes does: deleting a held node also deletes its hold, and a sweep
// that read the node's token as live before the deletion and its hold as gone after it would let a
// guard-held node take requests until its connection closed. With the sweeps one at a time, the deleted
// nodes' connections are marked unusable (held) and closed here, before any sweep can see the hold gone.
func (g *Gateway) sweepAnon() {
	g.sweepMu.Lock()
	defer g.sweepMu.Unlock()
	g.mu.Lock()
	live := make(map[string]bool, len(g.node))
	for id := range g.node {
		live[id] = true
	}
	g.mu.Unlock()
	gone, err := g.accounts.expireAnon(g.anonNow(), func(id string) bool { return live[id] })
	if err != nil {
		g.log.Error("nodes without an account: expired ones not deleted; retried", "err", err)
		return
	}
	var drop []*nodeConn
	g.mu.Lock()
	for _, id := range gone {
		if n := g.node[id]; n != nil && n.anon {
			n.held = true // takes nothing more, until its connection is gone
			drop = append(drop, n)
		}
	}
	g.mu.Unlock()
	for _, id := range gone {
		g.log.Info("node without an account deleted (retention)", "node", id)
	}
	for _, n := range drop {
		if n.conn != nil {
			n.conn.Close(websocket.StatusPolicyViolation, "node deleted")
		}
	}
}

// noteAnonSeen writes when a node without an account was last connected (on connect and on leaving), for
// its retention.
func (g *Gateway) noteAnonSeen(n *nodeConn) {
	if !n.anon {
		return
	}
	if err := g.accounts.anonSeen(n.account, n.id, g.anonNow()); err != nil {
		g.log.Warn("node without an account: last connection not recorded", "node", n.id, "err", err)
	}
}

// anonSeenAll writes the last-seen time of the connected nodes without an account, so that one connected
// for months is not taken for one gone.
func (g *Gateway) anonSeenAll() {
	g.mu.Lock()
	var l []*nodeConn
	for _, n := range g.node {
		if n.anon {
			l = append(l, n)
		}
	}
	g.mu.Unlock()
	for _, n := range l {
		g.noteAnonSeen(n)
	}
}

// Node states for GET /v1/node/self (protocol.NodeSelf.State).
const (
	selfChecking = protocol.SelfChecking
	selfAdmitted = protocol.SelfAdmitted
	selfHeld     = protocol.SelfHeld
	selfOffline  = protocol.SelfOffline
	selfNoCheck  = protocol.SelfNoCheck
	selfServing  = protocol.SelfServing
)

// handleNodeSelf serves GET /v1/node/self: what the gateway knows of the node whose token comes with the
// request, for its local page. A generic reason only: never which probe or check failed.
func (g *Gateway) handleNodeSelf(w http.ResponseWriter, r *http.Request) {
	token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if _, form := splitNodeToken(token); !form || g.accounts == nil {
		apiError(w, http.StatusUnauthorized, "unknown node token")
		return
	}
	account, id, ok := g.accounts.nodeToken(token)
	if !ok {
		apiError(w, http.StatusUnauthorized, "unknown node token")
		return
	}
	s := protocol.NodeSelf{NodeID: id, Anonymous: isAnon(account), ChecksNeeded: anonAdmitChecks}
	if s.Anonymous {
		s.ScienceOnly = true
	}
	g.mu.Lock()
	n := g.node[id]
	connected := n != nil && n.account == account
	admitted := connected && n.admitted
	g.mu.Unlock()
	if g.mc != nil {
		s.ChecksPassed = g.mc.passedInWindow(id)
	}
	switch {
	case g.nodeHeld(id) && s.Anonymous:
		s.State = selfHeld
		s.Message, s.DeleteAt = g.anonHeldMessage(id)
	case g.nodeHeld(id):
		s.State, s.Message = selfHeld, anonHeldMsg+" It gets no work until the operator clears it: contact@aisathome.org."
	case !connected:
		s.State = selfOffline
	case !s.Anonymous:
		s.State = selfServing
	case admitted:
		s.State, s.Message = selfAdmitted, "Admitted: the model check passed; this node gets science work."
	case g.mc == nil || !g.mc.checks(n):
		s.State, s.Message = selfNoCheck, "The gateway has no model check for this node's model, so it cannot admit it: it gets no work."
	default:
		s.State, s.Message = selfChecking, "Checking the model: the gateway sends a few test prompts first; science work starts once enough checks passed."
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, s)
}

// setAdmitted records whether the model check admits the node without an account n; admitted, the requests
// waiting for science may go to it now.
func (g *Gateway) setAdmitted(n *nodeConn, ok bool) {
	g.mu.Lock()
	was := n.admitted
	n.admitted = ok
	g.mu.Unlock()
	if ok && !was {
		g.log.Info("node without an account admitted: it gets science now", "node", n.id)
		g.signal()
	}
}

// byNodeID finds the node with id. Caller holds s.mu.
func (s *AccountStore) byNodeID(id string) (nodeRef, bool) {
	for _, r := range s.byNode {
		if r.node.ID == id {
			return r, true
		}
	}
	return nodeRef{}, false
}

// anonHeldMessage tells the operator of a held node without an account why (the kind of check only, never
// which probe) and what to do, and when the node is deleted (anonHeldKeep after its hold).
func (g *Gateway) anonHeldMessage(id string) (string, *time.Time) {
	var hold *NodeHold
	for _, h := range g.holds() {
		if h.Node == id {
			h := h
			hold = &h
		}
	}
	install := "curl -fsSL " + g.acfg.PublicURL + "/node.sh | sh"
	var at *time.Time
	when := "24 hours after it was held"
	if hold != nil {
		t := hold.At.Add(anonHeldKeep).UTC()
		at, when = &t, "at "+t.Format("2006-01-02 15:04")+" UTC"
	}
	if hold != nil && hold.Rule == ruleModelCheck {
		return "Failed the model check before admission: this usually means an engine or settings different from the current " +
			"release. Reinstall with the latest version: " + install + " . This node is deleted " + when + "; a reinstall " +
			"registers it again.", at
	}
	return "Held after a safety check on one of its answers: it gets no work. This node is deleted " + when +
		"; if you think this is a mistake, write to contact@aisathome.org.", at
}

// handleTokenKnown serves POST /v1/node/token-known {"sha256"}: whether a node token with that SHA-256
// connects to this gateway now (an account's or a node without an account's, or one from the config). The
// installer asks before it decides what to do with an installed token whose issuer it cannot be sure of
// (node.json is editable): it sends the hash, never the token, so no gateway it asks learns a credential.
func (g *Gateway) handleTokenKnown(w http.ResponseWriter, r *http.Request) {
	var body protocol.TokenKnownRequest
	if !decodeLinkBody(w, r, &body) {
		return
	}
	h := strings.ToLower(body.SHA256)
	if len(h) != 64 || strings.Trim(h, "0123456789abcdef") != "" {
		apiError(w, http.StatusBadRequest, "sha256: the node token's SHA-256, 64 hex digits")
		return
	}
	var known bool
	if g.accounts != nil {
		account, nodeID, ok := g.accounts.tokenHashKnown(h)
		// A held node without an account is not worth keeping: its hold message tells the operator to
		// reinstall, and the reinstall then moves the token aside and registers a fresh node (subject to
		// the registration limits). An account's held node stays the account's.
		known = ok && !(isAnon(account) && g.nodeHeld(nodeID))
	}
	if !known {
		g.mu.Lock()
		for tok := range g.tokens {
			if keyHash(tok) == h {
				known = true
			}
		}
		g.mu.Unlock()
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, protocol.TokenKnown{Known: known})
}

// tokenHashKnown reports whether a node token with SHA-256 h connects now (nodeToken by hash), and whose.
func (s *AccountStore) tokenHashKnown(h string) (account, nodeID string, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, found := s.byNode[h]
	if !found || !r.acct.nodesWork() {
		return "", "", false
	}
	return r.acct.ID, r.node.ID, true
}
