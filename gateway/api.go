// HTTP routes, request authentication, model discovery, and chat admission.
//
// SPDX-License-Identifier: Apache-2.0
package gateway

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
)

// Request bodies: at most maxBody each, bodyBudget reserved across all requests at once
// (decoded JSON takes a few times more), and read within bodyReadTimeout.
const (
	maxBody         = 8 << 20
	bodyBudget      = 64 << 20
	bodyReadTimeout = 30 * time.Second
)

// reserveBody admits a request body of size bytes for user: at most maxBody in flight per
// user (so one user cannot fill the shared budget with stalled uploads), and bodyBudget across
// everyone. status is 429 when the user is over their share, 503 when the gateway is full.
func (g *Gateway) reserveBody(user string, size int64) (release func(), status int) {
	g.bodyMu.Lock()
	defer g.bodyMu.Unlock()
	if g.bodyBy[user]+size > maxBody {
		return nil, http.StatusTooManyRequests
	}
	if g.bodies.Load()+size > bodyBudget {
		return nil, http.StatusServiceUnavailable
	}
	g.bodies.Add(size)
	g.bodyBy[user] += size
	return func() {
		g.bodyMu.Lock()
		defer g.bodyMu.Unlock()
		g.bodies.Add(-size)
		if g.bodyBy[user] -= size; g.bodyBy[user] <= 0 {
			delete(g.bodyBy, user)
		}
	}, 0
}

// Handler returns the HTTP routes.
func (g *Gateway) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/chat/completions", g.handleChat)
	mux.HandleFunc("GET /v1/models", g.handleModels)
	mux.HandleFunc("GET /v1/node/connect", g.handleNodeConnect)
	mux.HandleFunc("GET /v1/stats", g.handleStats)
	mux.HandleFunc("GET /v1/stats/credit", g.handleCreditStats)
	mux.HandleFunc("GET /v1/stats/history", g.handleHistory)
	mux.HandleFunc("POST /v1/projects/{id}/status", g.handleStatusPost)
	mux.HandleFunc("GET /v1/projects/{id}/status", g.handleStatusGet)
	mux.HandleFunc("POST /v1/projects/{id}/science", g.handleSciencePost)
	mux.HandleFunc("GET /v1/projects/{id}/science", g.handleScienceGet)
	mux.HandleFunc("GET /stats/{$}", handleExportIndex)
	mux.HandleFunc("GET /stats/{file}", g.handleExport)
	if g.accounts != nil {
		g.accountRoutes(mux)
	}
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { fmt.Fprintln(w, "ok") })
	return mux
}

type frontendCtx struct{}

// FrontendHandler serves the same API on the private listener for the trusted front end
// (Config.FrontendAddr). Only requests that arrive here may use forward_users keys.
func (g *Gateway) FrontendHandler() http.Handler {
	h := g.Handler()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), frontendCtx{}, true)))
	})
}

func (g *Gateway) user(r *http.Request) (string, bool) {
	k, _, ok := g.authenticate(r)
	return k.User, ok
}

// authenticate resolves the request's API key: configured keys first, then keys that accounts
// created themselves. accountID is set for the latter. Every endpoint uses this, so a key works
// the same everywhere.
func (g *Gateway) authenticate(r *http.Request) (info KeyInfo, accountID string, ok bool) {
	key := bearer(r)
	if p := g.project(key); p != nil { // a science project's key (project.go)
		info = p.info()
		info.Project = p.ID
		return info, "", true
	}
	if info, ok = g.keys.Load().Lookup(r.Context(), key); ok {
		if info.ForwardUsers && r.Context().Value(frontendCtx{}) == nil {
			return KeyInfo{}, "", false // front-end keys work only on the private listener
		}
		return info, "", true
	}
	if g.accounts != nil {
		var agents bool
		if accountID, agents, ok = g.accounts.keyAccount(key); ok {
			info = g.accountInfo(accountID)
			info.VolunteerAgents = agents
			return info, accountID, true
		}
	}
	return KeyInfo{}, "", false
}

func bearer(r *http.Request) string {
	return strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
}

// minAdvertisedCtx: smaller announced contexts do not lower what /v1/models advertises.
const minAdvertisedCtx = 8192

