//go:build !windows

// Command aisathome-tray is the node's notification-area icon on Windows; elsewhere the local
// page and `aisathome-node status` do its job.
//
// SPDX-License-Identifier: Apache-2.0
package main

import (
	"fmt"
	"os"
)

func main() {
	fmt.Fprintln(os.Stderr, "aisathome-tray runs only on Windows")
	os.Exit(2)
}
