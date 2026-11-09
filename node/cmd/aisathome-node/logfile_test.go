// SPDX-License-Identifier: Apache-2.0
package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func read(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// The log appends across restarts and moves to .1 before it would pass its cap.
func TestLogFileRotates(t *testing.T) {
	p := filepath.Join(t.TempDir(), "logs", "node.log") // the folder is created
	l, err := openLog(p, 20)
	if err != nil {
		t.Fatal(err)
	}
	l.Write([]byte("0123456789\n")) // 11 bytes
	l.Close()

	l, err = openLog(p, 20) // a restart appends
	if err != nil {
		t.Fatal(err)
	}
	l.Write([]byte("abcdefgh\n")) // 20 bytes in all: at the cap, not past it
	if got := read(t, p); got != "0123456789\nabcdefgh\n" {
		t.Fatalf("before rotation: %q", got)
	}
	l.Write([]byte("next\n")) // would pass the cap: rotate first
	if got := read(t, p); got != "next\n" {
		t.Fatalf("after rotation: %q", got)
	}
	if got := read(t, p+".1"); got != "0123456789\nabcdefgh\n" {
		t.Fatalf("rotated file: %q", got)
	}
	l.Write([]byte(strings.Repeat("x", 30) + "\n")) // a line over the cap replaces .1
	l.Write([]byte("last\n"))
	l.Close()
	if got := read(t, p+".1"); got != strings.Repeat("x", 30)+"\n" {
		t.Fatalf("second rotation: %q", got)
	}
	if got := read(t, p); got != "last\n" {
		t.Fatalf("after the second rotation: %q", got)
	}
}
