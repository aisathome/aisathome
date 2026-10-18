// SPDX-License-Identifier: Apache-2.0
package gateway

import (
	"fmt"
	"testing"
	"time"
)

// Unrecorded guard holds are bounded: beyond maxUnrecorded every node is held (fail closed),
// never one dropped. Recording drops only the hold that was written, not a newer one.
func TestUnrecordedHolds(t *testing.T) {
	var u unrecordedHolds
	at := time.Now()
	for i := 0; i < maxUnrecorded; i++ {
		if !u.add(NodeHold{Node: fmt.Sprintf("n-%08x", i), Rule: "r", At: at}) {
			t.Fatalf("hold %d refused below the bound", i)
		}
	}
	if u.has("n-other") {
		t.Fatal("an unknown node is held below the bound")
	}
	if u.add(NodeHold{Node: "n-other", Rule: "r", At: at}) || !u.has("n-other") || len(u.m) != maxUnrecorded {
		t.Fatalf("beyond the bound: every node held, none added (%d)", len(u.m))
	}
	var v unrecordedHolds
	old := NodeHold{Node: "n-1", Rule: "a", At: at}
	v.add(old)
	v.recorded(NodeHold{Node: "n-1", Rule: "a", At: at.Add(time.Second)}) // another hold
	if !v.has("n-1") {
		t.Fatal("recording another hold dropped this one")
	}
	v.recorded(old)
	if v.has("n-1") {
		t.Fatal("a recorded hold is still kept in memory")
	}
}
