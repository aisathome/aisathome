// SPDX-License-Identifier: Apache-2.0
package setup

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// DownloadOptions tunes Download; the zero value is the real setting.
type DownloadOptions struct {
	Client       *http.Client  // default: a client that follows only https redirects
	Attempts     int           // tries before giving up (each resumes); default 5
	RetryDelay   time.Duration // pause between tries; default 10 s
	StallWindow  time.Duration // a try is stalled when it gets less than MinSpeed over this; default 60 s
	MinSpeed     int64         // bytes per second; default 10,000 (curl's --speed-limit in install.sh)
	Reserve      int64         // disk space kept free beyond the model; default 2 GiB
	FreeSpace    func(dir string) (uint64, error)
	Progress     func(done, total int64) // after each chunk written, and once when verifying
	HashProgress func(done, total int64) // while the SHA-256 of existing bytes is computed
}

func (o *DownloadOptions) defaults() {
	if o.Client == nil {
		o.Client = &http.Client{}
	}
	if o.Attempts <= 0 {
		o.Attempts = 5
	}
	if o.RetryDelay <= 0 {
		o.RetryDelay = 10 * time.Second
	}
	if o.StallWindow <= 0 {
		o.StallWindow = 60 * time.Second
	}
	if o.MinSpeed <= 0 {
		o.MinSpeed = 10000
	}
	if o.Reserve <= 0 {
		o.Reserve = 2 << 30
	}
	if o.FreeSpace == nil {
		o.FreeSpace = diskFree
	}
}

// httpsOnly refuses a redirect away from https (Hugging Face redirects to its CDN).
func httpsOnly(req *http.Request, via []*http.Request) error {
	if req.URL.Scheme != "https" {
		return fmt.Errorf("%w: %s", errInsecureRedirect, req.URL.Redacted())
	}
	if len(via) >= 10 {
		return errors.New("stopped after 10 redirects")
	}
	return nil
}

var reSHA256 = regexp.MustCompile(`^[0-9a-f]{64}$`)

// finalError is a download failure that a retry cannot fix (HTTP 404, a disk error).
type finalError struct{ error }

func (e finalError) Unwrap() error { return e.error }

var (
	errStalled          = errors.New("the download stalled")
	errInsecureRedirect = errors.New("redirect refused: the model is downloaded over https only")
)

