// SPDX-License-Identifier: Apache-2.0
package gateway

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/aisathome/aisathome/protocol"
)

const shortProjectKey = "ak-short-project-key-24680135"

// A project's min_node_ctx is checked like its other limits.
func TestProjectMinNodeCtxChecked(t *testing.T) {
	if _, err := newProjectTable([]Project{{ID: "p", KeySHA256: []string{keyHash(shortProjectKey)}, MinNodeCtx: -1}}); err == nil {
		t.Fatal("a negative min_node_ctx was accepted")
	}
	pt, err := newProjectTable([]Project{{ID: "p", KeySHA256: []string{keyHash(shortProjectKey)}, MinNodeCtx: 131072}})
	if err != nil || pt.byID["p"].MinNodeCtx != 131072 {
		t.Fatalf("min_node_ctx 131072: %v", err)
	}
}

// pilot_tiers: names and contexts distinct and well formed, at most maxPilotTiers; kept smallest first.
func TestConnectorPilotTiersChecked(t *testing.T) {
	mk := func(tiers ...PilotTier) error {
		_, err := newConnectorTable([]Connector{{ID: "boinc", KeySHA256: []string{keyHash(testConnectorKey)}, MaxPilots: 1, PilotTiers: tiers}}, nil)
		return err
	}
	var many []PilotTier
	for i := range maxPilotTiers + 1 {
		many = append(many, PilotTier{fmt.Sprintf("t%d", i), 1000 + i})
	}
	for name, tiers := range map[string][]PilotTier{
		"empty name":      {{"", 81920}},
		"upper case":      {{"Big", 81920}},
		"zero ctx":        {{"big", 0}},
		"negative ctx":    {{"big", -1}},
		"same name":       {{"big", 81920}, {"big", 196608}},
		"same ctx":        {{"standard", 81920}, {"big", 81920}},
		"too many":        many,
		"too long a name": {{strings.Repeat("a", 33), 81920}},
	} {
		if mk(tiers...) == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	ct, err := newConnectorTable([]Connector{{ID: "boinc", KeySHA256: []string{keyHash(testConnectorKey)}, MaxPilots: 1,
		PilotTiers: []PilotTier{{"big", 196608}, {"standard", 81920}}}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	c := ct.byID["boinc"]
	if c.tiers[0].Name != "standard" || c.largestTier() != "big" {
		t.Fatalf("tiers not smallest first: %v", c.tiers)
	}
	if ct, _ = newConnectorTable([]Connector{{ID: "boinc", KeySHA256: []string{keyHash(testConnectorKey)}, MaxPilots: 1}}, nil); ct.byID["boinc"].largestTier() != "" {
		t.Fatal("a connector without tiers has a largest tier")
	}
}

// A project's floor keeps its requests for a role off models with less context; a request naming
// the model is not held to it; a model announcing no context is assumed to fit; a session bound to
// a smaller model cannot follow it there.
func TestMinNodeCtxRouting(t *testing.T) {
	s := newStickyRig(t)
	add := func(id string, m protocol.ModelInfo) {
		s.g.mu.Lock()
		s.g.node[id] = &nodeConn{id: id, operator: "op-" + id, pending: map[string]*pendingReq{}, closed: make(chan struct{}),
			models: []protocol.ModelInfo{m}}
		s.g.mu.Unlock()
	}
	add("small", protocol.ModelInfo{Name: "qwen3.8-27b-q3", Roles: []string{"agent"}, Slots: 1, Ctx: 81920})
	lab := &Project{ID: "lab", MinNodeCtx: 131072}
	pick := func(rt *route) string {
		t.Helper()
		n, _ := s.g.pick(rt)
		if n == nil {
			return ""
		}
		n.mu.Lock()
		n.inflight--
		n.mu.Unlock()
		return n.id
	}
	if got := pick(sciRoute("agent").forProject(lab)); got != "" {
		t.Fatalf("a role request under a floor of 131072 went to an 81,920 node (%s)", got)
	}
	if got := pick(sciRoute("agent").forProject(&Project{ID: "lab"})); got != "small" {
		t.Fatalf("without a floor: %q", got)
	}
	if got := pick(sciRoute("qwen3.8-27b-q3").forProject(lab)); got != "small" {
		t.Fatalf("a request naming the model is held to the floor: %q", got)
	}
	bound := sciRoute("agent").forProject(lab)
	bound.model = "qwen3.8-27b-q3"
	if got := pick(bound); got != "" {
		t.Fatalf("a session bound to the small model took it under the floor: %q", got)
	}
	add("big", protocol.ModelInfo{Name: "qwen3.8-27b", Roles: []string{"agent"}, Slots: 1, Ctx: 196608})
	if got := pick(sciRoute("agent").forProject(lab)); got != "big" {
		t.Fatalf("under the floor with a 196,608 node: %q", got)
	}
	s.g.mu.Lock()
	delete(s.g.node, "big")
	s.g.mu.Unlock()
	add("unknown", protocol.ModelInfo{Name: "x", Roles: []string{"agent"}, Slots: 1})
	if got := pick(sciRoute("agent").forProject(lab)); got != "unknown" {
		t.Fatalf("a model with no context announced is not assumed to fit: %q", got)
	}
}

// With pilot_tiers, a connector may bring a pilot for a request only if a tier clears its project's
// floor; without tiers, as before.
func TestPilotTiersMayServe(t *testing.T) {
	s := pilotModelsRig(t, func(c *Connector) { c.PilotTiers = []PilotTier{{"standard", 81920}, {"big", 196608}} })
	for floor, want := range map[int]bool{0: true, 131072: true, 196608: true, 300000: false} {
		if got := s.g.pilotsMayServe(sciRoute("deep").forProject(&Project{ID: "lab", MinNodeCtx: floor})); got != want {
			t.Errorf("floor %d: %v, want %v", floor, got, want)
		}
	}
	setConnector(t, s.g, nil)
	if !s.g.pilotsMayServe(sciRoute("deep").forProject(&Project{ID: "lab", MinNodeCtx: 300000})) {
		t.Error("without tiers a floor refused the pilots")
	}
}

// pilotTierRig is a pilot rig whose connector has a standard and a big tier, and a second agent project
// ("short", no floor) beside lab, whose floor is 131072.
func pilotTierRig(t *testing.T, mod func(*Config)) *pilotRig {
	return newPilotRig(t, func(c *Config) {
		c.Connectors[0].MaxPilots, c.Connectors[0].MintPerHour, c.Connectors[0].UnsentBuffer = 20, 100, 10
		c.Connectors[0].PilotTiers = []PilotTier{{"standard", 81920}, {"big", 196608}}
		c.Projects[0].MinNodeCtx = 131072
		// short first in the config: the floor, not the order, decides who takes a slot first.
		c.Projects = append([]Project{{ID: "short", KeySHA256: []string{keyHash(shortProjectKey)}, AgentOK: true}}, c.Projects...)
		if mod != nil {
			mod(c)
		}
	})
}

// sessions gives project n open tasks and n workers seen: n sessions.
func (r *pilotRig) sessions(project, key, kind string, n int) {
	r.t.Helper()
	tr := &taskRig{t: r.t, g: r.g}
	for i := range n {
		tr.call("POST", "/v1/projects/"+project+"/tasks", key, newTask(fmt.Sprintf("%s-%d", project, i), kind))
		r.g.tasks.mu.Lock()
		r.g.tasks.sawWorker(project, fmt.Sprintf("%s-w%d", project, i), r.clock())
		r.g.tasks.mu.Unlock()
	}
}

func (r *pilotRig) demandNow() map[string]any {
	r.t.Helper()
	return r.want(200, "GET", "/v1/connectors/boinc/demand", testConnectorKey, nil)
}

// tierNum reads one number of one tier of a demand answer.
func tierNum(t *testing.T, d map[string]any, tier, k string) int {
	t.Helper()
	ts, ok := d["tiers"].(map[string]any)
	if !ok {
		t.Fatalf("no tiers in %v", d)
	}
	return int(ts[tier].(map[string]any)[k].(float64))
}

func dnum(d map[string]any, k string) int { return int(d[k].(float64)) }

// checkSums: the top-level pilots_wanted, cancel_unsent and target are the tiers' sums.
func checkSums(t *testing.T, d map[string]any) {
	t.Helper()
	for _, k := range []string{"pilots_wanted", "cancel_unsent", "target"} {
		sum := 0
		for _, tier := range []string{"standard", "big"} {
			sum += tierNum(t, d, tier, k)
		}
		if sum != dnum(d, k) {
			t.Fatalf("%s: tiers sum to %d, top level %d: %v", k, sum, dnum(d, k), d)
		}
	}
}

func (r *pilotRig) createTier(tier string) string {
	r.t.Helper()
	body := map[string]any{"deadline": r.clock().Add(48 * time.Hour)}
	if tier != "-" {
		body["tier"] = tier
	}
	out := r.want(201, "POST", pilotsPath, testConnectorKey, body)
	return out["pilot"].(map[string]any)["id"].(string)
}

// The demand per tier: a project with a floor asks for the large tier only, one without for the
// smallest; their sums are the top-level numbers.
func TestPilotDemandTiers(t *testing.T) {
	t.Run("floor: big only", func(t *testing.T) {
		r := pilotTierRig(t, func(c *Config) {
			c.NodeTokens = map[string]NodeToken{"home-token": {Operator: "owner"}}
			c.Projects[0].AgentOK = false // short: its agents would count the small node
		})
		r.sessions("lab", testProjectKey, "p2m-v7", 4)
		// A small node online is no slot for lab.
		r.g.mu.Lock()
		r.g.node["home-small"] = &nodeConn{id: "home-small", token: "home-token", operator: "owner",
			models: []protocol.ModelInfo{{Name: "qwen3.8-27b-q3", Roles: []string{"agent"}, Slots: 1, Ctx: 81920}}}
		r.g.mu.Unlock()
		d := r.demandNow()
		if dnum(d["science"].(map[string]any), "slots_online") != 0 {
			t.Fatalf("a small node counted as a slot for a project with a floor: %v", d)
		}
		if tierNum(t, d, "big", "pilots_wanted") != 2 || tierNum(t, d, "standard", "pilots_wanted") != 0 || tierNum(t, d, "big", "ctx") != 196608 {
			t.Fatalf("%v", d)
		}
		checkSums(t, d)
	})
	t.Run("no floor: standard", func(t *testing.T) {
		r := pilotTierRig(t, nil)
		r.sessions("short", shortProjectKey, "other", 4)
		d := r.demandNow()
		if tierNum(t, d, "standard", "pilots_wanted") != 2 || tierNum(t, d, "big", "pilots_wanted") != 0 {
			t.Fatalf("%v", d)
		}
		checkSums(t, d)
	})
	t.Run("mixed, and chat to the smallest tier", func(t *testing.T) {
		r := pilotTierRig(t, func(c *Config) { c.Connectors[0].PilotsChatEnabled, c.Connectors[0].ChatHeadroom = true, 1 })
		r.sessions("lab", testProjectKey, "p2m-v7", 3)
		r.sessions("short", shortProjectKey, "other", 2)
		d := r.demandNow()
		if tierNum(t, d, "big", "target") != 2 || tierNum(t, d, "standard", "target") != 2 || dnum(d, "pilots_wanted") != 4 {
			t.Fatalf("%v", d)
		}
		checkSums(t, d)
	})
	t.Run("a floor no tier clears asks for nothing", func(t *testing.T) {
		r := pilotTierRig(t, func(c *Config) { c.Projects[1].MinNodeCtx = 300000 })
		r.sessions("lab", testProjectKey, "p2m-v7", 4)
		if d := r.demandNow(); dnum(d, "target") != 0 || dnum(d["science"].(map[string]any), "sessions") != 4 {
			t.Fatalf("%v", d)
		}
	})
	t.Run("slots: the highest floor first, on the least context", func(t *testing.T) {
		r := pilotTierRig(t, func(c *Config) { c.NodeTokens = map[string]NodeToken{"home-token": {Operator: "owner"}} })
		r.g.mu.Lock()
		r.g.node["home-big"] = &nodeConn{id: "home-big", token: "home-token", operator: "owner",
			models: []protocol.ModelInfo{{Name: "qwen3.8-27b", Roles: []string{"agent"}, Slots: 1, Ctx: 196608}}}
		r.g.mu.Unlock()
		r.sessions("short", shortProjectKey, "other", 2)
		r.sessions("lab", testProjectKey, "p2m-v7", 2)
		// lab (floor) takes the big node's two sessions; short needs one standard pilot.
		d := r.demandNow()
		if tierNum(t, d, "big", "target") != 0 || tierNum(t, d, "standard", "target") != 1 {
			t.Fatalf("one big node: %v", d)
		}
		// A small node: short goes there, lab keeps the big one.
		r.g.mu.Lock()
		r.g.node["home-small"] = &nodeConn{id: "home-small", token: "home-token", operator: "owner",
			models: []protocol.ModelInfo{{Name: "qwen3.8-27b-q3", Roles: []string{"agent"}, Slots: 1, Ctx: 81920}}}
		r.g.mu.Unlock()
		if d = r.demandNow(); dnum(d, "target") != 0 || dnum(d["science"].(map[string]any), "slots_online") != 2 {
			t.Fatalf("a big and a small node: %v", d)
		}
		// The small node alone: it is no slot for lab (floor) and serves short; lab needs a big pilot.
		r.g.mu.Lock()
		delete(r.g.node, "home-big")
		r.g.mu.Unlock()
		if d = r.demandNow(); tierNum(t, d, "big", "target") != 1 || tierNum(t, d, "standard", "target") != 0 {
			t.Fatalf("one small node: %v", d)
		}
	})
	t.Run("caps cut the smallest tier first", func(t *testing.T) {
		r := pilotTierRig(t, func(c *Config) { c.Connectors[0].MaxPilots = 3 })
		r.sessions("lab", testProjectKey, "p2m-v7", 4)
		r.sessions("short", shortProjectKey, "other", 4)
		d := r.demandNow()
		if dnum(d, "target") != 3 || d["capped_by"] != "max_pilots" || tierNum(t, d, "big", "target") != 2 || tierNum(t, d, "standard", "target") != 1 {
			t.Fatalf("%v", d)
		}
		checkSums(t, d)
		r.g.spendCap = func(string) (int, bool) { return 1, true }
		if d = r.demandNow(); tierNum(t, d, "big", "target") != 1 || tierNum(t, d, "standard", "target") != 0 || d["capped_by"] != "spend" {
			t.Fatalf("spend cap 1: %v", d)
		}
	})
	t.Run("a big surplus covers standard; cancels per tier", func(t *testing.T) {
		r := pilotTierRig(t, nil)
		r.sessions("short", shortProjectKey, "other", 2) // one standard pilot
		big := r.createTier("big")
		d := r.demandNow()
		if dnum(d, "pilots_wanted") != 0 || dnum(d, "cancel_unsent") != 0 || tierNum(t, d, "big", "outstanding") != 1 || tierNum(t, d, "big", "unsent") != 1 {
			t.Fatalf("an unsent big pilot covers the standard need: %v", d)
		}
		checkSums(t, d)
		// Two unsent standard pilots now: the standard need is met by its own; both big and one
		// standard are surplus, and only unsent ones are cancelled.
		r.createTier("standard")
		r.createTier("standard")
		r.bind(big, "5")
		d = r.demandNow()
		if tierNum(t, d, "standard", "cancel_unsent") != 1 || tierNum(t, d, "big", "cancel_unsent") != 0 || dnum(d, "pilots_wanted") != 0 {
			t.Fatalf("surplus: %v", d)
		}
		checkSums(t, d)
	})
	t.Run("a standard surplus never covers big", func(t *testing.T) {
		r := pilotTierRig(t, nil)
		r.sessions("lab", testProjectKey, "p2m-v7", 2) // one big pilot
		r.createTier("standard")
		d := r.demandNow()
		if tierNum(t, d, "big", "pilots_wanted") != 1 || tierNum(t, d, "standard", "cancel_unsent") != 1 {
			t.Fatalf("%v", d)
		}
		checkSums(t, d)
	})
	t.Run("the shared caps go to the largest tier first", func(t *testing.T) {
		r := pilotTierRig(t, func(c *Config) { c.Connectors[0].UnsentBuffer = 1 })
		r.sessions("lab", testProjectKey, "p2m-v7", 2)
		r.sessions("short", shortProjectKey, "other", 2)
		d := r.demandNow()
		if tierNum(t, d, "big", "pilots_wanted") != 1 || tierNum(t, d, "standard", "pilots_wanted") != 0 {
			t.Fatalf("%v", d)
		}
		checkSums(t, d)
	})
	t.Run("max_pilots bounds what is wanted on top of the outstanding", func(t *testing.T) {
		r := pilotTierRig(t, func(c *Config) { c.Connectors[0].MaxPilots = 1 })
		r.sessions("lab", testProjectKey, "p2m-v7", 2)
		r.createTier("standard")
		if d := r.demandNow(); dnum(d, "pilots_wanted") != 0 || tierNum(t, d, "standard", "cancel_unsent") != 1 {
			t.Fatalf("%v", d)
		}
	})
}

// A pilot's tier: the one asked for, the largest for an older connector that names none, refused
// when unknown or when the connector has no tiers; shown in the pilot's record and list.
func TestPilotCreateTier(t *testing.T) {
	r := pilotTierRig(t, nil)
	id := r.createTier("standard")
	if p := r.want(200, "GET", pilotsPath+"/"+id, testConnectorKey, nil)["pilot"].(map[string]any); p["tier"] != "standard" {
		t.Fatalf("record: %v", p)
	}
	old := r.createTier("-")
	if p, _ := r.g.sci.pilot("boinc", old); p.Tier != "big" {
		t.Fatalf("no tier given: %q", p.Tier)
	}
	list := r.want(200, "GET", pilotsPath, testConnectorKey, nil)["pilots"].([]any)
	if len(list) != 2 || list[0].(map[string]any)["tier"] == nil {
		t.Fatalf("list: %v", list)
	}
	if w, _ := r.call("POST", pilotsPath, testConnectorKey, map[string]any{"deadline": r.clock().Add(time.Hour), "tier": "huge"}, ""); w.Code != 400 {
		t.Fatalf("unknown tier: HTTP %d", w.Code)
	}
	plain := newPilotRig(t, nil)
	if w, _ := plain.call("POST", pilotsPath, testConnectorKey, map[string]any{"deadline": plain.clock().Add(time.Hour), "tier": "big"}, ""); w.Code != 400 {
		t.Fatalf("a tier for a connector without tiers: HTTP %d", w.Code)
	}
	if d := plain.demandNow(); d["tiers"] != nil {
		t.Fatalf("tiers in the demand of a connector without them: %v", d)
	}
}

// A pilot whose tier the config no longer has counts as the largest tier.
func TestPilotDemandRemovedTier(t *testing.T) {
	r := pilotTierRig(t, nil)
	if _, err := r.g.sci.db.Exec(`INSERT INTO pilots (id, connector, cred_sha256, state, deadline, created_at, tier)
		VALUES ('p-0000000000aa', 'boinc', 'h', 'created', ?, ?, 'gone')`, ptime(r.clock().Add(time.Hour)), ptime(r.clock())); err != nil {
		t.Fatal(err)
	}
	if d := r.demandNow(); tierNum(t, d, "big", "unsent") != 1 || tierNum(t, d, "big", "cancel_unsent") != 1 {
		t.Fatalf("%v", d)
	}
}

// A science database from before tiers (version 3) gets the column; its pilots have none.
func TestScienceDBTierMigration(t *testing.T) {
	all := scienceMigrations
	defer func() { scienceMigrations = all }()
	dir := t.TempDir()
	scienceMigrations = all[:3]
	s, err := openScienceDB(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT INTO pilots (id, connector, cred_sha256, state, deadline, created_at)
		VALUES ('p-000000000001', 'boinc', 'h', 'created', '2099-01-01T00:00:00.000Z', '2026-11-18T12:00:00.000Z')`); err != nil {
		t.Fatal(err)
	}
	s.close()
	scienceMigrations = all
	if s, err = openScienceDB(dir); err != nil {
		t.Fatal(err)
	}
	defer s.close()
	if v, _ := s.schemaVersion(); v != len(all) {
		t.Fatalf("version %d", v)
	}
	p, err := s.pilot("boinc", "p-000000000001")
	if err != nil || p.Tier != "" {
		t.Fatalf("old pilot: %+v %v", p, err)
	}
	p, _, err = s.createPilot("boinc", "standard", time.Now().Add(time.Hour), 5, 5, time.Now())
	if err != nil || p.Tier != "standard" {
		t.Fatalf("new pilot: %+v %v", p, err)
	}
	by, _, err := s.pilotCounts("boinc", time.Now())
	if err != nil || by[""][PilotCreated] != 1 || by["standard"][PilotCreated] != 1 {
		t.Fatalf("counts: %v %v", by, err)
	}
}

// The pressure sample counts for a project only the nodes with the context it asks of a node.
func TestPressureNowMinNodeCtx(t *testing.T) {
	g := newTestGateway(t, Config{StateDir: t.TempDir(), Projects: []Project{
		{ID: "lab", KeySHA256: []string{keyHash(testProjectKey)}, AgentOK: true, PublicQueue: true, MinNodeCtx: 131072},
	}})
	defer g.Close()
	for id, ctx := range map[string]int{"small": 81920, "big": 196608} {
		g.mu.Lock()
		g.node[id] = &nodeConn{id: id, operator: "op-" + id, pending: map[string]*pendingReq{}, closed: make(chan struct{}),
			models: []protocol.ModelInfo{{Name: "m-" + id, Roles: []string{"agent"}, Slots: 1, Ctx: ctx}}}
		g.mu.Unlock()
	}
	if s := g.pressureNow(); s.Slots != 2 || s.ScienceSlots != 1 {
		t.Fatalf("slots %d, science slots %d: want 2 and 1", s.Slots, s.ScienceSlots)
	}
}
