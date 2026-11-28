// How attempts ended, per node and model, per UTC day (`aisathome-gateway accounts node-failures`): did
// a smaller model or context make requests fail? A 16 GB node serves another model file (UD-Q3_K_XL,
// "qwen3.8-27b-q3") with less context (81,920) than a 24 GB node (UD-IQ4_XS, "qwen3.8-27b", 196,608), so
// the model name is the tier. Before these counts the usage log held only attempts that produced output
// (a context overflow before generation left no record at all), and it does not tell a cut at max_tokens
// from one at the end of the context.
//
// Each attempt on a node counts once, by how it ended:
//
//   - ok: the answer reached the client, finished by the model;
//   - length_max_tokens: finish_reason "length" at the request's max_tokens;
//   - length_context: finish_reason "length" before max_tokens: llama-server stops there when the
//     node's context is full (the gateway may route a request with less room than max_tokens when no
//     node fits it all, routing.go needMin);
//   - failed:<category>: the node or its engine failed the attempt, before or after output, with the
//     category errCategory gives its error (context_length_exceeded, engine returned 500, engine stream
//     ended early, node stalled, node disconnected, ...), or the category of an error event the engine
//     put in its stream: context_length_exceeded, tool_call_parse (llama-server could not parse the
//     model's tool call or reasoning syntax) or engine_error;
//   - cancelled (the client left or did not keep up), preempted (science gave its slot to a person),
//     blocked (a guard stopped the answer), gateway_busy (the gateway's own buffers were full).
//
// bad_tool_calls counts, beside that, answers with at least one tool call the gateway could not use
// (no name, or arguments that are not a JSON object: signals.go). Requests the gateway refused before
// any node (no node with enough context: too_long, a session's model offline, ...) count as
// refused:<reason> under the node "-" and the model asked for (the session's model, when it continues
// one; "(unknown)" for a name no node offered, so nothing a client typed reaches the file).
//
// Counts only: no request, prompt, answer, error text, user, key, account or operator; a node's id and
// its model's name and context. In memory, saved in the state dir (failures.json) every few seconds and
// on close, for failuresKeepDays days; at most failuresMaxRows node rows a day, the rest under "(other)".
// Names are kept to 128 bytes of printable ASCII.
//
// SPDX-License-Identifier: Apache-2.0
package gateway

import (
	"bytes"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	// FailuresFile is the counts' file in the state dir.
	FailuresFile     = "failures.json"
	failuresKeepDays = 35
	failuresMaxRows  = 1000 // per day: nodes x models; pilots come and go
	failuresVersion  = 1

	kindOK              = "ok"
	kindLengthMax       = "length_max_tokens"
	kindLengthContext   = "length_context"
	kindBadToolCalls    = "bad_tool_calls"
	kindCancelled       = "cancelled"
	kindPreempted       = "preempted"
	kindBlocked         = "blocked"
	kindGatewayBusy     = "gateway_busy"
	kindFailedPrefix    = "failed:"
	kindRefusedPrefix   = "refused:"
	engineToolCallParse = "tool_call_parse"
	engineErrorEvent    = "engine_error"
	failuresOther       = "(other)"
	failuresNoNode      = "-"
)

// FailureRow is one node's and model's counts on one day, by kind (the list above).
type FailureRow struct {
	Node  string         `json:"node"`
	Model string         `json:"model"`
	Ctx   int            `json:"ctx,omitempty"` // the context the node announced for the model
	Kinds map[string]int `json:"kinds"`
}

// FailureDay is one UTC day's rows.
type FailureDay struct {
	Day  string        `json:"day"` // 2006-01-02
	Rows []*FailureRow `json:"rows"`
}

type failuresState struct {
	Version int           `json:"version"`
	Days    []*FailureDay `json:"days"`
}