// Download fetches m to dest and verifies its SHA-256 over the whole file. An interrupted
// download continues from dest+".part" (HTTP Range); the bytes already there are hashed again
// first, so a resumed file is checked as a whole. A complete dest with the right hash is kept
// without a request. On a checksum mismatch the part is deleted. Over https only.
func Download(ctx context.Context, m Model, dest string, o DownloadOptions) error {
	o.defaults()
	if !reSHA256.MatchString(m.SHA256) || m.Size <= 0 {
		return fmt.Errorf("model pin is invalid: sha256 %q, %d bytes", m.SHA256, m.Size)
	}
	u, err := url.Parse(m.URL)
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return fmt.Errorf("model URL must start with https://: %q", m.URL)
	}
	client := *o.Client
	if client.CheckRedirect == nil {
		client.CheckRedirect = httpsOnly
	}

	if fi, err := os.Stat(dest); err == nil && fi.Mode().IsRegular() && fi.Size() == m.Size {
		got, err := hashFile(ctx, dest, m.Size, o.HashProgress)
		if err != nil {
			return err
		}
		if got == m.SHA256 {
			if o.Progress != nil {
				o.Progress(m.Size, m.Size)
			}
			return nil
		}
		// Not the pinned file: downloaded again into the part, then renamed over it.
	}

	part := dest + ".part"
	var have int64
	if fi, err := os.Stat(part); err == nil {
		have = fi.Size()
	}
	if have > m.Size {
		if err := os.Remove(part); err != nil {
			return err
		}
		have = 0
	}
	if err := checkDisk(filepath.Dir(dest), m.Size-have+o.Reserve, o.FreeSpace); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(part, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	h := sha256.New()
	if have > 0 {
		if err := hashInto(ctx, h, f, have, o.HashProgress); err != nil {
			return err
		}
	}
	d := &downloader{ctx: ctx, client: &client, m: m, f: f, h: h, have: have, o: o}
	if err := d.run(); err != nil {
		return err
	}
	got := hex.EncodeToString(h.Sum(nil))
	if got != m.SHA256 {
		f.Close()
		os.Remove(part)
		return fmt.Errorf("model checksum mismatch: got %s, want %s. The download was deleted; run the setup again to download it again", got, m.SHA256)
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(part, dest)
}

type downloader struct {
	ctx    context.Context
	client *http.Client
	m      Model
	f      *os.File
	h      hash.Hash
	have   int64
	o      DownloadOptions
}

// run tries until the part holds m.Size bytes, resuming each time.
func (d *downloader) run() error {
	var last error
	for i := 0; i < d.o.Attempts && d.have < d.m.Size; i++ {
		if i > 0 {
			select {
			case <-d.ctx.Done():
				return d.ctx.Err()
			case <-time.After(d.o.RetryDelay):
			}
		}
		err := d.try()
		if err == nil {
			continue
		}
		var fe finalError
		if errors.As(err, &fe) {
			return fe.error
		}
		if d.ctx.Err() != nil {
			return d.ctx.Err()
		}
		last = err
	}
	if d.have < d.m.Size {
		return fmt.Errorf("model download failed %d times (%v); run the setup again to resume", d.o.Attempts, last)
	}
	if d.o.Progress != nil {
		d.o.Progress(d.have, d.m.Size)
	}
	return nil
}

// restart drops the bytes downloaded so far (the server did not resume where we asked).
func (d *downloader) restart() error {
	if err := d.f.Truncate(0); err != nil {
		return finalError{err}
	}
	if _, err := d.f.Seek(0, io.SeekStart); err != nil {
		return finalError{err}
	}
	d.h.Reset()
	d.have = 0
	return nil
}

// try is one request: from d.have to the end.
func (d *downloader) try() error {
	ctx, cancel := context.WithCancelCause(d.ctx)
	defer cancel(nil)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, d.m.URL, nil)
	if err != nil {
		return finalError{err}
	}
	if d.have > 0 {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", d.have))
	}
	// Stall watch: less than MinSpeed over a window cancels the try (it resumes on the next).
	var got atomic.Int64
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		t := time.NewTicker(d.o.StallWindow)
		defer t.Stop()
		prev := int64(0)
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				n := got.Load()
				if float64(n-prev) < float64(d.o.MinSpeed)*d.o.StallWindow.Seconds() {
					cancel(errStalled)
					return
				}
				prev = n
			}
		}
	}()
	resp, err := d.client.Do(req)
	if err != nil {
		if c := context.Cause(ctx); errors.Is(c, errStalled) {
			return c
		}
		if errors.Is(err, errInsecureRedirect) {
			return finalError{err}
		}
		return err
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode == http.StatusPartialContent && d.have > 0:
		start, total, ok := contentRange(resp.Header.Get("Content-Range"))
		if ok && total >= 0 && total != d.m.Size {
			return finalError{fmt.Errorf("model download: the server has %d bytes, the pinned model %d", total, d.m.Size)}
		}
		if !ok || start != d.have {
			if err := d.restart(); err != nil {
				return err
			}
			return fmt.Errorf("server resumed at the wrong offset (%q)", resp.Header.Get("Content-Range"))
		}
	case resp.StatusCode == http.StatusOK:
		if d.have > 0 { // the server ignored Range: start over
			if err := d.restart(); err != nil {
				return err
			}
		}
		if resp.ContentLength >= 0 && resp.ContentLength != d.m.Size {
			return finalError{fmt.Errorf("model download: the server has %d bytes, the pinned model %d", resp.ContentLength, d.m.Size)}
		}
	case resp.StatusCode == http.StatusRequestedRangeNotSatisfiable:
		if err := d.restart(); err != nil {
			return err
		}
		return fmt.Errorf("server could not resume (HTTP 416)")
	case resp.StatusCode >= 400 && resp.StatusCode < 500 && resp.StatusCode != http.StatusTooManyRequests && resp.StatusCode != http.StatusRequestTimeout:
		return finalError{fmt.Errorf("model download failed: %s: HTTP %d", d.m.URL, resp.StatusCode)}
	default:
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}

	buf := make([]byte, 1<<20)
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			if d.have+int64(n) > d.m.Size {
				d.restart()
				return finalError{fmt.Errorf("model download: the server sent more than the pinned %d bytes", d.m.Size)}
			}
			if _, err := d.f.Write(buf[:n]); err != nil {
				return finalError{fmt.Errorf("writing the model: %w", err)}
			}
			d.h.Write(buf[:n])
			d.have += int64(n)
			got.Add(int64(n))
			if d.o.Progress != nil {
				d.o.Progress(d.have, d.m.Size)
			}
		}
		if rerr == io.EOF {
			if d.have < d.m.Size {
				return io.ErrUnexpectedEOF
			}
			return nil
		}
		if rerr != nil {
			if c := context.Cause(ctx); errors.Is(c, errStalled) {
				return c
			}
			return rerr
		}
	}
}

