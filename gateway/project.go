// Workload classes and science projects.
//
// Every chat request has a class: "chat" (people in the chat front end), "agent" (people's own
// API keys) or "science" (a project's key). When nodes are busy, a waiting request of a higher
// class goes first, and a request never takes a node that a waiting request of a higher class
// could use (routing.go). A chat or agent request that has waited preempt_after_seconds stops a
// science request on a node it could use (preempt.go); people's requests are never preempted.
//
// A project is an operator-run workload: it exists only in the gateway config, is called with its
// own keys (by SHA-256), has its own daily quota instead of an account's, and may use at most
// max_node_share of the nodes that may serve it at once. What the gateway knows of a project is
// that data (no project's name or workflow is in the code): its task kinds, whether its
// queue is public, and where its public page is.
//
// SPDX-License-Identifier: Apache-2.0
package gateway

import (
	"fmt"
	"math"
	"regexp"
	"slices"
	"strings"
	"time"
)

// Workload classes, in the order of priority (higher first).
const (
	ClassChat    = "chat"
	ClassAgent   = "agent"
	ClassScience = "science"
)

// classRank orders the classes: a higher rank goes first.
func classRank(c string) int {
	switch c {
	case ClassChat:
		return 2
	case ClassAgent:
		return 1
	}
	return 0
}

// classes lists the classes for statistics, highest priority first.
var classes = []string{ClassChat, ClassAgent, ClassScience}

// Project is an operator-run workload (Config.Projects).
type Project struct {
	// ID names the project in statistics and the usage log ("prove2me"): lower-case letters,
	// digits and dashes. Public: it appears in /v1/stats.
	ID string `json:"id"`
	// KeySHA256 lists the SHA-256 (hex) of the project's API keys. The keys themselves are
	// never in the config: generate one with `aisathome-gateway project-key <file>`.
	KeySHA256 []string `json:"key_sha256"`
	// DailyTokens is the project's own budget in weighted tokens per UTC day, shared by all
	// its keys; 0 = no limit. Accounts' budgets do not apply.
	DailyTokens int `json:"daily_tokens,omitempty"`
	// MaxNodeShare caps the share of the nodes that may serve a request that the project's
	// requests occupy at once (rounded down, at least one node). 0 = no cap.
	MaxNodeShare float64 `json:"max_node_share,omitempty"`
	// AgentOK lets the project's requests with tools (agents) reach volunteer nodes past their
	// probation, as an account key with consent does. Without it they go to no node (a project has
	// no nodes of its own).
	AgentOK bool `json:"agent_ok,omitempty"`
	// PilotProbationOK lets the project's agents (with AgentOK) use connector pilots whose account is
	// still in probation (science only: Lean checks every proof).
	PilotProbationOK bool `json:"pilot_probation_ok,omitempty"`
	// LegacyMinTrust is the removed "min_trust" (removed.go): accepted and ignored.
	LegacyMinTrust string `json:"min_trust,omitempty"`
	// RPM and Concurrent limit the project's requests (all keys together); 0 = unlimited.
	RPM        int `json:"rpm,omitempty"`
	Concurrent int `json:"concurrent,omitempty"`
	// QueueTimeoutSec is how long a request waits for a node; science waits behind people's
	// requests, so it may wait longer than they do. Default the gateway's queue_timeout_seconds.
	QueueTimeoutSec int `json:"queue_timeout_seconds,omitempty"`
	// PublicQueue publishes the public fields of the project's tasks (ids, kinds, roles, states,
	// outcomes, relations, labels, token counts) at GET /v1/projects/<id>/queue and GET /v1/tasks,
	// without a key (queue_public.go, tasks_public.go). Off: 404, and none in /v1/tasks.
	PublicQueue bool `json:"public_queue,omitempty"`
	// Kinds: the task kinds the project may create besides "other" (tasks.go): one per scaffold
	// generation ("p2m-v7"), each 1-32 lower-case letters, digits, '_' or '-'. Its workers claim by
	// kind. A config written before the typed task schema has none and stays valid.
	Kinds []string `json:"kinds,omitempty"`
	// Page: the project's public page on the website, a path ("/science/prove2me/"); the account
	// page links a node's science work to it. PageAnchor: a label name ("leaf") whose value places a
	// task on that page ("#leaf-<value>"). Both optional.
	Page       string `json:"page,omitempty"`
	PageAnchor string `json:"page_anchor,omitempty"`
	// TaskCompactDays: a finished task is compacted this many days after its end (tasks.go,
	// Retention): 1 to maxCompactDays; 0 = defaultCompactDays. Tasks are kept either way.
	TaskCompactDays int `json:"task_compact_days,omitempty"`
	// PublicLinks: the https prefixes ("https://example.org/items/", ending in /) the project's
	// campaign items and external results may link to (campaigns.go); none: they carry no links.
	PublicLinks []string `json:"public_links,omitempty"`
}

// projectPageRe: a project's page, a site path.
var projectPageRe = regexp.MustCompile(`^/[a-z0-9][a-z0-9/_-]{0,99}$`)

var projectID = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)

// projectTable resolves project keys. It is replaced as a whole on reload.
type projectTable struct {
	byHash map[string]*Project // key SHA-256 → project
	byID   map[string]*Project
	ids    []string // in the config's order
}

