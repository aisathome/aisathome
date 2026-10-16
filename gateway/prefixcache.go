// Prompt caching as the gateway sees it, for credit: a prompt counts as cached up to the
// longest prefix that the same node served for the same requester (account, end user behind
// the chat, or API key) within the last cacheWindow. What a node claims about its own cache
// is ignored. The gateway keeps no text: only chained hashes of 256-byte blocks of the
// rendered conversation, per node and requester, bounded in count and length.
//
// Routing reads the same record (routing.go): a conversation goes back to the node that holds
// the longest prefix of it, when that node is free, because each node's llama.cpp keeps its
// own prompt cache and a switch re-reads the whole conversation.
//
// SPDX-License-Identifier: Apache-2.0
package gateway

import (
	"hash/maphash"
	"sync"
	"time"
)

const (
	cacheWindow       = 10 * time.Minute
	cacheBlock        = 256  // bytes per hashed block
	cacheMaxBlocks    = 2048 // 512 KiB of conversation (about 128k tokens) per entry: 16 KiB of hashes
	cacheConvsPerNode = 32   // conversations remembered per node (least recently used go first)
	cacheMaxNodes     = 256  // so at most 128 MiB in all, and far less in practice
)

type convEntry struct {
	who    string   // the requester, hashed
	hashes []uint64 // hashes[k] covers blocks 0..k
	at     time.Time
}

type nodeConvs struct {
	convs []*convEntry // least recently used first
	used  time.Time
}

type prefixCache struct {
	mu    sync.Mutex
	seed  maphash.Seed
	nodes map[string]*nodeConvs
}

func newPrefixCache() *prefixCache {
	return &prefixCache{seed: maphash.MakeSeed(), nodes: map[string]*nodeConvs{}}
}

// convPrint is a request's conversation as the cache sees it, rendered and hashed once
// before the node is chosen, for routing and then for credit.
type convPrint struct {
	prompt string
	args   templateArgs
	hashes []uint64 // blockHashes(prompt)
}

// print renders and hashes nb, the request as a node gets it (forNode).
func (c *prefixCache) print(nb map[string]any) *convPrint {
	p := &convPrint{}
	p.prompt, p.args = renderPrompt(nb)
	p.hashes = c.blockHashes(p.prompt)
	return p
}

// blockHashes chains a hash over each whole block of s.
func (c *prefixCache) blockHashes(s string) []uint64 { return c.blockHashesFrom(nil, s) }

// blockHashesFrom is blockHashes(s), reusing have: the hashes of a prefix of s.
func (c *prefixCache) blockHashesFrom(have []uint64, s string) []uint64 {
	n := min(len(s)/cacheBlock, cacheMaxBlocks)
	out := make([]uint64, n)
	var prev [8]byte
	k := copy(out, have)
	if k > 0 {
		for i := range prev {
			prev[i] = byte(out[k-1] >> (8 * i))
		}
	}
	for ; k < n; k++ {
		var h maphash.Hash
		h.SetSeed(c.seed)
		h.Write(prev[:])
		h.WriteString(s[k*cacheBlock : (k+1)*cacheBlock])
		v := h.Sum64()
		out[k] = v
		for i := range prev {
			prev[i] = byte(v >> (8 * i))
		}
	}
	return out
}

func commonPrefix(a, b []uint64) int {
	n := 0
	for n < len(a) && n < len(b) && a[n] == b[n] {
		n++
	}
	return n
}

// served returns how many leading bytes of prompt node already served for who within the
// window, then remembers prompt+tail (the conversation as the next request will start), in
// place of an entry that it continues. remember=false only looks.
func (c *prefixCache) served(node, who, prompt, tail string, now time.Time, remember bool) int {
	return c.servedHashed(node, who, nil, prompt, tail, now, remember)
}

