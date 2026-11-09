// SPDX-License-Identifier: Apache-2.0
package main

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/aisathome/aisathome/node"
)

// loadConfig reads the config file, then lets the environment override it, then fills what is
// still empty from the platform's defaults (layout).
func loadConfig(path string, getenv func(string) string, layout node.Layout) (node.Config, error) {
	var cfg node.Config
	b, err := os.ReadFile(path)
	if err != nil {
		return cfg, fmt.Errorf("reading %s: %w", path, err)
	}
	if err := json.Unmarshal(b, &cfg); err != nil {
		return cfg, fmt.Errorf("parsing %s: %w", path, err)
	}
	// The environment wins over the config file, so an installer-provided token is never
	// silently replaced by a token left in an example config.
	if tok := getenv("AISATHOME_NODE_TOKEN"); tok != "" {
		cfg.Token = tok
	}
	// Local UI and state, set by the Linux installer's systemd drop-in (local-ui.conf).
	for env, field := range map[string]*string{"AISATHOME_STATE_FILE": &cfg.StateFile,
		"AISATHOME_RESTART_TRIGGER": &cfg.RestartTrigger, "AISATHOME_PRESET": &cfg.Preset} {
		if v := getenv(env); v != "" {
			*field = v
		}
	}
	// A Windows service has no drop-in: its state lives in the agent's folder. On Linux the
	// layout has no state file, so the state stays in memory unless configured.
	if cfg.StateFile == "" {
		cfg.StateFile = layout.StateFile
	}
	return cfg, nil
}

// passwordFile is the local UI's password file: AISATHOME_UI_PASSWORD_FILE, or the layout's.
func passwordFile(getenv func(string) string, layout node.Layout) string {
	if f := getenv("AISATHOME_UI_PASSWORD_FILE"); f != "" {
		return f
	}
	return layout.PasswordFile
}
