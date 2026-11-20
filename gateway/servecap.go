// Serving capacity by class: how many connected nodes may serve people's requests and science right
// now. On 2026-11-20 the operator terms gate shipped while no account had accepted them: every node
// online did science only, every chat and API request was refused ("operator_terms") for 8 hours, and
// nothing said so (the node count, the smoke test and every test were green). These counts make that
// state visible: in /v1/stats (aggregates only, for the post-deploy smoke test and the site), on the
// operations page with a health warning, and as a rate-limited WARN log line when people's requests
// are refused while it lasts.
//
// The counts ask route.allows, the placement rule itself, for a request from someone who is not the
// node's operator: whatever keeps a node from people's requests (the operator terms, a draft of them,
// the own-only switch, a pilot's science-only switch, a guard's hold, a future rule) counts. Context and
// free slots do not: these are what may serve, not what is free.
//
// SPDX-License-Identifier: Apache-2.0
package gateway

import (
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// serveCapacity counts the connected nodes, held ones excluded (as "online"), that may serve each kind
// of request from a stranger now. Public in /v1/stats as serve: counts only, never which node or
// whose.
type serveCapacity struct {
	// Chat: people's requests without tools, from the chat front end or an API key (classes chat and
	// agent).
	Chat int `json:"chat"`
	// Tools: people's requests with tools (agents) from a key that allows volunteer nodes: Chat, and
	// the node's account past its probation.
	Tools int `json:"tools"`
	// Science: a science project's requests without tools.
	Science int `json:"science"`
}

// Why online nodes may not serve people's requests (noChatWhy), for the log and the operations page.
const (
	whyOwnOnly      = "own_only"        // the operator's emergency switch
	whyTermsDraft   = "terms_draft"     // the operator terms are a draft: nobody has accepted them
	whyTerms        = "operator_terms"  // its operator has not accepted the current operator terms
	whyPilotScience = "pilot_science"   // a pilot whose connector or user keeps it to science
	whyPilotNotYet  = "pilot_not_ready" // a pilot still registering or past its deadline
	whyOther        = "other"           // another rule of route.allows
)

// capacityRoutes are the requests the counts ask about: a stranger's chat, a stranger's agent (key
// allowing volunteer nodes), a science project's request.
func (g *Gateway) capacityRoutes() (chat, tools, sci *route) {
	ownOnly := g.accounts != nil && g.accounts.Settings().OwnOnly()
	rules := g.chatRulesOn()
	return &route{class: ClassChat, rules: rules, ownOnlyMode: ownOnly},
		&route{class: ClassAgent, rules: rules, ownOnlyMode: ownOnly, tools: true, consent: true},
		&route{class: ClassScience, rules: rules, ownOnlyMode: ownOnly}
}

// serveCapacity counts the nodes that may serve each kind of request now, and the nodes online. why
// counts, for the online nodes that may not serve a stranger's chat, the first reason found.
func (g *Gateway) serveCapacity(now time.Time) (c serveCapacity, online int, why map[string]int) {
	chat, tools, sci := g.capacityRoutes() // before g.mu: the accounts store's lock comes after it
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, n := range g.node {
		if n.held {
			continue
		}
		online++
		if len(n.models) == 0 {
			continue
		}
		if sci.allows(n, now) {
			c.Science++
		}
		if tools.allows(n, now) {
			c.Tools++
		}
		if chat.allows(n, now) {
			c.Chat++
			continue
		}
		if why == nil {
			why = map[string]int{}
		}
		why[noChatWhy(chat, n, now)]++
	}
	return c, online, why
}

// noChatWhy names why rt (a stranger's chat) may not go to n. Caller holds g.mu.
func noChatWhy(rt *route, n *nodeConn, now time.Time) string {
	switch {
	case rt.ownOnly():
		return whyOwnOnly
	case n.pilot != "" && (n.registering || !now.Before(n.deadline)):
		return whyPilotNotYet
	case n.pilot != "" && n.scienceOnly:
		return whyPilotScience
	case rt.gatedBy(n) && !termsFinal():
		return whyTermsDraft
	case rt.gatedBy(n):
		return whyTerms
	}
	return whyOther
}

// whyText is why, for people: "3 operator_terms, 1 pilot_science", most first.
func whyText(why map[string]int) string {
	keys := make([]string, 0, len(why))
	for k := range why {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if why[keys[i]] != why[keys[j]] {
			return why[keys[i]] > why[keys[j]]
		}
		return keys[i] < keys[j]
	})
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = fmt.Sprintf("%d %s", why[k], k)
	}
	return strings.Join(parts, ", ")
}

// noChatWarnEvery: at most one WARN line per this long while people's requests are refused with no
// node online allowed to serve them; the next line says how many were refused meanwhile.
const noChatWarnEvery = 10 * time.Minute

// noChatLog rate-limits the WARN line.
type noChatLog struct {
	mu      sync.Mutex
	last    time.Time
	refused int // people's requests refused since the last line, while no node could serve them
}

// noChatDemand is called when a person's request (not science) was refused: if nodes are online and
// none may serve a stranger's chat, it logs a WARN, at most once per noChatWarnEvery.
func (g *Gateway) noChatDemand(class string) {
	if class == ClassScience || g.log == nil {
		return
	}
	now := time.Now()
	c, online, why := g.serveCapacity(now)
	if online == 0 || c.Chat > 0 {
		return // no node at all is logged and warned elsewhere (no_node, the "no-nodes" health warning)
	}
	g.noChat.mu.Lock()
	g.noChat.refused++
	if !g.noChat.last.IsZero() && now.Sub(g.noChat.last) < noChatWarnEvery {
		g.noChat.mu.Unlock()
		return
	}
	refused := g.noChat.refused
	g.noChat.last, g.noChat.refused = now, 0
	g.noChat.mu.Unlock()
	g.log.Warn("no online node may serve people's requests", "online", online, "serve_chat", c.Chat,
		"serve_tools", c.Tools, "serve_science", c.Science, "why", whyText(why), "refused", refused,
		"every", noChatWarnEvery.String())
}

// capacityWarning is the health warning for nodes online of which none may serve people's requests.
func capacityWarning(c serveCapacity, online int, why map[string]int) []healthWarning {
	if online == 0 || c.Chat > 0 {
		return nil
	}
	return []healthWarning{{Rule: ruleNoChat, Text: fmt.Sprintf("%d node(s) online, %d may serve science, none may serve "+
		"people's chats or API requests (%s): every one is refused.", online, c.Science, whyText(why))}}
}
