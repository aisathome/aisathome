// SPDX-License-Identifier: Apache-2.0
package main

import (
	"os"
	"path/filepath"
	"sync"
)

// logFile is an append-only log that never grows past max bytes: a write that would pass it
// first moves the file to <path>.1 (replacing the older one), so the log takes at most about
// twice max on disk. The agent logs no request content, only connections, health and errors.
type logFile struct {
	path string
	max  int64

	mu   sync.Mutex
	f    *os.File
	size int64
}

// openLog opens (or creates) the log file and its folder.
func openLog(path string, max int64) (*logFile, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return nil, err
	}
	l := &logFile{path: path, max: max}
	if err := l.open(); err != nil {
		return nil, err
	}
	return l, nil
}

func (l *logFile) open() error {
	f, err := os.OpenFile(l.path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o640)
	if err != nil {
		return err
	}
	fi, err := f.Stat()
	if err != nil {
		f.Close()
		return err
	}
	l.f, l.size = f, fi.Size()
	return nil
}

func (l *logFile) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.size > 0 && l.size+int64(len(p)) > l.max {
		// Windows cannot rename an open file: close, move, reopen. If the move fails, keep
		// appending rather than lose the line.
		l.f.Close()
		os.Rename(l.path, l.path+".1")
		if err := l.open(); err != nil {
			return 0, err
		}
	}
	n, err := l.f.Write(p)
	l.size += int64(n)
	return n, err
}

// Close closes the file.
func (l *logFile) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.f.Close()
}
