//go:build !linux && !windows

// SPDX-License-Identifier: Apache-2.0
package accountlink

import "runtime"

func osName() string { return runtime.GOOS }
