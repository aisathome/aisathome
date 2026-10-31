// The task queue's HTTP endpoints (tasks.go).
//
// SPDX-License-Identifier: Apache-2.0
package gateway

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"slices"
	"strings"
	"time"
)

// decodeTaskBody reads a JSON body of at most limit bytes into v: unknown fields, trailing data
// and other content types are refused. It writes the error and returns false on failure.
func decodeTaskBody(w http.ResponseWriter, r *http.Request, limit int64, v any) bool {
	if mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || mt != "application/json" {
		apiError(w, http.StatusUnsupportedMediaType, "send application/json")
		return false
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, limit))
	if err != nil {
		apiError(w, http.StatusRequestEntityTooLarge, fmt.Sprintf("at most %d bytes", limit))
		return false
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		apiError(w, http.StatusBadRequest, "not a valid request: "+err.Error())
		return false
	}
	if dec.More() {
		apiError(w, http.StatusBadRequest, "not a valid request: data after the object")
		return false
	}
	return true
}

func taskError(w http.ResponseWriter, err error) {
	var te *taskErr
	if errors.As(err, &te) {
		if te.code == http.StatusServiceUnavailable {
			w.Header().Set("Retry-After", "30")
		}
		apiError(w, te.code, te.msg)
		return
	}
	apiError(w, http.StatusInternalServerError, "task store error")
}

// tasksReader allows the project's own key or an admin's session (the operations view).
func (g *Gateway) tasksReader(w http.ResponseWriter, r *http.Request, id string) bool {
	if bearer(r) == "" && g.accounts != nil {
		if acct, ok := g.session(r); ok && slices.Contains(g.acfg.Admins, acct) && g.accounts.active(acct) {
			if t := g.projects.Load(); t == nil || t.byID[id] == nil {
				apiError(w, http.StatusNotFound, "no such project")
				return false
			}
			return true
		}
	}
	return g.projectFor(w, r, id)
}

func (g *Gateway) handleTaskCreate(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !g.projectFor(w, r, id) {
		return
	}
	var n taskNew
	if !decodeTaskBody(w, r, maxTaskCreateBody, &n) {
		return
	}
	if err := n.validate(); err != nil {
		apiError(w, http.StatusBadRequest, err.Error())
		return
	}
	t, created, err := g.tasks.create(id, &n)
	if err != nil {
		taskError(w, err)
		return
	}
	code := http.StatusOK
	if created {
		code = http.StatusCreated
	}
	writeJSON(w, code, map[string]any{"created": created, "task": t})
}

func splitList(v string) []string {
	if v == "" {
		return nil
	}
	return strings.Split(v, ",")
}

func (g *Gateway) handleTaskList(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !g.tasksReader(w, r, id) {
		return
	}
	if err := g.tasks.ready(id); err != nil {
		taskError(w, err)
		return
	}
	states, kinds := splitList(r.URL.Query().Get("state")), splitList(r.URL.Query().Get("kind"))
	exported := r.URL.Query().Get("exported")
	if exported != "" && exported != "true" && exported != "false" {
		apiError(w, http.StatusBadRequest, "exported: true or false")
		return
	}
	for _, s := range states {
		if !slices.Contains(queueStates, s) {
			apiError(w, http.StatusBadRequest, "state: "+strings.Join(queueStates, ", "))
			return
		}
	}
	for _, k := range kinds {
		if _, ok := taskKinds[k]; !ok {
			apiError(w, http.StatusBadRequest, "kind: "+kindsHelp)
			return
		}
	}
	ts, counts := g.tasks.list(id, states, kinds, exported)
	w.Header().Set("Cache-Control", "no-store")
	var chains []string
	for _, t := range ts {
		chains = append(chains, t.Chain)
	}
	writeJSON(w, http.StatusOK, map[string]any{"project": id, "counts": counts, "tasks": ts,
		"chains": g.tasks.chainsOf(id, chains)})
}

func (g *Gateway) handleTaskGet(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !g.tasksReader(w, r, id) {
		return
	}
	if err := g.tasks.ready(id); err != nil {
		taskError(w, err)
		return
	}
	t := g.tasks.get(id, r.PathValue("task"))
	if t == nil {
		apiError(w, http.StatusNotFound, "no such task")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]any{"task": t, "chain": g.tasks.chainView(id, t.Chain)})
}

