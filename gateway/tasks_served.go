// Who served a task: served_by in GET /v1/tasks (tasks_public.go), the nodes that did a task's model
// work, named only as the leaderboard names people.
//
// From the usage log, the gateway's record of every model call: a record of a project's call that
// named a task (X-Aisathome-Task, nodework.go) and ended ok adds one call to that task's node in an
// index kept in memory (per project and task, per node: its operator, as the log names it, and its
// calls). The gateway replays the whole log at start (stats.go replayUsage) and adds each record as it
// writes it (relay.go commit), so the index covers old and new tasks alike, as far back as the log
// names tasks (the harness has sent the header since 2026-11-14), with no second store to keep and
// nothing to backfill. A task the log does not name (made before, or its worker sent no header) has
// no index entry: served_by is null, never a guess.
//
// Public, per task, one row per public name and class of node, most calls first:
//
//	name      the username of the node's account (username.go: setting one is the consent to appear
//	          publicly, as on the leaderboard and in the BOINC exports), else null. Read at every build of
//	          the list, so a cleared username or a ban takes the name off the next build.
//	class     volunteer (an account's node, a node without an account included), pilot (a connector's,
//	          connector.go) or trusted (a config token's, before 2026-11-20): the classes node_classes uses
//	nodes     the nodes of that row
//	requests  their calls that ended ok
//
// The nodes without a public name are one row per class. Never node ids, account ids, pseudonyms,
// operators, addresses or anything about the machines. A record's operator is the account's pseudonym
// (accounts.go) for an account's node and a pilot, a config token's operator otherwise; the pseudonym
// of an account with a username maps to that name, nothing else does (a pseudonym from before a
// change of the session secret maps to none, and its row stays without a name).
//
// Bounds: at most maxServedTasks tasks (the oldest stay; a newer task past it shows null) and
// maxServedNodes nodes per task (the calls of the nodes past it count in an unnamed row of their class,
// as one node).
// A task id a worker named that is not a task of the project is never shown: the public list reads
// the index only for the tasks it lists.
//
// SPDX-License-Identifier: Apache-2.0
package gateway

import (
	"cmp"
	"slices"
	"strings"
	"sync"
)

const (
	// maxServedTasks bounds the index (about 200 bytes a task: years of tasks at today's rate).
	maxServedTasks = 200_000
	// maxServedNodes bounds the nodes kept per task.
	maxServedNodes = 16
	// servedOverflow names, with the class after it, the row of the nodes past maxServedNodes.
	servedOverflow = "~"
)

// servedIndex is the index of who served which task (see the top of the file).
type servedIndex struct {
	mu sync.Mutex
	m  map[liveKey][]servedNode // by project and task
}

// servedNode is one node's calls for a task, as the usage log names the node.
type servedNode struct {
	node, operator, class string
	requests              int64
}

// publicServer is one row of a task's served_by.
type publicServer struct {
	Name     *string `json:"name"`
	Class    string  `json:"class"`
	Nodes    int     `json:"nodes"`
	Requests int64   `json:"requests"`
}

// servedClass is the class of the node a record names: a pilot by its id, an account's node by its
// pseudonym, else a config token's.
func servedClass(r *usageRecord) string {
	switch {
	case validPilotID(r.Node):
		return nodeClassPilot
	case strings.HasPrefix(r.Operator, "acct-"):
		return nodeClassVolunteer
	default:
		return nodeClassTrusted
	}
}

// add counts a usage record: a project's call that named a well-formed task and ended ok.
func (x *servedIndex) add(r *usageRecord) {
	if r.Outcome != "ok" || r.Project == "" || !pubTaskIDRe.MatchString(r.Task) || r.Node == "" {
		return
	}
	cls := servedClass(r)
	k := liveKey{r.Project, r.Task}
	x.mu.Lock()
	defer x.mu.Unlock()
	if x.m == nil {
		x.m = map[liveKey][]servedNode{}
	}
	ns, ok := x.m[k]
	if !ok && len(x.m) >= maxServedTasks {
		return
	}
	node, op := r.Node, r.Operator
	i := slices.IndexFunc(ns, func(s servedNode) bool { return s.node == node })
	if i < 0 && len(ns) >= maxServedNodes {
		node, op = servedOverflow+cls, ""
		i = slices.IndexFunc(ns, func(s servedNode) bool { return s.node == node })
	}
	if i < 0 {
		ns = append(ns, servedNode{node: node, operator: op, class: cls})
		i = len(ns) - 1
	}
	ns[i].requests++
	x.m[k] = ns
}

// rows is a task's served_by: nil when the index has none of its calls. names maps the pseudonyms
// of the accounts with a username to it (Gateway.servedNames).
func (x *servedIndex) rows(project, task string, names map[string]string) []publicServer {
	x.mu.Lock()
	ns := slices.Clone(x.m[liveKey{project, task}])
	x.mu.Unlock()
	if len(ns) == 0 {
		return nil
	}
	type key struct{ name, class string }
	sum := map[key]*publicServer{}
	var out []publicServer
	var order []key
	for _, n := range ns {
		k := key{names[n.operator], n.class}
		p := sum[k]
		if p == nil {
			p = &publicServer{Class: n.class}
			if k.name != "" {
				name := k.name
				p.Name = &name
			}
			sum[k] = p
			order = append(order, k)
		}
		p.Nodes++ // the nodes past maxServedNodes count as one
		p.Requests += n.requests
	}
	for _, k := range order {
		out = append(out, *sum[k])
	}
	// most calls first; then named before unnamed, by name, by class: the same order every time
	slices.SortStableFunc(out, func(a, b publicServer) int {
		return cmp.Or(cmp.Compare(b.Requests, a.Requests), cmp.Compare(nameKey(a.Name), nameKey(b.Name)), cmp.Compare(a.Class, b.Class))
	})
	return out
}

// nameKey orders rows by name, the unnamed last.
func nameKey(n *string) string {
	if n == nil {
		return "\U0010FFFF"
	}
	return strings.ToLower(*n)
}

// servedNames maps the usage log's pseudonym of every account that has a public username to that
// username (username.go publicNames: active accounts that set one); empty without accounts.
func (g *Gateway) servedNames() map[string]string {
	out := map[string]string{}
	for id, pa := range g.publicNames() {
		out[g.pseudonym(id)] = pa.Name
	}
	return out
}
