// SPDX-License-Identifier: Apache-2.0

package gateway

import (
	"testing"
	"time"
)

// Only connections replaced soon after connecting count towards the warning, in a row.
func TestNoteReplace(t *testing.T) {
	g := &Gateway{flaps: map[string]*flapState{}}
	now := time.Now()
	quick, lasted := now.Add(-time.Second), now.Add(-flapWindow)
	for i, c := range []struct {
		since time.Time
		at    time.Time
		want  int
	}{
		{quick, now, 0}, {quick, now, 0}, {lasted, now, 0}, // a connection that lasted resets the count
		{quick, now, 0}, {quick, now, 0}, {quick, now, 3}, // three quick in a row: warn
		{quick, now, 0}, {quick, now, 0}, {quick, now, 0}, {now.Add(flapQuiet - time.Second), now.Add(flapQuiet), 4}, // then at most once per flapQuiet
	} {
		if got := g.noteReplaceLocked("n1", c.since, c.at); got != c.want {
			t.Fatalf("replacement %d: got %d, want %d", i+1, got, c.want)
		}
	}
	if _, ok := g.flaps["n2"]; ok || len(g.flaps) != 1 {
		t.Fatal("state for other ids")
	}
}

func TestCleanAgent(t *testing.T) {
	for in, want := range map[string]string{
		"aisathome-node/0.2.0": "aisathome-node/0.2.0", "aisathome-node/dev": "aisathome-node/dev",
		"aisathome-node/0.3.0-rc1+g1234": "aisathome-node/0.3.0-rc1+g1234",
		"":                               "", "<b>x</b>": "", "a b": "", "x\ny": "", "/x": "", string(make([]byte, 65)): "",
	} {
		if got := cleanAgent(in); got != want {
			t.Errorf("cleanAgent(%q) = %q, want %q", in, got, want)
		}
	}
}
