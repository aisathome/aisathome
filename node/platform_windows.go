// SPDX-License-Identifier: Apache-2.0
package node

import "os"

// RestartCommand restarts the node by hand when the agent may not restart the engine itself
// (PowerShell, as administrator). On Windows the engine is meant to run under the agent, so
// restarting the service restarts both.
const RestartCommand = "Restart-Service " + ServiceName

// StopCommand, StartCommand and UninstallCommand: on Windows the setup page's Stop engine and
// Windows' own Apps settings do these; the local UI shows no commands for them.
const (
	StopCommand      = ""
	StartCommand     = ""
	UninstallerPath  = ""
	UninstallCommand = ""
)

// StatusCommand shows whether the agent's service runs, for when the agent does not answer.
const StatusCommand = "Get-Service " + ServiceName

// DefaultLayout is this platform's default files.
func DefaultLayout() Layout { return WindowsLayout(os.Getenv) }

// PasswordHint is how the volunteer shows the local UI's password in file: the installing
// user may read it (the installer's permission), in Notepad (Win+R, then this).
func PasswordHint(file string) string { return `notepad "` + file + `"` }
