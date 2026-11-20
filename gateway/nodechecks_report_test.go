// SPDX-License-Identifier: Apache-2.0
package gateway

import (
	"strings"
	"testing"
	"time"
)

func TestReadNodeChecks(t *testing.T) {
	now := time.Now().UTC()
	at := func(d time.Duration) string { return now.Add(-d).Format(time.RFC3339Nano) }
	log := strings.Join([]string{
		`{"time":"` + at(time.Hour) + `","node":"n-bad","operator":"op","outcome":"ok","voided":250,"checks":["speed","max-tokens"],"task":"t1","signals":{"tps":3000,"finish":"length","tool_calls":2,"bad_calls":1,"degenerate":true}}`,
		`{"time":"` + at(time.Hour) + `","node":"n-bad","outcome":"cancelled","credit":10,"task":"t1","signals":{"tps":40}}`,
		`{"time":"` + at(time.Hour) + `","node":"n-bad","outcome":"failed","checks":["speed"]}`,
		`{"time":"` + at(time.Hour) + `","node":"n-good","outcome":"ok","credit":20,"task":"t2","signals":{"tps":42},"discarded":[{"req_id":"r1","node":"n-bad"},{"req_id":"r2","node":"n-gone"}]}`,
		`{"time":"` + at(48*time.Hour) + `","node":"n-old","outcome":"ok","credit":5}`,
		`garbage`,
	}, "\n")
	rows, err := ReadNodeChecks(strings.NewReader(log), now.Add(-24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	by := map[string]NodeCheckRow{}
	for _, r := range rows {
		by[r.Node] = r
	}
	b := by["n-bad"]
	if len(rows) != 3 || rows[0].Node != "n-bad" || b.Answers != 2 || b.Failed != 1 || b.ByRule["speed"] != 1 || b.ByRule["max-tokens"] != 1 ||
		b.Voided != 250 || b.Credit != 10 || b.Length != 1 || b.ToolCalls != 2 || b.BadCalls != 1 || b.Degenerate != 1 ||
		b.Discarded != 1 || b.Tasks != 2 || b.DistinctTasks != 1 || b.TPSMax != 3000 || b.Operator != "op" || b.Rules() != "max-tokens 1, speed 1" {
		t.Fatalf("n-bad %+v (rows %+v)", b, rows)
	}
	if g := by["n-good"]; g.Answers != 1 || g.Discarded != 0 || g.TPS50 != 42 || g.Credit != 20 {
		t.Fatalf("n-good %+v", g)
	}
	if g := by["n-gone"]; g.Discarded != 1 || g.Answers != 0 {
		t.Fatalf("a node with only a discarded answer in the span: %+v", g)
	}
	if _, ok := by["n-old"]; ok {
		t.Fatal("a record before since counted")
	}
}
