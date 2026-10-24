// Daily history for the network page: per UTC day, requests, tokens and nodes online, in all
// and per workload class; and per UTC hour, requests and tokens in all and per class (the last
// historyHours hours), so a node schedule (always one on, off at night) can be judged on data.
// Aggregates only: nothing per user, key or node.
//
// SPDX-License-Identifier: Apache-2.0
package gateway

import (
	"encoding/json"
	"errors"
	"log/slog"
	"math"
	"net/http"
	"os"
	"sort"
	"sync"
	"time"
)

// historyDays bounds the history: older days are dropped. historyHours bounds the hourly part
// (two weeks): older hours are dropped.
const (
	historyDays  = 400
	historyHours = 14 * 24
)

// hourFormat names an hour (UTC) in the hourly history.
const hourFormat = "2006-01-02T15:00Z"

// histHour is one UTC hour: requests and tokens, in all and per class. Hours without requests
// are not kept.
type histHour struct {
	Hour       string               `json:"hour"`
	Requests   int                  `json:"requests"`
	Completed  int                  `json:"completed"`
	Prompt     int64                `json:"prompt_tokens"`
	Completion int64                `json:"completion_tokens"`
	Classes    map[string]*useCount `json:"classes,omitempty"`
}

// histDay is one UTC day. Node counts come from samples taken while the gateway ran; a day
// with no samples (from before the history existed) has no node numbers.
type histDay struct {
	Date        string               `json:"date"`
	Requests    int                  `json:"requests"`
	Completed   int                  `json:"completed"`
	Prompt      int64                `json:"prompt_tokens"`
	Completion  int64                `json:"completion_tokens"`
	Classes     map[string]*useCount `json:"classes,omitempty"`
	NodesPeak   int                  `json:"nodes_peak"`
	NodeSamples int                  `json:"node_samples"`
	NodeSum     int64                `json:"node_sum"`
}

type history struct {
	mu     sync.Mutex
	days   map[string]*histDay
	hours  map[string]*histHour
	path   string // state file; "" = memory only
	dirty  bool
	saveMu sync.Mutex
}

func newHistory() *history {
	return &history{days: map[string]*histDay{}, hours: map[string]*histHour{}}
}

// dayLocked returns the entry of a day, creating it (and dropping the oldest beyond
// historyDays). Caller holds h.mu.
func (h *history) dayLocked(date string) *histDay {
	d := h.days[date]
	if d == nil {
		d = &histDay{Date: date}
		h.days[date] = d
		h.pruneLocked()
	}
	return d
}

func (h *history) pruneLocked() {
	if len(h.days) <= historyDays {
		return
	}
	dates := make([]string, 0, len(h.days))
	for k := range h.days {
		dates = append(dates, k)
	}
	sort.Strings(dates)
	for _, k := range dates[:len(dates)-historyDays] {
		delete(h.days, k)
	}
}

// hourLocked returns the entry of an hour, creating it (and dropping the oldest beyond
// historyHours). Caller holds h.mu.
func (h *history) hourLocked(hour string) *histHour {
	e := h.hours[hour]
	if e == nil {
		e = &histHour{Hour: hour}
		h.hours[hour] = e
		h.pruneHoursLocked()
	}
	return e
}

func (h *history) pruneHoursLocked() {
	if len(h.hours) <= historyHours {
		return
	}
	hours := make([]string, 0, len(h.hours))
	for k := range h.hours {
		hours = append(hours, k)
	}
	sort.Strings(hours)
	for _, k := range hours[:len(hours)-historyHours] {
		delete(h.hours, k)
	}
}

// add counts one usage record on its day and in its hour.
func (h *history) add(r usageRecord) {
	prompt, completion := recordTokens(r)
	h.mu.Lock()
	defer h.mu.Unlock()
	d := h.dayLocked(r.Time.UTC().Format(time.DateOnly))
	d.Requests++
	e := h.hourLocked(r.Time.UTC().Format(hourFormat))
	e.Requests++
	if r.Outcome == "ok" {
		d.Completed++
		e.Completed++
	}
	d.Prompt += int64(max(prompt, 0))
	d.Completion += int64(max(completion, 0))
	e.Prompt += int64(max(prompt, 0))
	e.Completion += int64(max(completion, 0))
	if r.Class != "" {
		countIn(&d.Classes, r.Class).add(prompt, completion, 0, false)
		countIn(&e.Classes, r.Class).add(prompt, completion, 0, false)
	}
	h.dirty = true
}

// sample records the number of nodes online now.
func (h *history) sample(nodes int, now time.Time) {
	h.mu.Lock()
	defer h.mu.Unlock()
	d := h.dayLocked(now.UTC().Format(time.DateOnly))
	d.NodeSamples++
	d.NodeSum += int64(nodes)
	d.NodesPeak = max(d.NodesPeak, nodes)
	h.dirty = true
}

