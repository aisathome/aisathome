// SPDX-License-Identifier: Apache-2.0
package gateway

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// digest is what a crash must not change: every task's place in the loop and the chains' records.
func (r *taskRig) digest() string {
	r.t.Helper()
	r.g.tasks.mu.Lock()
	defer r.g.tasks.mu.Unlock()
	p, _, err := r.g.tasks.open("tauceti")
	if err != nil {
		r.t.Fatal(err)
	}
	var out []string
	for _, t := range p.byID {
		qwen, next := "", ""
		if t.QwenReview != nil {
			qwen = t.QwenReview.Verdict
		}
		if t.NextFix != nil {
			next = t.NextFix.ID
		}
		out = append(out, fmt.Sprintf("task %s %s kind=%s stage=%q chain=%s by=%s round=%q next=%q cont=%q qwen=%q owed=%v", t.ID, t.State, t.Kind,
			t.Stage, t.Chain, t.CreatedBy, t.RoundKind, next, t.ContinuedAs, qwen, t.LoopPending != nil))
	}
	for _, c := range p.chains {
		raw, _ := json.Marshal(c)
		out = append(out, "chain "+string(raw))
	}
	slices.Sort(out)
	return strings.Join(out, "\n")
}

func taskFile(dir, id string) string { return filepath.Join(dir, "tasks-tauceti", id+".json") }

func red(fix map[string]any) map[string]any {
	res := map[string]any{"outcome": "qwen review: red", "model_min": 12.5, "prompt_tokens": 1000, "completion_tokens": 300,
		"review": map[string]any{"verdict": "red", "by": "qwen-judge", "judge": "qwen", "findings": 2}}
	if fix != nil {
		res["fix"] = fix
	}
	return res
}

// loopEnd is one way a task ends: setup brings a rig to the moment before, the step it returns does
// the call and gives its HTTP status.
type loopEnd struct {
	name  string
	limit int
	setup func(r *taskRig) func(r *taskRig) int
}

