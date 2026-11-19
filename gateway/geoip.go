// A GeoIP country table (item 12 tier 2, chatrules.go): which country a node connects from, by its
// address. The format is DB-IP's "IP to Country Lite" CSV, one range per line:
//
//	first address,last address,ISO 3166-1 alpha-2 code
//
// IPv4 and IPv6 ranges in any order; "ZZ" (unknown) and malformed lines are skipped. A file ending
// in .gz is read compressed. The Lite database is CC BY 4.0 (https://db-ip.com/db/lite.php): pages
// that show its results credit "IP Geolocation by DB-IP" with a link (the account page does). It is
// approximate: a VPN or a proxy hides the real country, which is why a node's country must also
// match the one its operator states.
//
// SPDX-License-Identifier: Apache-2.0
package gateway

import (
	"bufio"
	"compress/gzip"
	"encoding/binary"
	"fmt"
	"io"
	"net/netip"
	"os"
	"sort"
	"strings"
)

// geoRange is one range of addresses in one country, as two 128-bit numbers (IPv4 mapped into
// IPv6, ::ffff:a.b.c.d, so one sorted list holds both).
type geoRange struct {
	firstHi, firstLo, lastHi, lastLo uint64
	cc                               [2]byte
}

// geoTable is a loaded country table: ranges sorted by their first address, not overlapping, with
// neighbours of the same country merged. DB-IP's November 2026 file (733,000 lines) takes about 0.3 s
// and 30 MB.
type geoTable struct {
	ranges []geoRange
}

func addr128(a netip.Addr) (hi, lo uint64) {
	b := a.As16()
	return binary.BigEndian.Uint64(b[:8]), binary.BigEndian.Uint64(b[8:])
}

func less128(aHi, aLo, bHi, bLo uint64) bool { return aHi < bHi || (aHi == bHi && aLo < bLo) }

// loadGeoTable reads a country table file (see the package comment above for the format).
func loadGeoTable(path string) (*geoTable, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var r io.Reader = f
	if strings.HasSuffix(path, ".gz") {
		z, err := gzip.NewReader(f)
		if err != nil {
			return nil, fmt.Errorf("geoip %s: %w", path, err)
		}
		defer z.Close()
		r = z
	}
	t, err := parseGeoTable(r)
	if err != nil {
		return nil, fmt.Errorf("geoip %s: %w", path, err)
	}
	return t, nil
}

func parseGeoTable(r io.Reader) (*geoTable, error) {
	var out []geoRange
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		parts := strings.Split(strings.TrimSpace(sc.Text()), ",")
		if len(parts) < 3 {
			continue
		}
		cc := strings.ToUpper(strings.Trim(parts[len(parts)-1], `" `))
		first, err1 := netip.ParseAddr(strings.Trim(parts[0], `" `))
		last, err2 := netip.ParseAddr(strings.Trim(parts[1], `" `))
		if err1 != nil || err2 != nil || len(cc) != 2 || cc == "ZZ" || cc[0] < 'A' || cc[0] > 'Z' || cc[1] < 'A' || cc[1] > 'Z' ||
			first.Is4() != last.Is4() {
			continue
		}
		first, last = netip.AddrFrom16(first.As16()), netip.AddrFrom16(last.As16())
		if last.Less(first) {
			continue
		}
		g := geoRange{cc: [2]byte{cc[0], cc[1]}}
		g.firstHi, g.firstLo = addr128(first)
		g.lastHi, g.lastLo = addr128(last)
		out = append(out, g)
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no ranges (expected lines \"first,last,CC\")")
	}
	sort.Slice(out, func(i, j int) bool { return less128(out[i].firstHi, out[i].firstLo, out[j].firstHi, out[j].firstLo) })
	merged := out[:1]
	for _, g := range out[1:] {
		p := &merged[len(merged)-1]
		if !less128(p.lastHi, p.lastLo, g.firstHi, g.firstLo) {
			// Overlapping ranges: the data is wrong somewhere, and a country it cannot tell apart is
			// no country. Cut the overlap out of both (the later range keeps what lies beyond).
			if less128(p.lastHi, p.lastLo, g.lastHi, g.lastLo) {
				g.firstHi, g.firstLo = inc128(p.lastHi, p.lastLo)
			} else {
				continue
			}
			if g.cc != p.cc {
				p.cc = [2]byte{} // unknown
			}
		}
		nextHi, nextLo := inc128(p.lastHi, p.lastLo)
		if g.cc == p.cc && nextHi == g.firstHi && nextLo == g.firstLo {
			p.lastHi, p.lastLo = g.lastHi, g.lastLo
			continue
		}
		merged = append(merged, g)
	}
	return &geoTable{ranges: append([]geoRange(nil), merged...)}, nil
}

func inc128(hi, lo uint64) (uint64, uint64) {
	if lo == ^uint64(0) {
		return hi + 1, 0
	}
	return hi, lo + 1
}

// country returns the ISO code of the country addr is in, or "" if the table does not know it.
// addr is an address or, as clientIP gives IPv6 clients, a prefix ("2001:db8::/64"); a prefix
// counts only if the whole of it lies in one range.
func (t *geoTable) country(addr string) string {
	if t == nil || addr == "" {
		return ""
	}
	var first, last netip.Addr
	if p, err := netip.ParsePrefix(addr); err == nil {
		p = p.Masked()
		first, last = p.Addr(), prefixLast(p)
	} else if a, err := netip.ParseAddr(addr); err == nil {
		first, last = a, a
	} else {
		return ""
	}
	fHi, fLo := addr128(first)
	lHi, lLo := addr128(last)
	i := sort.Search(len(t.ranges), func(i int) bool {
		g := t.ranges[i]
		return less128(fHi, fLo, g.firstHi, g.firstLo) // first range starting after addr
	}) - 1
	if i < 0 {
		return ""
	}
	g := t.ranges[i]
	if less128(g.lastHi, g.lastLo, lHi, lLo) || g.cc == [2]byte{} {
		return ""
	}
	return string(g.cc[:])
}

// prefixLast is the last address of p, in the 16-byte form.
func prefixLast(p netip.Prefix) netip.Addr {
	b := p.Addr().As16()
	bits := p.Bits()
	if p.Addr().Is4() {
		bits += 96
	}
	for i := bits; i < 128; i++ {
		b[i/8] |= 1 << (7 - i%8)
	}
	return netip.AddrFrom16(b)
}
