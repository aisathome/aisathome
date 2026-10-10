package gateway

import (
	"fmt"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/aisathome/aisathome/protocol"
)

// Names announced by nodes are remembered up to maxKnown; names already known keep updating.
func TestKnownNamesCapped(t *testing.T) {
	g, err := New(Config{StateDir: t.TempDir()}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	start := time.Now()
	g.mu.Lock()
	defer g.mu.Unlock()
	g.rememberLocked([]protocol.ModelInfo{{Name: "real", Roles: []string{"deep"}}}, start)
	for i := 0; i < 100; i++ { // a token reconnecting with fresh names each time
		models := make([]protocol.ModelInfo, 16)
		for j := range models {
			models[j] = protocol.ModelInfo{Name: fmt.Sprintf("junk-%d-%d", i, j), Roles: []string{fmt.Sprintf("r-%d-%d", i, j)}}
		}
		g.rememberLocked(models, start)
	}
	if len(g.known) > maxKnown {
		t.Fatalf("%d names remembered, cap %d", len(g.known), maxKnown)
	}
	later := start.Add(time.Hour)
	g.rememberLocked([]protocol.ModelInfo{{Name: "real"}}, later)
	if !g.known["real"].Equal(later) {
		t.Fatal("a known name stopped updating once the cap was reached")
	}
}
