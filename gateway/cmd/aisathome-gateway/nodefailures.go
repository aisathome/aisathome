// SPDX-License-Identifier: Apache-2.0
package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/aisathome/aisathome/gateway"
)

// nodeFailures prints `accounts node-failures [days] [node]` from the state dir's failure counts
// (gateway/failures.go): per node and model, and per model (the tier), how attempts ended over the
// last days (UTC days, today included), then the refusals before any node. Totals only.
func nodeFailures(out io.Writer, stateDir string, args []string, now time.Time) error {
	days, node := 7, ""
	for _, a := range args {
		if n, err := strconv.Atoi(a); err == nil {
			if n < 1 {
				return errors.New("node-failures: days must be 1 or more")
			}
			days = n
		} else if node == "" {
			node = a
		} else {
			return errors.New("node-failures takes at most a number of days and a node")
		}
	}
	all, err := gateway.ReadFailures(filepath.Join(stateDir, gateway.FailuresFile))
	if errors.Is(err, os.ErrNotExist) {
		return errors.New("no failure counts yet: the gateway saves them in its state dir once a request has ended (a gateway with failures.go)")
	} else if err != nil {
		return err
	}
	since := now.UTC().AddDate(0, 0, -days+1).Format(time.DateOnly)
	type key struct{ node, model string }
	byNode := map[key]*gateway.FailureRow{}
	first := ""
	for _, d := range all {
		if d.Day < since {
			continue
		}
		if first == "" || d.Day < first {
			first = d.Day
		}
		for _, r := range d.Rows {
			if node != "" && r.Node != node {
				continue
			}
			k := key{r.Node, r.Model}
			s := byNode[k]
			if s == nil {
				s = &gateway.FailureRow{Node: r.Node, Model: r.Model, Kinds: map[string]int{}}
				byNode[k] = s
			}
			s.Ctx = max(s.Ctx, r.Ctx)
			for kind, n := range r.Kinds {
				s.Kinds[kind] += n
			}
		}
	}
	fmt.Fprintf(out, "How attempts ended per node and model, the last %d UTC days (from %s; counts only)\n", days, dash(first))
	fmt.Fprintln(out, "The model is the tier: qwen3.8-27b-q3 = 16 GB (81,920 ctx), qwen3.8-27b = 24 GB (196,608 ctx).")
	fmt.Fprintln(out, "ANSWERS reached the client; CUT MAX/CTX: finish_reason length at max_tokens / before it (context full);")
	fmt.Fprintln(out, "CTX EXCEEDED: the engine refused or stopped for context; PARSE: the engine could not parse a tool call;")
	fmt.Fprintln(out, "BAD CALLS: answers with an unusable tool call; OTHER FAILED: the node or engine failed otherwise.")
	fmt.Fprintln(out)

	var nodes, models []*gateway.FailureRow
	byModel := map[string]*gateway.FailureRow{}
	var refused []*gateway.FailureRow
	for _, r := range byNode {
		if r.Node == "-" {
			refused = append(refused, r)
			continue
		}
		nodes = append(nodes, r)
		m := byModel[r.Model]
		if m == nil {
			m = &gateway.FailureRow{Node: "all", Model: r.Model, Kinds: map[string]int{}}
			byModel[r.Model] = m
			models = append(models, m)
		}
		m.Ctx = max(m.Ctx, r.Ctx)
		for kind, n := range r.Kinds {
			m.Kinds[kind] += n
		}
	}
	sort.Slice(nodes, func(i, j int) bool {
		if nodes[i].Model != nodes[j].Model {
			return nodes[i].Model < nodes[j].Model
		}
		return nodes[i].Node < nodes[j].Node
	})
	sort.Slice(models, func(i, j int) bool { return models[i].Model < models[j].Model })
	header := "NODE\tMODEL\tCTX\tATTEMPTS\tANSWERS\tCUT MAX\tCUT CTX\tCTX EXCEEDED\tPARSE\tBAD CALLS\tOTHER FAILED\tCANCELLED/PREEMPTED"
	table := func(title string, rows []*gateway.FailureRow) {
		fmt.Fprintln(out, title)
		if len(rows) == 0 {
			fmt.Fprintln(out, "(none)")
			fmt.Fprintln(out)
			return
		}
		tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, header)
		for _, r := range rows {
			c := summarize(r.Kinds)
			pc := func(n int) string {
				if c.attempts == 0 || n == 0 {
					return strconv.Itoa(n)
				}
				return fmt.Sprintf("%d (%.1f%%)", n, 100*float64(n)/float64(c.attempts))
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\t%d\t%d\t%s\t%s\t%s\t%s\t%s\t%s\t%d/%d\n", r.Node, r.Model, ctxText(r.Ctx), c.attempts,
				c.answers, pc(c.lengthMax), pc(c.lengthCtx), pc(c.ctxExceeded), pc(c.parse), pc(c.badCalls), pc(c.otherFailed),
				c.cancelled, c.preempted)
		}
		tw.Flush()
		fmt.Fprintln(out)
	}
	table("Per node", nodes)
	table("Per model (tier)", models)

	// What the failures were, by model: the categories behind OTHER FAILED too.
	fmt.Fprintln(out, "Failures by category and model")
	cats := map[string]map[string]int{}
	for _, m := range models {
		for kind, n := range m.Kinds {
			if c, ok := strings.CutPrefix(kind, "failed:"); ok {
				if cats[c] == nil {
					cats[c] = map[string]int{}
				}
				cats[c][m.Model] += n
			}
		}
	}
	printCounts(out, "CATEGORY", cats)

	fmt.Fprintln(out, "Refused before any node, by reason and the model asked for (a session's model when it continues one)")
	reasons := map[string]map[string]int{}
	for _, r := range refused {
		for kind, n := range r.Kinds {
			if reason, ok := strings.CutPrefix(kind, "refused:"); ok {
				if reasons[reason] == nil {
					reasons[reason] = map[string]int{}
				}
				reasons[reason][r.Model] += n
			}
		}
	}
	printCounts(out, "REASON", reasons)
	return nil
}

