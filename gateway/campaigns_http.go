// The HTTP endpoints of campaigns and external results (campaigns.go).
//
// SPDX-License-Identifier: Apache-2.0
package gateway

import (
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"
)

// projectLinks is a project's public_links.
func (g *Gateway) projectLinks(project string) []string {
	if t := g.projects.Load(); t != nil && t.byID[project] != nil {
		return t.byID[project].PublicLinks
	}
	return nil
}

// scienceReader allows anyone for a project with public_queue, else the project's key or an
// admin's session (tasksReader).
func (g *Gateway) scienceReader(w http.ResponseWriter, r *http.Request, id string) bool {
	t := g.projects.Load()
	if t == nil || t.byID[id] == nil {
		apiError(w, http.StatusNotFound, "no such project")
		return false
	}
	if t.byID[id].PublicQueue {
		return true
	}
	return g.tasksReader(w, r, id)
}

// readHeaders: a public project's reads may be cached for a few seconds; another's are its own.
func (g *Gateway) readHeaders(w http.ResponseWriter, project string) {
	cc := "no-store"
	if t := g.projects.Load(); t != nil && t.byID[project] != nil && t.byID[project].PublicQueue {
		cc = "public, max-age=15"
	}
	w.Header().Set("Cache-Control", cc)
	w.Header().Set("X-Content-Type-Options", "nosniff")
}

func listLimit(w http.ResponseWriter, v string, def, hi int) (int, bool) {
	if v == "" {
		return def, true
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 || n > hi {
		apiError(w, http.StatusBadRequest, fmt.Sprintf("limit: 1 to %d", hi))
		return 0, false
	}
	return n, true
}

func (g *Gateway) handleCampaignList(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !g.scienceReader(w, r, id) {
		return
	}
	q := r.URL.Query()
	limit, ok := listLimit(w, q.Get("limit"), campaignsListDefault, campaignsListMax)
	if !ok {
		return
	}
	after := q.Get("after")
	if _, _, ok := splitCursor(after, recordIDRe); after != "" && !ok {
		apiError(w, http.StatusBadRequest, "after: the next of the page before")
		return
	}
	cs, next, err := g.sci.campaigns(id, limit, after)
	if err != nil {
		taskError(w, err)
		return
	}
	g.readHeaders(w, id)
	writeJSON(w, http.StatusOK, map[string]any{"project": id, "campaigns": cs, "next": next})
}

func (g *Gateway) handleCampaignGet(w http.ResponseWriter, r *http.Request) {
	id, cid := r.PathValue("id"), r.PathValue("campaign")
	if !g.scienceReader(w, r, id) {
		return
	}
	if !recordIDRe.MatchString(cid) {
		apiError(w, http.StatusNotFound, "no such campaign")
		return
	}
	c, err := g.sci.campaign(id, cid)
	if err != nil {
		taskError(w, err)
		return
	}
	if c == nil {
		apiError(w, http.StatusNotFound, "no such campaign")
		return
	}
	g.readHeaders(w, id)
	writeJSON(w, http.StatusOK, map[string]any{"campaign": c})
}

// handleCampaignPut creates or replaces a campaign (project key): 201 created, 200 otherwise.
func (g *Gateway) handleCampaignPut(w http.ResponseWriter, r *http.Request) {
	id, cid := r.PathValue("id"), r.PathValue("campaign")
	if !g.projectFor(w, r, id) {
		return
	}
	if !recordIDRe.MatchString(cid) {
		apiError(w, http.StatusBadRequest, "campaign: 1-64 letters, digits, '.', '_' or '-'")
		return
	}
	var in campaignIn
	if !decodeTaskBody(w, r, maxCampaignBody, &in) {
		return
	}
	c := &Campaign{Project: id, ID: cid, Title: in.Title, Status: in.Status, StartedAt: in.StartedAt, EndedAt: in.EndedAt,
		Labels: in.Labels, Text: in.Text, Items: in.Items}
	g.writeCampaign(w, c, nil)
}

// handleCampaignPatch changes some of a campaign's fields (project key).
func (g *Gateway) handleCampaignPatch(w http.ResponseWriter, r *http.Request) {
	id, cid := r.PathValue("id"), r.PathValue("campaign")
	if !g.projectFor(w, r, id) {
		return
	}
	if !recordIDRe.MatchString(cid) {
		apiError(w, http.StatusNotFound, "no such campaign")
		return
	}
	var p campaignPatch
	if !decodeTaskBody(w, r, maxCampaignBody, &p) {
		return
	}
	g.writeCampaign(w, &Campaign{Project: id, ID: cid}, &p)
}

func (g *Gateway) writeCampaign(w http.ResponseWriter, c *Campaign, p *campaignPatch) {
	stored, created, err := g.sci.putCampaign(c, g.projectLinks(c.Project), time.Now(), p)
	if err != nil {
		taskError(w, err)
		return
	}
	code := http.StatusOK
	if created {
		code = http.StatusCreated
	}
	writeJSON(w, code, map[string]any{"created": created, "campaign": stored})
}

func (g *Gateway) handleCampaignDelete(w http.ResponseWriter, r *http.Request) {
	id, cid := r.PathValue("id"), r.PathValue("campaign")
	if !g.projectFor(w, r, id) {
		return
	}
	if !recordIDRe.MatchString(cid) {
		apiError(w, http.StatusNotFound, "no such campaign")
		return
	}
	if err := g.sci.deleteCampaign(id, cid); err != nil {
		taskError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"deleted": cid})
}

