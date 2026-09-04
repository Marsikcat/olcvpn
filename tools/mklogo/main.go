// Command mklogo turns the source artwork into the app icon and the header
// logo: a square crop, area-averaged down to each size, with rounded corners.
//
//	go run ./tools/mklogo -src assets/logo.jpg -ico olcvpn.ico -png web/logo.png
package main

import (
	"bytes"
	"encoding/binary"
	"flag"
	"fmt"
	"image"
	"image/color"
	_ "image/jpeg"
	"image/png"
	"math"
	"os"
)

func main() {
	var (
		src     = flag.String("src", "assets/logo.jpg", "source artwork")
		icoPath = flag.String("ico", "olcvpn.ico", "output .ico")
		pngPath = flag.String("png", "web/logo.png", "output PNG for the UI")
		cx      = flag.Float64("cx", 0.45, "crop centre X, fraction of width")
		cy      = flag.Float64("cy", 0.30, "crop centre Y, fraction of height")
		side    = flag.Float64("side", 0.86, "crop side, fraction of width")
		radius  = flag.Float64("radius", 0.22, "corner radius, fraction of side")
	)
	flag.Parse()

	f, err := os.Open(*src)
	if err != nil {
		fail(err)
	}
	img, _, err := image.Decode(f)
	f.Close()
	if err != nil {
		fail(err)
	}

	square := crop(img, *cx, *cy, *side)

	if err := writePNG(*pngPath, resize(square, 256, *radius)); err != nil {
		fail(err)
	}
	if err := writeICO(*icoPath, square, *radius); err != nil {
		fail(err)
	}
	b := square.Bounds()
	fmt.Printf("crop %dx%d -> %s, %s\n", b.Dx(), b.Dy(), *pngPath, *icoPath)
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "mklogo:", err)
	os.Exit(1)
}

// crop cuts a square around the given relative centre, clamped to the image.
func crop(src image.Image, cx, cy, side float64) image.Image {
	b := src.Bounds()
	w, h := float64(b.Dx()), float64(b.Dy())
	s := side * w

	if s > w {
		s = w
	}
	if s > h {
		s = h
	}
	x0 := clampF(cx*w-s/2, 0, w-s)
	y0 := clampF(cy*h-s/2, 0, h-s)

	r := image.Rect(
		b.Min.X+int(x0), b.Min.Y+int(y0),
		b.Min.X+int(x0+s), b.Min.Y+int(y0+s),
	)
	out := image.NewRGBA(image.Rect(0, 0, r.Dx(), r.Dy()))
	for y := r.Min.Y; y < r.Max.Y; y++ {
		for x := r.Min.X; x < r.Max.X; x++ {
			out.Set(x-r.Min.X, y-r.Min.Y, src.At(x, y))
		}
	}
	return out
}

func clampF(v, lo, hi float64) float64 {
	if hi < lo {
		return lo
	}
	return math.Min(math.Max(v, lo), hi)
}

// resize does an area average down to n pixels and applies rounded corners.
// Area averaging is the right filter here: the source is an order of magnitude
// larger than every target size, so each output pixel covers many input ones.
func resize(src image.Image, n int, radius float64) *image.RGBA {
	b := src.Bounds()
	sw, sh := b.Dx(), b.Dy()
	out := image.NewRGBA(image.Rect(0, 0, n, n))

	fn := float64(n)
	rad := radius * fn

	for py := 0; py < n; py++ {
		for px := 0; px < n; px++ {
			x0 := b.Min.X + px*sw/n
			x1 := b.Min.X + (px+1)*sw/n
			y0 := b.Min.Y + py*sh/n
			y1 := b.Min.Y + (py+1)*sh/n
			if x1 <= x0 {
				x1 = x0 + 1
			}
			if y1 <= y0 {
				y1 = y0 + 1
			}

			var sr, sg, sb, count uint64
			for y := y0; y < y1; y++ {
				for x := x0; x < x1; x++ {
					r, g, bl, _ := src.At(x, y).RGBA()
					sr += uint64(r >> 8)
					sg += uint64(g >> 8)
					sb += uint64(bl >> 8)
					count++
				}
			}
			if count == 0 {
				continue
			}

			out.Set(px, py, color.RGBA{
				R: uint8(sr / count),
				G: uint8(sg / count),
				B: uint8(sb / count),
				A: uint8(255 * cornerCoverage(float64(px), float64(py), fn, rad)),
			})
		}
	}
	return out
}

// cornerCoverage supersamples the rounded-square mask so the corners stay
// smooth even at 16 pixels.
func cornerCoverage(px, py, n, radius float64) float64 {
	const ss = 4
	var in float64
	for sy := 0; sy < ss; sy++ {
		for sx := 0; sx < ss; sx++ {
			x := px + (float64(sx)+0.5)/ss
			y := py + (float64(sy)+0.5)/ss
			dx := x - clampF(x, radius, n-radius)
			dy := y - clampF(y, radius, n-radius)
			if dx*dx+dy*dy <= radius*radius {
				in++
			}
		}
	}
	return in / (ss * ss)
}

func writePNG(path string, img image.Image) error {
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return err
	}
	return os.WriteFile(path, buf.Bytes(), 0o644)
}

// writeICO packs PNG-compressed images, which Windows has accepted since Vista.
func writeICO(path string, square image.Image, radius float64) error {
	sizes := []int{16, 24, 32, 48, 64, 128, 256}

	type entry struct {
		size int
		data []byte
	}
	entries := make([]entry, 0, len(sizes))
	for _, s := range sizes {
		var buf bytes.Buffer
		if err := png.Encode(&buf, resize(square, s, radius)); err != nil {
			return err
		}
		entries = append(entries, entry{s, buf.Bytes()})
	}

	var out bytes.Buffer
	binary.Write(&out, binary.LittleEndian, uint16(0)) // reserved
	binary.Write(&out, binary.LittleEndian, uint16(1)) // type: icon
	binary.Write(&out, binary.LittleEndian, uint16(len(entries)))

	offset := 6 + 16*len(entries)
	for _, e := range entries {
		dim := byte(e.size)
		if e.size >= 256 {
			dim = 0 // 256 is encoded as zero
		}
		out.WriteByte(dim)
		out.WriteByte(dim)
		out.WriteByte(0) // palette size
		out.WriteByte(0) // reserved
		binary.Write(&out, binary.LittleEndian, uint16(1))
		binary.Write(&out, binary.LittleEndian, uint16(32))
		binary.Write(&out, binary.LittleEndian, uint32(len(e.data)))
		binary.Write(&out, binary.LittleEndian, uint32(offset))
		offset += len(e.data)
	}
	for _, e := range entries {
		out.Write(e.data)
	}
	return os.WriteFile(path, out.Bytes(), 0o644)
}
