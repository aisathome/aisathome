// Why an agent request has nowhere to go (backlog v0.2.0 H, 2026-10-13): the consent advice
// sent the owner hunting for a key problem when his own node was offline or busy.
//
// SPDX-License-Identifier: Apache-2.0
package e2e

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

const consentAdvice = "Run requests through nodes operated by other people"

// blockingEngine holds every request until release is closed; entered gets one value per request.
func blockingEngine(t *testing.T, entered chan<- struct{}, release <-chan struct{}) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if health(w, r) {
			return
		}
		entered <- struct{}{}
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"done\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	}))
	t.Cleanup(srv.Close)
	return srv
}

// A key without consent, a stranger's volunteer node online (it may not take the request), no
// maintainer node: the answer names the actual reason.
func TestAgentNowhereToGoNamesTheReason(t *testing.T) {
	for _, c := range []struct {
		name    string
		own     string // "" (no node of its own), "offline" or "busy"
		retry   string
		want    string
		consent bool // the consent advice is in the answer
	}{
		{"no own node", "", "60", "requests with tools go only to your own node or the maintainer's", true},
		{"own node offline", "offline", "60", "your own node is offline", false},
		{"own node busy", "busy", "10", "are busy; try again shortly", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			var vol atomic.Int32
			a := nodeRig(t, nil)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			s2, _ := a.signIn(t, 2)
			strangerToken, _, _ := a.addNode(t, s2, "")
			a.startTokenNode(ctx, strangerToken, countEngine(t, &vol), 4, 32768)
			nodes := 1
			s1, _ := a.signIn(t, 1)
			_, page := a.createKey(t, s1) // without consent
			key := keyRe.FindString(page)
			if c.own != "" {
				token, _, _ := a.addNode(t, s1, "mine")
				if c.own == "busy" {
					entered, release := make(chan struct{}, 1), make(chan struct{})
					defer close(release)
					a.startTokenNode(ctx, token, blockingEngine(t, entered, release), 1, 32768)
					nodes++
					a.waitNodes(t, nodes)
					go func() { // takes the own node's only slot (own nodes are preferred)
						resp := a.do(t, "POST", "/v1/chat/completions", key, chatBody)
						io.Copy(io.Discard, resp.Body)
						resp.Body.Close()
					}()
					<-entered
				}
			}
			a.waitNodes(t, nodes)
			resp := a.do(t, "POST", "/v1/chat/completions", key, toolsBody)
			b, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			body := string(b)
			if resp.StatusCode != http.StatusServiceUnavailable || resp.Header.Get("Retry-After") != c.retry || !strings.Contains(body, c.want) {
				t.Fatalf("%d (Retry-After %q): %s", resp.StatusCode, resp.Header.Get("Retry-After"), body)
			}
			if strings.Contains(body, consentAdvice) != c.consent {
				t.Fatalf("consent advice should be there: %v: %s", c.consent, body)
			}
			if vol.Load() != 0 {
				t.Fatal("a tool request without consent reached the volunteer node")
			}
		})
	}
}