func loopEnds() []loopEnd {
	type step = func(r *taskRig) int
	done := func(id, lease string, body map[string]any) step {
		return func(r *taskRig) int { return r.lease(id, "complete", "w1", lease, map[string]any{"result": body}).Code }
	}
	passed := map[string]any{"outcome": "passes", "checks_passed": true, "model_min": 30.0, "prompt_tokens": 900, "completion_tokens": 90}
	return []loopEnd{
		{"a candidate passes", 3, func(r *taskRig) step {
			r.create("T1.a-A", "attempt")
			return done("T1.a-A", r.claimID("w1", "T1.a-A"), passed)
		}},
		{"the review id is taken", 3, func(r *taskRig) step {
			r.createReview("T1.a-A.review", "T1.a-A")
			r.create("T1.a-A", "attempt")
			return done("T1.a-A", r.claimID("w1", "T1.a-A"), passed)
		}},
		{"green", 3, func(r *taskRig) step {
			r.candidate("T1.a-A", true)
			res := map[string]any{"outcome": "qwen review: green", "model_min": 12.5,
				"review": map[string]any{"verdict": "green", "by": "qwen-judge", "judge": "qwen"}}
			return done("T1.a-A.review", r.claimID("w1", "T1.a-A.review"), res)
		}},
		{"red", 3, func(r *taskRig) step {
			r.candidate("T1.a-A", true)
			return done("T1.a-A.review", r.claimID("w1", "T1.a-A.review"), red(recipe("T1.a-P1")))
		}},
		{"red, the last round the limit gives", 1, func(r *taskRig) step {
			r.candidate("T1.a-A", true)
			return done("T1.a-A.review", r.claimID("w1", "T1.a-A.review"), red(recipe("T1.a-P1")))
		}},
		{"red at the limit", 0, func(r *taskRig) step {
			r.candidate("T1.a-A", true)
			return done("T1.a-A.review", r.claimID("w1", "T1.a-A.review"), red(recipe("T1.a-P1")))
		}},
		{"a review without a verdict", 3, func(r *taskRig) step {
			r.candidate("T1.a-A", true)
			return done("T1.a-A.review", r.claimID("w1", "T1.a-A.review"), map[string]any{"outcome": "no verdict", "model_min": 5.0})
		}},
		{"a round passes", 3, func(r *taskRig) step {
			r.candidate("T1.a-A", true)
			r.redRound("T1.a-A", "T1.a-P1")
			return done("T1.a-P1", r.claimID("w1", "T1.a-P1"), passed)
		}},
		{"a round fails its checks", 3, func(r *taskRig) step {
			r.candidate("T1.a-A", true)
			r.redRound("T1.a-A", "T1.a-P1")
			return done("T1.a-P1", r.claimID("w1", "T1.a-P1"), map[string]any{"outcome": "fails", "checks_passed": false, "model_min": 60.0,
				"fix": retry("T1.a-P2", "fix_round", "prereview-qwen")})
		}},
		{"a skip moves to a fresh base", 3, func(r *taskRig) step {
			r.create("T1.a-A", "attempt")
			lease := r.claimID("w1", "T1.a-A")
			return func(r *taskRig) int {
				return r.lease("T1.a-A", "fail", "w1", lease, map[string]any{"reason": "skipped: main changed X.lean", "fix": retry("T1.a-AB1", "attempt", "")}).Code
			}
		}},
		{"a review fails", 3, func(r *taskRig) step {
			r.candidate("T1.a-A", true)
			lease := r.claimID("w1", "T1.a-A.review")
			return func(r *taskRig) int {
				return r.lease("T1.a-A.review", "fail", "w1", lease, map[string]any{"reason": "boom"}).Code
			}
		}},
		{"a review is cancelled", 3, func(r *taskRig) step {
			r.candidate("T1.a-A", true)
			return func(r *taskRig) int {
				w, _ := r.call("POST", tasksPath+"/T1.a-A.review/cancel", testProjectKey, map[string]any{"reason": "operator"})
				return w.Code
			}
		}},
		{"the operator continues", 0, func(r *taskRig) step {
			r.candidate("T1.a-A", true)
			r.finishTask("T1.a-A.review", red(recipe("T1.a-P1")))
			return func(r *taskRig) int {
				w, _ := r.call("POST", tasksPath+"/T1.a-A.review/continue", testProjectKey, map[string]any{"created_by": "owner"})
				return w.Code
			}
		}},
	}
}

// A crash at any write of a task's end, or again at any write of the recovery, comes to the state
// without a crash, and the worker's retry of the same report changes nothing: no second review or
// round, nothing counted twice, no recipe lost.
func TestLoopCrashAtEveryWrite(t *testing.T) {
	for _, tc := range loopEnds() {
		t.Run(tc.name, func(t *testing.T) {
			ref := loopRig(t, t.TempDir(), tc.limit)
			final := tc.setup(ref)
			writes := 0
			ref.g.tasks.write = func(path string, raw []byte) error {
				writes++
				return writeFileAtomic(path, raw)
			}
			final(ref)
			want := ref.digest()
			if writes < 2 {
				t.Fatalf("%d writes: the case tests nothing", writes)
			}
			// stopAfter lets n writes through; -1 lets all through.
			stopAfter := func(r *taskRig, n int) {
				r.g.tasks.write = func(path string, raw []byte) error {
					if n == 0 {
						return errors.New("crashed")
					}
					n--
					return writeFileAtomic(path, raw)
				}
			}
			for at := 0; at <= writes; at++ {
				for _, again := range []int{-1, 0, 2} {
					dir := t.TempDir()
					r := loopRig(t, dir, tc.limit)
					final := tc.setup(r)
					stopAfter(r, at)
					final(r)
					r.g.Close()

					r = loopRig(t, dir, tc.limit)
					if again >= 0 { // the recovery crashes too
						stopAfter(r, again)
						r.digest()
						r.g.Close()
						r = loopRig(t, dir, tc.limit)
					}
					if got := r.digest(); at > 0 && got != want {
						t.Fatalf("crash after write %d of %d, then %d: after the restart\n%s\nwant\n%s", at, writes, again, got, want)
					}
					final(r) // the worker never heard back, and reports again
					if got := r.digest(); got != want {
						t.Fatalf("crash after write %d of %d, then %d: after the retry\n%s\nwant\n%s", at, writes, again, got, want)
					}
					r.g.Close()
					r = loopRig(t, dir, tc.limit)
					if got := r.digest(); got != want {
						t.Fatalf("crash after write %d of %d, then %d: after another restart\n%s\nwant\n%s", at, writes, again, got, want)
					}
					if strings.Contains(mustRead(t, dir), "loop_pending") {
						t.Fatal("a debt stayed on disk")
					}
				}
			}
		})
	}
}

