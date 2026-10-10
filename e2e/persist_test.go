// Tests for state that must survive a gateway restart, and for per-user API keys managed in
// a keys file.
//
// SPDX-License-Identifier: Apache-2.0
package e2e

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aisathome/aisathome/gateway"
)

// A restart must not hand a key a fresh daily budget.
func TestBudgetSurvivesRestart(t *testing.T) {
	var cfg gateway.Config
	r := newRigCfg(t, func(c *gateway.Config) {
		c.StateDir = t.TempDir()
		c.Keys["budget-key"] = gateway.KeyInfo{User: "dave", TokensPerDay: 10}
		cfg = *c
	}, mockEngine(t, false))
	ask := func(url string) int {
		req, _ := http.NewRequest("POST", url+"/v1/chat/completions", strings.NewReader(chatBody))
		req.Header.Set("Authorization", "Bearer budget-key")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		return resp.StatusCode
	}
	// mockEngine's usage weighs 7 tokens: two requests use up a budget of 10.
	for i := 0; i < 2; i++ {
		if c := ask(r.gw.URL); c != 200 {
			t.Fatalf("request %d: %d", i, c)
		}
	}
	if c := ask(r.gw.URL); c != 429 {
		t.Fatalf("budget not enforced before restart: %d", c)
	}
	r.gw.Close()
	if err := r.g.Close(); err != nil {
		t.Fatal(err)
	}

	cfg.QueueTimeoutSec = 1 // without the saved budget, fail fast rather than wait for a node
	g2, err := gateway.New(cfg, quiet())
	if err != nil {
		t.Fatal(err)
	}
	defer g2.Close()
	gw2 := httptest.NewServer(g2.Handler())
	defer gw2.Close()
	if c := ask(gw2.URL); c != 429 {
		t.Fatalf("restart reset the daily budget: got %d, want 429", c)
	}
}
