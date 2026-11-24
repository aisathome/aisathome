// SPDX-License-Identifier: Apache-2.0
package node

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestCheckGatewayURL(t *testing.T) {
	for u, ok := range map[string]bool{
		"wss://api.aisathome.org/v1/node/connect": true,
		"https://gw.example:8443":                 true,
		"ws://127.0.0.1:8080/v1/node/connect":     true,
		"ws://[::1]:8080/v1/node/connect":         true,
		"ws://localhost:8080/v1/node/connect":     true,
		"ws://gw.example/v1/node/connect":         false,
		"http://10.0.0.2/v1/node/connect":         false,
		"ws://127.0.0.1.gw.example/v1":            false,
		"ftp://gw.example/":                       false,
		"wss://":                                  false,
		"":                                        false,
	} {
		if err := CheckGatewayURL(u); (err == nil) != ok {
			t.Errorf("%q: %v, want ok %v", u, err, ok)
		}
	}
}

// redirectingGateway answers everything with a redirect to elsewhere, which records any Authorization
// header it gets: Go would send it on to the same host on another port.
func redirectingGateway(t *testing.T) (gw *httptest.Server, leaked *atomic.Value) {
	leaked = &atomic.Value{}
	leaked.Store("")
	elsewhere := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if a := r.Header.Get("Authorization"); a != "" {
			leaked.Store(a)
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(elsewhere.Close)
	gw = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, elsewhere.URL+r.URL.Path, http.StatusTemporaryRedirect)
	}))
	t.Cleanup(gw.Close)
	return gw, leaked
}

// The node's token never follows a redirect (code review 2026-11-24): not on the WebSocket connection, not
// with /v1/node/self.
func TestTokenNeverFollowsARedirect(t *testing.T) {
	const tok = "nt-a-0a0b0c0d.BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB"
	gw, leaked := redirectingGateway(t)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	a := New(Config{GatewayURL: "ws" + strings.TrimPrefix(gw.URL, "http") + "/v1/node/connect", Token: tok}, log)
	if err := a.session(context.Background()); !errors.Is(err, ErrRedirect) {
		t.Errorf("session: %v, want the redirect refused", err)
	}
	if _, err := a.fetchSelf(context.Background()); !errors.Is(err, ErrRedirect) {
		t.Errorf("fetchSelf: %v, want the redirect refused", err)
	}
	if l := leaked.Load(); l != "" {
		t.Fatalf("the token went on to the redirect's target: %q", l)
	}
}

// Nor in plain text across a network: ws:// only to this machine.
func TestTokenNeverInPlainTextAcrossANetwork(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	a := New(Config{GatewayURL: "ws://gw.example/v1/node/connect", Token: "nt-a-0a0b0c0d.x"}, log)
	if err := a.session(context.Background()); err == nil || !strings.Contains(err.Error(), "not encrypted") {
		t.Errorf("session over ws:// to another machine: %v", err)
	}
	if _, err := a.fetchSelf(context.Background()); err == nil || !strings.Contains(err.Error(), "not encrypted") {
		t.Errorf("fetchSelf over ws:// to another machine: %v", err)
	}
}
