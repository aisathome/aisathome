// SPDX-License-Identifier: Apache-2.0
package tray

import (
	"bytes"
	"encoding/binary"
	"flag"
	"os"
	"path/filepath"
	"testing"
)

var update = flag.Bool("update", false, "rewrite the installer's icon file")

// Each state's .ico is well-formed: the directory, one 32-bit BMP entry per size inside the
// file, the mask rows; and the icons differ from each other.
func TestIcons(t *testing.T) {
	seen := map[string]State{}
	for _, s := range States {
		b := Icon(s)
		le := binary.LittleEndian
		if le.Uint16(b[0:]) != 0 || le.Uint16(b[2:]) != 1 || int(le.Uint16(b[4:])) != len(iconSizes) {
			t.Fatalf("%s: bad header % x", s, b[:6])
		}
		for i, n := range iconSizes {
			e := b[6+16*i:]
			size, off := le.Uint32(e[8:]), le.Uint32(e[12:])
			if int(e[0]) != n || int(e[1]) != n || le.Uint16(e[4:]) != 1 || le.Uint16(e[6:]) != 32 {
				t.Fatalf("%s: entry %d: % x", s, i, e[:8])
			}
			if int(off)+int(size) > len(b) {
				t.Fatalf("%s: entry %d past the end", s, i)
			}
			bmp := b[off : off+size]
			mask := (n + 31) / 32 * 4 * n
			if le.Uint32(bmp) != 40 || int32(le.Uint32(bmp[4:])) != int32(n) || int32(le.Uint32(bmp[8:])) != int32(2*n) ||
				le.Uint16(bmp[14:]) != 32 || int(size) != 40+n*n*4+mask {
				t.Fatalf("%s %d: bad bitmap header % x", s, n, bmp[:16])
			}
			// The middle pixel is opaque, except for the ring; the corner transparent and masked.
			mid := 40 + ((n/2)*n+n/2)*4
			if a := bmp[mid+3]; (a == 0) != (s == Down) {
				t.Errorf("%s %d: middle alpha %d", s, n, a)
			}
			if a := bmp[40+3]; a != 0 || bmp[40+n*n*4]&0x80 == 0 {
				t.Errorf("%s %d: corner not transparent", s, n)
			}
		}
		if prev, ok := seen[string(b)]; ok {
			t.Errorf("%s and %s have the same icon", s, prev)
		}
		seen[string(b)] = s
	}
}

// The installer's icon (Setup, shortcuts, Apps & features) is the connected icon, committed;
// go test ./node/tray -update rewrites it.
func TestInstallerIcon(t *testing.T) {
	path := filepath.Join("..", "packaging", "windows", "aisathome.ico")
	want := Icon(Connected)
	if *update {
		if err := os.WriteFile(path, want, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("%s differs from Icon(Connected): run go test ./node/tray -run TestInstallerIcon -update", path)
	}
}
