// SPDX-License-Identifier: Apache-2.0
package gateway

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type relAsset struct {
	Name  string `json:"name"`
	Size  int64  `json:"size"`
	State string `json:"state"`
}

// ghRelease is a release as GitHub's API returns it (the fields the gateway reads, and a few more).
func ghRelease(tag string, edit func(m map[string]any)) []byte {
	m := map[string]any{
		"tag_name": tag, "name": tag, "draft": false, "prerelease": false, "body": "notes",
		"assets": []relAsset{
			{"SHA256SUMS", 773, "uploaded"},
			{"AIsAtHome-node-setup-" + tag + ".exe", 391_000_000, "uploaded"},
			{"AIsAtHome-node-setup.exe", 391_000_000, "uploaded"},
			{"aisathome-node-bundle-linux-amd64.tar.xz", 480_000_000, "uploaded"},
		},
	}
	if edit != nil {
		edit(m)
	}
	b, _ := json.Marshal(m)
	return b
}

func editAsset(name string, f func(a *relAsset) bool) func(m map[string]any) {
	return func(m map[string]any) {
		var out []relAsset
		for _, a := range m["assets"].([]relAsset) {
			if a.Name != name || f(&a) {
				out = append(out, a)
			}
		}
		m["assets"] = out
	}
}

func dropAsset(name string) func(m map[string]any) {
	return editAsset(name, func(*relAsset) bool { return false })
}

func TestParseNodeRelease(t *testing.T) {
	rel, err := parseNodeRelease(ghRelease("v0.8.0", nil))
	if err != nil || rel != (NodeRelease{Version: "v0.8.0", InstallerBytes: 391_000_000, BundleBytes: 480_000_000, VersionFreeInstaller: true}) {
		t.Fatalf("got %+v, %v", rel, err)
	}
	// A release without the version-free name (v0.5.0 had none): the versioned file.
	rel, err = parseNodeRelease(ghRelease("v0.5.0", dropAsset("AIsAtHome-node-setup.exe")))
	if err != nil || rel.VersionFreeInstaller || rel.InstallerBytes != 391_000_000 {
		t.Fatalf("versioned only: %+v, %v", rel, err)
	}
	rel, err = parseNodeRelease(ghRelease("v0.8.0", dropAsset("AIsAtHome-node-setup-v0.8.0.exe")))
	if err != nil || !rel.VersionFreeInstaller {
		t.Fatalf("version-free only: %+v, %v", rel, err)
	}
}

func TestParseNodeReleaseRejects(t *testing.T) {
	for _, tag := range []string{"0.8.0", "v0.8", "v0.8.0.1", "v0.8.0-rc1", "v08.0.0", "V0.8.0", "v0.8.0 ", " v0.8.0",
		"v0.8.0\n", "refs/tags/v0.8.0", "v0.8.0<script>", ""} {
		if _, err := parseNodeRelease(ghRelease(tag, nil)); err == nil || !strings.Contains(err.Error(), "not a release tag") {
			t.Errorf("tag %q: %v", tag, err)
		}
	}
	setAll := func(k string, v any) func(m map[string]any) { return func(m map[string]any) { m[k] = v } }
	for _, c := range []struct {
		edit func(m map[string]any)
		want string
	}{
		{setAll("draft", true), "draft or a pre-release"},
		{setAll("prerelease", true), "draft or a pre-release"},
		{func(m map[string]any) { delete(m, "draft") }, "draft or a pre-release"},
		{func(m map[string]any) { delete(m, "prerelease") }, "draft or a pre-release"},
		{setAll("tag_name", 8), "not a release"},
		{dropAsset("aisathome-node-bundle-linux-amd64.tar.xz"), "has no aisathome-node-bundle"},
		{func(m map[string]any) {
			dropAsset("AIsAtHome-node-setup.exe")(m)
			dropAsset("AIsAtHome-node-setup-v0.8.0.exe")(m)
		}, "has neither"},
		{editAsset("AIsAtHome-node-setup-v0.8.0.exe", func(a *relAsset) bool { a.Size = 1; return true }), "differ in size"},
		{editAsset("AIsAtHome-node-setup.exe", func(a *relAsset) bool { a.State = "starter"; return true }), "not fully uploaded"},
		{editAsset("AIsAtHome-node-setup-v0.8.0.exe", func(a *relAsset) bool { a.State = "starter"; return true }), "not fully uploaded"},
		{editAsset("aisathome-node-bundle-linux-amd64.tar.xz", func(a *relAsset) bool { a.Size = 0; return true }), "not fully uploaded"},
	} {
		if _, err := parseNodeRelease(ghRelease("v0.8.0", c.edit)); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("want %q, got %v", c.want, err)
		}
	}
	if _, err := parseNodeRelease([]byte("{not json")); err == nil {
		t.Error("not JSON accepted")
	}
}

