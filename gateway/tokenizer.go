// The gateway's own tokenizer, so that credit does not rest on token counts a node reports.
// It is a byte-level BPE tokenizer read from the model's tokenizer.json (Hugging Face format):
// added (special) tokens are split off first, the rest is cut into pieces by the model's
// pre-tokenizer regex, each piece is mapped to GPT-2's byte alphabet and merged by rank.
//
// Only the Qwen3.5-and-later pre-tokenizer is supported (the regex in qwenPattern, checked
// when the file is loaded): Go's regexp has no look-ahead, so the pattern is implemented by
// hand in pretokenize. The NFC normalizer in tokenizer.json is not applied, as llama.cpp (what
// nodes run) does not apply it either; for text already in NFC, which is nearly all text, the
// ids equal those of Hugging Face `tokenizers` (testdata/qwen38-tokenizer-fixture.json).
//
// SPDX-License-Identifier: Apache-2.0
package gateway

import (
	"container/heap"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"
)

// qwenPattern is the pre-tokenizer regex of Qwen3.5 and later (Qwen3.8 included).
const qwenPattern = `(?i:'s|'t|'re|'ve|'m|'ll|'d)|[^\r\n\p{L}\p{N}]?[\p{L}\p{M}]+|\p{N}| ?[^\s\p{L}\p{M}\p{N}]+[\r\n]*|\s*[\r\n]+|\s+(?!\S)|\s+`

// Tokenizer is a byte-level BPE tokenizer. It is safe for concurrent use.
type Tokenizer struct {
	SHA256 string // of the tokenizer.json it was loaded from
	vocab  map[string]int32
	merges map[uint64]merge // (left id, right id) → rank and merged id
	byteID [256]int32       // the token of each single byte
	added  map[byte][]addedToken
	idLen  map[int32]int32 // bytes per token, for byte offsets

	cacheMu sync.Mutex
	cache   map[string][]int32 // piece → ids (bounded: cleared when full)

	trace func(rank int32) // tests: called for every merge applied
}

type merge struct{ rank, id int32 }

type addedToken struct {
	text string
	id   int32
}

const tokCacheMax = 1 << 16

// tokenizerFile is the part of tokenizer.json the gateway reads.
type tokenizerFile struct {
	AddedTokens []struct {
		ID      int32  `json:"id"`
		Content string `json:"content"`
	} `json:"added_tokens"`
	PreTokenizer struct {
		Type          string `json:"type"`
		Pretokenizers []struct {
			Type    string `json:"type"`
			Pattern struct {
				Regex string `json:"Regex"`
			} `json:"pattern"`
			Behavior       string `json:"behavior"`
			Invert         bool   `json:"invert"`
			AddPrefixSpace bool   `json:"add_prefix_space"`
			UseRegex       bool   `json:"use_regex"`
		} `json:"pretokenizers"`
	} `json:"pre_tokenizer"`
	Model struct {
		Type   string           `json:"type"`
		Vocab  map[string]int32 `json:"vocab"`
		Merges json.RawMessage  `json:"merges"`
	} `json:"model"`
}

// LoadTokenizer reads a tokenizer.json. wantSHA, if set, must match the file's SHA-256 (hex).
func LoadTokenizer(path, wantSHA string) (*Tokenizer, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("tokenizer: %w", err)
	}
	sum := sha256.Sum256(raw)
	got := hex.EncodeToString(sum[:])
	if wantSHA != "" && !strings.EqualFold(wantSHA, got) {
		return nil, fmt.Errorf("tokenizer: %s has SHA-256 %s, want %s", path, got, wantSHA)
	}
	t, err := parseTokenizer(raw)
	if err != nil {
		return nil, fmt.Errorf("tokenizer %s: %w", path, err)
	}
	t.SHA256 = got
	return t, nil
}

