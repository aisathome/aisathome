// SPDX-License-Identifier: Apache-2.0
package gateway

import (
	"slices"
	"testing"
	"time"
)

func rules(ws []healthWarning) []string {
	var out []string
	for _, w := range ws {
		out = append(out, w.Rule)
	}
	return out
}

func stored(st *runnerStatus, received time.Time) *storedStatus {
	return &storedStatus{Project: "tauceti", ReceivedAt: received, Status: st}
}

func tp(s string) *time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return &t
}

// Recorded case: trial 2's T2-NEW-1.a-A (raw/2026-10-24/T2-NEW-1.a-A in the research repo),
// replayed by the runner's reporter (tools/status_report.py --replay ... --at ...):
//   - 18:10 UTC, step 10: 2.38 min per step against the NEW median 0.344 (8 attempts): slow;
//     23.8 of 120 budget minutes: no budget warning yet;
//   - 19:31, step 26: 104.8 of 120 minutes (87%) with 26 of 80 steps: budget, and still slow;
//   - 19:44:30: no session event since 19:33:55 (the runner was down): stalled.
func TestHealthT2New1Recorded(t *testing.T) {
	for _, c := range []struct {
		file      string
		want, not []string
	}{
		{"status-t2-new-1-1810.json", []string{ruleSlow}, []string{ruleBudget, ruleStalled}},
		{"status-t2-new-1-1931.json", []string{ruleSlow, ruleBudget}, []string{ruleStalled}},
		{"status-t2-new-1-1944.json", []string{ruleStalled, ruleBudget}, nil},
	} {
		st := loadStatusFixture(t, c.file)
		got := rules(health(stored(st, st.GeneratedAt), 1, st.GeneratedAt))
		for _, r := range c.want {
			if !slices.Contains(got, r) {
				t.Errorf("%s: %s did not fire: %v", c.file, r, got)
			}
		}
		for _, r := range c.not {
			if slices.Contains(got, r) {
				t.Errorf("%s: %s fired: %v", c.file, r, got)
			}
		}
	}
}

