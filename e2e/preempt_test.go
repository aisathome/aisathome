// Preempting science for people (BACKLOG v0.3.1): a chat or agent request that has waited
// preempt_after_seconds for a node stops a science request there and gets the node; the
// science request is sent again by the gateway if nothing of it reached its client, or ends
// with a retryable error event. People's requests are never preempted.
//
// SPDX-License-Identifier: Apache-2.0
package e2e

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/aisathome/aisathome/gateway"
)

// sizedEngine streams as many chunks as the prompt asks for ("chunks=N"), one every 20 ms, and
// stops when the node cancels the request (the client left, or a preemption).
func sizedEngine(t *testing.T) *httptest.Server {
	re := regexp.MustCompile(`chunks=(\d+)`)
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if health(w, r) {
			return
		}
		b, _ := io.ReadAll(r.Body)
		n := 5
		if m := re.FindSubmatch(b); m != nil {
			n, _ = strconv.Atoi(string(m[1]))
		}
		w.Header().Set("Content-Type", "text/event-stream")
		for i := 0; i < n; i++ {
			select {
			case <-r.Context().Done():
				return
			case <-time.After(20 * time.Millisecond):
			}
			fmt.Fprintf(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"t%d \"},\"finish_reason\":null}]}\n\n", i)
			w.(http.Flusher).Flush()
		}
		fmt.Fprint(w, "data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n")
		fmt.Fprintf(w, "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":5,\"completion_tokens\":%d}}\n\n", n)
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
}

// preemptRig is one node with one slot, the projects of projects_test.go, a front end key for
// chats, and preemption after preemptAfter seconds (-1: off).
func preemptRig(t *testing.T, preemptAfter int) *rig {
	r := newRigCfg(t, func(c *gateway.Config) {
		withProjects(c)
		c.Keys["owui-key"] = gateway.KeyInfo{User: "owui", ForwardUsers: true}
		c.PreemptAfterSec = preemptAfter
	})
	r.startNode(t.Context(), "solo", "node-token", sizedEngine(t), 1)
	r.waitNodes(t, 1)
	return r
}

func sizedBody(chunks int, stream bool) string {
	return fmt.Sprintf(`{"model":"deep","stream":%v,"max_tokens":4000,"messages":[{"role":"user","content":"chunks=%d"}]}`, stream, chunks)
}

// answer is what a client got: the status, the text chunks, and the stream's error event or
// [DONE].
type answer struct {
	status  int
	header  http.Header
	chunks  int
	done    bool
	errEvt  map[string]any
	body    string
	elapsed time.Duration
}

// send makes a request as one of the classes: "chat" through the front end, "agent" with an
// account-less user key, "science" with the tauceti project key.
func send(t *testing.T, r *rig, class, body string) answer {
	t.Helper()
	base, key := r.gw.URL, map[string]string{"agent": "user-key", "science": projectKey}[class]
	if class == "chat" {
		base, key = r.front.URL, "owui-key"
	}
	req, _ := http.NewRequest("POST", base+"/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+key)
	if class == "chat" {
		req.Header.Set("X-OpenWebUI-User-Id", "u1")
	}
	t0 := time.Now()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Error(err)
		return answer{}
	}
	defer resp.Body.Close()
	a := answer{status: resp.StatusCode, header: resp.Header}
	if !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		b, _ := io.ReadAll(resp.Body)
		a.body, a.elapsed = string(b), time.Since(t0)
		return a
	}
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		data, ok := strings.CutPrefix(sc.Text(), "data: ")
		if !ok {
			continue
		}
		if data == "[DONE]" {
			a.done = true
			continue
		}
		var ev map[string]any
		json.Unmarshal([]byte(data), &ev)
		if e, ok := ev["error"].(map[string]any); ok {
			a.errEvt = e
		} else if strings.Contains(data, `"content"`) {
			a.chunks++
		}
	}
	a.elapsed = time.Since(t0)
	return a
}

// start sends a request in the background.
func start(t *testing.T, r *rig, class, body string) chan answer {
	ch := make(chan answer, 1)
	go func() { ch <- send(t, r, class, body) }()
	return ch
}

// busyWith waits until the node's slot is taken.
func busyWith(t *testing.T, r *rig) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		st := statsJSON(t, r, "/v1/stats")
		if st["nodes"].(map[string]any)["busy"] == 1.0 {
			time.Sleep(100 * time.Millisecond) // let some output reach the client
			return
		}
	}
	t.Fatal("the node never got busy")
}

func outcomes(t *testing.T, r *rig, user string) []string {
	var out []string
	for _, u := range r.usage(t) {
		if u["user"] == user {
			out = append(out, u["outcome"].(string))
		}
	}
	return out
}

