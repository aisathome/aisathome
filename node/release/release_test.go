// SPDX-License-Identifier: Apache-2.0
package release

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestCompare(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want int
		ok   bool
	}{
		{"v0.7.0", "v0.7.0", 0, true},
		{"v0.7.0", "0.7.0", 0, true},
		{"v0.8.0", "v0.7.0", 1, true},
		{"v0.7.0", "v0.8.0", -1, true},
		{"v0.10.0", "v0.9.0", 1, true}, // numbers, not text
		{"v0.9.0", "v0.10.0", -1, true},
		{"v1.0.0", "v0.99.99", 1, true},
		{"v0.7.1", "v0.7.0", 1, true},
		{"v0.7.0", "v0.7.1", -1, true},
		{"v1.2.3", "v1.3.0", -1, true},
		{"v2.0.0", "v1.9.9", 1, true},
		{"v0.8.0", "v0.8.0-rc.1", 1, true}, // a release is newer than its pre-releases
		{"v0.8.0-rc.1", "v0.8.0", -1, true},
		{"v0.8.0-rc.2", "v0.8.0-rc.1", 1, true},
		{"v0.8.0-rc.10", "v0.8.0-rc.9", 1, true}, // numeric identifiers as numbers
		{"v0.8.0-alpha", "v0.8.0-1", 1, true},    // words after numbers
		{"v0.8.0-1", "v0.8.0-alpha", -1, true},
		{"v0.8.0-alpha", "v0.8.0-beta", -1, true},
		{"v0.8.0-rc", "v0.8.0-rc.1", -1, true}, // fewer identifiers first
		{"v0.8.0-rc.1", "v0.8.0-rc", 1, true},
		{"v0.8.0-rc.1", "v0.8.0-rc.1", 0, true},
		{"v0.8.0+build.5", "v0.8.0", 0, true},
		{"v0.7.9-rc.1", "v0.8.0-rc.1", -1, true},
		{"dev", "v0.7.0", 0, false},
		{"v0.7.0", "dev", 0, false},
		{"v0.7", "v0.7.0", 0, false},
		{"v0.7.0.1", "v0.7.0", 0, false},
		{"v0.07.0", "v0.7.0", 0, false},
		{"v0.7.x", "v0.7.0", 0, false},
		{"v0.7.0-", "v0.7.0", 0, false},
		{"v0.7.0-rc..1", "v0.7.0", 0, false},
		{"", "v0.7.0", 0, false},
		{"v-1.0.0", "v0.7.0", 0, false},
	} {
		got, ok := Compare(c.a, c.b)
		if got != c.want || ok != c.ok {
			t.Errorf("Compare(%q, %q) = %d, %v; want %d, %v", c.a, c.b, got, ok, c.want, c.ok)
		}
	}
	for _, c := range []struct {
		latest, current string
		want            bool
	}{
		{"v0.8.0", "v0.7.0", true},
		{"v0.7.0", "v0.7.0", false},
		{"v0.6.0", "v0.7.0", false},
		{"v0.8.0", "dev", false}, // a development build is never told to update
		{"", "v0.7.0", false},
	} {
		if got := Newer(c.latest, c.current); got != c.want {
			t.Errorf("Newer(%q, %q) = %v", c.latest, c.current, got)
		}
	}
}

const gwRelease = `{"version": "v0.8.0", "installerBytes": 390380154, "bundleBytes": 478190808, "versionFreeInstaller": true, "fetched_at": "2026-11-26T12:00:00Z"}`

