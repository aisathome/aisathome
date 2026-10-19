//go:build linux

// SPDX-License-Identifier: Apache-2.0
package main

import (
	"os"
	"syscall"
)

// fileID returns the file's inode and size.
func fileID(f *os.File) (uint64, int64, error) {
	var st syscall.Stat_t
	if err := syscall.Fstat(int(f.Fd()), &st); err != nil {
		return 0, 0, err
	}
	return st.Ino, st.Size, nil
}

// freeRange frees the bytes before end: they read back as zeros and their memory is released
// (FALLOC_FL_PUNCH_HOLE, supported by tmpfs). The file keeps its size, so the web server goes
// on appending at its end and no line can be lost between reading and freeing.
func freeRange(f *os.File, end int64) error {
	const keepSize, punchHole = 0x01, 0x02
	return syscall.Fallocate(int(f.Fd()), keepSize|punchHole, 0, end)
}

func lockFile(path string) (func(), error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		return nil, err
	}
	return func() { syscall.Flock(int(f.Fd()), syscall.LOCK_UN); f.Close() }, nil
}
