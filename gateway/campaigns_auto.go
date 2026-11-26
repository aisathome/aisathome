// Campaigns that finish by themselves: for a project with campaign_auto_finish, the gateway finishes
// a running campaign once its work is over, and reopens a finished one that gets new work.
//
// Which tasks belong to a campaign is the project's own naming: the config names the task labels
// whose value is a campaign's id (prove2me: "trial" on its proof tasks, "campaign" on its submission
// tasks). A task with none of them belongs to no campaign; a task made before labels has none.
//
// The sweep (every campaignSweepEvery) finishes a running campaign when
//
//   - none of its tasks is queued or leased,
//   - at least one of its tasks has finished (done, failed or cancelled),
//   - the last of them ended at least idle_hours ago (default 6), and
//   - the campaign itself has not changed for idle_hours either (so a campaign the project has just
//     put back to running, by hand or with a new plan, is not finished under it).
//
// Its ended_at is the last task's end (a done task's result finished_at, else the time the task
// ended), never before its started_at; its updated_at is the sweep's time. A submission is an
// ordinary task, so one that is still queued or leased keeps its campaign open. A campaign the sweep
// has read and that changes before the sweep writes is left alone (the write is conditional on
// updated_at), and a campaign that is preparing is never touched.
//
// Reopening: a task created (a new one: a repeated create changes nothing) whose labels name a
// finished campaign of the project puts that campaign back to running and clears its ended_at
// (started_at stays); the sweep finishes it again once that work is over. The sweep and the reopen
// are serialised, so a task created while a sweep runs either keeps the campaign open (the sweep saw
// it) or reopens it right after (it did not). A failed reopen is logged; the task stands.
//
// Finishing by hand (PATCH {"status": "finished"}) still works and is an override: the sweep only
// ever finishes running campaigns, and only the next new task reopens one.
//
// SPDX-License-Identifier: Apache-2.0
package gateway

import (
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"
	"time"
)

// campaignSweepEvery: how often idle campaigns are looked for; the idle time is hours, so a few
// minutes late is fine. A variable for the tests.
var campaignSweepEvery = 5 * time.Minute

const (
	// defaultAutoFinishIdle: a campaign_auto_finish without idle_hours.
	defaultAutoFinishIdle = 6 * time.Hour
	// maxAutoFinishIdleHours bounds idle_hours (30 days).
	maxAutoFinishIdleHours = 720
	// maxAutoFinishLabels bounds campaign_auto_finish.labels.
	maxAutoFinishLabels = 4
)

// check validates a project's campaign_auto_finish.
func (a *CampaignAutoFinish) check() error {
	if len(a.Labels) == 0 || len(a.Labels) > maxAutoFinishLabels {
		return fmt.Errorf("labels: 1 to %d label names", maxAutoFinishLabels)
	}
	for i, l := range a.Labels {
		if !taskLabelKeyRe.MatchString(l) || slices.Contains(a.Labels[:i], l) {
			return fmt.Errorf("labels: %q: a label name (lower-case letters, digits and _, a letter first), once", l)
		}
	}
	if math.IsNaN(a.IdleHours) || a.IdleHours < 0 || a.IdleHours > maxAutoFinishIdleHours {
		return fmt.Errorf("idle_hours: more than 0, at most %d (0: %g)", maxAutoFinishIdleHours, defaultAutoFinishIdle.Hours())
	}
	return nil
}

// idle is how long a campaign's tasks must have been idle before it is finished.
func (a *CampaignAutoFinish) idle() time.Duration {
	if a.IdleHours == 0 {
		return defaultAutoFinishIdle
	}
	return time.Duration(a.IdleHours * float64(time.Hour))
}

// autoFinish is a project's campaign_auto_finish, or nil.
func (g *Gateway) autoFinish(project string) *CampaignAutoFinish {
	if t := g.projects.Load(); t != nil && t.byID[project] != nil {
		return t.byID[project].CampaignAutoFinish
	}
	return nil
}

// taskCampaigns: the campaign ids a task's labels name under the given label names, each once; a
// value that is no campaign id names none.
func taskCampaigns(labels map[string]string, names []string) []string {
	var out []string
	for _, n := range names {
		if v := labels[n]; v != "" && recordIDRe.MatchString(v) && !slices.Contains(out, v) {
			out = append(out, v)
		}
	}
	return out
}

// taskEnd is when a finished task ended: its result's finished_at, else its last change (as the
// public views show it, tasks_public.go).
func taskEnd(t *Task) time.Time {
	if t.Result != nil && !t.Result.FinishedAt.IsZero() {
		return t.Result.FinishedAt
	}
	return t.UpdatedAt
}

// campaignWork is what a campaign's tasks are doing.
type campaignWork struct {
	open, finished int
	lastEnd        time.Time // the latest end of its finished tasks
}

// campaignWork returns, per campaign named by the given labels, its tasks' work in a project's queue.
func (s *taskStore) campaignWork(project string, labels []string) (map[string]*campaignWork, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, _, err := s.open(project)
	if err != nil {
		return nil, err
	}
	out := map[string]*campaignWork{}
	for _, t := range p.byID {
		for _, id := range taskCampaigns(t.Labels, labels) {
			w := out[id]
			if w == nil {
				w = &campaignWork{}
				out[id] = w
			}
			if !finished(t.State) {
				w.open++
				continue
			}
			w.finished++
			if end := taskEnd(t); end.After(w.lastEnd) {
				w.lastEnd = end
			}
		}
	}
	return out, nil
}