// The gateway's endpoint is on the host the node connects to: https for wss, http for a ws:// gateway on
// this machine only; nothing else.
func TestEndpointURL(t *testing.T) {
	for in, want := range map[string]string{
		"wss://api.aisathome.org/v1/node/connect":    "https://api.aisathome.org/v1/node/release",
		"wss://gw.example:8443/v1/node/connect?x=1":  "https://gw.example:8443/v1/node/release",
		" wss://api.aisathome.org/v1/node/connect ":  "https://api.aisathome.org/v1/node/release",
		"ws://127.0.0.1:18081/v1/node/connect":       "http://127.0.0.1:18081/v1/node/release",
		"ws://localhost:8080/v1/node/connect":        "http://localhost:8080/v1/node/release",
		"ws://[::1]:8080/v1/node/connect":            "http://[::1]:8080/v1/node/release",
		"ws://gw.example/v1/node/connect":            "",
		"ws://10.0.0.5/v1/node/connect":              "",
		"https://api.aisathome.org/v1/node/connect":  "",
		"wss://user:pw@api.aisathome.org/v1/connect": "",
		"wss:///v1/node/connect":                     "",
		"":                                           "",
		"api.aisathome.org":                          "",
	} {
		got, err := EndpointURL(in)
		if got != want || (want == "") != (err != nil) {
			t.Errorf("EndpointURL(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
}

func TestParseGateway(t *testing.T) {
	rel, err := ParseGateway([]byte(gwRelease))
	if err != nil || rel != (Release{Version: "v0.8.0", URL: "https://github.com/aisathome/aisathome/releases/tag/v0.8.0", BundleBytes: 478190808}) {
		t.Fatalf("%+v %v", rel, err)
	}
	for name, body := range map[string]string{
		"rc tag":     strings.Replace(gwRelease, `"v0.8.0"`, `"v0.8.0-rc.1"`, 1),
		"odd tag":    strings.Replace(gwRelease, `"v0.8.0"`, `"<b>v0.8.0</b>"`, 1),
		"path tag":   strings.Replace(gwRelease, `"v0.8.0"`, `"v0.8.0/../../evil"`, 1),
		"no version": `{"bundleBytes": 1}`,
		"not json":   `<html>502 bad gateway</html>`,
	} {
		if rel, err := ParseGateway([]byte(body)); err == nil {
			t.Errorf("%s: accepted: %+v", name, rel)
		}
	}
	if rel, err := ParseGateway([]byte(`{"version": "v1.0.0", "bundleBytes": -5}`)); err != nil || rel.BundleBytes != 0 {
		t.Fatalf("negative size: %+v %v", rel, err)
	}
}

// Latest asks the configured gateway's endpoint, with no token, and follows no redirect; 503 (the gateway
// has not read the release yet) and no gateway are "not known".
func TestGatewayLatest(t *testing.T) {
	var path, ua, auth string
	code, body := http.StatusOK, gwRelease
	elsewhere := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("followed a redirect")
	}))
	defer elsewhere.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path, ua, auth = r.URL.Path, r.Header.Get("User-Agent"), r.Header.Get("Authorization")
		if code == http.StatusFound {
			http.Redirect(w, r, elsewhere.URL+"/v1/node/release", http.StatusFound)
			return
		}
		w.WriteHeader(code)
		w.Write([]byte(body))
	}))
	defer srv.Close()
	gw := "ws://" + strings.TrimPrefix(srv.URL, "http://") + "/v1/node/connect"
	g := Gateway{GatewayURL: func() string { return gw }}
	rel, err := g.Latest(context.Background())
	if err != nil || rel.Version != "v0.8.0" || rel.BundleBytes != 478190808 || path != "/v1/node/release" || ua != "aisathome-node" || auth != "" {
		t.Fatalf("%+v %v path=%q ua=%q auth=%q", rel, err, path, ua, auth)
	}
	code, body = http.StatusServiceUnavailable, `{"error":{"message":"the latest node release is not known yet"}}`
	if _, err := g.Latest(context.Background()); !errors.Is(err, ErrUnknown) {
		t.Fatalf("503: %v", err)
	}
	code = http.StatusFound
	if _, err := g.Latest(context.Background()); err == nil || !strings.Contains(err.Error(), "302") {
		t.Fatalf("redirect: %v", err)
	}
	code = http.StatusNotFound // a gateway from before the endpoint
	if _, err := g.Latest(context.Background()); err == nil || errors.Is(err, ErrUnknown) {
		t.Fatalf("404: %v", err)
	}
	code, body = http.StatusOK, `{"version": "`+strings.Repeat("x", maxBody)+`"}`
	if _, err := g.Latest(context.Background()); err == nil || !strings.Contains(err.Error(), "larger") {
		t.Fatalf("oversize: %v", err)
	}
	for _, none := range []Gateway{{}, {GatewayURL: func() string { return "" }}} {
		if _, err := none.Latest(context.Background()); !errors.Is(err, ErrUnknown) {
			t.Fatalf("no gateway: %v", err)
		}
	}
	if _, err := (Gateway{GatewayURL: func() string { return "ws://gw.example/v1/node/connect" }}).Latest(context.Background()); err == nil {
		t.Fatal("a plain ws:// gateway elsewhere was asked")
	}
}

