// SPDX-License-Identifier: Apache-2.0
package setup

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// modelServer serves data over TLS with Range support (http.ServeContent); before, when set,
// can answer a request itself (by its number, from 0) and return true.
type modelServer struct {
	*httptest.Server
	data   []byte
	before func(w http.ResponseWriter, r *http.Request, n int) bool

	mu     sync.Mutex
	ranges []string // each request's Range header
}

func newModelServer(t *testing.T, data []byte) *modelServer {
	s := &modelServer{data: data}
	s.Server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		n := len(s.ranges)
		s.ranges = append(s.ranges, r.Header.Get("Range"))
		s.mu.Unlock()
		if s.before != nil && s.before(w, r, n) {
			return
		}
		http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(s.data))
	}))
	t.Cleanup(s.Close)
	return s
}

func (s *modelServer) requests() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.ranges...)
}

func (s *modelServer) model() Model {
	sum := sha256.Sum256(s.data)
	return Model{URL: s.URL + "/resolve/abc/Qwen3.8-27B-UD-Q3_K_XL.gguf", SHA256: hex.EncodeToString(sum[:]), Size: int64(len(s.data))}
}

func (s *modelServer) opts() DownloadOptions {
	return DownloadOptions{
		Client: s.Client(), RetryDelay: time.Millisecond, Reserve: 1,
		FreeSpace: func(string) (uint64, error) { return 1 << 40, nil },
	}
}

// cut sends the full Content-Length but only the first n bytes, then drops the connection.
func (s *modelServer) cut(w http.ResponseWriter, n int) {
	w.Header().Set("Content-Length", strconv.Itoa(len(s.data)))
	w.WriteHeader(http.StatusOK)
	w.Write(s.data[:n])
	w.(http.Flusher).Flush()
	panic(http.ErrAbortHandler)
}

func testData(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i*7 + i/251)
	}
	return b
}

func readFile(t *testing.T, p string) []byte {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func noPart(t *testing.T, dest string) {
	t.Helper()
	if _, err := os.Stat(dest + ".part"); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("%s.part left behind (%v)", dest, err)
	}
}

func TestDownloadFresh(t *testing.T) {
	s := newModelServer(t, testData(5000))
	dest := filepath.Join(t.TempDir(), "models", "model.gguf")
	var last int64
	o := s.opts()
	o.Progress = func(done, total int64) {
		if done < last || total != 5000 {
			t.Errorf("progress %d/%d after %d", done, total, last)
		}
		last = done
	}
	if err := Download(context.Background(), s.model(), dest, o); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(readFile(t, dest), s.data) || last != 5000 {
		t.Errorf("file differs or progress ended at %d", last)
	}
	noPart(t, dest)
	if got := s.requests(); len(got) != 1 || got[0] != "" {
		t.Errorf("requests %q", got)
	}
	// A second run keeps the verified file without a request.
	if err := Download(context.Background(), s.model(), dest, s.opts()); err != nil || len(s.requests()) != 1 {
		t.Errorf("complete file: %v, %d requests", err, len(s.requests()))
	}
}

func TestDownloadResumesAfterCut(t *testing.T) {
	s := newModelServer(t, testData(5000))
	s.before = func(w http.ResponseWriter, r *http.Request, n int) bool {
		if n == 0 {
			s.cut(w, 1800)
		}
		return false
	}
	dest := filepath.Join(t.TempDir(), "model.gguf")
	if err := Download(context.Background(), s.model(), dest, s.opts()); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(readFile(t, dest), s.data) {
		t.Error("file differs")
	}
	if got := s.requests(); len(got) != 2 || got[1] != "bytes=1800-" {
		t.Errorf("requests %q, want a resume at 1800", got)
	}
	noPart(t, dest)
}

// A part left by an earlier run (an installer killed mid-download) is resumed and hashed whole.
func TestDownloadResumesEarlierPart(t *testing.T) {
	s := newModelServer(t, testData(5000))
	dest := filepath.Join(t.TempDir(), "model.gguf")
	os.WriteFile(dest+".part", s.data[:3210], 0o644)
	var hashed int64
	o := s.opts()
	o.HashProgress = func(done, total int64) { hashed = done }
	if err := Download(context.Background(), s.model(), dest, o); err != nil {
		t.Fatal(err)
	}
	if got := s.requests(); len(got) != 1 || got[0] != "bytes=3210-" || hashed != 3210 {
		t.Errorf("requests %q, hashed %d", got, hashed)
	}
	if !bytes.Equal(readFile(t, dest), s.data) {
		t.Error("file differs")
	}
}

