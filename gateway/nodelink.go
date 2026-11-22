// Linking a node to an account without pasting a token: a device code, approved in the browser (RFC 8628
// style; owner, 2026-11-22). The node asks for a code (POST /v1/node/link) and shows the person a short
// user code and a link; the person signs in on /link, sees what the node reported about itself and
// approves or denies; the node polls (POST /v1/node/link/poll) and gets its node token once.
//
// Approving creates the node exactly as "Add a node" on the account page does (AccountStore.addNode): the
// same limits (active accounts only, nodes closed, per account, network-wide), and the same operator
// terms line, so creating it accepts the terms unless the account chose science only. The node's record
// says it came by a link (AccountNode.Via), and an acceptance recorded with it says so too (ViaNodeLinked).
// Chat stays behind the operator terms exactly as for any node of the account: a link adds nothing to that.
//
// Codes live in memory only, for linkTTL, and are used once. Both the device code (the node's secret
// for polling) and the user code are kept as HMACs under a key made at start, never in clear; a restart
// drops pending links. The node token exists in clear only between the approval and the node's next
// poll, in memory, and is dropped once handed over; a token the node never picked up is revoked when its
// link expires. Limits, all in memory (the privacy notice: addresses are used in memory for limits and
// not stored): codes asked per address and network-wide, pending links at once, wrong codes typed per
// account and per address, and polls faster than the interval (slow_down).
//
// The risk that stays (RFC 8628 §5.4, remote phishing): someone sends a person their own code, and the
// person approves it. That links the sender's machine to the person's account as one of its nodes: the
// person's own requests may then go to it like to any of their nodes. The page says what is being
// approved, shows the node's own details and says to approve only a code a machine of one's own shows.
//
// SPDX-License-Identifier: Apache-2.0
package gateway

import (
	"bytes"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"mime"
	"net/http"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/aisathome/aisathome/protocol"
)

const (
	linkTTL      = 10 * time.Minute // a code works this long, once
	linkInterval = 5 * time.Second  // how often the node may poll
	linkSlowDown = 5 * time.Second  // added to the interval each time it polls too fast (RFC 8628 §3.5)
	linkMaxWait  = time.Minute      // the interval never grows past this
	linkPickup   = time.Minute      // an approved node has at least this long to pick up its token
	linkMaxOpen  = 500              // pending links at once, over the whole gateway
	maxLinkBody  = 4 << 10
	// linkPruneEvery: how often expired links are dropped (a poll after expiry already gets "expired").
	linkPruneEvery = 15 * time.Second
	// ViaNodeLink is AccountNode.Via for a node that came by a link.
	ViaNodeLink = "link"
	// userCodeLetters: RFC 8628 §6.1's 20 consonants (no vowels, so no words; no I or O to mistake for
	// digits).
	userCodeLetters = "BCDFGHJKLMNPQRSTVWXZ"
)

// Limits on links (rule, events: email.go). Wrong codes count per signed-in account and per address; a
// code can only be typed by a signed-in, active account.
var (
	linkPerClient = []rule{{20, 10 * time.Minute}, {60, time.Hour}} // a LAN of machines behind one address, retries
	linkNetwork   = []rule{{300, time.Hour}}
	linkMissAcct  = []rule{{10, time.Hour}}
	linkMissAddr  = []rule{{30, time.Hour}}
)

var (
	errLinkBusy    = errors.New("too many link codes asked from this address; wait a few minutes and try again")
	errLinkFull    = errors.New("too many nodes are linking right now; try again in a few minutes")
	errLinkUnknown = errors.New("This code is unknown or has expired. A code works for 10 minutes and once: start the link again on the node.")
	errLinkTries   = errors.New("Too many wrong codes. Try again in an hour.")
	errLinkUsed    = errors.New("This code was already approved or denied. To link the node, start the link again on it.")
	errLinkForm    = errors.New("This page is out of date. Open the link again.")
)

// LinkInfo is what a node reports about itself (protocol.LinkInfo).
type LinkInfo = protocol.LinkInfo

// cleanLinkInfo keeps the fields short and printable.
func cleanLinkInfo(i LinkInfo) LinkInfo {
	return LinkInfo{Name: cleanText(i.Name, maxNodeName), GPU: cleanText(i.GPU, 80), OS: cleanText(i.OS, 80),
		Agent: cleanText(i.Agent, 60), VRAMMiB: min(max(i.VRAMMiB, 0), 1<<24)}
}