// contentRange reads "bytes <start>-<end>/<total>"; total is -1 for "*".
func contentRange(s string) (start, total int64, ok bool) {
	s, found := strings.CutPrefix(strings.TrimSpace(s), "bytes ")
	if !found {
		return 0, 0, false
	}
	rng, tot, found := strings.Cut(s, "/")
	if !found {
		return 0, 0, false
	}
	a, _, found := strings.Cut(rng, "-")
	if !found {
		return 0, 0, false
	}
	start, err := strconv.ParseInt(a, 10, 64)
	if err != nil {
		return 0, 0, false
	}
	total = -1
	if tot != "*" {
		if total, err = strconv.ParseInt(tot, 10, 64); err != nil {
			return 0, 0, false
		}
	}
	return start, total, true
}

// hashFile is the SHA-256 of the file's first n bytes, hex.
func hashFile(ctx context.Context, path string, n int64, progress func(done, total int64)) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if err := hashInto(ctx, h, f, n, progress); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// hashInto feeds the first n bytes of f into h and leaves f's offset at n.
func hashInto(ctx context.Context, h hash.Hash, f *os.File, n int64, progress func(done, total int64)) error {
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return err
	}
	buf := make([]byte, 4<<20)
	var done int64
	for done < n {
		if err := ctx.Err(); err != nil {
			return err
		}
		k, err := io.ReadFull(f, buf[:min(int64(len(buf)), n-done)])
		h.Write(buf[:k])
		done += int64(k)
		if progress != nil {
			progress(done, n)
		}
		if err != nil {
			return fmt.Errorf("reading %s: %w", f.Name(), err)
		}
	}
	return nil
}

// checkDisk: dir (or its nearest existing parent) must have need bytes free.
func checkDisk(dir string, need int64, free func(string) (uint64, error)) error {
	d := dir
	for {
		if fi, err := os.Stat(d); err == nil && fi.IsDir() {
			break
		}
		p := filepath.Dir(d)
		if p == d {
			break
		}
		d = p
	}
	have, err := free(d)
	if err != nil {
		return fmt.Errorf("cannot read the free disk space of %s: %v", d, err)
	}
	if need > 0 && have < uint64(need) {
		return fmt.Errorf("not enough disk space on %s: need %.1f GB for the model download, %.1f GB free. Free some space and run the setup again (it resumes)",
			d, float64(need)/1e9, float64(have)/1e9)
	}
	return nil
}