func mustRead(t *testing.T, dir string) string {
	t.Helper()
	files, err := filepath.Glob(taskFile(dir, "*"))
	if err != nil {
		t.Fatal(err)
	}
	var all strings.Builder
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		all.Write(raw)
	}
	return all.String()
}

// The recovery reads what a task file holds: the recipe comes back from loop_pending.
func TestLoopPendingOnDisk(t *testing.T) {
	dir := t.TempDir()
	r := loopRig(t, dir, 3)
	r.candidate("T1.a-A", true)
	lease := r.claimID("w1", "T1.a-A.review")
	r.g.tasks.write = func(path string, raw []byte) error {
		if strings.HasSuffix(path, "T1.a-A.review.json") && strings.Contains(string(raw), `"loop_pending"`) {
			if err := writeFileAtomic(path, raw); err != nil {
				t.Error(err)
			}
		}
		return errors.New("crashed")
	}
	r.lease("T1.a-A.review", "complete", "w1", lease, map[string]any{"result": red(recipe("T1.a-P1"))})
	r.g.Close()
	raw, err := os.ReadFile(taskFile(dir, "T1.a-A.review"))
	if err != nil || !strings.Contains(string(raw), `"loop_pending":{"step":"done","fix":{"id":"T1.a-P1"`) {
		t.Fatalf("on disk: %s %v", raw, err)
	}
	r = loopRig(t, dir, 3)
	if n := r.g.tasks.get("tauceti", "T1.a-P1"); n == nil || n.CreatedBy != gatewayCreator || n.RoundKind != roundQwen {
		t.Fatalf("the round after the restart: %+v", n)
	}
	if c := r.chainOf("T1.a-A"); c.AutoRounds != 1 || c.Reviews != 1 {
		t.Fatalf("chain %+v", c)
	}
}

// A recipe that reuses an earlier round's id is refused: only a crash's own round may be found again.
func TestLoopEarlierRoundIDStaysTaken(t *testing.T) {
	r := loopRig(t, "", 3)
	r.candidate("T1.a-A", true)
	r.redRound("T1.a-A", "T1.a-P1")
	r.candidate("T1.a-P1", true)
	r.redRound("T1.a-P1", "T1.a-P1")
	if p := r.g.tasks.get("tauceti", "T1.a-P1"); p.Stage != StageNeedsOperator {
		t.Fatalf("stage %q", p.Stage)
	}
	if c := r.chainOf("T1.a-A"); c.AutoRounds != 1 || !strings.Contains(c.Stop, "is taken") {
		t.Fatalf("chain %+v", c)
	}
}

// fillFinished adds exported finished tasks until the oldest exported ones are pruned.
func (r *taskRig) fillFinished(prefix string, n int) {
	r.t.Helper()
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("%s-%d", prefix, i)
		r.create(id, "other")
		r.now = r.now.Add(time.Second)
		r.call("POST", tasksPath+"/"+id+"/cancel", testProjectKey, map[string]any{})
		r.now = r.now.Add(time.Second)
		r.export(r.t, 1)
	}
}

