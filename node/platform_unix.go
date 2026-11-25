//go:build !windows

// SPDX-License-Identifier: Apache-2.0
package node

// RestartCommand restarts the engine by hand when the agent may not (no restart trigger).
const RestartCommand = "sudo systemctl restart aisathome-llama"

// StopCommand stops the node and frees the GPU by hand; StartCommand starts it again.
const (
	StopCommand  = "sudo systemctl stop " + ServiceName + " aisathome-llama"
	StartCommand = "sudo systemctl start aisathome-llama " + ServiceName
)

// UninstallerPath is the Linux installer's permanent copy of uninstall.sh; UninstallCommand runs it
// (the model stays unless --purge).
const (
	UninstallerPath  = "/usr/local/sbin/aisathome-node-uninstall"
	UninstallCommand = "sudo aisathome-node-uninstall"
)

// StatusCommand shows whether the agent's service runs, for when the agent does not answer.
const StatusCommand = "systemctl status " + ServiceName

// DefaultLayout is this platform's default files.
func DefaultLayout() Layout { return LinuxLayout }

// PasswordHint is how the volunteer shows the local UI's password in file (root:aisathome 0640).
func PasswordHint(file string) string { return "sudo cat " + file }