func (g *Gateway) handleTaskClaim(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !g.projectFor(w, r, id) {
		return
	}
	var c taskControl
	if !decodeTaskBody(w, r, maxTaskControlBody, &c) {
		return
	}
	if err := checkID("worker", c.Worker); err != nil {
		apiError(w, http.StatusBadRequest, err.Error())
		return
	}
	if c.LeaseSec == 0 {
		c.LeaseSec = defaultLeaseSec
	}
	if c.LeaseSec < minLeaseSec || c.LeaseSec > maxLeaseSec {
		apiError(w, http.StatusBadRequest, fmt.Sprintf("lease_seconds: %d to %d", minLeaseSec, maxLeaseSec))
		return
	}
	for _, k := range c.Kinds {
		if _, ok := taskKinds[k]; !ok {
			apiError(w, http.StatusBadRequest, "kinds: "+kindsHelp)
			return
		}
	}
	if c.Lease != "" || c.Result != nil || c.Reason != "" || c.Requeue {
		apiError(w, http.StatusBadRequest, "claim takes worker, lease_seconds, kinds, max_class and task only")
		return
	}
	if c.MaxClass < 0 || c.MaxClass > classOther {
		apiError(w, http.StatusBadRequest, fmt.Sprintf("max_class: 1 to %d", classOther))
		return
	}
	if c.Task != "" {
		if err := checkID("task", c.Task); err != nil || len(c.Kinds) > 0 || c.MaxClass != 0 {
			apiError(w, http.StatusBadRequest, "task: one task id, without kinds or max_class")
			return
		}
	}
	t, err := g.tasks.claim(id, c.Worker, c.LeaseSec, c.Kinds, c.MaxClass, c.Task)
	if err != nil {
		taskError(w, err)
		return
	}
	if t == nil {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"task": t, "lease": map[string]any{
		"id": t.Lease.ID, "expires_at": t.Lease.ExpiresAt, "ttl_seconds": t.Lease.TTLSec}})
}

// leaseRequest decodes and checks a heartbeat, complete or fail body.
func (g *Gateway) leaseRequest(w http.ResponseWriter, r *http.Request, limit int64) (string, *taskControl, bool) {
	id := r.PathValue("id")
	if !g.projectFor(w, r, id) {
		return "", nil, false
	}
	var c taskControl
	if !decodeTaskBody(w, r, limit, &c) {
		return "", nil, false
	}
	err := checkID("worker", c.Worker)
	if err == nil && (len(c.Lease) != 32 || strings.Trim(c.Lease, "0123456789abcdef") != "") {
		err = errors.New("lease: the id the claim returned")
	}
	if err == nil && (c.LeaseSec != 0 || len(c.Kinds) > 0 || c.MaxClass != 0 || c.Task != "") {
		err = errors.New("lease_seconds, kinds, max_class and task belong to a claim")
	}
	if err != nil {
		apiError(w, http.StatusBadRequest, err.Error())
		return "", nil, false
	}
	return id, &c, true
}

func (g *Gateway) handleTaskHeartbeat(w http.ResponseWriter, r *http.Request) {
	id, c, ok := g.leaseRequest(w, r, maxTaskControlBody)
	if !ok {
		return
	}
	if c.Result != nil || c.Reason != "" || c.Requeue {
		apiError(w, http.StatusBadRequest, "heartbeat takes worker and lease only")
		return
	}
	t, err := g.tasks.heartbeat(id, r.PathValue("task"), c.Worker, c.Lease)
	if err != nil {
		taskError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"state": t.State, "expires_at": t.Lease.ExpiresAt})
}

func (g *Gateway) handleTaskComplete(w http.ResponseWriter, r *http.Request) {
	id, c, ok := g.leaseRequest(w, r, maxTaskCompleteBody)
	if !ok {
		return
	}
	if c.Result == nil || c.Reason != "" || c.Requeue {
		apiError(w, http.StatusBadRequest, "complete takes worker, lease and result")
		return
	}
	if err := c.Result.validate(); err != nil {
		apiError(w, http.StatusBadRequest, err.Error())
		return
	}
	t, err := g.tasks.complete(id, r.PathValue("task"), c.Worker, c.Lease, c.Result)
	if err != nil {
		taskError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"state": t.State})
}

func (g *Gateway) handleTaskFail(w http.ResponseWriter, r *http.Request) {
	id, c, ok := g.leaseRequest(w, r, maxTaskCompleteBody) // a skipped task may bring a recipe
	if !ok {
		return
	}
	if c.Result != nil {
		apiError(w, http.StatusBadRequest, "fail takes worker, lease, reason and requeue")
		return
	}
	if err := checkText("reason", c.Reason, 500); err != nil {
		apiError(w, http.StatusBadRequest, err.Error())
		return
	}
	t, err := g.tasks.fail(id, r.PathValue("task"), c.Worker, c.Lease, c.Reason, c.Requeue, c.Fix)
	if err != nil {
		taskError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"state": t.State, "tries": t.Tries, "max_tries": t.MaxTries, "interruptions": t.Interruptions})
}

// handleTaskMaxTries changes an open task's max_tries (project key only).
func (g *Gateway) handleTaskMaxTries(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !g.projectFor(w, r, id) {
		return
	}
	var body struct {
		MaxTries int `json:"max_tries"`
	}
	if !decodeTaskBody(w, r, maxTaskControlBody, &body) {
		return
	}
	if body.MaxTries < 1 || body.MaxTries > maxMaxTries {
		apiError(w, http.StatusBadRequest, fmt.Sprintf("max_tries: 1 to %d", maxMaxTries))
		return
	}
	t, err := g.tasks.setMaxTries(id, r.PathValue("task"), body.MaxTries)
	if err != nil {
		taskError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"state": t.State, "tries": t.Tries, "max_tries": t.MaxTries, "interruptions": t.Interruptions})
}

