// SPDX-License-Identifier: Apache-2.0
package node

import "os"

// RestartCommand restarts the node by hand when the agent may not restart the engine itself
// (PowerShell, as administrator). On Windows the engine is meant to run under the agent, so
// restarting the service restarts both.
const RestartCommand = "Restart-Service " + ServiceName

// StatusCommand shows whether the agent's service runs, for when the agent does not answer.
const StatusCommand = "Get-Service " + ServiceName

// DefaultLayout is this platform's default files.
func DefaultLayout() Layout { return WindowsLayout(os.Getenv) }
