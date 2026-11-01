// The review loop's bookkeeping (tasks.go, "The review loop"; owner 2026-10-31): per chain, how many
// Qwen reviews and automatic rounds ran, what they cost, each verdict and why the loop stopped; and
// what happens when a candidate, a review or an automatic round ends. The cap on automatic rounds
// lives here, so a runner's restart or a new round id cannot get around it. Reviews and rounds are
// two switches (decision 0017 ended the reviews and kept the rounds): loopSetting, project.go.
//
// SPDX-License-Identifier: Apache-2.0
package gateway

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// taskChain is the record of one chain: an attempt and all its rounds. It outlives its tasks, which
// are dropped after the export. On disk it comes before the round it counts, so that no failed write
// or crash leaves a round the limit did not see.
type taskChain struct {
	ID        string `json:"id"`
	Project   string `json:"project"`
	Candidate string `json:"candidate,omitempty"` // the latest candidate
	Status    string `json:"status,omitempty"`    // a Stage, or operator-round
	Stop      string `json:"stop,omitempty"`      // why the loop stopped
	Reviews   int    `json:"reviews"`             // Qwen reviews finished
	// AutoRounds: rounds the loop scheduled after a failure, of either kind: the count the limit
	// reads. FreshBases: tasks it moved to a fresh base, which no model failed, with their own limit.
	// OperatorRounds: a person's. Rounds: each of them all, with its kind (a round a person created
	// follows no task).
	AutoRounds     int          `json:"auto_rounds"`
	FreshBases     int          `json:"fresh_bases"`
	OperatorRounds int          `json:"operator_rounds"`
	Rounds         []chainRound `json:"rounds,omitempty"`
	// What the reviews and the automatic rounds cost, apart.
	ReviewModelMin float64        `json:"review_model_min"`
	ReviewPrompt   int64          `json:"review_prompt_tokens"`
	ReviewCompl    int64          `json:"review_completion_tokens"`
	RoundModelMin  float64        `json:"round_model_min"`
	RoundPrompt    int64          `json:"round_prompt_tokens"`
	RoundCompl     int64          `json:"round_completion_tokens"`
	Verdicts       []chainVerdict `json:"verdicts,omitempty"`
	UpdatedAt      time.Time      `json:"updated_at"`
}

// The kinds of automatic round (chainRound.Kind).
const (
	roundQwen      = "qwen-review"  // after a red verdict
	roundChecks    = "local-checks" // after failed local checks
	roundFreshBase = "fresh-base"   // after a re-check skipped the task
	roundOperator  = "operator"     // a waiting recipe the operator continued
)

// chainOperatorRound: a chain's status while a person's round runs.
const chainOperatorRound = "operator-round"

type chainRound struct {
	Task   string    `json:"task"`
	Kind   string    `json:"kind"`
	From   string    `json:"from"` // the task it follows
	At     time.Time `json:"at"`
	Costed bool      `json:"costed,omitempty"` // its cost is in the chain's
}

type chainVerdict struct {
	Task     string    `json:"task"`
	Verdict  string    `json:"verdict"` // green, red, none
	Findings int       `json:"findings"`
	ModelMin float64   `json:"model_min"`
	Prompt   int64     `json:"prompt_tokens"`
	Compl    int64     `json:"completion_tokens"`
	Manual   bool      `json:"manual,omitempty"` // a person asked for it: it only records
	At       time.Time `json:"at"`
}

// chainOf is a task's chain: its id without the last "-<approach>...", so the sub-task: every
// approach and round of it, their follow-ups and reviews. Derived, so a new id cannot leave it.
func chainOf(id string) string {
	if i := strings.LastIndexByte(id, '-'); i > 0 {
		return id[:i]
	}
	return id
}

// chainFor is a task's chain: a review belongs to its candidate's, whatever its own id looks like.
func chainFor(id, parent string) string {
	if parent != "" {
		return chainOf(parent)
	}
	return chainOf(id)
}

func (s *taskStore) chainDir(project string) string {
	return filepath.Join(s.dir, "task-chains-"+project)
}

