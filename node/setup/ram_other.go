//go:build !windows

// SPDX-License-Identifier: Apache-2.0
package setup

import "os"

// SystemRAMMiB is the machine's physical memory in MiB (MemTotal in /proc/meminfo, as
// install.sh reads it), or 0 when it cannot be read.
func SystemRAMMiB() int {
	b, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0
	}
	return memTotalMiB(string(b))
}
