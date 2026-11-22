// SPDX-License-Identifier: Apache-2.0
package accountlink

import (
	"fmt"

	"golang.org/x/sys/windows"
)

// osName is "Windows" with its version and build, e.g. "Windows 10.0 (build 26100)" (Windows 11 reports
// 10.0 too; the build tells them apart).
func osName() string {
	v := windows.RtlGetVersion()
	return fmt.Sprintf("Windows %d.%d (build %d)", v.MajorVersion, v.MinorVersion, v.BuildNumber)
}
