// SPDX-License-Identifier: Apache-2.0
package gateway

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
	"time"
)

// autoRig: prove2me with campaign_auto_finish (labels trial and campaign, the default idle time) and
// lab without it, on a test clock that tasks, reopens and sweeps share.
func autoRig(t *testing.T, af *CampaignAutoFinish) *taskRig {
	t.Helper()
	g := newTestGateway(t, Config{StateDir: t.TempDir(), Projects: []Project{
		{ID: "prove2me", KeySHA256: []string{keyHash(testProjectKey)}, PublicQueue: true, Kinds: []string{"p2m-v7"}, CampaignAutoFinish: af},
		{ID: "lab", KeySHA256: []string{keyHash(otherProjectKey)}},
	}})
	r := &taskRig{t: t, g: g, now: time.Date(2026, 11, 24, 18, 0, 0, 0, time.UTC)}
	g.tasks.now = func() time.Time { return r.now }
	t.Cleanup(func() { g.Close() })
	return r
}

func p2mAuto() *CampaignAutoFinish { return &CampaignAutoFinish{Labels: []string{"trial", "campaign"}} }

func projectKey(project string) string {
	if project == "lab" {
		return otherProjectKey
	}
	return testProjectKey
}

// putCamp stores a campaign of a project at the rig's time.
func (r *taskRig) putCamp(project, id, status string, started *time.Time) *Campaign {
	r.t.Helper()
	c, _, err := r.g.sci.putCampaign(&Campaign{Project: project, ID: id, Title: "Trial", Status: status, StartedAt: started}, nil, r.now, nil)
	if err != nil {
		r.t.Fatal(err)
	}
	return c
}

func (r *taskRig) camp(project, id string) *Campaign {
	r.t.Helper()
	c, err := r.g.sci.campaign(project, id)
	if err != nil || c == nil {
		r.t.Fatalf("campaign %s/%s: %v %v", project, id, c, err)
	}
	return c
}

// addTask creates a task through the API (the path that reopens campaigns); want is its HTTP code.
func (r *taskRig) addTask(project, id string, labels map[string]string, want int) {
	r.t.Helper()
	r.want(want, "POST", "/v1/projects/"+project+"/tasks", projectKey(project), map[string]any{
		"id": id, "kind": "other", "title": id, "created_by": "owner", "spec": map[string]any{"leaf": id}, "labels": labels})
}

// lease claims a queued task and returns its lease id.
func (r *taskRig) claimFor(project, id string) string {
	r.t.Helper()
	t, err := r.g.tasks.claim(project, "w1", 7*24*3600, nil, id, nil) // the store takes any TTL; days outlast the test clock
	if err != nil || t == nil {
		r.t.Fatalf("claim %s: %v %v", id, t, err)
	}
	return t.Lease.ID
}

// done claims and completes a task at the rig's time.
func (r *taskRig) done(project, id string) {
	r.t.Helper()
	if _, err := r.g.tasks.complete(project, id, "w1", r.claimFor(project, id), &TaskResult{Outcome: "proved"}, ""); err != nil {
		r.t.Fatal(err)
	}
}

func (r *taskRig) wantStatus(project, id, status string, ended *time.Time) *Campaign {
	r.t.Helper()
	c := r.camp(project, id)
	if c.Status != status || !sameTime(c.EndedAt, ended) {
		r.t.Fatalf("at %s campaign %s: %s ended %v, want %s ended %v", r.now.Format(time.RFC3339), id, c.Status, c.EndedAt, status, ended)
	}
	return c
}

func at(t time.Time) *time.Time { return &t }

