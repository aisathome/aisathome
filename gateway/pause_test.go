// SPDX-License-Identifier: Apache-2.0
package gateway

import "testing"

// A paused node's refusal is a known category (logged as such, retried on another node).
func TestErrCategoryPaused(t *testing.T) {
	if got := errCategory("node paused"); got != "node paused" {
		t.Fatalf("errCategory(node paused) = %q", got)
	}
	if got := errCategory("node paused: " + "secret prompt text"); got != "node error" {
		t.Fatalf("free text after a known message must not pass: %q", got)
	}
}
