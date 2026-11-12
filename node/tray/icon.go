// SPDX-License-Identifier: Apache-2.0
package tray

import (
	"bytes"
	"encoding/binary"
	"image"
	"image/color"
	"math"
	"sync"
)

// iconSizes are the sizes in each .ico: the notification area's 16 and 20 to 32 pixels at the
// usual scalings, and 48 for shortcuts.
var iconSizes = []int{16, 20, 24, 32, 48}

// The colours: green running, blue on its way, violet to set up, amber paused, grey stopped
// or not running, red needs the volunteer. Each state also has its own mark, so the icons
// differ without colour too.
var iconColor = map[State]color.NRGBA{
	Serving:   {0x16, 0xa3, 0x4a, 0xff},
	Connected: {0x16, 0xa3, 0x4a, 0xff},
	Starting:  {0x25, 0x63, 0xeb, 0xff},
	Setup:     {0x7c, 0x3a, 0xed, 0xff},
	Paused:    {0xd9, 0x77, 0x06, 0xff},
	Stopped:   {0x6b, 0x72, 0x80, 0xff},
	Down:      {0x6b, 0x72, 0x80, 0xff},
	Problem:   {0xdc, 0x26, 0x26, 0xff},
}

var (
	iconMu    sync.Mutex
	iconCache = map[State][]byte{}
)

// Icon is the .ico file (32-bit images of iconSizes) for a state.
func Icon(s State) []byte {
	iconMu.Lock()
	defer iconMu.Unlock()
	if b, ok := iconCache[s]; ok {
		return b
	}
	var imgs []*image.NRGBA
	for _, n := range iconSizes {
		imgs = append(imgs, render(s, n))
	}
	b := encodeICO(imgs)
	iconCache[s] = b
	return b
}

// render draws a state's icon at n×n pixels: a disc (a ring when the agent is down) with a
// darker edge and a white mark, smoothed by 4×4 samples per pixel.
func render(s State, n int) *image.NRGBA {
	fill, ok := iconColor[s]
	if !ok {
		fill = iconColor[Down]
	}
	dark := func(c uint8) uint8 { return uint8(int(c) * 7 / 10) }
	edge := color.NRGBA{dark(fill.R), dark(fill.G), dark(fill.B), 0xff}
	white := color.NRGBA{0xff, 0xff, 0xff, 0xff}
	img := image.NewNRGBA(image.Rect(0, 0, n, n))
	const k = 4
	for y := 0; y < n; y++ {
		for x := 0; x < n; x++ {
			var r, g, b, a float64
			for sy := 0; sy < k; sy++ {
				for sx := 0; sx < k; sx++ {
					u := (float64(x) + (float64(sx)+0.5)/k) / float64(n)
					v := (float64(y) + (float64(sy)+0.5)/k) / float64(n)
					c, in := sample(s, u, v, fill, edge, white, float64(n))
					if in {
						r += float64(c.R)
						g += float64(c.G)
						b += float64(c.B)
						a++
					}
				}
			}
			if a > 0 {
				img.SetNRGBA(x, y, color.NRGBA{uint8(r / a), uint8(g / a), uint8(b / a), uint8(math.Round(a * 255 / (k * k)))})
			}
		}
	}
	return img
}

// sample is the colour at (u, v) in [0,1)², and whether the icon covers that point.
func sample(s State, u, v float64, fill, edge, white color.NRGBA, n float64) (color.NRGBA, bool) {
	d := math.Hypot(u-0.5, v-0.5)
	const outer = 0.48
	edgeW := math.Max(0.06, 1/n) // at least a pixel
	if d > outer {
		return color.NRGBA{}, false
	}
	if s == Down { // a ring: nothing runs
		if d < outer-0.17 {
			return color.NRGBA{}, false
		}
		return fill, true
	}
	if mark(s, u, v) {
		return white, true
	}
	if d > outer-edgeW {
		return edge, true
	}
	return fill, true
}

// mark is each state's white mark.
func mark(s State, u, v float64) bool {
	in := func(u0, u1, v0, v1 float64) bool { return u >= u0 && u < u1 && v >= v0 && v < v1 }
	dot := func(cu, cv, r float64) bool { return math.Hypot(u-cu, v-cv) <= r }
	switch s {
	case Serving: // a dot: at work
		return dot(0.5, 0.5, 0.17)
	case Paused: // two bars
		return in(0.31, 0.44, 0.28, 0.72) || in(0.56, 0.69, 0.28, 0.72)
	case Stopped: // a square
		return in(0.32, 0.68, 0.32, 0.68)
	case Problem: // an exclamation mark
		return in(0.43, 0.57, 0.20, 0.58) || in(0.43, 0.57, 0.67, 0.81)
	case Setup: // three dots: to do
		return dot(0.27, 0.5, 0.085) || dot(0.5, 0.5, 0.085) || dot(0.73, 0.5, 0.085)
	case Starting: // a ring inside: on its way
		d := math.Hypot(u-0.5, v-0.5)
		return d >= 0.13 && d <= 0.23
	}
	return false
}

// encodeICO writes images as an .ico file with 32-bit BMP entries (straight alpha, bottom-up
// rows, and the 1-bit transparency mask older readers use).
func encodeICO(imgs []*image.NRGBA) []byte {
	var hdr, data bytes.Buffer
	le := binary.LittleEndian
	binary.Write(&hdr, le, [3]uint16{0, 1, uint16(len(imgs))})
	offset := 6 + 16*len(imgs)
	for _, img := range imgs {
		w, h := img.Bounds().Dx(), img.Bounds().Dy()
		maskRow := (w + 31) / 32 * 4
		size := 40 + w*h*4 + maskRow*h
		dim := func(x int) uint8 {
			if x >= 256 {
				return 0
			}
			return uint8(x)
		}
		hdr.Write([]byte{dim(w), dim(h), 0, 0})
		binary.Write(&hdr, le, []uint16{1, 32})
		binary.Write(&hdr, le, []uint32{uint32(size), uint32(offset + data.Len())})
		// BITMAPINFOHEADER: the height counts the colour rows and the mask's.
		binary.Write(&data, le, struct {
			Size                       uint32
			Width, Height              int32
			Planes, BitCount           uint16
			Compression, SizeImage     uint32
			XPels, YPels, Used, Import int32
		}{40, int32(w), int32(2 * h), 1, 32, 0, uint32(w*h*4 + maskRow*h), 0, 0, 0, 0})
		for y := h - 1; y >= 0; y-- {
			for x := 0; x < w; x++ {
				c := img.NRGBAAt(x, y)
				data.Write([]byte{c.B, c.G, c.R, c.A})
			}
		}
		for y := h - 1; y >= 0; y-- {
			row := make([]byte, maskRow)
			for x := 0; x < w; x++ {
				if img.NRGBAAt(x, y).A == 0 {
					row[x/8] |= 0x80 >> (x % 8)
				}
			}
			data.Write(row)
		}
	}
	return append(hdr.Bytes(), data.Bytes()...)
}
