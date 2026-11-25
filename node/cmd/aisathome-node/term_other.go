//go:build !linux

// SPDX-License-Identifier: Apache-2.0
package main

import "os"

// stdinIsTerminal: config preflight is for the Linux installer; elsewhere stdin counts as piped.
func stdinIsTerminal() bool { return false }

// withoutEcho runs body with plain reads from stdin (echoed: config preflight is for the Linux installer).
func withoutEcho(body func(read func() (string, error)) error) error {
	return body(func() (string, error) { return readLine(os.Stdin) })
}
