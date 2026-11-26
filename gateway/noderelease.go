// The latest node release, for the website's download block: GET /v1/node/release, without
// authentication. The gateway fills it from GitHub's public API (Config.NodeReleaseURL, the
// repository's releases/latest) every 10 minutes, without a token, and keeps serving the last
// good release while a fetch fails, retrying sooner and then backing off.
//
// SPDX-License-Identifier: Apache-2.0
package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"sync"
	"time"
)

const (
	// nodeReleaseEvery: how often the release is read; GitHub allows 60 requests an hour without a
	// token per address, and a 304 (unchanged, If-None-Match) does not count against it.
	nodeReleaseEvery = 10 * time.Minute
	// After a failed read the next try comes after nodeReleaseRetry, doubling with each failure in a
	// row up to nodeReleaseRetryMax, or later when GitHub says when (Retry-After, the rate limit's
	// reset; that is honoured up to nodeReleaseHintMax).
	nodeReleaseRetry    = time.Minute
	nodeReleaseRetryMax = time.Hour
	nodeReleaseHintMax  = 24 * time.Hour
	nodeReleaseTimeout  = 30 * time.Second
	nodeReleaseMaxBody  = 4 << 20 // a release with long notes is tens of kB

	// The release's files the download block names (.github/workflows/release.yml attaches them).
	nodeInstaller = "AIsAtHome-node-setup.exe" // the same bytes as AIsAtHome-node-setup-<tag>.exe
	nodeBundle    = "aisathome-node-bundle-linux-amd64.tar.xz"
)

// releaseTagRE: a stable release tag, vMAJOR.MINOR.PATCH without leading zeros. Pre-releases are not
// what releases/latest/download serves.
var releaseTagRE = regexp.MustCompile(`^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$`)

// NodeRelease is the answer of GET /v1/node/release. The field names are the website's
// (site/src/data/node-release.ts), whose values are the build-time fallback.
type NodeRelease struct {
	Version              string `json:"version"`
	InstallerBytes       int64  `json:"installerBytes"`
	BundleBytes          int64  `json:"bundleBytes"`
	VersionFreeInstaller bool   `json:"versionFreeInstaller"`
	// FetchedAt: the last time GitHub confirmed it (a new read or an unchanged answer).
	FetchedAt time.Time `json:"fetched_at"`
}

