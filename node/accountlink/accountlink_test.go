// SPDX-License-Identifier: Apache-2.0
package accountlink

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aisathome/aisathome/node"
	"github.com/aisathome/aisathome/protocol"
)

func TestAPIBase(t *testing.T) {
	for in, want := range map[string]string{
		"wss://api.aisathome.org/v1/node/connect": "https://api.aisathome.org",
		"ws://127.0.0.1:8080/v1/node/connect":     "http://127.0.0.1:8080",
		"https://gw.example:8443":                 "https://gw.example:8443",
	} {
		if got, err := APIBase(in); err != nil || got != want {
			t.Errorf("APIBase(%q) = %q, %v", in, got, err)
		}
	}
	for _, in := range []string{"", "ftp://x/", "wss://", "wss://user:pw@host/", "api.aisathome.org"} {
		if got, err := APIBase(in); err == nil {
			t.Errorf("APIBase(%q) = %q, accepted", in, got)
		}
	}
}

func TestValidURL(t *testing.T) {
	for _, u := range []string{"https://aisathome.org/link?code=KXQT-4821", "http://127.0.0.1:8080/link"} {
		if !ValidURL(u) {
			t.Errorf("%q refused", u)
		}
	}
	for _, u := range []string{"javascript:alert(1)", "file:///etc/passwd", "https://a@b/", "https://x/\nhi", "//x/", "https:///x"} {
		if ValidURL(u) {
			t.Errorf("%q accepted", u)
		}
	}
}

func TestPrettyName(t *testing.T) {
	if got := prettyNameOf("NAME=Ubuntu\nPRETTY_NAME=\"Ubuntu 24.04.1 LTS\"\n"); got != "Ubuntu 24.04.1 LTS" {
		t.Errorf("got %q", got)
	}
}

// Wait follows the gateway's interval and slow_down, retries a failed poll, and stops on approval,
// denial, expiry or a token that is not a node token.
func TestWait(t *testing.T) {
	type step struct {
		status int
		poll   protocol.LinkPoll
	}
	run := func(steps []step) (protocol.LinkPoll, []time.Duration, error) {
		i := 0
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var req protocol.LinkPollRequest
			json.NewDecoder(r.Body).Decode(&req)
			if req.DeviceCode != "dev" || r.URL.Path != protocol.LinkPollPath || r.Header.Get("Content-Type") != "application/json" {
				t.Errorf("poll %s %+v", r.URL.Path, req)
			}
			s := steps[min(i, len(steps)-1)]
			i++
			w.WriteHeader(s.status)
			json.NewEncoder(w).Encode(s.poll)
		}))
		defer srv.Close()
		var slept []time.Duration
		c := &Client{Base: srv.URL, Sleep: func(_ context.Context, d time.Duration) error { slept = append(slept, d); return nil }}
		p, err := c.Wait(context.Background(), protocol.LinkStart{DeviceCode: "dev", Interval: 5, ExpiresIn: 600})
		return p, slept, err
	}
	tok := "nt-n-1a2b3c4d." + strings.Repeat("A", 43)
	p, slept, err := run([]step{{200, protocol.LinkPoll{Status: "pending", Interval: 5}}, {503, protocol.LinkPoll{}},
		{200, protocol.LinkPoll{Status: "slow_down", Interval: 10}}, {200, protocol.LinkPoll{Status: "approved", Token: tok, NodeID: "n-1a2b3c4d"}}})
	if err != nil || p.Token != tok {
		t.Fatalf("approved: %+v %v", p, err)
	}
	if want := []time.Duration{5 * time.Second, 5 * time.Second, 5 * time.Second, 10 * time.Second}; len(slept) != 4 || slept[3] != want[3] || slept[0] != want[0] {
		t.Errorf("slept %v, want %v", slept, want)
	}
	if _, _, err := run([]step{{200, protocol.LinkPoll{Status: "denied"}}}); !errors.Is(err, ErrDenied) {
		t.Errorf("denied: %v", err)
	}
	if _, _, err := run([]step{{200, protocol.LinkPoll{Status: "expired"}}}); !errors.Is(err, ErrExpired) {
		t.Errorf("expired: %v", err)
	}
	if _, _, err := run([]step{{200, protocol.LinkPoll{Status: "approved", Token: "ak-not-a-node-token"}}}); err == nil {
		t.Error("an approval without a node token was taken")
	}
	// The token goes into token.env (a systemd EnvironmentFile): only the exact form the gateway issues
	// is taken, so a gateway cannot add lines or settings to it.
	for _, bad := range []string{
		tok + "\nLD_PRELOAD=/tmp/x.so",
		tok + "\n",
		"nt-n-1a2b3c4d." + strings.Repeat("A", 20) + "=" + strings.Repeat("A", 22),
		"nt-n-1a2b3c4d." + strings.Repeat("A", 20) + " " + strings.Repeat("A", 22),
		"nt-n-1a2b3c4d." + strings.Repeat("A", 42),
		"nt-n-1a2b3c4d." + strings.Repeat("A", 44),
		"nt-n-1a2b3c4d.secret",
	} {
		p, _, err := run([]step{{200, protocol.LinkPoll{Status: "approved", Token: bad, NodeID: "n-1a2b3c4d"}}})
		if err == nil || p.Token != "" {
			t.Errorf("approval with token %q taken: %+v %v", bad, p, err)
		}
	}
	if _, _, err := run([]step{{200, protocol.LinkPoll{Status: "approved", Token: tok, NodeID: "n-ffffffff"}}}); err == nil {
		t.Error("an approval naming another node id than its token carries was taken")
	}
	if p, _, err := run([]step{{200, protocol.LinkPoll{Status: "approved", Token: tok}}}); err != nil || p.NodeID != "n-1a2b3c4d" {
		t.Errorf("approval without a node id: %+v %v", p, err)
	}
	// A gateway that never answers usefully: Wait gives up after the code's life.
	if _, slept, err := run([]step{{500, protocol.LinkPoll{}}}); !errors.Is(err, ErrExpired) || len(slept) < 100 {
		t.Errorf("lost gateway: %v after %d polls", err, len(slept))
	}
}

