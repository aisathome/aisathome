//go:build !windows

// SPDX-License-Identifier: Apache-2.0
package main

import "github.com/aisathome/aisathome/node"

var osVersionCommand = []string{"uname", "-a"}

// serviceCommands: the systemd units and the agent's recent journal.
func serviceCommands(node.Layout, string) []diagCommand {
	return []diagCommand{
		{"systemctl", []string{"systemctl", "status", "--no-pager", node.ServiceName, "aisathome-llama"}},
		{"journal", []string{"journalctl", "--no-pager", "-n", "300", "-u", node.ServiceName, "-u", "aisathome-llama"}},
	}
}