// parseNodeRelease reads a GitHub release object (GET /repos/{owner}/{repo}/releases/latest) and
// checks it is one the download block can offer: a stable tag, published (not a draft or a
// pre-release), the installer and the bundle fully uploaded.
func parseNodeRelease(body []byte) (NodeRelease, error) {
	var r struct {
		TagName    string `json:"tag_name"`
		Draft      *bool  `json:"draft"`
		Prerelease *bool  `json:"prerelease"`
		Assets     []struct {
			Name  string `json:"name"`
			Size  int64  `json:"size"`
			State string `json:"state"`
		} `json:"assets"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		return NodeRelease{}, fmt.Errorf("not a release: %w", err)
	}
	if !releaseTagRE.MatchString(r.TagName) {
		return NodeRelease{}, fmt.Errorf("tag %q is not a release tag like v1.2.3", r.TagName)
	}
	if r.Draft == nil || *r.Draft || r.Prerelease == nil || *r.Prerelease {
		return NodeRelease{}, fmt.Errorf("%s is a draft or a pre-release", r.TagName)
	}
	sizes := map[string]int64{}
	for _, a := range r.Assets {
		if a.State != "uploaded" || a.Size <= 0 {
			sizes[a.Name] = -1 // there, but not usable
			continue
		}
		sizes[a.Name] = a.Size
	}
	size := func(name string) (int64, bool, error) {
		s, ok := sizes[name]
		if ok && s < 0 {
			return 0, true, fmt.Errorf("%s: %s is not fully uploaded", r.TagName, name)
		}
		return s, ok, nil
	}
	rel := NodeRelease{Version: r.TagName}
	versioned := "AIsAtHome-node-setup-" + r.TagName + ".exe"
	free, hasFree, err := size(nodeInstaller)
	if err != nil {
		return NodeRelease{}, err
	}
	vsize, hasVersioned, err := size(versioned)
	if err != nil {
		return NodeRelease{}, err
	}
	switch {
	case hasFree && hasVersioned && free != vsize:
		return NodeRelease{}, fmt.Errorf("%s: %s and %s differ in size", r.TagName, nodeInstaller, versioned)
	case hasFree:
		rel.InstallerBytes, rel.VersionFreeInstaller = free, true
	case hasVersioned:
		rel.InstallerBytes = vsize
	default:
		return NodeRelease{}, fmt.Errorf("%s has neither %s nor %s", r.TagName, nodeInstaller, versioned)
	}
	bundle, hasBundle, err := size(nodeBundle)
	if err != nil {
		return NodeRelease{}, err
	}
	if !hasBundle {
		return NodeRelease{}, fmt.Errorf("%s has no %s", r.TagName, nodeBundle)
	}
	rel.BundleBytes = bundle
	return rel, nil
}

// nodeReleaseWatch keeps the latest node release.
type nodeReleaseWatch struct {
	url    string
	client *http.Client
	log    *slog.Logger
	now    func() time.Time

	mu    sync.Mutex
	last  *NodeRelease // the last good release; nil until the first
	etag  string       // of the answer last holds, for If-None-Match
	fails int          // failed reads in a row
}

func newNodeReleaseWatch(rawURL string, log *slog.Logger) (*nodeReleaseWatch, error) {
	u, err := url.Parse(rawURL)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return nil, fmt.Errorf("node_release_url %q: want an http(s) URL", rawURL)
	}
	return &nodeReleaseWatch{url: rawURL, client: &http.Client{Timeout: nodeReleaseTimeout}, log: log, now: time.Now}, nil
}

// current is the last good release; ok is false before the first (and for a nil watch: off).
func (w *nodeReleaseWatch) current() (NodeRelease, bool) {
	if w == nil {
		return NodeRelease{}, false
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.last == nil {
		return NodeRelease{}, false
	}
	return *w.last, true
}

// errNotModified: GitHub answered 304, the release held is still the latest.
var errNotModified = errors.New("not modified")

// fetch reads the release once. wait is GitHub's own hint of when to come back after a refusal.
func (w *nodeReleaseWatch) fetch(ctx context.Context, etag string) (rel NodeRelease, newETag string, wait time.Duration, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, w.url, nil)
	if err != nil {
		return NodeRelease{}, "", 0, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "aisathome-gateway")
	if etag != "" {
		req.Header.Set("If-None-Match", etag)
	}
	resp, err := w.client.Do(req)
	if err != nil {
		return NodeRelease{}, "", 0, err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusNotModified:
		return NodeRelease{}, etag, 0, errNotModified
	default:
		return NodeRelease{}, "", retryHint(resp.Header, w.now()), fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, nodeReleaseMaxBody+1))
	if err != nil {
		return NodeRelease{}, "", 0, err
	}
	if len(body) > nodeReleaseMaxBody {
		return NodeRelease{}, "", 0, fmt.Errorf("answer larger than %d bytes", nodeReleaseMaxBody)
	}
	rel, err = parseNodeRelease(body)
	return rel, resp.Header.Get("ETag"), 0, err
}

// retryHint is when GitHub asks to be asked again: Retry-After (seconds), or the rate limit's
// reset when none are left. 0 when it does not say.
func retryHint(h http.Header, now time.Time) time.Duration {
	if s, err := strconv.Atoi(h.Get("Retry-After")); err == nil && s > 0 {
		return time.Duration(s) * time.Second
	}
	if h.Get("X-Ratelimit-Remaining") == "0" {
		if reset, err := strconv.ParseInt(h.Get("X-Ratelimit-Reset"), 10, 64); err == nil {
			if d := time.Unix(reset, 0).Sub(now); d > 0 {
				return d
			}
		}
	}
	return 0
}

// poll reads the release once, keeps it when it is good, and returns how long to wait before the
// next read: nodeReleaseEvery after a good read, a growing delay after failed ones. A failed read
// keeps the last good release.
func (w *nodeReleaseWatch) poll(ctx context.Context) time.Duration {
	w.mu.Lock()
	etag := ""
	if w.last != nil {
		etag = w.etag
	}
	w.mu.Unlock()
	rel, newETag, hint, err := w.fetch(ctx, etag)
	now := w.now().UTC()
	w.mu.Lock()
	defer w.mu.Unlock()
	switch {
	case errors.Is(err, errNotModified) && w.last != nil:
		w.last.FetchedAt = now
	case err == nil:
		if w.last == nil || w.last.Version != rel.Version {
			w.log.Info("node release", "version", rel.Version, "installer_bytes", rel.InstallerBytes, "bundle_bytes", rel.BundleBytes)
		}
		rel.FetchedAt = now
		w.last, w.etag = &rel, newETag
	default:
		if ctx.Err() != nil {
			return 0 // shutting down
		}
		w.fails++
		// Our own backoff stops at an hour; when GitHub says how long to wait, never ask sooner (at most a
		// day, in case a header is wrong).
		d := min(nodeReleaseRetry<<min(w.fails-1, 10), nodeReleaseRetryMax)
		d = max(d, min(hint, nodeReleaseHintMax))
		kept := "none yet"
		if w.last != nil {
			kept = w.last.Version
		}
		w.log.Warn("reading the latest node release failed; keeping the last good one", "err", err,
			"kept", kept, "failures", w.fails, "retry_in", d.String())
		return d
	}
	if w.fails > 0 {
		w.log.Info("reading the latest node release works again", "after_failures", w.fails)
	}
	w.fails = 0
	return nodeReleaseEvery
}

// run reads the release at once, then as poll says, until stop is closed.
func (w *nodeReleaseWatch) run(stop <-chan struct{}) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		select {
		case <-stop:
			cancel()
		case <-ctx.Done():
		}
	}()
	for ctx.Err() == nil { // cancelled once stop is closed
		t := time.NewTimer(w.poll(ctx))
		select {
		case <-stop:
			t.Stop()
			return
		case <-t.C:
		}
	}
}

// handleNodeRelease serves GET /v1/node/release without authentication: the last good release, or
// 503 before the first (or with node_release_url unset), when the site keeps its own copy.
func (g *Gateway) handleNodeRelease(w http.ResponseWriter, _ *http.Request) {
	rel, ok := g.nrel.current()
	if !ok {
		w.Header().Set("Cache-Control", "no-store")
		apiError(w, http.StatusServiceUnavailable, "the latest node release is not known yet")
		return
	}
	w.Header().Set("Cache-Control", "public, max-age=300")
	writeJSON(w, http.StatusOK, rel)
}
