// Workload classes and science projects (decision 0011): a project key has its own quota
// instead of an account's budget, its requests wait behind people's, and the statistics count
// classes and projects.
//
// SPDX-License-Identifier: Apache-2.0
package e2e

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aisathome/aisathome/gateway"
)

const (
	projectKey   = "ak-tauceti-test-key-0123456789abcdefghijklmnopq"
	unlimitedKey = "ak-other-project-test-key-0123456789abcdefghij"
)

func sha(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

func withProjects(c *gateway.Config) {
	c.Projects = []gateway.Project{
		{ID: "tauceti", KeySHA256: []string{sha(projectKey)}, DailyTokens: 60, MaxNodeShare: 0.7, AgentOK: true},
		{ID: "unlimited", KeySHA256: []string{sha(unlimitedKey)}},
	}
}

func statsJSON(t *testing.T, r *rig, path string) map[string]any {
	t.Helper()
	resp, err := http.Get(r.gw.URL + path)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil || resp.StatusCode != 200 {
		t.Fatalf("%s: %d %v", path, resp.StatusCode, err)
	}
	return out
}

// A project key has the project's own daily quota, not an account's budget: it keeps working
// after an account is over its budget, and stops at its own. Its answers name the node; the
// usage log and /v1/stats count it as science, under the project.
func TestProjectKeyHasItsOwnQuota(t *testing.T) {
	a := newAcctRigWith(t, 100, func(c *gateway.AccountsConfig) { c.Limits.TokensPerDay = 40 }, withProjects)
	s, _ := a.signIn(t, 9)
	_, page := a.createKey(t, s)
	acctKey := keyRe.FindString(page)
	used := 0
	for ; used < 20; used++ {
		if c, _ := ask(t, a.rig, acctKey, chatBody); c != 200 {
			break
		}
		waitUsage(t, a.rig, used+1)
	}
	if c, _ := ask(t, a.rig, acctKey, chatBody); c != 429 {
		t.Fatalf("account key over budget: %d", c)
	}
	sent := used
	served := 0
	for ; served < 30; served++ {
		resp := a.do(t, "POST", "/v1/chat/completions", projectKey, chatBody)
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode == 429 {
			if resp.Header.Get("Retry-After") == "" {
				t.Fatal("over quota without Retry-After")
			}
			break
		}
		if resp.StatusCode != 200 {
			t.Fatalf("project key: %d", resp.StatusCode)
		}
		if resp.Header.Get(servedByHeader) != "node-0" {
			t.Fatalf("a project key's answer does not name its node: %q", resp.Header.Get(servedByHeader))
		}
		sent++
		waitUsage(t, a.rig, sent)
	}
	if served == 0 || served == 30 {
		t.Fatalf("project key served %d requests: want its own quota of 60 tokens to end it", served)
	}
	// A project without a quota is not limited.
	for i := 0; i < 12; i++ {
		if c, _ := ask(t, a.rig, unlimitedKey, chatBody); c != 200 {
			t.Fatalf("project without a quota, request %d: %d", i, c)
		}
		sent++
		waitUsage(t, a.rig, sent)
	}
	var sci, agents int
	for _, u := range a.usage(t) {
		switch u["class"] {
		case "science":
			sci++
			if u["user"] != "project:"+u["project"].(string) {
				t.Fatalf("science record: %v", u)
			}
		case "agent":
			agents++
		default:
			t.Fatalf("record without class: %v", u)
		}
	}
	if sci != served+12 || agents != used {
		t.Fatalf("usage log: %d science, %d agent; want %d and %d", sci, agents, served+12, used)
	}
	st := statsJSON(t, a.rig, "/v1/stats")
	today := st["today"].(map[string]any)
	if p := today["projects"].(map[string]any)["tauceti"].(map[string]any); p["requests"] != float64(served) {
		t.Fatalf("stats today, tauceti: %v", p)
	}
	if c := today["classes"].(map[string]any)["science"].(map[string]any); c["requests"] != float64(served+12) {
		t.Fatalf("stats today, science: %v", c)
	}
	if p := st["total"].(map[string]any)["projects"].(map[string]any)["unlimited"].(map[string]any); p["requests"] != 12.0 {
		t.Fatalf("stats total, unlimited: %v", p)
	}
}

// With one busy node, requests that wait are served chat first, then agents, then science,
// although the science request came first: it never delays a waiting person's request.
func TestScienceWaitsBehindPeople(t *testing.T) {
	r := newRigCfg(t, func(c *gateway.Config) {
		withProjects(c)
		c.Keys["owui-key"] = gateway.KeyInfo{User: "owui", ForwardUsers: true}
	})
	ctx := t.Context()
	r.startNode(ctx, "solo", "node-token", slowEngine(t, 5, 40*time.Millisecond), 1)
	r.waitNodes(t, 1)
	var mu sync.Mutex
	var order []string
	var wg sync.WaitGroup
	send := func(name, base, key string) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req, _ := http.NewRequest("POST", base+"/v1/chat/completions", strings.NewReader(chatBody))
			req.Header.Set("Authorization", "Bearer "+key)
			if base == r.front.URL {
				req.Header.Set("X-OpenWebUI-User-Id", "u1")
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Error(err)
				return
			}
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			if resp.StatusCode != 200 {
				t.Errorf("%s: %d", name, resp.StatusCode)
			}
			mu.Lock()
			order = append(order, name)
			mu.Unlock()
		}()
		time.Sleep(40 * time.Millisecond) // arrive in this order, all while the first runs
	}
	send("running", r.gw.URL, "user-key")
	send("science", r.gw.URL, projectKey)
	send("agent", r.gw.URL, "user-key")
	send("chat", r.front.URL, "owui-key")
	wg.Wait()
	if got := strings.Join(order, ","); got != "running,chat,agent,science" {
		t.Fatalf("served in the order %s", got)
	}
	for _, u := range r.usage(t) {
		if want := map[string]string{"owui/u1": "chat", "alice": "agent", "project:tauceti": "science"}[u["user"].(string)]; u["class"] != want {
			t.Fatalf("record of %v has class %v, want %s", u["user"], u["class"], want)
		}
	}
}

