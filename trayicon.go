package main

import (
	"bytes"
	"encoding/binary"
	"image"
	"image/color"
	"image/png"
	"math"
	"sync"
)

// trayState is what the tray icon shows at a glance.
type trayState int

const (
	trayOff  trayState = iota // отключено: логотип серый
	trayBusy                  // подключение, капча, переподключение: жёлтая точка
	trayOn                    // подключено: зелёная точка
	trayErr                   // ошибка: серый логотип, красная точка
)

func trayStateOf(ph phase) trayState {
	switch ph {
	case phaseProxy, phaseConnected:
		return trayOn
	case phaseStarting, phaseCaptcha, phaseWaiting:
		return trayBusy
	case phaseError:
		return trayErr
	}
	return trayOff
}

// Цвета — те же, что у статусов в окне.
var badgeColors = map[trayState]color.RGBA{
	trayBusy: {0xe0, 0xa1, 0x3a, 0xff},
	trayOn:   {0x35, 0xc4, 0x6a, 0xff},
	trayErr:  {0xf2, 0x56, 0x4c, 0xff},
}

// trayIconSizes covers what LoadImage picks for the tray at every common
// scale: systray asks for the default icon size (32 at 100%, 40 at 125%,
// 48 at 150%, 64 at 200%) and Windows shrinks it to the notification area.
var trayIconSizes = []int{16, 20, 24, 32, 40, 48, 64}

// trayIcons draws the four variants once from the logo already embedded for
// the window header. Each size is drawn separately, badge included, so the
// dot stays round and crisp instead of being a blur scaled from 256 pixels.
var trayIcons = sync.OnceValue(func() map[trayState][]byte {
	raw, err := webFS.ReadFile("web/logo.png")
	if err != nil {
		return nil
	}
	src, err := png.Decode(bytes.NewReader(raw))
	if err != nil {
		return nil
	}
	out := map[trayState][]byte{}
	for _, st := range []trayState{trayOff, trayBusy, trayOn, trayErr} {
		var frames [][]byte
		for _, n := range trayIconSizes {
			img := downscale(src, n)
			if st == trayOff || st == trayErr {
				grayscale(img)
			}
			if c, ok := badgeColors[st]; ok {
				drawBadge(img, c)
			}
			var buf bytes.Buffer
			if err := png.Encode(&buf, img); err != nil {
				return nil
			}
			frames = append(frames, buf.Bytes())
		}
		out[st] = packICO(trayIconSizes, frames)
	}
	return out
})

// trayIconFor returns the icon for st, or the plain app icon if drawing failed.
func trayIconFor(st trayState) []byte {
	if icons := trayIcons(); icons != nil {
		if b, ok := icons[st]; ok {
			return b
		}
	}
	return trayIcon
}

// downscale area-averages src to n×n in premultiplied alpha, so the rounded
// transparent corners do not bleed dark fringes into the edge.
func downscale(src image.Image, n int) *image.NRGBA {
	b := src.Bounds()
	sw, sh := b.Dx(), b.Dy()
	out := image.NewNRGBA(image.Rect(0, 0, n, n))
	for py := 0; py < n; py++ {
		y0, y1 := b.Min.Y+py*sh/n, b.Min.Y+(py+1)*sh/n
		if y1 <= y0 {
			y1 = y0 + 1
		}
		for px := 0; px < n; px++ {
			x0, x1 := b.Min.X+px*sw/n, b.Min.X+(px+1)*sw/n
			if x1 <= x0 {
				x1 = x0 + 1
			}
			var r, g, bl, a, cnt uint64
			for y := y0; y < y1; y++ {
				for x := x0; x < x1; x++ {
					cr, cg, cb, ca := src.At(x, y).RGBA() // уже premultiplied
					r += uint64(cr)
					g += uint64(cg)
					bl += uint64(cb)
					a += uint64(ca)
					cnt++
				}
			}
			if a == 0 {
				continue
			}
			out.SetNRGBA(px, py, color.NRGBA{
				R: uint8(r * 255 / a),
				G: uint8(g * 255 / a),
				B: uint8(bl * 255 / a),
				A: uint8(a / cnt >> 8),
			})
		}
	}
	return out
}

// grayscale turns the logo into the «off» look: luminance only, a touch
// translucent, the way disabled tray icons usually read.
func grayscale(img *image.NRGBA) {
	for i := 0; i < len(img.Pix); i += 4 {
		p := img.Pix[i : i+4 : i+4]
		y := uint8(0.299*float64(p[0]) + 0.587*float64(p[1]) + 0.114*float64(p[2]))
		p[0], p[1], p[2] = y, y, y
		p[3] = uint8(float64(p[3]) * 0.85)
	}
}

// drawBadge puts a status dot in the bottom-right corner, with a dark ring
// that separates it from the artwork on both light and dark taskbars.
func drawBadge(img *image.NRGBA, fill color.RGBA) {
	n := float64(img.Bounds().Dx())
	r := n * 0.26
	ring := math.Max(1, n*0.07)
	cx, cy := n-r-ring, n-r-ring
	ringColor := color.RGBA{0x0e, 0x11, 0x16, 0xff}

	const ss = 4 // сглаживание: 4×4 выборки на пиксель
	for py := 0; py < img.Bounds().Dy(); py++ {
		for px := 0; px < img.Bounds().Dx(); px++ {
			var outer, inner float64
			for sy := 0; sy < ss; sy++ {
				for sx := 0; sx < ss; sx++ {
					x := float64(px) + (float64(sx)+0.5)/ss - cx
					y := float64(py) + (float64(sy)+0.5)/ss - cy
					d := math.Hypot(x, y)
					if d <= r+ring {
						outer++
					}
					if d <= r {
						inner++
					}
				}
			}
			outer /= ss * ss
			inner /= ss * ss
			if outer == 0 {
				continue
			}
			blend(img, px, py, ringColor, outer)
			blend(img, px, py, fill, inner)
		}
	}
}

// blend composites c over the pixel with coverage k (source-over).
func blend(img *image.NRGBA, x, y int, c color.RGBA, k float64) {
	if k <= 0 {
		return
	}
	i := img.PixOffset(x, y)
	p := img.Pix[i : i+4 : i+4]
	da := float64(p[3]) / 255
	sa := k
	oa := sa + da*(1-sa)
	if oa == 0 {
		return
	}
	mix := func(s, d uint8) uint8 {
		return uint8((float64(s)*sa + float64(d)*da*(1-sa)) / oa)
	}
	p[0], p[1], p[2] = mix(c.R, p[0]), mix(c.G, p[1]), mix(c.B, p[2])
	p[3] = uint8(oa * 255)
}

// packICO writes PNG-compressed icon frames, which Windows reads since Vista.
func packICO(sizes []int, frames [][]byte) []byte {
	var out bytes.Buffer
	_ = binary.Write(&out, binary.LittleEndian, [3]uint16{0, 1, uint16(len(frames))})
	offset := 6 + 16*len(frames)
	for i, f := range frames {
		dim := byte(sizes[i]) // 256 записывается как 0; таких здесь нет
		out.Write([]byte{dim, dim, 0, 0})
		_ = binary.Write(&out, binary.LittleEndian, [2]uint16{1, 32})
		_ = binary.Write(&out, binary.LittleEndian, [2]uint32{uint32(len(f)), uint32(offset)})
		offset += len(f)
	}
	for _, f := range frames {
		out.Write(f)
	}
	return out.Bytes()
}
