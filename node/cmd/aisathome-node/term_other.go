//go:build !linux

// SPDX-License-Identifier: Apache-2.0
package main

import "os"

// stdinIsTerminal: config preflight is for the Linux installer; elsewhere stdin counts as piped.
func stdinIsTerminal() bool { return false }

// readSecretLine prompts and reads a line from stdin (echoed: config preflight is for the Linux installer).
func readSecretLine(prompt func()) (string, error) {
	prompt()
	return readLine(os.Stdin)
}