// loadChains reads a project's chain records. Holds s.mu.
func (s *taskStore) loadChains(project string, p *projectTasks) error {
	ents, err := os.ReadDir(s.chainDir(project))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, e := range ents {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".json") || strings.HasPrefix(name, ".") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(s.chainDir(project), name))
		if err != nil {
			return err
		}
		var c taskChain
		if err = json.Unmarshal(raw, &c); err != nil || c.ID+".json" != name {
			s.log.Warn("chain file unreadable, ignoring it", "project", project, "file", name, "err", err)
			continue
		}
		p.chains[c.ID] = &c
	}
	return nil
}

// chain returns a chain's record, made on first use. Holds s.mu.
func (s *taskStore) chain(p *projectTasks, id string, now time.Time) *taskChain {
	c, ok := p.chains[id]
	if !ok {
		c = &taskChain{ID: id}
		p.chains[id] = c
	}
	c.UpdatedAt = now
	return c
}

// saveChain writes one chain's record. On an error the caller stops, and what it was scheduling
// waits: the limit reads this count. Holds s.mu.
func (s *taskStore) saveChain(project string, c *taskChain) error {
	c.Project = project
	_, err := s.put(project, s.chainDir(project), c.ID, c)
	return err
}

// countPersonsRound records a round a person creates, by its id, so that a create repeated after a
// failed write does not count it twice. Holds s.mu.
func (s *taskStore) countPersonsRound(p *projectTasks, t *Task, now time.Time) error {
	c := s.chain(p, t.Chain, now)
	if !c.hasRound(t.ID, "", roundOperator) {
		c.OperatorRounds++
		c.Rounds = append(c.Rounds, chainRound{Task: t.ID, Kind: roundOperator, At: now.UTC()})
	}
	c.Status, c.Stop = chainOperatorRound, ""
	return s.saveChain(t.Project, c)
}

// What a finished task still owes the loop (loopPending.Step).
const (
	owedDone     = "done"     // it completed: afterDone
	owedSkipped  = "skipped"  // a re-check skipped it: skipped
	owedStop     = "stop"     // it ended without a result: loopStopped
	owedContinue = "continue" // the operator asked for the round waiting on it
)

// loopPending is saved with a task's end, so that a crash before the loop has acted on it does not
// lose the step, or the recipe, which is nowhere else.
type loopPending struct {
	Step string   `json:"step"`
	Why  string   `json:"why,omitempty"` // owedStop
	Fix  *taskNew `json:"fix,omitempty"`
}

// settle does what t owes the loop, then clears the debt. After a crash or a failed write it runs
// again, over whatever part reached the disk: each step below notices what is there already, so
// nothing is counted or made twice. It stops at a failed write. Holds s.mu.
func (s *taskStore) settle(p *projectTasks, t *Task, now time.Time) error {
	lp := t.LoopPending
	if lp == nil {
		return nil
	}
	var err error
	switch lp.Step {
	case owedDone:
		err = s.afterDone(p, t, lp.Fix, now)
	case owedSkipped:
		err = s.skipped(p, t, lp.Fix, now)
	case owedStop:
		err = s.loopStopped(p, t, lp.Why, now)
	case owedContinue:
		err = s.continueRound(p, t, lp.Fix, now)
	default:
		s.log.Warn("unknown loop step, dropping it", "project", t.Project, "task", t.ID, "step", lp.Step)
	}
	if err != nil {
		return err
	}
	t.LoopPending = nil
	return s.save(t)
}

// settleAll finishes what a crash or a failed write left, oldest first, before anyone reads the
// queue, and stops at a failed write. Holds s.mu.
func (s *taskStore) settleAll(p *projectTasks, now time.Time) {
	var owing []*Task
	for _, t := range p.byID {
		if t.LoopPending != nil {
			owing = append(owing, t)
		}
	}
	slices.SortFunc(owing, func(a, b *Task) int { return int(a.Seq - b.Seq) })
	for _, t := range owing {
		s.log.Info("finishing the loop's step", "project", t.Project, "task", t.ID, "step", t.LoopPending.Step)
		if s.settle(p, t, now) != nil {
			return
		}
	}
}

