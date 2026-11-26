// Package release tells the node agent which node release is the latest, for the local page's "a new
// release is out" notice. Source is where the answer comes from: Gateway asks the gateway the node
// already connects to (GET /v1/node/release), so the node contacts no other host for it.
// Cache asks a Source at most once per TTL and never makes its caller wait.
//
// Nothing here installs anything: the page only tells the volunteer, who updates as usual (the one-line
// install on Linux, the new installer on Windows).
//
// SPDX-License-Identifier: Apache-2.0
package release

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Release is the latest node release.
type Release struct {
	Version string `json:"version"` // a stable tag, "v1.2.3"
	URL     string `json:"url"`     // the release's page on GitHub (notes, files): a link for the page
	// BundleBytes is the size of the Linux bundle; 0 when unknown.
	BundleBytes int64 `json:"bundle_bytes,omitempty"`
}

// Source answers which release is the latest.
type Source interface {
	Latest(ctx context.Context) (Release, error)
}

// ReleasesURL is the repository's releases; a release's page is ReleasesURL + "/tag/" + its tag.
const ReleasesURL = "https://github.com/aisathome/aisathome/releases"

// Path is the gateway's endpoint for the latest node release (no authentication).
const Path = "/v1/node/release"

// ErrUnknown: the gateway does not know the latest release yet (503: before its first good read, or the
// endpoint is off), or no gateway is configured. The page then shows no notice.
var ErrUnknown = errors.New("the latest node release is not known")

// Gateway asks the node's gateway for the latest release: GET <its https base>/v1/node/release. The
// request carries no token and follows no redirect.
type Gateway struct {
	// GatewayURL returns the node's configured gateway URL (wss://host/v1/node/connect), read at each ask:
	// a Windows node's setup can change it while the agent runs.
	GatewayURL func() string
	Client     *http.Client // nil = a client with a 20 s timeout that follows no redirect
	UserAgent  string       // "" = "aisathome-node"
}

const maxBody = 64 << 10 // the answer is about 150 bytes

// stableTag is a stable release tag, vMAJOR.MINOR.PATCH: what releases/latest/download serves.
var stableTag = regexp.MustCompile(`^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$`)

// EndpointURL is the release endpoint of the gateway a node connects to at gatewayURL: wss://host[:port]/...
// becomes https://host[:port]/v1/node/release; ws:// (a gateway on this machine only, as the node allows)
// becomes http://.
func EndpointURL(gatewayURL string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(gatewayURL))
	if err != nil || u.Host == "" || u.User != nil {
		return "", fmt.Errorf("not a gateway URL: %q", gatewayURL)
	}
	switch u.Scheme {
	case "wss":
		return "https://" + u.Host + Path, nil
	case "ws":
		if ip := net.ParseIP(u.Hostname()); u.Hostname() == "localhost" || (ip != nil && ip.IsLoopback()) {
			return "http://" + u.Host + Path, nil
		}
	}
	return "", fmt.Errorf("not a wss:// gateway URL: %q", gatewayURL)
}