// fakeGitHub answers as the handler set last; it counts the requests it answered and remembers the last
// If-None-Match. A request takes the handler set when it arrives, under the same lock as set, so a test that
// sets a new handler knows which requests get it.
type fakeGitHub struct {
	*httptest.Server
	mu     sync.Mutex
	answer func(w http.ResponseWriter, r *http.Request)
	inm    string
	served atomic.Int32 // requests answered (counted once the handler returned)
}

func newFakeGitHub(t *testing.T) *fakeGitHub {
	f := &fakeGitHub{}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer f.served.Add(1)
		f.mu.Lock()
		f.inm = r.Header.Get("If-None-Match")
		answer := f.answer
		f.mu.Unlock()
		if r.Header.Get("User-Agent") == "" || r.Header.Get("Authorization") != "" {
			http.Error(w, "want a User-Agent and no token", http.StatusBadRequest)
			return
		}
		answer(w, r)
	}))
	t.Cleanup(f.Close)
	return f
}

// lastINM is the If-None-Match of the last request.
func (f *fakeGitHub) lastINM() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.inm
}

func (f *fakeGitHub) set(answer func(w http.ResponseWriter, r *http.Request)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.answer = answer
}

// release answers body with an ETag, and 304 to a request that names that ETag.
func (f *fakeGitHub) release(body []byte, etag string) {
	f.set(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("If-None-Match") == etag {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("ETag", etag)
		w.Write(body)
	})
}

func (f *fakeGitHub) status(code int, header map[string]string) {
	f.set(func(w http.ResponseWriter, _ *http.Request) {
		for k, v := range header {
			w.Header().Set(k, v)
		}
		w.WriteHeader(code)
	})
}

func newTestWatch(t *testing.T, url string) (*nodeReleaseWatch, *time.Time) {
	w, err := newNodeReleaseWatch(url, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)
	w.now = func() time.Time { return now }
	return w, &now
}