// /v1/stats/history is public and holds aggregates only: per day and class, no users, keys,
// nodes or operators.
func TestHistoryEndpoint(t *testing.T) {
	r := newRigCfg(t, withProjects, mockEngine(t, false))
	for _, key := range []string{"user-key", projectKey} {
		if c, _ := ask(t, r, key, chatBody); c != 200 {
			t.Fatalf("%s: %d", key, c)
		}
	}
	waitUsage(t, r, 2)
	resp, err := http.Get(r.gw.URL + "/v1/stats/history")
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !strings.Contains(resp.Header.Get("Cache-Control"), "public") {
		t.Fatalf("history: %d %v", resp.StatusCode, resp.Header)
	}
	for _, s := range []string{"alice", "node-0", "op1", "tauceti", "user-key", "project:"} {
		if strings.Contains(string(raw), s) {
			t.Fatalf("history names %q: %s", s, raw)
		}
	}
	var h struct {
		Days []struct {
			Date     string                    `json:"date"`
			Requests int                       `json:"requests"`
			Classes  map[string]map[string]int `json:"classes"`
		} `json:"days"`
	}
	if err := json.Unmarshal(raw, &h); err != nil || len(h.Days) != 1 {
		t.Fatalf("history: %s", raw)
	}
	d := h.Days[0]
	if d.Date != time.Now().UTC().Format(time.DateOnly) || d.Requests != 2 ||
		d.Classes["agent"]["requests"] != 1 || d.Classes["science"]["requests"] != 1 {
		t.Fatalf("today: %+v", d)
	}
}
