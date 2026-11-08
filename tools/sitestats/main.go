// SPDX-License-Identifier: Apache-2.0
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: aisathome-sitestats run|report [flags] (see go doc)")
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "run":
		err = runCmd(os.Args[2:])
	case "report":
		err = reportCmd(os.Args[2:], os.Stdout)
	default:
		err = fmt.Errorf("unknown command %q (run or report)", os.Args[1])
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "aisathome-sitestats:", err)
		os.Exit(1)
	}
}

// maxRead caps one run's read; the rest waits for the next run.
const maxRead = 64 << 20

func runCmd(args []string) error {
	fl := flag.NewFlagSet("run", flag.ContinueOnError)
	host := fl.String("host", "", "the website's host name (lines for other hosts are ignored)")
	logPath := fl.String("log", "", "Caddy's JSON access log (on a tmpfs)")
	stateDir := fl.String("state", "", "directory for the state: read offset, today's salt and hashes (a tmpfs)")
	out := fl.String("out", "", "the aggregates file (JSON)")
	if err := fl.Parse(args); err != nil {
		return err
	}
	if *host == "" || *logPath == "" || *stateDir == "" || *out == "" {
		return errors.New("run needs -host, -log, -state and -out")
	}
	if err := os.MkdirAll(*stateDir, 0o700); err != nil {
		return err
	}
	unlock, err := lockFile(filepath.Join(*stateDir, "lock"))
	if err != nil {
		return err
	}
	defer unlock()
	return Run(*host, *logPath, *stateDir, *out, time.Now())
}

// Run reads the new log lines, adds them to the aggregates, saves both files and then frees
// the lines it read from the log.
func Run(host, logPath, stateDir, out string, now time.Time) error {
	statePath := filepath.Join(stateDir, "state.json")
	var stats Stats
	if err := readJSON(out, &stats); err != nil {
		return err
	}
	var state State
	if err := readJSON(statePath, &state); err != nil {
		state = State{} // a damaged state only costs the day's visitor deduplication
	}
	agg := NewAggregator(host, &stats, &state)

	f, err := os.OpenFile(logPath, os.O_RDWR, 0)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	var consumed int64
	if f != nil {
		defer f.Close()
		ino, size, err := fileID(f)
		if err != nil {
			return err
		}
		if ino != state.Inode || size < state.Offset { // a new log file (tmpfs remounted)
			state.Inode, state.Offset = ino, 0
		}
		buf := make([]byte, min(size-state.Offset, maxRead))
		n, err := f.ReadAt(buf, state.Offset)
		if err != nil && !errors.Is(err, io.EOF) {
			return err
		}
		buf = buf[:n]
		if end := bytes.LastIndexByte(buf, '\n'); end >= 0 { // complete lines only
			consumed = int64(end + 1)
			for _, line := range bytes.Split(buf[:end], []byte{'\n'}) {
				if line = bytes.Trim(line, "\x00\r "); len(line) > 0 {
					agg.Add(line)
				}
			}
		}
		clear(buf)
	}
	agg.Finish(now)
	if err := writeJSON(out, &stats, 0o644); err != nil {
		return err
	}
	state.Offset += consumed
	if err := writeJSON(statePath, &state, 0o600); err != nil {
		return err
	}
	if f != nil && state.Offset > 0 {
		return freeRange(f, state.Offset)
	}
	return nil
}

func readJSON(path string, v any) error {
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}

