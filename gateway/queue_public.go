// The public view of a project's task queue (owner, 2026-11-13: the prove2me page lagged behind its
// trial because it showed a list made when the site was built). GET /v1/projects/{id}/queue, without
// a key, for a project with public_queue on; any other project answers 404.
//
// The tasks themselves stay private (tasks.go: specs, prompts, diffs, workers, leases and errors are
// for the project key and the admins). The view copies only fields that are public anyway, each
// checked against a strict pattern and left out when it does not match:
//
//   - the task's id (t5-F01-h03, as the trial's pages name it), its state, when it was made and last
//     changed, and when its lease began while it runs;
//   - from the result: a short outcome word ("proved", "outline"), the token counts and model minutes;
//   - from the spec: the trial, the leaf and its group, the theorem's name, and for a hole task its
//     role, outline generation and hole number (prove2me's scaffold v5).
//
// Never in it: the spec beyond those fields, the title, the worker, the lease id, the error text, the
// events, reviews, decisions and the raw evidence's location. The answer is built at most every
// publicQueueTTL per project and served from memory in between, so a burst of page loads costs one
// pass over the queue.
//
// SPDX-License-Identifier: Apache-2.0
package gateway

import (
	"encoding/json"
	"net/http"
	"regexp"
	"sort"
	"sync"
	"time"
)

const (
	// publicQueueTTL: how long a built view is served before the next request builds it again.
	publicQueueTTL = 15 * time.Second
	// publicQueueFinished: at most this many finished tasks, newest first (all open ones are listed).
	publicQueueFinished = 200
)

