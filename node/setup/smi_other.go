//go:build !windows

// SPDX-License-Identifier: Apache-2.0
package setup

// smiCandidates: elsewhere nvidia-smi is on PATH or not installed.
func smiCandidates() []string { return nil }