func cleanText(s string, n int) string {
	s = strings.Map(func(r rune) rune {
		if unicode.IsPrint(r) {
			return r
		}
		return -1
	}, strings.TrimSpace(s))
	if r := []rune(s); len(r) > n {
		s = string(r[:n])
	}
	return s
}

type linkState int

const (
	linkPending   linkState = iota
	linkApproving           // being created (addNode runs outside the store's lock)
	linkApproved            // created: the token waits for the node's next poll
	linkDenied
)

// nodeLink is one pending link.
type nodeLink struct {
	id       string // random, not secret: ties the approval form to this link
	device   string // HMAC of the device code
	user     string // HMAC of the user code (normalised)
	info     LinkInfo
	created  time.Time
	expires  time.Time
	lastPoll time.Time
	interval time.Duration
	state    linkState
	account  string // who approved or denied
	token    string // approved: the node token until the node picks it up
	nodeID   string
}

// linkView is what the approval page shows of a link.
type linkView struct {
	ID      string
	Info    LinkInfo
	Created time.Time
	Expires time.Time
}

// nodeLinks holds the pending links and the limits' counters, in memory only.
type nodeLinks struct {
	key []byte
	now func() time.Time

	mu       sync.Mutex
	byDevice map[string]*nodeLink
	byUser   map[string]*nodeLink
	starts   events // "c:<address>", "*"
	misses   events // "a:<account>", "c:<address>"
}

func newNodeLinks() *nodeLinks {
	key := make([]byte, 32)
	rand.Read(key)
	return &nodeLinks{key: key, now: time.Now, byDevice: map[string]*nodeLink{}, byUser: map[string]*nodeLink{},
		starts: events{}, misses: events{}}
}

func (s *nodeLinks) mac(kind, v string) string {
	m := hmac.New(sha256.New, s.key)
	m.Write([]byte(kind + "|" + v))
	return hex.EncodeToString(m.Sum(nil))
}

// newUserCode returns a code like "KXQT-4821": four letters of userCodeLetters and four digits.
func newUserCode() string {
	var b strings.Builder
	for i := 0; i < 8; i++ {
		if i == 4 {
			b.WriteByte('-')
		}
		set := userCodeLetters
		if i >= 4 {
			set = "0123456789"
		}
		n, _ := rand.Int(rand.Reader, big.NewInt(int64(len(set))))
		b.WriteByte(set[n.Int64()])
	}
	return b.String()
}

// normUserCode reads a typed code: any case, with or without the dash or spaces, O for 0 and I or L for 1
// in the digits. ok is false for anything else.
func normUserCode(s string) (string, bool) {
	s = strings.Map(func(r rune) rune {
		if r == '-' || unicode.IsSpace(r) {
			return -1
		}
		return unicode.ToUpper(r)
	}, s)
	if len(s) != 8 {
		return "", false
	}
	b := []byte(s)
	for i := range b {
		if i < 4 {
			if !strings.ContainsRune(userCodeLetters, rune(b[i])) {
				return "", false
			}
			continue
		}
		switch b[i] {
		case 'O':
			b[i] = '0'
		case 'I', 'L':
			b[i] = '1'
		}
		if b[i] < '0' || b[i] > '9' {
			return "", false
		}
	}
	return string(b[:4]) + "-" + string(b[4:]), true
}

// start makes a link for a node at address client: its device code (the node's secret) and user code.
func (s *nodeLinks) start(client string, info LinkInfo) (device, user string, err error) {
	now := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.starts.allows("c:"+client, now, linkPerClient) {
		return "", "", errLinkBusy
	}
	if !s.starts.allows("*", now, linkNetwork) || len(s.byDevice) >= linkMaxOpen {
		return "", "", errLinkFull
	}
	device = randomToken()
	for user == "" || s.byUser[s.mac("user", user)] != nil {
		user = newUserCode()
	}
	b := make([]byte, 12)
	rand.Read(b)
	l := &nodeLink{id: base64.RawURLEncoding.EncodeToString(b), device: s.mac("device", device), user: s.mac("user", user),
		info: cleanLinkInfo(info), created: now, expires: now.Add(linkTTL), interval: linkInterval}
	s.byDevice[l.device], s.byUser[l.user] = l, l
	s.starts.add("c:"+client, now)
	s.starts.add("*", now)
	return device, user, nil
}

