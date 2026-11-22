// SPDX-License-Identifier: Apache-2.0
package accountlink

import "os"

// osName is the distribution's PRETTY_NAME from os-release, else "Linux".
func osName() string {
	for _, p := range []string{"/etc/os-release", "/usr/lib/os-release"} {
		b, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		if n := prettyNameOf(string(b)); n != "" {
			return n
		}
	}
	return "Linux"
}
