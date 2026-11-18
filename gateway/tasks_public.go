// The network's tasks in one public schema (decision 0019, D8; owner 2026-11-14): GET /v1/tasks,
// without a key, across every project with public_queue on (project.go), for the site's task tables
// and the "All tasks" page. A project without public_queue is never in it.
//
// One schema for every project, with only fields that are public anyway:
//
//	project, id, kind, role, state, outcome, try,
//	parent, needs, retry_of, replaces, closed_by,   the relations (tasks.go)
//	labels, hole_id,                                the project's own public names
//	steps, model_min, tokens (prompt + completion), prompt_tokens, cached_tokens, completion_tokens,
//	uncached_prompt_tokens, tokens_computed, tokens_with_cache      explicit counts (below)
//	created_at, started_at, finished_at, updated_at,
//	node_classes                                    trusted, volunteer or pilot: the nodes that served it
//	live                                            while it runs: the gateway's own count of its
//	                                                model calls so far (tasks_live.go), else null
//
// Token counts: the result's prompt_tokens and tokens are what they always were (the worker's prompt
// count, which leaves cache re-reads out when it reports cached_tokens, and an incomplete count of
// unknown meaning when it does not). Read the explicit ones instead: uncached_prompt_tokens (prompt
// tokens the model computed), tokens_computed (those plus completion_tokens) and tokens_with_cache
// (plus cached_tokens), all null unless the result reported cached_tokens. `live` has the same three.
//
// A field the task does not have is null (labels {}, needs and node_classes []): a task made before
// the typed fields has no relations, and its role and labels come from its spec as on the public
// queue (queue_public.go: trial, leaf, group, theorem, outline, hole). Never in it: the spec, the
// title, prompts or model text, the worker, the lease, the error text, the events, who created it,
// node ids or their operators. Labels were checked at creation (public strings only) and are checked
// again here; a task whose id does not match pubTaskIDRe is left out.
//
// Query: project, state, kind, role (each a comma-separated list; none: all), limit (1-500, default
// 100) and after (the `next` of the page before). Newest first by creation, then project and id; the
// cursor names the last task of a page, so a page never repeats or skips a task that existed when
// the one before was read. The list is built at most every publicQueueTTL and served from memory in
// between.
//
// SPDX-License-Identifier: Apache-2.0
package gateway

import (
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	publicTasksDefault = 100
	publicTasksMax     = 500
)

// publicTaskV1 is one task in GET /v1/tasks.
type publicTaskV1 struct {
	Project          string            `json:"project"`
	ID               string            `json:"id"`
	Kind             string            `json:"kind"`
	Role             *string           `json:"role"`
	State            string            `json:"state"`
	Outcome          *string           `json:"outcome"`
	Try              *int              `json:"try"`
	Parent           *string           `json:"parent"`
	Needs            []string          `json:"needs"`
	RetryOf          *string           `json:"retry_of"`
	Replaces         *string           `json:"replaces"`
	ClosedBy         *string           `json:"closed_by"`
	Labels           map[string]string `json:"labels"`
	HoleID           *string           `json:"hole_id"`
	Steps            *int              `json:"steps"`
	ModelMin         *float64          `json:"model_min"`
	Tokens           *int64            `json:"tokens"`
	PromptTokens     *int64            `json:"prompt_tokens"`
	CachedTokens     *int64            `json:"cached_tokens"`
	CompletionTokens *int64            `json:"completion_tokens"`
	// The explicit counts (audit 2026-11-17, tokens): known only for a result that reported
	// cached_tokens, else null. UncachedPromptTokens: prompt tokens the model computed;
	// TokensComputed: those plus completion_tokens (what the GPU did); TokensWithCache: also the
	// prompt tokens re-read from the cache.
	UncachedPromptTokens *int64      `json:"uncached_prompt_tokens"`
	TokensComputed       *int64      `json:"tokens_computed"`
	TokensWithCache      *int64      `json:"tokens_with_cache"`
	CreatedAt            time.Time   `json:"created_at"`
	StartedAt            *time.Time  `json:"started_at"`
	FinishedAt           *time.Time  `json:"finished_at"`
	UpdatedAt            time.Time   `json:"updated_at"`
	NodeClasses          []string    `json:"node_classes"`
	Live                 *publicLive `json:"live"`
}

