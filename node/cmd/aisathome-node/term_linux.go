// SPDX-License-Identifier: Apache-2.0
package main

import (
	"fmt"
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

// readSecretLine reads a line from the terminal on stdin without echoing it (a pasted token). Echo comes
// back afterwards, also when the read is interrupted (Ctrl+C: exit 130).
func readSecretLine() (string, error) {
	fd := int(os.Stdin.Fd())
	old, err := unix.IoctlGetTermios(fd, unix.TCGETS)
	if err != nil {
		return readLine(os.Stdin)
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
			fmt.Fprintln(os.Stderr)
			os.Exit(130)
		case <-done:
		}
	}()
	line, err := readLine(os.Stdin)
	close(done)
	signal.Stop(sig)
	restore()
	fmt.Fprintln(os.Stderr) // the Enter the person typed was not echoed
	return line, err
}
