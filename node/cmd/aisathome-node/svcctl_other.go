//go:build !windows

// SPDX-License-Identifier: Apache-2.0
package main

import (
	"errors"
	"io"
)

var errNotWindows = errors.New("only on Windows; on Linux, install.sh sets up the systemd units")

func serviceInstall(installOptions, io.Writer) error { return errNotWindows }
func serviceUninstall(io.Writer) error               { return errNotWindows }
func serviceStop(io.Writer) error                    { return errNotWindows }
