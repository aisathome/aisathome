//go:build unix

// SPDX-License-Identifier: Apache-2.0
package setup

import "golang.org/x/sys/unix"

// diskFree is the space an unprivileged user may use on dir's file system, in bytes.
func diskFree(dir string) (uint64, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(dir, &st); err != nil {
		return 0, err
	}
	return uint64(st.Bavail) * uint64(st.Bsize), nil
}