func TestNodeReleaseWatchKeepsTheLastGoodAndBacksOff(t *testing.T) {
	gh := newFakeGitHub(t)
	w, now := newTestWatch(t, gh.URL)
	ctx := context.Background()
	if _, ok := w.current(); ok {
		t.Fatal("a release before the first read")
	}

	// Before the first good read, failures back off too and nothing is served.
	gh.status(http.StatusInternalServerError, nil)
	if d := w.poll(ctx); d != time.Minute {
		t.Fatalf("first failure: retry in %v", d)
	}
	if _, ok := w.current(); ok {
		t.Fatal("a release after a failed first read")
	}

	gh.release(ghRelease("v0.8.0", nil), `"e1"`)
	if d := w.poll(ctx); d != nodeReleaseEvery {
		t.Fatalf("good read: next in %v", d)
	}
	rel, ok := w.current()
	if !ok || rel.Version != "v0.8.0" || rel.InstallerBytes != 391_000_000 || !rel.FetchedAt.Equal(*now) {
		t.Fatalf("after a good read: %+v %v", rel, ok)
	}

	// Unchanged: GitHub answers 304 to the ETag; the release stays, its time moves on.
	*now = now.Add(10 * time.Minute)
	if d := w.poll(ctx); d != nodeReleaseEvery || gh.lastINM() != `"e1"` {
		t.Fatalf("304: next in %v, If-None-Match %q", d, gh.lastINM())
	}
	if rel, _ := w.current(); rel.Version != "v0.8.0" || !rel.FetchedAt.Equal(*now) {
		t.Fatalf("after 304: %+v", rel)
	}

	// Failures: the last good release stays, and the retries double up to an hour.
	gh.status(http.StatusBadGateway, nil)
	var waits []time.Duration
	for range 9 {
		waits = append(waits, w.poll(ctx))
	}
	want := []time.Duration{1, 2, 4, 8, 16, 32, 60, 60, 60}
	for i, d := range waits {
		if d != want[i]*time.Minute {
			t.Fatalf("retries %v, want %v minutes", waits, want)
		}
	}
	if rel, ok := w.current(); !ok || rel.Version != "v0.8.0" {
		t.Fatalf("lost the last good release: %+v %v", rel, ok)
	}
	// A release that is not usable (a pre-release made latest by hand) is a failure as well.
	gh.release(ghRelease("v0.9.0", func(m map[string]any) { m["prerelease"] = true }), `"e2"`)
	if d := w.poll(ctx); d != time.Hour {
		t.Fatalf("unusable release: retry in %v", d)
	}
	if rel, _ := w.current(); rel.Version != "v0.8.0" {
		t.Fatalf("an unusable release replaced the good one: %+v", rel)
	}

	// A new release: taken, and the backoff starts over.
	gh.release(ghRelease("v0.9.0", nil), `"e3"`)
	if d := w.poll(ctx); d != nodeReleaseEvery {
		t.Fatalf("recovered: next in %v", d)
	}
	if rel, _ := w.current(); rel.Version != "v0.9.0" {
		t.Fatalf("new release not taken: %+v", rel)
	}
	gh.status(http.StatusInternalServerError, nil)
	if d := w.poll(ctx); d != time.Minute {
		t.Fatalf("backoff did not start over: %v", d)
	}
}

func TestNodeReleaseWatchFollowsGitHubsHints(t *testing.T) {
	gh := newFakeGitHub(t)
	w, now := newTestWatch(t, gh.URL)
	ctx := context.Background()
	// Rate limited: no requests left until the reset, 30 minutes away.
	gh.status(http.StatusForbidden, map[string]string{"X-Ratelimit-Remaining": "0",
		"X-Ratelimit-Reset": strconv.FormatInt(now.Add(30*time.Minute).Unix(), 10)})
	if d := w.poll(ctx); d != 30*time.Minute {
		t.Fatalf("rate limit: retry in %v", d)
	}
	gh.status(http.StatusTooManyRequests, map[string]string{"Retry-After": "300"})
	if d := w.poll(ctx); d != 5*time.Minute {
		t.Fatalf("Retry-After: retry in %v", d)
	}
	// What GitHub asks for is honoured beyond our own backoff's hour (3 hours here), up to a day.
	gh.status(http.StatusTooManyRequests, map[string]string{"Retry-After": "10800"})
	if d := w.poll(ctx); d != 3*time.Hour {
		t.Fatalf("Retry-After of 3 hours: retry in %v", d)
	}
	gh.status(http.StatusForbidden, map[string]string{"X-Ratelimit-Remaining": "0",
		"X-Ratelimit-Reset": strconv.FormatInt(now.Add(2*time.Hour).Unix(), 10)})
	if d := w.poll(ctx); d != 2*time.Hour {
		t.Fatalf("rate limit reset in 2 hours: retry in %v", d)
	}
	gh.status(http.StatusTooManyRequests, map[string]string{"Retry-After": "604800"}) // a week: a wrong header
	if d := w.poll(ctx); d != 24*time.Hour {
		t.Fatalf("Retry-After of a week: retry in %v", d)
	}
	// A 304 with nothing held (no If-None-Match was sent) is a failure, not a crash.
	gh.status(http.StatusNotModified, nil)
	if d := w.poll(ctx); d != 32*time.Minute { // the sixth failure in a row
		t.Fatalf("304 without a release: %v", d)
	}
	if _, ok := w.current(); ok || gh.lastINM() != "" {
		t.Fatalf("304 without a release: served one, or sent If-None-Match %q", gh.lastINM())
	}
	// An answer larger than the limit is refused.
	big := ghRelease("v0.8.0", func(m map[string]any) { m["body"] = strings.Repeat("x", nodeReleaseMaxBody) })
	gh.release(big, `"big"`)
	if w.poll(ctx); func() bool { _, ok := w.current(); return ok }() {
		t.Fatal("an answer over the size limit was taken")
	}
}

