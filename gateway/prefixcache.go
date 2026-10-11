// Prompt caching as the gateway sees it, for credit: a prompt counts as cached up to the
// longest prefix that the same node served for the same requester (account, end user behind
// the chat, or API key) within the last cacheWindow. What a node claims about its own cache
// is ignored. The gateway keeps no text: only chained hashes of 256-byte blocks of the
// rendered conversation, per node and requester, bounded in count and length.
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

// blockHashes chains a hash over each whole block of s.
func (c *prefixCache) blockHashes(s string) []uint64 {
	n := min(len(s)/cacheBlock, cacheMaxBlocks)
	out := make([]uint64, n)
	var prev [8]byte
	for k := 0; k < n; k++ {
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
	ph := c.blockHashes(prompt)
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
	nc.convs = append(nc.convs, &convEntry{who: who, hashes: c.blockHashes(prompt + tail), at: now})
	return matched
}
