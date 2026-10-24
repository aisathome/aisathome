// Operations status of a science project's runner (theme A2): the runner posts a status JSON
// every minute to POST /v1/projects/<id>/status with that project's key; the gateway checks it
// against a strict schema, keeps the latest one in state_dir, and shows it with its own data
// (nodes online) and the health warnings (health.go) on the admin-only page /account/ops
// (ops_http.go). GET on the same path gives the project's own key the active warnings (for the
// runner's daily summary).
//
// The status is operator data, never secrets: unknown fields are refused, free text is short,
// and anything that looks like a key or token is refused. A key reports only for its own
// project.
//
// SPDX-License-Identifier: Apache-2.0
package gateway

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

const (
	// maxStatusBody bounds a posted status (trial 2's queue of eight attempts is about 6 KB).
	maxStatusBody = 64 << 10
	// statusMinInterval: a project posts at most this often (the runner posts every minute).
	statusMinInterval = 10 * time.Second
	// statusMaxAge refuses a status generated longer ago (or this far in the future: clock skew).
	statusMaxAge = time.Hour
	statusSchema = 1
)

// runnerStatus is what a runner posts (schema 1). Every field is listed here: others are
// refused.
type runnerStatus struct {
	Schema        int                       `json:"schema"`
	GeneratedAt   time.Time                 `json:"generated_at"`
	Runner        statusRunner              `json:"runner"`
	Attempts      []statusAttempt           `json:"attempts"`
	Candidates    []statusCandidate         `json:"candidates"`
	CI            *statusCI                 `json:"ci,omitempty"`
	Interruptions []statusInterruption      `json:"interruptions"`
	Baselines     map[string]statusBaseline `json:"baselines,omitempty"` // by task type
	Nodes         []statusNode              `json:"nodes,omitempty"`     // contracts of the operator's nodes
	CostToday     *statusCost               `json:"cost_today,omitempty"`
}

type statusRunner struct {
	Paused   bool    `json:"paused"`
	Service  string  `json:"service"`            // the queue service: active, inactive, failed, unknown
	FreeGB   float64 `json:"free_gb"`            // the work volume
	Instance string  `json:"instance,omitempty"` // e.g. "m7i.large spot eu-north-1c"
}

// statusAttempt is one attempt of the queue (or one finished today).
type statusAttempt struct {
	Attempt  string `json:"attempt"`
	TaskType string `json:"task_type,omitempty"` // task_types.py code: NS, DOC, GEN, LINT, NEW
	Size     string `json:"size,omitempty"`      // easy, medium, hard
	State    string `json:"state"`               // queued, running, interrupted, done, failed, skipped
	Stage    string `json:"stage,omitempty"`     // the queue service's stage (fresh, resume, score, ...)
	Round    int    `json:"round,omitempty"`     // follow-up round; 0 = the first
	Steps    int    `json:"steps"`               // model steps done in this round
	StepCap  int    `json:"step_cap,omitempty"`
	// UsedMin is what the time cap counts (BudgetKind "model": the model's own time; "awake":
	// the harness's awake time, before 2026-10-24's model-time cap); AwakeMin is awake time.
	UsedMin       float64     `json:"used_min"`
	BudgetMin     float64     `json:"budget_min,omitempty"`
	BudgetKind    string      `json:"budget_kind,omitempty"`
	AwakeMin      float64     `json:"awake_min"`
	CacheHit      *float64    `json:"cache_hit,omitempty"` // cached prompt tokens / prompt tokens, 0-1
	Interruptions int         `json:"interruptions"`
	AddedAt       *time.Time  `json:"added_at,omitempty"`
	StartedAt     *time.Time  `json:"started_at,omitempty"`
	LastActivity  *time.Time  `json:"last_activity,omitempty"` // the newest session event
	LastStep      *statusStep `json:"last_step,omitempty"`     // to find the node that served it
	Outcome       string      `json:"outcome,omitempty"`
}

type statusStep struct {
	Prompt     int       `json:"prompt_tokens"`
	Completion int       `json:"completion_tokens"`
	Ended      time.Time `json:"ended_at"`
}