func parseTokenizer(raw []byte) (*Tokenizer, error) {
	var f tokenizerFile
	if err := json.Unmarshal(raw, &f); err != nil {
		return nil, err
	}
	if f.Model.Type != "BPE" {
		return nil, fmt.Errorf("model type %q, want BPE", f.Model.Type)
	}
	p := f.PreTokenizer.Pretokenizers
	if f.PreTokenizer.Type != "Sequence" || len(p) != 2 || p[0].Type != "Split" || p[0].Pattern.Regex != qwenPattern ||
		p[0].Behavior != "Isolated" || p[0].Invert || p[1].Type != "ByteLevel" || p[1].AddPrefixSpace || p[1].UseRegex {
		return nil, errors.New("unsupported pre-tokenizer (only Qwen3.5 and later's byte-level split is implemented)")
	}
	t := &Tokenizer{vocab: f.Model.Vocab, merges: map[uint64]merge{}, added: map[byte][]addedToken{},
		idLen: map[int32]int32{}, cache: map[string][]int32{}}
	b2u := bytesToUnicode()
	for b := 0; b < 256; b++ {
		id, ok := t.vocab[string(b2u[b])]
		if !ok {
			return nil, fmt.Errorf("no token for byte %d", b)
		}
		t.byteID[b] = id
	}
	u2b := map[rune]byte{}
	for b, r := range b2u {
		u2b[r] = byte(b)
	}
	for s, id := range t.vocab {
		n := 0
		for _, r := range s {
			if _, ok := u2b[r]; !ok {
				n = -1
				break
			}
			n++
		}
		if n > 0 {
			t.idLen[id] = int32(n)
		}
	}
	// Merges are "a b" strings (older files) or ["a", "b"] pairs.
	var pairs [][2]string
	var strs []string
	if err := json.Unmarshal(f.Model.Merges, &strs); err == nil {
		for _, s := range strs {
			a, b, ok := strings.Cut(s, " ")
			if !ok {
				return nil, fmt.Errorf("bad merge %q", s)
			}
			pairs = append(pairs, [2]string{a, b})
		}
	} else if err := json.Unmarshal(f.Model.Merges, &pairs); err != nil {
		return nil, fmt.Errorf("merges: %w", err)
	}
	for rank, pr := range pairs {
		a, okA := t.vocab[pr[0]]
		b, okB := t.vocab[pr[1]]
		m, okM := t.vocab[pr[0]+pr[1]]
		if !okA || !okB || !okM {
			return nil, fmt.Errorf("merge %q refers to unknown tokens", pr)
		}
		k := uint64(uint32(a))<<32 | uint64(uint32(b))
		if _, dup := t.merges[k]; !dup {
			t.merges[k] = merge{int32(rank), m}
		}
	}
	for _, a := range f.AddedTokens {
		if a.Content == "" {
			continue
		}
		t.added[a.Content[0]] = append(t.added[a.Content[0]], addedToken{a.Content, a.ID})
		t.idLen[a.ID] = int32(len(a.Content))
	}
	for _, l := range t.added { // longest first
		sort.Slice(l, func(i, j int) bool { return len(l[i].text) > len(l[j].text) })
	}
	return t, nil
}

// bytesToUnicode is GPT-2's map from bytes to printable characters.
func bytesToUnicode() [256]rune {
	var m [256]rune
	n := 0
	for b := 0; b < 256; b++ {
		if (b >= '!' && b <= '~') || (b >= 0xA1 && b <= 0xAC) || (b >= 0xAE && b <= 0xFF) {
			m[b] = rune(b)
		} else {
			m[b] = rune(256 + n)
			n++
		}
	}
	return m
}

// Encode returns the token ids of text (no BOS or other tokens added).
func (t *Tokenizer) Encode(text string) []int32 {
	var ids []int32
	t.encode(text, func(id int32) { ids = append(ids, id) })
	return ids
}

// Count returns the number of tokens in text.
func (t *Tokenizer) Count(text string) int {
	n := 0
	t.encode(text, func(int32) { n++ })
	return n
}

