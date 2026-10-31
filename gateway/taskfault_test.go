// A write that fails is a crash of that project's queue (tasks.go): these tests inject a full disk
// and a read-only directory and hold the store to it.
//
// SPDX-License-Identifier: Apache-2.0
package gateway

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func isChainFile(path string) bool { return strings.Contains(path, "task-chains-") }
func anyFile(string) bool          { return true }

// diskFull fails the writes of the files that match, with what a full disk gives.
func (r *taskRig) diskFull(match func(path string) bool) {
	r.g.tasks.write = func(path string, raw []byte) error {
		if match(path) {
			return syscall.ENOSPC
		}
		return writeFileAtomic(path, raw)
	}
}

// fullAfter lets n writes through, then the disk is full.
func (r *taskRig) fullAfter(n int) {
	r.g.tasks.write = func(path string, raw []byte) error {
		if n == 0 {
			return syscall.ENOSPC
		}
		n--
		return writeFileAtomic(path, raw)
	}
}

func (r *taskRig) diskBack() { r.g.tasks.write = writeFileAtomic }

// readOnly makes a queue's directories read-only until the returned call; root ignores that.
func readOnly(t *testing.T, dirs ...string) func() {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory modes")
	}
	back := func() {
		for _, d := range dirs {
			if err := os.Chmod(d, 0o700); err != nil {
				t.Fatal(err)
			}
		}
	}
	for _, d := range dirs {
		if err := os.Chmod(d, 0o500); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(back)
	return back
}

// memoryIsDisk: after the next open, memory holds exactly what the files hold, every field.
func (r *taskRig) memoryIsDisk(dir string) {
	r.t.Helper()
	s := r.g.tasks
	s.mu.Lock()
	defer s.mu.Unlock()
	p, _, err := s.open("tauceti")
	if err != nil {
		r.t.Fatal(err)
	}
	compare := func(what, sub string, have map[string]any) {
		r.t.Helper()
		files, _ := filepath.Glob(filepath.Join(dir, sub, "*.json"))
		if len(files) != len(have) {
			r.t.Fatalf("%d %ss in memory, %d on disk", len(have), what, len(files))
		}
		for _, f := range files {
			name := strings.TrimSuffix(filepath.Base(f), ".json")
			mem, ok := have[name]
			if !ok {
				r.t.Fatalf("%s %s on disk, not in memory", what, name)
			}
			raw, err := os.ReadFile(f)
			if err != nil {
				r.t.Fatal(err)
			}
			want, _ := json.Marshal(mem)
			if string(want) != string(raw) {
				r.t.Fatalf("%s %s\nin memory %s\non disk   %s", what, name, want, raw)
			}
		}
	}
	tasks, chains := map[string]any{}, map[string]any{}
	for id, t := range p.byID {
		tasks[id] = t
	}
	for id, c := range p.chains {
		chains[id] = c
	}
	compare("task", "tasks-tauceti", tasks)
	compare("chain", "task-chains-tauceti", chains)
}

// chainState is the digest as it stands, without paying a debt on the way.
func (r *taskRig) chainState() string {
	r.t.Helper()
	s := r.g.tasks
	s.mu.Lock()
	defer s.mu.Unlock()
	p, err := s.load("tauceti")
	if err != nil {
		r.t.Fatal(err)
	}
	var out []string
	for _, t := range p.byID {
		out = append(out, fmt.Sprintf("%s owed=%v", t.ID, t.LoopPending != nil))
	}
	return strings.Join(out, "\n")
}

func unavailable(t *testing.T, code int, what string) {
	t.Helper()
	if code != http.StatusServiceUnavailable {
		t.Fatalf("%s: HTTP %d, want 503", what, code)
	}
}