type statusCandidate struct {
	Attempt string    `json:"attempt"`
	Since   time.Time `json:"since"`
	Outcome string    `json:"outcome,omitempty"`
}

type statusCI struct {
	Pause     bool       `json:"pause"`
	Reasons   []string   `json:"reasons,omitempty"`
	Since     *time.Time `json:"since,omitempty"` // the back-off's start, while it pauses
	CheckedAt time.Time  `json:"checked_at"`
}

type statusInterruption struct {
	At      time.Time `json:"at"`
	Attempt string    `json:"attempt,omitempty"`
	By      string    `json:"by"` // reboot, service stop, service restart
}

type statusBaseline struct {
	MinPerStep float64 `json:"min_per_step"` // median awake minutes per step of finished attempts
	Attempts   int     `json:"attempts"`
}

type statusNode struct {
	Node         string     `json:"node"`
	ContractEnds *time.Time `json:"contract_ends,omitempty"`
	USDPerHour   float64    `json:"usd_per_hour,omitempty"`
}

type statusCost struct {
	USD  float64 `json:"usd"`
	Note string  `json:"note,omitempty"`
}

var (
	statusIDRe   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)
	statusTypeRe = regexp.MustCompile(`^[A-Z]{1,8}$`)
	// secretLike: a long unbroken run of key characters (API keys, GitHub tokens, hashes) or a
	// known key prefix. A status never needs one.
	secretLike = regexp.MustCompile(`[A-Za-z0-9_+/=-]{32,}|(?i:\b(?:ak|sk|ghp|gho|ghs|ghu|github_pat|xox[abp])[-_][a-z0-9])|\bAKIA[0-9A-Z]{16}\b|(?i:\bbearer\s)|-----BEGIN`)
)

var (
	attemptStates = []string{"queued", "running", "interrupted", "done", "failed", "skipped"}
	serviceStates = []string{"active", "inactive", "failed", "activating", "deactivating", "unknown"}
	sizes         = []string{"", "easy", "medium", "hard"}
	budgetKinds   = []string{"", "model", "awake"}
)

func oneOf(v string, set []string) bool {
	for _, s := range set {
		if v == s {
			return true
		}
	}
	return false
}

// checkText accepts short printable text without anything that looks like a secret.
func checkText(field, s string, maxLen int) error {
	if len(s) > maxLen {
		return fmt.Errorf("%s: longer than %d bytes", field, maxLen)
	}
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return fmt.Errorf("%s: control characters", field)
		}
	}
	if secretLike.MatchString(s) {
		return fmt.Errorf("%s: looks like a key or token; a status never carries one", field)
	}
	return nil
}

func checkNum(field string, v, hi float64) error {
	if math.IsNaN(v) || math.IsInf(v, 0) || v < 0 || v > hi {
		return fmt.Errorf("%s: must be from 0 to %g", field, hi)
	}
	return nil
}

func checkID(field, s string) error {
	if !statusIDRe.MatchString(s) {
		return fmt.Errorf("%s: 1-64 letters, digits, '.', '_' or '-'", field)
	}
	return nil
}

