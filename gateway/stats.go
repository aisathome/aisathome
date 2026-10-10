// Public network statistics for the website: aggregates only, never node names, operators,
// addresses or anything about users.
//
// SPDX-License-Identifier: Apache-2.0
package gateway

import (
	"bufio"
	"encoding/json"
	"net/http"
	"os"
	"sort"
	"sync"
	"time"
)

// dayStats counts today's (UTC) requests and all requests since the usage log began. On
// start both are rebuilt from the usage log, so a restart does not reset the numbers.
type dayStats struct {
	mu                       sync.Mutex
	day                      string
	requests, ok             int
	promptTok, completionTok int64

	allRequests, allOK         int
	allPromptTok, allCompleted int64
	since                      time.Time // first record
}

func (s *dayStats) add(r usageRecord) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.allRequests++
	if r.Outcome == "ok" {
		s.allOK++
	}
	s.allPromptTok += int64(max(r.Usage.PromptTokens, 0))
	s.allCompleted += int64(max(r.Usage.CompletionTokens, 0))
	if s.since.IsZero() || r.Time.Before(s.since) {
		s.since = r.Time.UTC()
	}
	day := r.Time.UTC().Format(time.DateOnly)
	if day != s.day {
		if day < s.day {
			return // an old record (log replay out of order)
		}
		s.day, s.requests, s.ok, s.promptTok, s.completionTok = day, 0, 0, 0, 0 // not *s = …: that would reset the held mutex
	}
	s.requests++
	if r.Outcome == "ok" {
		s.ok++
	}
	s.promptTok += int64(max(r.Usage.PromptTokens, 0))
	s.completionTok += int64(max(r.Usage.CompletionTokens, 0))
}

// replay counts the records in the usage log (JSON lines): all of them for the totals, and
// today's for the day.
func (s *dayStats) replay(path string, now time.Time) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		var r usageRecord
		if json.Unmarshal(sc.Bytes(), &r) == nil {
			s.add(r) // add keeps old days out of today's counts
		}
	}
}

// handleStats serves GET /v1/stats without authentication.
func (g *Gateway) handleStats(w http.ResponseWriter, _ *http.Request) {
	nodes, slots, busy := 0, 0, 0
	models := map[string]bool{}
	g.mu.Lock()
	for _, n := range g.node {
		nodes++
		n.mu.Lock()
		busy += n.inflight
		n.mu.Unlock()
		for _, m := range n.models {
			slots += m.Slots
			for _, r := range m.Roles {
				models[r] = true
			}
		}
	}
	g.mu.Unlock()
	roles := make([]string, 0, len(models))
	for r := range models {
		roles = append(roles, r)
	}
	sort.Strings(roles)

	now := time.Now().UTC()
	g.stats.mu.Lock()
	today := map[string]any{"date": now.Format(time.DateOnly), "requests": 0, "completed": 0,
		"prompt_tokens": 0, "completion_tokens": 0}
	if g.stats.day == now.Format(time.DateOnly) {
		today["requests"], today["completed"] = g.stats.requests, g.stats.ok
		today["prompt_tokens"], today["completion_tokens"] = g.stats.promptTok, g.stats.completionTok
	}
	total := map[string]any{"requests": g.stats.allRequests, "completed": g.stats.allOK,
		"prompt_tokens": g.stats.allPromptTok, "completion_tokens": g.stats.allCompleted,
		"tokens": g.stats.allPromptTok + g.stats.allCompleted}
	if !g.stats.since.IsZero() {
		total["since"] = g.stats.since.Format(time.DateOnly)
	}
	g.stats.mu.Unlock()

	w.Header().Set("Cache-Control", "public, max-age=15")
	writeJSON(w, http.StatusOK, map[string]any{
		"updated": now.Format(time.RFC3339),
		"nodes":   map[string]any{"online": nodes, "slots": slots, "busy": busy},
		"models":  roles,
		"today":   today,
		"total":   total,
	})
}