// setStage moves a candidate in the loop; like a review, it needs another export. Holds s.mu.
func (s *taskStore) setStage(t *Task, stage, note string, now time.Time) error {
	t.Stage, t.ExportedAt = stage, nil
	t.event(now, "stage", "", strings.TrimSpace(stage+" "+note))
	return s.save(t)
}

// afterDone is what a finished task means for the loop. Holds s.mu.
func (s *taskStore) afterDone(p *projectTasks, t *Task, fix *taskNew, now time.Time) error {
	loop := s.loop(t.Project)
	r := t.Result
	if t.Kind == "review" {
		return s.reviewDone(p, t, fix, loop, now)
	}
	passed := r.ChecksPassed != nil && *r.ChecksPassed
	if t.CreatedBy == gatewayCreator {
		c := s.chain(p, t.Chain, now)
		if c.costRound(t.ID) {
			c.RoundModelMin += r.ModelMin
			c.RoundPrompt += r.PromptTokens
			c.RoundCompl += r.CompletionTokens
			if err := s.saveChain(t.Project, c); err != nil {
				return err
			}
		}
	}
	if !loop.rounds || t.Kind == "other" {
		return nil
	}
	switch {
	case !passed:
		return s.scheduleRound(p, t, t, fix, roundChecks, loop.limit, now)
	case loop.reviews:
		return s.queueReview(p, t, now)
	}
	return s.candidateReady(p, t, stopPassed, now)
}

// Why a chain stopped at ready-for-operator.
const (
	stopPassed      = "passed"
	stopReviewEnded = "passed its local checks; reviews are off"
)

// reviewsOff is why the sweep ends the loop's open reviews once a project has none.
const reviewsOff = "reviews are off for this project"

// candidateReady: a candidate waits for the operator. Holds s.mu.
func (s *taskStore) candidateReady(p *projectTasks, t *Task, stop string, now time.Time) error {
	c := s.chain(p, t.Chain, now)
	c.Candidate, c.Status, c.Stop = t.ID, StageReady, stop
	if err := s.saveChain(t.Project, c); err != nil {
		return err
	}
	return s.setStage(t, StageReady, "", now)
}

// reviewEnds: an open review of the loop in a project with reviews off. The sweep ends it; one still
// here after a sweep could not be saved as ended, and has no debt on disk to say so (owes).
func (s *taskStore) reviewEnds(t *Task) bool {
	return t.Kind == "review" && t.CreatedBy == gatewayCreator && !finished(t.State) && !s.loop(t.Project).reviews
}

// endReviews: reviews went off, so nothing waits for one. The open reviews end first, each with its
// debt saved: a claim cannot lease one while a candidate's write waits for the disk. A failed end
// leaves the review as it was and p stale; the next sweep tries again. Holds s.mu.
func (s *taskStore) endReviews(p *projectTasks, now time.Time) {
	for _, t := range p.byID {
		if p.stale {
			return
		}
		if s.reviewEnds(t) {
			s.endTask(p, t, reviewsOff, now)
		}
	}
	for _, t := range p.byID {
		if p.stale {
			return
		}
		if t.Stage == StageInReview && !s.loop(t.Project).reviews {
			s.candidateFreed(p, t.ID, now)
		}
	}
}

// candidateFreed: reviews went off with this candidate waiting for one, which will not come or
// schedules nothing: it passed its checks, so it goes to the operator. Only from in-review: a
// candidate the loop moved on stays where it is. A chain a person's round moved on keeps its status
// and candidate; a chain that says ready already is a crash between the two writes. Holds s.mu.
func (s *taskStore) candidateFreed(p *projectTasks, id string, now time.Time) error {
	t, ok := p.byID[id]
	if !ok || t.Stage != StageInReview {
		return nil
	}
	s.log.Info("candidate no longer waits for a review", "project", t.Project, "task", id)
	if c := s.chain(p, t.Chain, now); c.Status != StageInReview || c.Candidate != t.ID {
		return s.setStage(t, StageReady, stopReviewEnded, now)
	}
	return s.candidateReady(p, t, stopReviewEnded, now)
}