func strOrNil(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// publicLabels are a task's labels as GET /v1/tasks shows them: its own, checked again, or for a
// task made before them, what its spec says on the public queue.
func (t *Task) publicLabels() map[string]string {
	out := map[string]string{}
	if t.typed() {
		for k, v := range t.Labels {
			if checkLabels(map[string]string{k: v}) == nil {
				out[k] = v
			}
		}
		return out
	}
	ref := readSpecRef(t.Spec)
	for k, v := range map[string]string{"trial": ref.Trial, "leaf": ref.Leaf, "group": ref.Group, "theorem": ref.Theorem} {
		if v != "" {
			out[k] = v
		}
	}
	if ref.Gen > 0 {
		out["outline"] = strconv.Itoa(ref.Gen)
	}
	if ref.Hole > 0 {
		out["hole"] = strconv.Itoa(ref.Hole)
	}
	return out
}

// publicV1 copies the public fields of a task (see the top of the file).
func (t *Task) publicV1() publicTaskV1 {
	p := publicTaskV1{Project: t.Project, ID: t.ID, Kind: keepPublic(t.Kind, taskKindRe), State: t.State,
		Parent: strOrNil(t.Parent), Needs: []string{}, RetryOf: strOrNil(t.RetryOf), Replaces: strOrNil(t.Replaces),
		ClosedBy: strOrNil(t.ClosedBy), Labels: t.publicLabels(), HoleID: strOrNil(t.HoleID),
		CreatedAt: t.CreatedAt, UpdatedAt: t.UpdatedAt, NodeClasses: []string{}, Live: t.publicLive()}
	if p.Kind == "" { // an older kind with a character today's pattern refuses
		p.Kind = kindOther
	}
	if role := t.ref().Role; role != "" {
		p.Role = &role
	}
	if t.Try > 0 {
		try := t.Try
		p.Try = &try
	}
	for _, id := range t.Needs {
		if pubTaskIDRe.MatchString(id) {
			p.Needs = append(p.Needs, id)
		}
	}
	for _, c := range t.NodeClasses {
		if c == nodeClassTrusted || c == nodeClassVolunteer || c == nodeClassPilot {
			p.NodeClasses = append(p.NodeClasses, c)
		}
	}
	switch {
	case t.StartedAt != nil:
		at := *t.StartedAt
		p.StartedAt = &at
	case t.Lease != nil: // a task from before started_at: its last lease
		at := t.Lease.Since
		p.StartedAt = &at
	}
	if r := t.Result; r != nil {
		out := "other"
		if pubOutcomeRe.MatchString(r.Outcome) {
			out = r.Outcome
		}
		p.Outcome = &out
		steps, mm := min(max(r.Steps, 0), 100000), min(max(r.ModelMin, 0), 1e6)
		prompt, compl := max(r.PromptTokens, 0), max(r.CompletionTokens, 0)
		tok := prompt + compl
		p.Steps, p.ModelMin, p.PromptTokens, p.CompletionTokens, p.Tokens = &steps, &mm, &prompt, &compl, &tok
		if r.CachedTokens != nil { // the result's prompt_tokens leave the cached ones out
			c := max(*r.CachedTokens, 0)
			computed, withCache := prompt+compl, prompt+c+compl
			p.CachedTokens, p.UncachedPromptTokens, p.TokensComputed, p.TokensWithCache = &c, &prompt, &computed, &withCache
		}
		fin := r.FinishedAt
		p.FinishedAt = &fin
	} else if finished(t.State) {
		fin := t.UpdatedAt
		p.FinishedAt = &fin
	}
	return p
}

// publicTasks returns the public view of the given projects' tasks, newest first. An unreadable
// queue is an error (a 503), not an empty one.
func (s *taskStore) publicTasks(projects []string) ([]publicTaskV1, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []publicTaskV1
	for _, project := range projects {
		p, err := s.load(project) // load, not open: a public read never sweeps, so never writes
		if err != nil {
			return nil, err
		}
		for _, t := range p.byID {
			if pubTaskIDRe.MatchString(t.ID) && t.Project == project {
				out = append(out, t.publicV1())
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return publicBefore(&out[i], &out[j]) })
	return out, nil
}

// publicBefore: a comes before b in GET /v1/tasks (newest first, then project, then id).
func publicBefore(a, b *publicTaskV1) bool {
	if !a.CreatedAt.Equal(b.CreatedAt) {
		return a.CreatedAt.After(b.CreatedAt)
	}
	if a.Project != b.Project {
		return a.Project < b.Project
	}
	return a.ID < b.ID
}

// publicCursor names a task's place in the order: "<unix nanoseconds>~<project>~<id>" ('~' is in
// no project or task id).
func publicCursor(t *publicTaskV1) string {
	return fmt.Sprintf("%d~%s~%s", t.CreatedAt.UnixNano(), t.Project, t.ID)
}

func parseCursor(c string) (*publicTaskV1, bool) {
	parts := strings.Split(c, "~")
	if len(parts) != 3 {
		return nil, false
	}
	ns, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil || !projectID.MatchString(parts[1]) || !pubTaskIDRe.MatchString(parts[2]) {
		return nil, false
	}
	return &publicTaskV1{CreatedAt: time.Unix(0, ns), Project: parts[1], ID: parts[2]}, true
}

// publicTaskList caches the built list of GET /v1/tasks.
type publicTaskList struct {
	mu   sync.Mutex
	at   time.Time
	key  string // the public projects it was built for
	list []publicTaskV1
}

// publicProjects are the projects whose tasks are public, in the config's order.
func (g *Gateway) publicProjects() []string {
	t := g.projects.Load()
	if t == nil {
		return nil
	}
	var out []string
	for _, id := range t.ids {
		if t.byID[id].PublicQueue {
			out = append(out, id)
		}
	}
	return out
}

func (g *Gateway) handlePublicTasks(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	projects, states, kinds, roles := splitList(q.Get("project")), splitList(q.Get("state")), splitList(q.Get("kind")), splitList(q.Get("role"))
	for _, s := range states {
		if !slices.Contains(queueStates, s) {
			apiError(w, http.StatusBadRequest, "state: "+strings.Join(queueStates, ", "))
			return
		}
	}
	for _, k := range append(slices.Clone(kinds), roles...) {
		if err := checkKind("kind and role", k); err != nil {
			apiError(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	for _, p := range projects {
		if !projectID.MatchString(p) {
			apiError(w, http.StatusBadRequest, "project: a project id")
			return
		}
	}
	limit := publicTasksDefault
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > publicTasksMax {
			apiError(w, http.StatusBadRequest, fmt.Sprintf("limit: 1 to %d", publicTasksMax))
			return
		}
		limit = n
	}
	var after *publicTaskV1
	if v := q.Get("after"); v != "" {
		var ok bool
		if after, ok = parseCursor(v); !ok {
			apiError(w, http.StatusBadRequest, "after: the next of the page before")
			return
		}
	}
	pub := g.publicProjects()
	if pub == nil {
		pub = []string{}
	}
	now := time.Now()
	key := strings.Join(pub, ",")
	c := &g.ptasks
	c.mu.Lock()
	if c.list == nil || c.key != key || now.Sub(c.at) >= publicQueueTTL || now.Before(c.at) {
		list, err := g.tasks.publicTasks(pub)
		if err != nil {
			c.mu.Unlock()
			taskError(w, err)
			return
		}
		if list == nil {
			list = []publicTaskV1{}
		}
		c.list, c.at, c.key = list, now, key
	}
	list, built := c.list, c.at
	c.mu.Unlock()
	page := []publicTaskV1{}
	var next *string
	for i := range list {
		t := &list[i]
		if (after != nil && !publicBefore(after, t)) || (len(projects) > 0 && !slices.Contains(projects, t.Project)) ||
			(len(states) > 0 && !slices.Contains(states, t.State)) || (len(kinds) > 0 && !slices.Contains(kinds, t.Kind)) ||
			(len(roles) > 0 && (t.Role == nil || !slices.Contains(roles, *t.Role))) {
			continue
		}
		if len(page) == limit {
			cur := publicCursor(&page[len(page)-1])
			next = &cur
			break
		}
		page = append(page, *t)
	}
	body, err := json.Marshal(map[string]any{"updated": built.UTC().Truncate(time.Second), "projects": pub, "tasks": page, "next": next})
	if err != nil {
		apiError(w, http.StatusInternalServerError, "could not build the view")
		return
	}
	h := w.Header()
	h.Set("Content-Type", "application/json")
	h.Set("Cache-Control", "public, max-age=15")
	h.Set("X-Content-Type-Options", "nosniff")
	w.Write(body)
}
