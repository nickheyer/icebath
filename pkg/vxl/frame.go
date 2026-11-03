package vxl

import (
	"math"
	"slices"
)

// Pixel grid with the surface, depth and facing of every opaque pixel and the shape of its pixels on screen
type Frame struct {
	W, H  int
	Color []RGB
	// Color of each opaque pixel before scene light and fog, its reflectance where the facing is marked lit
	Albedo []RGB
	// One over the view depth of the pixel, zero where nothing was drawn
	Depth []float32
	// Packed facing of the opaque face the pixel shows, bit 25 marking it lit and gloss above, zero where none
	Normal []uint32
	// Width of one pixel over its height as the screen shows it
	PixelAspect float64
	vig         []float32
	vigW, vigH  int
	vigS        float64
	tmp         []RGB
}

// Frame of the size, the buffers reused when the size did not change
func (fr *Frame) Resize(w, h int) {
	w, h = max(w, 0), max(h, 0)
	if fr.PixelAspect == 0 {
		fr.PixelAspect = 1
	}
	if fr.W != w || fr.H != h || fr.Color == nil || len(fr.Normal) != w*h || len(fr.Albedo) != w*h {
		fr.W, fr.H = w, h
		fr.Color = make([]RGB, w*h)
		fr.Albedo = make([]RGB, w*h)
		fr.Depth = make([]float32, w*h)
		fr.Normal = make([]uint32, w*h)
	}
}

// Paints the background from a function of the pixel, called from several goroutines, and clears depth and facing
func (fr *Frame) Clear(bg func(x, y int) RGB) {
	parallelRows(fr.H, func(y0, y1 int) {
		for y := y0; y < y1; y++ {
			row := fr.Color[y*fr.W : (y+1)*fr.W]
			for x := range row {
				row[x] = bg(x, y)
			}
		}
		clear(fr.Depth[y0*fr.W : y1*fr.W])
		clear(fr.Normal[y0*fr.W : y1*fr.W])
	})
}

// Copies the pixels, surfaces, depths and facings of a frame
func (fr *Frame) CopyFrom(o *Frame) {
	fr.PixelAspect = o.PixelAspect
	fr.Resize(o.W, o.H)
	parallelRows(fr.H, func(y0, y1 int) {
		a, b := y0*fr.W, y1*fr.W
		copy(fr.Color[a:b], o.Color[a:b])
		copy(fr.Albedo[a:b], o.Albedo[a:b])
		copy(fr.Depth[a:b], o.Depth[a:b])
		copy(fr.Normal[a:b], o.Normal[a:b])
	})
}

// Red, green and blue of every pixel as bytes, row by row, written over the slice given
func (fr *Frame) Bytes(dst []byte) []byte {
	n := fr.W * fr.H * 3
	if cap(dst) < n {
		dst = make([]byte, n)
	}
	dst = dst[:n]
	parallelRows(fr.H, func(y0, y1 int) {
		for i := y0 * fr.W; i < y1*fr.W; i++ {
			c := fr.Color[i]
			dst[i*3], dst[i*3+1], dst[i*3+2] = channel(c.R), channel(c.G), channel(c.B)
		}
	})
	return dst
}

// Pixel color, the nearest pixel of the frame past its edge
func (fr *Frame) At(x, y int) RGB {
	if fr.W == 0 || fr.H == 0 {
		return RGB{}
	}
	x, y = min(max(x, 0), fr.W-1), min(max(y, 0), fr.H-1)
	return fr.Color[y*fr.W+x]
}

// Pixel color between pixel centers, blended from the four around a place in pixels
func (fr *Frame) lerpAt(x, y float64) RGB {
	x, y = x-0.5, y-0.5
	x0, y0 := int(math.Floor(x)), int(math.Floor(y))
	tx, ty := x-float64(x0), y-float64(y0)
	a, b := fr.At(x0, y0), fr.At(x0+1, y0)
	c, d := fr.At(x0, y0+1), fr.At(x0+1, y0+1)
	top := RGB{a.R + (b.R-a.R)*tx, a.G + (b.G-a.G)*tx, a.B + (b.B-a.B)*tx}
	bot := RGB{c.R + (d.R-c.R)*tx, c.G + (d.G-c.G)*tx, c.B + (d.B-c.B)*tx}
	return RGB{top.R + (bot.R-top.R)*ty, top.G + (bot.G-top.G)*ty, top.B + (bot.B-top.B)*ty}
}

