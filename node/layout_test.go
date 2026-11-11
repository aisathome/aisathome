// SPDX-License-Identifier: Apache-2.0
package node

import (
	"runtime"
	"testing"
)

func env(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

// The Windows folder follows %ProgramData%, and never becomes a relative path.
func TestWindowsDataDir(t *testing.T) {
	for _, c := range []struct{ programData, want string }{
		{`C:\ProgramData`, `C:\ProgramData\AIsAtHome`},
		{`D:\Data\`, `D:\Data\AIsAtHome`},
		{` E:\PD `, `E:\PD\AIsAtHome`},
		{`d:/pd`, `d:/pd\AIsAtHome`},
		{`\\server\share`, `\\server\share\AIsAtHome`},
		{"", `C:\ProgramData\AIsAtHome`},
		{"ProgramData", `C:\ProgramData\AIsAtHome`},
		{`\ProgramData`, `C:\ProgramData\AIsAtHome`},
		{`C:ProgramData`, `C:\ProgramData\AIsAtHome`},
		{`C:`, `C:\ProgramData\AIsAtHome`},
		{`\\`, `C:\ProgramData\AIsAtHome`},
		{`1:\x`, `C:\ProgramData\AIsAtHome`},
	} {
		if got := WindowsDataDir(env(map[string]string{"ProgramData": c.programData})); got != c.want {
			t.Errorf("ProgramData=%q: %q, want %q", c.programData, got, c.want)
		}
	}
}

func TestWindowsLayout(t *testing.T) {
	l := WindowsLayout(env(map[string]string{"ProgramData": `C:\ProgramData`}))
	want := Layout{
		Config:         `C:\ProgramData\AIsAtHome\node.json`,
		StateFile:      `C:\ProgramData\AIsAtHome\state.json`,
		PasswordFile:   `C:\ProgramData\AIsAtHome\ui-password`,
		LogFile:        `C:\ProgramData\AIsAtHome\logs\node.log`,
		EngineProgram:  `engine\llama-server.exe`,
		Model:          `C:\ProgramData\AIsAtHome\models\model.gguf`,
		EngineLogFile:  `C:\ProgramData\AIsAtHome\logs\engine.log`,
		Setup:          true,
		SetupStateFile: `C:\ProgramData\AIsAtHome\setup-state.json`,
	}
	if l != want {
		t.Fatalf("layout:\n%+v\nwant\n%+v", l, want)
	}
}

// Linux keeps the defaults it had before the layout existed: the systemd units set the rest.
func TestDefaultLayoutOnThisPlatform(t *testing.T) {
	l := DefaultLayout()
	if runtime.GOOS == "windows" {
		if l.Config == "" || l.StateFile == "" || l.PasswordFile == "" || l.LogFile == "" || !winAbs(l.Config) {
			t.Fatalf("windows layout: %+v", l)
		}
		return
	}
	if l != (Layout{Config: "node.json", PasswordFile: "/etc/aisathome-node/ui-password", EngineProgram: "llama-server"}) {
		t.Fatalf("linux layout: %+v", l)
	}
}