// costRound: t's cost is not in the chain yet; it is from now on.
func (c *taskChain) costRound(id string) bool {
	for i := range c.Rounds {
		if c.Rounds[i].Task == id {
			first := !c.Rounds[i].Costed
			c.Rounds[i].Costed = true
			return first
		}
	}
	return true
}

// hasRound: this round was counted, after `from`: a crash stopped it there. A recipe that merely
// reuses an earlier round's id follows another task.
func (c *taskChain) hasRound(id, from, kind string) bool {
	return slices.ContainsFunc(c.Rounds, func(r chainRound) bool { return r.Task == id && r.From == from && r.Kind == kind })
}

// skipped: a re-check skipped a task. No model failed; the runner's recipe moves it to a fresh base,
// and without one the operator decides. Holds s.mu.
func (s *taskStore) skipped(p *projectTasks, t *Task, fix *taskNew, now time.Time) error {
	loop := s.loop(t.Project)
	switch {
	case !loop.rounds:
		return s.loopStopped(p, t, t.Error, now)
	case fix == nil:
		return s.stopChain(p, t, t.Error, now)
	default:
		return s.scheduleRound(p, t, t, fix, roundFreshBase, loop.limit, now)
	}
}

// stopChain ends the loop at t: the operator takes over. Holds s.mu.
func (s *taskStore) stopChain(p *projectTasks, t *Task, why string, now time.Time) error {
	c := s.chain(p, t.Chain, now)
	c.Status, c.Stop = StageNeedsOperator, why
	if err := s.saveChain(t.Project, c); err != nil {
		return err
	}
	if err := s.setStage(t, StageNeedsOperator, why, now); err != nil {
		return err
	}
	s.log.Info("review loop stopped", "project", t.Project, "task", t.ID, "why", why)
	return nil
}

// loopStopped: a loop's task ended without a result to act on. Only tasks of the loop stop it:
// a person's task failing is a person's to see. Holds s.mu.
func (s *taskStore) loopStopped(p *projectTasks, t *Task, why string, now time.Time) error {
	if t.CreatedBy != gatewayCreator {
		return nil
	}
	if t.Kind == "review" && !s.loop(t.Project).reviews {
		return s.candidateFreed(p, t.Parent, now)
	}
	if parent, ok := p.byID[t.Parent]; ok && t.Kind == "review" && parent.State == TaskDone {
		return s.stopChain(p, parent, "the review did not finish: "+why, now)
	}
	return s.stopChain(p, t, "the automatic task did not finish: "+why, now)
}

// queueReview creates a candidate's review task. Holds s.mu. After a crash it may run again over the
// loop's own review: queued or running, the rest of the step is written; ended, that end moved the
// candidate on. Any other task under the review's id means no review will come: the operator decides.
func (s *taskStore) queueReview(p *projectTasks, t *Task, now time.Time) error {
	id := t.ID + reviewSuffix
	if err := checkID("id", id); err != nil {
		return s.stopChain(p, t, "the review task's id is too long", now)
	}
	rv, ok := p.byID[id]
	if ok && (rv.CreatedBy != gatewayCreator || rv.Kind != "review") {
		return s.stopChain(p, t, "the review's id "+id+" is taken by another task", now)
	}
	if ok && finished(rv.State) {
		return nil
	}
	if !ok {
		var spec map[string]json.RawMessage
		if len(t.Spec) == 0 || json.Unmarshal(t.Spec, &spec) != nil || spec == nil {
			spec = map[string]json.RawMessage{}
		}
		spec["parent"], _ = json.Marshal(t.ID)
		if _, ok := spec["attempt"]; !ok {
			spec["attempt"], _ = json.Marshal(t.ID)
		}
		raw, _ := json.Marshal(spec)
		p.seq++
		rv = &Task{ID: id, Project: t.Project, Kind: "review", Class: classReview, Title: "Qwen review of " + t.ID, Spec: raw,
			BaseRef: t.BaseRef, PR: t.PR, CreatedBy: gatewayCreator, State: TaskQueued, MaxTries: defaultMaxTries, Seq: p.seq,
			CreatedAt: now, Parent: t.ID, Chain: t.Chain}
		rv.event(now, "created", "", gatewayCreator)
		p.byID[id] = rv
		if err := s.save(rv); err != nil {
			return err
		}
	}
	c := s.chain(p, t.Chain, now)
	c.Candidate, c.Status, c.Stop = t.ID, StageInReview, ""
	if err := s.saveChain(t.Project, c); err != nil {
		return err
	}
	if err := s.setStage(t, StageInReview, "", now); err != nil {
		return err
	}
	s.log.Info("review queued", "project", t.Project, "task", id)
	return nil
}