// find returns the pending link of a typed code for account at address client. Too many wrong codes
// from the account or the address refuse every code for a while; each unknown or expired code counts.
// Caller holds s.mu.
func (s *nodeLinks) findLocked(account, client, code string, now time.Time) (*nodeLink, error) {
	if !s.misses.allows("a:"+account, now, linkMissAcct) || !s.misses.allows("c:"+client, now, linkMissAddr) {
		return nil, errLinkTries
	}
	norm, ok := normUserCode(code)
	var l *nodeLink
	if ok {
		l = s.byUser[s.mac("user", norm)]
	}
	if l == nil || !now.Before(l.expires) {
		s.misses.add("a:"+account, now)
		s.misses.add("c:"+client, now)
		return nil, errLinkUnknown
	}
	if l.state != linkPending {
		return nil, errLinkUsed
	}
	return l, nil
}

// lookup returns what the approval page shows of a typed code.
func (s *nodeLinks) lookup(account, client, code string) (linkView, error) {
	now := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()
	l, err := s.findLocked(account, client, code, now)
	if err != nil {
		return linkView{}, err
	}
	return linkView{ID: l.id, Info: l.info, Created: l.created, Expires: l.expires}, nil
}

// claim marks the link of code (shown in the form with id) as being approved by account, so that no
// second approval or a denial can run meanwhile. finish ends it.
func (s *nodeLinks) claim(account, client, code, id string) (*nodeLink, LinkInfo, error) {
	now := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()
	l, err := s.findLocked(account, client, code, now)
	if err != nil {
		return nil, LinkInfo{}, err
	}
	if l.id != id {
		return nil, LinkInfo{}, errLinkForm
	}
	l.state, l.account = linkApproving, account
	return l, l.info, nil
}

// finish ends an approval that claim began: with the new node's token, or, when creating the node failed,
// back to pending (the person may fix what stopped it, e.g. revoke a node, and approve again). It reports
// false when the link expired meanwhile: the caller then revokes the node it created.
func (s *nodeLinks) finish(l *nodeLink, token, nodeID string, failed bool) bool {
	now := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.byDevice[l.device] != l || l.state != linkApproving {
		return false
	}
	if failed {
		l.state, l.account = linkPending, ""
		return true
	}
	if !now.Before(l.expires) {
		s.dropLocked(l)
		return false
	}
	l.state, l.token, l.nodeID = linkApproved, token, nodeID
	l.expires = maxTime(l.expires, now.Add(linkPickup))
	return true
}