// A part whose first bytes are wrong fails the whole-file hash and is deleted.
func TestDownloadCorruptPart(t *testing.T) {
	s := newModelServer(t, testData(5000))
	dest := filepath.Join(t.TempDir(), "model.gguf")
	bad := append([]byte(nil), s.data[:2000]...)
	bad[10] ^= 0xff
	os.WriteFile(dest+".part", bad, 0o644)
	err := Download(context.Background(), s.model(), dest, s.opts())
	if err == nil || !strings.Contains(err.Error(), "model checksum mismatch") {
		t.Fatalf("got %v", err)
	}
	noPart(t, dest)
	if _, err := os.Stat(dest); !errors.Is(err, os.ErrNotExist) {
		t.Error("a file that failed its hash was installed")
	}
	// The next run downloads it again, whole.
	if err := Download(context.Background(), s.model(), dest, s.opts()); err != nil || !bytes.Equal(readFile(t, dest), s.data) {
		t.Errorf("second run: %v", err)
	}
}

// A file of the right size but the wrong content is replaced.
func TestDownloadReplacesWrongFile(t *testing.T) {
	s := newModelServer(t, testData(5000))
	dest := filepath.Join(t.TempDir(), "model.gguf")
	os.WriteFile(dest, make([]byte, 5000), 0o644)
	if err := Download(context.Background(), s.model(), dest, s.opts()); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(readFile(t, dest), s.data) || len(s.requests()) != 1 {
		t.Errorf("not replaced (%d requests)", len(s.requests()))
	}
}

// A server that ignores Range sends the whole file again: the part starts over.
func TestDownloadServerIgnoresRange(t *testing.T) {
	s := newModelServer(t, testData(5000))
	s.before = func(w http.ResponseWriter, r *http.Request, n int) bool {
		w.Header().Set("Content-Length", strconv.Itoa(len(s.data)))
		w.Write(s.data)
		return true
	}
	dest := filepath.Join(t.TempDir(), "model.gguf")
	os.WriteFile(dest+".part", s.data[:1000], 0o644)
	if err := Download(context.Background(), s.model(), dest, s.opts()); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(readFile(t, dest), s.data) {
		t.Error("file differs")
	}
}

// A part longer than the model is not the model's start: it is deleted and the file fetched.
func TestDownloadOversizedPart(t *testing.T) {
	s := newModelServer(t, testData(5000))
	dest := filepath.Join(t.TempDir(), "model.gguf")
	os.WriteFile(dest+".part", testData(6000), 0o644)
	if err := Download(context.Background(), s.model(), dest, s.opts()); err != nil {
		t.Fatal(err)
	}
	if got := s.requests(); len(got) != 1 || got[0] != "" {
		t.Errorf("requests %q", got)
	}
}

func TestDownloadStallResumes(t *testing.T) {
	s := newModelServer(t, testData(5000))
	s.before = func(w http.ResponseWriter, r *http.Request, n int) bool {
		if n > 0 {
			return false
		}
		w.Header().Set("Content-Length", strconv.Itoa(len(s.data)))
		w.Write(s.data[:1000])
		w.(http.Flusher).Flush()
		<-r.Context().Done() // stalls until the client gives up
		return true
	}
	o := s.opts()
	o.StallWindow, o.MinSpeed = 200*time.Millisecond, 1000 // under 200 bytes in 200 ms
	dest := filepath.Join(t.TempDir(), "model.gguf")
	if err := Download(context.Background(), s.model(), dest, o); err != nil {
		t.Fatal(err)
	}
	if got := s.requests(); len(got) != 2 || got[1] != "bytes=1000-" {
		t.Errorf("requests %q", got)
	}
}

func TestDownloadRetriesServerErrors(t *testing.T) {
	s := newModelServer(t, testData(5000))
	s.before = func(w http.ResponseWriter, r *http.Request, n int) bool {
		if n < 2 {
			http.Error(w, "busy", http.StatusServiceUnavailable)
			return true
		}
		return false
	}
	dest := filepath.Join(t.TempDir(), "model.gguf")
	if err := Download(context.Background(), s.model(), dest, s.opts()); err != nil || len(s.requests()) != 3 {
		t.Errorf("%v after %d requests", err, len(s.requests()))
	}
}

func TestDownloadGivesUp(t *testing.T) {
	s := newModelServer(t, testData(5000))
	s.before = func(w http.ResponseWriter, r *http.Request, n int) bool {
		http.Error(w, "busy", http.StatusBadGateway)
		return true
	}
	o := s.opts()
	o.Attempts = 3
	err := Download(context.Background(), s.model(), filepath.Join(t.TempDir(), "m.gguf"), o)
	if err == nil || !strings.Contains(err.Error(), "failed 3 times (HTTP 502); run the setup again to resume") || len(s.requests()) != 3 {
		t.Errorf("%v after %d requests", err, len(s.requests()))
	}
}

