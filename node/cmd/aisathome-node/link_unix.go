//go:build !windows

// SPDX-License-Identifier: Apache-2.0
package main

import (
	"os"
	"syscall"
)

// keepOwner gives path the owner and group of old (a file it replaces). Without the right to (not root),
// a file of one's own keeps one's own.
func keepOwner(path string, old os.FileInfo) error {
	st, ok := old.Sys().(*syscall.Stat_t)
	if !ok {
		return nil
	}
	if err := os.Chown(path, int(st.Uid), int(st.Gid)); err != nil && os.Geteuid() == 0 {
		return err
	}
	return nil
}

// lockDir takes an exclusive lock on a folder until unlock is called (closing the folder releases it).
func lockDir(dir string) (unlock func(), err error) {
	f, err := os.Open(dir)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		return nil, err
	}
	return func() { f.Close() }, nil
}
