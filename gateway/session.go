// A session stays on one model file (owner, 2026-11-09). Nodes of different tiers serve the
// same roles ("deep", "agent") from different files (a 24 GB node UD-IQ4_XS, a 16 GB node
// UD-Q3_K_XL) under different model names. Once a conversation has an answer from one model,
// its later requests for a role go only to nodes serving that same model: a silent change of
// model in the middle of an agent session or a chat would change its answers and re-read the
// whole conversation.
//
// A session is recognised the way the prefix cache recognises a conversation (prefixcache.go),
// since clients send no common session id: a request continues a session when its rendered
// prompt is longer than, and starts with all of, an earlier answered conversation of the same
// requester (account, end user behind the chat, API key or science project): that request's
// prompt and its answer, byte for byte to the end. So a new conversation that only shares a
// system prompt (or its first question) with an earlier one is not taken for it. The answer
// reappears in a later prompt in one of three forms: with its reasoning (a tool result follows
// and the client sends the reasoning back), with empty reasoning (a client that does not), or
// without (a new user message follows: the template drops earlier turns' reasoning); all three
// are kept. Secret placeholders (privacy.go) carry a nonce that changes with every request; they
// are hashed in a form without it, so a conversation with a hidden credential keeps its session.
// The gateway keeps no text: per requester, the length and two hashes of each form of each
// answered conversation, with the model that answered.
//
// Requests that continue no session (the first of a session) are routed as before. A request
// that names a model rather than a role is not held to the session's model: it chose one.
//
// SPDX-License-Identifier: Apache-2.0
package gateway

import (
	"encoding/binary"
	"hash/maphash"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/aisathome/aisathome/protocol"
)

const (
	// bindTTL is how long a session keeps its model after its last answer. A session paused for
	// longer (a chat taken up again the next day) is routed as a new one.
	bindTTL = 24 * time.Hour
	// bindPerWho bounds the answered conversations remembered per requester. Each turn of a
	// session adds one, and the older turns stay so that a regenerated or edited answer is
	// still recognised; a requester running many sessions at once (a science project) keeps
	// the latest turn of each as long as fewer than this many turns come in between.
	bindPerWho = 128
	// bindWhos bounds the requesters remembered (least recently active go first): at most
	// bindPerWho*bindWhos entries of about 150 bytes, 20 MiB.
	bindWhos = 1024
	// bindBlock is the length of the hashed blocks: a lookup hashes the prompt once, then each
	// remembered conversation costs one comparison and the hash of under bindBlock bytes.
	bindBlock = 256
)

// placeholderRe matches a secret placeholder's body after its kind prefix ("AISX" + 4 hex + 2-3
// digits + "X" padding to the secret's length, privacy.go). Nonce and number change from request
// to request for the same secret, and with them the padding: number 100 is one digit longer
// than 99, so one "X" shorter.
var placeholderRe = regexp.MustCompile(`AISX[0-9a-f]{4}[0-9]{2,3}X*`)

// nonceFree is s with every secret placeholder's body (nonce, number and padding) replaced by a
// fixed text, keeping only the kind prefix before it: the form a conversation is hashed in.
// (The secrets themselves never reach this: s is redacted.)
func nonceFree(s string) string {
	if !strings.Contains(s, "AISX") {
		return s
	}
	return placeholderRe.ReplaceAllLiteralString(s, "AISX#")
}

// convMark is a conversation as a later prompt must start: its length, the chained hash of its
// whole blocks and the hash of the rest, chained to it (all of the nonce-free text).
type convMark struct {
	n    int // bytes; 0 = no such form
	full uint64
	tail uint64
}

// binding is one answered conversation, in up to three forms, and the model that answered it.
type binding struct {
	forms [3]convMark
	model string   // the model name the node serves it under
	roles []string // that model's roles on the node, so a request for a role finds it
	at    time.Time
}

type whoBinds struct {
	list []binding // oldest first
	used time.Time
}

// sessionBinds remembers which model answered each requester's conversations.
type sessionBinds struct {
	mu   sync.Mutex
	seed maphash.Seed
	by   map[string]*whoBinds
}

func newSessionBinds() *sessionBinds {
	return &sessionBinds{seed: maphash.MakeSeed(), by: map[string]*whoBinds{}}
}