// reviewDone records a finished review and, for the loop's own, acts on the verdict. Holds s.mu.
func (s *taskStore) reviewDone(p *projectTasks, t *Task, fix *taskNew, loop loopSetting, now time.Time) error {
	r, c := t.Result, s.chain(p, t.Chain, now)
	v := chainVerdict{Task: t.ID, Verdict: "none", ModelMin: r.ModelMin, Prompt: r.PromptTokens, Compl: r.CompletionTokens,
		Manual: t.CreatedBy != gatewayCreator, At: now.UTC()}
	if t.Review != nil {
		v.Verdict = t.Review.Verdict
		if t.Review.Findings != nil {
			v.Findings = *t.Review.Findings
		}
	}
	// The newest verdict is never the one trimmed, so this finds a review that was counted.
	if !slices.ContainsFunc(c.Verdicts, func(x chainVerdict) bool { return x.Task == t.ID }) {
		c.Reviews++
		c.ReviewModelMin += r.ModelMin
		c.ReviewPrompt += r.PromptTokens
		c.ReviewCompl += r.CompletionTokens
		c.Verdicts = append(c.Verdicts, v)
		if n := len(c.Verdicts) - maxChainVerdicts; n > 0 {
			c.Verdicts = c.Verdicts[n:]
		}
		if err := s.saveChain(t.Project, c); err != nil {
			return err
		}
	}
	parent, ok := p.byID[t.Parent]
	if ok && parent.State == TaskDone && t.Review != nil && !sameVerdict(parent.QwenReview, t.Review) {
		q := *t.Review
		parent.QwenReview, parent.ExportedAt = &q, nil
		parent.event(now, "qwen reviewed", "", q.Verdict+" by "+q.By)
		if err := s.save(parent); err != nil {
			return err
		}
	}
	if v.Manual { // it records; it schedules nothing
		return nil
	}
	if !loop.reviews { // nor does a verdict that comes after reviews went off, but for a round a crash cut short
		if ok && fix != nil && c.hasRound(fix.ID, parent.ID, roundQwen) {
			return s.scheduleRound(p, t, parent, fix, roundQwen, loop.limit, now)
		}
		return s.candidateFreed(p, t.Parent, now)
	}
	if !ok || parent.State != TaskDone {
		return s.stopChain(p, t, "the candidate's task is gone", now)
	}
	switch v.Verdict {
	case "green":
		return s.candidateReady(p, parent, stopPassed, now)
	case "red":
		return s.scheduleRound(p, t, parent, fix, roundQwen, loop.limit, now)
	default:
		return s.stopChain(p, parent, "the review gave no verdict", now)
	}
}

func sameVerdict(a, b *TaskReview) bool {
	return a != nil && a.Verdict == b.Verdict && a.By == b.By && a.At.Equal(b.At)
}

// scheduleRound makes the next automatic round after `from`, unless the chain has used its rounds:
// the limit covers failed reviews and failed checks together, and fresh bases apart, which no model
// failed but which must end too. `origin` keeps the recipe for the operator. Holds s.mu.
func (s *taskStore) scheduleRound(p *projectTasks, origin, from *Task, fix *taskNew, kind string, limit int, now time.Time) error {
	c := s.chain(p, from.Chain, now)
	stop := func(why string) error {
		if fix != nil {
			origin.NextFix = fix
			if err := s.save(origin); err != nil {
				return err
			}
		}
		return s.stopChain(p, from, why, now)
	}
	if fix == nil || !c.hasRound(fix.ID, from.ID, kind) {
		used := c.AutoRounds
		if kind == roundFreshBase {
			used = c.FreshBases
		}
		if used >= limit {
			return stop(fmt.Sprintf("the limit of %d automatic rounds (%s)", limit, kind))
		}
		if fix == nil {
			return stop("the runner gave no round after " + kind)
		}
		if err := checkRecipe(fix, from, kind); err != nil {
			fix = nil
			return stop("the runner's round was refused: " + err.Error())
		}
		if _, taken := p.byID[fix.ID]; taken {
			return stop("the round's id " + fix.ID + " is taken")
		}
	}
	spec, err := canonicalJSON(fix.Spec)
	if err != nil {
		return stop("the round's spec does not parse")
	}
	if err := s.makeRound(p, c, from, fix, spec, kind, now); err != nil {
		return err
	}
	s.log.Info("round scheduled", "project", from.Project, "task", fix.ID, "kind", kind, "rounds", c.AutoRounds, "limit", limit)
	return nil
}