// A campaign finishes by itself once none of its tasks has been queued or leased for the idle time
// (6 h by default), at the last task's end; not while a proof task or a submission is queued or
// leased, not before the idle time, and not with no finished task.
func TestCampaignAutoFinish(t *testing.T) {
	r := autoRig(t, p2mAuto())
	t0 := r.now
	r.putCamp("prove2me", "t11", CampaignRunning, nil)
	r.putCamp("prove2me", "t12", CampaignRunning, nil)  // no tasks at all
	r.putCamp("prove2me", "t13", CampaignRunning, nil)  // only open tasks
	r.putCamp("prove2me", "t9", CampaignPreparing, nil) // never touched
	r.putCamp("lab", "t11", CampaignRunning, nil)       // a project without campaign_auto_finish
	r.now = t0.Add(time.Minute)
	r.addTask("prove2me", "t11-a", map[string]string{"trial": "t11", "leaf": "p2m-aaaaaa"}, 201)
	r.addTask("prove2me", "t11-b", map[string]string{"trial": "t11"}, 201)
	r.addTask("prove2me", "t11-c", map[string]string{"trial": "t11"}, 201)
	r.addTask("prove2me", "t13-a", map[string]string{"trial": "t13"}, 201)
	r.addTask("prove2me", "t9-a", map[string]string{"trial": "t9"}, 201)
	r.addTask("prove2me", "x-1", nil, 201) // no campaign
	r.addTask("lab", "t11-a", map[string]string{"trial": "t11"}, 201)
	r.now = t0.Add(time.Hour)
	r.done("prove2me", "t11-a")
	r.done("prove2me", "t9-a")
	r.done("prove2me", "x-1")
	r.done("lab", "t11-a")
	if _, err := r.g.tasks.cancel("prove2me", "t11-b", "not needed", ""); err != nil { // cancelled counts as finished
		t.Fatal(err)
	}
	r.now = t0.Add(2 * time.Hour)
	lease := r.claimFor("prove2me", "t11-c")
	if _, err := r.g.tasks.fail("prove2me", "t11-c", "w1", lease, "crashed", false); err != nil { // so does failed
		t.Fatal(err)
	}
	// The proof work is over; its submission (labels: campaign) comes later and is still queued.
	r.now = t0.Add(3 * time.Hour)
	r.addTask("prove2me", "t11-a-submit", map[string]string{"campaign": "t11", "leaf": "p2m-aaaaaa"}, 201)
	r.now = t0.Add(20 * time.Hour)
	r.g.sweepCampaigns(r.now)
	r.wantStatus("prove2me", "t11", CampaignRunning, nil) // a queued submission keeps it open
	subLease := r.claimFor("prove2me", "t11-a-submit")
	r.now = t0.Add(30 * time.Hour)
	r.g.sweepCampaigns(r.now)
	r.wantStatus("prove2me", "t11", CampaignRunning, nil) // a leased one too
	r.now = t0.Add(31 * time.Hour)
	if _, err := r.g.tasks.complete("prove2me", "t11-a-submit", "w1", subLease, &TaskResult{Outcome: "accepted"}, ""); err != nil {
		t.Fatal(err)
	}
	end := r.now
	r.now = end.Add(6*time.Hour - time.Second)
	r.g.sweepCampaigns(r.now)
	r.wantStatus("prove2me", "t11", CampaignRunning, nil) // not before the idle time
	r.now = end.Add(6 * time.Hour)
	r.g.sweepCampaigns(r.now)
	c := r.wantStatus("prove2me", "t11", CampaignFinished, &end) // ended at the last task's end
	if !c.UpdatedAt.Equal(r.now) || c.StartedAt == nil || !c.StartedAt.Equal(t0) {
		t.Fatalf("finished t11: updated %v started %v", c.UpdatedAt, c.StartedAt)
	}
	r.wantStatus("prove2me", "t12", CampaignRunning, nil)  // no finished task
	r.wantStatus("prove2me", "t13", CampaignRunning, nil)  // only an open one
	r.wantStatus("prove2me", "t9", CampaignPreparing, nil) // preparing is not running
	r.wantStatus("lab", "t11", CampaignRunning, nil)       // not configured
	r.now = r.now.Add(48 * time.Hour)                      // a later sweep changes nothing
	r.g.sweepCampaigns(r.now)
	r.wantStatus("prove2me", "t11", CampaignFinished, &end)
	if c2 := r.camp("prove2me", "t11"); !c2.UpdatedAt.Equal(c.UpdatedAt) {
		t.Fatalf("a later sweep rewrote t11: %v", c2.UpdatedAt)
	}
}