// TokenBytes returns the number of bytes token id stands for.
func (t *Tokenizer) TokenBytes(id int32) int { return int(t.idLen[id]) }

func (t *Tokenizer) encode(text string, emit func(int32)) {
	start := 0
	for i := 0; i < len(text); i++ {
		cands := t.added[text[i]]
		if cands == nil {
			continue
		}
		for _, a := range cands {
			if strings.HasPrefix(text[i:], a.text) {
				t.encodePlain(text[start:i], emit)
				emit(a.id)
				i += len(a.text) - 1
				start = i + 1
				break
			}
		}
	}
	t.encodePlain(text[start:], emit)
}

func (t *Tokenizer) encodePlain(s string, emit func(int32)) {
	if s == "" {
		return
	}
	pretokenize(s, func(piece string) {
		for _, id := range t.piece(piece) {
			emit(id)
		}
	})
}

// piece runs BPE on one pre-token, with a bounded cache.
func (t *Tokenizer) piece(p string) []int32 {
	if len(p) == 1 {
		return []int32{t.byteID[p[0]]}
	}
	t.cacheMu.Lock()
	ids, ok := t.cache[p]
	t.cacheMu.Unlock()
	if ok {
		return ids
	}
	ids = t.bpe(p)
	if len(p) <= 256 {
		t.cacheMu.Lock()
		if len(t.cache) >= tokCacheMax {
			clear(t.cache)
		}
		t.cache[p] = ids
		t.cacheMu.Unlock()
	}
	return ids
}

// bpe merges the bytes of p, lowest rank first and leftmost first among equal ranks (as
// Hugging Face does), in O(n log n) so that a long run of letters cannot stall the gateway.
func (t *Tokenizer) bpe(p string) []int32 {
	type sym struct {
		id         int32
		prev, next int32 // -1 at the ends
		dead       bool
	}
	syms := make([]sym, len(p))
	for i := 0; i < len(p); i++ {
		syms[i] = sym{id: t.byteID[p[i]], prev: int32(i - 1), next: int32(i + 1)}
	}
	syms[len(p)-1].next = -1
	h := &pairHeap{}
	push := func(i int32) {
		j := syms[i].next
		if j < 0 {
			return
		}
		if m, ok := t.merges[uint64(uint32(syms[i].id))<<32|uint64(uint32(syms[j].id))]; ok {
			heap.Push(h, pairCand{m.rank, i, syms[i].id, syms[j].id, m.id})
		}
	}
	for i := int32(0); i < int32(len(p))-1; i++ {
		push(i)
	}
	for h.Len() > 0 {
		c := heap.Pop(h).(pairCand)
		s := &syms[c.pos]
		if s.dead || s.id != c.left || s.next < 0 || syms[s.next].id != c.right {
			continue // stale: one of the two was merged since
		}
		if t.trace != nil {
			t.trace(c.rank)
		}
		nx := s.next
		s.id = c.merged
		syms[nx].dead = true
		s.next = syms[nx].next
		if s.next >= 0 {
			syms[s.next].prev = c.pos
		}
		if s.prev >= 0 {
			push(s.prev)
		}
		push(c.pos)
	}
	var out []int32
	for i := int32(0); i >= 0; i = syms[i].next {
		out = append(out, syms[i].id)
	}
	return out
}

type pairCand struct {
	rank, pos           int32
	left, right, merged int32
}

type pairHeap []pairCand

func (h pairHeap) Len() int { return len(h) }
func (h pairHeap) Less(i, j int) bool {
	return h[i].rank < h[j].rank || (h[i].rank == h[j].rank && h[i].pos < h[j].pos)
}
func (h pairHeap) Swap(i, j int) { h[i], h[j] = h[j], h[i] }
func (h *pairHeap) Push(x any)   { *h = append(*h, x.(pairCand)) }
func (h *pairHeap) Pop() any {
	old := *h
	x := old[len(old)-1]
	*h = old[:len(old)-1]
	return x
}

