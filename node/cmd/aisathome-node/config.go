// SPDX-License-Identifier: Apache-2.0
package main

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/aisathome/aisathome/node"
	"github.com/aisathome/aisathome/node/nodeconf"
)

// loadConfig reads the config file, then lets the environment override it, then fills what is
// still empty from the platform's defaults (layout).
func loadConfig(path string, getenv func(string) string, layout node.Layout) (node.Config, error) {
	var cfg node.Config
	b, err := os.ReadFile(path)
	if err != nil {
		return defaults(cfg, layout), fmt.Errorf("reading %s: %w", path, err)
	}
	// One canonical form (nodeconf): keys that are equal in any case are refused, so no reader of this
	// file can see another gateway than this one.
	if err := nodeconf.Parse(b, &cfg); err != nil {
		return defaults(node.Config{}, layout), fmt.Errorf("parsing %s: %w", path, err)
	}
	// The environment wins over the config file, so an installer-provided token is never
	// silently replaced by a token left in an example config; its recorded gateway comes with it.
	if tok := getenv("AISATHOME_NODE_TOKEN"); tok != "" {
		cfg.Token, cfg.TokenGateway = tok, getenv("AISATHOME_TOKEN_GATEWAY")
	}
	// A token goes only to the gateway that issued it.
	if cfg.Token != "" {
		if err := nodeconf.CheckTokenGateway(cfg.TokenGateway, cfg.GatewayURL); err != nil {
			return defaults(cfg, layout), fmt.Errorf("%v: not connecting (link the node again: aisathome-node link)", err)
		}
	}
	// Local UI and state, set by the Linux installer's systemd drop-in (local-ui.conf).
	for env, field := range map[string]*string{"AISATHOME_STATE_FILE": &cfg.StateFile,
		"AISATHOME_RESTART_TRIGGER": &cfg.RestartTrigger, "AISATHOME_PRESET": &cfg.Preset} {
		if v := getenv(env); v != "" {
			*field = v
		}
	}
	// The watchdog (node.Config.Watchdog): a duration such as "10m", or "off".
	switch v := strings.TrimSpace(getenv("AISATHOME_WATCHDOG")); v {
	case "":
	case "off", "0":
		cfg.Watchdog = -1
	default:
		d, err := time.ParseDuration(v)
		if err != nil || d < time.Minute {
			return cfg, fmt.Errorf("AISATHOME_WATCHDOG must be a duration of at least 1m (such as 10m) or \"off\", not %q", v)
		}
		cfg.Watchdog = d
	}
	return defaults(cfg, layout), nil
}

// defaults fills what the layout sets: a Windows service has no drop-in, so its state lives in
// the agent's folder. On Linux the layout has no state file, so the state stays in memory
// unless configured.
func defaults(cfg node.Config, layout node.Layout) node.Config {
	if cfg.StateFile == "" {
		cfg.StateFile = layout.StateFile
	}
	return cfg
}

// passwordFile is the local UI's password file: AISATHOME_UI_PASSWORD_FILE, or the layout's.
func passwordFile(getenv func(string) string, layout node.Layout) string {
	if f := getenv("AISATHOME_UI_PASSWORD_FILE"); f != "" {
		return f
	}
	return layout.PasswordFile
}
