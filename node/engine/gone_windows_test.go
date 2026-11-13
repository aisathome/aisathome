// SPDX-License-Identifier: Apache-2.0
package engine

import (
	"time"

	"golang.org/x/sys/windows"
)

// processGone waits up to d for pid to end.
func processGone(pid int, d time.Duration) bool {
	h, err := windows.OpenProcess(windows.SYNCHRONIZE, false, uint32(pid))
	if err != nil {
		return true // no such process (any more)
	}
	defer windows.CloseHandle(h)
	ev, err := windows.WaitForSingleObject(h, uint32(d/time.Millisecond))
	return err == nil && ev == windows.WAIT_OBJECT_0
}

func killPID(pid int) {
	if h, err := windows.OpenProcess(windows.PROCESS_TERMINATE, false, uint32(pid)); err == nil {
		windows.TerminateProcess(h, 1)
		windows.CloseHandle(h)
	}
}