// writeJSON replaces the file atomically (a temporary file in the same directory, renamed).
func writeJSON(path string, v any, mode os.FileMode) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(append(b, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

func reportCmd(args []string, w io.Writer) error {
	fl := flag.NewFlagSet("report", flag.ContinueOnError)
	file := fl.String("file", "/var/lib/aisathome/sitestats/stats.json", "the aggregates file")
	days := fl.Int("days", 7, "number of UTC days, up to today")
	top := fl.Int("top", 15, "rows in the top lists")
	page := fl.String("page", "", "only this page's referring domains (a path, e.g. /science/tauceti/)")
	if err := fl.Parse(args); err != nil {
		return err
	}
	var s Stats
	b, err := os.ReadFile(*file)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	if *page != "" {
		ReportPage(w, &s, *page, *days, *top, time.Now())
		return nil
	}
	Report(w, &s, *days, *top, time.Now())
	return nil
}

// lastDays returns the last days' keys, oldest first, up to now's UTC day.
func lastDays(days int, now time.Time) []string {
	if days < 1 {
		days = 1
	}
	keys := make([]string, 0, days)
	for i := days - 1; i >= 0; i-- {
		keys = append(keys, now.UTC().AddDate(0, 0, -i).Format(time.DateOnly))
	}
	return keys
}

// ReportPage prints one page's views and referring domains, per day and summed over the days.
func ReportPage(w io.Writer, s *Stats, page string, days, top int, now time.Time) {
	if p, ok := normalizePath(page); ok {
		page = p
	}
	keys := lastDays(days, now)
	fmt.Fprintf(w, "Referring domains for %s on %s, last %d UTC days (updated %s UTC)\n\n", page, s.Host, len(keys), s.Updated.UTC().Format("2006-01-02 15:04"))
	fmt.Fprintf(w, "%-10s  %6s  %8s  %s\n", "day", "views", "referred", "top domains")
	refs := map[string]int{}
	views, referred := 0, 0
	for _, key := range keys {
		d := s.Days[key]
		if d == nil {
			d = &Day{}
		}
		n := 0
		for k, v := range d.PageReferrers[page] {
			refs[k] += v
			n += v
		}
		views += d.Pages[page]
		referred += n
		fmt.Fprintf(w, "%-10s  %6d  %8d  %s\n", key, d.Pages[page], n, inline(d.PageReferrers[page], 3))
	}
	fmt.Fprintf(w, "%-10s  %6d  %8d\n", "total", views, referred)
	fmt.Fprintln(w, "(referred: views with a referring domain other than ours; the rest came without one or from our own pages)")
	fmt.Fprintf(w, "\nReferring domains\n")
	printTop(w, refs, top)
}

// inline renders the n largest counts as "a 3, b 2".
func inline(m map[string]int, n int) string {
	var parts []string
	for i, k := range topKeys(m) {
		if i == n {
			parts = append(parts, fmt.Sprintf("+%d more", len(m)-n))
			break
		}
		parts = append(parts, fmt.Sprintf("%s %d", k, m[k]))
	}
	return strings.Join(parts, ", ")
}

func printTop(w io.Writer, m map[string]int, top int) {
	if len(m) == 0 {
		fmt.Fprintln(w, "  (none)")
		return
	}
	for i, k := range topKeys(m) {
		if i == top {
			fmt.Fprintf(w, "  ... %d more\n", len(m)-top)
			break
		}
		fmt.Fprintf(w, "  %6d  %s\n", m[k], k)
	}
}

// Report prints the last days (oldest first) and the top pages, referrers and bots over them.
func Report(w io.Writer, s *Stats, days, top int, now time.Time) {
	keys := lastDays(days, now)
	fmt.Fprintf(w, "Site statistics for %s, last %d UTC days (updated %s UTC)\n\n", s.Host, len(keys), s.Updated.UTC().Format("2006-01-02 15:04"))
	fmt.Fprintf(w, "%-10s  %6s  %8s  %5s  %5s  %5s  %5s\n", "day", "views", "visitors", "feed", "404", "bots", "tests")
	var sum Day
	pages, refs, bots := map[string]int{}, map[string]int{}, map[string]int{}
	pageRefs, referred := map[string]map[string]int{}, map[string]int{}
	for _, key := range keys {
		d := s.Days[key]
		if d == nil {
			d = &Day{}
		}
		nb := 0
		for k, v := range d.Bots {
			nb += v
			bots[k] += v
		}
		for k, v := range d.Pages {
			pages[k] += v
		}
		for k, v := range d.Referrers {
			refs[k] += v
		}
		for p, m := range d.PageReferrers {
			if pageRefs[p] == nil {
				pageRefs[p] = map[string]int{}
			}
			for k, v := range m {
				pageRefs[p][k] += v
				referred[p] += v
			}
		}
		fmt.Fprintf(w, "%-10s  %6d  %8d  %5d  %5d  %5d  %5d\n", key, d.PageViews, d.Visitors, d.Feed, d.NotFound, nb, d.Tests)
		sum.PageViews += d.PageViews
		sum.Visitors += d.Visitors
		sum.Feed += d.Feed
		sum.NotFound += d.NotFound
		sum.Tests += d.Tests
	}
	nbots := 0
	for _, v := range bots {
		nbots += v
	}
	fmt.Fprintf(w, "%-10s  %6d  %8d  %5d  %5d  %5d  %5d\n", "total", sum.PageViews, sum.Visitors, sum.Feed, sum.NotFound, nbots, sum.Tests)
	fmt.Fprintln(w, "(visitors: per day; the total adds the days, a person visiting on two days counts twice)")
	for _, t := range []struct {
		title string
		m     map[string]int
	}{{"Pages", pages}, {"Referring domains", refs}, {"Bots", bots}} {
		fmt.Fprintf(w, "\n%s\n", t.title)
		printTop(w, t.m, top)
	}
	fmt.Fprintf(w, "\nReferring domains by page (views with a referring domain; -page PATH for one page)\n")
	if len(referred) == 0 {
		fmt.Fprintln(w, "  (none)")
	}
	for i, p := range topKeys(referred) {
		if i == top {
			fmt.Fprintf(w, "  ... %d more\n", len(referred)-top)
			break
		}
		fmt.Fprintf(w, "  %6d  %s  %s\n", referred[p], p, inline(pageRefs[p], 5))
	}
}

func topKeys(m map[string]int) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if m[keys[i]] != m[keys[j]] {
			return m[keys[i]] > m[keys[j]]
		}
		return strings.Compare(keys[i], keys[j]) < 0
	})
	return keys
}