// Whatever write of a task's end fails, the answer is true: ok means a restart keeps all of it, and
// a refused first write means nothing happened. Memory never runs ahead of the disk, and the queue
// comes to the state without a failure, with or without a restart, however often the recovery fails
// again. A worker's retry changes nothing.
func TestFailedWriteAtEveryPoint(t *testing.T) {
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
			for at := 0; at < writes; at++ {
				for _, again := range []int{0, 1, 2} {
					dir := t.TempDir()
					r := loopRig(t, dir, tc.limit)
					final := tc.setup(r)
					before := r.digest()
					r.fullAfter(at)
					code := final(r)
					if at == 0 {
						unavailable(t, code, "the first write failed")
					} else if code >= 300 && code != http.StatusServiceUnavailable {
						t.Fatalf("write %d: HTTP %d", at, code)
					}
					r.memoryIsDisk(dir)
					full := r.digest()
					if at == 0 && full != before {
						t.Fatalf("a refused call changed the queue\n%s\nwant\n%s", full, before)
					}

					// A restart while the disk is still full sees what the running gateway sees.
					r.g.Close()
					r = loopRig(t, dir, tc.limit)
					r.diskFull(anyFile)
					if got := r.digest(); got != full {
						t.Fatalf("write %d: after a restart\n%s\nwant\n%s", at, got, full)
					}

					// The recovery fails too, and the sweep pays what is left once it can.
					r.fullAfter(again)
					r.g.tasks.sweepAll()
					r.memoryIsDisk(dir)
					r.diskBack()
					r.g.tasks.sweepAll()
					if at > 0 {
						if got := r.digest(); got != want {
							t.Fatalf("write %d, recovery stops after %d: after the sweep\n%s\nwant\n%s", at, again, got, want)
						}
					}
					if code := final(r); code >= 300 {
						t.Fatalf("write %d, recovery stops after %d: the retry: HTTP %d", at, again, code)
					}
					if got := r.digest(); got != want {
						t.Fatalf("write %d, recovery stops after %d: after the retry\n%s\nwant\n%s", at, again, got, want)
					}
					r.memoryIsDisk(dir)
					r.g.Close()
					r = loopRig(t, dir, tc.limit)
					if got := r.digest(); got != want {
						t.Fatalf("write %d, recovery stops after %d: after a restart\n%s\nwant\n%s", at, again, got, want)
					}
					if strings.Contains(mustRead(t, dir), "loop_pending") {
						t.Fatal("a debt stayed on disk")
					}
				}
			}
		})
	}
}

// One write fails and the next ones work, as when space comes and goes or one directory is broken: the
// call must stop there, not go on and clear the debt over a write that is missing.
func TestOneFailedWriteAtEveryPoint(t *testing.T) {
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
			for at := 0; at < writes; at++ {
				dir := t.TempDir()
				r := loopRig(t, dir, tc.limit)
				final := tc.setup(r)
				n := 0
				r.g.tasks.write = func(path string, raw []byte) error {
					if n++; n == at+1 {
						return syscall.ENOSPC
					}
					return writeFileAtomic(path, raw)
				}
				final(r)
				if s := r.chainState(); at > 0 && !strings.Contains(s, "owed=true") {
					t.Fatalf("write %d failed and no debt is left\n%s", at, s)
				}
				r.memoryIsDisk(dir)
				r.g.tasks.sweepAll()
				if at == 0 {
					final(r)
				}
				if got := r.digest(); got != want {
					t.Fatalf("write %d failed: after the sweep\n%s\nwant\n%s", at, got, want)
				}
				r.memoryIsDisk(dir)
			}
		})
	}
}

// Without a restart too: the failure heals, and the next sweep does the rest.
func TestFailedWriteHealsWithoutRestart(t *testing.T) {
	dir := t.TempDir()
	r := loopRig(t, dir, 3)
	r.candidate("T1.a-A", true)
	lease := r.claimID("w1", "T1.a-A.review")
	r.diskFull(isChainFile)
	if w := r.lease("T1.a-A.review", "complete", "w1", lease, map[string]any{"result": red(recipe("T1.a-P1"))}); w.Code != 200 {
		t.Fatalf("the end is saved, the chain is not: HTTP %d %s", w.Code, w.Body)
	}
	r.diskBack()
	r.g.tasks.sweepAll()
	if n := r.g.tasks.get("tauceti", "T1.a-P1"); n == nil {
		t.Fatal("no round after the disk came back")
	}
	if c := r.chainOf("T1.a-A"); c.AutoRounds != 1 || c.Reviews != 1 {
		t.Fatalf("chain %+v", c)
	}
}