func (g *Gateway) handleResultList(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !g.scienceReader(w, r, id) {
		return
	}
	q := r.URL.Query()
	f := resultFilter{campaign: q.Get("campaign"), item: q.Get("item"), source: q.Get("source"), states: splitList(q.Get("state"))}
	switch {
	case f.campaign != "" && !recordIDRe.MatchString(f.campaign):
		apiError(w, http.StatusBadRequest, "campaign: a campaign's id")
		return
	case f.item != "" && !recordIDRe.MatchString(f.item):
		apiError(w, http.StatusBadRequest, "item: an item's id")
		return
	case f.source != "" && checkKind("source", f.source) != nil:
		apiError(w, http.StatusBadRequest, "source: a source's name")
		return
	}
	for _, s := range f.states {
		if !slices.Contains(resultStates, s) {
			apiError(w, http.StatusBadRequest, "state: "+strings.Join(resultStates, ", "))
			return
		}
	}
	limit, ok := listLimit(w, q.Get("limit"), resultsListDefault, resultsListMax)
	if !ok {
		return
	}
	after := q.Get("after")
	if _, _, ok := splitCursor(after, resultIDRe); after != "" && !ok {
		apiError(w, http.StatusBadRequest, "after: the next of the page before")
		return
	}
	rs, counts, next, err := g.sci.results(id, f, limit, after)
	if err != nil {
		taskError(w, err)
		return
	}
	g.readHeaders(w, id)
	writeJSON(w, http.StatusOK, map[string]any{"project": id, "counts": counts, "results": rs, "next": next})
}

// handleResultPost creates or updates a result by (source, external_id) (project key): 201 created,
// 200 otherwise. The answer alone carries external_id.
func (g *Gateway) handleResultPost(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !g.projectFor(w, r, id) {
		return
	}
	var in resultIn
	if !decodeTaskBody(w, r, maxResultBody, &in) {
		return
	}
	if err := in.validate(g.projectLinks(id)); err != nil {
		apiError(w, http.StatusBadRequest, err.Error())
		return
	}
	res, created, err := g.sci.putResult(id, &in, time.Now())
	if err != nil {
		taskError(w, err)
		return
	}
	code := http.StatusOK
	if created {
		code = http.StatusCreated
	}
	writeJSON(w, code, map[string]any{"created": created, "result": res})
}

func (g *Gateway) handleResultDelete(w http.ResponseWriter, r *http.Request) {
	id, rid := r.PathValue("id"), r.PathValue("result")
	if !g.projectFor(w, r, id) {
		return
	}
	if !resultIDRe.MatchString(rid) {
		apiError(w, http.StatusNotFound, "no such result")
		return
	}
	if err := g.sci.deleteResult(id, rid); err != nil {
		taskError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"deleted": rid})
}

func (g *Gateway) campaignRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/projects/{id}/campaigns", g.handleCampaignList)
	mux.HandleFunc("GET /v1/projects/{id}/campaigns/{campaign}", g.handleCampaignGet)
	mux.HandleFunc("PUT /v1/projects/{id}/campaigns/{campaign}", g.handleCampaignPut)
	mux.HandleFunc("PATCH /v1/projects/{id}/campaigns/{campaign}", g.handleCampaignPatch)
	mux.HandleFunc("DELETE /v1/projects/{id}/campaigns/{campaign}", g.handleCampaignDelete)
	mux.HandleFunc("GET /v1/projects/{id}/results", g.handleResultList)
	mux.HandleFunc("POST /v1/projects/{id}/results", g.handleResultPost)
	mux.HandleFunc("DELETE /v1/projects/{id}/results/{result}", g.handleResultDelete)
}
