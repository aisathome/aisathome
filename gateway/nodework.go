// What an operator's node is doing, for that operator's account page (owner, 2026-11-13): its
// average speed and the requests it is serving right now.
//
// Speed: the output tokens of a node's last speedWindow answers that ended ok, over the time the
// gateway itself measured for them (from sending the request to its end: prefill and any wait on
// the node included, so it is lower than the engine's decode rate). The tokens are the clamped
// counts that credit uses, so a node cannot inflate its own speed. Kept in memory per node id, so a
// reconnect keeps it and a gateway restart clears it.
//
// Right now: per request in flight, its class and, for a science project, the task. Never its
// content and never who asked: a person's request is "chat" or "API" only. A project's runner may
// name its task in the X-Aisathome-Task header; the name counts only when it is one of the project's
// running tasks on the gateway's queue. Without it, a project with one running task is taken to be
// working on it; with several, the page says how many.
//
// SPDX-License-Identifier: Apache-2.0
package gateway

import (
	"fmt"
	"net/http"
	"sort"
	"sync"
	"time"
)

const (
	// speedWindow: the answers a node's speed is averaged over.
	speedWindow = 50
	// taskHeader: a project's request may name its task (a running task of the project, or ignored).
	taskHeader = "X-Aisathome-Task"
)

// nodeWork is one request in flight on a node (nodeConn.work, keyed by request id).
type nodeWork struct {
	class, project, task string
	since                time.Time
}

// nodeSpeeds keeps each node's last answers.
type nodeSpeeds struct {
	mu sync.Mutex
	m  map[string]*speedRing
}

type speedRing struct {
	tokens [speedWindow]int
	ms     [speedWindow]int64
	n      int // answers recorded, ever
}

// add records an answer of node: its output tokens and duration.
func (s *nodeSpeeds) add(node string, tokens int, ms int64) {
	if node == "" || tokens <= 0 || ms <= 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.m == nil {
		s.m = map[string]*speedRing{}
	}
	r := s.m[node]
	if r == nil {
		r = &speedRing{}
		s.m[node] = r
	}
	i := r.n % speedWindow
	r.tokens[i], r.ms[i] = tokens, ms
	r.n++
}

// get returns a node's output tokens per second over its last answers, and how many answers that
// is (0: none recorded).
func (s *nodeSpeeds) get(node string) (float64, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.m[node]
	if r == nil || r.n == 0 {
		return 0, 0
	}
	k := min(r.n, speedWindow)
	var tok, ms int64
	for i := range k {
		tok += int64(r.tokens[i])
		ms += r.ms[i]
	}
	if ms <= 0 {
		return 0, 0
	}
	return float64(tok) * 1000 / float64(ms), k
}

// requestTask is the task a project's request names in its header, if the name is well formed;
// whether it is one of the project's running tasks is checked when it is shown.
func requestTask(r *http.Request) string {
	if t := r.Header.Get(taskHeader); pubTaskIDRe.MatchString(t) {
		return t
	}
	return ""
}

// workItem is one line of "Working on" on the account page.
type workItem struct {
	Text   string // "Chat request", "prove2me · t5-F01-h03"
	Detail string // the theorem, or how many tasks it may be one of
	Href   string // the project's science page (a leaf's place on it), or ""
}

// sciencePages: the projects whose results the website shows, at /science/<id>/.
var sciencePages = map[string]bool{"prove2me": true, "tauceti": true}

// nodeWorkView says what an account's node is serving now (nil when nothing or when it is not the
// account's), and its speed ("" before any answer).
func (g *Gateway) nodeWorkView(account, id string) (items []workItem, speed string) {
	if tps, k := g.speeds.get(id); k > 0 {
		speed = fmt.Sprintf("%.1f tokens/s (output, last %d %s)", tps, k, map[bool]string{true: "answer", false: "answers"}[k == 1])
	}
	g.mu.Lock()
	n := g.node[id]
	g.mu.Unlock()
	if n == nil || n.account != account {
		return nil, speed
	}
	n.mu.Lock()
	work := make([]nodeWork, 0, len(n.work))
	for _, w := range n.work {
		work = append(work, w)
	}
	n.mu.Unlock()
	sort.Slice(work, func(i, j int) bool { return work[i].since.Before(work[j].since) })
	for _, w := range work {
		switch w.class {
		case ClassChat:
			items = append(items, workItem{Text: "Chat request"})
		case ClassScience:
			items = append(items, g.scienceWork(w))
		default:
			items = append(items, workItem{Text: "API request"})
		}
	}
	return items, speed
}

// scienceWork names a science request's task: the one its header named when that task runs, else
// the project's only running task, else how many it may be one of.
func (g *Gateway) scienceWork(w nodeWork) workItem {
	it := workItem{Text: w.project}
	if sciencePages[w.project] {
		it.Href = "/science/" + w.project + "/"
	}
	if g.tasks == nil {
		return it
	}
	running := g.tasks.leasedRefs(w.project)
	var t *publicTask
	for i := range running {
		if running[i].ID == w.task {
			t = &running[i]
		}
	}
	if t == nil && len(running) == 1 {
		t = &running[0]
	}
	switch {
	case t != nil:
		it.Text += " · " + t.ID
		it.Detail = t.Theorem
		if t.Leaf != "" && it.Href != "" && w.project == "prove2me" {
			it.Href += "#leaf-" + t.Leaf
		}
	case len(running) > 1:
		it.Detail = fmt.Sprintf("one of %d running tasks", len(running))
	}
	return it
}