func maxTime(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

// deny refuses the link of code: its node gets no token.
func (s *nodeLinks) deny(account, client, code, id string) error {
	now := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()
	l, err := s.findLocked(account, client, code, now)
	if err != nil {
		return err
	}
	if l.id != id {
		return errLinkForm
	}
	l.state, l.account = linkDenied, account
	return nil
}

// Poll answers (protocol.LinkPoll.Status).
const (
	LinkPending  = protocol.LinkPending
	LinkSlowDown = protocol.LinkSlowDown
	LinkApproved = protocol.LinkApproved
	LinkDenied   = protocol.LinkDenied
	LinkExpired  = protocol.LinkExpired
)

// poll answers a node's poll with its device code. An approved link hands over its token once and is gone.
func (s *nodeLinks) poll(device string) (status string, interval time.Duration, token, nodeID string) {
	now := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()
	l := s.byDevice[s.mac("device", device)]
	if l == nil || !now.Before(l.expires) {
		return LinkExpired, 0, "", "" // an approved one is revoked by prune
	}
	fast := !l.lastPoll.IsZero() && now.Sub(l.lastPoll) < l.interval*4/5
	l.lastPoll = now
	switch {
	case l.state == linkApproved:
		token, nodeID = l.token, l.nodeID
		l.token = ""
		s.dropLocked(l)
		return LinkApproved, 0, token, nodeID
	case l.state == linkDenied:
		s.dropLocked(l)
		return LinkDenied, 0, "", ""
	case fast:
		l.interval = min(l.interval+linkSlowDown, linkMaxWait)
		return LinkSlowDown, l.interval, "", ""
	}
	return LinkPending, l.interval, "", ""
}

func (s *nodeLinks) dropLocked(l *nodeLink) {
	delete(s.byDevice, l.device)
	delete(s.byUser, l.user)
}

// orphan is a node whose token was never picked up.
type orphan struct{ account, nodeID string }

// prune forgets expired links and old counts. It returns the nodes created for links whose token was never
// picked up: nobody holds their token, so the caller revokes them.
func (s *nodeLinks) prune() []orphan {
	now := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []orphan
	for _, l := range s.byDevice {
		if now.Before(l.expires) || l.state == linkApproving {
			continue
		}
		if l.state == linkApproved {
			out = append(out, orphan{l.account, l.nodeID})
		}
		s.dropLocked(l)
	}
	s.starts.prune(now)
	s.misses.prune(now)
	return out
}

// pruneLinks runs prune and revokes the nodes nobody picked up.
func (g *Gateway) pruneLinks() {
	for _, o := range g.links.prune() {
		if err := g.accounts.RevokeNode(o.account, o.nodeID); err != nil {
			g.log.Warn("link expired before the node picked up its token; revoking that node failed", "node", o.nodeID, "err", err)
			continue
		}
		g.log.Info("link expired before the node picked up its token; node revoked", "node", o.nodeID, "operator", g.pseudonym(o.account))
	}
}

// ---------------------------------------------------------------- HTTP: the node's side

// decodeLinkBody reads a small JSON body. Unknown fields are ignored: newer nodes may report more.
func decodeLinkBody(w http.ResponseWriter, r *http.Request, v any) bool {
	if mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || mt != "application/json" {
		apiError(w, http.StatusUnsupportedMediaType, "send application/json")
		return false
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxLinkBody))
	if err != nil {
		apiError(w, http.StatusRequestEntityTooLarge, fmt.Sprintf("at most %d bytes", maxLinkBody))
		return false
	}
	if len(bytes.TrimSpace(body)) == 0 {
		body = []byte("{}")
	}
	if err := json.Unmarshal(body, v); err != nil {
		apiError(w, http.StatusBadRequest, "not a valid request: "+err.Error())
		return false
	}
	return true
}

// handleLinkStart serves POST /v1/node/link: a node without a token asks for a code. No authentication:
// the code is worth nothing until a signed-in person approves it.
func (g *Gateway) handleLinkStart(w http.ResponseWriter, r *http.Request) {
	var info LinkInfo
	if !decodeLinkBody(w, r, &info) {
		return
	}
	device, user, err := g.links.start(g.clientIP(r), info)
	if err != nil {
		w.Header().Set("Retry-After", "300")
		apiError(w, http.StatusTooManyRequests, err.Error())
		return
	}
	base := g.acfg.PublicURL + "/link"
	writeJSON(w, http.StatusOK, protocol.LinkStart{DeviceCode: device, UserCode: user, VerificationURI: base,
		VerificationURIComplete: base + "?code=" + user, ExpiresIn: int(linkTTL / time.Second), Interval: int(linkInterval / time.Second)})
}

// handleLinkPoll serves POST /v1/node/link/poll {"device_code"}.
func (g *Gateway) handleLinkPoll(w http.ResponseWriter, r *http.Request) {
	var body protocol.LinkPollRequest
	if !decodeLinkBody(w, r, &body) {
		return
	}
	if body.DeviceCode == "" || len(body.DeviceCode) > 64 {
		apiError(w, http.StatusBadRequest, "device_code: the code POST /v1/node/link gave")
		return
	}
	status, interval, token, nodeID := g.links.poll(body.DeviceCode)
	switch status {
	case LinkApproved:
		g.log.Info("linked node picked up its token", "node", nodeID)
	case LinkExpired:
		g.pruneLinks() // a node approved too late is revoked now, not at the next prune
	}
	w.Header().Set("Cache-Control", "no-store") // a token must not end up in a cache
	writeJSON(w, http.StatusOK, protocol.LinkPoll{Status: status, Interval: int(interval / time.Second), Token: token, NodeID: nodeID})
}