// A finished task an open one depends on survives the pruning, however old: a review's candidate, a
// round's predecessor. Once nothing depends on it, it goes as any other.
func TestPruneKeepsWhatOpenTasksNeed(t *testing.T) {
	r := loopRig(t, "", 3)
	r.candidate("T1.a-A", true)
	r.candidate("T2.a-A", true)
	r.redRound("T2.a-A", "T2.a-P1")
	r.candidate("T3.a-A", true)
	r.export(t, 10) // candidates and the finished review, exported and the oldest
	r.fillFinished("O", maxKeptTasks+5)
	for _, id := range []string{"T1.a-A", "T2.a-A", "T3.a-A"} {
		if r.g.tasks.get("tauceti", id) == nil {
			t.Fatalf("%s was pruned while an open task needs it", id)
		}
	}
	if r.g.tasks.get("tauceti", "T2.a-A.review") != nil {
		t.Error("a finished review nothing needs was kept")
	}
	if r.g.tasks.get("tauceti", "O-0") != nil {
		t.Error("the oldest exported task was kept")
	}
	// The review finds its candidate: not "the candidate's task is gone".
	r.verdict("T1.a-A.review", "green", nil)
	if p := r.g.tasks.get("tauceti", "T1.a-A"); p.Stage != StageReady || p.QwenReview == nil {
		t.Fatalf("after the review: %+v", p)
	}
	// With the review done and exported the candidate is pruned like any other.
	r.export(t, 2)
	r.fillFinished("N", maxKeptTasks+5)
	if r.g.tasks.get("tauceti", "T1.a-A") != nil {
		t.Error("the candidate stayed after its review ended")
	}
}

// A recipe waiting for the operator is not pruned from under them; after it runs, it may be.
func TestPruneKeepsAWaitingRecipe(t *testing.T) {
	r := loopRig(t, "", 0)
	r.candidate("T1.a-A", true)
	r.verdict("T1.a-A.review", "red", recipe("T1.a-P1"))
	r.export(t, 10)
	r.fillFinished("O", maxKeptTasks+5)
	if rv := r.g.tasks.get("tauceti", "T1.a-A.review"); rv == nil || rv.NextFix == nil {
		t.Fatalf("the waiting recipe went: %+v", rv)
	}
	if w, _ := r.call("POST", tasksPath+"/T1.a-A.review/continue", testProjectKey, map[string]any{"created_by": "owner"}); w.Code != 201 {
		t.Fatalf("continue: HTTP %d %s", w.Code, w.Body)
	}
}

func bigRecipe(id string, n int) map[string]any {
	m := recipe(id)
	key := id[strings.LastIndexByte(id, '-')+1:]
	m["spec"] = map[string]any{"attempt": id, "files": map[string]any{"tasks/tree_patches/x.diff": strings.Repeat("x", n)},
		"ledger": map[string]any{"subtask": map[string]any{"fix_rounds": map[string]any{key: map[string]any{"pr_base": 12345678901234567}}}}}
	return m
}