// runningCampaigns returns a project's running campaigns (without items and counts).
func (s *scienceDB) runningCampaigns(project string) ([]*Campaign, error) {
	ctx, cancel := scienceCtx()
	defer cancel()
	rows, err := s.db.QueryContext(ctx, `SELECT `+campaignCols+` FROM campaigns WHERE project = ? AND status = ?`, project, CampaignRunning)
	if err != nil {
		return nil, s.fail("running campaigns", project, err)
	}
	var out []*Campaign
	for rows.Next() {
		c, err := scanCampaign(project, rows)
		if err != nil {
			rows.Close()
			return nil, s.fail("running campaigns: scan", project, err)
		}
		out = append(out, c)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, s.fail("running campaigns: rows", project, err)
	}
	return out, nil
}

// finishCampaign finishes a running campaign that has not changed since updated, ended at end; it
// reports whether it did (false: it changed or is not running any more).
func (s *scienceDB) finishCampaign(project, id string, end, updated, now time.Time) (bool, error) {
	ctx, cancel := scienceCtx()
	defer cancel()
	res, err := s.db.ExecContext(ctx, `UPDATE campaigns SET status = ?, ended_at = ?, updated_at = ?
		WHERE project = ? AND id = ? AND status = ? AND updated_at = ?`,
		CampaignFinished, fmtTime(end), fmtTime(now), project, id, CampaignRunning, fmtTime(updated))
	if err != nil {
		return false, s.fail("finish campaign", project, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, s.fail("finish campaign: rows", project, err)
	}
	return n > 0, nil
}

// reopenCampaigns puts those of the given campaigns that are finished back to running, without an
// ended_at (started_at stays, or is now when it had none), and returns their ids.
func (s *scienceDB) reopenCampaigns(project string, ids []string, now time.Time) ([]string, error) {
	ctx, cancel := scienceCtx()
	defer cancel()
	var out []string
	for _, id := range ids {
		res, err := s.db.ExecContext(ctx, `UPDATE campaigns SET status = ?, ended_at = NULL, started_at = COALESCE(started_at, ?),
			updated_at = ? WHERE project = ? AND id = ? AND status = ?`,
			CampaignRunning, fmtTime(now), fmtTime(now), project, id, CampaignFinished)
		if err != nil {
			return out, s.fail("reopen campaign", project, err)
		}
		if n, err := res.RowsAffected(); err != nil {
			return out, s.fail("reopen campaign: rows", project, err)
		} else if n > 0 {
			out = append(out, id)
		}
	}
	return out, nil
}

// sweepCampaigns finishes the idle running campaigns of every project with campaign_auto_finish
// (see the top of the file), at now.
func (g *Gateway) sweepCampaigns(now time.Time) {
	t := g.projects.Load()
	if t == nil {
		return
	}
	g.campMu.Lock()
	defer g.campMu.Unlock()
	for _, id := range t.ids {
		if af := t.byID[id].CampaignAutoFinish; af != nil {
			g.finishIdleCampaigns(id, af, now)
		}
	}
}

// finishIdleCampaigns finishes a project's idle running campaigns. Holds g.campMu.
func (g *Gateway) finishIdleCampaigns(project string, af *CampaignAutoFinish, now time.Time) {
	cs, err := g.sci.runningCampaigns(project)
	if err != nil || len(cs) == 0 {
		return // a failure is logged by the science database
	}
	work, err := g.tasks.campaignWork(project, af.Labels)
	if err != nil {
		g.log.Warn("campaign sweep: the task queue cannot be read; campaigns left as they are", "project", project, "err", err)
		return
	}
	idle := af.idle()
	for _, c := range cs {
		w := work[c.ID]
		if w == nil || w.open > 0 || w.finished == 0 || now.Sub(w.lastEnd) < idle || now.Sub(c.UpdatedAt) < idle {
			continue
		}
		end := w.lastEnd.UTC().Truncate(time.Second)
		if c.StartedAt != nil && end.Before(*c.StartedAt) {
			end = *c.StartedAt
		}
		done, err := g.sci.finishCampaign(project, c.ID, end, c.UpdatedAt, now)
		if err != nil {
			return
		}
		if done {
			g.log.Info("campaign finished by itself", "project", project, "campaign", c.ID, "ended_at", fmtTime(end),
				"finished_tasks", w.finished, "idle", idle.String())
		}
	}
}

// campaignTaskCreated reopens the finished campaigns a project's new task belongs to, for a project
// with campaign_auto_finish. A failure is logged; the task stands.
func (g *Gateway) campaignTaskCreated(project string, labels map[string]string) {
	af := g.autoFinish(project)
	if af == nil {
		return
	}
	ids := taskCampaigns(labels, af.Labels)
	if len(ids) == 0 {
		return
	}
	g.campMu.Lock()
	defer g.campMu.Unlock()
	reopened, _ := g.sci.reopenCampaigns(project, ids, g.tasks.now())
	if len(reopened) > 0 {
		g.log.Info("campaign reopened by a new task", "project", project, "campaigns", strings.Join(reopened, ","))
	}
}
