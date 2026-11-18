//go:build !windows

// SPDX-License-Identifier: Apache-2.0
package main

import (
	"archive/tar"
	"encoding/xml"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/ulikunitz/xz"
)

// BOINC's stock wrapper, as far as the pilot's tests need it (samples/wrapper/wrapper.cpp at
// tag wrapper/26019, the same file at server_release/1.6/1.6.3).

// jobTask is boinc/job.xml's one task.
type jobTask struct {
	Application string    `xml:"task>application"`
	CommandLine string    `xml:"task>command_line"`
	Setenv      []string  `xml:"task>setenv"`
	AppendArgs  *struct{} `xml:"task>append_cmdline_args"`
}

func readJob(t *testing.T) jobTask {
	t.Helper()
	var j jobTask
	if err := xml.Unmarshal([]byte(readFile(t, filepath.Join("..", "..", "..", "boinc", "job.xml"))), &j); err != nil {
		t.Fatalf("boinc/job.xml: %v", err)
	}
	return j
}

// wuCommandLine is the work unit's command line as boinc/README.md documents it, for gateway.
func wuCommandLine(t *testing.T, gateway string) string {
	t.Helper()
	m := regexp.MustCompile("A work unit's command line is `([^`]+)`").
		FindStringSubmatch(readFile(t, filepath.Join("..", "..", "..", "boinc", "README.md")))
	if m == nil {
		t.Fatal("boinc/README.md does not give the work unit's command line")
	}
	return strings.ReplaceAll(m[1], "wss://<gateway>/v1/node/connect", gateway)
}

// wrapperChildArgs parses the wrapper's own command line (the work unit's) as main() does: the
// options it knows, everything after --passthrough_child for the task, and anything else refused
// ("Unrecognized option", then exit).
func wrapperChildArgs(argv []string) ([]string, error) {
	var child []string
	pass := false
	for j := 0; j < len(argv); j++ {
		switch a := argv[j]; {
		case pass:
			child = append(child, a)
		case a == "--nthreads" || a == "--device" || a == "--trickle":
			j++
		case a == "--sporadic" || a == "--use_tstp" || a == "--version" || a == "-v":
		case a == "--passthrough_child":
			pass = true
		default:
			return nil, fmt.Errorf("Unrecognized option %s", a)
		}
	}
	return child, nil
}

// taskCommand builds the task from job.xml as the wrapper does: $PROJECT_DIR and
// $GPU_DEVICE_NUM substituted in the command line and the <setenv> entries, the pass-through
// arguments appended (<append_cmdline_args/>), each boinc_resolve(name) replaced by
// resolve(name). It returns the command line the wrapper prints to stderr ("wrapper: running
// ..."), which BOINC shows on the public result page; the task's arguments; and its environment,
// which on Linux is only the <setenv> entries (execve with them alone).
func taskCommand(t *testing.T, child []string, projectDir string, resolve func(string) string) (logged string, args, env []string) {
	t.Helper()
	j := readJob(t)
	if j.Application != "aisathome-node" {
		t.Fatalf("job.xml runs %q", j.Application)
	}
	sub := strings.NewReplacer("$PROJECT_DIR", projectDir, "$GPU_DEVICE_NUM", "0")
	line := sub.Replace(j.CommandLine)
	for _, e := range j.Setenv {
		env = append(env, sub.Replace(e))
	}
	if j.AppendArgs != nil {
		for _, a := range child {
			line += " " + a
		}
	}
	re := regexp.MustCompile(`boinc_resolve\(([^)]*)\)`)
	line = re.ReplaceAllStringFunc(line, func(m string) string { return resolve(re.FindStringSubmatch(m)[1]) })
	return line, strings.Fields(line), env
}

// boincResolve is boinc_resolve_filename in a slot folder: a file whose first line is
// <soft_link>path</soft_link> stands for path; any other name stands for itself.
func boincResolve(slot, name string) string {
	b, err := os.ReadFile(filepath.Join(slot, name))
	if err != nil {
		return name
	}
	line, _, _ := strings.Cut(string(b), "\n")
	if p, ok := strings.CutPrefix(line, "<soft_link>"); ok {
		if p, ok = strings.CutSuffix(p, "</soft_link>"); ok {
			return p
		}
	}
	return name
}

// writeBundle writes a node bundle (.tar.xz) whose bin/llama-server is script.
func writeBundle(t *testing.T, path, script string) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	xw, err := xz.NewWriter(f)
	if err != nil {
		t.Fatal(err)
	}
	tw := tar.NewWriter(xw)
	dir := "aisathome-node-bundle-linux-amd64/"
	for _, h := range []*tar.Header{
		{Name: dir, Typeflag: tar.TypeDir, Mode: 0o755},
		{Name: dir + "bin/llama-server", Typeflag: tar.TypeReg, Mode: 0o755, Size: int64(len(script))},
		{Name: dir + "lib/libggml-cuda.so.0", Typeflag: tar.TypeSymlink, Linkname: "libggml-cuda.so"},
	} {
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if h.Typeflag == tar.TypeReg {
			tw.Write([]byte(script))
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := xw.Close(); err != nil {
		t.Fatal(err)
	}
}

// The work unit's command line as documented gets past the wrapper's own option parsing, and
// job.xml never has the wrapper resolve (and so print) the credential's physical name. The
// task's environment has PATH (the wrapper passes no other variable).
func TestJobFile(t *testing.T) {
	child, err := wrapperChildArgs(strings.Fields(wuCommandLine(t, "wss://gw.example/v1/node/connect")))
	if err != nil {
		t.Fatalf("the wrapper refuses the documented work-unit command line: %v", err)
	}
	if strings.Join(child, " ") != "--gateway wss://gw.example/v1/node/connect" {
		t.Errorf("the task gets %q", child)
	}
	j := readJob(t)
	if j.AppendArgs == nil {
		t.Error("job.xml has no <append_cmdline_args/>: the task would not get --gateway")
	}
	if strings.Contains(j.CommandLine, "boinc_resolve(credential)") {
		t.Error("job.xml resolves the credential: the wrapper prints its physical name on the result page")
	}
	path := false
	for _, e := range j.Setenv {
		path = path || strings.HasPrefix(e, "PATH=/")
	}
	if !path {
		t.Error("job.xml sets no PATH: the task's environment is only <setenv>")
	}
}
