// Key-store updates and live reloading of API keys and node credentials.
//
// SPDX-License-Identifier: Apache-2.0
package gateway

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/coder/websocket"
)

// keyStoreBox lets the key store be swapped atomically (Reload, SetKeyStore).
type keyStoreBox struct{ KeyStore }

func staticKeys(cfg Config) StaticKeys {
	keys := StaticKeys{}
	for k, u := range cfg.APIKeys {
		keys[k] = KeyInfo{User: u}
	}
	for k, info := range cfg.Keys {
		keys[k] = info
	}
	return keys
}

func copyTokens(in map[string]NodeToken) map[string]NodeToken {
	out := make(map[string]NodeToken, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

// SetKeyStore replaces the key store (e.g. with an operator's accounts service).
func (g *Gateway) SetKeyStore(ks KeyStore) { g.keys.Store(&keyStoreBox{ks}) }

// keysFileCheck is how soon an added or revoked key takes effect.
const keysFileCheck = 2 * time.Second

// loadKeys builds the built-in key store from cfg and its keys file. It changes nothing if
// the keys file cannot be read, or if a custom key store was set.
func (g *Gateway) loadKeys(cfg Config) error {
	g.keysMu.Lock()
	defer g.keysMu.Unlock()
	return g.loadKeysLocked(cfg)
}

func (g *Gateway) loadKeysLocked(cfg Config) error {
	var hashed map[string]KeyInfo
	var sum [32]byte
	if cfg.KeysFile != "" {
		raw, err := os.ReadFile(cfg.KeysFile)
		if err != nil && !errors.Is(err, os.ErrNotExist) { // missing: no keys yet
			return fmt.Errorf("keys file: %w", err)
		}
		sum = sha256.Sum256(raw)
		f, err := parseKeysFile(raw)
		if err != nil {
			return err
		}
		hashed = make(map[string]KeyInfo, len(f.Keys))
		for _, r := range f.Keys {
			hashed[r.Hash] = r.KeyInfo
		}
	}
	if _, builtin := g.keys.Load().KeyStore.(*configKeys); builtin {
		g.keys.Store(&keyStoreBox{&configKeys{plain: staticKeys(cfg), hashed: hashed}})
	}
	g.keysCfg, g.keysSum = cfg, sum
	return nil
}

// checkKeysFile applies the keys file when its content changed. A broken file keeps the
// keys in use (and is reported once), rather than locking every user out.
func (g *Gateway) checkKeysFile() {
	g.keysMu.Lock()
	defer g.keysMu.Unlock()
	if g.keysCfg.KeysFile == "" {
		return
	}
	raw, err := os.ReadFile(g.keysCfg.KeysFile)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return // e.g. permissions; Reload reports it
	}
	sum := sha256.Sum256(raw)
	if sum == g.keysSum {
		return
	}
	if err := g.loadKeysLocked(g.keysCfg); err != nil {
		g.keysSum = sum // report this version once
		g.log.Error("keys file not applied, keeping the keys in use", "err", err)
		return
	}
	g.log.Info("keys file reloaded")
}

// Reload applies new API keys and node tokens without dropping connections. Nodes whose
// token was revoked, or is now bound to another node id, are disconnected. A custom key
// store set with SetKeyStore is kept; only the built-in keys are replaced. If the keys file
// cannot be read, nothing changes.
func (g *Gateway) Reload(cfg Config) error {
	if err := g.loadKeys(cfg); err != nil {
		return err
	}
	g.mu.Lock()
	g.tokens = copyTokens(cfg.NodeTokens)
	var revoked []*nodeConn
	for _, n := range g.node {
		if n.account != "" { // an account's node: its token is in the accounts file (sweepNodes)
			continue
		}
		if t, ok := g.tokens[n.token]; !ok || (t.NodeID != "" && t.NodeID != n.id) || (t.Trust == "trusted") != n.trusted {
			revoked = append(revoked, n)
			continue
		}
		for tok, other := range g.tokens { // the id is now bound to another token
			if tok != n.token && other.NodeID == n.id {
				revoked = append(revoked, n)
				break
			}
		}
	}
	g.mu.Unlock()
	for _, n := range revoked {
		g.log.Info("node token revoked, disconnecting", "node", n.id)
		n.conn.Close(websocket.StatusPolicyViolation, "node token revoked")
	}
	return nil
}
