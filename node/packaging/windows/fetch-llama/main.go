// Command fetch-llama downloads the pinned upstream llama.cpp build for the Windows node
// (setup.WindowsLlama), checks each file's size and SHA-256, and unpacks the zips into one
// folder: llama-server.exe beside its DLLs. CI runs it; the installer ships the result (the
// node itself never downloads code). It runs on any platform.
//
//	go run ./node/packaging/windows/fetch-llama -out <dir> [-smoke-model]
//
// <dir>/download holds the zips (a rerun resumes and checks them again), <dir>/llama the
// unpacked build. -smoke-model also fetches a tiny pinned GGUF to <dir>/smoke-model.gguf, for
// running llama-server on a CPU in CI.
//
// SPDX-License-Identifier: Apache-2.0
package main

import (
	"archive/zip"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"

	"github.com/aisathome/aisathome/node/setup"
)

// smokeModel is a tiny model of the same architecture family as the network's (qwen35, with a
// thinking chat template, so the reasoning-budget flags take effect) from ggml-org, llama.cpp's
// own test fixtures, pinned to a commit (Checked 2026-11-11: the Hub's LFS pointer). About 34M
// parameters; its output is noise. Licence CC BY-NC 4.0: CI only downloads it to test that
// llama-server loads and serves with our flags; it is never shipped.
var smokeModel = setup.Model{
	URL:    "https://huggingface.co/ggml-org/tinyopenjev-for-testing-gguf/resolve/953e44649ca8a71e5a8eee4676edebff2a45b2a2/tinyopenjev-for-testing-Q8_0.gguf",
	SHA256: "bb451e6faf46fea0b28dec91765a59d8392e40711ea5554f8e7d39b5e03b15a7",
	Size:   45712544,
}

func main() {
	out := flag.String("out", "", "output folder")
	withModel := flag.Bool("smoke-model", false, "also fetch the tiny test model")
	flag.Parse()
	if *out == "" || flag.NArg() > 0 {
		fmt.Fprintln(os.Stderr, "usage: fetch-llama -out <dir> [-smoke-model]")
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if err := fetch(ctx, setup.WindowsLlama, *out, *withModel, setup.DownloadOptions{}); err != nil {
		fmt.Fprintln(os.Stderr, "fetch-llama:", err)
		os.Exit(1)
	}
}

// fetch downloads and checks b's files into out/download, unpacks them into out/llama (a fresh
// folder each time) and, with withModel, fetches the smoke model.
func fetch(ctx context.Context, b setup.LlamaBuild, out string, withModel bool, o setup.DownloadOptions) error {
	dl := filepath.Join(out, "download")
	if err := os.MkdirAll(dl, 0o755); err != nil {
		return err
	}
	dest := filepath.Join(out, "llama")
	if err := os.RemoveAll(dest); err != nil {
		return err
	}
	if err := os.MkdirAll(dest, 0o755); err != nil {
		return err
	}
	for _, f := range b.Files {
		p := filepath.Join(dl, f.File())
		fmt.Printf("%s (%d bytes, sha256 %s)\n", f.URL, f.Size, f.SHA256)
		if err := setup.Download(ctx, f, p, o); err != nil {
			return fmt.Errorf("%s: %w", f.File(), err)
		}
		n, err := unzipFlat(p, dest)
		if err != nil {
			return fmt.Errorf("%s: %w", f.File(), err)
		}
		fmt.Printf("  checked; %d files unpacked\n", n)
	}
	if _, err := os.Stat(filepath.Join(dest, "llama-server.exe")); err != nil {
		return fmt.Errorf("no llama-server.exe in the build: %w", err)
	}
	if withModel {
		p := filepath.Join(out, "smoke-model.gguf")
		fmt.Printf("%s (%d bytes, sha256 %s)\n", smokeModel.URL, smokeModel.Size, smokeModel.SHA256)
		if err := setup.Download(ctx, smokeModel, p, o); err != nil {
			return fmt.Errorf("smoke model: %w", err)
		}
	}
	return nil
}

// unzipFlat unpacks the zip's files into dir. The upstream zips are flat; an entry with a
// folder, an absolute path, "..", or a name another entry already used is refused, so nothing
// lands outside dir or replaces another file of the build.
func unzipFlat(zipPath, dir string) (int, error) {
	r, err := zip.OpenReader(zipPath)
	if err != nil {
		return 0, err
	}
	defer r.Close()
	n := 0
	for _, f := range r.File {
		name := f.Name
		if name == "" || name == "." || name == ".." || strings.ContainsAny(name, `/\:`) {
			return n, fmt.Errorf("unexpected path %q in the zip", name)
		}
		if !f.Mode().IsRegular() {
			return n, fmt.Errorf("%q is not a regular file", name)
		}
		if err := extract(f, filepath.Join(dir, name)); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

func extract(f *zip.File, path string) error {
	rc, err := f.Open()
	if err != nil {
		return err
	}
	defer rc.Close()
	// O_EXCL: a second file of the same name (in this zip or the other) is an error.
	w, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if errors.Is(err, os.ErrExist) {
		return fmt.Errorf("%s: two files of that name in the build", f.Name)
	}
	if err != nil {
		return err
	}
	// The zip's own CRC-32 is checked by the reader at the end of the copy.
	if _, err := io.Copy(w, rc); err != nil {
		w.Close()
		return fmt.Errorf("%s: %w", f.Name, err)
	}
	return w.Close()
}
