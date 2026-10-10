// Public statistics (GET /v1/stats): aggregates only, no authentication, survive restarts.
//
// SPDX-License-Identifier: Apache-2.0
package e2e

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/aisathome/aisathome/gateway"
)

func getStats(t *testing.T, url string) (map[string]any, string) {
	t.Helper()
	resp, err := http.Get(url + "/v1/stats") // no API key
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		t.Fatalf("stats: %d %s", resp.StatusCode, b)
	}
	var s map[string]any
	json.Unmarshal(b, &s)
	return s, string(b)
}

func TestStatsArePublicAndAggregate(t *testing.T) {
	r := newRig(t, mockEngine(t, false))
	for i := 0; i < 3; i++ {
		resp := r.do(t, "POST", "/v1/chat/completions", "user-key", chatBody)
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}
	waitUsage(t, r, 3)
	s, raw := getStats(t, r.gw.URL)
	nodes := s["nodes"].(map[string]any)
	today := s["today"].(map[string]any)
	if nodes["online"].(float64) != 1 || nodes["slots"].(float64) < 1 || today["requests"].(float64) != 3 ||
		today["completion_tokens"].(float64) <= 0 {
		t.Fatalf("unexpected stats: %s", raw)
	}
	// Nothing that identifies nodes, operators or users.
	for _, leak := range []string{"node-1", "alice", "operator", "127.0.0.1", "user-key"} {
		if strings.Contains(raw, leak) {
			t.Fatalf("stats contain %q: %s", leak, raw)
		}
	}
	// A restart rebuilds today's counts from the usage log.
	r2 := newRigCfg(t, func(c *gateway.Config) { c.UsageLog = r.usageLog })
	s2, raw2 := getStats(t, r2.gw.URL)
	if s2["today"].(map[string]any)["requests"].(float64) != 3 {
		t.Fatalf("counts lost on restart: %s", raw2)
	}
	// All-time totals (shown on the site) survive too and add up.
	tot := s2["total"].(map[string]any)
	if tot["requests"].(float64) != 3 || tot["tokens"].(float64) != tot["prompt_tokens"].(float64)+tot["completion_tokens"].(float64) ||
		tot["tokens"].(float64) <= 0 || tot["since"] == nil {
		t.Fatalf("totals: %s", raw2)
	}
}