// A round past the limit, too big for a create, goes on with `continue`: an operator round, not
// automatic, without the recipe being sent again, with the operator's note where the harness reads it.
func TestLoopContinueBigRecipe(t *testing.T) {
	r := loopRig(t, "", 0)
	r.candidate("T1.a-A", true)
	r.verdict("T1.a-A.review", "red", bigRecipe("T1.a-P1", 300<<10))
	const rid = "T1.a-A.review"
	if r.g.tasks.get("tauceti", rid).NextFix == nil {
		t.Fatal("no waiting recipe")
	}
	// A create keeps its limit: the same recipe does not fit.
	n := bigRecipe("T1.a-P1", 300<<10)
	n["created_by"] = "owner"
	if w, _ := r.call("POST", tasksPath, testProjectKey, n); w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("create: HTTP %d", w.Code)
	}
	cont := func(key string, body any) (int, map[string]any) {
		t.Helper()
		w, out := r.call("POST", tasksPath+"/"+rid+"/continue", key, body)
		return w.Code, out
	}
	for name, c := range map[string]struct {
		key  string
		body any
		want int
	}{"no key": {"", map[string]any{"created_by": "owner"}, 401},
		"another project": {otherProjectKey, map[string]any{"created_by": "owner"}, 403},
		"no creator":      {testProjectKey, map[string]any{}, 400},
		"the loop":        {testProjectKey, map[string]any{"created_by": "gateway"}, 400},
		"key-like note":   {testProjectKey, map[string]any{"created_by": "owner", "note": "bearer abc"}, 400},
		"unknown field":   {testProjectKey, map[string]any{"created_by": "owner", "spec": map[string]any{}}, 400}} {
		if code, out := cont(c.key, c.body); code != c.want {
			t.Errorf("%s: HTTP %d, want %d: %v", name, code, c.want, out)
		}
	}
	code, out := cont(testProjectKey, map[string]any{"created_by": "owner", "note": "`A.lean` imports `B`."})
	if code != 201 || out["created"] != true {
		t.Fatalf("continue: HTTP %d %v", code, out)
	}
	if strings.Contains(fmt.Sprint(out), "xxxx") {
		t.Error("the response carries the recipe")
	}
	round := r.g.tasks.get("tauceti", "T1.a-P1")
	if round == nil || round.CreatedBy != "owner" || round.Class != classFixQwen || round.Chain != "T1.a" || round.RoundKind != roundOperator || round.State != TaskQueued {
		t.Fatalf("the round: %+v", round)
	}
	var spec struct {
		Files  map[string]string
		Ledger struct {
			Subtask struct {
				FixRounds map[string]map[string]json.RawMessage `json:"fix_rounds"`
			}
		}
	}
	if err := json.Unmarshal(round.Spec, &spec); err != nil {
		t.Fatal(err)
	}
	fx := spec.Ledger.Subtask.FixRounds["P1"]
	if string(fx["operator_note"]) != "\"`A.lean` imports `B`.\"" || string(fx["pr_base"]) != "12345678901234567" {
		t.Errorf("the note, and a number as written: %v", fx)
	}
	if len(spec.Files["tasks/tree_patches/x.diff"]) != 300<<10 {
		t.Error("the tree patch was cut")
	}
	c := r.chainOf(rid)
	if c.AutoRounds != 0 || c.OperatorRounds != 1 || c.Status != chainOperatorRound || c.Stop != "" {
		t.Fatalf("chain %+v", c)
	}
	if p := r.g.tasks.get("tauceti", "T1.a-A"); p.Stage != StageFixScheduled {
		t.Fatalf("stage %q", p.Stage)
	}
	rv := r.g.tasks.get("tauceti", rid)
	if rv.NextFix != nil || rv.ContinuedAs != "T1.a-P1" {
		t.Fatalf("the review after: %+v", rv)
	}
	// Twice is once.
	code, out = cont(testProjectKey, map[string]any{"created_by": "owner", "note": "other"})
	if code != 200 || out["created"] != false || out["task"].(map[string]any)["id"] != "T1.a-P1" {
		t.Fatalf("again: HTTP %d %v", code, out)
	}
	if c := r.chainOf(rid); c.OperatorRounds != 1 {
		t.Fatalf("chain %+v", c)
	}
	// The round runs as a person's: its cost is not the loop's, and its review is the gateway's, as before.
	r.candidate("T1.a-P1", true)
	if c := r.chainOf(rid); c.RoundModelMin != 0 {
		t.Fatalf("an operator's round in the automatic cost: %+v", c)
	}
	if rv := r.g.tasks.get("tauceti", "T1.a-P1.review"); rv == nil || rv.CreatedBy != gatewayCreator {
		t.Fatalf("its review: %+v", rv)
	}
}

