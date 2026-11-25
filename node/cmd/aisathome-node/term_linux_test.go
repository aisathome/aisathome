// SPDX-License-Identifier: Apache-2.0
package main

import (
	"bytes"
	"fmt"
	"os"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

// openPTY opens a pseudo-terminal: the master (the person's side) and the terminal the node reads.
func openPTY(t *testing.T) (master, tty *os.File) {
	t.Helper()
	m, err := os.OpenFile("/dev/ptmx", os.O_RDWR|unix.O_NOCTTY, 0)
	if err != nil {
		t.Skipf("no pseudo-terminal: %v", err)
	}
	t.Cleanup(func() { m.Close() })
	if err := unix.IoctlSetPointerInt(int(m.Fd()), unix.TIOCSPTLCK, 0); err != nil {
		t.Skipf("unlockpt: %v", err)
	}
	n, err := unix.IoctlGetInt(int(m.Fd()), unix.TIOCGPTN)
	if err != nil {
		t.Skipf("ptsname: %v", err)
	}
	s, err := os.OpenFile(fmt.Sprintf("/dev/pts/%d", n), os.O_RDWR|unix.O_NOCTTY, 0)
	if err != nil {
		t.Skipf("open pts: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return m, s
}

// Tokens pasted right after the question, and right after its "Answer y or n.", never show on the terminal:
// echo is off before the first line is printed and stays off through every try (each line is printed while
// it is off), and it is back afterwards.
func TestAskLinkNoEcho(t *testing.T) {
	master, tty := openPTY(t)
	tok1 := "nt-a-0000000d." + strings.Repeat("Q", 43)
	tok2 := "nt-n-0000000e." + strings.Repeat("N", 43)
	echoing := func() bool {
		tm, err := unix.IoctlGetTermios(int(tty.Fd()), unix.TCGETS)
		return err != nil || tm.Lflag&unix.ECHO != 0
	}
	var msg bytes.Buffer
	questions, retries := 0, 0
	say := func(f string, a ...any) {
		line := fmt.Sprintf(f, a...)
		if echoing() {
			t.Errorf("printed while the terminal echoes: %q", line)
		}
		msg.WriteString(line + "\n")
		switch {
		case strings.Contains(line, "[Y/n]"):
			if questions++; questions == 1 {
				master.Write([]byte(tok1 + "\n")) // pasted at once
			}
		case line == "Answer y or n.":
			if retries++; retries == 1 {
				master.Write([]byte(tok2 + "\n")) // pasted right after the retry line
			} else {
				master.Write([]byte("n\n"))
			}
		}
	}
	e := preflightEnv{hidden: func(body func(read func() (string, error)) error) error {
		return withoutEchoOn(tty, &msg, body)
	}}
	var r preflightResult
	if err := e.askLink(&r, say); err != nil || r.Account != "anonymous" || questions != 3 || retries != 2 {
		t.Fatalf("askLink: %v, account %q after %d questions, %d retries: %s", err, r.Account, questions, retries, msg.String())
	}
	if !echoing() {
		t.Error("echo is not back after the question")
	}
	if strings.Count(msg.String(), "It was not stored, and it was not shown") != 2 {
		t.Errorf("not told twice that the token was dropped: %s", msg.String())
	}
	// Everything the terminal showed before this marker is what it echoed.
	tty.Write([]byte("END\n"))
	var screen []byte
	buf := make([]byte, 256)
	for !bytes.Contains(screen, []byte("END")) {
		n, err := master.Read(buf)
		if err != nil {
			t.Fatalf("read the terminal: %v (%q)", err, screen)
		}
		screen = append(screen, buf[:n]...)
	}
	if bytes.Contains(screen, []byte("QQQQ")) || bytes.Contains(screen, []byte("NNNN")) || bytes.Contains(screen, []byte("nt-")) {
		t.Errorf("a token was echoed: %q", screen)
	}
}
