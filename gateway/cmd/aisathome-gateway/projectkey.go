// SPDX-License-Identifier: Apache-2.0

package main

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
)

const projectKeyUsage = `usage: aisathome-gateway project-key <file>

Writes a new API key for a science project to <file> (mode 600; an existing file is never
overwritten) and prints only its SHA-256, for "key_sha256" of the project in the config.
The key itself is never printed: give the file to the project's runner.
`

// runProjectKey implements the project-key subcommand.
func runProjectKey(args []string, out io.Writer) error {
	if len(args) != 1 || args[0] == "" || args[0][0] == '-' {
		return errors.New(projectKeyUsage)
	}
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return err
	}
	// The "ak-" form is one the gateway's redaction recognises, as for other keys.
	key := "ak-" + base64.RawURLEncoding.EncodeToString(b)
	f, err := os.OpenFile(args[0], os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err = f.WriteString(key + "\n"); err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(args[0])
		return err
	}
	sum := sha256.Sum256([]byte(key))
	fmt.Fprintln(out, hex.EncodeToString(sum[:]))
	return nil
}
