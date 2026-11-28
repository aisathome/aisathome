// SPDX-License-Identifier: Apache-2.0
package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aisathome/aisathome/protocol"
	"github.com/coder/websocket"
)

const servedSecret = "served-test-secret-0123456789abcdef"

// servedRec is a usage record of a project's call that named task, served by node of operator.
func servedRec(project, task, node, operator, outcome string) usageRecord {
	return usageRecord{Time: time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC), Node: node, Operator: operator, Outcome: outcome,
		Class: ClassScience, Project: project, Task: task, Usage: protocol.Usage{PromptTokens: 100, CompletionTokens: 10}}
}

// The index: only ok calls of a project that named a well-formed task count; a row per public name
// and class, most calls first, the nodes without a name one row per class; a pseudonym without a
// username, a config token's operator and a pilot's unnamed account are never a name.
func TestServedIndexRows(t *testing.T) {
	var x servedIndex
	alice, bob := AccountPseudonym(servedSecret, "gh:1"), AccountPseudonym(servedSecret, "gh:2")
	names := map[string]string{alice: "Alice"}
	add := func(r usageRecord, n int) {
		for range n {
			x.add(&r)
		}
	}
	add(servedRec("prove2me", "t6-a", "n-0a0a0a0a", alice, "ok"), 3)
	add(servedRec("prove2me", "t6-a", "n-0b0b0b0b", alice, "ok"), 2) // Alice's second node
	add(servedRec("prove2me", "t6-a", "n-0c0c0c0c", bob, "ok"), 4)   // no username
	add(servedRec("prove2me", "t6-a", "a-0d0d0d0d", AccountPseudonym(servedSecret, "ax:9"), "ok"), 1)
	add(servedRec("prove2me", "t6-a", "p-0123456789ab", bob, "ok"), 2)
	add(servedRec("prove2me", "t6-a", "gpu-box", "maintainer", "ok"), 1) // a config token's
	for _, o := range []string{"failed", "cancelled", outcomePreempted, outcomeBlocked} {
		add(servedRec("prove2me", "t6-a", "n-0a0a0a0a", alice, o), 5)
	}
	add(servedRec("", "t6-a", "n-0a0a0a0a", alice, "ok"), 1)            // no project
	add(servedRec("prove2me", "", "n-0a0a0a0a", alice, "ok"), 1)        // no task named
	add(servedRec("prove2me", "bad id!", "n-0a0a0a0a", alice, "ok"), 1) // not a task id
	add(servedRec("prove2me", "t6-b", "", alice, "ok"), 1)              // no node
	add(servedRec("lab", "t6-a", "n-0a0a0a0a", alice, "ok"), 7)         // another project's task of that name

	got := x.rows("prove2me", "t6-a", names)
	b, _ := json.Marshal(got)
	want := `[{"name":"Alice","class":"volunteer","nodes":2,"requests":5},{"name":null,"class":"volunteer","nodes":2,"requests":5},` +
		`{"name":null,"class":"pilot","nodes":1,"requests":2},{"name":null,"class":"trusted","nodes":1,"requests":1}]`
	if string(b) != want {
		t.Fatalf("rows\n got %s\nwant %s", b, want)
	}
	for _, leak := range []string{alice, bob, "n-0", "a-0", "p-0", "gpu-box", "maintainer", "gh:", "ax:"} {
		if strings.Contains(string(b), leak) {
			t.Errorf("rows name %q: %s", leak, b)
		}
	}
	if r := x.rows("lab", "t6-a", names); len(r) != 1 || r[0].Requests != 7 || r[0].Name == nil {
		t.Errorf("lab's task: %+v", r)
	}
	for _, k := range [][2]string{{"prove2me", "t6-b"}, {"prove2me", "nosuch"}, {"prove2me", ""}} {
		if r := x.rows(k[0], k[1], names); r != nil {
			t.Errorf("%v: %+v, want nil", k, r)
		}
	}
	// Without the name (the username cleared, an account banned): the same calls, unnamed.
	b, _ = json.Marshal(x.rows("prove2me", "t6-a", nil))
	if string(b) != `[{"name":null,"class":"volunteer","nodes":4,"requests":10},{"name":null,"class":"pilot","nodes":1,"requests":2},{"name":null,"class":"trusted","nodes":1,"requests":1}]` {
		t.Fatalf("without names: %s", b)
	}
}