// step chains the hash of chunk to prev.
func (s *sessionBinds) step(prev uint64, chunk string) uint64 {
	var h maphash.Hash
	h.SetSeed(s.seed)
	var b [8]byte
	binary.LittleEndian.PutUint64(b[:], prev)
	h.Write(b[:])
	h.WriteString(chunk)
	return h.Sum64()
}

// chain returns the chained hashes of t's whole blocks: [k] covers blocks 0..k.
func (s *sessionBinds) chain(t string) []uint64 {
	out := make([]uint64, len(t)/bindBlock)
	prev := uint64(0)
	for k := range out {
		prev = s.step(prev, t[k*bindBlock:(k+1)*bindBlock])
		out[k] = prev
	}
	return out
}

// mark returns the mark of conversation conv (zero for "").
func (s *sessionBinds) mark(conv string) convMark {
	t := nonceFree(conv)
	if t == "" {
		return convMark{}
	}
	full := uint64(0)
	if ch := s.chain(t); len(ch) > 0 {
		full = ch[len(ch)-1]
	}
	k := len(t) / bindBlock
	return convMark{n: len(t), full: full, tail: s.step(full, t[k*bindBlock:])}
}

// continued reports whether the nonce-free prompt t, whose chain is ch, continues c: it is
// longer and starts with all of it.
func (s *sessionBinds) continued(c convMark, t string, ch []uint64) bool {
	if c.n == 0 || len(t) <= c.n {
		return false
	}
	k, prev := c.n/bindBlock, uint64(0)
	if k > 0 {
		prev = ch[k-1]
	}
	return prev == c.full && s.step(prev, t[k*bindBlock:c.n]) == c.tail
}

// bind records that model m answered a conversation; forms are its text as a later request
// can start (meter.conversations: the prompt and the answer, rendered the ways it reappears).
func (s *sessionBinds) bind(who string, forms []string, m protocol.ModelInfo, now time.Time) {
	if s == nil || who == "" || m.Name == "" {
		return
	}
	b := binding{model: m.Name, roles: slices.Clone(m.Roles), at: now}
	n := 0
	for i, f := range forms {
		if f != "" && n < len(b.forms) && !slices.Contains(forms[:i], f) {
			b.forms[n] = s.mark(f)
			n++
		}
	}
	if n == 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	w := s.by[who]
	if w == nil {
		if len(s.by) >= bindWhos {
			var oldest string
			for k, x := range s.by {
				if oldest == "" || x.used.Before(s.by[oldest].used) {
					oldest = k
				}
			}
			delete(s.by, oldest)
		}
		w = &whoBinds{}
		s.by[who] = w
	}
	w.used = now
	live := w.list[:0]
	for _, b := range w.list {
		if now.Sub(b.at) <= bindTTL {
			live = append(live, b)
		}
	}
	w.list = live
	if len(w.list) >= bindPerWho {
		w.list = w.list[1:]
	}
	w.list = append(w.list, b)
}

// model returns the model a request must stay on: the one that answered the longest earlier
// conversation of who that the request's rendered prompt continues (in any of its forms), if
// that model serves want. "" = none: route as usual.
func (s *sessionBinds) model(who, prompt, want string, now time.Time) string {
	if s == nil || who == "" || prompt == "" {
		return ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	w := s.by[who]
	if w == nil {
		return ""
	}
	t := nonceFree(prompt)
	var ch []uint64 // hashed once, on the first live entry
	best, bestLen := binding{}, 0
	for _, b := range w.list {
		if now.Sub(b.at) > bindTTL {
			continue
		}
		if ch == nil {
			ch = s.chain(t)
		}
		for _, f := range b.forms {
			if s.continued(f, t, ch) && (f.n > bestLen || (f.n == bestLen && b.at.After(best.at))) {
				best, bestLen = b, f.n
			}
		}
	}
	if best.model == "" || (best.model != want && !contains(best.roles, want)) {
		return ""
	}
	return best.model
}

// modelInfo returns n's offer named name (zero if it has none).
func (n *nodeConn) modelInfo(name string) protocol.ModelInfo {
	for _, m := range n.models {
		if m.Name == name {
			return m
		}
	}
	return protocol.ModelInfo{}
}