// The background sweep replaces stale memory by itself; nothing waits for a call.
func TestSweepReadsAgainAfterAFailedWrite(t *testing.T) {
	r := newTaskRig(t, t.TempDir())
	r.create("T-A", "attempt")
	r.diskFull(anyFile)
	r.call("POST", tasksPath, testProjectKey, newTask("T-B", "attempt"))
	s := r.g.tasks
	s.sweepAll()
	s.mu.Lock()
	defer s.mu.Unlock()
	if p := s.byPr["tauceti"]; p.stale || p.byID["T-B"] != nil {
		t.Fatalf("after the sweep: stale %v, %d tasks", p.stale, len(p.byID))
	}
}

// A chain that cannot be saved stops scheduling: without its count on disk no round may exist, or a
// restart would hand the chain its rounds again. With the limit of 1 and a chain that fails at the
// first round, the second red verdict still ends at the operator.
func TestChainWriteFailureHoldsTheLimit(t *testing.T) {
	for _, fault := range []string{"full disk", "read-only dir"} {
		t.Run(fault, func(t *testing.T) {
			dir := t.TempDir()
			r := loopRig(t, dir, 1)
			r.candidate("T1.a-A", true)
			lease := r.claimID("w1", "T1.a-A.review")
			heal := func() { r.diskBack() }
			r.diskFull(isChainFile)
			if fault == "read-only dir" {
				r.diskBack()
				heal = readOnly(t, filepath.Join(dir, "task-chains-tauceti"))
			}
			if w := r.lease("T1.a-A.review", "complete", "w1", lease, map[string]any{"result": red(recipe("T1.a-P1"))}); w.Code != 200 {
				t.Fatalf("the verdict is saved with its debt, so it is ok: HTTP %d %s", w.Code, w.Body)
			}
			check := func(when string) {
				t.Helper()
				if n := r.g.tasks.get("tauceti", "T1.a-P1"); n != nil {
					t.Fatalf("%s: a round without its count: %+v", when, n)
				}
				if c := r.chainOf("T1.a-A"); c.AutoRounds != 0 || c.Reviews != 0 {
					t.Fatalf("%s: memory ahead of the disk: %+v", when, c)
				}
				if rv := r.g.tasks.get("tauceti", "T1.a-A.review"); rv.State != TaskDone || rv.LoopPending == nil || rv.LoopPending.Fix == nil {
					t.Fatalf("%s: the debt went: %+v", when, rv)
				}
				if w, _ := r.call("POST", tasksPath+"/claim", testProjectKey, map[string]any{"worker": "w2"}); w.Code != http.StatusServiceUnavailable {
					t.Fatalf("%s: claim with a debt open: HTTP %d %s", when, w.Code, w.Body)
				}
			}
			check("at once")
			r.g.tasks.sweepAll()
			r.g.tasks.sweepAll()
			check("after two sweeps")
			if fault == "full disk" { // the write hook does not survive a restart; the directory does
				r.g.Close()
				r = loopRig(t, dir, 1)
				r.diskFull(isChainFile)
				check("after a restart")
			}
			heal()
			r.g.tasks.sweepAll()
			c := r.chainOf("T1.a-A")
			if n := r.g.tasks.get("tauceti", "T1.a-P1"); n == nil || c.AutoRounds != 1 || c.Reviews != 1 || len(c.Rounds) != 1 {
				t.Fatalf("after the disk came back: round %+v, chain %+v", n, c)
			}
			r.g.Close()
			r = loopRig(t, dir, 1)
			r.candidate("T1.a-P1", false)
			if n := r.g.tasks.get("tauceti", "T1.a-P2"); n != nil {
				t.Fatalf("a second round past the limit of 1: %+v", n)
			}
			if c := r.chainOf("T1.a-A"); c.AutoRounds != 1 || c.Status != StageNeedsOperator {
				t.Fatalf("chain %+v", c)
			}
		})
	}
}

