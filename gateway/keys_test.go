// SPDX-License-Identifier: Apache-2.0

package gateway

import (
	"bytes"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func discard() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestBudgetsSurviveSaveAndLoad(t *testing.T) {
	path := filepath.Join(t.TempDir(), "budgets.json")
	now := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	b := newBudgets()
	b.path = path
	b.add("key-a", 100, now)
	if err := b.save(); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(path)
	if strings.Contains(string(raw), "key-a") {
		t.Fatalf("state file holds a raw API key: %s", raw)
	}
	c := newBudgets()
	c.path = path
	c.load(now.Add(time.Hour), discard())
	if _, over := c.over("key-a", 100, now.Add(time.Hour)); !over {
		t.Fatal("budget use was not restored")
	}
}

// Entries from earlier UTC days are dropped on load, and from memory when a new day starts.
func TestBudgetsDropOldDays(t *testing.T) {
	path := filepath.Join(t.TempDir(), "budgets.json")
	yesterday := time.Date(2026, 10, 9, 23, 59, 0, 0, time.UTC)
	today := yesterday.Add(2 * time.Minute)
	b := newBudgets()
	b.path = path
	b.add("old", 100, yesterday)
	b.add("new", 100, today)
	b.mu.Lock()
	if len(b.used) != 1 {
		t.Fatalf("yesterday's entry kept in memory: %d entries", len(b.used))
	}
	b.mu.Unlock()
	b.add("old", 100, yesterday) // late, from before midnight: ignored
	b.mu.Lock()
	if len(b.used) != 1 {
		t.Fatalf("a late add from yesterday was kept")
	}
	b.mu.Unlock()
	// A file saved just before midnight, loaded after it.
	os.WriteFile(path, []byte(`{"version":1,"used":{"`+keyHash("old")+`":{"day":"2026-10-09","n":100},`+
		`"`+keyHash("new")+`":{"day":"2026-10-10","n":100}}}`), 0o600)

	c := newBudgets()
	c.path = path
	c.load(today, discard())
	if len(c.used) != 1 {
		t.Fatalf("loaded %d entries, want only today's", len(c.used))
	}
	if _, over := c.over("new", 100, today); !over {
		t.Fatal("today's entry lost")
	}
}

func TestCorruptBudgetStateStartsEmpty(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "budgets.json"), []byte(`{"used": {"x": `), 0o600)
	var logs bytes.Buffer
	g, err := New(Config{StateDir: dir}, slog.New(slog.NewTextHandler(&logs, nil)))
	if err != nil {
		t.Fatalf("a corrupt state file must not stop the gateway: %v", err)
	}
	defer g.Close()
	if len(g.budget.used) != 0 || !strings.Contains(logs.String(), "budget state unreadable") {
		t.Fatalf("want empty budgets and a warning; got %d entries, logs %q", len(g.budget.used), logs.String())
	}
}

func TestLimiterPrunesIdleBuckets(t *testing.T) {
	l := newLimiter()
	now := time.Now()
	k := KeyInfo{RPM: 10}
	busy, _ := l.acquire("busy", k, now)
	for _, key := range []string{"a", "b", "c"} {
		release, _ := l.acquire(key, k, now)
		release()
	}
	later := now.Add(2 * idleAfter)
	l.acquire("d", k, later)
	if len(l.buckets) != 2 { // "busy" (in flight) and "d"
		t.Fatalf("%d buckets left, want 2", len(l.buckets))
	}
	busy()
}