// `continue` refuses what cannot run, and the recipe stays for the operator.
func TestLoopContinueRefused(t *testing.T) {
	r := loopRig(t, "", 0)
	if w, _ := r.call("POST", tasksPath+"/nope/continue", testProjectKey, map[string]any{"created_by": "owner"}); w.Code != 404 {
		t.Fatalf("no task: HTTP %d", w.Code)
	}
	r.create("T1.a-A", "attempt")
	if w, _ := r.call("POST", tasksPath+"/T1.a-A/continue", testProjectKey, map[string]any{"created_by": "owner"}); w.Code != 409 {
		t.Fatalf("nothing waits: HTTP %d", w.Code)
	}
	// A failed first attempt's retry has no fix-round entry: a note would be lost, so it is refused.
	r.failed("T1.a-A", retry("T1.a-AR1", "attempt", ""))
	if w, _ := r.call("POST", tasksPath+"/T1.a-A/continue", testProjectKey, map[string]any{"created_by": "owner", "note": "a fact"}); w.Code != 409 || !strings.Contains(w.Body.String(), "fix-round") {
		t.Fatalf("note without a place: HTTP %d %s", w.Code, w.Body)
	}
	if w, _ := r.call("POST", tasksPath+"/T1.a-A/continue", testProjectKey, map[string]any{"created_by": "owner"}); w.Code != 201 {
		t.Fatalf("without the note: HTTP %d %s", w.Code, w.Body)
	}
	if n := r.g.tasks.get("tauceti", "T1.a-AR1"); n == nil || n.Class != classAttempt || r.chainOf("T1.a-A").OperatorRounds != 1 {
		t.Fatalf("a retry of kind attempt is the operator's too: %+v", n)
	}
	// A recipe that leaves the chain: past the limit nothing checked it.
	r.candidate("T2.a-A", true)
	r.verdict("T2.a-A.review", "red", recipe("T9.a-P1"))
	if w, _ := r.call("POST", tasksPath+"/T2.a-A.review/continue", testProjectKey, map[string]any{"created_by": "owner"}); w.Code != 409 || !strings.Contains(w.Body.String(), "chain") {
		t.Fatalf("out of the chain: HTTP %d %s", w.Code, w.Body)
	}
	// An id taken since.
	r.candidate("T3.a-A", true)
	r.verdict("T3.a-A.review", "red", recipe("T3.a-P1"))
	r.createFix("T3.a-P1", "fix_round", "prereview-qwen")
	if w, _ := r.call("POST", tasksPath+"/T3.a-A.review/continue", testProjectKey, map[string]any{"created_by": "owner"}); w.Code != 409 || !strings.Contains(w.Body.String(), "taken") {
		t.Fatalf("taken: HTTP %d %s", w.Code, w.Body)
	}
	if rv := r.g.tasks.get("tauceti", "T3.a-A.review"); rv.NextFix == nil || rv.ContinuedAs != "" {
		t.Fatalf("the recipe after a refusal: %+v", rv)
	}
}

// Ids without a hyphen: a review stays in its candidate's chain, also after a restart and when a
// person creates it, so a restart cannot split the count.
func TestChainOfPlainIDs(t *testing.T) {
	for id, want := range map[[2]string]string{{"plain.review", "plain"}: "plain", {"plain.r-2.review", "plain"}: "plain",
		{"T5-NS-3.a-P3.review", "T5-NS-3.a-P3"}: "T5-NS-3.a", {"plain", ""}: "plain", {"T5.a-A", ""}: "T5.a"} {
		if got := chainFor(id[0], id[1]); got != want {
			t.Errorf("chainFor(%v) = %s, want %s", id, got, want)
		}
	}
	dir := t.TempDir()
	r := loopRig(t, dir, 3)
	r.candidate("plain", true)
	if rv := r.g.tasks.get("tauceti", "plain.review"); rv == nil || rv.Chain != "plain" {
		t.Fatalf("review: %+v", rv)
	}
	r.g.Close()
	r = loopRig(t, dir, 3)
	if rv := r.g.tasks.get("tauceti", "plain.review"); rv.Chain != "plain" {
		t.Fatalf("after a restart the review's chain is %q", rv.Chain)
	}
	r.verdict("plain.review", "red", recipe("plain-P1"))
	c := r.g.tasks.chainView("tauceti", "plain")
	if c == nil || c.Reviews != 1 || c.AutoRounds != 1 || c.Status != StageFixScheduled {
		t.Fatalf("chain %+v", c)
	}
	if r.g.tasks.chainView("tauceti", "plain.review") != nil {
		t.Fatal("the restart split the chain")
	}
	r.createReview("plain.r2.review", "plain")
	if rv := r.g.tasks.get("tauceti", "plain.r2.review"); rv.Chain != "plain" {
		t.Fatalf("a person's review starts in chain %q", rv.Chain)
	}
	r.g.Close()
	r = loopRig(t, dir, 3)
	if rv := r.g.tasks.get("tauceti", "plain.r2.review"); rv.Chain != "plain" {
		t.Fatalf("a person's review is in chain %q", rv.Chain)
	}
}

