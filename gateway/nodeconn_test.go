// SPDX-License-Identifier: Apache-2.0

package gateway

import (
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/aisathome/aisathome/protocol"
)

// The node protocol range defaults to the gateway's version and the one before it (the
// current and the previous node release); a range the gateway cannot serve is a config error.
func TestNodeProtocolRange(t *testing.T) {
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	g, err := New(Config{}, quiet)
	if err != nil {
		t.Fatal(err)
	}
	if g.cfg.NodeProtocolMin != protocol.Version-1 || g.cfg.NodeProtocolMax != protocol.Version {
		t.Fatalf("default range %d-%d", g.cfg.NodeProtocolMin, g.cfg.NodeProtocolMax)
	}
	g.Close()
	for _, c := range [][2]int{{-1, 0}, {0, protocol.Version + 1}, {2, 1}, {1, -3}} {
		if g, err := New(Config{NodeProtocolMin: c[0], NodeProtocolMax: c[1]}, quiet); err == nil {
			g.Close()
			t.Errorf("range %v accepted", c)
		}
	}
	if g, err := New(Config{NodeProtocolMin: 1, NodeProtocolMax: 1}, quiet); err != nil {
		t.Errorf("only version 1: %v", err)
	} else {
		g.Close()
	}
}

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