// projectKinds is a project's declared task kinds (none: only "other").
func (g *Gateway) projectKinds(project string) []string {
	if t := g.projects.Load(); t != nil && t.byID[project] != nil {
		return t.byID[project].Kinds
	}
	return nil
}

// taskCompactAge is how long after its end a project's finished task is compacted (tasks.go,
// Retention): its task_compact_days, else defaultCompactDays.
func (g *Gateway) taskCompactAge(project string) time.Duration {
	days := defaultCompactDays
	if t := g.projects.Load(); t != nil && t.byID[project] != nil && t.byID[project].TaskCompactDays > 0 {
		days = t.byID[project].TaskCompactDays
	}
	return time.Duration(days) * 24 * time.Hour
}

// newProjectTable checks the projects of a config.
func newProjectTable(ps []Project) (*projectTable, error) {
	t := &projectTable{byHash: map[string]*Project{}, byID: map[string]*Project{}}
	for i := range ps {
		p := ps[i] // a copy: the table must not change with the config it came from
		if !projectID.MatchString(p.ID) {
			return nil, fmt.Errorf("project %q: id must be 1-32 lower-case letters, digits or dashes", p.ID)
		}
		if t.byID[p.ID] != nil {
			return nil, fmt.Errorf("project %q is defined twice", p.ID)
		}
		if p.DailyTokens < 0 || p.RPM < 0 || p.Concurrent < 0 || p.QueueTimeoutSec < 0 {
			return nil, fmt.Errorf("project %q: limits must not be negative", p.ID)
		}
		if math.IsNaN(p.MaxNodeShare) || p.MaxNodeShare < 0 || p.MaxNodeShare > 1 {
			return nil, fmt.Errorf("project %q: max_node_share must be from 0 to 1", p.ID)
		}
		if p.TaskCompactDays < 0 || p.TaskCompactDays > maxCompactDays {
			return nil, fmt.Errorf("project %q: task_compact_days must be from 1 to %d (0: %d)", p.ID, maxCompactDays, defaultCompactDays)
		}
		if len(p.Kinds) > maxProjectKinds {
			return nil, fmt.Errorf("project %q: at most %d kinds", p.ID, maxProjectKinds)
		}
		for j, k := range p.Kinds {
			if !taskKindRe.MatchString(k) || k == kindOther || slices.Contains(p.Kinds[:j], k) {
				return nil, fmt.Errorf("project %q: kind %q: 1-32 lower-case letters, digits, '_' or '-', a letter first, once, not other", p.ID, k)
			}
		}
		p.Kinds = slices.Clone(p.Kinds)
		if err := checkPublicLinks(p.PublicLinks); err != nil {
			return nil, fmt.Errorf("project %q: %w", p.ID, err)
		}
		p.PublicLinks = slices.Clone(p.PublicLinks)
		if p.Page != "" && (!projectPageRe.MatchString(p.Page) || strings.Contains(p.Page, "//")) {
			return nil, fmt.Errorf("project %q: page must be a site path such as /science/%s/", p.ID, p.ID)
		}
		if p.PageAnchor != "" && (!taskLabelKeyRe.MatchString(p.PageAnchor) || p.Page == "") {
			return nil, fmt.Errorf("project %q: page_anchor must be a label name, with a page", p.ID)
		}
		if len(p.KeySHA256) == 0 {
			return nil, fmt.Errorf("project %q has no key_sha256", p.ID)
		}
		p.KeySHA256 = append([]string(nil), p.KeySHA256...)
		for j, h := range p.KeySHA256 {
			h = strings.ToLower(strings.TrimSpace(h))
			if len(h) != 64 || strings.Trim(h, "0123456789abcdef") != "" {
				return nil, fmt.Errorf("project %q: key_sha256 entries must be 64 hex characters", p.ID)
			}
			if t.byHash[h] != nil {
				return nil, fmt.Errorf("project %q: a key hash is listed twice", p.ID)
			}
			p.KeySHA256[j] = h
		}
		pp := &p
		t.byID[p.ID] = pp
		t.ids = append(t.ids, p.ID)
		for _, h := range p.KeySHA256 {
			t.byHash[h] = pp
		}
	}
	return t, nil
}

// project returns the project of an API key, or nil.
func (g *Gateway) project(key string) *Project {
	t := g.projects.Load()
	if t == nil || key == "" {
		return nil
	}
	return t.byHash[keyHash(key)]
}

// info is the KeyInfo a project's requests are held to. User names it in the usage log.
// Project keys are the operator's: their answers name the node that served them.
func (p *Project) info() KeyInfo {
	return KeyInfo{User: projectUser(p.ID), RPM: p.RPM, MaxConcurrent: p.Concurrent, TokensPerDay: p.DailyTokens,
		VolunteerAgents: p.AgentOK, ServedBy: true}
}

// projectUser is the budget key and usage-log user of a project's requests. The colon cannot
// occur in an account pseudonym or a front-end user id.
func projectUser(id string) string { return "project:" + id }

// shareCap is how many nodes a project may occupy at once out of pool nodes that may serve
// it: the share rounded down, but at least one, so a small network still does science while
// people leave it idle. 0 = no cap.
func shareCap(share float64, pool int) int {
	if share <= 0 || share >= 1 {
		return 0
	}
	return max(1, int(math.Floor(share*float64(pool)+1e-9)))
}