// Every call that changes a task: with a full disk or a read-only directory it answers 503, changes
// nothing, not in memory and not after a restart, and works when repeated afterwards.
func TestEveryCallReportsAFailedWrite(t *testing.T) {
	calls := []struct {
		name string
		do   func(r *taskRig, lease string) int
	}{
		{"create", func(r *taskRig, _ string) int {
			w, _ := r.call("POST", tasksPath, testProjectKey, newTask("N", "attempt"))
			return w.Code
		}},
		{"claim", func(r *taskRig, _ string) int {
			w, _ := r.call("POST", tasksPath+"/claim", testProjectKey, map[string]any{"worker": "w2"})
			return w.Code
		}},
		{"heartbeat", func(r *taskRig, lease string) int { return r.lease("L", "heartbeat", "w1", lease, nil).Code }},
		{"complete", func(r *taskRig, lease string) int {
			return r.lease("L", "complete", "w1", lease, map[string]any{"result": okResult}).Code
		}},
		{"fail", func(r *taskRig, lease string) int {
			return r.lease("L", "fail", "w1", lease, map[string]any{"reason": "boom"}).Code
		}},
		{"give back", func(r *taskRig, lease string) int {
			return r.lease("L", "fail", "w1", lease, map[string]any{"reason": "spot stop", "requeue": true}).Code
		}},
		{"cancel", func(r *taskRig, _ string) int {
			w, _ := r.call("POST", tasksPath+"/Q/cancel", testProjectKey, map[string]any{"reason": "no"})
			return w.Code
		}},
		{"max_tries", func(r *taskRig, _ string) int {
			w, _ := r.call("POST", tasksPath+"/Q/max_tries", testProjectKey, map[string]any{"max_tries": 7})
			return w.Code
		}},
		{"review", func(r *taskRig, _ string) int {
			w, _ := r.call("POST", tasksPath+"/D/review", testProjectKey, map[string]any{"verdict": "green", "by": "owner-codex"})
			return w.Code
		}},
		{"decide", func(r *taskRig, _ string) int {
			w, _ := r.call("POST", tasksPath+"/D/decide", testProjectKey, map[string]any{"decision": "approved", "by": "owner"})
			return w.Code
		}},
		{"exported", func(r *taskRig, _ string) int {
			seen := r.g.tasks.get("tauceti", "D").UpdatedAt
			w, _ := r.call("POST", tasksPath+"/D/exported", testProjectKey, map[string]any{"seen": seen})
			return w.Code
		}},
	}
	for _, fault := range []string{"full disk", "read-only dir"} {
		for _, c := range calls {
			t.Run(fault+"/"+c.name, func(t *testing.T) {
				dir := t.TempDir()
				r := newTaskRig(t, dir)
				r.create("D", "attempt")
				r.lease("D", "complete", "w1", r.claimID("w1", "D"), map[string]any{"result": okResult})
				r.create("L", "attempt")
				lease := r.claimID("w1", "L")
				r.create("Q", "attempt")
				r.g.tasks.mu.Lock()
				before, _ := json.Marshal(r.g.tasks.byPr["tauceti"].byID)
				r.g.tasks.mu.Unlock()

				heal := r.diskBack
				if fault == "full disk" {
					r.diskFull(anyFile)
				} else {
					heal = readOnly(t, filepath.Join(dir, "tasks-tauceti"))
				}
				unavailable(t, c.do(r, lease), c.name)
				r.memoryIsDisk(dir)
				r.g.tasks.mu.Lock()
				after, _ := json.Marshal(r.g.tasks.byPr["tauceti"].byID)
				r.g.tasks.mu.Unlock()
				if string(after) != string(before) {
					t.Fatalf("a refused call changed the queue\n%s\nwant\n%s", after, before)
				}
				heal()
				if code := c.do(r, lease); code >= 300 {
					t.Fatalf("repeated: HTTP %d", code)
				}
				r.memoryIsDisk(dir)
			})
		}
	}
}

