// SPDX-License-Identifier: Apache-2.0
package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/aisathome/aisathome/node"
)

func env(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "node.json")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

var winLayout = node.WindowsLayout(env(map[string]string{"ProgramData": `C:\ProgramData`}))

func TestLoadConfig(t *testing.T) {
	p := writeConfig(t, `{"node_id":"n1","token":"from-file","upstream":"http://127.0.0.1:8000","preset":"file-preset"}`)

	// Linux: the file as written; no state file unless one is configured.
	cfg, err := loadConfig(p, env(nil), node.LinuxLayout)
	if err != nil || cfg.NodeID != "n1" || cfg.Token != "from-file" || cfg.StateFile != "" || cfg.Preset != "file-preset" {
		t.Fatalf("linux, no environment: %+v %v", cfg, err)
	}

	// The environment wins over the file.
	cfg, _ = loadConfig(p, env(map[string]string{"AISATHOME_NODE_TOKEN": "from-env", "AISATHOME_STATE_FILE": "/var/lib/s.json",
		"AISATHOME_RESTART_TRIGGER": "/run/t", "AISATHOME_PRESET": "env-preset"}), node.LinuxLayout)
	if cfg.Token != "from-env" || cfg.StateFile != "/var/lib/s.json" || cfg.RestartTrigger != "/run/t" || cfg.Preset != "env-preset" {
		t.Fatalf("environment: %+v", cfg)
	}

	// Windows: the state goes to the agent's folder unless the config or environment says.
	cfg, _ = loadConfig(p, env(nil), winLayout)
	if cfg.StateFile != `C:\ProgramData\AIsAtHome\state.json` {
		t.Fatalf("windows default state file: %q", cfg.StateFile)
	}
	cfg, _ = loadConfig(writeConfig(t, `{"state_file":"D:\\node\\state.json"}`), env(nil), winLayout)
	if cfg.StateFile != `D:\node\state.json` {
		t.Fatalf("windows configured state file: %q", cfg.StateFile)
	}
	cfg, _ = loadConfig(p, env(map[string]string{"AISATHOME_STATE_FILE": `E:\s.json`}), winLayout)
	if cfg.StateFile != `E:\s.json` {
		t.Fatalf("windows state file from the environment: %q", cfg.StateFile)
	}

	if _, err := loadConfig(filepath.Join(t.TempDir(), "missing.json"), env(nil), node.LinuxLayout); err == nil {
		t.Fatal("a missing config loaded")
	}
	if _, err := loadConfig(writeConfig(t, `{"node_id":`), env(nil), node.LinuxLayout); err == nil {
		t.Fatal("a broken config loaded")
	}
}

// AISATHOME_WATCHDOG sets the watchdog: unset keeps the default, "off" turns it off, and a
// limit short enough to catch a normal reconnect is refused.
func TestWatchdogFromEnvironment(t *testing.T) {
	p := writeConfig(t, `{"node_id":"n1"}`)
	for v, want := range map[string]time.Duration{"": 0, "off": -1, "0": -1, "10m": 10 * time.Minute, " 2m ": 2 * time.Minute} {
		cfg, err := loadConfig(p, env(map[string]string{"AISATHOME_WATCHDOG": v}), node.LinuxLayout)
		if err != nil || cfg.Watchdog != want {
			t.Errorf("AISATHOME_WATCHDOG=%q: %v, %v; want %v", v, cfg.Watchdog, err, want)
		}
	}
	for _, v := range []string{"30s", "-5m", "soon"} {
		if _, err := loadConfig(p, env(map[string]string{"AISATHOME_WATCHDOG": v}), node.LinuxLayout); err == nil {
			t.Errorf("AISATHOME_WATCHDOG=%q accepted", v)
		}
	}
}

func TestPasswordFile(t *testing.T) {
	if f := passwordFile(env(nil), node.LinuxLayout); f != "/etc/aisathome-node/ui-password" {
		t.Fatalf("linux default: %q", f)
	}
	if f := passwordFile(env(nil), winLayout); f != `C:\ProgramData\AIsAtHome\ui-password` {
		t.Fatalf("windows default: %q", f)
	}
	if f := passwordFile(env(map[string]string{"AISATHOME_UI_PASSWORD_FILE": "/tmp/pw"}), winLayout); f != "/tmp/pw" {
		t.Fatalf("environment: %q", f)
	}
}