// Latest asks the gateway.
func (g Gateway) Latest(ctx context.Context) (Release, error) {
	gw := ""
	if g.GatewayURL != nil {
		gw = g.GatewayURL()
	}
	if gw == "" {
		return Release{}, ErrUnknown
	}
	endpoint, err := EndpointURL(gw)
	if err != nil {
		return Release{}, err
	}
	c, ua := g.Client, g.UserAgent
	if c == nil {
		c = &http.Client{Timeout: 20 * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	}
	if ua == "" {
		ua = "aisathome-node"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return Release{}, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", ua)
	resp, err := c.Do(req)
	if err != nil {
		return Release{}, err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusServiceUnavailable:
		return Release{}, ErrUnknown
	default:
		return Release{}, fmt.Errorf("%s: HTTP %d", endpoint, resp.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
	if err != nil {
		return Release{}, err
	}
	if len(b) > maxBody {
		return Release{}, fmt.Errorf("%s: answer larger than %d bytes", endpoint, maxBody)
	}
	return ParseGateway(b)
}

// ParseGateway reads the gateway's answer, {"version", "installerBytes", "bundleBytes",
// "versionFreeInstaller", "fetched_at"}, and checks the version is a stable release tag.
func ParseGateway(body []byte) (Release, error) {
	var r struct {
		Version     string `json:"version"`
		BundleBytes int64  `json:"bundleBytes"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		return Release{}, fmt.Errorf("not a node release: %w", err)
	}
	if !stableTag.MatchString(r.Version) {
		return Release{}, fmt.Errorf("version %q is not a release tag like v1.2.3", r.Version)
	}
	rel := Release{Version: r.Version, URL: ReleasesURL + "/tag/" + r.Version}
	if r.BundleBytes > 0 {
		rel.BundleBytes = r.BundleBytes
	}
	return rel, nil
}

// version is a parsed "v1.2.3" or "1.2.3-rc.1" (build metadata after "+" ignored).
type version struct {
	core [3]uint64
	pre  []string // nil for a release
}

func parse(s string) (version, bool) {
	s = strings.TrimPrefix(strings.TrimSpace(s), "v")
	s, _, _ = strings.Cut(s, "+")
	core, pre, hasPre := strings.Cut(s, "-")
	parts := strings.Split(core, ".")
	if len(parts) != 3 {
		return version{}, false
	}
	var v version
	for i, p := range parts {
		n, err := strconv.ParseUint(p, 10, 64)
		if err != nil || p == "" || (len(p) > 1 && p[0] == '0') {
			return version{}, false
		}
		v.core[i] = n
	}
	if hasPre {
		if pre == "" {
			return version{}, false
		}
		v.pre = strings.Split(pre, ".")
		for _, id := range v.pre {
			if id == "" {
				return version{}, false
			}
		}
	}
	return v, true
}

// Compare compares two versions by semantic versioning's precedence: -1, 0 or 1, and false when
// either is not a version (such as "dev").
func Compare(a, b string) (int, bool) {
	va, ok := parse(a)
	if !ok {
		return 0, false
	}
	vb, ok := parse(b)
	if !ok {
		return 0, false
	}
	for i := range va.core {
		if va.core[i] != vb.core[i] {
			if va.core[i] < vb.core[i] {
				return -1, true
			}
			return 1, true
		}
	}
	// A pre-release comes before its release.
	switch {
	case va.pre == nil && vb.pre == nil:
		return 0, true
	case va.pre == nil:
		return 1, true
	case vb.pre == nil:
		return -1, true
	}
	for i := 0; i < len(va.pre) && i < len(vb.pre); i++ {
		if c := comparePre(va.pre[i], vb.pre[i]); c != 0 {
			return c, true
		}
	}
	switch {
	case len(va.pre) < len(vb.pre):
		return -1, true
	case len(va.pre) > len(vb.pre):
		return 1, true
	}
	return 0, true
}

// comparePre compares two pre-release identifiers: numbers numerically and before words, words as text.
func comparePre(a, b string) int {
	na, ea := strconv.ParseUint(a, 10, 64)
	nb, eb := strconv.ParseUint(b, 10, 64)
	switch {
	case ea == nil && eb == nil:
		switch {
		case na < nb:
			return -1
		case na > nb:
			return 1
		}
		return 0
	case ea == nil:
		return -1
	case eb == nil:
		return 1
	}
	return strings.Compare(a, b)
}

// Newer reports whether latest is a newer version than current; false when either is not a version
// (a development build is never told to update).
func Newer(latest, current string) bool {
	c, ok := Compare(latest, current)
	return ok && c > 0
}

// Cache asks its Source at most once per TTL, and after a failure again after Retry. Known never
// waits for the Source: it answers with what it knows and starts a read in the background when that is
// stale, so the first call after a start answers "not known yet".
type Cache struct {
	src   Source
	ttl   time.Duration
	retry time.Duration
	now   func() time.Time

	mu      sync.Mutex
	rel     Release
	ok      bool      // rel is a release that was read
	next    time.Time // no read before this
	reading bool
	lastErr error
}

// Default intervals: every hour (the gateway's answer is cached for 5 minutes and costs it nothing), 15
// minutes after a failure or a 503.
const (
	DefaultTTL   = time.Hour
	DefaultRetry = 15 * time.Minute
	readTimeout  = 30 * time.Second
)

// NewCache returns a Cache for src; ttl and retry 0 take the defaults.
func NewCache(src Source, ttl, retry time.Duration) *Cache {
	if ttl <= 0 {
		ttl = DefaultTTL
	}
	if retry <= 0 {
		retry = DefaultRetry
	}
	return &Cache{src: src, ttl: ttl, retry: retry, now: time.Now}
}

// Known returns the latest release as last read, and whether one was read; it starts a read in the
// background when the last one is older than the TTL (or failed more than Retry ago).
func (c *Cache) Known() (Release, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.reading && !c.now().Before(c.next) {
		c.reading = true
		go c.read()
	}
	return c.rel, c.ok
}

// Refresh reads the Source now and waits for it (tests; a caller that may wait).
func (c *Cache) Refresh(ctx context.Context) (Release, error) {
	rel, err := c.src.Latest(ctx)
	c.store(rel, err)
	return rel, err
}

func (c *Cache) read() {
	ctx, cancel := context.WithTimeout(context.Background(), readTimeout)
	defer cancel()
	rel, err := c.src.Latest(ctx)
	c.store(rel, err)
}

func (c *Cache) store(rel Release, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.reading = false
	if err != nil {
		c.lastErr = err
		c.next = c.now().Add(c.retry) // the last good release stays
		return
	}
	c.rel, c.ok, c.lastErr = rel, true, nil
	c.next = c.now().Add(c.ttl)
}

// Err is the last read's error, nil after a good read.
func (c *Cache) Err() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.lastErr
}
