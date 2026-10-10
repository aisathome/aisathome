//go:build !unix

// SPDX-License-Identifier: Apache-2.0
package gateway

import "sync"

var fileLocks sync.Mutex

// lockFile serialises changes within this process only (the gateway runs on Linux).
func lockFile(string) (func(), error) {
	fileLocks.Lock()
	return fileLocks.Unlock, nil
}