func (g *Gateway) handleModels(w http.ResponseWriter, r *http.Request) {
	if _, ok := g.user(r); !ok {
		apiError(w, http.StatusUnauthorized, "invalid API key")
		return
	}
	// One entry per model or role, with the context that is reliably available for it: the
	// smallest among the connected nodes serving it, so that a client sizing its compaction
	// from context_length (opencode) fits on every node. Announcements below
	// minAdvertisedCtx count only if no node offers more: one node announcing a tiny context
	// must not shrink it for everyone (it still gets the requests that fit it).
	var ids []string
	small, all := map[string]int{}, map[string]int{} // smallest ≥ minAdvertisedCtx; smallest of any
	g.mu.Lock()
	for _, n := range g.node {
		if n.held {
			continue
		}
		for _, m := range n.models {
			for _, id := range append([]string{m.Name}, m.Roles...) {
				if _, ok := all[id]; !ok {
					ids = append(ids, id)
					all[id] = m.Ctx
				}
				if m.Ctx > 0 && (all[id] == 0 || m.Ctx < all[id]) {
					all[id] = m.Ctx
				}
				if m.Ctx >= minAdvertisedCtx && (small[id] == 0 || m.Ctx < small[id]) {
					small[id] = m.Ctx
				}
			}
		}
	}
	g.mu.Unlock()
	data := []map[string]any{}
	for _, id := range ids {
		e := map[string]any{"id": id, "object": "model", "owned_by": "AIs@home"}
		if c := cmp.Or(small[id], all[id]); c > 0 {
			e["context_length"] = c
		}
		data = append(data, e)
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": data})
}

// keyLimit is a key whose budget and request limits a chat request is held to.
type keyLimit struct {
	k    string
	info KeyInfo
}

func (g *Gateway) handleChat(w http.ResponseWriter, r *http.Request) {
	key := bearer(r)
	info, accountID, ok := g.authenticate(r)
	if !ok {
		apiError(w, http.StatusUnauthorized, "invalid API key")
		return
	}
	// End users behind a trusted front end (Open WebUI) are identified by its pseudonymous
	// user id and get their own limits, on top of the front end's key limits. Signed-in
	// accounts arrive as gh-<id>@users.invalid, google-<sub>@users.invalid or
	// em-<hex>@users.invalid and share one budget with their API keys.
	acct, userKey, reqAcct := info, key, accountID // reqAcct: the account behind the request, API key or chat
	// The workload class (project.go): the chat front end's users, people's own API keys, or
	// a science project.
	class, proj := ClassAgent, (*Project)(nil)
	if info.Project != "" {
		if proj = g.projects.Load().byID[info.Project]; proj == nil { // removed by a reload just now
			apiError(w, http.StatusUnauthorized, "invalid API key")
			return
		}
		class, userKey = ClassScience, projectUser(proj.ID) // one budget for all its keys
	} else if info.ForwardUsers {
		class = ClassChat
	}
	if accountID != "" {
		userKey = "acct:" + accountID
	} else if info.ForwardUsers {
		if id, isAcct := accountFromEmail(r.Header.Get("X-OpenWebUI-User-Email")); isAcct {
			if g.accounts == nil || !g.accounts.active(id) { // banned or waiting: not through the chat either
				apiError(w, http.StatusForbidden, "this account is not active")
				return
			}
			acct, userKey, reqAcct = g.accountInfo(id).within(info), "acct:"+id, id
		} else if uid := r.Header.Get("X-OpenWebUI-User-Id"); uid != "" {
			acct, userKey = info.forUser(uid), key+"|"+uid
		}
	}
	now := time.Now()
	charge := []string{key}
	limits := []keyLimit{{key, info}}
	if accountID != "" || proj != nil { // an account's or a project's key: limits and budget belong to it
		charge, limits[0].k = []string{userKey}, userKey
	} else if userKey != key {
		// Only the end user is charged: a node inflating usage must not drain the shared
		// front-end key for everyone. The front end keeps its request-rate limits.
		charge = []string{userKey}
		limits = append(limits, keyLimit{userKey, acct})
	}
	// An account over its budget may still use its own nodes, which it is not charged for
	// (relay.go); the route is narrowed to them once it is built.
	overWait := 0
	for _, l := range limits {
		if wait, over := g.budget.over(l.k, l.info.TokensPerDay, now); over {
			if reqAcct != "" && l.k == userKey {
				overWait = max(wait, 1)
				continue
			}
			budgetError(w, wait)
			return
		}
	}
	// The body is read before taking a concurrency slot (a slow upload must not hold one),
	// within a deadline and a memory budget shared by all requests.
	size := int64(maxBody)
	if r.ContentLength >= 0 && r.ContentLength < size {
		size = r.ContentLength
	}
	release, status := g.reserveBody(userKey, size)
	if release == nil {
		w.Header().Set("Retry-After", "5")
		if status == http.StatusTooManyRequests {
			apiError(w, status, "too many uploads in progress for this user")
		} else {
			apiError(w, status, "the gateway is busy; try again shortly")
		}
		return
	}
	defer release()
	rc := http.NewResponseController(w)
	rc.SetReadDeadline(time.Now().Add(bodyReadTimeout))
	var body map[string]any
	err := json.NewDecoder(http.MaxBytesReader(w, r.Body, size)).Decode(&body)
	rc.SetReadDeadline(time.Time{})
	if err != nil {
		apiError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	for _, l := range limits {
		release, reason := g.lim.acquire(l.k, l.info, time.Now())
		if release == nil {
			apiError(w, http.StatusTooManyRequests, reason)
			return
		}
		defer release()
	}
	if msgs, ok := body["messages"].([]any); body == nil || !ok || len(msgs) == 0 {
		apiError(w, http.StatusBadRequest, "the body must be a JSON object with a non-empty messages array")
		return
	}
	if n, ok := body["n"].(float64); ok && n != 1 { // answers are assembled as one choice
		apiError(w, http.StatusBadRequest, "n must be 1")
		return
	}
	if msg := checkReasoningBudget(body, g.cfg.MaxMaxTokens); msg != "" {
		apiError(w, http.StatusBadRequest, msg)
		return
	}
	want, _ := body["model"].(string)
	clientStream, _ := body["stream"].(bool)
	if body["max_tokens"] == nil && body["max_completion_tokens"] == nil && g.cfg.DefaultMaxTokens > 0 {
		body["max_tokens"] = g.cfg.DefaultMaxTokens
	}
	for _, k := range []string{"max_tokens", "max_completion_tokens"} { // bounds node output and memory
		if v, ok := body[k].(float64); ok && v > float64(g.cfg.MaxMaxTokens) {
			body[k] = g.cfg.MaxMaxTokens
		}
	}
	// Privacy: personal data goes only to trusted nodes (and the requester's own); secrets
	// never reach a node at all (replaced by placeholders, restored in the answer).
	rt := g.routeFor(want, body, acct, reqAcct, acct.VolunteerAgents && !info.ForwardUsers)
	rt.class = class
	queueWait := g.cfg.QueueTimeoutSec
	if proj != nil {
		rt.project, rt.share = proj.ID, proj.MaxNodeShare
		if proj.QueueTimeoutSec > 0 {
			queueWait = proj.QueueTimeoutSec
		}
	}
	if overWait > 0 {
		rt.onlySelf = true
		if g.servesFor(rt) == 0 {
			budgetError(w, overWait)
			return
		}
	}
	reqText := requestText(body) // before redaction: addresses the user wrote, for the guards
	red := newRedactor()
	// Nothing to hide only when the key itself belongs to the operator whose nodes serve it
	// (not for end users behind a shared front-end key).
	ownNodes := !info.ForwardUsers && info.OnlyOperator != "" && info.OnlyOperator == info.User
	if !ownNodes {
		red.redactBody(body)
	}
	red.restoreOn = acct.RestoreSecrets
	if red.overflow {
		apiError(w, http.StatusBadRequest, "too many secrets in one request; remove credentials and try again")
		return
	}

	// Rendered and hashed once, as a node will get it: routing looks for the node that holds
	// this conversation (routing.go), credit for what that node had cached (credit_http.go).
	conv := g.pcache.print(forNode(body))
	rt.who, rt.conv = keyHash(userKey), conv.hashes
	req := &chatReq{user: acct.User, who: userKey, charge: charge, self: rt.self, want: want,
		clientStream: clientStream, logprobs: wantsLogprobs(body), body: body, red: red, conv: conv,
		servedBy: g.servedBy(info, accountID), class: class, project: rt.project}
	deadline := time.Now().Add(time.Duration(queueWait) * time.Second)
	preempted := 0
	for attempt := 0; attempt < 2; attempt++ { // one retry before any output, on another connection
		n, model, why := g.pickWait(r.Context(), rt, deadline)
		if n == nil {
			switch {
			case why == clientGone:
			case preempted > 0: // it had a node, and gave it to a person
				preemptedError(w)
			case attempt == 0:
				g.unavailable(w, rt, why)
			default:
				apiError(w, http.StatusBadGateway, "request failed on all available nodes")
			}
			return
		}
		rt.exclude[n] = true
		var gd *guard // answers from strangers' nodes are checked (guard.go)
		if !n.trusted && n.operator != rt.self {
			gd = newGuard(reqText)
		}
		started, err := g.run(w, r, req, n, model, gd)
		if errors.Is(err, errPreempted) && !started {
			// Nothing reached the client: send it again, behind the people, on any node (the
			// one it gave up included). This is no failover attempt, and it gets some time to
			// wait even near its deadline.
			if preempted++; preempted > maxPreemptRetries {
				preemptedError(w)
				return
			}
			delete(rt.exclude, n)
			attempt--
			deadline = later(deadline, time.Now().Add(30*time.Second))
			continue
		}
		var flagged *guardError
		if errors.As(err, &flagged) && !started {
			// Not one byte of the stopped answer went out: try a trusted node if one is free now,
			// within all of the request's restrictions (operator, own nodes only, context).
			fb := *rt
			fb.trustedOnly, fb.preferSelf = true, false
			if tn, tmodel := g.pick(&fb); tn != nil {
				if started, err = g.run(w, r, req, tn, tmodel, nil); err == nil || started {
					return
				}
				if errors.Is(err, errPreempted) {
					preemptedError(w)
					return
				}
			}
			apiError(w, http.StatusBadGateway, "the volunteer node's answer was stopped by a safety check (a tool call that "+
				"downloads and runs code, reads credentials, sends files, or uses a web address not in the conversation), "+
				"and no trusted node is free to answer instead; try again later")
			return
		}
		if err == nil || started {
			return // success, or failure after output began: the client sees the error
		}
		if isContextOverflow(err.Error()) { // the request itself is too large: no retry
			contextError(w, err.Error())
			return
		}
		if errors.Is(err, errGatewayBusy) {
			w.Header().Set("Retry-After", "10")
			apiError(w, http.StatusServiceUnavailable, "the gateway is busy; try again shortly")
			return
		}
		g.log.Warn("node failed before output, retrying", "node", n.id, "err", errCategory(err.Error()))
	}
	apiError(w, http.StatusBadGateway, "request failed on all available nodes")
}

// servedBy reports whether a request's answer may name the node that served it: only for the
// operator's own keys, config keys marked served_by or keys of admin accounts (the runner and
// dogfooding), never for other accounts or the users behind a front end (KeyInfo.ServedBy).
func (g *Gateway) servedBy(info KeyInfo, accountID string) bool {
	if accountID != "" { // an account's own API key
		return slices.Contains(g.acfg.Admins, accountID)
	}
	return info.ServedBy && !info.ForwardUsers
}

func later(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

// contextError answers as OpenAI does, so clients can compact and retry.
func contextError(w http.ResponseWriter, detail string) {
	writeJSON(w, http.StatusBadRequest, map[string]any{"error": map[string]any{
		"message": "the conversation is longer than this node's context; shorten or compact it",
		"type":    "invalid_request_error", "code": "context_length_exceeded"}})
}

func apiError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]any{"error": map[string]any{"message": msg, "type": http.StatusText(code)}})
}

