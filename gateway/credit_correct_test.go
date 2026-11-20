// SPDX-License-Identifier: Apache-2.0
package gateway

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
	"time"
)

// A correction asked for with the CLI is applied once, to the node and to whoever it earned for,
// kept in the ledger and shown on the leaderboard with its reason, never whose it is.
func TestCreditCorrection(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	g := newTestGateway(t, Config{StateDir: dir})
	g.credit.grant("n-acct", "gh:42", "", 1000, time.Time{}, time.Time{}, now)
	g.credit.grant("n-acct2", "gh:42", "", 500, time.Time{}, time.Time{}, now)
	g.credit.grant("n-maint", "", "op-secret", 300, time.Time{}, time.Time{}, now)
	g.credit.grant("n-other", "gh:7", "", 200, time.Time{}, time.Time{}, now)
	if err := g.credit.save(); err != nil {
		t.Fatal(err)
	}
	const why = "count bug 2026-10-15..11-14, fixed in #52"
	for _, bad := range []struct {
		node, reason string
		credit       float64
	}{
		{"n-acct", why, 0},
		{"n-acct", why, math.NaN()},
		{"n-acct", "", -1},
		{"n-acct", "two\nlines", -1},
		{"n-acct", strings.Repeat("x", maxReason+1), -1},
		{"n-none", why, -1},
		{"n-acct", why, -1000.5}, // more than the node has
	} {
		if _, err := AddCreditCorrection(dir, bad.node, bad.credit, bad.reason, now); err == nil {
			t.Errorf("%+v: accepted", bad)
		}
	}
	c1, err := AddCreditCorrection(dir, "n-acct", -100.5, why, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := AddCreditCorrection(dir, "n-acct", -900, why, now); err == nil {
		t.Error("pending corrections are not counted against the node's credit")
	}
	if _, err := AddCreditCorrection(dir, "n-maint", -20.625, why, now); err != nil {
		t.Fatal(err)
	}
	_, pending, err := ReadCreditCorrections(dir)
	if err != nil || len(pending) != 2 {
		t.Fatalf("pending %v, %v", pending, err)
	}

	// The running gateway applies them (and again does nothing).
	for range 2 {
		g.applyCreditCorrections()
	}
	near := func(got, want float64) bool { return math.Abs(got-want) < 1e-9 }
	if n, a := g.credit.node("n-acct"), g.credit.account("gh:42"); !near(n.Total, 899.5) || !near(a.Total, 1399.5) {
		t.Fatalf("node %v, account %v; want 899.5 and 1399.5", n.Total, a.Total)
	}
	if n, o := g.credit.node("n-maint"), g.credit.snapshot().Operators["op-secret"]; !near(n.Total, 279.375) || !near(o.Total, 279.375) {
		t.Fatalf("maintainer node %v, operator %v", n.Total, o.Total)
	}
	if n, a := g.credit.node("n-other"), g.credit.account("gh:7"); n.Total != 200 || a.Total != 200 {
		t.Fatalf("another account changed: %v, %v", n.Total, a.Total)
	}
	if e := g.credit.account("gh:42"); !near(e.TodayAt(now), 1500) {
		t.Fatalf("today's credit changed: %v", e.TodayAt(now))
	}
	snap := g.credit.snapshot()
	if len(snap.Corrections) != 2 || snap.Corrections[0].ID != c1.ID || snap.Corrections[0].Account != "gh:42" ||
		snap.Corrections[1].Operator != "op-secret" || snap.Corrections[0].Applied.IsZero() {
		t.Fatalf("ledger corrections: %+v", snap.Corrections)
	}

	// Public: when, how much, why; no node, account or operator.
	board := g.leaderboard(now)
	raw := string(mustJSON(board))
	for _, secret := range []string{"gh:42", "n-acct", "n-maint", "op-secret", c1.ID} {
		if strings.Contains(raw, secret) {
			t.Errorf("the leaderboard names %q: %s", secret, raw)
		}
	}
	var pub struct {
		Corrections []struct {
			Credit float64 `json:"credit"`
			Reason string  `json:"reason"`
			Time   string  `json:"time"`
		} `json:"corrections"`
		Total struct {
			Credit float64 `json:"credit"`
		} `json:"total"`
	}
	json.Unmarshal([]byte(raw), &pub)
	if len(pub.Corrections) != 2 || pub.Corrections[0].Credit != -100.5 || pub.Corrections[0].Reason != why || pub.Corrections[0].Time == "" {
		t.Fatalf("public corrections: %+v", pub.Corrections)
	}
	if !near(pub.Total.Credit, 2000-121.1) { // rounded to a tenth
		t.Fatalf("total %v", pub.Total.Credit)
	}

	// Saved, it is applied and no longer pending; a new request drops applied ones from the file.
	if err := g.Close(); err != nil {
		t.Fatal(err)
	}
	applied, pending, err := ReadCreditCorrections(dir)
	if err != nil || len(applied) != 2 || len(pending) != 0 {
		t.Fatalf("applied %d, pending %d, %v", len(applied), len(pending), err)
	}
	if _, err := AddCreditCorrection(dir, "n-other", 10, "a credit withheld by mistake", now); err != nil {
		t.Fatal(err)
	}
	reqs, _ := readCorrections(dir + "/" + CreditCorrectionsFile)
	if len(reqs) != 1 {
		t.Fatalf("the request file keeps %d requests, want the 1 pending", len(reqs))
	}

	// A restarted gateway applies the pending one at start, and the old ones not again.
	g = newTestGateway(t, Config{StateDir: dir})
	defer g.Close()
	if n := g.credit.node("n-other"); n.Total != 210 {
		t.Fatalf("pending correction at start: %v", n.Total)
	}
	if n := g.credit.node("n-acct"); !near(n.Total, 899.5) {
		t.Fatalf("applied twice: %v", n.Total)
	}
}

// A request the ledger cannot take (a node with no credit, or one that would go below zero) stays
// pending and changes nothing.
func TestCreditCorrectionRefused(t *testing.T) {
	l := newCreditLedger()
	now := time.Now()
	l.grant("n-a", "gh:1", "", 10, time.Time{}, time.Time{}, now)
	for _, c := range []CreditCorrection{
		{ID: "c1", Node: "n-b", Credit: -1, Reason: "x"},
		{ID: "c2", Node: "n-a", Credit: -11, Reason: "x"},
		{ID: "c3", Node: "n-a", Credit: -1, Reason: ""},
		{ID: "", Node: "n-a", Credit: -1, Reason: "x"},
	} {
		if ok, err := l.correct(c, now); ok || err == nil {
			t.Errorf("%+v: applied", c)
		}
	}
	if l.node("n-a").Total != 10 || len(l.f.Corrections) != 0 {
		t.Fatal("a refused correction changed the ledger")
	}
}
