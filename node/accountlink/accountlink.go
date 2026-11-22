// Package accountlink links a node to an account without pasting a token: it asks the gateway for a code
// (protocol.LinkPath), the person approves it in a browser on any device, and the node polls until it
// gets its node token (protocol.LinkPollPath). `aisathome-node link`, the Linux installer and the
// Windows setup page use it.
//
// SPDX-License-Identifier: Apache-2.0
package accountlink

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/aisathome/aisathome/node"
	"github.com/aisathome/aisathome/node/setup"
	"github.com/aisathome/aisathome/protocol"
)

// Why waiting ended without a token.
var (
	ErrDenied  = errors.New("the link was denied in the browser; nothing was changed")
	ErrExpired = errors.New("the code expired (it works for 10 minutes) or was already used; start the link again")
)

// APIBase is the HTTPS address of a node's gateway: wss://host[:port]/... becomes https://host[:port]
// (ws:// becomes http://, for a local test gateway).
func APIBase(gatewayURL string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(gatewayURL))
	if err != nil || u.Host == "" || u.User != nil {
		return "", fmt.Errorf("the gateway address %q is not a URL like wss://host/v1/node/connect", gatewayURL)
	}
	switch u.Scheme {
	case "wss", "https":
		return "https://" + u.Host, nil
	case "ws", "http":
		return "http://" + u.Host, nil
	}
	return "", fmt.Errorf("the gateway address %q must start with wss://", gatewayURL)
}

// Client talks to one gateway.
type Client struct {
	Base string       // APIBase
	HTTP *http.Client // nil: one with a 20 s timeout
	// Token is a node without an account's token (node.Anonymous): sent with Start, so that linking moves
	// that node to the account (its identity without an account is deleted), and with Self. "" otherwise.
	Token string
	// Sleep waits between polls; nil: a timer that ends early with ctx.
	Sleep func(ctx context.Context, d time.Duration) error
}

func (c *Client) client() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return &http.Client{Timeout: 20 * time.Second}
}

func (c *Client) post(ctx context.Context, path string, body, out any) error {
	return c.do(ctx, http.MethodPost, path, body, false, out)
}