func TestDownloadFinalErrors(t *testing.T) {
	s := newModelServer(t, testData(5000))
	s.before = func(w http.ResponseWriter, r *http.Request, n int) bool {
		switch {
		case strings.HasSuffix(r.URL.Path, "/missing.gguf"):
			http.NotFound(w, r)
		case strings.HasSuffix(r.URL.Path, "/plain.gguf"):
			http.Redirect(w, r, "http://example.invalid/m.gguf", http.StatusFound)
		case strings.HasSuffix(r.URL.Path, "/long.gguf"):
			w.Write(append(append([]byte(nil), s.data...), 'x')) // no Content-Length: chunked
		case strings.HasSuffix(r.URL.Path, "/size.gguf"):
			w.Header().Set("Content-Length", "4000")
			w.Write(s.data[:4000])
		default:
			return false
		}
		return true
	}
	for _, c := range []struct{ file, want string }{
		{"missing.gguf", "HTTP 404"},
		{"plain.gguf", "https only"},
		{"long.gguf", "sent more than the pinned 5000 bytes"},
		{"size.gguf", "the server has 4000 bytes, the pinned model 5000"},
	} {
		before := len(s.requests())
		m := s.model()
		m.URL = s.URL + "/" + c.file
		dest := filepath.Join(t.TempDir(), "m.gguf")
		err := Download(context.Background(), m, dest, s.opts())
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: got %v, want %q", c.file, err, c.want)
		}
		if n := len(s.requests()) - before; n != 1 {
			t.Errorf("%s: %d requests; a final error must not be retried", c.file, n)
		}
	}
}

func TestDownloadChecks(t *testing.T) {
	s := newModelServer(t, testData(5000))
	dest := filepath.Join(t.TempDir(), "model.gguf")
	m := s.model()
	m.URL = strings.Replace(m.URL, "https://", "http://", 1)
	if err := Download(context.Background(), m, dest, s.opts()); err == nil || !strings.Contains(err.Error(), "https://") {
		t.Errorf("http URL: %v", err)
	}
	m = s.model()
	m.SHA256 = "abc"
	if err := Download(context.Background(), m, dest, s.opts()); err == nil {
		t.Error("bad sha256: no error")
	}
	// Disk: the bytes still missing plus the reserve, on the nearest existing folder.
	o := s.opts()
	o.Reserve = 1000
	var asked string
	o.FreeSpace = func(dir string) (uint64, error) { asked = dir; return 5999, nil }
	deep := filepath.Join(filepath.Dir(dest), "a", "b", "model.gguf")
	err := Download(context.Background(), s.model(), deep, o)
	if err == nil || !strings.Contains(err.Error(), "not enough disk space") || asked != filepath.Dir(dest) {
		t.Errorf("disk: %v (asked %q)", err, asked)
	}
	o.FreeSpace = func(string) (uint64, error) { return 6000, nil }
	if err := Download(context.Background(), s.model(), deep, o); err != nil {
		t.Errorf("exactly enough disk: %v", err)
	}
	if len(s.requests()) != 1 {
		t.Errorf("%d requests; checks must come before the download", len(s.requests()))
	}
}

func TestDownloadCancel(t *testing.T) {
	s := newModelServer(t, testData(5000))
	ctx, cancel := context.WithCancel(context.Background())
	s.before = func(w http.ResponseWriter, r *http.Request, n int) bool {
		cancel()
		<-r.Context().Done()
		return true
	}
	dest := filepath.Join(t.TempDir(), "model.gguf")
	if err := Download(ctx, s.model(), dest, s.opts()); !errors.Is(err, context.Canceled) || len(s.requests()) != 1 {
		t.Errorf("%v after %d requests", err, len(s.requests()))
	}
}

func TestContentRange(t *testing.T) {
	for _, c := range []struct {
		in           string
		start, total int64
		ok           bool
	}{
		{"bytes 100-4999/5000", 100, 5000, true}, {"bytes 0-9/*", 0, -1, true},
		{"bytes */5000", 0, 0, false}, {"items 1-2/3", 0, 0, false}, {"", 0, 0, false},
	} {
		s, tot, ok := contentRange(c.in)
		if s != c.start || tot != c.total || ok != c.ok {
			t.Errorf("contentRange(%q) = %d, %d, %v", c.in, s, tot, ok)
		}
	}
}

func TestDiskFree(t *testing.T) {
	n, err := diskFree(t.TempDir())
	if err != nil || n == 0 {
		t.Errorf("diskFree = %d, %v", n, err)
	}
}
