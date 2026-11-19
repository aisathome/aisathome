// How the nodes spend their GPU time, for the network pressure statistics (pressure.go): per
// answer, the time to read the prompt (prefill) and the time to write the answer (decode), the
// tokens of each and the prompt tokens read from the engine's cache. Decode sets the network's
// throughput (research: 75-85% of GPU time for about 4% of the tokens), and prompt-cache hits
// shorten prefill, so these two say whether faster GPUs or better cache routing would help.
//
// Times come from the engine's own timings when it sends them (llama-server's "timings" object in
// its last chunk: prompt_ms and predicted_ms) and they fit within what the gateway measured; else
// from the gateway's clock: from sending the request to the node until the first generated text
// (prefill, plus the network and the node's own queue), and from there to the end (decode). Token
// counts are the node's, clamped as for credit. Aggregates only, never a node.
//
// SPDX-License-Identifier: Apache-2.0
package gateway

import (
	"bytes"
	"encoding/json"
	"math"
	"time"

	"github.com/aisathome/aisathome/protocol"
)

// nodeAgg is the timed answers of one bucket.
type nodeAgg struct {
	Requests  int   `json:"requests"` // answers timed
	Engine    int   `json:"engine"`   // of those, timed by the engine itself
	PrefillMs int64 `json:"prefill_ms"`
	DecodeMs  int64 `json:"decode_ms"`
	PromptTok int64 `json:"prompt_tok"` // prompt tokens, the cached ones included
	CachedTok int64 `json:"cached_tok"` // prompt tokens read from the engine's cache
	OutTok    int64 `json:"out_tok"`    // generated tokens
}

func (a *nodeAgg) merge(o *nodeAgg) {
	a.Requests += o.Requests
	a.Engine += o.Engine
	a.PrefillMs += o.PrefillMs
	a.DecodeMs += o.DecodeMs
	a.PromptTok += o.PromptTok
	a.CachedTok += o.CachedTok
	a.OutTok += o.OutTok
}

// public: the share of node time in prefill, the speeds (prompt tokens computed per prefill second,
// tokens generated per decode second, per answer) and the prompt-cache hit rate.
func (a *nodeAgg) public() map[string]any {
	m := map[string]any{"requests": a.Requests, "engine_timed": a.Engine}
	if t := a.PrefillMs + a.DecodeMs; t > 0 {
		m["prefill_share"] = math.Round(float64(a.PrefillMs)/float64(t)*1000) / 1000
	}
	if a.PrefillMs > 0 {
		m["prefill_tok_s"] = round1(float64(max(a.PromptTok-a.CachedTok, 0)) / (float64(a.PrefillMs) / 1000))
	}
	if a.DecodeMs > 0 {
		m["decode_tok_s"] = round1(float64(a.OutTok) / (float64(a.DecodeMs) / 1000))
	}
	if a.PromptTok > 0 {
		m["cache_hit"] = math.Round(float64(min(a.CachedTok, a.PromptTok))/float64(a.PromptTok)*1000) / 1000
	}
	return m
}

// engineTimings is the part of llama-server's "timings" the statistics read.
type engineTimings struct {
	PromptMs    float64 `json:"prompt_ms"`
	PredictedMs float64 `json:"predicted_ms"`
}

var timingsKey = []byte(`"timings"`)

// chunkTimings reads the engine's timings from a chunk, if it has them.
func chunkTimings(data []byte) *engineTimings {
	if !bytes.Contains(data, timingsKey) {
		return nil
	}
	var c struct {
		Timings *engineTimings `json:"timings"`
	}
	if json.Unmarshal(data, &c) != nil || c.Timings == nil {
		return nil
	}
	t := c.Timings
	if math.IsNaN(t.PromptMs) || math.IsNaN(t.PredictedMs) || t.PromptMs < 0 || t.PredictedMs < 0 || t.PromptMs+t.PredictedMs <= 0 {
		return nil
	}
	return t
}

// nodeTime splits one answer's node time: the engine's timings when they fit within the total the
// gateway measured (they cannot be longer), else the gateway's own split at the first text.
func nodeTime(total, toFirst time.Duration, et *engineTimings) (prefill, decode time.Duration, engine bool) {
	if et != nil {
		p, d := time.Duration(et.PromptMs*float64(time.Millisecond)), time.Duration(et.PredictedMs*float64(time.Millisecond))
		if p+d <= total {
			return p, d, true
		}
	}
	if toFirst <= 0 || toFirst > total {
		return 0, 0, false
	}
	return toFirst, total - toFirst, false
}

// node counts one answer a node finished: u is its usage as metered (clamped).
func (p *pressure) node(total, toFirst time.Duration, et *engineTimings, u protocol.Usage, now time.Time) {
	if p == nil { // a gateway built without New (tests)
		return
	}
	pre, dec, engine := nodeTime(total, toFirst, et)
	if pre+dec <= 0 {
		return // no text came: nothing to split
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	h, d := p.bucketsLocked(now)
	for _, b := range []*pressBucket{h, d} {
		if b.Node == nil {
			b.Node = &nodeAgg{}
		}
		a := b.Node
		a.Requests++
		if engine {
			a.Engine++
		}
		a.PrefillMs += pre.Milliseconds()
		a.DecodeMs += dec.Milliseconds()
		a.PromptTok += int64(max(u.PromptTokens, 0))
		a.CachedTok += int64(min(max(u.CachedTokens, 0), max(u.PromptTokens, 0)))
		a.OutTok += int64(max(u.CompletionTokens, 0))
	}
}
