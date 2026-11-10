// SPDX-License-Identifier: Apache-2.0
package setup

import "golang.org/x/sys/windows"

// diskFree is the space the caller may use on dir's volume (quotas included), in bytes.
func diskFree(dir string) (uint64, error) {
	p, err := windows.UTF16PtrFromString(dir)
	if err != nil {
		return 0, err
	}
	var avail, total, free uint64
	if err := windows.GetDiskFreeSpaceEx(p, &avail, &total, &free); err != nil {
		return 0, err
	}
	return avail, nil
}
