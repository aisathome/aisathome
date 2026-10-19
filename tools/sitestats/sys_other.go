//go:build !linux

// SPDX-License-Identifier: Apache-2.0
package main

import (
	"errors"
	"os"
)

// The deployment is Linux only (tmpfs and hole punching); elsewhere only report works.

func fileID(f *os.File) (uint64, int64, error) {
	st, err := f.Stat()
	if err != nil {
		return 0, 0, err
	}
	return 0, st.Size(), nil
}

func freeRange(*os.File, int64) error {
	return errors.New("freeing read log lines needs Linux")
}

func lockFile(string) (func(), error) { return func() {}, nil }