type failureSummary struct {
	attempts, answers, lengthMax, lengthCtx, ctxExceeded, parse, badCalls, otherFailed, cancelled, preempted int
}

// summarize turns a row's kinds (gateway/failures.go) into the table's columns. bad_tool_calls is an
// answer's extra mark, not an attempt of its own.
func summarize(k map[string]int) failureSummary {
	var s failureSummary
	for kind, n := range k {
		switch {
		case kind == "ok":
			s.answers += n
		case kind == "length_max_tokens":
			s.answers += n
			s.lengthMax += n
		case kind == "length_context":
			s.answers += n
			s.lengthCtx += n
		case kind == "bad_tool_calls":
			s.badCalls += n
			continue
		case kind == "failed:context_length_exceeded":
			s.ctxExceeded += n
		case kind == "failed:tool_call_parse":
			s.parse += n
		case strings.HasPrefix(kind, "failed:"):
			s.otherFailed += n
		case kind == "cancelled":
			s.cancelled += n
		case kind == "preempted":
			s.preempted += n
		case strings.HasPrefix(kind, "refused:"):
			continue
		}
		s.attempts += n
	}
	return s
}

func ctxText(ctx int) string {
	if ctx == 0 {
		return "-"
	}
	return strconv.Itoa(ctx)
}

// printCounts prints counts[first][model] as a table, one row per first key and model.
func printCounts(out io.Writer, first string, counts map[string]map[string]int) {
	if len(counts) == 0 {
		fmt.Fprintln(out, "(none)")
		fmt.Fprintln(out)
		return
	}
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintf(tw, "%s\tMODEL\tCOUNT\n", first)
	keys := make([]string, 0, len(counts))
	for k := range counts {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		ms := make([]string, 0, len(counts[k]))
		for m := range counts[k] {
			ms = append(ms, m)
		}
		sort.Strings(ms)
		for _, m := range ms {
			fmt.Fprintf(tw, "%s\t%s\t%d\n", k, m, counts[k][m])
		}
	}
	tw.Flush()
	fmt.Fprintln(out)
}
