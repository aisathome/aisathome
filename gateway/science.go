// Public science results of a project (the website's /science/<id>/ page): the project's runner
// posts its results once a day to POST /v1/projects/<id>/science with the project key (built
// from its run records and Tau Ceti's public reviews); anyone can read them at GET on the same
// path, together with a short "working on now" view of the runner's latest status
// (projstatus.go): the running tasks' target, type, size and state, and how many are queued.
//
// Everything here is public, so the schema is strict and narrow: unknown fields are refused,
// links must point at GitHub pull requests and their comments, free text is bounded, and text
// that names a reviewer model or looks like a key is refused. The "now" view copies only the
// whitelisted status fields; attempt ids, outcomes, budgets, the instance and costs stay on the
// admin-only operations page.
//
// SPDX-License-Identifier: Apache-2.0
package gateway

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
	// maxScienceBody bounds a posted result set (about 40 KB with nine PRs and their rounds).
	maxScienceBody = 1 << 20
	// scienceMinInterval: a project posts at most this often (the runner posts once a day).
	scienceMinInterval = 10 * time.Second
	// nowMaxAge: a runner status older than this is not shown as "working on now".
	nowMaxAge = time.Hour
)

// scienceResults is what a runner posts (and what GET returns, plus received_at and now).
// Every field is listed here: others are refused.
type scienceResults struct {
	Project        string            `json:"project"`
	ProjectURL     string            `json:"project_url"`
	Repository     string            `json:"repository"`
	Model          string            `json:"model"`
	GeneratedAt    time.Time         `json:"generated_at"`
	StatesAsOf     string            `json:"states_as_of"` // YYYY-MM-DD
	Methodology    string            `json:"methodology"`
	TypeLabels     map[string]string `json:"type_labels,omitempty"` // task type code -> label
	Totals         scienceTotals     `json:"totals"`
	PRs            []sciencePR       `json:"prs"`
	TasksWithoutPR []scienceTask     `json:"tasks_without_pr"`
}

// scienceTotals: counts are never negative; a value never recorded is null.
type scienceTotals struct {
	PRsOpened               int      `json:"prs_opened"`
	PRsMerged               int      `json:"prs_merged"`
	PRsClosed               int      `json:"prs_closed"`
	PRsOpen                 int      `json:"prs_open"`
	FirstReviewApproved     int      `json:"first_review_approved"`
	FirstReviewApprovalRate *float64 `json:"first_review_approval_rate"`
	Attempts                int      `json:"attempts"`
	AttemptsPassing         int      `json:"attempts_passing_local_checks"`
	LocalCheckPassRate      *float64 `json:"local_check_pass_rate"`
	GPUHours                float64  `json:"gpu_hours"`
	AttemptsWithoutGPUTime  int      `json:"attempts_without_gpu_time"`
	QwenTokens              int64    `json:"qwen_tokens"`
	QwenOutputTokens        int64    `json:"qwen_output_tokens"`
	QwenTokensPerMergedPR   *float64 `json:"qwen_tokens_per_merged_pr"`
	AttemptsWithoutTokenCnt int      `json:"attempts_without_token_count"`
}

// scienceCost is the cost of one task over all its attempts (fields shared by PRs and tasks
// without a PR).
type scienceCost struct {
	Attempts               int      `json:"attempts"`
	QwenTokens             *int64   `json:"qwen_tokens"`
	QwenOutputTokens       *int64   `json:"qwen_output_tokens"`
	GPUMinutes             *float64 `json:"gpu_minutes"`
	AttemptsWithoutTokens  int      `json:"attempts_without_tokens"`
	AttemptsWithoutGPUTime int      `json:"attempts_without_gpu_time"`
}

type sciencePR struct {
	Number              int            `json:"number"`
	URL                 string         `json:"url"`
	Title               string         `json:"title"`
	Type                string         `json:"type"`
	Size                *string        `json:"size"`
	State               string         `json:"state"` // merged, open, closed
	Opened              time.Time      `json:"opened"`
	Merged              *time.Time     `json:"merged"`
	Closed              *time.Time     `json:"closed"`
	FixRounds           int            `json:"fix_rounds"`
	FirstReviewApproved bool           `json:"first_review_approved"`
	ReviewURL           *string        `json:"review_url"` // Tau Ceti's review scoreboard comment
	Rounds              []scienceRound `json:"rounds"`
	scienceCost
}

// scienceRound is one reviewed commit of a PR: the rubrics that asked for changes, each with
// Tau Ceti's own one-line reason, verbatim, linked to its comment.
type scienceRound struct {
	Round   int             `json:"round"`
	Commit  string          `json:"commit"`
	At      time.Time       `json:"at"`
	Result  string          `json:"result"` // "changes requested", "approved", ...
	Changes []scienceChange `json:"changes"`
}