// A task under a candidate's review id means the loop's review will not come: the candidate goes to the
// operator, whatever the task is and however far it got, and nothing is made in its place.
func TestLoopReviewIDTaken(t *testing.T) {
	for name, setup := range map[string]func(r *taskRig){
		"a person's review, queued":               func(r *taskRig) { r.createReview("T1.a-A.review", "T1.a-A") },
		"a person's review, of another candidate": func(r *taskRig) { r.createReview("T1.a-A.review", "T0.a-A") },
		"a person's review, running": func(r *taskRig) {
			r.createReview("T1.a-A.review", "T1.a-A")
			r.claimID("w2", "T1.a-A.review")
		},
		"a person's review, finished": func(r *taskRig) {
			r.createReview("T1.a-A.review", "T1.a-A")
			r.verdict("T1.a-A.review", "green", nil)
		},
		"another kind of task": func(r *taskRig) { r.create("T1.a-A.review", "other") },
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			r := loopRig(t, dir, 3)
			setup(r)
			r.candidate("T1.a-A", true)
			check := func() {
				t.Helper()
				c, got := r.chainOf("T1.a-A"), r.g.tasks.get("tauceti", "T1.a-A")
				if got.Stage != StageNeedsOperator || got.LoopPending != nil || c.Status != StageNeedsOperator || !strings.Contains(c.Stop, "T1.a-A.review") {
					t.Fatalf("candidate stage %q, chain %+v", got.Stage, c)
				}
			}
			check()
			r.g.Close()
			r = loopRig(t, dir, 3)
			check()
		})
	}
}

// The same for one of the loop's own rounds, which a recipe may name like a review.
func TestLoopReviewIDTakenByARound(t *testing.T) {
	r := loopRig(t, t.TempDir(), 3)
	r.create("T1.a-P1", "attempt")
	r.candidate("T1.a-A", true)
	r.verdict("T1.a-A.review", "red", recipe("T1.a-P1.review"))
	if n := r.g.tasks.get("tauceti", "T1.a-P1.review"); n == nil || n.Kind != "fix_round" || n.CreatedBy != gatewayCreator {
		t.Fatalf("round %+v", n)
	}
	r.candidate("T1.a-P1", true)
	if got := r.g.tasks.get("tauceti", "T1.a-P1"); got.Stage != StageNeedsOperator {
		t.Fatalf("stage %q", got.Stage)
	}
}

// owe gives a task its debt again, as a crash before settle cleared it would leave it, and pays it.
func (r *taskRig) owe(id string) {
	r.t.Helper()
	s := r.g.tasks
	s.mu.Lock()
	defer s.mu.Unlock()
	p, err := s.load("tauceti")
	if err != nil {
		r.t.Fatal(err)
	}
	t := p.byID[id]
	t.LoopPending = &loopPending{Step: owedDone}
	if err := s.settle(p, t, r.now); err != nil {
		r.t.Fatal(err)
	}
}

