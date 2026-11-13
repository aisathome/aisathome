// The operator's view of a node on the account page (owner, 2026-11-13): its average speed and
// what it serves right now, with a science task named and linked, and never a person's request
// content.
//
// SPDX-License-Identifier: Apache-2.0
package e2e

import (
	"context"
	"encoding/json"
	"html"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aisathome/aisathome/gateway"
)

const p2mKey = "ak-prove2me-test-key-0123456789abcdefghijklmnop"

func withPublicP2M(c *gateway.Config) {
	c.Projects = []gateway.Project{{ID: "prove2me", KeySHA256: []string{sha(p2mKey)}, AgentOK: true, PublicQueue: true}}
}

// p2mCall calls the prove2me queue with the project key.
func p2mCall(t *testing.T, r *rig, path string, body any) (int, map[string]any) {
	t.Helper()
	b, _ := json.Marshal(body)
	resp := r.do(t, "POST", "/v1/projects/prove2me/tasks"+path, p2mKey, string(b))
	defer resp.Body.Close()
	var out map[string]any
	json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func p2mTask(id, leaf, theorem string) map[string]any {
	return map[string]any{"id": id, "kind": "attempt", "title": "prove2me t5 " + leaf + " (open)", "created_by": "owner",
		"spec": map[string]any{"trial": "t5", "leaf": leaf, "group": "open", "theorem": theorem, "selection": "2026-11-13",
			"v5": map[string]any{"role": "skeleton", "gen": 1, "task": id}}}
}

// askAs sends a chat request with key and extra headers, and returns when it has ended.
func askAs(t *testing.T, r *rig, key, content string, headers map[string]string) int {
	b, _ := json.Marshal(map[string]any{"model": "deep", "stream": true, "messages": []any{map[string]any{"role": "user", "content": content}}})
	req, _ := http.NewRequest("POST", r.gw.URL+"/v1/chat/completions", strings.NewReader(string(b)))
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Error(err)
		return 0
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	return resp.StatusCode
}

// workLine is the node's "Working on now" line on the account page (unescaped), or "".
func workLine(t *testing.T, a *acctRig, session, node string) string {
	t.Helper()
	_, page := a.get(t, "/account", session)
	i := strings.Index(page, `data-node-now="`+node+`"`)
	if i < 0 {
		return ""
	}
	row := page[i:]
	row = row[:strings.Index(row, "</tr>")]
	return html.UnescapeString(row)
}

func TestOperatorSeesNodeWorkAndSpeed(t *testing.T) {
	a := newAcctRigNodes(t, 100, nil, func(c *gateway.Config) {
		withPublicP2M(c)
		c.QueueTimeoutSec = 3
	})
	s, _ := a.signIn(t, 1)
	token, id, _ := a.addNode(t, s, "my 4090")
	if line := workLine(t, a, s, id); !strings.Contains(line, "not measured yet") || strings.Contains(line, "Working on now") {
		t.Fatalf("an offline node with no answers: %q", line)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a.startTokenNode(ctx, token, slowEngine(t, 30, 50*time.Millisecond), 2, 32768) // 1.5 s per answer
	a.waitNodes(t, 1)
	if line := workLine(t, a, s, id); !strings.Contains(line, "Working on now:</span> nothing") {
		t.Fatalf("an idle connected node: %q", line)
	}

	// Two prove2me tasks, one of them running.
	for _, tk := range []map[string]any{p2mTask("t5-F01", "F01", "CodedSorting.proposition_7_1_transpose"), p2mTask("t5-C12", "C12", "InsiderDonations.price_recast")} {
		if code, out := p2mCall(t, a.rig, "", tk); code != http.StatusCreated {
			t.Fatalf("create: %d %v", code, out)
		}
	}
	if code, _ := p2mCall(t, a.rig, "/claim", map[string]any{"worker": "runner-host-7", "task": "t5-F01", "lease_seconds": 600}); code != 200 {
		t.Fatalf("claim: %d", code)
	}

	// A science request in flight, naming its task: the task, its theorem and a link to the leaf on
	// the prove2me page; never the worker.
	inFlight := func(key, content string, headers map[string]string, want ...string) {
		t.Helper()
		var wg sync.WaitGroup
		wg.Add(1)
		go func() { defer wg.Done(); askAs(t, a.rig, key, content, headers) }()
		var line string
		waitFor(t, func() bool {
			line = workLine(t, a, s, id)
			for _, w := range want {
				if !strings.Contains(line, w) {
					return false
				}
			}
			return true
		})
		for _, bad := range []string{"runner-host-7", "marker-7f3a", "Working on now:</span> nothing"} {
			if strings.Contains(line, bad) {
				t.Fatalf("the work line shows %q: %s", bad, line)
			}
		}
		wg.Wait()
	}
	inFlight(p2mKey, "prove it", map[string]string{"X-Aisathome-Task": "t5-F01"},
		`<a href="/science/prove2me/#leaf-F01">prove2me · t5-F01</a>`, "(CodedSorting.proposition_7_1_transpose)")
	// A header that names a task that is not running is ignored: the one running task it is.
	inFlight(p2mKey, "prove it", map[string]string{"X-Aisathome-Task": "t5-C12"}, "prove2me · t5-F01")
	// With two running tasks and no header the page says it is one of them; the header picks one.
	if code, _ := p2mCall(t, a.rig, "/claim", map[string]any{"worker": "runner-host-7", "task": "t5-C12", "lease_seconds": 600}); code != 200 {
		t.Fatalf("claim: %d", code)
	}
	inFlight(p2mKey, "prove it", nil, `<a href="/science/prove2me/">prove2me</a>`, "(one of 2 running tasks)")
	inFlight(p2mKey, "prove it", map[string]string{"X-Aisathome-Task": "t5-C12"}, "prove2me · t5-C12", "(InsiderDonations.price_recast)")

	// A person's request (the account's own API key) is an "API request", without its content.
	_, page := a.createKey(t, s)
	acctKey := keyRe.FindString(page)
	inFlight(acctKey, "marker-7f3a what is my secret", nil, "Working on now:</span> API request")

	// Once the answers ended: the average speed over them (30 tokens in about 1.5 s each), and nothing in flight.
	line := workLine(t, a, s, id)
	if !strings.Contains(line, "tokens/s (output, last 5 answers)") || !strings.Contains(line, "Working on now:</span> nothing") {
		t.Fatalf("after the answers: %q", line)
	}
	var tps float64
	if i := strings.Index(line, "Average speed:</span> "); i >= 0 {
		json.Unmarshal([]byte(strings.Fields(line[i+len("Average speed:</span> "):])[0]), &tps)
	}
	if tps < 5 || tps > 30 {
		t.Fatalf("speed %.1f tokens/s, want about 20 (30 tokens in 1.5 s): %q", tps, line)
	}

	// Another account never sees this node or its work.
	s2, _ := a.signIn(t, 2)
	if _, page := a.get(t, "/account", s2); strings.Contains(page, id) || strings.Contains(page, "t5-F01") {
		t.Fatal("another account's page shows the node or its work")
	}
}