// A chat or an agent request that waits for the only node, busy with a long science stream,
// gets it within preempt_after_seconds and a little; the science stream, which had output,
// ends with a retryable error event; the attempt is logged as preempted, credited for what
// reached the client, and counted in /v1/stats per class and project.
func TestPeoplePreemptStreamingScience(t *testing.T) {
	for _, class := range []string{"chat", "agent"} {
		t.Run(class, func(t *testing.T) {
			r := preemptRig(t, 1)
			sci := start(t, r, "science", sizedBody(1500, true)) // 30 s of output
			busyWith(t, r)
			t0 := time.Now()
			a := send(t, r, class, sizedBody(5, true))
			if a.status != 200 || !a.done || a.errEvt != nil {
				t.Fatalf("%s: %d done=%v err=%v %s", class, a.status, a.done, a.errEvt, a.body)
			}
			if took := time.Since(t0); took > 4*time.Second {
				t.Fatalf("%s took %v behind science; want about the 1 s threshold", class, took)
			}
			var s answer
			select {
			case s = <-sci:
			case <-time.After(5 * time.Second):
				t.Fatal("the science stream did not end when it was preempted")
			}
			if s.done || s.chunks == 0 || s.chunks >= 1500 {
				t.Fatalf("science stream: done=%v chunks=%d; want ended early after some output", s.done, s.chunks)
			}
			// What opencode v2 retries: a 503 code (provider-side failure) and "try again".
			if s.errEvt["code"] != 503.0 || s.errEvt["reason"] != "preempted" || s.errEvt["type"] != "server_error" ||
				!strings.Contains(s.errEvt["message"].(string), "try again") {
				t.Fatalf("science error event: %v", s.errEvt)
			}
			waitUsage(t, r, 2)
			var rec map[string]any
			for _, u := range r.usage(t) {
				if u["user"] == "project:tauceti" {
					rec = u
				}
			}
			if rec["outcome"] != "preempted" || rec["class"] != "science" || rec["project"] != "tauceti" {
				t.Fatalf("usage record of the science request: %v", rec)
			}
			if c, _ := rec["credit"].(float64); c <= 0 {
				t.Fatalf("the node earned nothing for the output that reached the client: %v", rec)
			}
			st := statsJSON(t, r, "/v1/stats")
			today := st["today"].(map[string]any)
			if p := today["projects"].(map[string]any)["tauceti"].(map[string]any); p["preempted"] != 1.0 {
				t.Fatalf("stats today, tauceti: %v", p)
			}
			if c := today["classes"].(map[string]any)["science"].(map[string]any); c["preempted"] != 1.0 {
				t.Fatalf("stats today, science: %v", c)
			}
			if c := today["classes"].(map[string]any)[class].(map[string]any); c["preempted"] != nil {
				t.Fatalf("stats today, %s: %v", class, c)
			}
			if p := st["total"].(map[string]any)["projects"].(map[string]any)["tauceti"].(map[string]any); p["preempted"] != 1.0 {
				t.Fatalf("stats total, tauceti: %v", p)
			}
		})
	}
}

// A science request whose client got nothing yet (non-streaming) is sent again by the gateway
// behind the person: its client just gets the whole answer, later.
func TestPreemptedScienceWithoutOutputIsRetried(t *testing.T) {
	r := preemptRig(t, 1)
	sci := start(t, r, "science", sizedBody(150, false)) // 3 s
	busyWith(t, r)
	t0 := time.Now()
	if a := send(t, r, "chat", sizedBody(5, true)); a.status != 200 || !a.done {
		t.Fatalf("chat: %d %v", a.status, a.errEvt)
	}
	if took := time.Since(t0); took > 2500*time.Millisecond {
		t.Fatalf("the chat took %v; the 3 s science answer was not preempted", took)
	}
	s := <-sci
	if s.status != 200 || strings.Count(s.body, "t149 ") != 1 {
		t.Fatalf("science after its retry: %d %.200s", s.status, s.body)
	}
	waitUsage(t, r, 3)
	if got := strings.Join(outcomes(t, r, "project:tauceti"), ","); got != "preempted,ok" {
		t.Fatalf("science attempts: %s", got)
	}
}

// People's requests are never preempted, whichever class waits for them; and with
// preempt_after_seconds -1 science is not preempted either. The waiting request is served
// once the running one ended, in full.
func TestNoPreemption(t *testing.T) {
	for _, c := range []struct {
		running, waiting string
		after            int
	}{
		{"chat", "chat", 1}, {"agent", "chat", 1}, {"chat", "agent", 1}, {"agent", "agent", 1},
		{"science", "chat", -1},
	} {
		t.Run(fmt.Sprintf("%s-for-%s-after-%d", c.running, c.waiting, c.after), func(t *testing.T) {
			r := preemptRig(t, c.after)
			run := start(t, r, c.running, sizedBody(125, true)) // 2.5 s
			busyWith(t, r)
			t0 := time.Now()
			w := send(t, r, c.waiting, sizedBody(5, true))
			if w.status != 200 || !w.done {
				t.Fatalf("waiting %s: %d %v", c.waiting, w.status, w.errEvt)
			}
			if took := time.Since(t0); took < 2*time.Second {
				t.Fatalf("the waiting %s was served after %v: the running %s was preempted", c.waiting, took, c.running)
			}
			if a := <-run; a.status != 200 || !a.done || a.errEvt != nil || a.chunks != 125 {
				t.Fatalf("running %s: done=%v chunks=%d err=%v", c.running, a.done, a.chunks, a.errEvt)
			}
			for _, u := range r.usage(t) {
				if u["outcome"] == "preempted" {
					t.Fatalf("preempted: %v", u)
				}
			}
		})
	}
}