// Share of pixel depth a neighbor must stand nearer than the plane through the pixel predicts to cast a silhouette
const silhouetteJump = 0.02

// Share of the outline strength crease lines darken by
const creaseShare = 0.5

// Dot product of packed facings below which two faces meet at a crease, about twenty six degrees apart
const creaseDot = 127 * 127 * 9 / 10

// Reports whether two packed facings of drawn pixels turn apart past the fold angle, lit or not, whatever their gloss
func crease(a, b uint32) bool {
	a, b = a&normalDir, b&normalDir
	if a == b || a == 0 || b == 0 {
		return false
	}
	d := int(int8(a))*int(int8(b)) + int(int8(a>>8))*int(int8(b>>8)) + int(int8(a>>16))*int(int8(b>>16))
	return d < creaseDot
}

// Reports whether a neighbor stands nearer than the plane through a pixel and its opposite neighbor predicts
func jump(d, n, opp float32) bool {
	want := d
	if opp > 0 {
		want = 2*d - opp
	}
	return n > want+silhouetteJump*d
}

// Darkens pixels beside a nearer surface into silhouettes and pixels where faces turn apart into crease lines
func (fr *Frame) Outline(strength float64) {
	w, h := fr.W, fr.H
	if w == 0 || h == 0 || strength <= 0 {
		return
	}
	strength = min(strength, 1)
	sil, fold := 1-strength, 1-strength*creaseShare
	D, N, C := fr.Depth, fr.Normal, fr.Color
	parallelRows(h, func(y0, y1 int) {
		for y := y0; y < y1; y++ {
			row, up, down := y*w, max(y-1, 0)*w, min(y+1, h-1)*w
			for x := range w {
				i := row + x
				l, r, u, b := row+max(x-1, 0), row+min(x+1, w-1), up+x, down+x
				d := D[i]
				f := 1.0
				switch {
				case d == 0:
					if D[l] > 0 || D[r] > 0 || D[u] > 0 || D[b] > 0 {
						f = sil
					}
				case jump(d, D[l], D[r]) || jump(d, D[r], D[l]) || jump(d, D[u], D[b]) || jump(d, D[b], D[u]):
					f = sil
				case crease(N[i], N[r]) && !jump(D[r], d, 0) || crease(N[i], N[b]) && !jump(D[b], d, 0):
					f = fold
				}
				if f < 1 {
					C[i] = C[i].Scale(f)
				}
			}
		}
	})
}

// One level of the bloom chain, a grid of pixels at a shrink of the frame
type bloomLevel struct {
	w, h int
	px   []RGB
	tmp  []RGB
}

// Scratch buffers of the bloom pass, reused across frames
type Post struct {
	levels []bloomLevel
	w, h   int
}

// Most levels the bloom chain shrinks through, each half the size of the one before
const bloomLevels = 6