var (
	pubTrialRe   = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{0,15}$`)
	pubLeafRe    = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,31}$`)
	pubGroupRe   = regexp.MustCompile(`^[a-z0-9-]{1,32}$`)
	pubRoleRe    = regexp.MustCompile(`^[a-z]{1,16}$`)
	pubOutcomeRe = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,31}$`)
	// A Lean declaration name: letters (any script), digits, subscripts and _ . ' ! ? -.
	pubTheoremRe = regexp.MustCompile(`^[\p{L}\p{N}_][\p{L}\p{N}_.'!?\x{2080}-\x{2089}-]{0,199}$`)
	pubTaskIDRe  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,99}$`)
)

// publicTask is one task in the public view.
type publicTask struct {
	ID        string     `json:"id"`
	State     string     `json:"state"`
	CreatedAt time.Time  `json:"created_at"`
	UpdatedAt time.Time  `json:"updated_at"`
	Since     *time.Time `json:"since,omitempty"` // a running task's lease began
	// From the spec.
	Trial   string `json:"trial,omitempty"`
	Leaf    string `json:"leaf,omitempty"`
	Group   string `json:"group,omitempty"`
	Theorem string `json:"theorem,omitempty"`
	Role    string `json:"role,omitempty"`
	Gen     int    `json:"gen,omitempty"`
	Hole    int    `json:"hole,omitempty"` // a hole task's hole, from 1
	// From the result, once there is one.
	Outcome          string  `json:"outcome,omitempty"`
	PromptTokens     int64   `json:"prompt_tokens,omitempty"`
	CompletionTokens int64   `json:"completion_tokens,omitempty"`
	ModelMin         float64 `json:"model_min,omitempty"`
}

// specRef is what the public view and the account page read from a spec (prove2me's fields).
type specRef struct {
	Trial, Leaf, Group, Theorem, Role string
	Gen, Hole                         int
}

// readSpecRef reads the public fields of a task's spec; a field that does not match its pattern is
// left empty, a spec that is not an object gives nothing.
func readSpecRef(spec json.RawMessage) specRef {
	var s struct {
		Trial   string `json:"trial"`
		Leaf    string `json:"leaf"`
		Group   string `json:"group"`
		Theorem string `json:"theorem"`
		V5      *struct {
			Role  string `json:"role"`
			Gen   int    `json:"gen"`
			Index *int   `json:"index"`
		} `json:"v5"`
	}
	if len(spec) == 0 || json.Unmarshal(spec, &s) != nil {
		return specRef{}
	}
	keep := func(v string, re *regexp.Regexp) string {
		if re.MatchString(v) {
			return v
		}
		return ""
	}
	r := specRef{Trial: keep(s.Trial, pubTrialRe), Leaf: keep(s.Leaf, pubLeafRe), Group: keep(s.Group, pubGroupRe),
		Theorem: keep(s.Theorem, pubTheoremRe)}
	if v := s.V5; v != nil {
		r.Role = keep(v.Role, pubRoleRe)
		if v.Gen > 0 && v.Gen < 1000 {
			r.Gen = v.Gen
		}
		if v.Index != nil && *v.Index >= 0 && *v.Index < 1000 {
			r.Hole = *v.Index + 1
		}
	}
	return r
}

// public copies the public fields of a task (see the top of the file).
func (t *Task) public() publicTask {
	ref := readSpecRef(t.Spec)
	p := publicTask{ID: t.ID, State: t.State, CreatedAt: t.CreatedAt, UpdatedAt: t.UpdatedAt, Trial: ref.Trial, Leaf: ref.Leaf,
		Group: ref.Group, Theorem: ref.Theorem, Role: ref.Role, Gen: ref.Gen, Hole: ref.Hole}
	if t.State == TaskLeased && t.Lease != nil {
		since := t.Lease.Since
		p.Since = &since
	}
	if r := t.Result; r != nil {
		if pubOutcomeRe.MatchString(r.Outcome) {
			p.Outcome = r.Outcome
		} else if r.Outcome != "" {
			p.Outcome = "other"
		}
		p.PromptTokens, p.CompletionTokens = max(r.PromptTokens, 0), max(r.CompletionTokens, 0)
		if r.ModelMin > 0 && r.ModelMin < 1e6 {
			p.ModelMin = r.ModelMin
		}
	}
	return p
}

// publicList returns the public view of a project's queue: the open tasks (running ones first, by
// when they began, then the queued ones in claim order), then up to publicQueueFinished finished ones,
// newest first; and the count of tasks per state. A task whose id does not match pubTaskIDRe is
// counted but not listed. An unreadable queue is an error (a 503), not an empty one.
func (s *taskStore) publicList(project string) ([]publicTask, map[string]int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	counts := map[string]int{}
	// load, not open: a public read never sweeps (and so never writes); the sweep runs every
	// taskSweepEvery anyway.
	p, err := s.load(project)
	if err != nil {
		return nil, nil, err
	}
	var queued, leased, fin []*Task
	for _, t := range p.byID {
		counts[t.State]++
		if !pubTaskIDRe.MatchString(t.ID) {
			continue
		}
		switch {
		case t.State == TaskQueued:
			queued = append(queued, t)
		case t.State == TaskLeased && t.Lease != nil:
			leased = append(leased, t)
		case finished(t.State):
			fin = append(fin, t)
		}
	}
	claimOrder(queued)
	sort.Slice(leased, func(i, j int) bool { return leased[i].Lease.Since.Before(leased[j].Lease.Since) })
	sort.Slice(fin, func(i, j int) bool { return fin[i].UpdatedAt.After(fin[j].UpdatedAt) })
	out := make([]publicTask, 0, len(leased)+len(queued)+min(len(fin), publicQueueFinished))
	for _, t := range leased {
		out = append(out, t.public())
	}
	for _, t := range queued {
		out = append(out, t.public())
	}
	for _, t := range fin[:min(len(fin), publicQueueFinished)] {
		out = append(out, t.public())
	}
	return out, counts, nil
}

// leasedRefs returns the public reference of each running task of a project, by when it began.
func (s *taskStore) leasedRefs(project string) []publicTask {
	ts, _, _ := s.publicList(project)
	var out []publicTask
	for _, t := range ts {
		if t.State == TaskLeased {
			out = append(out, t)
		}
	}
	return out
}

// publicQueues caches each project's built view.
type publicQueues struct {
	mu   sync.Mutex
	byID map[string]publicQueueBuilt
}

type publicQueueBuilt struct {
	at   time.Time
	body []byte
}

func (g *Gateway) handlePublicQueue(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	t := g.projects.Load()
	if t == nil || t.byID[id] == nil || !t.byID[id].PublicQueue {
		apiError(w, http.StatusNotFound, "no public queue for this project")
		return
	}
	now := time.Now()
	// One build at a time: concurrent requests wait for it and share it. The lock covers only the
	// build, never the write: a client that reads slowly must not hold every other reader up.
	g.pubq.mu.Lock()
	b, ok := g.pubq.byID[id]
	if !ok || now.Sub(b.at) >= publicQueueTTL || now.Before(b.at) {
		tasks, counts, err := g.tasks.publicList(id)
		if err != nil {
			g.pubq.mu.Unlock()
			taskError(w, err) // an unreadable queue is a 503, never an empty one
			return
		}
		for _, s := range queueStates { // every state, 0 included, so a reader need not guess
			counts[s] += 0
		}
		body, err := json.Marshal(map[string]any{"project": id, "updated": now.UTC().Truncate(time.Second),
			"counts": counts, "tasks": tasks})
		if err != nil {
			g.pubq.mu.Unlock()
			apiError(w, http.StatusInternalServerError, "could not build the view")
			return
		}
		b = publicQueueBuilt{now, body}
		if g.pubq.byID == nil {
			g.pubq.byID = map[string]publicQueueBuilt{}
		}
		g.pubq.byID[id] = b
	}
	g.pubq.mu.Unlock()
	h := w.Header()
	h.Set("Content-Type", "application/json")
	h.Set("Cache-Control", "public, max-age=15")
	h.Set("X-Content-Type-Options", "nosniff")
	w.Write(b.body)
}
