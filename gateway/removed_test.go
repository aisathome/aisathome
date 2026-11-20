// SPDX-License-Identifier: Apache-2.0
package gateway

import (
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A config written before trusted nodes were removed (2026-11-20) still starts: every removed field is
// accepted, ignored and named once in the log, also after a reload; a keys file's min_trust too.
func TestRemovedTrustFieldsStart(t *testing.T) {
	dir := t.TempDir()
	h := keyHash(testProjectKey)
	keysFile := filepath.Join(dir, "keys.json")
	if err := os.WriteFile(keysFile, []byte(`{"keys": [{"sha256": "`+keyHash("ak-file-key")+`", "prefix": "ak-file-key", "user": "carol", "min_trust": "trusted"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	raw := `{
		"state_dir": "` + filepath.Join(dir, "state") + `",
		"keys_file": "` + keysFile + `",
		"node_tokens": {"home-token": {"operator": "owner", "trust": "trusted"}, "other-token": {"operator": "op2", "trust": "trusted"}},
		"keys": {"user-key": {"user": "bob", "min_trust": "trusted"}, "any-key": {"user": "dan", "min_trust": "any"}},
		"projects": [{"id": "lab", "key_sha256": ["` + h + `"], "min_trust": "trusted"}],
		"model_check": {"enabled": false, "include_trusted": true},
		"accounts": {"file": "` + filepath.Join(dir, "accounts.json") + `", "public_url": "https://site.test",
			"session_secret": "` + strings.Repeat("s", 32) + `", "github_client_id": "id", "github_client_secret": "secret",
			"min_trust": "trusted"},
		"connectors": [{"id": "boinc", "key_sha256": ["` + keyHash(testConnectorKey) + `"], "max_pilots": 1, "trusted_users": ["1", "not a valid user id!"]}]
	}`
	var cfg Config
	if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
		t.Fatal(err)
	}
	var logs strings.Builder
	g, err := New(cfg, slog.New(slog.NewTextHandler(&logs, nil)))
	if err != nil {
		t.Fatalf("an old config does not start: %v", err)
	}
	defer g.Close()
	if err := g.Reload(cfg); err != nil {
		t.Fatal(err)
	}
	for field, n := range map[string]string{"node_tokens.trust": "2", "keys.min_trust": "1", "keys_file.min_trust": "1",
		"accounts.min_trust": "1", "projects.min_trust": "1", "connectors.trusted_users": "1", "model_check.include_trusted": "1"} {
		line := "field=" + field + " entries=" + n
		if c := strings.Count(logs.String(), line); c != 1 {
			t.Errorf("%s: logged %d times, want once: %s", line, c, logs.String())
		}
	}
	if !strings.Contains(logs.String(), `field=keys.min_trust entries=1 effect="their requests may now go to every node"`) {
		t.Errorf("the min_trust warning does not say what changes: %s", logs.String())
	}
	// Ignored, not kept: the old values change nothing.
	if n := g.tokens["home-token"]; n.LegacyTrust != "trusted" || g.rulesFor("", n).terms {
		t.Errorf("a formerly trusted token: %+v", n)
	}
	if c := g.connector("boinc"); c == nil || !pilotScienceOnly(c, "1", false) {
		t.Error("a former trusted user's pilots serve chat before pilots_chat_enabled")
	}
	rt := g.routeFor("deep", map[string]any{"messages": []any{}}, KeyInfo{User: "bob", LegacyMinTrust: "trusted"}, "", false)
	vol := &nodeConn{id: "vol", operator: "volunteer", rules: nodeRules{terms: true}}
	if !rt.allows(vol, g.start) {
		t.Error("a key's removed min_trust still keeps it off a volunteer's node")
	}
}