// Bounded: past maxServedNodes a task's nodes count as one unnamed node of their class; past
// maxServedTasks a new task is not indexed (the ones there keep counting).
func TestServedIndexBounds(t *testing.T) {
	var x servedIndex
	alice := AccountPseudonym(servedSecret, "gh:1")
	for i := range maxServedNodes + 5 {
		r := servedRec("prove2me", "t6-a", fmt.Sprintf("n-%08x", i), alice, "ok")
		x.add(&r)
	}
	r := servedRec("prove2me", "t6-a", "n-00000000", alice, "ok") // a node it has: still its own
	x.add(&r)
	if n := len(x.m[liveKey{"prove2me", "t6-a"}]); n != maxServedNodes+1 {
		t.Fatalf("%d nodes kept, want %d and the overflow", n, maxServedNodes)
	}
	rows := x.rows("prove2me", "t6-a", map[string]string{alice: "Alice"})
	if len(rows) != 2 || rows[0].Name == nil || rows[0].Nodes != maxServedNodes || rows[0].Requests != maxServedNodes+1 ||
		rows[1].Name != nil || rows[1].Nodes != 1 || rows[1].Requests != 5 {
		t.Fatalf("rows %+v", rows)
	}

	for i := len(x.m); i < maxServedTasks; i++ {
		x.m[liveKey{"prove2me", fmt.Sprintf("t-%d", i)}] = nil
	}
	r = servedRec("prove2me", "t6-new", "n-0a0a0a0a", alice, "ok")
	x.add(&r)
	if _, ok := x.m[liveKey{"prove2me", "t6-new"}]; ok || len(x.m) != maxServedTasks {
		t.Fatalf("a task past the bound was indexed (%d tasks)", len(x.m))
	}
	r = servedRec("prove2me", "t6-a", "n-0a0a0a0a", alice, "ok")
	x.add(&r)
	if rows := x.rows("prove2me", "t6-a", nil); rows[0].Requests != maxServedNodes+7 {
		t.Fatalf("an indexed task stopped counting: %+v", rows)
	}
}

