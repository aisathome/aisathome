// Config fields removed with trusted nodes: every node is untrusted. The one
// exception, a requester's own nodes, which may receive that requester's personal data, is no level of
// trust: it is one's own data on one's own machine (routing.go). A config that still sets a removed
// field starts: the field is accepted, ignored, and named once in the log.
//
//	node_tokens.*.trust          a config token's node is a node like any other
//	keys.*.min_trust,            a key, account or project restricted to trusted nodes; such requests
//	keys file min_trust,         may now go to every node, like everyone's (only_operator and an
//	accounts.min_trust,          account's "only my own nodes" still restrict them)
//	projects[].min_trust
//	connectors[].trusted_users   their pilots serve chat only once pilots_chat_enabled is on, as all
//	model_check.include_trusted  the model check checks every node
//
// SPDX-License-Identifier: Apache-2.0
package gateway

import (
	"sort"
	"strings"
)

// removedFields counts, per removed field, the entries of cfg and of its keys file (fileKeys) that set
// it. min_trust counts only "trusted", the one value that restricted anything.
func removedFields(cfg Config, fileKeys map[string]KeyInfo) map[string]int {
	out := map[string]int{}
	add := func(field string, set bool) {
		if set {
			out[field]++
		}
	}
	for _, t := range cfg.NodeTokens {
		add("node_tokens.trust", t.LegacyTrust != "")
	}
	for _, k := range cfg.Keys {
		add("keys.min_trust", k.LegacyMinTrust == "trusted")
	}
	for _, k := range fileKeys {
		add("keys_file.min_trust", k.LegacyMinTrust == "trusted")
	}
	if cfg.Accounts != nil {
		add("accounts.min_trust", cfg.Accounts.LegacyMinTrust == "trusted")
	}
	for _, p := range cfg.Projects {
		add("projects.min_trust", p.LegacyMinTrust == "trusted")
	}
	for _, c := range cfg.Connectors {
		add("connectors.trusted_users", len(c.LegacyTrustedUsers) > 0)
	}
	if cfg.ModelCheck != nil {
		add("model_check.include_trusted", cfg.ModelCheck.LegacyIncludeTrusted)
	}
	return out
}

// warnRemoved logs each removed field once per process, with how many entries set it. Caller holds
// g.keysMu (loadKeysLocked).
func (g *Gateway) warnRemoved(fields map[string]int) {
	if g.log == nil {
		return
	}
	names := make([]string, 0, len(fields))
	for f := range fields {
		names = append(names, f)
	}
	sort.Strings(names)
	for _, f := range names {
		if g.removedWarned[f] {
			continue
		}
		if g.removedWarned == nil {
			g.removedWarned = map[string]bool{}
		}
		g.removedWarned[f] = true
		args := []any{"field", f, "entries", fields[f]}
		if strings.HasSuffix(f, ".min_trust") {
			args = append(args, "effect", "their requests may now go to every node")
		}
		g.log.Warn("removed config field ignored: there are no trusted nodes any more, every node is untrusted "+
			"(docs/gateway.md, \"Volunteer nodes\"); remove it from the config", args...)
	}
}
