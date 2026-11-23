//go:build !windows

// SPDX-License-Identifier: Apache-2.0
package nodeconf

import (
	"os"
	"path/filepath"
	"syscall"
)

// Lock takes the writers' lock in dir (LockFile; the installer's flock takes the same) and returns its
// release.
func Lock(dir string) (unlock func(), err error) {
	f, err := os.OpenFile(filepath.Join(dir, LockFile), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		return nil, err
	}
	return func() { f.Close() }, nil
}
