// SPDX-License-Identifier: Apache-2.0

package gateway

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"
)

// A keys file holds per-user API keys that the operator manages with
// `aisathome-gateway keys add|list|revoke`. The gateway rereads it when it changes, so keys
// are added and revoked without a restart or SIGHUP. Only a SHA-256 hash of each key is
// stored: keys are 256 random bits, so a fast hash is enough (no password hashing needed),
// and a leaked file cannot be used to call the API.

// KeyRecord is one API key in a keys file.
type KeyRecord struct {
	Hash    string    `json:"sha256"`
	Prefix  string    `json:"prefix"` // the key's first characters, to tell keys apart
	Created time.Time `json:"created"`
	KeyInfo
}

// KeysFile is the keys file's format.
type KeysFile struct {
	Keys []KeyRecord `json:"keys"`
}

// keyPrefixLen: "ak-" plus 8 characters (48 bits) is enough to name a key, far too little
// to guess it.
const keyPrefixLen = 11

// LoadKeysFile reads a keys file. A missing file has no keys.
func LoadKeysFile(path string) (KeysFile, error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return KeysFile{}, nil
	}
	if err != nil {
		return KeysFile{}, err
	}
	return parseKeysFile(raw)
}

func parseKeysFile(raw []byte) (KeysFile, error) {
	var f KeysFile
	if len(strings.TrimSpace(string(raw))) == 0 {
		return f, nil
	}
	if err := json.Unmarshal(raw, &f); err != nil {
		return f, fmt.Errorf("keys file: %w", err)
	}
	for _, r := range f.Keys {
		if len(r.Hash) != 64 || r.User == "" {
			return f, fmt.Errorf("keys file: entry %q needs a 64-character sha256 and a user", r.Prefix)
		}
	}
	return f, nil
}

// Save writes the keys file atomically, so the gateway never reads half a file.
func (f KeysFile) Save(path string) error {
	raw, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(path, append(raw, '\n'))
}

// Add creates a key for info and returns it. The key itself is not stored; show it once.
// The "ak-" form is one the gateway's redaction recognises, so a key pasted into a prompt
// never reaches a node.
func (f *KeysFile) Add(info KeyInfo, now time.Time) (string, error) {
	if info.User == "" {
		return "", errors.New("a key needs a user")
	}
	for {
		b := make([]byte, 32)
		if _, err := rand.Read(b); err != nil {
			return "", err
		}
		key := "ak-" + base64.RawURLEncoding.EncodeToString(b)
		if f.find(key[:keyPrefixLen]) >= 0 { // prefixes name keys; keep them unique
			continue
		}
		f.Keys = append(f.Keys, KeyRecord{Hash: keyHash(key), Prefix: key[:keyPrefixLen],
			Created: now.UTC().Truncate(time.Second), KeyInfo: info})
		return key, nil
	}
}

// Revoke removes the key named by its prefix (or given in full) and returns its record.
func (f *KeysFile) Revoke(prefixOrKey string) (KeyRecord, error) {
	i := f.find(prefixOrKey)
	if i < 0 {
		return KeyRecord{}, fmt.Errorf("no key with prefix %q", prefixOrKey)
	}
	r := f.Keys[i]
	f.Keys = append(f.Keys[:i], f.Keys[i+1:]...)
	return r, nil
}

func (f *KeysFile) find(prefixOrKey string) int {
	h := keyHash(prefixOrKey)
	for i, r := range f.Keys {
		if r.Prefix == prefixOrKey || r.Hash == h {
			return i
		}
	}
	return -1
}

// configKeys is the built-in key store: keys from the config (in clear, as before) and from
// the keys file (by hash).
type configKeys struct {
	plain  StaticKeys
	hashed map[string]KeyInfo // sha256 hex of the key → info
}

// Lookup implements KeyStore.
func (c *configKeys) Lookup(_ context.Context, key string) (KeyInfo, bool) {
	if k, ok := c.plain[key]; ok {
		return k, true
	}
	k, ok := c.hashed[keyHash(key)]
	return k, ok
}
