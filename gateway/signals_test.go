// SPDX-License-Identifier: Apache-2.0
package gateway

import (
	"math/rand"
	"strings"
	"testing"
)

const honestProse = `To prove the lemma, I first unfold the definition of the price function and look at the two cases
of the match. In the first case the allocation is empty, so both sides reduce to zero and simp closes the goal.
In the second case I need the monotonicity of the sum over the remaining bidders, which follows from
Finset.sum_le_sum once I show each term is nonnegative. The term is a product of a nonnegative weight and a
price that is bounded below by the reserve, so positivity should handle it. After that, linarith combines the
two inequalities. I will write the proof with an explicit calc block so that each step is checked separately,
and then try to shorten it if it compiles on the first attempt.`

func TestMeasureText(t *testing.T) {
	if measureText(strings.Repeat("a", degenMinBytes-1)) != nil {
		t.Fatal("a short text was measured")
	}
	h := measureText(honestProse)
	if h == nil || h.degenerate() || h.Rep != 0 || h.GZ < 0.3 || h.GZ > 0.7 {
		t.Fatalf("honest prose: %+v", h)
	}
	rep := measureText(strings.Repeat("The proof follows from the lemma above. ", 40))
	if !rep.degenerate() || rep.Rep < 0.9 {
		t.Fatalf("repeated padding: %+v", rep)
	}
	rnd := rand.New(rand.NewSource(1))
	b := make([]byte, 2000)
	for i := range b {
		b[i] = byte(33 + rnd.Intn(94)) // printable ASCII
	}
	if r := measureText(string(b)); !r.degenerate() {
		t.Fatalf("random characters: %+v", r)
	}
	var odd strings.Builder
	for odd.Len() < 600 {
		odd.WriteString("§¤ ")
	}
	if o := measureText(odd.String()); o.Odd < 0.5 || !o.degenerate() {
		t.Fatalf("odd bytes: %+v", o)
	}
	if u := measureText(strings.Repeat("Ein Beweis über Mengen und Gruppen, Schritt für Schritt erklärt. ", 1) + honestProse); u.degenerate() {
		t.Fatalf("letters outside ASCII counted as odd: %+v", u)
	}
	long := measureText(strings.Repeat("é", degenMaxBytes)) // cut on a character boundary
	if long == nil || long.Bytes != 2*degenMaxBytes || long.Odd != 0 {
		t.Fatalf("long text: %+v", long)
	}
}

// Each tail flags on its own; inside all of them nothing is flagged.
func TestDegenerateTails(t *testing.T) {
	ok := textStats{GZ: 0.43, Rep: 0.05, Odd: 0.01}
	if ok.degenerate() || (*textStats)(nil).degenerate() {
		t.Fatal("honest numbers flagged")
	}
	for _, s := range []textStats{{GZ: 0.217, Rep: 0.05, Odd: 0.01}, {GZ: 0.758, Rep: 0.05, Odd: 0.01},
		{GZ: 0.43, Rep: 0.78, Odd: 0.01}, {GZ: 0.43, Rep: 0.05, Odd: 0.07}} {
		if !s.degenerate() {
			t.Errorf("%+v not flagged", s)
		}
	}
	for _, s := range []textStats{{GZ: 0.218, Rep: 0.779, Odd: 0.069}, {GZ: 0.757, Rep: 0, Odd: 0}} {
		if s.degenerate() {
			t.Errorf("%+v at the tails flagged", s)
		}
	}
}

func TestBadCall(t *testing.T) {
	for _, c := range []struct {
		call toolCall
		bad  bool
	}{
		{toolCall{"read", `{"path":"a"}`}, false},
		{toolCall{"read", ""}, false},
		{toolCall{"read", "  {}  "}, false},
		{toolCall{"", `{"path":"a"}`}, true},
		{toolCall{"read", `{"path":`}, true},
		{toolCall{"read", `["a"]`}, true},
		{toolCall{"read", `"a"`}, true},
		{toolCall{"read", `{"a":1}{"b":2}`}, true},
	} {
		if got := badCall(&c.call); got != c.bad {
			t.Errorf("%+v: bad %v, want %v", c.call, got, c.bad)
		}
	}
}

func TestSignalsOf(t *testing.T) {
	var o genOut
	o.chunk([]byte(`{"choices":[{"delta":{"reasoning_content":"think"}}]}`))
	o.chunk([]byte(`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"name":"read","arguments":"{\"p\":1}"}}]}}]}`))
	o.chunk([]byte(`{"choices":[{"delta":{"tool_calls":[{"index":1,"function":{"name":"edit","arguments":"{oops"}}]}}]}`))
	o.chunk([]byte(`{"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`))
	o.chunk([]byte(`{"choices":[],"usage":{"completion_tokens":9}}`)) // no finish: the last one stays
	s := signalsOf(&o, 50, 7, 2000)
	if s.Finish != "tool_calls" || s.ToolCalls != 2 || s.BadCalls != 1 || s.TPS != 25 || s.Reasoning != 7 || s.Text != nil || s.Degenerate {
		t.Fatalf("%+v", s)
	}
	if s := signalsOf(&genOut{}, 0, -1, 0); s.TPS != 0 || s.Reasoning != 0 {
		t.Fatalf("empty: %+v", s)
	}
	var long genOut
	long.content.WriteString(strings.Repeat("pad ", 500))
	if s := signalsOf(&long, 500, -1, 10); s.Text == nil || !s.Degenerate {
		t.Fatalf("padding: %+v", s)
	}
}

// The usage log keeps only the finish reasons engines send; a node's own words become "other",
// and a finish_reason of the wrong type neither hides the chunk's text nor reaches the log.
func TestFinishReason(t *testing.T) {
	var o genOut
	o.chunk([]byte(`{"choices":[{"delta":{"content":"abc"},"finish_reason":"my secret is hunter2"}]}`))
	if o.finish != "other" || o.content.String() != "abc" {
		t.Fatalf("free text: %q %q", o.finish, o.content.String())
	}
	o.chunk([]byte(`{"choices":[{"delta":{"content":"def"},"finish_reason":42}]}`))
	if o.finish != "other" || o.content.String() != "abcdef" {
		t.Fatalf("a number hid the text: %q %q", o.finish, o.content.String())
	}
	for _, f := range []string{"stop", "length", "tool_calls"} {
		o.chunk([]byte(`{"choices":[{"delta":{},"finish_reason":"` + f + `"}]}`))
		if o.finish != f {
			t.Fatalf("%s: %q", f, o.finish)
		}
	}
	o.chunk([]byte(`{"choices":[{"delta":{},"finish_reason":null}]}`))
	if o.finish != "tool_calls" {
		t.Fatalf("null replaced it: %q", o.finish)
	}
}