type fakeSource struct {
	mu    sync.Mutex
	rel   Release
	err   error
	calls int
	block chan struct{} // when set, Latest waits for it
}

func (f *fakeSource) Latest(context.Context) (Release, error) {
	f.mu.Lock()
	f.calls++
	block := f.block
	f.mu.Unlock()
	if block != nil {
		<-block
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.rel, f.err
}

func (f *fakeSource) n() int { f.mu.Lock(); defer f.mu.Unlock(); return f.calls }

// waitCalls waits until the source was asked n times and the cache stored the answer.
func waitCalls(t *testing.T, c *Cache, f *fakeSource, n int) {
	t.Helper()
	for i := 0; i < 500; i++ {
		c.mu.Lock()
		reading := c.reading
		c.mu.Unlock()
		if f.n() >= n && !reading {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("source asked %d times, want %d", f.n(), n)
}

// The cache asks once per TTL, again after Retry when a read failed (keeping the last good release), and
// never makes its caller wait.
func TestCache(t *testing.T) {
	f := &fakeSource{rel: Release{Version: "v0.8.0"}, block: make(chan struct{})}
	c := NewCache(f, time.Hour, 10*time.Minute)
	now := time.Now()
	c.now = func() time.Time { return now }
	done := make(chan struct{})
	go func() {
		defer close(done)
		if _, ok := c.Known(); ok { // the read runs in the background, blocked
			t.Error("known before any read")
		}
		c.Known()
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Known waited for the source")
	}
	close(f.block)
	waitCalls(t, c, f, 1)
	if rel, ok := c.Known(); !ok || rel.Version != "v0.8.0" || f.n() != 1 {
		t.Fatalf("after a read: %+v %v, %d reads", rel, ok, f.n())
	}
	// Known starts a read synchronously (c.reading) or not at all.
	started := func() bool { c.mu.Lock(); defer c.mu.Unlock(); return c.reading }
	now = now.Add(59 * time.Minute)
	c.Known()
	if started() || f.n() != 1 {
		t.Fatal("read again within the TTL")
	}
	// After the TTL, a failed read keeps the last good release and is retried after Retry, not before.
	f.mu.Lock()
	f.err = errors.New("HTTP 403")
	f.mu.Unlock()
	now = now.Add(2 * time.Minute)
	c.Known()
	waitCalls(t, c, f, 2)
	if rel, ok := c.Known(); !ok || rel.Version != "v0.8.0" || c.Err() == nil {
		t.Fatalf("after a failed read: %+v %v err=%v", rel, ok, c.Err())
	}
	now = now.Add(9 * time.Minute)
	c.Known()
	if started() || f.n() != 2 {
		t.Fatalf("retried within %v: %d reads", 10*time.Minute, f.n())
	}
	f.mu.Lock()
	f.err, f.rel = nil, Release{Version: "v0.9.0"}
	f.mu.Unlock()
	now = now.Add(2 * time.Minute)
	c.Known()
	waitCalls(t, c, f, 3)
	if rel, _ := c.Known(); rel.Version != "v0.9.0" || c.Err() != nil {
		t.Fatalf("after the retry: %+v %v", rel, c.Err())
	}
	// Refresh reads at once.
	if rel, err := c.Refresh(context.Background()); err != nil || rel.Version != "v0.9.0" || f.n() != 4 {
		t.Fatalf("refresh: %+v %v", rel, err)
	}
	if d := NewCache(f, 0, 0); d.ttl != DefaultTTL || d.retry != DefaultRetry || DefaultTTL < time.Hour {
		t.Fatalf("defaults: %v %v", d.ttl, d.retry)
	}
}