// do sends a request (body nil: none) and reads its JSON answer; withToken sends c.Token.
func (c *Client) do(ctx context.Context, method, path string, body any, withToken bool, out any) error {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.Base+path, rd)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if withToken && c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	req.Header.Set("User-Agent", "aisathome-node/"+node.Version)
	resp, err := c.client().Do(req)
	if err != nil {
		return fmt.Errorf("cannot reach the gateway at %s: %w", c.Base, err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode != http.StatusOK {
		var e struct {
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal(data, &e) == nil && e.Error.Message != "" {
			return fmt.Errorf("the gateway refused: %s", e.Error.Message)
		}
		if resp.StatusCode == http.StatusNotFound {
			if path == protocol.RegisterPath {
				return fmt.Errorf("the gateway at %s does not take nodes without an account (HTTP 404)", c.Base)
			}
			return fmt.Errorf("the gateway at %s cannot link nodes to accounts (HTTP 404): paste a node token instead", c.Base)
		}
		return fmt.Errorf("the gateway answered HTTP %d", resp.StatusCode)
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("unexpected answer from the gateway: %v", err)
	}
	return nil
}

// Start asks for a code. The answer's DeviceCode is the node's secret for polling; show the person
// UserCode and VerificationURIComplete.
func (c *Client) Start(ctx context.Context, info protocol.LinkInfo) (protocol.LinkStart, error) {
	var s protocol.LinkStart
	if err := c.do(ctx, http.MethodPost, protocol.LinkPath, info, true, &s); err != nil {
		return s, err
	}
	if s.DeviceCode == "" || s.UserCode == "" || !ValidURL(s.VerificationURIComplete) {
		return s, errors.New("unexpected answer from the gateway: no code or link")
	}
	if s.Interval <= 0 {
		s.Interval = 5
	}
	if s.ExpiresIn <= 0 {
		s.ExpiresIn = 600
	}
	return s, nil
}

// SetToken sets Token (setupflow's linker).
func (c *Client) SetToken(token string) { c.Token = token }

// Register makes this machine a node without an account (protocol.RegisterPath): science only, no
// credit, linked to an account later with Start. acceptTerms says the person was told that they must be
// 18 or older and that running the node accepts the terms, and went on; the gateway refuses without it.
func (c *Client) Register(ctx context.Context, acceptTerms bool) (protocol.Register, error) {
	var r protocol.Register
	if err := c.post(ctx, protocol.RegisterPath, protocol.RegisterRequest{AcceptTerms: acceptTerms, Agent: "aisathome-node/" + node.Version}, &r); err != nil {
		return r, err
	}
	// Written into token.env: only the exact form the gateway issues (no newline, space or quote).
	if id, ok := node.IssuedAnonTokenNodeID(r.Token); !ok || id != r.NodeID {
		return r, errors.New("unexpected answer from the gateway: no valid token for a node without an account")
	}
	return r, nil
}

// Self asks the gateway what it knows of the node whose token is c.Token (protocol.SelfPath).
func (c *Client) Self(ctx context.Context) (protocol.NodeSelf, error) {
	var s protocol.NodeSelf
	err := c.do(ctx, http.MethodGet, protocol.SelfPath, nil, true, &s)
	return s, err
}

// Poll asks once whether the link is approved.
func (c *Client) Poll(ctx context.Context, device string) (protocol.LinkPoll, error) {
	var p protocol.LinkPoll
	err := c.post(ctx, protocol.LinkPollPath, protocol.LinkPollRequest{DeviceCode: device}, &p)
	return p, err
}

// Wait polls until the link is approved (the answer carries the token), denied (ErrDenied) or expired
// (ErrExpired), or ctx ends. A failed poll (the network) is retried until the code expires.
func (c *Client) Wait(ctx context.Context, s protocol.LinkStart) (protocol.LinkPoll, error) {
	sleep := c.Sleep
	if sleep == nil {
		sleep = sleepCtx
	}
	interval := time.Duration(s.Interval) * time.Second
	var waited time.Duration
	limit := time.Duration(s.ExpiresIn)*time.Second + time.Minute // the gateway decides; this only bounds a lost one
	for waited < limit {
		if err := sleep(ctx, interval); err != nil {
			return protocol.LinkPoll{}, err
		}
		waited += interval
		p, err := c.Poll(ctx, s.DeviceCode)
		if err != nil {
			if ctx.Err() != nil {
				return protocol.LinkPoll{}, ctx.Err()
			}
			continue // the network: try again at the next interval
		}
		switch p.Status {
		case protocol.LinkApproved:
			// The token is written into token.env (a systemd EnvironmentFile): only the exact form the
			// gateway issues is taken, so a gateway cannot add lines or settings to it.
			id, ok := node.IssuedTokenNodeID(p.Token)
			if !ok || (p.NodeID != "" && p.NodeID != id) {
				return protocol.LinkPoll{}, errors.New("the gateway approved the link but sent no valid node token")
			}
			p.NodeID = id
			return p, nil
		case protocol.LinkDenied:
			return p, ErrDenied
		case protocol.LinkExpired:
			return p, ErrExpired
		case protocol.LinkSlowDown, protocol.LinkPending:
			if p.Interval > 0 {
				interval = time.Duration(p.Interval) * time.Second
			}
		}
	}
	return protocol.LinkPoll{}, ErrExpired
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// ValidURL reports whether u is a link the node may show and open: http(s), a host, no user name or
// password, no spaces or control characters.
func ValidURL(u string) bool {
	p, err := url.Parse(u)
	return err == nil && (p.Scheme == "https" || p.Scheme == "http") && p.Host != "" && p.User == nil &&
		!strings.ContainsFunc(u, func(r rune) bool { return r <= ' ' || r == 0x7f })
}

// LocalInfo is what the node reports about this machine: the host name as the suggested name, the
// first NVIDIA card and its memory (smi; nil = none), the operating system and the agent's version.
func LocalInfo(ctx context.Context, smi setup.SMI) protocol.LinkInfo {
	info := protocol.LinkInfo{OS: osName(), Agent: "aisathome-node/" + node.Version}
	if h, err := os.Hostname(); err == nil {
		info.Name, _, _ = strings.Cut(h, ".")
	}
	if smi != nil {
		ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		if gpus, err := setup.ReadGPUs(ctx, smi); err == nil && len(gpus) > 0 {
			g := gpus[0]
			for _, o := range gpus[1:] { // the card the node would choose: the largest
				if o.TotalMiB > g.TotalMiB {
					g = o
				}
			}
			info.GPU, info.VRAMMiB = g.Name, g.TotalMiB
		}
	}
	return info
}

// prettyNameOf reads PRETTY_NAME from an os-release file's text.
func prettyNameOf(s string) string {
	for _, line := range strings.Split(s, "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), "PRETTY_NAME="); ok {
			return strings.Trim(v, `"'`)
		}
	}
	return ""
}
