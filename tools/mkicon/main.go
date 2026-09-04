// Command mkicon renders the olcvpn app icon (a rounded gradient tile with a
// power glyph) and writes it as a multi-size Windows .ico file.
package main

import (
	"bytes"
	"encoding/binary"
	"image"
	"image/color"
	"image/png"
	"math"
	"os"
)

const ss = 4 // supersampling factor

type rgb struct{ r, g, b float64 }

var (
	from = rgb{0x4c, 0x8d, 0xff}
	to   = rgb{0x7b, 0x5c, 0xff}
)

// roundedRect reports whether p is inside a rounded square of side n.
func roundedRect(x, y, n, radius float64) bool {
	dx, dy := x-clamp(x, radius, n-radius), y-clamp(y, radius, n-radius)
	return dx*dx+dy*dy <= radius*radius
}

func clamp(v, lo, hi float64) float64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// powerGlyph reports whether p belongs to the power symbol: an open ring plus
// a vertical stem through the gap.
func powerGlyph(x, y, n float64) bool {
	cx, cy := n/2, n*0.545
	r := n * 0.235
	w := n * 0.072

	dx, dy := x-cx, y-cy
	dist := math.Hypot(dx, dy)

	// Ring, open at the top (a 100-degree gap centred on -90 degrees).
	if math.Abs(dist-r) <= w/2 {
		ang := math.Atan2(dy, dx) * 180 / math.Pi // -180..180, -90 is up
		if !(ang > -140 && ang < -40) {
			return true
		}
	}

	// Stem.
	if math.Abs(dx) <= w/2 && y >= n*0.215 && y <= cy-r*0.15 {
		return true
	}
	return false
}

func render(n int) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, n, n))
	fn := float64(n)
	radius := fn * 0.22

	for py := 0; py < n; py++ {
		for px := 0; px < n; px++ {
			var cov, glyph float64
			for sy := 0; sy < ss; sy++ {
				for sx := 0; sx < ss; sx++ {
					x := float64(px) + (float64(sx)+0.5)/ss
					y := float64(py) + (float64(sy)+0.5)/ss
					if !roundedRect(x, y, fn, radius) {
						continue
					}
					cov++
					if powerGlyph(x, y, fn) {
						glyph++
					}
				}
			}
			total := float64(ss * ss)
			if cov == 0 {
				continue
			}
			// Diagonal gradient.
			t := (float64(px) + float64(py)) / (2 * fn)
			base := color.RGBA{
				R: uint8(from.r + (to.r-from.r)*t),
				G: uint8(from.g + (to.g-from.g)*t),
				B: uint8(from.b + (to.b-from.b)*t),
				A: 255,
			}
			g := glyph / total
			out := color.RGBA{
				R: uint8(float64(base.R)*(1-g) + 255*g),
				G: uint8(float64(base.G)*(1-g) + 255*g),
				B: uint8(float64(base.B)*(1-g) + 255*g),
				A: uint8(255 * cov / total),
			}
			img.Set(px, py, out)
		}
	}
	return img
}

func main() {
	sizes := []int{16, 24, 32, 48, 64, 128, 256}

	type entry struct {
		size int
		data []byte
	}
	var entries []entry
	for _, s := range sizes {
		var buf bytes.Buffer
		if err := png.Encode(&buf, render(s)); err != nil {
			panic(err)
		}
		entries = append(entries, entry{s, buf.Bytes()})
	}

	var out bytes.Buffer
	// ICONDIR
	binary.Write(&out, binary.LittleEndian, uint16(0))
	binary.Write(&out, binary.LittleEndian, uint16(1))
	binary.Write(&out, binary.LittleEndian, uint16(len(entries)))

	offset := 6 + 16*len(entries)
	for _, e := range entries {
		b := byte(e.size)
		if e.size >= 256 {
			b = 0
		}
		out.WriteByte(b)                                    // width
		out.WriteByte(b)                                    // height
		out.WriteByte(0)                                    // palette
		out.WriteByte(0)                                    // reserved
		binary.Write(&out, binary.LittleEndian, uint16(1))  // planes
		binary.Write(&out, binary.LittleEndian, uint16(32)) // bpp
		binary.Write(&out, binary.LittleEndian, uint32(len(e.data)))
		binary.Write(&out, binary.LittleEndian, uint32(offset))
		offset += len(e.data)
	}
	for _, e := range entries {
		out.Write(e.data)
	}

	if err := os.WriteFile(os.Args[1], out.Bytes(), 0o644); err != nil {
		panic(err)
	}
}