// makeRound counts a round after `from` and queues it; run again after a crash, it does the part
// that is missing. An operator's round is a person's task: it stays outside the automatic limit and
// the automatic cost, as a fix round created by hand. Holds s.mu.
func (s *taskStore) makeRound(p *projectTasks, c *taskChain, from *Task, fix *taskNew, spec json.RawMessage, kind string, now time.Time) error {
	creator, status := gatewayCreator, StageFixScheduled
	if kind == roundOperator {
		creator, status = fix.CreatedBy, chainOperatorRound
	}
	if !c.hasRound(fix.ID, from.ID, kind) {
		switch kind {
		case roundFreshBase:
			c.FreshBases++
		case roundOperator:
			c.OperatorRounds++
		default:
			c.AutoRounds++
		}
		c.Rounds = append(c.Rounds, chainRound{Task: fix.ID, Kind: kind, From: from.ID, At: now.UTC()})
	}
	c.Candidate, c.Status, c.Stop = from.ID, status, ""
	if err := s.saveChain(from.Project, c); err != nil {
		return err
	}
	if _, ok := p.byID[fix.ID]; !ok {
		maxTries := fix.MaxTries
		if maxTries == 0 {
			maxTries = defaultMaxTries
		}
		p.seq++
		n := &Task{ID: fix.ID, Project: from.Project, Kind: fix.Kind, Class: taskClass(fix.Kind, fix.FindingsSource), Title: fix.Title,
			Spec: spec, BaseRef: fix.BaseRef, PR: fix.PR, CreatedBy: creator, State: TaskQueued, MaxTries: maxTries, Seq: p.seq,
			CreatedAt: now, FindingsSource: fix.FindingsSource, Chain: from.Chain, RoundKind: kind}
		n.event(now, "created", "", kind+" after "+from.ID)
		p.byID[n.ID] = n
		if err := s.save(n); err != nil {
			return err
		}
	}
	return s.setStage(from, StageFixScheduled, fix.ID, now)
}

// resume schedules the round waiting on a finished task, as the operator's: the gateway keeps the
// recipe, so its size does not matter here. A repeated call gives the same round (created false).
// With no round to show yet (a failed write), the answer is 503 though the request is saved: the
// round comes by itself, and a repeated call finds it.
func (s *taskStore) resume(project, id, by, note string) (*Task, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, now, err := s.open(project)
	if err != nil {
		return nil, false, err
	}
	roundWaits := terr(http.StatusServiceUnavailable, "the round is saved and waits for the task queue to write it; ask again shortly")
	t, ok := p.byID[id]
	switch {
	case !ok:
		return nil, false, terr(http.StatusNotFound, "no task %s", id)
	case t.ContinuedAs != "":
		if t.LoopPending != nil {
			return nil, false, roundWaits
		}
		if n, ok := p.byID[t.ContinuedAs]; ok {
			return n.view(false), false, nil
		}
		return nil, false, terr(http.StatusConflict, "task %s went on as %s, which is gone", id, t.ContinuedAs)
	case t.NextFix == nil:
		return nil, false, terr(http.StatusConflict, "no round waits on task %s", id)
	}
	fix := *t.NextFix
	fix.CreatedBy = by
	kind := roundChecks // the class it must keep; a recipe past the limit was never checked
	if t.Kind == "review" {
		kind = roundQwen
	}
	if err := checkRecipe(&fix, t, kind); err != nil {
		return nil, false, terr(http.StatusConflict, "the waiting round is refused: %v", err)
	}
	if _, taken := p.byID[fix.ID]; taken {
		return nil, false, terr(http.StatusConflict, "the round's id %s is taken", fix.ID)
	}
	spec, err := canonicalJSON(fix.Spec)
	if err == nil && note != "" {
		spec, err = withNote(spec, note)
	}
	if err != nil {
		return nil, false, terr(http.StatusConflict, "the waiting round's spec: %v", err)
	}
	fix.Spec = spec
	if err := admit(p); err != nil {
		return nil, false, err
	}
	t.ContinuedAs, t.LoopPending = fix.ID, &loopPending{Step: owedContinue, Fix: &fix}
	if err := s.conclude(p, t, now); err != nil {
		return nil, false, err
	}
	if p.stale {
		return nil, false, roundWaits
	}
	s.prune(p)
	s.log.Info("waiting round continued", "project", project, "task", id, "round", fix.ID, "by", by)
	return p.byID[fix.ID].view(false), true, nil
}

