// The operations page /account/ops (theme A2), for admin accounts only (accounts.admins): every
// task and attempt of a science project's runner from its latest status (projstatus.go), with
// the gateway's own view (nodes online, their versions, which node served an attempt's last
// step) and the health warnings (health.go) at the top. Admin-only because it shows operator
// data: node ids, runner details, costs and internal task names. No script: the page reloads
// itself every minute (meta refresh) and follows the site's theme.
//
// SPDX-License-Identifier: Apache-2.0
package gateway

import (
	"fmt"
	"html/template"
	"math"
	"net/http"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"
)

// recentUse remembers the last science requests (time, node, token counts), so the page can
// name the node that served an attempt's last step: the runner knows the step's token counts,
// the gateway knows the node (as research/agent-runner/steptime.py matches them).
type recentUse struct {
	mu   sync.Mutex
	ring [512]recentRec
	next int
}

type recentRec struct {
	t                  time.Time
	project, node      string
	prompt, completion int
}

func (u *recentUse) add(r usageRecord) {
	if r.Project == "" {
		return
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	u.ring[u.next%len(u.ring)] = recentRec{r.Time, r.Project, r.Node, r.Usage.PromptTokens, r.Usage.CompletionTokens}
	u.next++
}

// node returns the node of project's request with these token counts that ended nearest to
// ended (within 3 minutes), or "".
func (u *recentUse) node(project string, st *statusStep) string {
	if st == nil || st.Prompt <= 0 {
		return ""
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	best, bestDT := "", 3*time.Minute
	for _, r := range u.ring {
		if r.project != project || r.prompt != st.Prompt || abs(r.completion-st.Completion) > 2 {
			continue
		}
		dt := r.t.Sub(st.Ended)
		if dt < 0 {
			dt = -dt
		}
		if dt <= bestDT {
			best, bestDT = r.node, dt
		}
	}
	return best
}

func abs(x int) int {
	if x < 0 {
		return -x
	}
	return x
}

// opsAttempt is one row of the attempts table.
type opsAttempt struct {
	statusAttempt
	Node, LastAgo, ETA, PerStep, Hit, Used string
	Warn                                   bool
}

type opsNode struct {
	ID, Version, Operator, Since, Contract, Load string
	Protocol, Science                            int
	Trusted, Paused, Held                        bool
}

type opsData struct {
	shell
	Project     string
	Received    string // when the status came, "" = never
	Generated   string
	Warnings    []healthWarning
	Running     []opsAttempt // running and interrupted
	Queue       []opsAttempt
	Finished    []opsAttempt
	Status      *runnerStatus
	CIReasons   string
	CISince     string
	Candidates  []opsCandidate
	Nodes       []opsNode
	Cost        string
	CostNote    string
	Now         string
	Forbidden   bool
	SignedOut   bool
	ProjectList []string
}

type opsCandidate struct {
	Attempt, Since, Waiting, Outcome string
}

// opsProject is the project the page shows: ?project=<id>, else the first configured one.
func (g *Gateway) opsProjects() []string {
	t := g.projects.Load()
	if t == nil {
		return nil
	}
	ids := make([]string, 0, len(t.byID))
	for id := range t.byID {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

func ago(d time.Duration) string {
	switch {
	case d < 0:
		return "just now"
	case d < time.Minute:
		return fmt.Sprintf("%d s ago", int(d.Seconds()))
	case d < 2*time.Hour:
		return fmt.Sprintf("%d min ago", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%.1f h ago", d.Hours())
	}
	return fmt.Sprintf("%d days ago", int(d.Hours()/24))
}

// eta is when a running attempt is expected to end: at its pace, by the step cap or the time
// budget, whichever comes first (in wall-clock time from the awake pace).
func eta(a statusAttempt, now time.Time) string {
	if a.State != "running" || a.Steps <= 0 || a.AwakeMin <= 0 {
		return ""
	}
	awakePer := a.AwakeMin / float64(a.Steps)
	left, why := math.Inf(1), ""
	if a.StepCap > 0 {
		left, why = float64(max(a.StepCap-a.Steps, 0))*awakePer, "steps"
	}
	if a.BudgetMin > 0 && a.UsedMin > 0 {
		// budget minutes still left, in awake minutes at the attempt's ratio so far
		b := math.Max(a.BudgetMin-a.UsedMin, 0) * a.AwakeMin / a.UsedMin
		if b < left {
			left, why = b, "time"
		}
	}
	if math.IsInf(left, 1) {
		return ""
	}
	return fmt.Sprintf("%s UTC (%s)", now.Add(time.Duration(left*float64(time.Minute))).UTC().Format("15:04"), why)
}

func (g *Gateway) opsNodes(contracts map[string]*time.Time) []opsNode {
	g.mu.Lock()
	defer g.mu.Unlock()
	var out []opsNode
	for _, n := range g.node {
		slots := 0
		for _, m := range n.models {
			slots += m.Slots
		}
		n.mu.Lock()
		busy, paused, sci := n.inflight, n.paused, 0
		for _, c := range n.projects {
			sci += c
		}
		n.mu.Unlock()
		row := opsNode{ID: n.id, Version: strings.TrimPrefix(n.agent, "aisathome-node/"), Protocol: n.proto,
			Operator: n.operator, Since: n.since.UTC().Format("01-02 15:04"), Load: fmt.Sprintf("%d of %d", busy, slots),
			Science: sci, Trusted: n.trusted, Paused: paused, Held: n.held}
		if c := contracts[n.id]; c != nil {
			row.Contract = c.UTC().Format("2006-01-02 15:04")
		}
		out = append(out, row)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func pct(x *float64) string {
	if x == nil {
		return ""
	}
	return fmt.Sprintf("%.0f%%", 100**x)
}

func (g *Gateway) opsData(project string, now time.Time) opsData {
	d := opsData{Project: project, Now: now.UTC().Format("2006-01-02 15:04"), ProjectList: g.opsProjects()}
	s := g.pstatus.get(project)
	d.Warnings = health(s, g.NodeCount(), now)
	warned := map[string]bool{}
	for _, w := range d.Warnings {
		if w.Attempt != "" {
			warned[w.Attempt] = true
		}
	}
	contracts := map[string]*time.Time{}
	if s != nil && s.Status != nil {
		st := s.Status
		d.Status = st
		d.Received = s.ReceivedAt.UTC().Format("15:04") + " UTC, " + ago(now.Sub(s.ReceivedAt))
		d.Generated = st.GeneratedAt.UTC().Format("15:04:05")
		for _, a := range st.Attempts {
			row := opsAttempt{statusAttempt: a, Warn: warned[a.Attempt], ETA: eta(a, now), Hit: pct(a.CacheHit),
				Node: g.recent.node(project, a.LastStep)}
			if a.LastActivity != nil {
				row.LastAgo = ago(now.Sub(*a.LastActivity))
			}
			if a.Steps > 0 && a.UsedMin > 0 {
				row.PerStep = fmt.Sprintf("%.2f", a.UsedMin/float64(a.Steps))
			}
			if a.UsedMin > 0 || a.BudgetMin > 0 {
				row.Used = fmt.Sprintf("%.0f", a.UsedMin)
				if a.BudgetMin > 0 {
					row.Used += fmt.Sprintf(" of %.0f", a.BudgetMin)
				}
				if a.BudgetKind == "model" {
					row.Used += " (model)"
				}
			}
			switch a.State {
			case "running", "interrupted":
				d.Running = append(d.Running, row)
			case "queued":
				d.Queue = append(d.Queue, row)
			default:
				d.Finished = append(d.Finished, row)
			}
		}
		if st.CI != nil {
			d.CIReasons = strings.Join(st.CI.Reasons, "; ")
			if st.CI.Since != nil {
				d.CISince = st.CI.Since.UTC().Format("01-02 15:04") + " UTC (" + ago(now.Sub(*st.CI.Since)) + ")"
			}
		}
		for _, c := range st.Candidates {
			d.Candidates = append(d.Candidates, opsCandidate{c.Attempt, c.Since.UTC().Format("01-02 15:04"), ago(now.Sub(c.Since)), c.Outcome})
		}
		for _, n := range st.Nodes {
			contracts[n.Node] = n.ContractEnds
		}
		if st.CostToday != nil {
			d.Cost, d.CostNote = fmt.Sprintf("$%.2f", st.CostToday.USD), st.CostToday.Note
		}
	}
	d.Nodes = g.opsNodes(contracts)
	return d
}

// handleOps serves GET /account/ops: admin accounts only (403 for everyone else).
func (g *Gateway) handleOps(w http.ResponseWriter, r *http.Request) {
	id, ok := g.session(r)
	d := opsData{}
	status := http.StatusOK
	switch {
	case !ok:
		d.SignedOut, status = true, http.StatusForbidden
	case !slices.Contains(g.acfg.Admins, id) || !g.accounts.active(id):
		d.Forbidden, status = true, http.StatusForbidden
	default:
		project := r.URL.Query().Get("project")
		ids := g.opsProjects()
		if !slices.Contains(ids, project) {
			project = ""
			if len(ids) > 0 {
				project = ids[0]
			}
		}
		d = g.opsData(project, time.Now())
	}
	d.shell = shell{Title: "Operations", ChatURL: g.acfg.ChatURL, Theme: themeOf(r), Back: "/account/ops", Head: opsHead}
	if d.SignedOut || d.Forbidden {
		d.shell.Head = ""
	}
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	h.Set("Content-Security-Policy", pageCSP+"; form-action 'self'; frame-ancestors 'none'")
	h.Set("Referrer-Policy", "no-referrer")
	w.WriteHeader(status)
	if err := opsTmpl.Execute(w, d); err != nil {
		g.log.Error("ops page", "err", err)
	}
}

// opsHead reloads the page every minute (no script) and adds its styles: on a phone each table
// row becomes a block of label/value lines (data-label).
const opsHead template.HTML = `<meta http-equiv="refresh" content="60">
<style>
.ops .wrap,main.wrap{max-width:72rem}
.ops table td,.ops table th{font-size:.9rem}
.warn-list{margin:0;padding-left:1.2rem}.warn-list li{margin:.3rem 0}
tr.warn td:first-child{box-shadow:inset 3px 0 var(--warn);padding-left:.6rem}
.kvs{display:flex;flex-wrap:wrap;gap:.25rem 1.5rem;margin:.5rem 0}
@media (max-width:40rem){
.ops table,.ops tbody,.ops tr,.ops td{display:block;width:100%}
.ops tr:first-child:has(th){display:none}
.ops tr{border-bottom:1px solid var(--line-hi);padding:.4rem 0}
.ops td{border:0;padding:.1rem 0;display:flex;gap:.75rem;justify-content:space-between;text-align:right}
.ops td::before{content:attr(data-label);color:var(--mut);text-align:left}
.ops td.num{text-align:right}
}
</style>`

var opsTmpl = newPage("ops", template.FuncMap{
	"badge": func(state string) string {
		switch state {
		case "running":
			return "busy"
		case "interrupted":
			return "paused"
		case "failed":
			return "held"
		case "done":
			return "online"
		}
		return "offline"
	},
}, `{{template "head" .}}{{template "top" .}}
{{if .SignedOut}}<h1>Operations</h1>
<div class="card err" role="alert"><p>This page is for the project's admins. <a href="/account">Sign in</a> with an admin account.</p></div>
{{else if .Forbidden}}<h1>Operations</h1>
<div class="card err" role="alert"><p>This page is for the project's admins only.</p></div>
{{else}}<div class="ops">
<h1>Operations</h1>
<p class="mut small">Project <strong>{{.Project}}</strong>{{if .Received}} · runner status received {{.Received}} (made {{.Generated}} UTC){{end}} · page at {{.Now}} UTC, reloads every minute.</p>
{{if .Warnings}}<div class="card err" role="alert" id="warnings"><h2>Warnings</h2><ul class="warn-list">{{range .Warnings}}<li data-rule="{{.Rule}}">{{.Text}}</li>{{end}}</ul></div>
{{else}}<div class="card ok" id="warnings"><p>No warnings.</p></div>{{end}}
{{with .Status}}
<div class="card"><div class="card-head"><h2>Runner</h2>{{if .Runner.Paused}}<span class="badge paused">paused</span>{{end}}</div>
<div class="kvs"><span>Queue service: <strong>{{.Runner.Service}}</strong></span><span>Free disk: <strong>{{printf "%.0f" .Runner.FreeGB}} GB</strong></span>
{{if .Runner.Instance}}<span>Instance: {{.Runner.Instance}}</span>{{end}}
{{with $.Cost}}<span>Cost today: <strong>{{.}}</strong>{{with $.CostNote}} <span class="mut small">({{.}})</span>{{end}}</span>{{end}}</div>
<div class="kvs"><span>CI back-off: <strong>{{if and .CI .CI.Pause}}on{{else}}off{{end}}</strong>{{if and .CI .CI.Pause}}{{with $.CISince}}, since {{.}}{{end}}{{end}}</span>
{{with $.CIReasons}}<span class="mut small">{{.}}</span>{{end}}</div></div>
{{end}}
<div class="card" id="attempts"><h2>Attempts running</h2>
{{if .Running}}<div class="table-wrap"><table><tr><th>Attempt</th><th>State</th><th class="num">Steps</th><th class="num">Minutes</th><th class="num">Min/step</th><th class="num">Cache hit</th><th>Node</th><th class="num">Interr.</th><th>Last activity</th><th>ETA</th></tr>
{{range .Running}}<tr{{if .Warn}} class="warn"{{end}} data-attempt="{{.Attempt}}"><td data-label="Attempt"><code>{{.Attempt}}</code>{{if .TaskType}} <span class="mut small">{{.TaskType}}{{with .Size}} {{.}}{{end}}</span>{{end}}{{if .Round}} <span class="mut small">follow-up {{.Round}}</span>{{end}}</td>
<td data-label="State"><span class="badge {{badge .State}}">{{.State}}</span></td>
<td data-label="Steps" class="num">{{.Steps}}{{if .StepCap}} of {{.StepCap}}{{end}}</td>
<td data-label="Minutes" class="num">{{.Used}}</td><td data-label="Min/step" class="num">{{.PerStep}}</td><td data-label="Cache hit" class="num">{{.Hit}}</td>
<td data-label="Node">{{if .Node}}<code>{{.Node}}</code>{{else}}<span class="mut">–</span>{{end}}</td><td data-label="Interruptions" class="num">{{.Interruptions}}</td>
<td data-label="Last activity">{{.LastAgo}}</td><td data-label="ETA">{{.ETA}}</td></tr>{{end}}</table></div>
{{else}}<p class="mut">Nothing is running.</p>{{end}}</div>
<div class="card" id="queue"><h2>Queue</h2>
{{if .Queue}}<ol>{{range .Queue}}<li><code>{{.Attempt}}</code>{{if .TaskType}} <span class="mut small">{{.TaskType}}{{with .Size}} {{.}}{{end}}</span>{{end}}{{with .AddedAt}} <span class="mut small">queued {{.UTC.Format "01-02 15:04"}} UTC</span>{{end}}</li>{{end}}</ol>
{{else}}<p class="mut">The queue is empty.</p>{{end}}</div>
<div class="card" id="candidates"><h2>Candidates waiting</h2>
{{if .Candidates}}<div class="table-wrap"><table><tr><th>Attempt</th><th>Since</th><th>Waiting</th><th>Outcome</th></tr>
{{range .Candidates}}<tr><td data-label="Attempt"><code>{{.Attempt}}</code></td><td data-label="Since">{{.Since}} UTC</td><td data-label="Waiting">{{.Waiting}}</td><td data-label="Outcome">{{.Outcome}}</td></tr>{{end}}</table></div>
{{else}}<p class="mut">No PR candidate is waiting for the owner.</p>{{end}}</div>
{{if .Finished}}<div class="card" id="finished"><h2>Finished</h2><div class="table-wrap"><table><tr><th>Attempt</th><th>State</th><th class="num">Steps</th><th class="num">Minutes</th><th>Outcome</th></tr>
{{range .Finished}}<tr data-attempt="{{.Attempt}}"><td data-label="Attempt"><code>{{.Attempt}}</code>{{if .TaskType}} <span class="mut small">{{.TaskType}}</span>{{end}}</td><td data-label="State"><span class="badge {{badge .State}}">{{.State}}</span></td>
<td data-label="Steps" class="num">{{.Steps}}{{if .StepCap}} of {{.StepCap}}{{end}}</td><td data-label="Minutes" class="num">{{.Used}}</td><td data-label="Outcome">{{.Outcome}}</td></tr>{{end}}</table></div></div>{{end}}
<div class="card" id="nodes"><h2>Nodes online ({{len .Nodes}})</h2>
{{if .Nodes}}<div class="table-wrap"><table><tr><th>Node</th><th>Version</th><th>Protocol</th><th class="num">Busy</th><th class="num">Science</th><th>Connected</th><th>Contract ends</th></tr>
{{range .Nodes}}<tr data-node="{{.ID}}"><td data-label="Node"><code>{{.ID}}</code>{{if .Trusted}} <span class="mut small">trusted</span>{{end}}{{if .Paused}} <span class="badge paused">paused</span>{{end}}{{if .Held}} <span class="badge held">held</span>{{end}}</td>
<td data-label="Version">{{if .Version}}<code>{{.Version}}</code>{{else}}<span class="mut">unknown</span>{{end}}</td><td data-label="Protocol">{{.Protocol}}</td>
<td data-label="Busy" class="num">{{.Load}}</td><td data-label="Science" class="num">{{.Science}}</td><td data-label="Connected">{{.Since}} UTC</td>
<td data-label="Contract ends">{{if .Contract}}{{.Contract}} UTC{{else}}<span class="mut">–</span>{{end}}</td></tr>{{end}}</table></div>
{{else}}<p class="mut">No node is online.</p>{{end}}</div>
{{if not .Status}}<p class="mut">No status from the runner yet: it posts one every minute (research tools/status_report.py).</p>{{end}}
{{if gt (len .ProjectList) 1}}<p class="small">Projects: {{range .ProjectList}}<a href="/account/ops?project={{.}}">{{.}}</a> {{end}}</p>{{end}}
</div>{{end}}
{{template "bottom" .}}`)
