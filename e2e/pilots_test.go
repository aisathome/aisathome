// Connector pilots end to end: a connector creates and binds a pilot, a
// real node agent connects with the pilot's credential, and the gateway routes, meters and ends it.
//
// SPDX-License-Identifier: Apache-2.0
package e2e

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/aisathome/aisathome/gateway"
	"github.com/aisathome/aisathome/node"
	"github.com/aisathome/aisathome/protocol"
)

const (
	connectorKey  = "ak-e2e-connector-key-0123456789abcdefghijkl"
	pilotSciKey   = "ak-e2e-pilot-science-key-0123456789abcdefgh"
	scienceOnlyID = "900" // in no_chat_users
)

// pilotRig: accounts, one science project whose agents may use volunteer nodes, a "boinc"
// connector, and no nodes.
func pilotRig(t *testing.T, chat bool) (*acctRig, *gateway.Config) {
	t.Helper()
	var cfg *gateway.Config
	a := newAcctRigNodes(t, 100, nil, func(c *gateway.Config) {
		cfg = c
		c.Projects = []gateway.Project{{ID: "lab", KeySHA256: []string{sha(pilotSciKey)}, AgentOK: true, PilotProbationOK: true}}
		c.Connectors = []gateway.Connector{{ID: "boinc", KeySHA256: []string{sha(connectorKey)}, MaxPilots: 5,
			PilotsChatEnabled: chat, NoChatUsers: []string{scienceOnlyID}}}
		c.QueueTimeoutSec = 2
	})
	return a, cfg
}

func (a *acctRig) connector(t *testing.T, method, path string, body any) map[string]any {
	t.Helper()
	b, _ := json.Marshal(body)
	if body == nil {
		b = nil
	}
	resp := a.do(t, method, "/v1/connectors/boinc"+path, connectorKey, string(b))
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode/100 != 2 {
		t.Fatalf("%s %s: HTTP %d: %s", method, path, resp.StatusCode, raw)
	}
	var out map[string]any
	json.Unmarshal(raw, &out)
	return out
}

// newPilot creates a pilot; it returns its id and credential.
func (a *acctRig) newPilot(t *testing.T) (string, string) {
	t.Helper()
	out := a.connector(t, "POST", "/pilots", map[string]any{"deadline": time.Now().Add(48 * time.Hour)})
	return out["pilot"].(map[string]any)["id"].(string), out["credential"].(string)
}

func (a *acctRig) pilotRecord(t *testing.T, id string) map[string]any {
	t.Helper()
	return a.connector(t, "GET", "/pilots/"+id, nil)["pilot"].(map[string]any)
}

// runPilot runs a node agent with only the pilot's credential, as `aisathome-node pilot` does.
func (a *acctRig) runPilot(ctx context.Context, cred string, engine string) *node.Agent {
	ag := node.New(node.Config{Token: cred, GatewayURL: "ws" + strings.TrimPrefix(a.gw.URL, "http") + "/v1/node/connect",
		Upstream: engine, HealthInterval: 50 * time.Millisecond,
		Models: []protocol.ModelInfo{{Name: "qwen3.8-27b-q4", Roles: []string{"deep"}, Slots: 1, Ctx: 32768}}}, quiet())
	go ag.Run(ctx)
	return ag
}

const deepBody = `{"model":"deep","messages":[{"role":"user","content":"hi"}]}`

func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(8 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if cond() {
			return
		}
	}
	t.Fatalf("timed out waiting for %s", what)
}