func TestStartRefusals(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.UserAgent(), "aisathome-node/") && r.URL.Path == protocol.LinkPath {
			w.WriteHeader(http.StatusTooManyRequests)
			w.Write([]byte(`{"error":{"message":"too many link codes asked from this address"}}`))
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()
	_, err := (&Client{Base: srv.URL}).Start(context.Background(), protocol.LinkInfo{})
	if err == nil || !strings.Contains(err.Error(), "too many link codes") {
		t.Errorf("429: %v", err)
	}
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(protocol.LinkStart{DeviceCode: "d", UserCode: "KXQT-4821", VerificationURIComplete: "javascript:alert(1)"})
	}))
	defer bad.Close()
	if _, err := (&Client{Base: bad.URL}).Start(context.Background(), protocol.LinkInfo{}); err == nil {
		t.Error("a link that is not http(s) was taken")
	}
}

// Linking and registering carry a node's token (or get one): a redirect is refused, never followed with
// the token (code review 2026-11-24); and a ws:// gateway off this machine is refused.
func TestClientRefusesRedirects(t *testing.T) {
	var leaked atomic.Value
	leaked.Store("")
	elsewhere := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if a := r.Header.Get("Authorization"); a != "" {
			leaked.Store(a)
		}
		http.NotFound(w, r)
	}))
	defer elsewhere.Close()
	gw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, elsewhere.URL+r.URL.Path, http.StatusTemporaryRedirect)
	}))
	defer gw.Close()
	c := &Client{Base: gw.URL, Token: "nt-a-0a0b0c0d.BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB"}
	if _, err := c.Start(context.Background(), protocol.LinkInfo{}); !errors.Is(err, node.ErrRedirect) {
		t.Errorf("Start: %v, want the redirect refused", err)
	}
	if _, err := c.Self(context.Background()); !errors.Is(err, node.ErrRedirect) {
		t.Errorf("Self: %v, want the redirect refused", err)
	}
	if _, err := c.Register(context.Background(), true); !errors.Is(err, node.ErrRedirect) {
		t.Errorf("Register: %v, want the redirect refused", err)
	}
	if l := leaked.Load(); l != "" {
		t.Fatalf("the token went on to the redirect's target: %q", l)
	}
	if _, err := APIBase("ws://gw.example/v1/node/connect"); err == nil {
		t.Error("APIBase took ws:// to another machine")
	}
}
