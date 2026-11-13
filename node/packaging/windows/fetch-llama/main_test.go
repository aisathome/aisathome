// SPDX-License-Identifier: Apache-2.0
package main

import (
	"archive/zip"
	"bytes"
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aisathome/aisathome/node/setup"
)

func makeZip(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var b bytes.Buffer
	w := zip.NewWriter(&b)
	for name, body := range files {
		f, err := w.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		f.Write([]byte(body))
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// release serves the given files over https and returns a build pinning them.
func release(t *testing.T, files map[string][]byte) (setup.LlamaBuild, setup.DownloadOptions) {
	t.Helper()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, ok := files[strings.TrimPrefix(r.URL.Path, "/")]
		if !ok {
			http.NotFound(w, r)
			return
		}
		http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(body))
	}))
	t.Cleanup(srv.Close)
	var b setup.LlamaBuild
	for _, name := range []string{"llama.zip", "cudart.zip"} {
		body, ok := files[name]
		if !ok {
			continue
		}
		sum := sha256.Sum256(body)
		b.Files = append(b.Files, setup.Model{URL: srv.URL + "/" + name, SHA256: hex.EncodeToString(sum[:]), Size: int64(len(body))})
	}
	return b, setup.DownloadOptions{Client: srv.Client(), Attempts: 1, RetryDelay: time.Millisecond}
}

// Both zips are checked and unpacked into one folder.
func TestFetch(t *testing.T) {
	b, o := release(t, map[string][]byte{
		"llama.zip":  makeZip(t, map[string]string{"llama-server.exe": "MZ server", "ggml-cuda.dll": "MZ cuda"}),
		"cudart.zip": makeZip(t, map[string]string{"cudart64_12.dll": "MZ cudart"}),
	})
	out := t.TempDir()
	if err := fetch(context.Background(), b, out, false, o); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]string{"llama-server.exe": "MZ server", "ggml-cuda.dll": "MZ cuda", "cudart64_12.dll": "MZ cudart"} {
		if got, err := os.ReadFile(filepath.Join(out, "llama", name)); err != nil || string(got) != want {
			t.Errorf("%s: %q %v", name, got, err)
		}
	}
	// A rerun starts from a fresh folder and checks the downloads again.
	os.WriteFile(filepath.Join(out, "llama", "stale.dll"), nil, 0o644)
	if err := fetch(context.Background(), b, out, false, o); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(out, "llama", "stale.dll")); err == nil {
		t.Error("a file from an earlier run survived")
	}
}

// A file that is not the pinned one is refused before anything is unpacked.
func TestFetchRefusesWrongFile(t *testing.T) {
	b, o := release(t, map[string][]byte{"llama.zip": makeZip(t, map[string]string{"llama-server.exe": "MZ"})})
	b.Files[0].SHA256 = strings.Repeat("0", 64)
	out := t.TempDir()
	if err := fetch(context.Background(), b, out, false, o); err == nil || !strings.Contains(err.Error(), "llama.zip") {
		t.Fatalf("a wrong file was accepted: %v", err)
	}
	if entries, _ := os.ReadDir(filepath.Join(out, "llama")); len(entries) != 0 {
		t.Fatalf("unpacked %d files of a wrong download", len(entries))
	}
}

// Entries that would leave the folder, sit in a folder, or replace another file are refused;
// so is a build without llama-server.exe.
func TestFetchRefusesBadZips(t *testing.T) {
	for name, files := range map[string]map[string][]byte{
		"parent":          {"llama.zip": makeZip(t, map[string]string{"../evil.dll": "x", "llama-server.exe": "MZ"})},
		"folder":          {"llama.zip": makeZip(t, map[string]string{"sub/evil.dll": "x", "llama-server.exe": "MZ"})},
		"backslash":       {"llama.zip": makeZip(t, map[string]string{`..\evil.dll`: "x", "llama-server.exe": "MZ"})},
		"drive":           {"llama.zip": makeZip(t, map[string]string{`C:evil.dll`: "x", "llama-server.exe": "MZ"})},
		"duplicate":       {"llama.zip": makeZip(t, map[string]string{"llama-server.exe": "MZ"}), "cudart.zip": makeZip(t, map[string]string{"llama-server.exe": "evil"})},
		"no llama-server": {"llama.zip": makeZip(t, map[string]string{"llama.exe": "MZ"})},
	} {
		b, o := release(t, files)
		out := t.TempDir()
		err := fetch(context.Background(), b, out, false, o)
		if err == nil {
			t.Errorf("%s: accepted", name)
		} else if want := map[string]string{"duplicate": "two files", "no llama-server": "no llama-server.exe"}[name]; !strings.Contains(err.Error(), cmp.Or(want, "unexpected path")) {
			t.Errorf("%s: %v", name, err)
		}
		if _, err := os.Stat(filepath.Join(out, "evil.dll")); err == nil {
			t.Errorf("%s: wrote outside the build folder", name)
		}
	}
}

// The pinned files: the build and the CUDA runtime, and the smoke model.
func TestPins(t *testing.T) {
	var names []string
	for _, f := range setup.WindowsLlama.Files {
		names = append(names, f.File())
	}
	if strings.Join(names, " ") != "llama-b12323-bin-win-cuda-12.4-x64.zip cudart-llama-bin-win-cuda-12.4-x64.zip" {
		t.Errorf("files %v", names)
	}
	if smokeModel.File() != "tinyopenjev-for-testing-Q8_0.gguf" || len(smokeModel.SHA256) != 64 || smokeModel.Size <= 0 {
		t.Errorf("smoke model %+v", smokeModel)
	}
}