// servedHashed is served with ph, the prompt's block hashes if already computed (nil = not).
func (c *prefixCache) servedHashed(node, who string, ph []uint64, prompt, tail string, now time.Time, remember bool) int {
	if ph == nil {
		ph = c.blockHashes(prompt)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	nc := c.nodes[node]
	best, bestAt := 0, -1
	if nc != nil {
		live := nc.convs[:0]
		for _, e := range nc.convs {
			if now.Sub(e.at) <= cacheWindow {
				live = append(live, e)
			}
		}
		nc.convs = live
		for i, e := range nc.convs {
			if e.who != who {
				continue
			}
			if k := commonPrefix(ph, e.hashes); k > best {
				best, bestAt = k, i
			}
		}
	}
	matched := best * cacheBlock
	if !remember {
		return matched
	}
	if nc == nil {
		if len(c.nodes) >= cacheMaxNodes {
			var oldest string
			for id, x := range c.nodes {
				if oldest == "" || x.used.Before(c.nodes[oldest].used) {
					oldest = id
				}
			}
			delete(c.nodes, oldest)
		}
		nc = &nodeConvs{}
		c.nodes[node] = nc
	}
	nc.used = now
	if bestAt >= 0 && best >= len(nc.convs[bestAt].hashes)-1 { // the conversation went on (the answer's last block may be rendered differently): replace its entry
		nc.convs = append(nc.convs[:bestAt], nc.convs[bestAt+1:]...)
	}
	if len(nc.convs) >= cacheConvsPerNode {
		nc.convs = nc.convs[1:]
	}
	nc.convs = append(nc.convs, &convEntry{who: who, hashes: c.blockHashesFrom(ph, prompt+tail), at: now})
	return matched
}

// stickyMin is the shortest held prefix, in blocks, that routing follows: 4 KiB, about 1k
// tokens or a second of prefill on a 3090. Below it, spreading the load is worth more.
const stickyMin = 4096 / cacheBlock

// prefixHit is how much of a conversation a node holds for a requester, and since when.
// cont: the request continues a whole conversation the node served (the same test that
// replaces the entry in servedHashed), not just a shared start such as a system prompt.
type prefixHit struct {
	blocks int
	at     time.Time
	cont   bool
}

// held returns, per node, the longest prefix of ph (block hashes of a prompt) that the node
// served for who within the window, if at least stickyMin blocks. It only looks. The cost is
// one string comparison per remembered conversation and a prefix comparison for who's own,
// so at most cacheMaxNodes*cacheConvsPerNode entries, far fewer in practice.
func (c *prefixCache) held(who string, ph []uint64, now time.Time) map[string]prefixHit {
	if len(ph) < stickyMin {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	var out map[string]prefixHit
	for id, nc := range c.nodes {
		best := prefixHit{}
		for _, e := range nc.convs {
			if e.who != who || now.Sub(e.at) > cacheWindow || len(e.hashes) < stickyMin {
				continue
			}
			k := commonPrefix(ph, e.hashes)
			cont := best.cont || (k >= stickyMin && k >= len(e.hashes)-1)
			if k > best.blocks || (k == best.blocks && e.at.After(best.at)) {
				best = prefixHit{k, e.at, false}
			}
			best.cont = cont
		}
		if best.blocks >= stickyMin {
			if out == nil {
				out = map[string]prefixHit{}
			}
			out[id] = best
		}
	}
	return out
}

// convKey names a conversation for the share cap: a hash of the requester and the first
// stickyMin blocks, which a conversation keeps while it goes on (0 = too short to tell).
// Two conversations of one requester that share their first 4 KiB (a long system prompt)
// get the same key; the share cap tells them apart with prefixHit.cont.
func (c *prefixCache) convKey(who string, ph []uint64) uint64 {
	if len(ph) < stickyMin {
		return 0
	}
	var h maphash.Hash
	h.SetSeed(c.seed)
	h.WriteString(who)
	var b [8]byte
	for i := range b {
		b[i] = byte(ph[stickyMin-1] >> (8 * i))
	}
	h.Write(b[:])
	return h.Sum64() | 1
}
