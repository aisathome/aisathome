// SPDX-License-Identifier: Apache-2.0
package engine

import (
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// processGone waits up to d for pid to end. A zombie (ended, not yet reaped by its new
// parent) counts as ended.
func processGone(pid int, d time.Duration) bool {
	deadline := time.Now().Add(d)
	for {
		b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
		if err != nil {
			return true
		}
		// "pid (comm) S ...": the state follows the last ')'.
		if s := string(b); strings.HasPrefix(strings.TrimSpace(s[strings.LastIndexByte(s, ')')+1:]), "Z") {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func killPID(pid int) { syscall.Kill(pid, syscall.SIGKILL) }
