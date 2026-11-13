// SPDX-License-Identifier: Apache-2.0
package setup

import (
	"strconv"
	"strings"
)

// memTotalMiB is MemTotal from /proc/meminfo's text in MiB (the file gives kB), or 0.
func memTotalMiB(meminfo string) int {
	for _, l := range strings.Split(meminfo, "\n") {
		f := strings.Fields(l)
		if len(f) >= 2 && f[0] == "MemTotal:" {
			if kb, err := strconv.Atoi(f[1]); err == nil && kb > 0 {
				return kb / 1024
			}
		}
	}
	return 0
}