// budgetError: the daily budget is used up; wait is the number of seconds until it resets.
func budgetError(w http.ResponseWriter, wait int) {
	w.Header().Set("Retry-After", strconv.Itoa(wait))
	apiError(w, http.StatusTooManyRequests, "daily token budget used up; it resets at 00:00 UTC")
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

// hasTools reports whether a request carries tool definitions (an agent request).
func hasTools(body map[string]any) bool {
	tools, _ := body["tools"].([]any)
	return len(tools) > 0
}

// maxBudgetMessage bounds reasoning_budget_message, in bytes. The node forces the message
// into the answer token by token when the budget runs out, so it costs decode time and
// context; the longest message in use is about 140 bytes.
const maxBudgetMessage = 512

// checkReasoningBudget validates the thinking limits a request may set (llama.cpp v0.5.0,
// tools/server/server-common.cpp): thinking_budget_tokens or reasoning_budget_tokens (one
// setting under two names; llama.cpp prefers the second) replaces the node's
// --reasoning-budget for this request, and -1 keeps it; reasoning_budget_message is the text
// forced before </think> when the budget runs out. It returns why the request is refused, or
// "". A budget is bounded by the largest max_tokens (more means nothing). The message is
// tokenized with special tokens recognised, so "<|im_end|>" in it would end the answer:
// it must be plain text without '<'.
func checkReasoningBudget(body map[string]any, maxTokens int) string {
	var set []float64
	for _, k := range []string{"thinking_budget_tokens", "reasoning_budget_tokens"} {
		v, ok := body[k]
		if !ok || v == nil {
			continue
		}
		f, isNum := v.(float64)
		if !isNum || f != math.Trunc(f) || f < -1 || f > float64(maxTokens) {
			return fmt.Sprintf("%s must be an integer from 0 to %d, or -1 for the node's default", k, maxTokens)
		}
		set = append(set, f)
	}
	if len(set) == 2 && set[0] != set[1] {
		return "thinking_budget_tokens and reasoning_budget_tokens are one setting; set only one"
	}
	if v, ok := body["reasoning_budget_message"]; ok && v != nil {
		s, isStr := v.(string)
		plain := isStr && len(s) <= maxBudgetMessage && !strings.ContainsRune(s, '<')
		for _, r := range s {
			if plain && unicode.IsControl(r) && r != '\n' && r != '\t' {
				plain = false
			}
		}
		if !plain {
			return fmt.Sprintf("reasoning_budget_message must be plain text of at most %d bytes, "+
				"without '<' or control characters other than newline and tab", maxBudgetMessage)
		}
	}
	return ""
}
