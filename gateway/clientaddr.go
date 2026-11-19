// Client addresses behind a reverse proxy. The gateway believes a request's X-Forwarded-For only when
// the request comes from loopback or from one of Config.TrustedProxies (Caddy, which appends the
// address it saw); from any other peer the header is ignored, since that peer could name any address.
// The address counts for the per-address node limit, email sign-in's limits and connectors' allow_from.
//
// SPDX-License-Identifier: Apache-2.0
package gateway

import (
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"strings"
)

// parsePrefixes parses addresses and prefixes ("203.0.113.7", "10.89.0.0/24").
func parsePrefixes(list []string) ([]netip.Prefix, error) {
	var out []netip.Prefix
	for _, a := range list {
		p, err := netip.ParsePrefix(a)
		if err != nil {
			addr, aerr := netip.ParseAddr(a)
			if aerr != nil {
				return nil, fmt.Errorf("%q is not an address or prefix", a)
			}
			p = netip.PrefixFrom(addr.Unmap(), addr.Unmap().BitLen())
		}
		out = append(out, p.Masked())
	}
	return out, nil
}

// trustedPeer reports whether addr is a proxy whose X-Forwarded-For the gateway believes.
func (g *Gateway) trustedPeer(addr netip.Addr) bool {
	if addr.IsLoopback() {
		return true
	}
	for _, p := range g.proxies {
		if p.Contains(addr) {
			return true
		}
	}
	return false
}

// requestAddr is the client address of r: the connecting address, or, when that is a trusted proxy,
// the rightmost X-Forwarded-For entry, which the proxy appended.
func (g *Gateway) requestAddr(r *http.Request) (netip.Addr, bool) {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	addr, err := netip.ParseAddr(host)
	if err != nil {
		return netip.Addr{}, false
	}
	addr = addr.Unmap()
	xff := r.Header.Values("X-Forwarded-For")
	if len(xff) == 0 {
		return addr, true
	}
	if !g.trustedPeer(addr) {
		if addr.IsPrivate() && g.log != nil && g.untrustedXFF.CompareAndSwap(false, true) {
			// Once: a proxy on a private network that trusted_proxies does not list makes every client
			// look like the proxy (one address for all of them).
			g.log.Warn("X-Forwarded-For from a peer not in trusted_proxies ignored; list the reverse proxy there",
				"peer", addr.String())
		}
		return addr, true
	}
	hops := strings.Split(xff[len(xff)-1], ",")
	if a, err := netip.ParseAddr(strings.TrimSpace(hops[len(hops)-1])); err == nil {
		addr = a.Unmap()
	}
	return addr, true
}

// clientIP is the address limits count against (requestAddr); IPv6 clients count per /64.
func (g *Gateway) clientIP(r *http.Request) string {
	addr, ok := g.requestAddr(r)
	if !ok {
		return r.RemoteAddr
	}
	if addr.Is6() {
		p, _ := addr.WithZone("").Prefix(64)
		return p.String()
	}
	return addr.String()
}
