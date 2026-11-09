//go:build !windows

// SPDX-License-Identifier: Apache-2.0
package main

import "context"

// runningAsService: on Linux, systemd runs the agent as an ordinary process.
func runningAsService() (bool, error) { return false, nil }

// runService is never called outside Windows.
func runService(func(context.Context) int) int { return 1 }
