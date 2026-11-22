// A node without an account (registered by the installer, `aisathome-node register`): its local page shows
// what the gateway knows of it (protocol.SelfPath): whether the model check has admitted it, or it is
// held. The agent asks with its own token; the page never sees the token.
//
// SPDX-License-Identifier: Apache-2.0
package node

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/aisathome/aisathome/protocol"
)

// selfEvery is how often the agent of a node without an account asks the gateway about itself.
const selfEvery = time.Minute

// selfURL is the gateway's protocol.SelfPath on the HTTPS address of a ws(s):// connect URL.
func selfURL(gatewayURL string) (string, error) {
	u, err := url.Parse(gatewayURL)
	if err != nil || u.Host == "" {
		return "", fmt.Errorf("gateway address %q", gatewayURL)
	}
	switch u.Scheme {
	case "wss", "https":
		u.Scheme = "https"
	case "ws", "http":
		u.Scheme = "http"
	default:
		return "", fmt.Errorf("gateway address %q", gatewayURL)
	}
	return (&url.URL{Scheme: u.Scheme, Host: u.Host, Path: protocol.SelfPath}).String(), nil
}

// fetchSelf asks the gateway about this node once.
func (a *Agent) fetchSelf(ctx context.Context) (protocol.NodeSelf, error) {
	var s protocol.NodeSelf
	u, err := selfURL(a.cfg.GatewayURL)
	if err != nil {
		return s, err
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return s, err
	}
	req.Header.Set("Authorization", "Bearer "+a.cfg.Token)
	req.Header.Set("User-Agent", AgentVersion())
	resp, err := (&http.Client{Timeout: 20 * time.Second}).Do(req)
	if err != nil {
		return s, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 16<<10))
	if resp.StatusCode != http.StatusOK {
		return s, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return s, json.Unmarshal(b, &s)
}

// watchSelf keeps the node without an account's state from the gateway fresh for its local page, until
// ctx ends. A node with an account's token does nothing here.
func (a *Agent) watchSelf(ctx context.Context) {
	if !Anonymous(a.cfg.Token) {
		return
	}
	t := time.NewTimer(5 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		s, err := a.fetchSelf(ctx)
		a.mu.Lock()
		if err == nil {
			s.Message = cleanText(s.Message, 300)
			a.self = &s
		}
		connected := a.connected
		a.mu.Unlock()
		if err != nil {
			a.log.Debug("asking the gateway about this node failed", "err", err)
		}
		next := selfEvery
		if connected && (err != nil || s.State != protocol.SelfAdmitted) {
			next = 15 * time.Second // while it is being checked, the page follows sooner
		}
		t.Reset(next)
	}
}

// cleanText keeps a gateway's sentence printable and short (shown as text only).
func cleanText(s string, n int) string {
	r := []rune(s)
	out := make([]rune, 0, min(len(r), n))
	for _, c := range r {
		if c >= ' ' && c != 0x7f {
			out = append(out, c)
		}
		if len(out) == n {
			break
		}
	}
	return string(out)
}
