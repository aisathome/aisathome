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

// withoutEcho runs body with the terminal on stdin not echoing (a pasted token, or an answer that may be
// one); read reads one line. Echo is off before body prints anything and stays off between its reads, so
// nothing typed or pasted after any of its lines shows. Echo comes back once body returns, also when the
// read is interrupted (Ctrl+C: exit 130).
func withoutEcho(body func(read func() (string, error)) error) error {
	return withoutEchoOn(os.Stdin, os.Stderr, body)
}

// withoutEchoOn is withoutEcho for the terminal tty, with the newline for each unechoed Enter on msg. Not
// a terminal: body with plain reads.
func withoutEchoOn(tty *os.File, msg io.Writer, body func(read func() (string, error)) error) error {
	fd := int(tty.Fd())
	old, err := unix.IoctlGetTermios(fd, unix.TCGETS)
	if err != nil {
		return body(func() (string, error) { return readLine(tty) })
	}
	quiet := *old
	quiet.Lflag &^= unix.ECHO
	quiet.Lflag |= unix.ICANON | unix.ISIG
	if err := unix.IoctlSetTermios(fd, unix.TCSETS, &quiet); err != nil {
		return err
	}
	restore := func() { unix.IoctlSetTermios(fd, unix.TCSETS, old) }
	defer restore()
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer signal.Stop(sig)
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-sig:
			restore()
			fmt.Fprintln(msg)
			os.Exit(130)
		case <-done:
		}
	}()
	return body(func() (string, error) {
		line, err := readLine(tty)
		fmt.Fprintln(msg) // the Enter the person typed was not echoed
		return line, err
	})
}