// A new task for a finished campaign reopens it (running, no ended_at, started_at kept), and the
// sweep finishes it again after that task's end; a repeated create, a task for a campaign that is not
// finished, and a project without campaign_auto_finish reopen nothing.
func TestCampaignAutoFinishReopen(t *testing.T) {
	r := autoRig(t, p2mAuto())
	t0 := r.now
	r.putCamp("prove2me", "t11", CampaignRunning, nil)
	r.putCamp("lab", "t11", CampaignRunning, nil)
	r.addTask("prove2me", "t11-a", map[string]string{"trial": "t11"}, 201)
	r.addTask("lab", "t11-a", map[string]string{"trial": "t11"}, 201)
	r.now = t0.Add(time.Hour)
	r.done("prove2me", "t11-a")
	r.done("lab", "t11-a")
	first := r.now
	r.now = first.Add(7 * time.Hour)
	r.g.sweepCampaigns(r.now)
	r.wantStatus("prove2me", "t11", CampaignFinished, &first)
	if _, _, err := r.g.sci.putCampaign(&Campaign{Project: "lab", ID: "t11"}, nil, r.now, &campaignPatch{Status: ptr(CampaignFinished)}); err != nil {
		t.Fatal(err)
	}

	// The same create again (200) is no new task.
	r.now = first.Add(8 * time.Hour)
	r.addTask("prove2me", "t11-a", map[string]string{"trial": "t11"}, 200)
	r.wantStatus("prove2me", "t11", CampaignFinished, &first)
	// Another project's campaign of the same id, without the config, stays finished.
	r.addTask("lab", "t11-r", map[string]string{"trial": "t11"}, 201)
	if c := r.camp("lab", "t11"); c.Status != CampaignFinished {
		t.Fatalf("lab's t11 reopened: %v", c.Status)
	}

	// A resubmission (labels: campaign) reopens it.
	r.addTask("prove2me", "t11-a-submit-2", map[string]string{"campaign": "t11"}, 201)
	c := r.wantStatus("prove2me", "t11", CampaignRunning, nil)
	if c.StartedAt == nil || !c.StartedAt.Equal(t0) || !c.UpdatedAt.Equal(r.now) {
		t.Fatalf("reopened: started %v updated %v", c.StartedAt, c.UpdatedAt)
	}
	// A second task for the running campaign changes nothing.
	r.now = r.now.Add(time.Minute)
	r.addTask("prove2me", "t11-z", map[string]string{"trial": "t11"}, 201)
	if c2 := r.camp("prove2me", "t11"); !c2.UpdatedAt.Equal(c.UpdatedAt) {
		t.Fatalf("a task for a running campaign rewrote it: %v", c2.UpdatedAt)
	}
	r.done("prove2me", "t11-z")
	r.now = r.now.Add(time.Hour)
	r.done("prove2me", "t11-a-submit-2")
	second := r.now
	r.now = second.Add(5 * time.Hour)
	r.g.sweepCampaigns(r.now)
	r.wantStatus("prove2me", "t11", CampaignRunning, nil)
	r.now = second.Add(6 * time.Hour)
	r.g.sweepCampaigns(r.now)
	r.wantStatus("prove2me", "t11", CampaignFinished, &second)

	// A campaign finished by hand with no started_at gets one when a task reopens it.
	r.putCamp("prove2me", "t15", CampaignFinished, nil)
	if c := r.camp("prove2me", "t15"); c.StartedAt != nil {
		t.Fatalf("t15 has started_at %v", c.StartedAt)
	}
	r.addTask("prove2me", "t15-a", map[string]string{"trial": "t15"}, 201)
	if c := r.wantStatus("prove2me", "t15", CampaignRunning, nil); c.StartedAt == nil || !c.StartedAt.Equal(r.now) {
		t.Fatalf("t15 reopened: started %v", c.StartedAt)
	}
	// A label naming no campaign, or one that does not exist, is fine.
	r.addTask("prove2me", "t16-a", map[string]string{"trial": "t16"}, 201)
}

func ptr[T any](v T) *T { return &v }

