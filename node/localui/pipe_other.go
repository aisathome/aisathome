//go:build !windows

// SPDX-License-Identifier: Apache-2.0
package localui

import (
	"context"
	"errors"
	"time"
)

// TrayPipe is the agent's pipe for the tray; there is no tray outside Windows.
const TrayPipe = ""

var errNoPipe = errors.New("the tray pipe exists only on Windows")

func (s *Server) servePipe(ctx context.Context, name string) error { return errNoPipe }

// AskLoginLink asks the agent's pipe for a one-time sign-in link (Windows only).
func AskLoginLink(name string, port int, timeout time.Duration) (string, error) {
	return "", errNoPipe
}