// A pilot is held until bound, then serves science; chat and agents stay off it until the
// connector enables chat on pilots (and its account's switch is on); its credit goes into its
// record, not the public ledger; it serves one connection at a time; the record continues over a
// resumed run; a final close reason ends it for good.
func TestPilotEndToEnd(t *testing.T) {
	a, saved := pilotRig(t, false)
	engine := mockEngine(t, false)
	t.Cleanup(engine.Close)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	id, cred := a.newPilot(t)
	runCtx, stopRun := context.WithCancel(ctx)
	ag := a.runPilot(runCtx, cred, engine.URL)
	time.Sleep(1200 * time.Millisecond)
	if n := a.g.NodeCount(); n != 0 {
		t.Fatalf("an unbound pilot connected (%d nodes)", n)
	}
	a.connector(t, "POST", "/pilots/"+id+"/bind", map[string]any{"user": "5", "host": "11", "team": "3"})
	waitUntil(t, "the bound pilot to connect", func() bool { return a.g.NodeCount() == 1 })
	if p := a.pilotRecord(t, id); p["state"] != gateway.PilotConnected || p["agent"] == "" {
		t.Fatalf("connected pilot's record: %v", p)
	}

	// Science: served by the pilot. Chat (an API key, agent class) and personal data: not.
	if code, body := ask(t, a.rig, pilotSciKey, deepBody); code != 200 || !strings.Contains(body, "Hello") {
		t.Fatalf("science on the pilot: %d %s", code, body)
	}
	if code, body := ask(t, a.rig, "user-key", deepBody); code != 503 {
		t.Fatalf("chat with pilots_chat_enabled off: %d %s", code, body)
	}
	pii := `{"model":"deep","messages":[{"role":"user","content":"mail me at someone@example.org"}]}`
	if code, _ := ask(t, a.rig, "user-key", pii); code != http.StatusBadRequest {
		t.Fatalf("personal data reached a pilot: %d", code)
	}
	p := a.pilotRecord(t, id)
	if p["requests"] != 1.0 || p["credit"].(float64) <= 0 {
		t.Fatalf("after one science answer: %v", p)
	}
	u := waitUsage(t, a.rig, 1)
	if u[0]["node"] != id || u[0]["class"] != gateway.ClassScience {
		t.Fatalf("usage: %v", u[0])
	}
	// Pilot credit is not in the public ledger.
	var board map[string]any
	getJSON(t, a.gw.URL+"/v1/stats/credit", &board)
	if c := board["total"].(map[string]any)["credit"]; c != 0.0 {
		t.Errorf("public ledger has pilot credit: %v", c)
	}

	// A second connection with the same credential is refused while the first is live.
	h := http.Header{"Authorization": {"Bearer " + cred}}
	c, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(a.gw.URL, "http")+"/v1/node/connect", &websocket.DialOptions{HTTPHeader: h})
	if err != nil {
		t.Fatal(err)
	}
	hello, _ := json.Marshal(protocol.Msg{Type: protocol.TypeHello, NodeID: id, Protocol: protocol.LegacyVersion,
		Models: []protocol.ModelInfo{{Name: "qwen3.8-27b-q4", Roles: []string{"deep"}, Slots: 1}}})
	c.Write(ctx, websocket.MessageText, hello)
	_, _, err = c.Read(ctx) // the welcome, then the close
	if err == nil {
		_, _, err = c.Read(ctx)
	}
	if !strings.Contains(err.Error(), "already connected") {
		t.Fatalf("second connection: %v", err)
	}
	if a.g.NodeCount() != 1 {
		t.Fatal("the second connection replaced the first")
	}

	// Chat on pilots enabled by the connector: chat reaches the pilot; the account's switch turns
	// it off again within a sweep.
	cfg := *saved
	cfg.Connectors = append([]gateway.Connector(nil), saved.Connectors...)
	cfg.Connectors[0].PilotsChatEnabled = true
	if err := a.g.Reload(cfg); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, "chat to reach the pilot", func() bool { code, _ := ask(t, a.rig, "user-key", deepBody); return code == 200 })
	if code, _ := ask(t, a.rig, "user-key", pii); code != http.StatusBadRequest {
		t.Fatalf("personal data reached a chat pilot: %d", code)
	}
	acct := ""
	a.store.Refresh() // the gateway wrote the unclaimed account
	for _, x := range a.store.Snapshot().Accounts {
		if x.Status == gateway.StatusUnclaimed {
			acct = x.ID
		}
	}
	if err := a.store.SetPilotChat(acct, false); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, "the switch to keep chat off the pilot", func() bool { code, _ := ask(t, a.rig, "user-key", deepBody); return code == 503 })
	if code, _ := ask(t, a.rig, pilotSciKey, deepBody); code != 200 {
		t.Fatal("science stopped with the chat switch")
	}

	// Suspend (sigterm) and resume: the same record continues.
	ag.Leave("pilot: sigterm", 2*time.Second)
	stopRun()
	waitUntil(t, "the pilot to leave", func() bool { return a.g.NodeCount() == 0 })
	waitUntil(t, "the record to say disconnected", func() bool { return a.pilotRecord(t, id)["state"] == gateway.PilotDisconnected })
	if p := a.pilotRecord(t, id); p["close_reason"] != "pilot: sigterm" {
		t.Fatalf("close reason %v", p["close_reason"])
	}
	runCtx, stopRun = context.WithCancel(ctx)
	ag = a.runPilot(runCtx, cred, engine.URL)
	waitUntil(t, "the resumed pilot", func() bool { return a.g.NodeCount() == 1 })
	if code, _ := ask(t, a.rig, pilotSciKey, deepBody); code != 200 {
		t.Fatal("science on the resumed pilot")
	}
	p = a.pilotRecord(t, id)
	if p["connections"] != 2.0 || p["requests"].(float64) < 4 {
		t.Fatalf("resumed record: %v", p)
	}
	// A final reason ends it; its credential no longer connects.
	ag.Leave("pilot: idle", 2*time.Second)
	stopRun()
	waitUntil(t, "the ended record", func() bool { return a.pilotRecord(t, id)["state"] == gateway.PilotEnded })
	runCtx, stopRun = context.WithCancel(ctx)
	defer stopRun()
	a.runPilot(runCtx, cred, engine.URL)
	time.Sleep(1500 * time.Millisecond)
	if a.g.NodeCount() != 0 {
		t.Fatal("an ended pilot connected again")
	}
	if cr := a.connector(t, "GET", "/pilots/"+id+"/credit", nil); cr["final"] != false || cr["final_at"] == nil {
		t.Fatalf("credit right after the end: %v", cr)
	}
}

