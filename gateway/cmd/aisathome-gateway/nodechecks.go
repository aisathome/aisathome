// SPDX-License-Identifier: Apache-2.0
package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"text/tabwriter"
	"time"

	"github.com/aisathome/aisathome/gateway"
)

// nodeChecks prints `accounts node-checks [days] [node]` from the usage log at path.
func nodeChecks(out io.Writer, path string, args []string, now time.Time) error {
	days, node := 7, ""
	for _, a := range args {
		if n, err := strconv.Atoi(a); err == nil {
			if n < 1 {
				return errors.New("node-checks: days must be 1 or more")
			}
			days = n
		} else if node == "" {
			node = a
		} else {
			return errors.New("node-checks takes at most a number of days and a node")
		}
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	rows, err := gateway.ReadNodeChecks(f, now.Add(-time.Duration(days)*24*time.Hour))
	if err != nil {
		return err
	}
	pc := func(n, all int) string {
		if all == 0 {
			return "-"
		}
		return fmt.Sprintf("%d (%.1f%%)", n, 100*float64(n)/float64(all))
	}
	fmt.Fprintf(out, "Answer checks and signals per node, last %d days (answers that could earn credit)\n\n", days)
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "NODE\tANSWERS\tFAILED\tRULES\tVOIDED\tCREDIT\tTOK/S p50/p99/max\tAT MAX_TOKENS\tBAD TOOL CALLS\tDISCARDED\tDEGENERATE\tTASKS")
	shown := 0
	for _, r := range rows {
		if node != "" && r.Node != node {
			continue
		}
		shown++
		fmt.Fprintf(tw, "%s\t%d\t%s\t%s\t%.1f\t%.1f\t%.1f/%.1f/%.1f\t%s\t%s\t%s\t%s\t%d\n", r.Node, r.Answers, pc(r.Failed, r.Answers),
			dash(r.Rules()), r.Voided, r.Credit, r.TPS50, r.TPS99, r.TPSMax, pc(r.Length, r.Answers),
			pc(r.BadCalls, r.ToolCalls), pc(r.Discarded, r.Answers), pc(r.Degenerate, r.Answers), r.DistinctTasks)
	}
	tw.Flush()
	if shown == 0 {
		fmt.Fprintln(out, "(no answers)")
	}
	return nil
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