type scienceChange struct {
	Rubric  string `json:"rubric"`
	Verdict string `json:"verdict"`
	Reason  string `json:"reason"`
	URL     string `json:"url"`
}

// scienceTask is a task whose attempts have not led to a PR (failures before a PR count too).
type scienceTask struct {
	Target      string     `json:"target"`
	Type        string     `json:"type"`
	Size        *string    `json:"size"`
	State       string     `json:"state"` // failed, checks-passed
	LastAttempt *time.Time `json:"last_attempt"`
	scienceCost
}

var (
	prURLRe      = regexp.MustCompile(`^https://github\.com/[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+/pull/[0-9]{1,9}$`)
	commentURLRe = regexp.MustCompile(`^https://github\.com/[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+/pull/[0-9]{1,9}#(discussion_r|issuecomment-|pullrequestreview-)[0-9]{1,20}$`)
	repoURLRe    = regexp.MustCompile(`^https://github\.com/[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)
	siteURLRe    = regexp.MustCompile(`^https://[a-z0-9.-]+(/[A-Za-z0-9_./-]*)?$`)
	commitRe     = regexp.MustCompile(`^[0-9a-f]{7,40}$`)
	rubricRe     = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,39}$`)
	verdictRe    = regexp.MustCompile(`^[a-z][a-z_ -]{0,39}$`)
	dayRe        = regexp.MustCompile(`^[0-9]{4}-[0-9]{2}-[0-9]{2}$`)
	// reviewerModel: the public page names no reviewer model (owner, 2026-10-24).
	reviewerModel = regexp.MustCompile(`(?i)claude|anthropic|\bopus\b|\bsonnet\b|\bhaiku\b|\bcodex\b|\bgpt-|\bgemini\b`)
	// publicSecret: the key-shaped text of secretLike without its long-run rule (public text
	// quotes long Lean names and paths).
	publicSecret = regexp.MustCompile(`(?i:\b(?:ak|sk|ghp|gho|ghs|ghu|github_pat|xox[abp])[-_][a-z0-9]{8})|\bAKIA[0-9A-Z]{16}\b|(?i:\bbearer\s)|-----BEGIN`)
	prStates     = []string{"merged", "open", "closed"}
	taskStates   = []string{"failed", "checks-passed"}
	taskSizes    = []string{"easy", "medium", "hard"}
)

// checkPublic accepts printable text up to maxLen bytes that names no reviewer model and holds
// nothing key-shaped.
func checkPublic(field, s string, maxLen int) error {
	if len(s) > maxLen {
		return fmt.Errorf("%s: longer than %d bytes", field, maxLen)
	}
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return fmt.Errorf("%s: control characters", field)
		}
	}
	if reviewerModel.MatchString(s) {
		return fmt.Errorf("%s: names a reviewer model; the public page never does", field)
	}
	if publicSecret.MatchString(s) {
		return fmt.Errorf("%s: looks like a key or token", field)
	}
	return nil
}

func match(field string, re *regexp.Regexp, s string) error {
	if !re.MatchString(s) {
		return fmt.Errorf("%s: %q is not allowed here", field, s)
	}
	return nil
}

func checkSize(field string, s *string) error {
	if s != nil && !oneOf(*s, taskSizes) {
		return fmt.Errorf("%s: easy, medium, hard or null", field)
	}
	return nil
}

func (c *scienceCost) validate(f string) []error {
	var errs []error
	if c.Attempts < 0 || c.Attempts > 10000 || c.AttemptsWithoutTokens < 0 || c.AttemptsWithoutGPUTime < 0 ||
		c.AttemptsWithoutTokens > c.Attempts || c.AttemptsWithoutGPUTime > c.Attempts {
		errs = append(errs, fmt.Errorf("%s: attempt counts out of range", f))
	}
	if (c.QwenTokens != nil && *c.QwenTokens < 0) || (c.QwenOutputTokens != nil && *c.QwenOutputTokens < 0) {
		errs = append(errs, fmt.Errorf("%s: negative tokens", f))
	}
	if c.GPUMinutes != nil {
		errs = append(errs, checkNum(f+".gpu_minutes", *c.GPUMinutes, 1e6))
	}
	return errs
}

// validate checks decoded results beyond their shape.
func (s *scienceResults) validate(now time.Time) error {
	if s.GeneratedAt.IsZero() || s.GeneratedAt.After(now.Add(10*time.Minute)) {
		return errors.New("generated_at: missing or in the future")
	}
	if len(s.PRs) > 1000 || len(s.TasksWithoutPR) > 1000 || len(s.TypeLabels) > 20 {
		return errors.New("too many entries (prs 1000, tasks_without_pr 1000, type_labels 20)")
	}
	errs := []error{
		checkPublic("project", s.Project, 80), checkPublic("model", s.Model, 80),
		checkPublic("methodology", s.Methodology, 2000),
		match("project_url", siteURLRe, s.ProjectURL), match("repository", repoURLRe, s.Repository),
		match("states_as_of", dayRe, s.StatesAsOf),
	}
	for k, v := range s.TypeLabels {
		if !statusTypeRe.MatchString(k) {
			errs = append(errs, fmt.Errorf("type_labels: key %q is not a task type", k))
		}
		errs = append(errs, checkPublic("type_labels."+k, v, 60))
	}
	t := s.Totals
	if t.PRsOpened < 0 || t.PRsMerged < 0 || t.PRsClosed < 0 || t.PRsOpen < 0 || t.FirstReviewApproved < 0 ||
		t.Attempts < 0 || t.AttemptsPassing < 0 || t.AttemptsWithoutGPUTime < 0 || t.QwenTokens < 0 ||
		t.QwenOutputTokens < 0 || t.AttemptsWithoutTokenCnt < 0 {
		errs = append(errs, errors.New("totals: negative count"))
	}
	errs = append(errs, checkNum("totals.gpu_hours", t.GPUHours, 1e7))
	for name, r := range map[string]*float64{"first_review_approval_rate": t.FirstReviewApprovalRate, "local_check_pass_rate": t.LocalCheckPassRate} {
		if r != nil {
			errs = append(errs, checkNum("totals."+name, *r, 1))
		}
	}
	if t.QwenTokensPerMergedPR != nil {
		errs = append(errs, checkNum("totals.qwen_tokens_per_merged_pr", *t.QwenTokensPerMergedPR, 1e13))
	}
	for i, p := range s.PRs {
		f := fmt.Sprintf("prs[%d]", i)
		errs = append(errs, match(f+".url", prURLRe, p.URL), checkPublic(f+".title", p.Title, 300),
			checkPublic(f+".type", p.Type, 60), checkSize(f+".size", p.Size))
		if p.Number <= 0 || !strings.HasSuffix(p.URL, fmt.Sprintf("/pull/%d", p.Number)) {
			errs = append(errs, fmt.Errorf("%s: number and url disagree", f))
		}
		if !oneOf(p.State, prStates) {
			errs = append(errs, fmt.Errorf("%s.state: merged, open or closed", f))
		}
		if p.Opened.IsZero() || (p.State == "merged") != (p.Merged != nil) {
			errs = append(errs, fmt.Errorf("%s: opened missing, or merged date without the merged state", f))
		}
		if p.FixRounds < 0 || p.FixRounds > 100 || len(p.Rounds) > 100 {
			errs = append(errs, fmt.Errorf("%s: fix_rounds or rounds out of range", f))
		}
		if p.ReviewURL != nil {
			errs = append(errs, match(f+".review_url", commentURLRe, *p.ReviewURL))
		}
		errs = append(errs, p.scienceCost.validate(f)...)
		for j, r := range p.Rounds {
			g := fmt.Sprintf("%s.rounds[%d]", f, j)
			errs = append(errs, match(g+".commit", commitRe, r.Commit), checkPublic(g+".result", r.Result, 60))
			if r.Round != j+1 || r.At.IsZero() || len(r.Changes) > 200 {
				errs = append(errs, fmt.Errorf("%s: rounds are numbered 1, 2, ... with a time and at most 200 changes", g))
			}
			for k, c := range r.Changes {
				h := fmt.Sprintf("%s.changes[%d]", g, k)
				errs = append(errs, match(h+".rubric", rubricRe, c.Rubric), match(h+".verdict", verdictRe, c.Verdict),
					checkPublic(h+".reason", c.Reason, 1500), match(h+".url", commentURLRe, c.URL))
			}
		}
	}
	for i, x := range s.TasksWithoutPR {
		f := fmt.Sprintf("tasks_without_pr[%d]", i)
		errs = append(errs, checkPublic(f+".target", x.Target, 300), checkPublic(f+".type", x.Type, 60), checkSize(f+".size", x.Size))
		if !oneOf(x.State, taskStates) {
			errs = append(errs, fmt.Errorf("%s.state: failed or checks-passed", f))
		}
		errs = append(errs, x.scienceCost.validate(f)...)
	}
	return errors.Join(errs...)
}

// decodeScience reads one result set: unknown fields, trailing data and invalid values are
// refused.
func decodeScience(body []byte, now time.Time) (*scienceResults, error) {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	var s scienceResults
	if err := dec.Decode(&s); err != nil {
		return nil, fmt.Errorf("not a result set: %w", err)
	}
	if dec.More() {
		return nil, errors.New("not a result set: data after the object")
	}
	if err := s.validate(now); err != nil {
		return nil, err
	}
	return &s, nil
}

// storedScience is a project's latest results and when the gateway got them.
type storedScience struct {
	ReceivedAt time.Time       `json:"received_at"`
	Results    *scienceResults `json:"results"`
}

// projectScience keeps each project's latest results, in memory and in state_dir.
type projectScience struct {
	mu   sync.Mutex
	dir  string
	byID map[string]*storedScience
}

func newProjectScience(dir string) *projectScience {
	return &projectScience{dir: dir, byID: map[string]*storedScience{}}
}

func (p *projectScience) file(id string) string { return filepath.Join(p.dir, "science-"+id+".json") }

func (p *projectScience) get(id string) *storedScience {
	p.mu.Lock()
	defer p.mu.Unlock()
	if s, ok := p.byID[id]; ok {
		return s
	}
	var s *storedScience
	if p.dir != "" && projectID.MatchString(id) {
		if raw, err := os.ReadFile(p.file(id)); err == nil {
			var st storedScience
			if json.Unmarshal(raw, &st) == nil && st.Results != nil {
				s = &st
			}
		}
	}
	p.byID[id] = s
	return s
}

func (p *projectScience) put(id string, r *scienceResults, now time.Time) (bool, error) {
	prev := p.get(id)
	p.mu.Lock()
	defer p.mu.Unlock()
	if prev != nil && now.Sub(prev.ReceivedAt) < scienceMinInterval && now.After(prev.ReceivedAt) {
		return false, nil
	}
	s := &storedScience{ReceivedAt: now.UTC(), Results: r}
	p.byID[id] = s
	if p.dir == "" {
		return true, nil
	}
	raw, _ := json.Marshal(s)
	return true, writeFileAtomic(p.file(id), raw)
}

// handleSciencePost serves POST /v1/projects/{id}/science (the project's own key).
func (g *Gateway) handleSciencePost(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !g.projectFor(w, r, id) {
		return
	}
	if mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || mt != "application/json" {
		apiError(w, http.StatusUnsupportedMediaType, "send the results as application/json")
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxScienceBody))
	if err != nil {
		apiError(w, http.StatusRequestEntityTooLarge, fmt.Sprintf("results are at most %d bytes", maxScienceBody))
		return
	}
	now := time.Now()
	res, err := decodeScience(body, now)
	if err != nil {
		apiError(w, http.StatusBadRequest, err.Error())
		return
	}
	ok, err := g.psci.put(id, res, now)
	if !ok {
		w.Header().Set("Retry-After", "10")
		apiError(w, http.StatusTooManyRequests, "one result set every 10 seconds at most")
		return
	}
	if err != nil {
		g.log.Warn("saving science results failed", "project", id, "err", err)
	}
	writeJSON(w, http.StatusOK, map[string]any{"stored": true, "prs": len(res.PRs)})
}

// sciNow is the public "working on now" view of a runner status.
type sciNow struct {
	ReportedAt time.Time    `json:"reported_at"`
	Running    []sciRunning `json:"running"`
	Queued     int          `json:"queued"`
}

type sciRunning struct {
	Target string `json:"target"`
	Type   string `json:"type"`
	Size   string `json:"size,omitempty"`
	State  string `json:"state"` // running, or interrupted (resumes when the runner is back)
}

// publicNow copies the whitelisted fields of a status: the running and interrupted tasks'
// target, type (its label when the results name one), size and state, and the queued count.
// nil without a status or with one older than nowMaxAge.
func publicNow(st *storedStatus, labels map[string]string, now time.Time) *sciNow {
	if st == nil || st.Status == nil || now.Sub(st.Status.GeneratedAt) > nowMaxAge {
		return nil
	}
	out := &sciNow{ReportedAt: st.Status.GeneratedAt.UTC(), Running: []sciRunning{}}
	for _, a := range st.Status.Attempts {
		switch a.State {
		case "queued":
			out.Queued++
		case "running", "interrupted":
			typ := a.TaskType
			if l, ok := labels[typ]; ok {
				typ = l
			}
			out.Running = append(out.Running, sciRunning{Target: a.Title, Type: typ, Size: a.Size, State: a.State})
		}
	}
	return out
}

// handleScienceGet serves GET /v1/projects/{id}/science to anyone: the latest results the
// runner posted, when they came, and what it is working on now.
func (g *Gateway) handleScienceGet(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	known := false
	for _, p := range g.cfg.Projects {
		known = known || p.ID == id
	}
	s := g.psci.get(id)
	if !known || s == nil {
		apiError(w, http.StatusNotFound, "no published results for this project")
		return
	}
	now := time.Now()
	type out struct {
		*scienceResults
		ReceivedAt time.Time `json:"received_at"`
		Now        *sciNow   `json:"now"`
	}
	w.Header().Set("Cache-Control", "public, max-age=60")
	writeJSON(w, http.StatusOK, out{s.Results, s.ReceivedAt, publicNow(g.pstatus.get(id), s.Results.TypeLabels, now)})
}
