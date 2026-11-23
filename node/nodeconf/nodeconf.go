// Package nodeconf is the one way the node's config (node.json) and its token file (token.env) are read
// and written, by the agent, `aisathome-node link`/`register` and the Windows setup page (the Linux
// installer follows the same rules in shell):
//
//   - One canonical form: a node.json with two keys that are equal in any case, at any level, is refused
//     (Go's decoder matches keys in any case and keeps the last; another reader could keep another), and a
//     written one has lower-case keys only.
//   - A token is trusted only together with its recorded issuing gateway: token.env holds
//     AISATHOME_NODE_TOKEN and AISATHOME_TOKEN_GATEWAY, node.json "token" and "token_gateway". A token is
//     never sent to another gateway than its record names.
//   - Every writer takes the same lock (Lock: node.lock beside node.json) for its whole check-and-write.
//
// SPDX-License-Identifier: Apache-2.0
package nodeconf

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
)

// LockFile is the lock every writer of node.json and token.env takes, in node.json's folder (the Linux
// installer takes it too, with flock).
const LockFile = "node.lock"

// gatewayRe is what a gateway address may look like (as install.sh checks it): a token's record goes into
// a systemd EnvironmentFile, so nothing else may get there.
var gatewayRe = regexp.MustCompile(`^wss?://[A-Za-z0-9.-]+(:[0-9]+)?(/[A-Za-z0-9._~/-]*)?$`)

// ValidGateway reports whether gw is a gateway address a token's record may name.
func ValidGateway(gw string) bool { return gatewayRe.MatchString(gw) }

// CheckKeys refuses JSON with two keys that are equal in any case in one object, at any level.
func CheckKeys(b []byte) error {
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	if err := checkValue(d); err != nil {
		return err
	}
	if _, err := d.Token(); err != io.EOF {
		return errors.New("trailing data after the JSON value")
	}
	return nil
}

func checkValue(d *json.Decoder) error {
	t, err := d.Token()
	if err != nil {
		return err
	}
	switch t {
	case json.Delim('{'):
		seen := map[string]bool{}
		for d.More() {
			k, err := d.Token()
			if err != nil {
				return err
			}
			ks, _ := k.(string)
			lk := strings.ToLower(ks)
			if seen[lk] {
				return fmt.Errorf("key %q appears more than once (in any case); keep one", ks)
			}
			seen[lk] = true
			if err := checkValue(d); err != nil {
				return err
			}
		}
		_, err = d.Token()
		return err
	case json.Delim('['):
		for d.More() {
			if err := checkValue(d); err != nil {
				return err
			}
		}
		_, err = d.Token()
		return err
	}
	return nil
}

// Parse reads a node.json in its canonical form into v (CheckKeys, then encoding/json).
func Parse(b []byte, v any) error {
	if err := CheckKeys(b); err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}

// ReadObject reads node.json at path as an object with lower-case top-level keys; a missing file is an
// empty object (exists false).
func ReadObject(path string) (m map[string]json.RawMessage, exists bool, err error) {
	m = map[string]json.RawMessage{}
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return m, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	raw := map[string]json.RawMessage{}
	if err := Parse(b, &raw); err != nil {
		return nil, true, fmt.Errorf("%s: %w", path, err)
	}
	for k, v := range raw {
		m[strings.ToLower(k)] = v
	}
	return m, true, nil
}

// String is a string value of m ("" if none or not a string).
func String(m map[string]json.RawMessage, key string) string {
	var s string
	json.Unmarshal(m[key], &s)
	return s
}

// Encode is m as node.json bytes: lower-case keys (ReadObject's), indented, a final newline.
func Encode(m map[string]json.RawMessage) ([]byte, error) {
	b, err := json.MarshalIndent(m, "", "  ")
	return append(b, '\n'), err
}

// Set puts v at key in m.
func Set(m map[string]json.RawMessage, key string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	m[strings.ToLower(key)] = b
	return nil
}

// TokenEnv is a token.env: the token and the gateway that issued it, one line each. Both are checked
// (issued is the token form check of the caller's choice): nothing else can get into the file.
func TokenEnv(token, gateway string, issued func(string) bool) ([]byte, error) {
	if !issued(token) {
		return nil, errors.New("not writing token.env: the token is not a node token")
	}
	if !ValidGateway(gateway) {
		return nil, fmt.Errorf("not writing token.env: %q is not a gateway address", gateway)
	}
	return []byte("AISATHOME_NODE_TOKEN=" + token + "\nAISATHOME_TOKEN_GATEWAY=" + gateway + "\n"), nil
}

// ParseTokenEnv reads a token.env: the token and its recorded gateway ("" if none).
func ParseTokenEnv(b []byte) (token, gateway string) {
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if v, ok := strings.CutPrefix(line, "AISATHOME_NODE_TOKEN="); ok {
			token = strings.TrimSpace(v)
		}
		if v, ok := strings.CutPrefix(line, "AISATHOME_TOKEN_GATEWAY="); ok {
			gateway = strings.TrimSpace(v)
		}
	}
	return token, gateway
}

// ErrTokenGateway: a token's recorded gateway is not the one the config connects to.
var ErrTokenGateway = errors.New("the node token was issued by another gateway than the config names")

// CheckTokenGateway reports ErrTokenGateway (wrapped) when a token's recorded gateway differs from the
// gateway the node connects to. An empty record is the caller's to judge.
func CheckTokenGateway(record, gateway string) error {
	if record != "" && record != gateway {
		return fmt.Errorf("%w: issued by %s, config names %s", ErrTokenGateway, record, gateway)
	}
	return nil
}
