// Where the agent keeps its files on each platform, when neither a flag, the config file nor
// the environment says otherwise.
//
// SPDX-License-Identifier: Apache-2.0
package node

import "strings"

// ServiceName is the name of the agent's service: the systemd unit on Linux, the Windows
// service on Windows.
const ServiceName = "aisathome-node"

// Layout is the agent's default files. Empty fields have no default.
type Layout struct {
	Config       string // the config file (-config)
	StateFile    string // pause switch, counters and events (Config.StateFile)
	PasswordFile string // the local UI's password
	LogFile      string // the agent's log when it runs as a Windows service (no console there)
	// The engine, when the agent runs it (Config.Engine): the llama-server program, relative to
	// the folder of the agent's own program; the model file; and the engine's log as a service.
	EngineProgram string
	Model         string
	EngineLogFile string
	// Setup: the agent runs without a complete config and serves the setup page (/setup) until
	// the volunteer has set it up (package setupflow); SetupStateFile keeps the setup's last
	// state for `aisathome-node diagnose`. Linux: the installer sets the node up instead.
	Setup          bool
	SetupStateFile string
}

// LinuxLayout is the Linux defaults. The installer's systemd units pass the config path and
// set the state file in the environment (local-ui.conf); the password file is
// root:aisathome 0640.
var LinuxLayout = Layout{
	Config:        "node.json",
	PasswordFile:  "/etc/aisathome-node/ui-password",
	EngineProgram: "llama-server",
}

// WindowsDataDir is the agent's folder on Windows: AIsAtHome under %ProgramData%, or under
// C:\ProgramData when the variable is unset or not an absolute path (a service must never
// write relative to its working directory, which is System32).
func WindowsDataDir(getenv func(string) string) string {
	base := strings.TrimSpace(getenv("ProgramData"))
	if !winAbs(base) {
		base = `C:\ProgramData`
	}
	return winJoin(base, "AIsAtHome")
}

// WindowsLayout is the Windows defaults: everything in WindowsDataDir, so one folder holds
// the config, the state and the password, and the installer sets its permissions once.
func WindowsLayout(getenv func(string) string) Layout {
	d := WindowsDataDir(getenv)
	return Layout{
		Config:       winJoin(d, "node.json"),
		StateFile:    winJoin(d, "state.json"),
		PasswordFile: winJoin(d, "ui-password"),
		LogFile:      winJoin(d, "logs", "node.log"),
		// The program in the installer's engine folder beside the agent (Program Files); the
		// model with the data, which the service account may write (the setup downloads it there).
		EngineProgram:  `engine\llama-server.exe`,
		Model:          winJoin(d, "models", "model.gguf"),
		EngineLogFile:  winJoin(d, "logs", "engine.log"),
		Setup:          true,
		SetupStateFile: winJoin(d, "setup-state.json"),
	}
}

// winAbs reports whether p is an absolute Windows path: a drive ("C:\...") or a UNC share
// ("\\server\share"). Written out rather than path/filepath, so it is the same on every
// platform and its tests run on Linux too.
func winAbs(p string) bool {
	if strings.HasPrefix(p, `\\`) && len(p) > 2 {
		return true
	}
	return len(p) >= 3 && isLetter(p[0]) && p[1] == ':' && (p[2] == '\\' || p[2] == '/')
}

func isLetter(c byte) bool { return 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' }

// winJoin joins Windows path elements with backslashes, without doubling one.
func winJoin(base string, elem ...string) string {
	s := strings.TrimRight(base, `\/`)
	for _, e := range elem {
		s += `\` + strings.Trim(e, `\/`)
	}
	return s
}
