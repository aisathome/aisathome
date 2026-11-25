// SPDX-License-Identifier: Apache-2.0
package main

import (
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"golang.org/x/sys/unix"
)

// stdinIsTerminal reports whether stdin is a terminal (a person can answer).
func stdinIsTerminal() bool {
	_, err := unix.IoctlGetTermios(int(os.Stdin.Fd()), unix.TCGETS)
	return err == nil
}

// readSecretLine reads a line from the terminal on stdin without echoing it (a pasted token, or an answer
// that may be one). prompt runs once echo is off, so nothing typed or pasted right after the question shows.
// Echo comes back afterwards, also when the read is interrupted (Ctrl+C: exit 130).
func readSecretLine(prompt func()) (string, error) {
	return readSecretFrom(os.Stdin, os.Stderr, prompt)
}

// readSecretFrom is readSecretLine for the terminal tty (stdin), with the closing newline on msg. Not a
// terminal: prompt, and a plain read.
func readSecretFrom(tty *os.File, msg io.Writer, prompt func()) (string, error) {
	fd := int(tty.Fd())
	old, err := unix.IoctlGetTermios(fd, unix.TCGETS)
	if err != nil {
		prompt()
		return readLine(tty)
	}
	quiet := *old
	quiet.Lflag &^= unix.ECHO
	quiet.Lflag |= unix.ICANON | unix.ISIG
	if err := unix.IoctlSetTermios(fd, unix.TCSETS, &quiet); err != nil {
		return "", err
	}
	restore := func() { unix.IoctlSetTermios(fd, unix.TCSETS, old) }
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	done := make(chan struct{})
	go func() {
		select {
		case <-sig:
			restore()
			fmt.Fprintln(msg)
			os.Exit(130)
		case <-done:
		}
	}()
	prompt()
	line, err := readLine(tty)
	close(done)
	signal.Stop(sig)
	restore()
	fmt.Fprintln(msg) // the Enter the person typed was not echoed
	return line, err
}