// The campaign itself must have been unchanged for the idle time: one put back to running by hand
// (or by a new plan) is not finished under it; its ended_at is never before its started_at; and a
// campaign that changed after the sweep read it is left alone.
func TestCampaignAutoFinishGuards(t *testing.T) {
	r := autoRig(t, &CampaignAutoFinish{Labels: []string{"trial"}, IdleHours: 2})
	t0 := r.now
	r.addTask("prove2me", "t11-a", map[string]string{"trial": "t11"}, 201)
	r.addTask("prove2me", "t12-a", map[string]string{"trial": "t12"}, 201)
	r.addTask("prove2me", "t13-a", map[string]string{"campaign": "t13"}, 201) // not a configured label
	r.now = t0.Add(time.Hour)
	r.done("prove2me", "t11-a")
	r.done("prove2me", "t12-a")
	r.done("prove2me", "t13-a")
	r.now = t0.Add(10 * time.Hour)
	r.putCamp("prove2me", "t11", CampaignRunning, at(t0)) // put back to running now, its tasks long done
	r.putCamp("prove2me", "t12", CampaignRunning, at(t0.Add(3*time.Hour)))
	r.putCamp("prove2me", "t13", CampaignRunning, nil)
	r.now = t0.Add(12*time.Hour - time.Second)
	r.g.sweepCampaigns(r.now)
	r.wantStatus("prove2me", "t11", CampaignRunning, nil)
	r.now = t0.Add(12 * time.Hour) // idle_hours 2 after the campaign's own change
	r.g.sweepCampaigns(r.now)
	r.wantStatus("prove2me", "t11", CampaignFinished, at(t0.Add(time.Hour)))
	r.wantStatus("prove2me", "t12", CampaignFinished, at(t0.Add(3*time.Hour))) // its started_at, not before
	r.wantStatus("prove2me", "t13", CampaignRunning, nil)                      // its task has no "trial" label

	// A write the sweep did not see wins.
	c := r.putCamp("prove2me", "t14", CampaignRunning, nil)
	if ok, err := r.g.sci.finishCampaign("prove2me", "t14", r.now, c.UpdatedAt.Add(-time.Second), r.now); err != nil || ok {
		t.Fatalf("finish with a stale updated_at: %v %v", ok, err)
	}
	if ok, err := r.g.sci.finishCampaign("prove2me", "t14", r.now, c.UpdatedAt, r.now); err != nil || !ok {
		t.Fatalf("finish: %v %v", ok, err)
	}
	if ok, err := r.g.sci.finishCampaign("prove2me", "t14", r.now, c.UpdatedAt, r.now); err != nil || ok {
		t.Fatalf("finish twice: %v %v", ok, err)
	}
}

// After a restart the sweep reads the queue from disk.
func TestCampaignAutoFinishSweepsAfterRestart(t *testing.T) {
	dir := t.TempDir()
	projects := []Project{{ID: "prove2me", KeySHA256: []string{keyHash(testProjectKey)}, CampaignAutoFinish: p2mAuto()}}
	g := newTestGateway(t, Config{StateDir: dir, Projects: projects})
	now := time.Date(2026, 11, 24, 18, 0, 0, 0, time.UTC)
	g.tasks.now = func() time.Time { return now }
	if _, _, err := g.sci.putCampaign(&Campaign{Project: "prove2me", ID: "t11", Status: CampaignRunning}, nil, now, nil); err != nil {
		t.Fatal(err)
	}
	if _, _, err := g.tasks.create("prove2me", &taskNew{ID: "t11-a", Kind: kindOther, CreatedBy: "owner", Spec: json.RawMessage(`{}`),
		Labels: map[string]string{"trial": "t11"}}); err != nil {
		t.Fatal(err)
	}
	tk, _ := g.tasks.claim("prove2me", "w1", 600, nil, "", nil)
	if _, err := g.tasks.complete("prove2me", "t11-a", "w1", tk.Lease.ID, &TaskResult{Outcome: "proved"}, ""); err != nil {
		t.Fatal(err)
	}
	g.Close()
	// A new gateway reads the queue from disk; the sweep finds the task there.
	g2 := newTestGateway(t, Config{StateDir: dir, Projects: projects})
	defer g2.Close()
	g2.sweepCampaigns(now.Add(7 * time.Hour))
	c, err := g2.sci.campaign("prove2me", "t11")
	if err != nil || c.Status != CampaignFinished || !c.EndedAt.Equal(now) {
		t.Fatalf("after a restart: %+v %v", c, err)
	}
}

