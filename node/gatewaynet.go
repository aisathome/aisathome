// SPDX-License-Identifier: Apache-2.0
package node

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// ErrRedirect: a gateway answered a request that carries the node's token with a redirect. Go's HTTP client
// would send the Authorization header on to the same host on another port or scheme, or to a subdomain, so
// a redirect is never followed: the token goes only to the address its record names.
var ErrRedirect = errors.New("the gateway answered with a redirect; a node token is never sent on to another address")

// TokenClient is the HTTP client for every request that carries a node token (the WebSocket connection,
// /v1/node/self, linking and registering): it never follows a redirect.
func TokenClient(timeout time.Duration) *http.Client {
	return &http.Client{Timeout: timeout, CheckRedirect: func(*http.Request, []*http.Request) error { return ErrRedirect }}
}

// CheckGatewayURL refuses a gateway address a node token must not go to: anything but wss:// or https://,
// except ws:// or http:// to this machine (loopback: development and tests), where nothing crosses a network.
func CheckGatewayURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return fmt.Errorf("the gateway address %q is not a URL", raw)
	}
	switch strings.ToLower(u.Scheme) {
	case "wss", "https":
		return nil
	case "ws", "http":
		if loopback(u.Hostname()) {
			return nil
		}
		return fmt.Errorf("the gateway address %q is not encrypted: use wss:// (ws:// only to this machine)", raw)
	}
	return fmt.Errorf("the gateway address %q must start with wss://", raw)
}

func loopback(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