func (g *Gateway) handleTaskCancel(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !g.projectFor(w, r, id) {
		return
	}
	var c taskControl
	if !decodeTaskBody(w, r, maxTaskControlBody, &c) {
		return
	}
	if c.Worker != "" || c.Lease != "" || c.LeaseSec != 0 || len(c.Kinds) > 0 || c.MaxClass != 0 || c.Result != nil || c.Requeue || c.Task != "" {
		apiError(w, http.StatusBadRequest, "cancel takes a reason only")
		return
	}
	if err := checkText("reason", c.Reason, 500); err != nil {
		apiError(w, http.StatusBadRequest, err.Error())
		return
	}
	t, err := g.tasks.cancel(id, r.PathValue("task"), c.Reason)
	if err != nil {
		taskError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"state": t.State})
}

// handleTaskContinue schedules the round waiting on a task, as the operator's (project key only).
func (g *Gateway) handleTaskContinue(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !g.projectFor(w, r, id) {
		return
	}
	var body struct {
		CreatedBy string `json:"created_by"`
		Note      string `json:"note"` // a fact for the round, in its fix round's ledger entry
	}
	if !decodeTaskBody(w, r, maxTaskControlBody, &body) {
		return
	}
	err := errors.Join(checkID("created_by", body.CreatedBy), checkText("note", body.Note, 500))
	if body.CreatedBy == gatewayCreator {
		err = errors.New("created_by: gateway is the review loop's own")
	}
	if err != nil {
		apiError(w, http.StatusBadRequest, err.Error())
		return
	}
	t, created, err := g.tasks.resume(id, r.PathValue("task"), body.CreatedBy, body.Note)
	if err != nil {
		taskError(w, err)
		return
	}
	code := http.StatusOK
	if created {
		code = http.StatusCreated
	}
	writeJSON(w, code, map[string]any{"created": created, "task": t})
}

// handleTaskReview stores the internal review's verdict on a done task (project key only).
func (g *Gateway) handleTaskReview(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !g.projectFor(w, r, id) {
		return
	}
	var v TaskReview
	if !decodeTaskBody(w, r, maxTaskCreateBody, &v) {
		return
	}
	if err := v.validate(); err != nil {
		apiError(w, http.StatusBadRequest, err.Error())
		return
	}
	t, err := g.tasks.review(id, r.PathValue("task"), &v)
	if err != nil {
		taskError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"state": t.State, "review": t.Review})
}

// handleTaskDecide stores the owner's decision on a done task's candidate (project key only).
func (g *Gateway) handleTaskDecide(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !g.projectFor(w, r, id) {
		return
	}
	var d TaskDecision
	if !decodeTaskBody(w, r, maxTaskControlBody, &d) {
		return
	}
	if err := d.validate(); err != nil {
		apiError(w, http.StatusBadRequest, err.Error())
		return
	}
	t, err := g.tasks.decide(id, r.PathValue("task"), &d)
	if err != nil {
		taskError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"state": t.State, "decision": t.Decision})
}

// handleTaskExported marks a finished task as exported to the ledger (project key only).
func (g *Gateway) handleTaskExported(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !g.projectFor(w, r, id) {
		return
	}
	var body struct {
		Seen time.Time `json:"seen"` // the task's updated_at as the export read it
	}
	if !decodeTaskBody(w, r, maxTaskControlBody, &body) {
		return
	}
	if body.Seen.IsZero() {
		apiError(w, http.StatusBadRequest, "seen: the task's updated_at as the export read it")
		return
	}
	t, err := g.tasks.exported(id, r.PathValue("task"), body.Seen)
	if err != nil {
		taskError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"state": t.State, "exported_at": t.ExportedAt})
}

func (g *Gateway) taskRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /v1/projects/{id}/tasks", g.handleTaskCreate)
	mux.HandleFunc("GET /v1/projects/{id}/tasks", g.handleTaskList)
	mux.HandleFunc("POST /v1/projects/{id}/tasks/claim", g.handleTaskClaim)
	mux.HandleFunc("GET /v1/projects/{id}/tasks/{task}", g.handleTaskGet)
	mux.HandleFunc("POST /v1/projects/{id}/tasks/{task}/heartbeat", g.handleTaskHeartbeat)
	mux.HandleFunc("POST /v1/projects/{id}/tasks/{task}/complete", g.handleTaskComplete)
	mux.HandleFunc("POST /v1/projects/{id}/tasks/{task}/fail", g.handleTaskFail)
	mux.HandleFunc("POST /v1/projects/{id}/tasks/{task}/cancel", g.handleTaskCancel)
	mux.HandleFunc("POST /v1/projects/{id}/tasks/{task}/max_tries", g.handleTaskMaxTries)
	mux.HandleFunc("POST /v1/projects/{id}/tasks/{task}/review", g.handleTaskReview)
	mux.HandleFunc("POST /v1/projects/{id}/tasks/{task}/decide", g.handleTaskDecide)
	mux.HandleFunc("POST /v1/projects/{id}/tasks/{task}/exported", g.handleTaskExported)
	mux.HandleFunc("POST /v1/projects/{id}/tasks/{task}/continue", g.handleTaskContinue)
}
