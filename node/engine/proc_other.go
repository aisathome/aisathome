//go:build !linux && !windows

// SPDX-License-Identifier: Apache-2.0
package engine

import (
	"errors"
	"time"
)

type sysProc struct{}

// The node runs on Linux and Windows only.
func (p *proc) startSys() error    { return errors.ErrUnsupported }
func (p *proc) stop(time.Duration) {}
func (p *proc) releaseSys()        {}