// continueRound makes the waiting round, after the candidate when it is still here. Holds s.mu.
func (s *taskStore) continueRound(p *projectTasks, t *Task, fix *taskNew, now time.Time) error {
	from := t
	if parent, ok := p.byID[t.Parent]; ok {
		from = parent
	}
	if err := s.makeRound(p, s.chain(p, t.Chain, now), from, fix, fix.Spec, roundOperator, now); err != nil {
		return err
	}
	t.NextFix = nil
	t.event(now, "continued", "", fix.ID+" by "+fix.CreatedBy)
	return nil
}

// withNote puts the operator's note in the round's fix-round ledger entry, the one place of a spec
// the gateway reads: the harness takes `operator_note` from there, and the spec is not sent again. A
// round with no such entry (a first attempt's retry) has nowhere to show it, so it is refused rather
// than lost.
func withNote(raw json.RawMessage, note string) (json.RawMessage, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var spec map[string]any
	if err := dec.Decode(&spec); err != nil {
		return nil, err
	}
	ledger, _ := spec["ledger"].(map[string]any)
	sub, _ := ledger["subtask"].(map[string]any)
	rounds, _ := sub["fix_rounds"].(map[string]any)
	n := 0
	for _, v := range rounds {
		if fx, ok := v.(map[string]any); ok {
			fx["operator_note"] = note
			n++
		}
	}
	if n == 0 {
		return nil, errors.New("this round has no fix-round entry to carry it")
	}
	return json.Marshal(spec)
}

// checkRecipe: a round from a runner stays in its chain. After a red verdict it answers Qwen's
// findings, at that class; a retry or a fresh base keeps the class of the task it follows.
func checkRecipe(n *taskNew, from *Task, kind string) error {
	errs := []error{checkID("id", n.ID), checkText("title", n.Title, 200)}
	if chainOf(n.ID) != from.Chain {
		errs = append(errs, errors.New("it leaves the chain"))
	}
	switch {
	case n.Kind != "attempt" && n.Kind != "fix_round":
		errs = append(errs, errors.New("kind: attempt or fix_round"))
	case kind == roundQwen && (n.Kind != "fix_round" || n.FindingsSource != findingsPrereviewQwen):
		errs = append(errs, errors.New("a fix_round answering prereview-qwen"))
	case kind != roundQwen && taskClass(n.Kind, n.FindingsSource) != from.Class:
		errs = append(errs, fmt.Errorf("not its task's class %d", from.Class))
	}
	if err := checkObject("spec", n.Spec, maxTaskRecipeSpec); err != nil {
		errs = append(errs, err)
	}
	if n.BaseRef != "" && !taskRefRe.MatchString(n.BaseRef) {
		errs = append(errs, errors.New("base_ref"))
	}
	if n.PR < 0 || n.PR > 999999999 || n.MaxTries < 0 || n.MaxTries > maxMaxTries {
		errs = append(errs, errors.New("pr or max_tries"))
	}
	return errors.Join(errs...)
}