// The answer of a refused write says to come back; the runner reads any 5xx that way and keeps its
// result.
func TestFailedWriteAnswer(t *testing.T) {
	r := newTaskRig(t, t.TempDir())
	r.diskFull(anyFile)
	w, out := r.call("POST", tasksPath, testProjectKey, newTask("N", "attempt"))
	unavailable(t, w.Code, "create")
	if w.Header().Get("Retry-After") == "" {
		t.Fatal("no Retry-After")
	}
	msg := out["error"].(map[string]any)["message"].(string)
	if !strings.Contains(msg, "nothing changed") || strings.Contains(msg, "no space") || strings.Contains(msg, "tasks-") {
		t.Fatalf("message %q", msg)
	}
}

// A disk that does not answer is a 503, never an empty queue, and comes back by itself.
func TestUnreadableQueueIsNotEmpty(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory modes")
	}
	dir := t.TempDir()
	r := newTaskRig(t, dir)
	r.create("T-A", "attempt")
	r.g.Close()
	r = newTaskRig(t, dir)
	tasks := filepath.Join(dir, "tasks-tauceti")
	if err := os.Chmod(tasks, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(tasks, 0o700) })
	for _, c := range []struct{ method, path string }{{"GET", tasksPath}, {"GET", tasksPath + "/T-A"}} {
		w, _ := r.call(c.method, c.path, testProjectKey, nil)
		unavailable(t, w.Code, c.method+" "+c.path)
	}
	w, _ := r.call("POST", tasksPath, testProjectKey, newTask("T-A", "attempt"))
	unavailable(t, w.Code, "create over a task it cannot see")
	if err := os.Chmod(tasks, 0o700); err != nil {
		t.Fatal(err)
	}
	if got := r.state("T-A"); got != TaskQueued {
		t.Fatalf("state %q", got)
	}
}

// A lease that expires while nothing can be written stays as the disk has it; when the disk is back
// the sweep gives it back, and its worker takes it back without losing the try.
func TestLeaseExpiresDuringFailedWrites(t *testing.T) {
	dir := t.TempDir()
	r := newTaskRig(t, dir)
	r.create("T-A", "attempt")
	id, lease := r.claim("w1", 60)
	r.diskFull(anyFile)
	r.now = r.now.Add(2 * time.Minute)
	unavailable(t, r.lease(id, "heartbeat", "w1", lease, nil).Code, "heartbeat")
	if got := r.g.tasks.get("tauceti", id); got.State != TaskLeased || !got.Lease.ExpiresAt.Before(r.now) {
		t.Fatalf("task %+v", got)
	}
	r.memoryIsDisk(dir)
	r.diskBack()
	if w := r.lease(id, "heartbeat", "w1", lease, nil); w.Code != http.StatusConflict {
		t.Fatalf("heartbeat after the disk came back: HTTP %d", w.Code)
	}
	if code, _ := r.takeBack("w1", id); code != 200 {
		t.Fatalf("take back: HTTP %d", code)
	}
	if got := r.g.tasks.get("tauceti", id); got.Tries != 1 || got.Interruptions != 1 {
		t.Fatalf("task %+v", got)
	}
}