// failureCounts are the counts in memory; nil-safe, like the gateway's other optional parts.
type failureCounts struct {
	mu     sync.Mutex
	saveMu sync.Mutex
	path   string // "" = memory only
	dirty  bool
	days   map[string]map[string]*FailureRow // day -> node\x00model -> row
}

func newFailureCounts() *failureCounts {
	return &failureCounts{days: map[string]map[string]*FailureRow{}}
}

// add counts kinds for node's model (with its context) at now.
func (f *failureCounts) add(node, model string, ctx int, now time.Time, kinds ...string) {
	if f == nil || len(kinds) == 0 {
		return
	}
	node, model = printable(node), printable(model)
	day := now.UTC().Format(time.DateOnly)
	f.mu.Lock()
	defer f.mu.Unlock()
	rows := f.days[day]
	if rows == nil {
		rows = map[string]*FailureRow{}
		f.days[day] = rows
		f.pruneLocked(now)
	}
	key := node + "\x00" + model
	r := rows[key]
	if r == nil {
		if len(rows) >= failuresMaxRows && node != failuresNoNode {
			node, model, ctx, key = failuresOther, failuresOther, 0, failuresOther+"\x00"+failuresOther
			r = rows[key]
		}
		if r == nil {
			r = &FailureRow{Node: node, Model: model, Kinds: map[string]int{}}
			rows[key] = r
		}
	}
	r.Ctx = max(r.Ctx, ctx)
	for _, k := range kinds {
		r.Kinds[k]++
	}
	f.dirty = true
}

// pruneLocked drops the days older than failuresKeepDays. Caller holds f.mu.
func (f *failureCounts) pruneLocked(now time.Time) {
	oldest := now.UTC().AddDate(0, 0, -failuresKeepDays+1).Format(time.DateOnly)
	for d := range f.days {
		if d < oldest {
			delete(f.days, d)
			f.dirty = true
		}
	}
}

// snapshot is the counts as saved: days and rows sorted.
func (f *failureCounts) snapshotLocked() failuresState {
	st := failuresState{Version: failuresVersion}
	for d, rows := range f.days {
		day := &FailureDay{Day: d}
		for _, r := range rows {
			c := *r
			c.Kinds = make(map[string]int, len(r.Kinds))
			for k, v := range r.Kinds {
				c.Kinds[k] = v
			}
			day.Rows = append(day.Rows, &c)
		}
		sort.Slice(day.Rows, func(i, j int) bool {
			a, b := day.Rows[i], day.Rows[j]
			return a.Node < b.Node || (a.Node == b.Node && a.Model < b.Model)
		})
		st.Days = append(st.Days, day)
	}
	sort.Slice(st.Days, func(i, j int) bool { return st.Days[i].Day < st.Days[j].Day })
	return st
}

func (f *failureCounts) save() error {
	if f == nil {
		return nil
	}
	f.saveMu.Lock()
	defer f.saveMu.Unlock()
	f.mu.Lock()
	f.pruneLocked(time.Now())
	if f.path == "" || !f.dirty {
		f.mu.Unlock()
		return nil
	}
	raw, err := json.Marshal(f.snapshotLocked())
	f.dirty = false
	f.mu.Unlock()
	if err == nil {
		err = writeFileAtomic(f.path, raw)
	}
	if err != nil {
		f.mu.Lock()
		f.dirty = true
		f.mu.Unlock()
	}
	return err
}