// merge takes the request and token counts that the usage log has for a day when they are
// at least what the saved history has (the log holds every request of the days it covers; a
// rotated or truncated log holds fewer, and then the saved numbers stay). Node samples are
// only in the saved history.
func (h *history) merge(log *history) {
	log.mu.Lock()
	defer log.mu.Unlock()
	h.mu.Lock()
	defer h.mu.Unlock()
	for date, l := range log.days {
		d := h.days[date]
		if d == nil {
			d = &histDay{Date: date}
			h.days[date] = d
		} else if d.Requests > l.Requests {
			continue
		}
		d.Requests, d.Completed, d.Prompt, d.Completion, d.Classes = l.Requests, l.Completed, l.Prompt, l.Completion, l.Classes
		h.dirty = true
	}
	h.pruneLocked()
	for hour, l := range log.hours { // the same rule per hour
		if e := h.hours[hour]; e != nil && e.Requests > l.Requests {
			continue
		}
		c := *l
		h.hours[hour] = &c
		h.dirty = true
	}
	h.pruneHoursLocked()
}

type historyState struct {
	Version int         `json:"version"`
	Days    []*histDay  `json:"days"`
	Hours   []*histHour `json:"hours,omitempty"` // since 2026-10-24; older files have none
}

// load reads the state file. A missing file is a first start; a corrupt one is logged and
// set aside, since history is not worth refusing to start for.
func (h *history) load(log *slog.Logger) {
	raw, err := os.ReadFile(h.path)
	if errors.Is(err, os.ErrNotExist) {
		return
	}
	var st historyState
	if err == nil {
		err = json.Unmarshal(raw, &st)
	}
	if err != nil {
		log.Warn("history unreadable, setting it aside", "file", h.path, "err", err)
		os.Rename(h.path, h.path+".bad")
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, d := range st.Days {
		if d != nil && d.Date != "" {
			h.days[d.Date] = d
		}
	}
	h.pruneLocked()
	for _, e := range st.Hours {
		if e != nil && e.Hour != "" {
			h.hours[e.Hour] = e
		}
	}
	h.pruneHoursLocked()
}

func (h *history) sortedHoursLocked() []*histHour {
	out := make([]*histHour, 0, len(h.hours))
	for _, e := range h.hours {
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Hour < out[j].Hour })
	return out
}

func (h *history) sortedLocked() []*histDay {
	out := make([]*histDay, 0, len(h.days))
	for _, d := range h.days {
		out = append(out, d)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Date < out[j].Date })
	return out
}

// save writes the state file if anything changed since the last save.
func (h *history) save() error {
	h.saveMu.Lock()
	defer h.saveMu.Unlock()
	h.mu.Lock()
	if h.path == "" || !h.dirty {
		h.mu.Unlock()
		return nil
	}
	raw, _ := json.Marshal(historyState{Version: 1, Days: h.sortedLocked(), Hours: h.sortedHoursLocked()})
	h.dirty = false
	h.mu.Unlock()
	if err := writeFileAtomic(h.path, raw); err != nil {
		h.mu.Lock()
		h.dirty = true
		h.mu.Unlock()
		return err
	}
	return nil
}

// public is the history as /v1/stats/history serves it, oldest day first.
func (h *history) public() []map[string]any {
	h.mu.Lock()
	defer h.mu.Unlock()
	days := h.sortedLocked()
	out := make([]map[string]any, 0, len(days))
	for _, d := range days {
		e := map[string]any{"date": d.Date, "requests": d.Requests, "completed": d.Completed,
			"prompt_tokens": d.Prompt, "completion_tokens": d.Completion}
		if c := copyCounts(d.Classes); c != nil {
			e["classes"] = c
		}
		if d.NodeSamples > 0 {
			e["nodes_peak"] = d.NodesPeak
			e["nodes_avg"] = math.Round(float64(d.NodeSum)/float64(d.NodeSamples)*10) / 10
		}
		out = append(out, e)
	}
	return out
}

// publicHours is the hourly part of /v1/stats/history, oldest hour first; hours without
// requests are left out.
func (h *history) publicHours() []map[string]any {
	h.mu.Lock()
	defer h.mu.Unlock()
	hours := h.sortedHoursLocked()
	out := make([]map[string]any, 0, len(hours))
	for _, e := range hours {
		m := map[string]any{"hour": e.Hour, "requests": e.Requests, "completed": e.Completed,
			"prompt_tokens": e.Prompt, "completion_tokens": e.Completion}
		if c := copyCounts(e.Classes); c != nil {
			m["classes"] = c
		}
		out = append(out, m)
	}
	return out
}

// handleHistory serves GET /v1/stats/history without authentication.
func (g *Gateway) handleHistory(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Cache-Control", "public, max-age=60")
	writeJSON(w, http.StatusOK, map[string]any{
		"updated": time.Now().UTC().Format(time.RFC3339),
		"days":    g.hist.public(),
		"hours":   g.hist.publicHours(),
	})
}