func TestNodeReleaseURL(t *testing.T) {
	for _, u := range []string{"api.github.com/repos/x/y/releases/latest", "ftp://example.org/x", "https://", ":"} {
		if _, err := New(Config{NodeReleaseURL: u}, slog.New(slog.NewTextHandler(io.Discard, nil))); err == nil {
			t.Errorf("node_release_url %q accepted", u)
		}
	}
}

func TestNodeReleaseEndpoint(t *testing.T) {
	gh := newFakeGitHub(t)
	gh.status(http.StatusInternalServerError, nil)
	g := newTestGateway(t, Config{NodeReleaseURL: gh.URL})
	defer g.Close()
	h := g.Handler()
	get := func() *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", "/v1/node/release", nil)) // no API key
		return w
	}
	// The first read (at start) failed: 503, not cached. (Answered; the gateway takes its next read a minute later.)
	waitFor(t, func() bool { return gh.served.Load() >= 1 })
	if w := get(); w.Code != http.StatusServiceUnavailable || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("before a release: %d %q", w.Code, w.Header().Get("Cache-Control"))
	}
	// A good read (the test reads at once instead of waiting for the retry).
	gh.release(ghRelease("v0.8.0", nil), `"e1"`)
	g.nrel.poll(context.Background())
	w := get()
	if w.Code != http.StatusOK || w.Header().Get("Cache-Control") != "public, max-age=300" {
		t.Fatalf("with a release: %d %q %s", w.Code, w.Header().Get("Cache-Control"), w.Body)
	}
	var got map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got) != 5 || got["version"] != "v0.8.0" || got["installerBytes"] != 391e6 || got["bundleBytes"] != 480e6 ||
		got["versionFreeInstaller"] != true {
		t.Fatalf("answer %s", w.Body)
	}
	if _, err := time.Parse(time.RFC3339, got["fetched_at"].(string)); err != nil {
		t.Fatalf("fetched_at: %v", err)
	}

	// Off (no node_release_url): 503 and no reads.
	off := newTestGateway(t, Config{})
	defer off.Close()
	w = httptest.NewRecorder()
	off.Handler().ServeHTTP(w, httptest.NewRequest("GET", "/v1/node/release", nil))
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("off: %d", w.Code)
	}
}

func TestNodeReleaseWatchStopsOnClose(t *testing.T) {
	gh := newFakeGitHub(t)
	gh.release(ghRelease("v0.8.0", nil), `"e1"`)
	g := newTestGateway(t, Config{NodeReleaseURL: gh.URL})
	waitFor(t, func() bool { _, ok := g.nrel.current(); return ok })
	done := make(chan struct{})
	go func() { g.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not stop the release watch")
	}
}

// A read in flight when the gateway stops is cancelled: Close does not wait for GitHub's answer.
func TestNodeReleaseCloseCancelsARead(t *testing.T) {
	arrived := make(chan struct{}, 1)
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		arrived <- struct{}{}
		<-r.Context().Done() // never answers
	}))
	defer gh.Close()
	g := newTestGateway(t, Config{NodeReleaseURL: gh.URL})
	<-arrived
	done := make(chan struct{})
	go func() { g.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second): // the client's own timeout is 30 s
		t.Fatal("Close waited for the read in flight")
	}
}

func waitFor(t *testing.T, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatal("timed out")
		}
		time.Sleep(5 * time.Millisecond)
	}
}
