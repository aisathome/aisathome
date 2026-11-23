// Per-answer signals in the usage record (node verification): numbers that, summed
// per node over many answers, show a node that degrades or pads answers inside science sessions.
// None of them is a verdict on one answer; they are compared against the maintainer's own nodes' rates
// (`accounts node-checks`) and decide what a person or, later, the replay sampler looks at.
//
//   - tps: output tokens per second of the gateway's own time (what the speed check reads);
//   - reasoning: reasoning tokens (what the reasoning-budget check reads);
//   - finish: the node's finish_reason; "length" is an answer cut at max_tokens;
//   - tool_calls and bad_calls: calls without a name, or whose arguments are not a JSON object
//     (honest baseline: 1 parse error in 8,253 tool calls of our prove2me sessions);
//   - degeneracy statistics of the generated text (reasoning, answer and tool arguments, at least
//     400 bytes): gz, its zlib-compressed size over its size (low: repetitive); rep, the share of
//     word 6-grams that occur more than once (high: loops); odd, the share of characters that are
//     neither letters, digits, spaces nor punctuation (high: random bytes). degenerate marks one
//     beyond the honest 0.1%/99.9% tails measured on 5,695 honest prove2me messages (gz < 0.218 or
//     > 0.757, rep > 0.779, odd > 0.069: 0.37% of honest messages, 100% of random or repeated
//     padding, 25% of word salad, measured on our own transcripts). Go's zlib
//     compresses a little differently from Python's, so the gz tails are approximate.
//
// The statistics are computed in memory from the text the meter already holds; no text is kept.
// The usage record also names the task the request named (X-Aisathome-Task), and a discarded
// answer is an event of its own (discard.go).
//
// SPDX-License-Identifier: Apache-2.0
package gateway

import (
	"bytes"
	"compress/zlib"
	"encoding/json"
	"math"
	"strings"
	"unicode"
)

// answerSignals are one answer's signals (usageRecord.Signals).
type answerSignals struct {
	TPS        float64    `json:"tps,omitempty"`
	Reasoning  int        `json:"reasoning,omitempty"`
	Finish     string     `json:"finish,omitempty"`
	ToolCalls  int        `json:"tool_calls,omitempty"`
	BadCalls   int        `json:"bad_calls,omitempty"`
	Text       *textStats `json:"text,omitempty"` // nil below degenMinBytes
	Degenerate bool       `json:"degenerate,omitempty"`
}

// textStats are the degeneracy statistics of an answer's generated text.
type textStats struct {
	Bytes int     `json:"bytes"`
	GZ    float64 `json:"gz"`
	Rep   float64 `json:"rep"`
	Odd   float64 `json:"odd"`
}

// Degeneracy: texts shorter than degenMinBytes are not measured; the statistics are computed on
// at most degenMaxBytes (the start of a longer text); the tails are the measured honest ones above.
const (
	degenMinBytes = 400
	degenMaxBytes = 256 << 10
	degenGZLow    = 0.218
	degenGZHigh   = 0.757
	degenRepHigh  = 0.779
	degenOddHigh  = 0.069
)

// round3 keeps three decimals (the log needs no more).
func round3(x float64) float64 { return math.Round(x*1000) / 1000 }

// measureText returns t's degeneracy statistics, or nil if it is too short.
func measureText(t string) *textStats {
	if len(t) < degenMinBytes {
		return nil
	}
	n := len(t)
	if len(t) > degenMaxBytes {
		t = t[:degenMaxBytes]
		for len(t) > 0 && !utf8Start(t[len(t)-1]) {
			t = t[:len(t)-1]
		}
	}
	var buf bytes.Buffer
	zw, _ := zlib.NewWriterLevel(&buf, 6)
	zw.Write([]byte(t))
	zw.Close()
	words := strings.Fields(t)
	rep := 0.0
	if len(words) >= 6 {
		grams := make(map[string]int, len(words))
		for i := 0; i+6 <= len(words); i++ {
			grams[strings.Join(words[i:i+6], "\x00")]++
		}
		repeated := 0
		for _, c := range grams {
			if c > 1 {
				repeated += c
			}
		}
		rep = float64(repeated) / float64(len(words)-5)
	}
	odd, chars := 0, 0
	for _, r := range t {
		chars++
		if r < 128 {
			if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == ' ' || r == '\t' ||
				r == '\n' || r == '\r' || r == '\v' || r == '\f' || strings.ContainsRune(asciiPunct, r)) {
				odd++
			}
		} else if !unicode.IsLetter(r) {
			odd++
		}
	}
	return &textStats{Bytes: n, GZ: round3(float64(buf.Len()) / float64(len(t))), Rep: round3(rep),
		Odd: round3(float64(odd) / float64(max(chars, 1)))}
}

// asciiPunct is Python's string.punctuation.
const asciiPunct = "!\"#$%&'()*+,-./:;<=>?@[\\]^_`{|}~"

// utf8Start reports whether b can begin a UTF-8 sequence (not a continuation byte).
func utf8Start(b byte) bool { return b&0xC0 != 0x80 }

// degenerate reports whether s is beyond the honest tails.
func (s *textStats) degenerate() bool {
	return s != nil && (s.GZ < degenGZLow || s.GZ > degenGZHigh || s.Rep > degenRepHigh || s.Odd > degenOddHigh)
}

// badCall reports whether a tool call is malformed: no name, or arguments that are not a JSON
// object (empty arguments count as "{}").
func badCall(c *toolCall) bool {
	if strings.TrimSpace(c.name) == "" {
		return true
	}
	a := strings.TrimSpace(c.args)
	if a == "" {
		return false
	}
	return a[0] != '{' || !json.Valid([]byte(a))
}

// signalsOf computes the signals of the output o (all the node generated) of an answer whose
// output tokens were out over durMs, with reasoning tokens (-1 = not counted).
func signalsOf(o *genOut, out, reasoning int, durMs int64) *answerSignals {
	s := &answerSignals{Finish: o.finish}
	if durMs > 0 && out > 0 {
		s.TPS = math.Round(float64(out)/(float64(durMs)/1000)*10) / 10
	}
	if reasoning > 0 {
		s.Reasoning = reasoning
	}
	var text strings.Builder
	text.WriteString(o.reasoning.String())
	text.WriteString(o.content.String())
	for _, c := range o.calls {
		s.ToolCalls++
		if badCall(c) {
			s.BadCalls++
		}
		text.WriteString(c.args)
	}
	s.Text = measureText(text.String())
	s.Degenerate = s.Text.degenerate()
	return s
}
