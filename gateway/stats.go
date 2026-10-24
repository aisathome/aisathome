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
	classes                  map[string]*useCount // today's, by workload class
	projects                 map[string]*useCount // today's, by project

	allRequests, allOK         int
	allPromptTok, allCompleted int64
	allProjects                map[string]*useCount
	since                      time.Time // first record
}

// useCount is what one class or project used.
type useCount struct {
	Requests   int     `json:"requests"`
	Prompt     int64   `json:"prompt_tokens"`
	Completion int64   `json:"completion_tokens"`
	Seconds    float64 `json:"node_seconds,omitempty"` // time on nodes (projects only)
}

func (c *useCount) add(prompt, completion int, ms int64, seconds bool) {
	c.Requests++
	c.Prompt += int64(max(prompt, 0))
	c.Completion += int64(max(completion, 0))
	if seconds {
		c.Seconds += float64(max(ms, 0)) / 1000
	}
}

func countIn(m *map[string]*useCount, k string) *useCount {
	if *m == nil {
		*m = map[string]*useCount{}
	}
	c := (*m)[k]
	if c == nil {
		c = &useCount{}
		(*m)[k] = c
	}
	return c
}

// recordTokens is what a record counts in the statistics: the gateway's own counts where it
// has them, otherwise the node's (clamped).
func recordTokens(r usageRecord) (prompt, completion int) {
	if r.Tokens != nil {
		return r.Tokens.Prompt, r.Tokens.Completion
	}
	return r.Usage.PromptTokens, r.Usage.CompletionTokens
}

func (s *dayStats) add(r usageRecord) {
	prompt, completion := recordTokens(r)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.allRequests++
	if r.Outcome == "ok" {
		s.allOK++
	}
	s.allPromptTok += int64(max(prompt, 0))
	s.allCompleted += int64(max(completion, 0))
	if r.Project != "" {
		countIn(&s.allProjects, r.Project).add(prompt, completion, r.Usage.DurationMs, true)
	}
	if s.since.IsZero() || r.Time.Before(s.since) {
		s.since = r.Time.UTC()
	}
	day := r.Time.UTC().Format(time.DateOnly)
	if day != s.day {
		if day < s.day {
			return // an old record (log replay out of order)
		}
		s.day, s.requests, s.ok, s.promptTok, s.completionTok = day, 0, 0, 0, 0 // not *s = …: that would reset the held mutex
		s.classes, s.projects = nil, nil
	}
	s.requests++
	if r.Outcome == "ok" {
		s.ok++
	}
	s.promptTok += int64(max(prompt, 0))
	s.completionTok += int64(max(completion, 0))
	if r.Class != "" {
		countIn(&s.classes, r.Class).add(prompt, completion, 0, false)
	}
	if r.Project != "" {
		countIn(&s.projects, r.Project).add(prompt, completion, r.Usage.DurationMs, true)
	}
}

// replayUsage passes every record of the usage log (JSON lines) to f, in order.
func replayUsage(path string, f func(usageRecord)) {
	if path == "" {
		return
	}
	file, err := os.Open(path)
	if err != nil {
		return
	}
	defer file.Close()
	sc := bufio.NewScanner(file)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		var r usageRecord
		if json.Unmarshal(sc.Bytes(), &r) == nil {
			f(r)
		}
	}
}

// copyCounts copies a count map for the JSON answer (nil stays nil).
func copyCounts(m map[string]*useCount) map[string]useCount {
	if len(m) == 0 {
		return nil
	}
	out := make(map[string]useCount, len(m))
	for k, c := range m {
		out[k] = *c
	}
	return out
}

// handleStats serves GET /v1/stats without authentication.
func (g *Gateway) handleStats(w http.ResponseWriter, _ *http.Request) {
	nodes, slots, busy, paused := 0, 0, 0, 0
	models := map[string]bool{}
	g.mu.Lock()
	for _, n := range g.node {
		nodes++
		n.mu.Lock()
		busy += n.inflight
		if n.paused {
			paused++
		}
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
		if c := copyCounts(g.stats.classes); c != nil {
			today["classes"] = c
		}
		if p := copyCounts(g.stats.projects); p != nil {
			today["projects"] = p
		}
	}
	total := map[string]any{"requests": g.stats.allRequests, "completed": g.stats.allOK,
		"prompt_tokens": g.stats.allPromptTok, "completion_tokens": g.stats.allCompleted,
		"tokens": g.stats.allPromptTok + g.stats.allCompleted}
	if !g.stats.since.IsZero() {
		total["since"] = g.stats.since.Format(time.DateOnly)
	}
	if p := copyCounts(g.stats.allProjects); p != nil {
		total["projects"] = p
	}
	g.stats.mu.Unlock()

	w.Header().Set("Cache-Control", "public, max-age=15")
	writeJSON(w, http.StatusOK, map[string]any{
		"updated": now.Format(time.RFC3339),
		"nodes":   map[string]any{"online": nodes, "slots": slots, "busy": busy, "paused": paused},
		"models":  roles,
		"today":   today,
		"total":   total,
	})
}
