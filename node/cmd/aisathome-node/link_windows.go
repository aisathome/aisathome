// SPDX-License-Identifier: Apache-2.0
package main

import "os"

// keepOwner does nothing on Windows: a new file in the agent's data folder inherits the folder's
// permissions (administrators and the service account only).
func keepOwner(string, os.FileInfo) error { return nil }