// The gateway's background loop runs the sweep (real clock, an idle time of about a second).
func TestCampaignAutoFinishLoop(t *testing.T) {
	defer func(d time.Duration) { campaignSweepEvery = d }(campaignSweepEvery)
	campaignSweepEvery = 20 * time.Millisecond
	g := newTestGateway(t, Config{StateDir: t.TempDir(), Projects: []Project{{ID: "prove2me", KeySHA256: []string{keyHash(testProjectKey)},
		CampaignAutoFinish: &CampaignAutoFinish{Labels: []string{"trial"}, IdleHours: 1.0 / 3600}}}})
	defer g.Close()
	if _, _, err := g.sci.putCampaign(&Campaign{Project: "prove2me", ID: "t11", Status: CampaignRunning}, nil, time.Now().Add(-time.Hour), nil); err != nil {
		t.Fatal(err)
	}
	if _, _, err := g.tasks.create("prove2me", &taskNew{ID: "t11-a", Kind: kindOther, CreatedBy: "owner", Spec: json.RawMessage(`{}`),
		Labels: map[string]string{"trial": "t11"}}); err != nil {
		t.Fatal(err)
	}
	tk, _ := g.tasks.claim("prove2me", "w1", 600, nil, "", nil)
	if _, err := g.tasks.complete("prove2me", "t11-a", "w1", tk.Lease.ID, &TaskResult{Outcome: "proved"}, ""); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		c, err := g.sci.campaign("prove2me", "t11")
		if err != nil {
			t.Fatal(err)
		}
		if c.Status == CampaignFinished {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("not finished by the loop: %+v", c)
		}
	}
}

// campaign_auto_finish is checked with the config.
func TestCampaignAutoFinishConfig(t *testing.T) {
	base := func(af *CampaignAutoFinish) []Project {
		return []Project{{ID: "p", KeySHA256: []string{keyHash(testProjectKey)}, CampaignAutoFinish: af}}
	}
	for _, af := range []*CampaignAutoFinish{
		{},
		{Labels: []string{}},
		{Labels: []string{"Trial"}},
		{Labels: []string{"trial", "trial"}},
		{Labels: []string{"a", "b", "c", "d", "e"}},
		{Labels: []string{"trial"}, IdleHours: -1},
		{Labels: []string{"trial"}, IdleHours: math.NaN()},
		{Labels: []string{"trial"}, IdleHours: 721},
	} {
		if _, err := newProjectTable(base(af)); err == nil || !strings.Contains(err.Error(), "campaign_auto_finish") {
			t.Errorf("%+v: %v", af, err)
		}
	}
	tab, err := newProjectTable(base(&CampaignAutoFinish{Labels: []string{"trial", "campaign"}}))
	if err != nil {
		t.Fatal(err)
	}
	if got := tab.byID["p"].CampaignAutoFinish.idle(); got != 6*time.Hour {
		t.Fatalf("default idle %v", got)
	}
	in := &CampaignAutoFinish{Labels: []string{"a", "b", "c", "d"}, IdleHours: 0.5}
	tab, err = newProjectTable(base(in))
	if err != nil {
		t.Fatal(err)
	}
	af := tab.byID["p"].CampaignAutoFinish
	if af.idle() != 30*time.Minute {
		t.Fatalf("idle %v", af.idle())
	}
	in.Labels[0] = "x" // the table keeps its own copy
	if af == in || af.Labels[0] != "a" {
		t.Fatalf("the table shares the config's campaign_auto_finish")
	}
	if _, err := newProjectTable(base(&CampaignAutoFinish{Labels: []string{"trial"}, IdleHours: 720})); err != nil {
		t.Fatal(err)
	}
	if tab, err := newProjectTable(base(nil)); err != nil || tab.byID["p"].CampaignAutoFinish != nil {
		t.Fatalf("off by default: %v", err)
	}
}

func TestTaskCampaigns(t *testing.T) {
	names := []string{"trial", "campaign"}
	for _, c := range []struct {
		labels map[string]string
		want   string
	}{
		{nil, ""},
		{map[string]string{"leaf": "t1"}, ""},
		{map[string]string{"trial": "t1"}, "t1"},
		{map[string]string{"campaign": "t1"}, "t1"},
		{map[string]string{"trial": "t1", "campaign": "t1"}, "t1"},
		{map[string]string{"trial": "t1", "campaign": "t2"}, "t1,t2"},
		{map[string]string{"trial": "not a campaign"}, ""},
	} {
		if got := strings.Join(taskCampaigns(c.labels, names), ","); got != c.want {
			t.Errorf("%v: %q, want %q", c.labels, got, c.want)
		}
	}
}
