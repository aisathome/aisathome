// Network pressure statistics, part 2 (GET /v1/stats/pressure): a runner's host metrics posted with
// its project key, and how a real node's answers spent their time, through the gateway and the node
// agent. Public and aggregate-only: no runner id, project, node, operator, user or key.
//
// SPDX-License-Identifier: Apache-2.0
package e2e

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/aisathome/aisathome/gateway"
)

// timedEngine streams an answer like llama-server: text, then a last chunk with usage and the
// engine's own timings (prompt and generation time, cached prompt tokens).
func timedEngine(t *testing.T) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if health(w, r) {
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		time.Sleep(30 * time.Millisecond)
		for _, piece := range []string{"Hello", ", ", "world"} {
			fmt.Fprintf(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":%q},\"finish_reason\":null}]}\n\n", piece)
			w.(http.Flusher).Flush()
			time.Sleep(10 * time.Millisecond)
		}
		fmt.Fprint(w, "data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n")
		fmt.Fprint(w, "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":40,\"completion_tokens\":3},"+
			"\"timings\":{\"cache_n\":30,\"prompt_n\":10,\"prompt_ms\":5,\"predicted_n\":3,\"predicted_ms\":20}}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
}

const runnerID = "5e1f0c2a9b8d7e6f"

func runnerReport(at time.Time, more string) string {
	return `{"schema":1,"runner":"` + runnerID + `","at":"` + at.UTC().Format(time.RFC3339) + `","period_s":60,"cpus":2,` +
		`"cpu_busy":0.62,"cpu_iowait":0.03,"cpu_steal":0,"mem_total_gb":7.6,"mem_private_gb":1.4,"mem_private_peak_gb":2.1,` +
		`"page_cache_gb":5.5,"page_cache_low_gb":5.2,"refaults_s":250,"disk_read_mb_s":3,"disk_write_mb_s":1,"free_gb":28,` +
		`"lean_slots":1,"lean_busy":0.5,"lean_busy_peak":1,"lean_waiting":0.3,"lean_waiting_peak":2,` +
		`"lean_runs_s":[8.2,7.5],"lean_waits_s":[4],"sessions":3,"sessions_peak":3,"spot_notice":false` + more + `}`
}

func TestPressureRunnersAndNodes(t *testing.T) {
	r := newRigCfg(t, func(c *gateway.Config) {
		withProjects(c)
		c.Projects[0].PublicQueue = true // sciproj publishes its queue; unlimited does not
	}, timedEngine(t))
	resp := r.do(t, "POST", "/v1/chat/completions", "user-key", chatBody)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("chat: HTTP %d", resp.StatusCode)
	}
	waitUsage(t, r, 1)

	now := time.Now()
	post := func(project, key, body string) *http.Response {
		t.Helper()
		resp := r.do(t, "POST", "/v1/projects/"+project+"/runner-metrics", key, body)
		resp.Body.Close()
		return resp
	}
	if c := post("sciproj", "", runnerReport(now, "")).StatusCode; c != 401 {
		t.Fatalf("no key: HTTP %d", c)
	}
	if c := post("sciproj", "user-key", runnerReport(now, "")).StatusCode; c != 401 {
		t.Fatalf("a user key: HTTP %d", c)
	}
	if c := post("sciproj", unlimitedKey, runnerReport(now, "")).StatusCode; c != 403 {
		t.Fatalf("another project's key: HTTP %d", c)
	}
	if c := post("sciproj", projectKey, strings.Replace(runnerReport(now, ""), runnerID, "i-0a1b2c3d4e5f60718", 1)).StatusCode; c != 400 {
		t.Fatalf("an instance id as the runner: HTTP %d", c)
	}
	if c := post("sciproj", projectKey, runnerReport(now, `,"hostname":"ip-10-0-0-5"`)).StatusCode; c != 400 {
		t.Fatalf("an unknown field: HTTP %d", c)
	}
	if c := post("sciproj", projectKey, runnerReport(now, "")).StatusCode; c != 200 {
		t.Fatalf("the project key: HTTP %d", c)
	}
	if c := post("sciproj", projectKey, runnerReport(now, "")).StatusCode; c != 429 {
		t.Fatalf("twice in a minute: HTTP %d", c)
	}
	// A project that keeps its queue private is answered, not counted.
	if c := post("unlimited", unlimitedKey, strings.Replace(runnerReport(now, ""), runnerID, "aaaaaaaaaaaaaaaa", 1)).StatusCode; c != 200 {
		t.Fatalf("a private project: HTTP %d", c)
	}

	res, err := http.Get(r.gw.URL + "/v1/stats/pressure?range=24h") // no key
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(res.Body)
	res.Body.Close()
	body := string(raw)
	for _, leak := range []string{runnerID, "aaaaaaaaaaaaaaaa", "sciproj", "unlimited", "node-0", "op1", "alice", "user-key", projectKey, "127.0.0.1", `"i-`} {
		if strings.Contains(body, leak) {
			t.Fatalf("the public answer holds %q: %s", leak, body)
		}
	}
	var out struct {
		Now  map[string]any `json:"now"`
		Last struct {
			Runner map[string]any `json:"runner"`
			Node   map[string]any `json:"node"`
		} `json:"last_24h"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	rn := out.Last.Runner
	if rn["reports"] != 1.0 || rn["cpu"].(map[string]any)["avg"] != 0.62 || rn["page_cache_gb"].(map[string]any)["low"] != 5.2 ||
		rn["lean"].(map[string]any)["wait_s"].(map[string]any)["count"] != 1.0 || out.Now["runners"] != 1.0 {
		t.Fatalf("runner part: %s", body)
	}
	nd := out.Last.Node
	if nd["requests"] != 1.0 || nd["engine_timed"] != 1.0 || nd["prefill_share"] != 0.2 || nd["cache_hit"] != 0.75 {
		t.Fatalf("node part: %s", body)
	}
}