// The loop's own review is never a stranger: a candidate's debt paid again after its review was
// claimed or ended neither stops the chain nor takes back what the review decided.
func TestLoopOwnReviewAfterACrash(t *testing.T) {
	r := loopRig(t, t.TempDir(), 3)
	r.candidate("T1.a-A", true)
	r.claimID("w1", "T1.a-A.review")
	r.owe("T1.a-A")
	if got, c := r.g.tasks.get("tauceti", "T1.a-A"), r.chainOf("T1.a-A"); got.Stage != StageInReview || c.Status != StageInReview || c.Stop != "" {
		t.Fatalf("running: stage %q, chain %+v", got.Stage, c)
	}

	r = loopRig(t, t.TempDir(), 3)
	r.candidate("T1.a-A", true)
	r.verdict("T1.a-A.review", "green", nil)
	r.owe("T1.a-A")
	if got, c := r.g.tasks.get("tauceti", "T1.a-A"), r.chainOf("T1.a-A"); got.Stage != StageReady || c.Status != StageReady || c.Stop != "passed" {
		t.Fatalf("ended: stage %q, chain %+v", got.Stage, c)
	}
}

// A chain in an answer is its own copy: a task ending meanwhile writes in the live chain's rounds
// (costRound), and the answer is written outside the lock. Run with -race.
func TestChainViewOwnsItsRounds(t *testing.T) {
	r := loopRig(t, t.TempDir(), 3)
	r.candidate("T1.a-A", true)
	r.redRound("T1.a-A", "T1.a-P1")
	view := r.chainOf("T1.a-A")
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			default:
				json.Marshal(view)
			}
		}
	}()
	r.candidate("T1.a-P1", true)
	close(stop)
	<-done
	if view.Rounds[0].Costed {
		t.Fatal("the view changed with the chain")
	}
	if c := r.chainOf("T1.a-A"); !c.Rounds[0].Costed {
		t.Fatalf("the chain did not record the cost: %+v", c)
	}
}

// A list leaves out a waiting recipe, as it does a task's spec, and the recipe stays for the loop: a
// debt that a disk fault keeps open must not cost each list its size again.
func TestListLeavesOutAWaitingRecipe(t *testing.T) {
	r := loopRig(t, t.TempDir(), 3)
	r.candidate("T1.a-A", true)
	lease := r.claimID("w1", "T1.a-A.review")
	r.diskFull(isChainFile)
	r.lease("T1.a-A.review", "complete", "w1", lease, map[string]any{"result": red(bigRecipe("T1.a-P1", 1<<20))})
	w, out := r.call("GET", tasksPath, testProjectKey, nil)
	if w.Code != 200 || w.Body.Len() > 64<<10 {
		t.Fatalf("list: HTTP %d, %d bytes", w.Code, w.Body.Len())
	}
	seen := false
	for _, x := range out["tasks"].([]any) {
		if m := x.(map[string]any); m["id"] == "T1.a-A.review" {
			lp, _ := m["loop_pending"].(map[string]any)
			seen = lp["step"] == owedDone
		}
	}
	if !seen {
		t.Fatalf("the debt is not in the list: %s", w.Body)
	}
	if got := r.g.tasks.get("tauceti", "T1.a-A.review"); got.LoopPending == nil || got.LoopPending.Fix == nil || len(got.LoopPending.Fix.Spec) < 1<<20 {
		t.Fatalf("a full view lost the recipe: %+v", got.LoopPending)
	}
	r.diskBack()
	r.g.tasks.sweepAll()
	if n := r.g.tasks.get("tauceti", "T1.a-P1"); n == nil || len(n.Spec) < 1<<20 {
		t.Fatal("the round lost its recipe")
	}
	live := &Task{LoopPending: &loopPending{Step: owedDone, Fix: &taskNew{ID: "x"}}}
	if v := live.summary(); v.LoopPending.Fix != nil || live.LoopPending.Fix == nil {
		t.Fatalf("summary %+v, task %+v", v.LoopPending, live.LoopPending)
	}
}