// Spreads the light of bright pixels into their surroundings through a chain of halved blurs, the glow added at the strength
func (fr *Frame) Bloom(p *Post, threshold, strength float64) {
	if fr.W == 0 || fr.H == 0 || strength <= 0 {
		return
	}
	if p.w != fr.W || p.h != fr.H || len(p.levels) == 0 {
		p.w, p.h = fr.W, fr.H
		p.levels = p.levels[:0]
		w, h := (fr.W+1)/2, (fr.H+1)/2
		for len(p.levels) < bloomLevels && w >= 2 && h >= 2 {
			p.levels = append(p.levels, bloomLevel{w: w, h: h, px: make([]RGB, w*h), tmp: make([]RGB, w*h)})
			w, h = (w+1)/2, (h+1)/2
		}
	}
	if len(p.levels) == 0 {
		return
	}
	top := &p.levels[0]
	parallelRows(top.h, func(y0, y1 int) {
		for y := y0; y < y1; y++ {
			for x := range top.w {
				var sum RGB
				for _, c := range [4]RGB{fr.At(2*x, 2*y), fr.At(2*x+1, 2*y), fr.At(2*x, 2*y+1), fr.At(2*x+1, 2*y+1)} {
					if l := c.Luma(); l > threshold {
						sum = sum.Add(c.Scale((l - threshold) / l))
					}
				}
				top.px[y*top.w+x] = sum.Scale(0.25)
			}
		}
	})
	top.blur()
	for i := 1; i < len(p.levels); i++ {
		src, dst := &p.levels[i-1], &p.levels[i]
		parallelRows(dst.h, func(y0, y1 int) {
			for y := y0; y < y1; y++ {
				for x := range dst.w {
					dst.px[y*dst.w+x] = src.at(2*x, 2*y).Add(src.at(2*x+1, 2*y)).Add(src.at(2*x, 2*y+1)).Add(src.at(2*x+1, 2*y+1)).Scale(0.25)
				}
			}
		})
		dst.blur()
	}
	for i := len(p.levels) - 2; i >= 0; i-- {
		src, dst := &p.levels[i+1], &p.levels[i]
		parallelRows(dst.h, func(y0, y1 int) {
			for y := y0; y < y1; y++ {
				for x := range dst.w {
					dst.px[y*dst.w+x] = dst.px[y*dst.w+x].Add(src.lerp((float64(x)+0.5)/2, (float64(y)+0.5)/2))
				}
			}
		})
	}
	k := strength / float64(len(p.levels))
	parallelRows(fr.H, func(y0, y1 int) {
		for y := y0; y < y1; y++ {
			row := fr.Color[y*fr.W : (y+1)*fr.W]
			for x := range row {
				row[x] = row[x].Add(top.lerp((float64(x)+0.5)/2, (float64(y)+0.5)/2).Scale(k))
			}
		}
	})
}

// Pixel of a level, the nearest one past its edge
func (l *bloomLevel) at(x, y int) RGB {
	return l.px[min(max(y, 0), l.h-1)*l.w+min(max(x, 0), l.w-1)]
}

// Color between the pixel centers of a level, blended from the four around a place in its pixels
func (l *bloomLevel) lerp(x, y float64) RGB {
	x, y = x-0.5, y-0.5
	x0, y0 := int(math.Floor(x)), int(math.Floor(y))
	tx, ty := x-float64(x0), y-float64(y0)
	a, b := l.at(x0, y0), l.at(x0+1, y0)
	c, d := l.at(x0, y0+1), l.at(x0+1, y0+1)
	top := RGB{a.R + (b.R-a.R)*tx, a.G + (b.G-a.G)*tx, a.B + (b.B-a.B)*tx}
	bot := RGB{c.R + (d.R-c.R)*tx, c.G + (d.G-c.G)*tx, c.B + (d.B-c.B)*tx}
	return RGB{top.R + (bot.R-top.R)*ty, top.G + (bot.G-top.G)*ty, top.B + (bot.B-top.B)*ty}
}

// Blurs a level with a three tap binomial filter across then down
func (l *bloomLevel) blur() {
	parallelRows(l.h, func(y0, y1 int) {
		for y := y0; y < y1; y++ {
			for x := range l.w {
				l.tmp[y*l.w+x] = l.at(x-1, y).Add(l.at(x, y).Scale(2)).Add(l.at(x+1, y)).Scale(0.25)
			}
		}
	})
	l.px, l.tmp = l.tmp, l.px
	parallelRows(l.h, func(y0, y1 int) {
		for y := y0; y < y1; y++ {
			for x := range l.w {
				l.tmp[y*l.w+x] = l.at(x, y-1).Add(l.at(x, y).Scale(2)).Add(l.at(x, y+1)).Scale(0.25)
			}
		}
	})
	l.px, l.tmp = l.tmp, l.px
}