func (f *failureCounts) load(log *slog.Logger, now time.Time) {
	st, err := ReadFailures(f.path)
	if errors.Is(err, os.ErrNotExist) {
		return
	}
	if err != nil {
		log.Warn("failure counts unreadable, setting them aside", "file", f.path, "err", err)
		os.Rename(f.path, f.path+".bad")
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, d := range st {
		rows := map[string]*FailureRow{}
		for _, r := range d.Rows {
			if r != nil && r.Kinds != nil {
				rows[r.Node+"\x00"+r.Model] = r
			}
		}
		f.days[d.Day] = rows
	}
	f.pruneLocked(now)
}

// ReadFailures reads the saved counts (the state dir's FailuresFile), oldest day first.
func ReadFailures(path string) ([]*FailureDay, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var st failuresState
	if err := json.Unmarshal(raw, &st); err != nil {
		return nil, err
	}
	return st.Days, nil
}

// engineErrorKind is the category of an error event the engine put in its stream (llama-server sends
// `{"error": {...}}` and ends the stream), or "" when chunk is none. The message is read here and
// dropped: it may quote the model's output.
func engineErrorKind(chunk []byte) string {
	if !bytes.Contains(chunk, []byte(`"error"`)) {
		return ""
	}
	var c struct {
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(chunk, &c) != nil || c.Error == nil {
		return ""
	}
	m := strings.ToLower(c.Error.Message)
	switch {
	case isContextOverflow(m):
		return "context_length_exceeded"
	case strings.Contains(m, "failed to parse"), strings.Contains(m, "parse tool call"):
		return engineToolCallParse
	}
	return engineErrorEvent
}

// attemptKinds is how rl's attempt ended, as counted above, for run's err.
func (rl *relay) attemptKinds(err error) []string {
	if rl.engineErr != "" { // the engine said why, whatever the stream did after
		return []string{kindFailedPrefix + rl.engineErr}
	}
	var flagged *guardError
	switch {
	case err == nil:
		kinds := []string{kindOK}
		if rec := rl.final; rec != nil && rec.Signals != nil {
			if rec.Signals.Finish == "length" {
				kinds[0] = kindLengthContext
				// The node's own count, unclamped: the usage record's is bounded by the chunks the gateway
				// saw, and a node that lies about it only mislabels its own cut answers. Without one
				// (a done message may carry no usage), the chunks with text, about one token each.
				out := rl.reportedOut
				if out < 0 {
					out = rl.relayed
				}
				if rl.maxTokens > 0 && out >= rl.maxTokens {
					kinds[0] = kindLengthMax
				}
			}
			if rec.Signals.BadCalls > 0 {
				kinds = append(kinds, kindBadToolCalls)
			}
		}
		return kinds
	case errors.Is(err, errPreempted):
		return []string{kindPreempted}
	case errors.As(err, &flagged):
		return []string{kindBlocked}
	case errors.Is(err, errGatewayBusy):
		return []string{kindGatewayBusy}
	case rl.ctx.Err() != nil, errors.Is(err, errClientSlow):
		return []string{kindCancelled}
	}
	return []string{kindFailedPrefix + errCategory(err.Error())}
}

// countAttempt counts how rl's attempt on its node ended (run, once per attempt).
func (g *Gateway) countAttempt(rl *relay, err error) {
	ctx := 0
	for _, m := range rl.n.models { // fixed at the node's hello
		if m.Name == rl.model {
			ctx = m.Ctx
		}
	}
	g.fails.add(rl.n.id, rl.model, ctx, time.Now(), rl.attemptKinds(err)...)
}

// printable keeps a node's or model's name to 128 bytes of printable ASCII ('?' for anything else), so
// a name cannot carry control characters into the operator's terminal or split a report's rows.
func printable(s string) string {
	if len(s) > 128 {
		s = s[:128]
	}
	b := []byte(s)
	for i, c := range b {
		if c < 0x20 || c > 0x7e {
			b[i] = '?'
		}
	}
	return string(b)
}

// countRefusal counts a request refused before any node, for reason (unavailable).
func (g *Gateway) countRefusal(rt *route, want, reason string) {
	model := want
	if rt.model != "" {
		model = rt.model
	} else {
		g.mu.Lock()
		_, known := g.known[want] // names nodes offered (models and roles), at most maxKnown
		g.mu.Unlock()
		if !known || reason == "unknown_model" {
			model = "(unknown)" // whatever a client typed never reaches the file
		}
	}
	g.fails.add(failuresNoNode, model, 0, time.Now(), kindRefusedPrefix+reason)
}
