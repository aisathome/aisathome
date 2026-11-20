// The answer checks and signals per node over days, from the usage log (`aisathome-gateway accounts
// node-checks`): what the operations page shows since the start (verify.go), for as long as the log
// goes back. Numbers only: the log holds no prompt or answer.
//
// SPDX-License-Identifier: Apache-2.0
package gateway

import (
	"bufio"
	"encoding/json"
	"io"
	"sort"
	"time"
)

// NodeCheckRow is one node's answers, checks and signals in a span of the usage log.
type NodeCheckRow struct {
	NodeCheckStats
	Credit        float64 // credit granted (after any voiding)
	TPS50, TPS99  float64 // output tok/s of its answers (gateway-timed), median and 99th percentile
	TPSMax        float64
	DistinctTasks int
}

// ReadNodeChecks sums the usage log r per node from since on.
func ReadNodeChecks(r io.Reader, since time.Time) ([]NodeCheckRow, error) {
	type acc struct {
		row   NodeCheckRow
		tps   []float64
		tasks map[string]bool
	}
	by := map[string]*acc{}
	get := func(node string) *acc {
		a := by[node]
		if a == nil {
			a = &acc{row: NodeCheckRow{NodeCheckStats: NodeCheckStats{Node: node, ByRule: map[string]int{}}}, tasks: map[string]bool{}}
			by[node] = a
		}
		return a
	}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 4<<20)
	for sc.Scan() {
		var rec usageRecord
		if json.Unmarshal(sc.Bytes(), &rec) != nil || rec.Node == "" || rec.Time.Before(since) {
			continue
		}
		for _, d := range rec.Discarded { // counted on the node that served the discarded answer
			get(d.Node).row.Discarded++
		}
		a := get(rec.Node)
		if rec.Operator != "" {
			a.row.Operator = rec.Operator
		}
		if rec.Time.After(a.row.Last) {
			a.row.Last = rec.Time
		}
		if !credited(rec.Outcome) {
			continue
		}
		a.row.Answers++
		a.row.Credit += rec.Credit
		a.row.Voided += rec.Voided
		if len(rec.Checks) > 0 {
			a.row.Failed++
			for _, c := range rec.Checks {
				a.row.ByRule[c]++
			}
		}
		if rec.Task != "" {
			a.row.Tasks++
			a.tasks[rec.Task] = true
		}
		if sg := rec.Signals; sg != nil {
			if sg.Finish == "length" {
				a.row.Length++
			}
			a.row.ToolCalls += sg.ToolCalls
			a.row.BadCalls += sg.BadCalls
			if sg.Degenerate {
				a.row.Degenerate++
			}
			if sg.TPS > 0 && rec.Outcome == "ok" {
				a.tps = append(a.tps, sg.TPS)
			}
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	out := make([]NodeCheckRow, 0, len(by))
	for _, a := range by {
		sort.Float64s(a.tps)
		if n := len(a.tps); n > 0 {
			a.row.TPS50, a.row.TPS99, a.row.TPSMax = a.tps[(n-1)/2], a.tps[min(n-1, n*99/100)], a.tps[n-1]
		}
		a.row.DistinctTasks = len(a.tasks)
		out = append(out, a.row)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Failed != out[j].Failed {
			return out[i].Failed > out[j].Failed
		}
		if out[i].Answers != out[j].Answers {
			return out[i].Answers > out[j].Answers
		}
		return out[i].Node < out[j].Node
	})
	return out, nil
}

// Rules is the row's failed rules as "max-tokens 1, speed 2".
func (r NodeCheckRow) Rules() string { return r.rules() }
