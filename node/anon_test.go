// SPDX-License-Identifier: Apache-2.0
package node

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aisathome/aisathome/protocol"
)

// A node without an account asks the gateway about itself with its own token and shows the answer
// (text cleaned) in its status; an account's node does not ask, and shows nothing of the kind.
func TestAnonymousSelf(t *testing.T) {
	const tok = "nt-a-0a0b0c0d.BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB"
	var asked atomic.Int32
	var auth atomic.Value
	gw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != protocol.SelfPath || r.Method != http.MethodGet {
			http.NotFound(w, r)
			return
		}
		asked.Add(1)
		auth.Store(r.Header.Get("Authorization"))
		json.NewEncoder(w).Encode(protocol.NodeSelf{NodeID: "a-0a0b0c0d", Anonymous: true, State: protocol.SelfChecking,
			ChecksPassed: 1, ChecksNeeded: 3, Message: "Checking\x07 the model"})
	}))
	defer gw.Close()
	if u, err := selfURL("ws" + strings.TrimPrefix(gw.URL, "http") + "/v1/node/connect"); err != nil || u != gw.URL+protocol.SelfPath {
		t.Fatalf("selfURL: %q %v", u, err)
	}
	if u, _ := selfURL("wss://api.example.org/v1/node/connect"); u != "https://api.example.org/v1/node/self" {
		t.Fatalf("selfURL wss: %q", u)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	a := New(Config{GatewayURL: "ws" + strings.TrimPrefix(gw.URL, "http") + "/v1/node/connect", Token: tok}, log)
	if s := a.Status(); s.Anonymous == nil || s.Anonymous.State != "" {
		t.Fatalf("before the gateway answered: %+v", s.Anonymous)
	}
	s, err := a.fetchSelf(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	a.mu.Lock()
	s.Message = cleanText(s.Message, 300)
	a.self = &s
	a.mu.Unlock()
	st := a.Status()
	if st.Anonymous == nil || st.Anonymous.State != protocol.SelfChecking || st.Anonymous.ChecksPassed != 1 || st.Anonymous.ChecksNeeded != 3 ||
		st.Anonymous.Message != "Checking the model" {
		t.Fatalf("status %+v", st.Anonymous)
	}
	if auth.Load() != "Bearer "+tok {
		t.Fatalf("asked with %v", auth.Load())
	}
	b, _ := json.Marshal(st)
	if strings.Contains(string(b), "BBBB") {
		t.Fatal("the status shows the token")
	}
	// An account's node: no question, nothing shown.
	n := asked.Load()
	acct := New(Config{GatewayURL: "ws" + strings.TrimPrefix(gw.URL, "http") + "/v1/node/connect", Token: "nt-n-0a0b0c0d.x"}, log)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	acct.watchSelf(ctx) // returns at once for an account's node
	if asked.Load() != n || acct.Status().Anonymous != nil {
		t.Fatal("an account's node asked about itself or shows the panel")
	}
}
