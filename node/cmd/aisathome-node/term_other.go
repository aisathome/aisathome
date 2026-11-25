//go:build !linux

// SPDX-License-Identifier: Apache-2.0
package main

import "os"

// stdinIsTerminal: config preflight is for the Linux installer; elsewhere stdin counts as piped.
func stdinIsTerminal() bool { return false }

// readSecretLine reads a line from stdin (echoed: config preflight is for the Linux installer).
func readSecretLine() (string, error) { return readLine(os.Stdin) }
