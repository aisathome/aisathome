//go:build !windows

// SPDX-License-Identifier: Apache-2.0
package node

// RestartCommand restarts the engine by hand when the agent may not (no restart trigger).
const RestartCommand = "sudo systemctl restart aisathome-llama"

// StatusCommand shows whether the agent's service runs, for when the agent does not answer.
const StatusCommand = "systemctl status " + ServiceName

// DefaultLayout is this platform's default files.
func DefaultLayout() Layout { return LinuxLayout }
