// SPDX-License-Identifier: Apache-2.0
package node

import (
	"io"
	"log/slog"
	"strings"
	"testing"
)

// A pilot's credential carries its node id, as an account's node token does; the setup flow's
// TokenNodeID still takes account tokens only.
func TestPilotNodeID(t *testing.T) {
	secret := strings.Repeat("A", 43)
	if id, ok := PilotNodeID("pc-0123456789ab." + secret); !ok || id != "p-0123456789ab" {
		t.Fatalf("pilot credential: %q %v", id, ok)
	}
	for _, bad := range []string{"pc-0123456789a." + secret, "pc-0123456789AB." + secret, "pc-0123456789ab", "nt-n-01234567." + secret} {
		if _, ok := PilotNodeID(bad); ok {
			t.Errorf("%q accepted", bad)
		}
	}
	if _, ok := TokenNodeID("pc-0123456789ab." + secret); ok {
		t.Error("TokenNodeID takes a pilot credential")
	}
	a := New(Config{Token: "pc-0123456789ab." + secret, NodeID: "other"}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if a.cfg.NodeID != "p-0123456789ab" {
		t.Errorf("node id %q, want the pilot's", a.cfg.NodeID)
	}
}