// ---------------------------------------------------------------- pre-tokenizer

func isL(r rune) bool  { return unicode.IsLetter(r) }
func isN(r rune) bool  { return unicode.IsNumber(r) }
func isM(r rune) bool  { return unicode.IsMark(r) }
func isS(r rune) bool  { return unicode.Is(unicode.White_Space, r) }
func isLM(r rune) bool { return isL(r) || isM(r) }
func isNL(r rune) bool { return r == '\r' || r == '\n' }

// pretokenize cuts s into the pieces qwenPattern matches, trying its alternatives in order
// at each position as a backtracking regex engine does:
//
//  1. (?i:'s|'t|'re|'ve|'m|'ll|'d)
//  2. [^\r\n\p{L}\p{N}]?[\p{L}\p{M}]+
//  3. \p{N}
//  4. ' '?[^\s\p{L}\p{M}\p{N}]+[\r\n]*
//  5. \s*[\r\n]+
//  6. \s+(?!\S)
//  7. \s+
//
// Every character matches one of them, so the pieces cover s.
func pretokenize(s string, emit func(string)) {
	if !utf8.ValidString(s) { // invalid bytes become U+FFFD, as when JSON is decoded
		s = string([]rune(s))
	}
	rs := []rune(s)
	off := make([]int, len(rs)+1) // byte offset of each rune, to cut s without re-encoding
	o := 0
	for i, r := range rs {
		off[i] = o
		o += utf8.RuneLen(r)
	}
	off[len(rs)] = len(s)
	n := len(rs)
	for i := 0; i < n; {
		j := matchAt(rs, i)
		emit(s[off[i]:off[j]])
		i = j
	}
}

// matchAt returns the end of the piece that starts at i (always > i).
func matchAt(rs []rune, i int) int {
	n := len(rs)
	r := rs[i]
	at := func(k int) rune {
		if k < n {
			return rs[k]
		}
		return -1
	}
	// 1. contractions, case-insensitive (ſ folds to s, as in the reference)
	if r == '\'' && i+1 < n {
		lower := func(c rune) rune {
			if c == 'ſ' {
				return 's'
			}
			return unicode.ToLower(c)
		}
		a, b := lower(at(i+1)), lower(at(i+2))
		switch {
		case a == 's' || a == 't' || a == 'm' || a == 'd':
			return i + 2
		case (a == 'r' || a == 'v') && b == 'e', a == 'l' && b == 'l':
			return i + 3
		}
	}
	// 2. an optional non-letter, non-digit, non-newline, then letters and marks
	letters := func(k int) int {
		for k < n && isLM(rs[k]) {
			k++
		}
		return k
	}
	if !isNL(r) && !isL(r) && !isN(r) && i+1 < n && isLM(rs[i+1]) {
		return letters(i + 1)
	}
	if isLM(r) {
		return letters(i)
	}
	// 3. one digit
	if isN(r) {
		return i + 1
	}
	// 4. an optional space, then symbols, then newlines
	sym := func(c rune) bool { return c >= 0 && !isS(c) && !isLM(c) && !isN(c) }
	k := -1
	if r == ' ' && sym(at(i+1)) {
		k = i + 1
	} else if sym(r) {
		k = i
	}
	if k >= 0 {
		for k < n && sym(rs[k]) {
			k++
		}
		for k < n && isNL(rs[k]) {
			k++
		}
		return k
	}
	// whitespace: r is \s here
	end := i
	for end < n && isS(rs[end]) {
		end++
	}
	// 5. whitespace up to and including its last newline
	for k := end - 1; k >= i; k-- {
		if isNL(rs[k]) {
			return k + 1
		}
	}
	// 6. whitespace not followed by a non-space: all of it at the end of the text, else
	// all but the last character (which then joins the next piece)
	if end == n || end-1 > i {
		if end == n {
			return end
		}
		return end - 1
	}
	// 7.
	return end
}