// A person's round: the chain first, as for an automatic one; a create repeated after either write
// failed counts it once.
func TestPersonsRoundFailedWrites(t *testing.T) {
	for _, tc := range []struct {
		name  string
		match func(string) bool
	}{
		{"the chain", isChainFile},
		{"the task", func(path string) bool { return strings.HasSuffix(path, "T1.a-P9.json") && !isChainFile(path) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			r := loopRig(t, dir, 3)
			r.candidate("T1.a-A", true)
			n := recipe("T1.a-P9")
			n["created_by"] = "owner"
			r.diskFull(tc.match)
			w, _ := r.call("POST", tasksPath, testProjectKey, n)
			unavailable(t, w.Code, "create")
			if r.g.tasks.get("tauceti", "T1.a-P9") != nil {
				t.Fatal("the round exists after a refused create")
			}
			r.memoryIsDisk(dir)
			r.diskBack()
			if w, _ := r.call("POST", tasksPath, testProjectKey, n); w.Code != http.StatusCreated {
				t.Fatalf("repeated: HTTP %d %s", w.Code, w.Body)
			}
			if c := r.chainOf("T1.a-A"); c.OperatorRounds != 1 || c.Status != chainOperatorRound {
				t.Fatalf("chain %+v", c)
			}
			r.memoryIsDisk(dir)
		})
	}
}

// `continue` saved but its round not written: 503, not "gone" or nothing, and the round comes.
func TestContinueFailedWrite(t *testing.T) {
	dir := t.TempDir()
	r := loopRig(t, dir, 0)
	r.candidate("T1.a-A", true)
	r.finishTask("T1.a-A.review", red(recipe("T1.a-P1")))
	cont := func() (int, map[string]any) {
		w, out := r.call("POST", tasksPath+"/T1.a-A.review/continue", testProjectKey, map[string]any{"created_by": "owner"})
		return w.Code, out
	}
	r.diskFull(isChainFile)
	code, _ := cont()
	unavailable(t, code, "continue, first")
	if rv := r.g.tasks.get("tauceti", "T1.a-A.review"); rv.ContinuedAs != "T1.a-P1" || rv.LoopPending == nil {
		t.Fatalf("the request is not saved: %+v", rv)
	}
	code, _ = cont()
	unavailable(t, code, "continue, again")
	r.diskBack()
	code, out := cont()
	if code != 200 || out["created"] != false || out["task"].(map[string]any)["id"] != "T1.a-P1" {
		t.Fatalf("continue, disk back: HTTP %d %v", code, out)
	}
}

// A task that owes the loop a step is not pruned, nor is the candidate the step reads, whatever the
// export did.
func TestPruneKeepsADebt(t *testing.T) {
	dir := t.TempDir()
	r := loopRig(t, dir, 3)
	r.candidate("T1.a-A", true)
	lease := r.claimID("w1", "T1.a-A.review")
	r.diskFull(isChainFile)
	r.lease("T1.a-A.review", "complete", "w1", lease, map[string]any{"result": red(recipe("T1.a-P1"))})
	r.export(t, 10)
	r.fillFinished("O", maxKeptTasks+5)
	for _, id := range []string{"T1.a-A", "T1.a-A.review"} {
		if r.g.tasks.get("tauceti", id) == nil {
			t.Fatalf("%s was pruned with a debt open", id)
		}
	}
	r.diskBack()
	r.g.tasks.sweepAll()
	if r.g.tasks.get("tauceti", "T1.a-P1") == nil {
		t.Fatal("no round")
	}
}

// A file that cannot be removed keeps its task; a later prune removes it.
func TestDropKeepsATaskItCannotRemove(t *testing.T) {
	dir := t.TempDir()
	r := newTaskRig(t, dir)
	r.create("T-A", "attempt")
	back := readOnly(t, filepath.Join(dir, "tasks-tauceti"))
	s := r.g.tasks
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.byPr["tauceti"]
	if s.drop(p.byID["T-A"]) || p.byID["T-A"] == nil {
		t.Fatal("the task went while its file stayed")
	}
	back()
	if !s.drop(p.byID["T-A"]) || p.byID["T-A"] != nil {
		t.Fatal("the task stayed")
	}
	if _, err := os.Stat(taskFile(dir, "T-A")); !os.IsNotExist(err) {
		t.Fatal(err)
	}
}