// validate checks a decoded status beyond its shape.
func (s *runnerStatus) validate(now time.Time) error {
	if s.Schema != statusSchema {
		return fmt.Errorf("schema: must be %d", statusSchema)
	}
	if s.GeneratedAt.IsZero() || s.GeneratedAt.Before(now.Add(-statusMaxAge)) || s.GeneratedAt.After(now.Add(10*time.Minute)) {
		return errors.New("generated_at: missing, older than an hour, or in the future")
	}
	if !oneOf(s.Runner.Service, serviceStates) {
		return fmt.Errorf("runner.service: one of %s", strings.Join(serviceStates, ", "))
	}
	errs := []error{checkNum("runner.free_gb", s.Runner.FreeGB, 1e6), checkText("runner.instance", s.Runner.Instance, 80)}
	if len(s.Attempts) > 200 || len(s.Candidates) > 50 || len(s.Interruptions) > 100 || len(s.Baselines) > 20 || len(s.Nodes) > 25 {
		return errors.New("too many entries (attempts 200, candidates 50, interruptions 100, baselines 20, nodes 25)")
	}
	for i, a := range s.Attempts {
		f := fmt.Sprintf("attempts[%d]", i)
		errs = append(errs, checkID(f+".attempt", a.Attempt))
		if a.TaskType != "" && !statusTypeRe.MatchString(a.TaskType) {
			errs = append(errs, fmt.Errorf("%s.task_type: 1-8 capital letters", f))
		}
		if !oneOf(a.Size, sizes) {
			errs = append(errs, fmt.Errorf("%s.size: easy, medium or hard", f))
		}
		if !oneOf(a.State, attemptStates) {
			errs = append(errs, fmt.Errorf("%s.state: one of %s", f, strings.Join(attemptStates, ", ")))
		}
		if !oneOf(a.BudgetKind, budgetKinds) {
			errs = append(errs, fmt.Errorf("%s.budget_kind: model or awake", f))
		}
		if a.Stage != "" {
			errs = append(errs, checkID(f+".stage", a.Stage))
		}
		if a.Round < 0 || a.Round > 100 || a.Steps < 0 || a.Steps > 100000 || a.StepCap < 0 || a.StepCap > 100000 ||
			a.Interruptions < 0 || a.Interruptions > 10000 {
			errs = append(errs, fmt.Errorf("%s: round, steps, step_cap or interruptions out of range", f))
		}
		errs = append(errs, checkNum(f+".used_min", a.UsedMin, 1e5), checkNum(f+".budget_min", a.BudgetMin, 1e5),
			checkNum(f+".awake_min", a.AwakeMin, 1e5), checkText(f+".outcome", a.Outcome, 200))
		if a.CacheHit != nil {
			errs = append(errs, checkNum(f+".cache_hit", *a.CacheHit, 1))
		}
		if a.LastStep != nil && (a.LastStep.Prompt < 0 || a.LastStep.Completion < 0) {
			errs = append(errs, fmt.Errorf("%s.last_step: negative tokens", f))
		}
	}
	for i, c := range s.Candidates {
		f := fmt.Sprintf("candidates[%d]", i)
		errs = append(errs, checkID(f+".attempt", c.Attempt), checkText(f+".outcome", c.Outcome, 200))
	}
	if s.CI != nil {
		if len(s.CI.Reasons) > 10 {
			errs = append(errs, errors.New("ci.reasons: at most 10"))
		}
		for i, r := range s.CI.Reasons {
			errs = append(errs, checkText(fmt.Sprintf("ci.reasons[%d]", i), r, 200))
		}
	}
	for i, x := range s.Interruptions {
		f := fmt.Sprintf("interruptions[%d]", i)
		if x.Attempt != "" {
			errs = append(errs, checkID(f+".attempt", x.Attempt))
		}
		errs = append(errs, checkText(f+".by", x.By, 40))
	}
	for k, b := range s.Baselines {
		if !statusTypeRe.MatchString(k) {
			errs = append(errs, fmt.Errorf("baselines: key %q is not a task type", k))
		}
		errs = append(errs, checkNum("baselines."+k+".min_per_step", b.MinPerStep, 1e4))
		if b.Attempts < 0 {
			errs = append(errs, fmt.Errorf("baselines.%s.attempts: negative", k))
		}
	}
	for i, n := range s.Nodes {
		f := fmt.Sprintf("nodes[%d]", i)
		errs = append(errs, checkID(f+".node", n.Node), checkNum(f+".usd_per_hour", n.USDPerHour, 1000))
	}
	if s.CostToday != nil {
		errs = append(errs, checkNum("cost_today.usd", s.CostToday.USD, 1e5), checkText("cost_today.note", s.CostToday.Note, 200))
	}
	return errors.Join(errs...)
}

// decodeStatus reads one status object: unknown fields, trailing data and a body over
// maxStatusBody are refused.
func decodeStatus(body []byte, now time.Time) (*runnerStatus, error) {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	var s runnerStatus
	if err := dec.Decode(&s); err != nil {
		return nil, fmt.Errorf("not a status: %w", err)
	}
	if dec.More() {
		return nil, errors.New("not a status: data after the object")
	}
	if err := s.validate(now); err != nil {
		return nil, err
	}
	return &s, nil
}