// GET /v1/tasks: served_by from the usage log the gateway replays at start (a task served before
// this gateway ran) and from the calls it relays since; named by username only, the name gone at the
// next build once it is cleared; null for a task the log does not name. Nothing private in it.
func TestPublicTasksServedBy(t *testing.T) {
	dir := t.TempDir()
	alice, bob := AccountPseudonym(servedSecret, "gh:1"), AccountPseudonym(servedSecret, "gh:2")
	var log strings.Builder
	for _, r := range []usageRecord{
		servedRec("prove2me", "t6-old", "n-0a0a0a0a", alice, "ok"),
		servedRec("prove2me", "t6-old", "n-0a0a0a0a", alice, "ok"),
		servedRec("prove2me", "t6-old", "n-0a0a0a0a", alice, "failed"),
		servedRec("prove2me", "t6-old", "p-0123456789ab", bob, "ok"),
		servedRec("prove2me", "t6-gone", "n-0a0a0a0a", alice, "ok"), // no such task: never shown
	} {
		b, _ := json.Marshal(r)
		log.Write(append(b, '\n'))
	}
	log.WriteString("not a record\n")
	usage := filepath.Join(dir, "usage.jsonl")
	if err := os.WriteFile(usage, []byte(log.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	g := newTestGateway(t, Config{StateDir: dir, UsageLog: usage,
		Accounts:   &AccountsConfig{File: filepath.Join(dir, "accounts.json"), PublicURL: "https://site.test", SessionSecret: servedSecret, GitHubClientID: "id", GitHubClientSecret: "secret"},
		NodeTokens: map[string]NodeToken{"node-token": {Operator: "op1", OperatorTerms: OperatorTermsVersion}},
		Projects:   []Project{{ID: "prove2me", KeySHA256: []string{keyHash(testProjectKey)}, PublicQueue: true, Kinds: []string{"p2m-v7"}}}})
	defer g.Close()
	g.accounts.Update(func(f *AccountsFile) error { f.Settings.MaxActive = 10; return nil })
	for _, id := range []string{"gh:1", "gh:2"} {
		if _, err := g.accounts.SignIn(id, time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	if err := g.accounts.SetUsername("gh:1", "Alice"); err != nil {
		t.Fatal(err)
	}
	r := &taskRig{t: t, g: g, now: time.Now()}
	for _, id := range []string{"t6-old", "t6-new", "t6-none"} {
		r.p2m("", map[string]any{"id": id, "kind": "p2m-v7", "created_by": "owner", "role": "hole", "spec": map[string]any{}})
	}
	r.p2m("/claim", map[string]any{"worker": "w1", "task": "t6-new", "lease_seconds": 600})

	servedOf := func() (map[string]any, string) {
		t.Helper()
		_, body, page := r.v1("?project=prove2me")
		out := map[string]any{}
		for _, tk := range page.Tasks {
			v, ok := tk["served_by"]
			if !ok {
				t.Fatalf("%s: no served_by: %s", tk["id"], body)
			}
			out[tk["id"].(string)] = v
		}
		for _, leak := range []string{alice, bob, "acct-", "n-0a", "p-0123", "op1", "n1", "gh:", "t6-gone"} {
			if strings.Contains(body, leak) {
				t.Errorf("GET /v1/tasks names %q: %s", leak, body)
			}
		}
		return out, body
	}
	js := func(v any) string { b, _ := json.Marshal(v); return string(b) }

	// From the log: the old task's calls; nothing yet for the others.
	got, body := servedOf()
	if js(got["t6-old"]) != `[{"class":"volunteer","name":"Alice","nodes":1,"requests":2},{"class":"pilot","name":null,"nodes":1,"requests":1}]` ||
		got["t6-new"] != nil || got["t6-none"] != nil {
		t.Fatalf("from the log: %s", body)
	}

	// A call relayed now, through a config token's node: it counts once it ended ok.
	srv := httptest.NewServer(g.Handler())
	defer srv.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h := http.Header{}
	h.Set("Authorization", "Bearer node-token")
	c, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http")+"/v1/node/connect", &websocket.DialOptions{HTTPHeader: h})
	if err != nil {
		t.Fatal(err)
	}
	write := func(m protocol.Msg) { b, _ := json.Marshal(m); c.Write(ctx, websocket.MessageText, b) }
	write(protocol.Msg{Type: protocol.TypeHello, NodeID: "n1", Protocol: protocol.LegacyVersion,
		Models: []protocol.ModelInfo{{Name: "qwen3.8-27b-q4", Roles: []string{"deep"}, Slots: 1}}})
	go func() {
		for {
			_, b, err := c.Read(ctx)
			if err != nil {
				return
			}
			var m protocol.Msg
			json.Unmarshal(b, &m)
			if m.Type == protocol.TypeRequest {
				write(protocol.Msg{Type: protocol.TypeChunk, ReqID: m.ReqID, Data: []byte(`{"choices":[{"index":0,"delta":{"content":"x"}}]}`)})
				write(protocol.Msg{Type: protocol.TypeDone, ReqID: m.ReqID, Usage: &protocol.Usage{PromptTokens: 5, CompletionTokens: 1}})
			}
		}
	}()
	for deadline := time.Now().Add(5 * time.Second); g.NodeCount() != 1; time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the node did not connect")
		}
	}
	for range 2 {
		req, _ := http.NewRequest("POST", srv.URL+"/v1/chat/completions", strings.NewReader(`{"model":"deep","messages":[{"role":"user","content":"hi"}]}`))
		req.Header.Set("Authorization", "Bearer "+testProjectKey)
		req.Header.Set(taskHeader, "t6-new")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("chat: HTTP %d", resp.StatusCode)
		}
	}
	r.freshV1()
	got, body = servedOf()
	if js(got["t6-new"]) != `[{"class":"trusted","name":null,"nodes":1,"requests":2}]` || got["t6-none"] != nil {
		t.Fatalf("after two relayed calls: %s", body)
	}
	if raw, _ := os.ReadFile(usage); strings.Count(string(raw), `"task":"t6-new"`) != 2 {
		t.Fatalf("the usage log holds the calls: %s", raw)
	}

	// The username cleared: unnamed at the next build of the list.
	if err := g.accounts.ClearUsername("gh:1"); err != nil {
		t.Fatal(err)
	}
	r.freshV1()
	got, body = servedOf()
	if js(got["t6-old"]) != `[{"class":"volunteer","name":null,"nodes":1,"requests":2},{"class":"pilot","name":null,"nodes":1,"requests":1}]` ||
		strings.Contains(body, "Alice") {
		t.Fatalf("after the username was cleared: %s", body)
	}

	// A restart rebuilds the same from the log.
	cancel()
	g.Close()
	g2 := newTestGateway(t, Config{StateDir: dir, UsageLog: usage,
		Accounts: &AccountsConfig{File: filepath.Join(dir, "accounts.json"), PublicURL: "https://site.test", SessionSecret: servedSecret, GitHubClientID: "id", GitHubClientSecret: "secret"},
		Projects: []Project{{ID: "prove2me", KeySHA256: []string{keyHash(testProjectKey)}, PublicQueue: true, Kinds: []string{"p2m-v7"}}}})
	defer g2.Close()
	g2.accounts.SetUsername("gh:1", "Alice2")
	r.g = g2
	got, body = servedOf()
	if js(got["t6-old"]) != `[{"class":"volunteer","name":"Alice2","nodes":1,"requests":2},{"class":"pilot","name":null,"nodes":1,"requests":1}]` ||
		js(got["t6-new"]) != `[{"class":"trusted","name":null,"nodes":1,"requests":2}]` {
		t.Fatalf("after a restart: %s", body)
	}
}