// A pilot of a no_chat_users user serves science only even with chat on pilots enabled; reporting a
// pilot drops its connection.
func TestPilotNoChatUserAndReported(t *testing.T) {
	a, _ := pilotRig(t, true)
	engine := mockEngine(t, false)
	t.Cleanup(engine.Close)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	id, cred := a.newPilot(t)
	a.connector(t, "POST", "/pilots/"+id+"/bind", map[string]any{"user": scienceOnlyID, "host": "12"})
	a.runPilot(ctx, cred, engine.URL)
	waitUntil(t, "the pilot", func() bool { return a.g.NodeCount() == 1 })
	if code, _ := ask(t, a.rig, "user-key", deepBody); code != 503 {
		t.Fatalf("chat on a no_chat_users pilot: %d", code)
	}
	if code, _ := ask(t, a.rig, pilotSciKey, deepBody); code != 200 {
		t.Fatalf("science on a no_chat_users pilot: %d", code)
	}
	a.connector(t, "POST", "/pilots/"+id+"/reported", map[string]any{"outcome": "success"})
	waitUntil(t, "the reported pilot to be dropped", func() bool { return a.g.NodeCount() == 0 })
	if p := a.pilotRecord(t, id); p["state"] != gateway.PilotEnded || p["end_reason"] != "reported: success" {
		t.Fatalf("reported: %v", p)
	}
	d := a.connector(t, "GET", "/demand", nil)
	if d["pilots"].(map[string]any)["outstanding"] != 0.0 {
		t.Fatalf("demand after the end: %v", d)
	}
}