// Darkens the pixels toward the edges of the frame by the strength
func (fr *Frame) Vignette(strength float64) {
	if fr.W == 0 || fr.H == 0 || strength <= 0 {
		return
	}
	if fr.vigW != fr.W || fr.vigH != fr.H || fr.vigS != strength || len(fr.vig) != fr.W*fr.H {
		fr.vigW, fr.vigH, fr.vigS = fr.W, fr.H, strength
		fr.vig = make([]float32, fr.W*fr.H)
		parallelRows(fr.H, func(y0, y1 int) {
			for y := y0; y < y1; y++ {
				dy := (float64(y)+0.5)/float64(fr.H)*2 - 1
				for x := range fr.W {
					dx := (float64(x)+0.5)/float64(fr.W)*2 - 1
					fr.vig[y*fr.W+x] = float32(1 - strength*Smoothstep((dx*dx+dy*dy*0.7-0.35)/1.3))
				}
			}
		})
	}
	parallelRows(fr.H, func(y0, y1 int) {
		for i := y0 * fr.W; i < y1*fr.W; i++ {
			if k := fr.vig[i]; k < 1 {
				fr.Color[i] = fr.Color[i].Scale(float64(k))
			}
		}
	})
}

// Look of the finished frame, exposure and a filmic curve over the light, then saturation and color fringing
type Grade struct {
	// Factor every pixel is scaled by before the curve, one when zero
	Exposure float64
	// Filmic curve rolling bright light off toward white instead of clipping it
	Tonemap bool
	// Factor the distance of every color from grey is scaled by, one when zero
	Saturation float64
	// Pixels the red and blue channels slide apart by at the corners of the frame, none at zero
	Aberration float64
}

// Applies the grade to every pixel, the fringing read from a copy of the frame
func (fr *Frame) Grade(g Grade) {
	if fr.W == 0 || fr.H == 0 {
		return
	}
	exposure, sat := g.Exposure, g.Saturation
	if exposure == 0 {
		exposure = 1
	}
	if sat == 0 {
		sat = 1
	}
	if g.Aberration > 0 {
		fr.tmp = slices.Grow(fr.tmp[:0], fr.W*fr.H)[:fr.W*fr.H]
		copy(fr.tmp, fr.Color)
		src := &Frame{W: fr.W, H: fr.H, Color: fr.tmp}
		cx, cy := float64(fr.W)/2, float64(fr.H)/2
		parallelRows(fr.H, func(y0, y1 int) {
			for y := y0; y < y1; y++ {
				fy := (float64(y) + 0.5 - cy) / cy
				for x := range fr.W {
					fx := (float64(x) + 0.5 - cx) / cx
					k := g.Aberration * (fx*fx + fy*fy)
					ox, oy := fx*k, fy*k*fr.PixelAspect
					px, py := float64(x)+0.5, float64(y)+0.5
					c := &fr.Color[y*fr.W+x]
					c.R = src.lerpAt(px-ox, py-oy).R
					c.B = src.lerpAt(px+ox, py+oy).B
				}
			}
		})
	}
	parallelRows(fr.H, func(y0, y1 int) {
		for i := y0 * fr.W; i < y1*fr.W; i++ {
			c := fr.Color[i]
			if exposure != 1 {
				c = c.Scale(exposure)
			}
			if g.Tonemap {
				c = RGB{aces(c.R), aces(c.G), aces(c.B)}
			}
			if sat != 1 {
				l := c.Luma()
				c = RGB{l + (c.R-l)*sat, l + (c.G-l)*sat, l + (c.B-l)*sat}
			}
			fr.Color[i] = c
		}
	})
}

// Filmic curve of one channel, near straight in the shadows and rolling off to one in the highlights
func aces(x float64) float64 {
	if x <= 0 {
		return 0
	}
	return min((x*(2.51*x+0.03))/(x*(2.43*x+0.59)+0.14), 1)
}