// storedStatus is a project's latest status and when the gateway got it.
type storedStatus struct {
	Project    string        `json:"project"`
	ReceivedAt time.Time     `json:"received_at"`
	Status     *runnerStatus `json:"status"`
}

// projectStatuses keeps each project's latest status, in memory and in state_dir.
type projectStatuses struct {
	mu   sync.Mutex
	dir  string // state dir; "" = memory only
	byID map[string]*storedStatus
	log  *slog.Logger
}

func newProjectStatuses(dir string, log *slog.Logger) *projectStatuses {
	return &projectStatuses{dir: dir, byID: map[string]*storedStatus{}, log: log}
}

func (p *projectStatuses) file(id string) string { return filepath.Join(p.dir, "status-"+id+".json") }

// get returns a project's latest status (loading it from state_dir after a restart), or nil.
func (p *projectStatuses) get(id string) *storedStatus {
	p.mu.Lock()
	defer p.mu.Unlock()
	if s, ok := p.byID[id]; ok {
		return s
	}
	var s *storedStatus
	if p.dir != "" && projectID.MatchString(id) {
		if raw, err := os.ReadFile(p.file(id)); err == nil {
			var st storedStatus
			if err := json.Unmarshal(raw, &st); err != nil || st.Status == nil {
				p.log.Warn("project status unreadable, ignoring it", "project", id, "err", err)
			} else {
				s = &st
			}
		}
	}
	p.byID[id] = s
	return s
}

// put stores a project's new status; it refuses one sooner than statusMinInterval after the
// last.
func (p *projectStatuses) put(id string, st *runnerStatus, now time.Time) (bool, error) {
	prev := p.get(id)
	p.mu.Lock()
	defer p.mu.Unlock()
	if prev != nil && now.Sub(prev.ReceivedAt) < statusMinInterval && now.After(prev.ReceivedAt) {
		return false, nil
	}
	s := &storedStatus{Project: id, ReceivedAt: now.UTC(), Status: st}
	p.byID[id] = s
	if p.dir == "" {
		return true, nil
	}
	raw, _ := json.Marshal(s)
	return true, writeFileAtomic(p.file(id), raw)
}

// projectFor checks that the request's key belongs to project id: 401 without a project key,
// 403 for another project's key.
func (g *Gateway) projectFor(w http.ResponseWriter, r *http.Request, id string) bool {
	p := g.project(bearer(r))
	if p == nil {
		apiError(w, http.StatusUnauthorized, "a project key is needed")
		return false
	}
	if p.ID != id {
		apiError(w, http.StatusForbidden, "this key reports only for its own project")
		return false
	}
	return true
}

// handleStatusPost serves POST /v1/projects/{id}/status.
func (g *Gateway) handleStatusPost(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !g.projectFor(w, r, id) {
		return
	}
	if mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || mt != "application/json" {
		apiError(w, http.StatusUnsupportedMediaType, "send the status as application/json")
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxStatusBody))
	if err != nil {
		apiError(w, http.StatusRequestEntityTooLarge, fmt.Sprintf("a status is at most %d bytes", maxStatusBody))
		return
	}
	now := time.Now()
	st, err := decodeStatus(body, now)
	if err != nil {
		apiError(w, http.StatusBadRequest, err.Error())
		return
	}
	ok, err := g.pstatus.put(id, st, now)
	if !ok {
		w.Header().Set("Retry-After", "10")
		apiError(w, http.StatusTooManyRequests, "one status every 10 seconds at most")
		return
	}
	if err != nil {
		g.log.Warn("saving project status failed", "project", id, "err", err)
	}
	writeJSON(w, http.StatusOK, map[string]any{"stored": true, "warnings": len(g.healthOf(id, now))})
}

// handleStatusGet serves GET /v1/projects/{id}/status to the project's own key: when the last
// status came, and the active health warnings (the runner's daily summary lists them).
func (g *Gateway) handleStatusGet(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !g.projectFor(w, r, id) {
		return
	}
	now := time.Now()
	out := map[string]any{"project": id, "warnings": g.healthOf(id, now)}
	if s := g.pstatus.get(id); s != nil {
		out["received_at"] = s.ReceivedAt.Format(time.RFC3339)
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, out)
}
