// SPDX-License-Identifier: Apache-2.0
package gateway

import (
	"bufio"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

type surveyCall struct {
	run, tool, args string
	req             string // the conversation so far, as requestText gives it
}

// surveyRuns calls f for each tool call of each recorded opencode run under dir, and with the
// text of each read tool's output (Lean files, as a model might write them back).
func surveyRuns(t *testing.T, dir string, f func(surveyCall), file func(run, text string)) (runs int) {
	filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || !d.IsDir() {
			return nil
		}
		run, _ := filepath.Rel(dir, path)
		for _, name := range []string{"session.json.gz", "session.json", "events.jsonl.gz", "events.jsonl"} {
			fp := filepath.Join(path, name)
			if _, err := os.Stat(fp); err != nil {
				continue
			}
			fh, err := os.Open(fp)
			if err != nil {
				t.Error(err)
				return nil
			}
			defer fh.Close()
			var r io.Reader = fh
			if strings.HasSuffix(name, ".gz") {
				if r, err = gzip.NewReader(fh); err != nil {
					t.Error(err)
					return nil
				}
			}
			runs++
			var req strings.Builder
			note := func(s string) { req.WriteString(strings.ToLower(s) + "\n") }
			call := func(tool string, input json.RawMessage, output string) {
				if len(input) > 0 {
					f(surveyCall{run, tool, string(input), req.String()})
					note(string(input))
				}
				note(output)
				if tool == "read" {
					file(run, output)
				}
			}
			if strings.HasPrefix(name, "session") {
				surveySession(t, run, r, note, call)
			} else {
				for _, m := range []string{"prompt.md", "followup*.md"} {
					ms, _ := filepath.Glob(filepath.Join(path, m))
					for _, m := range ms {
						b, _ := os.ReadFile(m)
						note(string(b))
					}
				}
				surveyEvents(t, run, r, note, call)
			}
			return nil
		}
		return nil
	})
	return runs
}

