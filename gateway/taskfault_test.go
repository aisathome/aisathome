// A write that fails is a crash of that project's queue (tasks.go): these tests inject a full disk
// and a read-only directory and hold the store to it.
//
// SPDX-License-Identifier: Apache-2.0
package gateway

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func anyFile(string) bool { return true }

func taskFile(dir, id string) string { return filepath.Join(dir, "tasks-lab", id+".json") }

// diskFull fails the writes of the files that match, with what a full disk gives.
func (r *taskRig) diskFull(match func(path string) bool) {
	r.g.tasks.write = func(path string, raw []byte) error {
		if match(path) {
			return syscall.ENOSPC
		}
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
	p, _, err := s.open("lab")
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
	tasks := map[string]any{}
	for id, t := range p.byID {
		tasks[id] = t
	}
	compare("task", "tasks-lab", tasks)
}

func unavailable(t *testing.T, code int, what string) {
	t.Helper()
	if code != http.StatusServiceUnavailable {
		t.Fatalf("%s: HTTP %d, want 503", what, code)
	}
}

// The background sweep replaces stale memory by itself; nothing waits for a call.
func TestSweepReadsAgainAfterAFailedWrite(t *testing.T) {
	r := newTaskRig(t, t.TempDir())
	r.create("T-A", "p2m-v7")
	r.diskFull(anyFile)
	r.call("POST", tasksPath, testProjectKey, newTask("T-B", "p2m-v7"))
	s := r.g.tasks
	s.sweepAll()
	s.mu.Lock()
	defer s.mu.Unlock()
	if p := s.byPr["lab"]; p.stale || p.byID["T-B"] != nil {
		t.Fatalf("after the sweep: stale %v, %d tasks", p.stale, len(p.byID))
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
			w, _ := r.call("POST", tasksPath, testProjectKey, newTask("N", "p2m-v7"))
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
		{"complete, closed by another", func(r *taskRig, lease string) int {
			return r.lease("L", "complete", "w1", lease, map[string]any{"result": map[string]any{"outcome": "closed"}, "closed_by": "reuse:formalpedia:foo"}).Code
		}},
		{"cancel, closed by another", func(r *taskRig, _ string) int {
			w, _ := r.call("POST", tasksPath+"/Q/cancel", testProjectKey, map[string]any{"reason": "hole closed", "closed_by": "t6-x-h01"})
			return w.Code
		}},
		{"exported", func(r *taskRig, _ string) int {
			seen := r.g.tasks.get("lab", "D").UpdatedAt
			w, _ := r.call("POST", tasksPath+"/D/exported", testProjectKey, map[string]any{"seen": seen})
			return w.Code
		}},
	}
	for _, fault := range []string{"full disk", "read-only dir"} {
		for _, c := range calls {
			t.Run(fault+"/"+c.name, func(t *testing.T) {
				dir := t.TempDir()
				r := newTaskRig(t, dir)
				r.create("D", "p2m-v7")
				r.lease("D", "complete", "w1", r.claimID("w1", "D"), map[string]any{"result": okResult})
				r.create("L", "p2m-v7")
				lease := r.claimID("w1", "L")
				r.create("Q", "p2m-v7")
				r.g.tasks.mu.Lock()
				before, _ := json.Marshal(r.g.tasks.byPr["lab"].byID)
				r.g.tasks.mu.Unlock()

				heal := r.diskBack
				if fault == "full disk" {
					r.diskFull(anyFile)
				} else {
					heal = readOnly(t, filepath.Join(dir, "tasks-lab"))
				}
				unavailable(t, c.do(r, lease), c.name)
				r.memoryIsDisk(dir)
				r.g.tasks.mu.Lock()
				after, _ := json.Marshal(r.g.tasks.byPr["lab"].byID)
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
	w, out := r.call("POST", tasksPath, testProjectKey, newTask("N", "p2m-v7"))
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
	r.create("T-A", "p2m-v7")
	r.g.Close()
	r = newTaskRig(t, dir)
	tasks := filepath.Join(dir, "tasks-lab")
	if err := os.Chmod(tasks, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(tasks, 0o700) })
	for _, c := range []struct{ method, path string }{{"GET", tasksPath}, {"GET", tasksPath + "/T-A"}} {
		w, _ := r.call(c.method, c.path, testProjectKey, nil)
		unavailable(t, w.Code, c.method+" "+c.path)
	}
	w, _ := r.call("POST", tasksPath, testProjectKey, newTask("T-A", "p2m-v7"))
	unavailable(t, w.Code, "create over a task it cannot see")
	var d opsData
	r.g.opsTasks(&d, "lab", r.now)
	if !strings.Contains(d.TaskCounts, "cannot be read") {
		t.Fatalf("the ops card says %q", d.TaskCounts)
	}
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
	r.create("T-A", "p2m-v7")
	id, lease := r.claim("w1", 60)
	r.diskFull(anyFile)
	r.advance(2 * time.Minute)
	unavailable(t, r.lease(id, "heartbeat", "w1", lease, nil).Code, "heartbeat")
	if got := r.g.tasks.get("lab", id); got.State != TaskLeased || !got.Lease.ExpiresAt.Before(r.now) {
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
	if got := r.g.tasks.get("lab", id); got.Tries != 1 || got.Interruptions != 1 {
		t.Fatalf("task %+v", got)
	}
}

// A list during a fault tries a failing write once per sweep, not once per chain: each try after a
// failed write reads the whole queue again.
func TestListDuringAFaultStaysCheap(t *testing.T) {
	r := newTaskRig(t, t.TempDir())
	r.create("T1.a-A", "p2m-v7")
	r.create("T2.a-A", "p2m-v7")
	r.create("T3.a-A", "p2m-v7")
	r.create("T4.a-A", "p2m-v7")
	r.claim("w1", 60)
	r.advance(2 * time.Minute) // an expired lease: every sweep tries to write
	tries := 0
	r.g.tasks.write = func(string, []byte) error {
		tries++
		return syscall.ENOSPC
	}
	if w, out := r.call("GET", tasksPath, testProjectKey, nil); w.Code != 200 || len(out["tasks"].([]any)) != 4 {
		t.Fatalf("list: HTTP %d %s", w.Code, w.Body)
	}
	if tries > 2 {
		t.Fatalf("%d tries for one list", tries)
	}
}
