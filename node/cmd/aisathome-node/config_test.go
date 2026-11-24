// SPDX-License-Identifier: Apache-2.0
package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aisathome/aisathome/node"
	"github.com/aisathome/aisathome/node/nodeconf"
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
	p := writeConfig(t, `{"node_id":"n1","gateway_url":"wss://gw.example/v1/node/connect","token":"from-file","token_gateway":"wss://gw.example/v1/node/connect","upstream":"http://127.0.0.1:8000","preset":"file-preset"}`)

	// Linux: the file as written; no state file unless one is configured.
	cfg, err := loadConfig(p, env(nil), node.LinuxLayout)
	if err != nil || cfg.NodeID != "n1" || cfg.Token != "from-file" || cfg.StateFile != "" || cfg.Preset != "file-preset" {
		t.Fatalf("linux, no environment: %+v %v", cfg, err)
	}

	// The environment wins over the file.
	cfg, _ = loadConfig(p, env(map[string]string{"AISATHOME_NODE_TOKEN": "from-env", "AISATHOME_TOKEN_GATEWAY": "wss://gw.example/v1/node/connect", "AISATHOME_STATE_FILE": "/var/lib/s.json",
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

// A token goes only to the gateway that issued it (code review 2026-11-23): the agent refuses to start
// when the token's record (token.env's AISATHOME_TOKEN_GATEWAY, or node.json's token_gateway) is another
// gateway than gateway_url; and it refuses a node.json with keys equal in any case.
func TestLoadConfigTokenGateway(t *testing.T) {
	const prod, staging = "wss://api.example.org/v1/node/connect", "wss://staging.example.org/v1/node/connect"
	p := writeConfig(t, `{"gateway_url":"`+staging+`"}`)
	for _, tc := range []struct {
		env map[string]string
		ok  bool
	}{
		{map[string]string{"AISATHOME_NODE_TOKEN": "nt-n-1.x", "AISATHOME_TOKEN_GATEWAY": staging}, true},
		{map[string]string{"AISATHOME_NODE_TOKEN": "nt-n-1.x", "AISATHOME_TOKEN_GATEWAY": prod}, false},
		{map[string]string{"AISATHOME_NODE_TOKEN": "nt-n-1.x"}, false}, // no record: never trusted (code review 2026-11-23)
		{map[string]string{"AISATHOME_NODE_TOKEN": "nt-n-1.x", "AISATHOME_TOKEN_GATEWAY": ""}, false},
	} {
		cfg, err := loadConfig(p, env(tc.env), node.LinuxLayout)
		if (err == nil) != tc.ok {
			t.Errorf("%v: %v, want ok %v", tc.env, err, tc.ok)
		}
		if err != nil && (!strings.Contains(err.Error(), "aisathome-node link") || strings.Contains(err.Error(), "nt-n-1.x")) {
			t.Errorf("message %q", err)
		}
		_ = cfg
	}
	// The environment's token comes with the environment's record, never the file's.
	p2 := writeConfig(t, `{"gateway_url":"`+staging+`","token":"nt-n-2.y","token_gateway":"`+staging+`"}`)
	if _, err := loadConfig(p2, env(map[string]string{"AISATHOME_NODE_TOKEN": "nt-n-1.x", "AISATHOME_TOKEN_GATEWAY": prod}), node.LinuxLayout); err == nil {
		t.Error("an environment token for another gateway borrowed the file's record")
	}
	if _, err := loadConfig(p2, env(nil), node.LinuxLayout); err != nil {
		t.Errorf("inline token with its record: %v", err)
	}
	if _, err := loadConfig(writeConfig(t, `{"gateway_url":"`+staging+`","token":"nt-n-2.y","token_gateway":"`+prod+`"}`), env(nil), node.LinuxLayout); err == nil {
		t.Error("an inline token for another gateway loaded")
	}
	if _, err := loadConfig(writeConfig(t, `{"gateway_url":"`+staging+`","token":"nt-n-2.y"}`), env(nil), node.LinuxLayout); !errors.Is(err, nodeconf.ErrNoTokenGateway) {
		t.Errorf("an inline token without its record: %v", err)
	}
	// A token never goes in plain text across a network: ws:// only to this machine.
	const plain = "ws://gw.example/v1/node/connect"
	if _, err := loadConfig(writeConfig(t, `{"gateway_url":"`+plain+`","token":"nt-n-2.y","token_gateway":"`+plain+`"}`), env(nil), node.LinuxLayout); err == nil ||
		!strings.Contains(err.Error(), "not encrypted") {
		t.Errorf("a token over ws:// to another machine: %v", err)
	}
	// No token (a node still to be linked): nothing to check.
	if _, err := loadConfig(p, env(nil), node.LinuxLayout); err != nil {
		t.Errorf("no token: %v", err)
	}
	if _, err := loadConfig(writeConfig(t, `{"gateway_url":"`+prod+`","Gateway_URL":"`+staging+`"}`), env(nil), node.LinuxLayout); err == nil ||
		!strings.Contains(err.Error(), "more than once") {
		t.Errorf("case-variant keys loaded: %v", err)
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
