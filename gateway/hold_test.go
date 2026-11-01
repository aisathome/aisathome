// SPDX-License-Identifier: Apache-2.0
package gateway

import (
	"encoding/json"
	"fmt"
	"maps"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/aisathome/aisathome/protocol"
)

// Unrecorded guard holds are bounded: beyond maxUnrecorded every node is held (fail closed),
// never one dropped. Recording drops only the hold that was written, not a newer one.
func TestUnrecordedHolds(t *testing.T) {
	var u unrecordedHolds
	at := time.Now()
	for i := 0; i < maxUnrecorded; i++ {
		if !u.add(NodeHold{Node: fmt.Sprintf("n-%08x", i), Rule: "r", At: at}) {
			t.Fatalf("hold %d refused below the bound", i)
		}
	}
	if u.has("n-other") {
		t.Fatal("an unknown node is held below the bound")
	}
	if u.add(NodeHold{Node: "n-other", Rule: "r", At: at}) || !u.has("n-other") || len(u.m) != maxUnrecorded {
		t.Fatalf("beyond the bound: every node held, none added (%d)", len(u.m))
	}
	var v unrecordedHolds
	old := NodeHold{Node: "n-1", Rule: "a", At: at}
	v.add(old)
	v.recorded(NodeHold{Node: "n-1", Rule: "a", At: at.Add(time.Second)}) // another hold
	if !v.has("n-1") {
		t.Fatal("recording another hold dropped this one")
	}
	v.recorded(old)
	if v.has("n-1") {
		t.Fatal("a recorded hold is still kept in memory")
	}
}

// A held node gets no requests, so it counts in stats' held and in nothing else, and a hold
// warns with its rule and time, not the node. The old fields of /v1/stats stay as they were.
func TestHeldNodeIsNotOnline(t *testing.T) {
	s := newStickyRig(t)
	s.node("a", "op", true, 0)
	s.node("p", "op", true, 0).paused = true
	h := s.node("n-heldnode", "vol", false, 0)
	h.models = []protocol.ModelInfo{{Name: "held-model", Roles: []string{"only-held"}, Slots: 3}}
	h.inflight = 2
	h2 := s.node("n-heldtoo", "vol", false, 0)
	h2.paused = true

	at := time.Now()
	s.g.holdNode(h, "file-upload", "req-1")
	s.g.holdNode(h, "credential-path", "req-2") // the first hold stands, as in the accounts file
	s.g.holdNode(h2, "download-and-execute", "req-3")

	w := httptest.NewRecorder()
	s.g.handleStats(w, httptest.NewRequest("GET", "/v1/stats", nil))
	var st struct {
		Nodes  map[string]int `json:"nodes"`
		Models []string       `json:"models"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &st); err != nil {
		t.Fatal(err)
	}
	if want := map[string]int{"online": 2, "slots": 2, "busy": 0, "paused": 1, "held": 2}; !maps.Equal(st.Nodes, want) {
		t.Fatalf("nodes %v, want %v", st.Nodes, want)
	}
	if !slices.Equal(st.Models, []string{"deep"}) {
		t.Fatalf("models %v: a held node's role counts", st.Models)
	}
	if s.g.NodeCount() != 4 || s.g.nodesOnline() != 2 {
		t.Fatalf("connected %d, online %d", s.g.NodeCount(), s.g.nodesOnline())
	}
	if board := s.g.leaderboard(at)["nodes"].(map[string]any); board["online"] != 2 {
		t.Fatalf("the leaderboard says %v online", board["online"])
	}

	holds := s.g.holds()
	if len(holds) != 2 || holds[0].Node != "n-heldnode" || holds[0].Rule != "file-upload" || holds[1].Node != "n-heldtoo" {
		t.Fatalf("holds %+v", holds)
	}
	var texts []string
	for _, w := range s.g.healthOf("tauceti", at) {
		if w.Rule == ruleNodeHeld {
			texts = append(texts, w.Text)
		}
	}
	if len(texts) != 2 || !strings.Contains(texts[0], "file-upload") || !strings.Contains(texts[0], holds[0].At.UTC().Format("01-02 15:04")) {
		t.Fatalf("warnings %q", texts)
	}
	if strings.Contains(texts[0], "n-held") {
		t.Fatalf("a project's warning names the node: %q", texts[0])
	}
}

// Only held nodes: nothing can answer, and the warning says so.
func TestOnlyHeldNodesWarnNoNodes(t *testing.T) {
	s := newStickyRig(t)
	s.g.holdNode(s.node("h", "vol", false, 0), "file-upload", "req")
	found := false
	for _, w := range s.g.healthOf("tauceti", time.Now()) {
		found = found || w.Rule == ruleNoNodes
	}
	if !found {
		t.Fatal("no no-nodes warning with every node held")
	}
}

// With an accounts file: a hold on record, and one it could not record yet, both list, the node
// connected or not. The file's record wins over the same node's copy in memory.
func TestHoldsListRecordedAndUnrecorded(t *testing.T) {
	g := newTestGateway(t, Config{})
	defer g.Close()
	store, err := OpenAccountStore(filepath.Join(t.TempDir(), "accounts.json"))
	if err != nil {
		t.Fatal(err)
	}
	g.accounts = store
	at := time.Date(2026, 10, 31, 22, 13, 37, 0, time.UTC)
	if err := store.HoldNode(NodeHold{Node: "n-recorded", Rule: "file-upload", At: at}); err != nil {
		t.Fatal(err)
	}
	g.unrec.add(NodeHold{Node: "n-recorded", Rule: "memory", At: at.Add(time.Minute)})
	g.unrec.add(NodeHold{Node: "n-inmemory", Rule: "credential-path", At: at.Add(2 * time.Minute)})
	got := g.holds()
	if len(got) != 2 || got[0].Node != "n-recorded" || got[0].Rule != "file-upload" || got[1].Node != "n-inmemory" {
		t.Fatalf("holds %+v", got)
	}
}

// The daily history's count of nodes online leaves held nodes out too.
func TestHistoryLeavesHeldNodesOut(t *testing.T) {
	g := newTestGateway(t, Config{HistorySample: 5 * time.Millisecond})
	defer g.Close()
	s := &stickyRig{t, g}
	g.mu.Lock()
	g.node["n-heldnode"] = &nodeConn{id: "n-heldnode", held: true, closed: make(chan struct{})}
	g.mu.Unlock()
	s.node("a", "op", true, 0)
	deadline := time.Now().Add(5 * time.Second)
	for {
		d := g.hist.public()
		if len(d) > 0 && d[len(d)-1]["nodes_peak"] == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("history %v: no sample of one node", d)
		}
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond) // several more samples
	d := g.hist.public()
	if peak := d[len(d)-1]["nodes_peak"]; peak != 1 {
		t.Fatalf("nodes_peak %v: a held node counted", peak)
	}
}