func surveySession(t *testing.T, run string, r io.Reader, note func(string), call func(string, json.RawMessage, string)) {
	var s struct {
		Messages []struct {
			Text    string          `json:"text"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if err := json.NewDecoder(r).Decode(&s); err != nil {
		t.Errorf("%s: %v", run, err)
		return
	}
	for _, m := range s.Messages {
		note(m.Text)
		var blocks []struct {
			Type, Text, Name string
			State            struct {
				Input   json.RawMessage `json:"input"`
				Content json.RawMessage `json:"content"`
			} `json:"state"`
		}
		json.Unmarshal(m.Content, &blocks)
		for _, b := range blocks {
			switch b.Type {
			case "text":
				note(b.Text)
			case "tool":
				var parts []struct{ Text string }
				var out strings.Builder
				if json.Unmarshal(b.State.Content, &parts) == nil {
					for _, p := range parts {
						out.WriteString(p.Text + "\n")
					}
				}
				call(b.Name, b.State.Input, out.String())
			}
		}
	}
}

func surveyEvents(t *testing.T, run string, r io.Reader, note func(string), call func(string, json.RawMessage, string)) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 1<<20), 1<<28)
	for sc.Scan() {
		var e struct {
			Type string `json:"type"`
			Part struct {
				Tool  string `json:"tool"`
				Text  string `json:"text"`
				State struct {
					Input  json.RawMessage `json:"input"`
					Output string          `json:"output"`
				} `json:"state"`
			} `json:"part"`
		}
		if json.Unmarshal(sc.Bytes(), &e) != nil {
			continue
		}
		note(e.Part.Text)
		if e.Type == "tool_use" {
			call(e.Part.Tool, e.Part.State.Input, e.Part.State.Output)
		}
	}
	if err := sc.Err(); err != nil {
		t.Errorf("%s: %v", run, err)
	}
}

// surveyWords counts the command words of the rules, whichever way a view shows them.
var surveyWords = regexp.MustCompile(`\b(?:nc|ncat|netcat|socat|scp|rsync|curl|wget|base64|iex|powershell|pwsh)\b`)

// TestGuardSurvey replays recorded agent sessions through the guard: set GUARD_SURVEY_DIR to a
// directory holding opencode runs (session.json or events.jsonl, gzipped or not). Each tool call is
// checked against the conversation before it, as a request would carry it. It reports matches
// per rule and tool, without the calls' text; GUARD_SURVEY_OUT writes the summary as JSON and
// GUARD_SURVEY_HITS the matches, clipped, for reading by hand.
func TestGuardSurvey(t *testing.T) {
	dir := os.Getenv("GUARD_SURVEY_DIR")
	if dir == "" {
		t.Skip("GUARD_SURVEY_DIR is not set")
	}
	type hit struct {
		Run, Rule, Tool, View, Fragment string
		Pattern                         int
	}
	counts, tools, words, stopped := map[string]int{}, map[string]int{}, map[string]int{}, map[string]int{}
	var hits []hit
	calls, fileChars, files := 0, 0, 0
	match := func(run, tool, view, text, req string) {
		for i, r := range guardRules {
			for _, m := range r.re.FindAllString(text, -1) {
				if r.rule == ruleCredentials && strings.Contains(req, strings.ToLower(strings.Trim(m, " \t\n\"'=:(/~);,"))) {
					continue
				}
				counts[fmt.Sprintf("%-22s pattern %2d %-16s %s", r.rule, i, view, tool)]++
				hits = append(hits, hit{run, r.rule, tool, view, surveyClip(m), i})
				break
			}
		}
		if h := unknownHost(text, req); h != "" {
			counts[fmt.Sprintf("%-22s            %-16s %s", ruleUnknownURL, view, tool)]++
			hits = append(hits, hit{run, ruleUnknownURL, tool, view, h, -1})
		}
		for _, w := range surveyWords.FindAllString(text, -1) {
			words[view+" "+w]++
		}
	}
	runs := surveyRuns(t, dir, func(c surveyCall) {
		calls++
		tools[c.tool]++
		views := surveyViews(c.tool, c.args)
		match(c.run, c.tool, "raw", views["raw"], c.req)
		match(c.run, c.tool, "decoded", views["decoded"], c.req)
		if r := checkToolCall(c.tool, c.args, c.req).rule; r != "" {
			stopped[r+" ("+c.tool+")"]++
		}
	}, func(run, text string) {
		files++
		fileChars += len(text)
		match(run, "read-output", "as-write", text, "")
	})
	var lines []string
	for k, n := range counts {
		lines = append(lines, fmt.Sprintf("%s: %d", k, n))
	}
	sort.Strings(lines)
	t.Logf("%d runs, %d tool calls %v; %d read outputs, %d bytes", runs, calls, tools, files, fileChars)
	t.Logf("matches:\n%s", strings.Join(lines, "\n"))
	t.Logf("words: %v", words)
	t.Logf("calls checkToolCall stops: %v", stopped)
	if out := os.Getenv("GUARD_SURVEY_OUT"); out != "" {
		b, _ := json.MarshalIndent(map[string]any{"runs": runs, "calls": calls, "tools": tools, "stopped": stopped,
			"matches": lines, "words": words, "read_outputs": files, "read_output_bytes": fileChars}, "", " ")
		if err := os.WriteFile(out, b, 0o600); err != nil {
			t.Error(err)
		}
	}
	if out := os.Getenv("GUARD_SURVEY_HITS"); out != "" {
		b, _ := json.MarshalIndent(hits, "", " ")
		if err := os.WriteFile(out, b, 0o600); err != nil {
			t.Error(err)
		}
	}
}

// surveyViews is the two texts the old check read: the raw JSON and the decoded string values.
func surveyViews(name, args string) map[string]string {
	var dec strings.Builder
	var v any
	if json.Unmarshal([]byte(args), &v) == nil {
		var walk func(any)
		walk = func(x any) {
			switch y := x.(type) {
			case string:
				dec.WriteString(y + "\n")
			case []any:
				for _, e := range y {
					walk(e)
				}
			case map[string]any:
				for k, e := range y {
					dec.WriteString(k + "\n")
					walk(e)
				}
			}
		}
		walk(v)
	}
	return map[string]string{"raw": name + "\n" + args, "decoded": name + "\n" + dec.String()}
}

// surveyClip keeps a match short and on one line, without anything that looks like a secret.
func surveyClip(m string) string {
	for _, p := range secretPatterns {
		m = p.ReplaceAllString(m, "[secret]")
	}
	if len(m) > 90 {
		m = m[:45] + " ... " + m[len(m)-40:]
	}
	return strings.ToValidUTF8(strings.ReplaceAll(m, "\n", "\\n"), "?")
}
