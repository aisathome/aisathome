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

// A token pasted right after the question never shows on the terminal: echo is off before the question
// is printed, the line is read whole, and echo is back afterwards.
func TestReadSecretFromNoEcho(t *testing.T) {
	master, tty := openPTY(t)
	tok := "nt-a-0000000d." + strings.Repeat("Q", 43)
	var msg bytes.Buffer
	line, err := readSecretFrom(tty, &msg, func() {
		tm, err := unix.IoctlGetTermios(int(tty.Fd()), unix.TCGETS)
		if err != nil || tm.Lflag&unix.ECHO != 0 {
			t.Errorf("the question is printed while the terminal echoes (%v)", err)
		}
		msg.WriteString("Link this node to your account? [Y/n]\n")
		master.Write([]byte(tok + "\n")) // pasted at once
	})
	if err != nil || line != tok {
		t.Fatalf("read %q, %v", line, err)
	}
	if tm, err := unix.IoctlGetTermios(int(tty.Fd()), unix.TCGETS); err != nil || tm.Lflag&unix.ECHO == 0 {
		t.Errorf("echo is not back after the read (%v)", err)
	}
	if !strings.HasSuffix(msg.String(), "[Y/n]\n\n") {
		t.Errorf("no newline after the unechoed Enter: %q", msg.String())
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
	if bytes.Contains(screen, []byte("QQQQ")) || bytes.Contains(screen, []byte("nt-a")) {
		t.Errorf("the token was echoed: %q", screen)
	}
}