// Each rule fires on a simulated case and stays quiet just inside its threshold; a warning
// clears when its cause is gone.
func TestHealthRules(t *testing.T) {
	now := *tp("2026-10-24T20:40:00Z")
	base := func() *runnerStatus {
		return &runnerStatus{Schema: 1, GeneratedAt: now, Runner: statusRunner{Service: "active"},
			Baselines: map[string]statusBaseline{"NS": {MinPerStep: 0.5, Attempts: 3}}}
	}
	run := func(a statusAttempt) statusAttempt {
		a.State, a.Attempt, a.TaskType = "running", "T2-NS-2.a-A", "NS"
		return a
	}
	cases := []struct {
		name  string
		rule  string
		fires func(*runnerStatus)
		quiet func(*runnerStatus)
		nodes int
	}{
		{name: "stalled", rule: ruleStalled,
			fires: func(s *runnerStatus) {
				s.Attempts = []statusAttempt{run(statusAttempt{LastActivity: tp("2026-10-24T20:30:00Z")})}
			},
			quiet: func(s *runnerStatus) {
				s.Attempts = []statusAttempt{run(statusAttempt{LastActivity: tp("2026-10-24T20:31:00Z")})}
			}},
		{name: "stalled: an interrupted attempt is not", rule: ruleStalled,
			fires: func(s *runnerStatus) {
				s.Attempts = []statusAttempt{run(statusAttempt{LastActivity: tp("2026-10-24T19:00:00Z")})}
			},
			quiet: func(s *runnerStatus) {
				a := run(statusAttempt{LastActivity: tp("2026-10-24T19:00:00Z")})
				a.State = "interrupted"
				s.Attempts = []statusAttempt{a}
			}},
		{name: "slow", rule: ruleSlow,
			fires: func(s *runnerStatus) { s.Attempts = []statusAttempt{run(statusAttempt{Steps: 10, AwakeMin: 10.1})} },
			quiet: func(s *runnerStatus) { s.Attempts = []statusAttempt{run(statusAttempt{Steps: 10, AwakeMin: 10})} }},
		{name: "slow needs a few steps", rule: ruleSlow,
			fires: func(s *runnerStatus) { s.Attempts = []statusAttempt{run(statusAttempt{Steps: 5, AwakeMin: 20})} },
			quiet: func(s *runnerStatus) { s.Attempts = []statusAttempt{run(statusAttempt{Steps: 4, AwakeMin: 20})} }},
		{name: "budget", rule: ruleBudget,
			fires: func(s *runnerStatus) {
				s.Attempts = []statusAttempt{run(statusAttempt{Steps: 39, StepCap: 80, UsedMin: 48, BudgetMin: 60, AwakeMin: 1})}
			},
			quiet: func(s *runnerStatus) {
				s.Attempts = []statusAttempt{run(statusAttempt{Steps: 40, StepCap: 80, UsedMin: 48, BudgetMin: 60, AwakeMin: 1})}
			}},
		// Recorded times: three queue-service stops on 2026-10-24 (raw/2026-10-24/runner/service.jsonl).
		{name: "interruptions", rule: ruleInterrupted,
			fires: func(s *runnerStatus) {
				s.Interruptions = []statusInterruption{
					{At: *tp("2026-10-24T20:23:42Z"), Attempt: "T2-NS-2.a-A", By: "service stop"},
					{At: *tp("2026-10-24T20:23:43Z"), Attempt: "T2-GEN-1.a-A", By: "service stop"},
					{At: *tp("2026-10-24T20:30:32Z"), Attempt: "T2-NS-2.a-A", By: "service stop"},
					{At: *tp("2026-10-24T20:36:08Z"), Attempt: "T2-NS-2.a-A", By: "service stop"}}
			},
			quiet: func(s *runnerStatus) { // one stop (two attempts at once) and one older than 2 h
				s.Interruptions = []statusInterruption{
					{At: *tp("2026-10-24T18:30:00Z"), By: "reboot"},
					{At: *tp("2026-10-24T20:23:42Z"), Attempt: "T2-NS-2.a-A", By: "service stop"},
					{At: *tp("2026-10-24T20:23:43Z"), Attempt: "T2-GEN-1.a-A", By: "service stop"}}
			}},
		{name: "CI back-off", rule: ruleCIBackoff,
			fires: func(s *runnerStatus) {
				s.CI = &statusCI{Pause: true, Reasons: []string{"pr-build queue: 312 runs queued (last 6 h)"}, Since: tp("2026-10-24T17:39:00Z"), CheckedAt: now}
			},
			quiet: func(s *runnerStatus) {
				s.CI = &statusCI{Pause: true, Since: tp("2026-10-24T17:41:00Z"), CheckedAt: now}
			}},
		{name: "CI back-off cleared", rule: ruleCIBackoff,
			fires: func(s *runnerStatus) {
				s.CI = &statusCI{Pause: true, Since: tp("2026-10-24T10:00:00Z"), CheckedAt: now}
			},
			quiet: func(s *runnerStatus) {
				s.CI = &statusCI{Pause: false, Since: tp("2026-10-24T10:00:00Z"), CheckedAt: now}
			}},
		{name: "contract", rule: ruleContract,
			fires: func(s *runnerStatus) {
				s.Nodes = []statusNode{{Node: "n-55045025", ContractEnds: tp("2026-10-26T20:00:00Z")}}
			},
			quiet: func(s *runnerStatus) {
				s.Nodes = []statusNode{{Node: "n-55045025", ContractEnds: tp("2027-01-01T13:17:00Z")}}
			}},
		{name: "contract ended", rule: ruleContract,
			fires: func(s *runnerStatus) {
				s.Nodes = []statusNode{{Node: "n-f09b774b", ContractEnds: tp("2026-10-24T20:12:00Z")}}
			},
			quiet: func(s *runnerStatus) { s.Nodes = []statusNode{{Node: "n-f09b774b"}} }},
		{name: "candidate waiting", rule: ruleCandidate,
			fires: func(s *runnerStatus) {
				s.Candidates = []statusCandidate{{Attempt: "G1a.a-F3c", Since: *tp("2026-10-23T20:30:00Z")}}
			},
			quiet: func(s *runnerStatus) {
				s.Candidates = []statusCandidate{{Attempt: "G1a.a-F3c", Since: *tp("2026-10-23T20:50:00Z")}}
			}},
	}
	for _, c := range cases {
		nodes := 1
		s := base()
		c.fires(s)
		if got := rules(health(stored(s, now), nodes, now)); !slices.Contains(got, c.rule) {
			t.Errorf("%s: %s did not fire: %v", c.name, c.rule, got)
		}
		s = base()
		c.quiet(s)
		if got := rules(health(stored(s, now), nodes, now)); slices.Contains(got, c.rule) {
			t.Errorf("%s: %s fired on the quiet case: %v", c.name, c.rule, got)
		}
	}
	// From the gateway's own data: no node online.
	if got := rules(health(stored(base(), now), 0, now)); !slices.Contains(got, ruleNoNodes) {
		t.Errorf("no nodes: %v", got)
	}
	if got := health(stored(base(), now), 1, now); len(got) != 0 {
		t.Errorf("a healthy status warns: %v", got)
	}
	// No status yet, and a status that stopped coming.
	if got := rules(health(nil, 1, now)); !slices.Equal(got, []string{ruleStatusAbsent}) {
		t.Errorf("no status: %v", got)
	}
	if got := rules(health(stored(base(), now.Add(-11*time.Minute)), 1, now)); !slices.Contains(got, ruleStatusStale) {
		t.Errorf("stale status: %v", got)
	}
}
