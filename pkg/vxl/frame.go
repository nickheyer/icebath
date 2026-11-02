package vxl

import (
	"math"
	"runtime"
	"slices"
)

// Pixel grid with a depth and a facing per pixel and the shape of its pixels on screen
type Frame struct {
	W, H  int
	Color []RGB
	// One over the view depth of the pixel, zero where nothing was drawn
	Depth []float32
	// Facing of the opaque face the pixel shows, packed, zero where none was drawn
	Normal []uint32
	// Width of one pixel over its height as the screen shows it
	PixelAspect float64
	vig         []float32
	vigW, vigH  int
	vigS        float64
}

// Frame of the size, the buffers reused when the size did not change
func (fr *Frame) Resize(w, h int) {
	w, h = max(w, 0), max(h, 0)
	if fr.PixelAspect == 0 {
		fr.PixelAspect = 1
	}
	if fr.W != w || fr.H != h || fr.Color == nil || len(fr.Normal) != w*h {
		fr.W, fr.H = w, h
		fr.Color = make([]RGB, w*h)
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

// Copies the pixels, depths and facings of a frame of the same shape
func (fr *Frame) CopyFrom(o *Frame) {
	fr.PixelAspect = o.PixelAspect
	fr.Resize(o.W, o.H)
	parallelRows(fr.H, func(y0, y1 int) {
		a, b := y0*fr.W, y1*fr.W
		copy(fr.Color[a:b], o.Color[a:b])
		copy(fr.Depth[a:b], o.Depth[a:b])
		copy(fr.Normal[a:b], o.Normal[a:b])
	})
}

// Pixel color, the nearest pixel of the frame past its edge
func (fr *Frame) At(x, y int) RGB {
	if fr.W == 0 || fr.H == 0 {
		return RGB{}
	}
	x, y = min(max(x, 0), fr.W-1), min(max(y, 0), fr.H-1)
	return fr.Color[y*fr.W+x]
}

// Share of pixel depth a neighbor must stand nearer than the plane through the pixel predicts to cast a silhouette
const silhouetteJump = 0.02

// Share of the outline strength crease lines darken by
const creaseShare = 0.5

// Dot product of packed facings below which two faces meet at a crease, about twenty six degrees apart
const creaseDot = 127 * 127 * 9 / 10

// Reports whether two packed facings of drawn pixels turn apart past the fold angle
func crease(a, b uint32) bool {
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

// Scratch buffers of the bloom pass, reused across frames
type Post struct {
	small []RGB
	sw    int
	sh    int
	w     int
	// Columns of the small grid each pixel column blends between and the share of the second
	x0, x1 []int32
	tx     []float64
	// Small rows blurred down the grid for each band of pixel rows, and which of them hold any light
	vert []RGB
	lit  []bool
	row  []RGB
}

// Factor the bloom pass shrinks the frame by before blurring
const bloomShrink = 4

// Weights of the nine tap blur, the five tap binomial blur run twice, so each axis takes one pass
var bloomWeights = [9]float64{1.0 / 256, 8.0 / 256, 28.0 / 256, 56.0 / 256, 70.0 / 256, 56.0 / 256, 28.0 / 256, 8.0 / 256, 1.0 / 256}

// Spreads the light of bright pixels into their surroundings, the glow added at the strength
func (fr *Frame) Bloom(p *Post, threshold, strength float64) {
	if fr.W == 0 || fr.H == 0 || strength <= 0 {
		return
	}
	sw, sh := (fr.W+bloomShrink-1)/bloomShrink, (fr.H+bloomShrink-1)/bloomShrink
	if p.sw != sw || p.sh != sh || p.w != fr.W {
		p.sw, p.sh, p.w = sw, sh, fr.W
		p.small = make([]RGB, sw*sh)
		p.x0, p.x1, p.tx = make([]int32, fr.W), make([]int32, fr.W), make([]float64, fr.W)
		for x := range fr.W {
			fx := (float64(x)+0.5)/bloomShrink - 0.5
			x0 := min(max(int(math.Floor(fx)), 0), sw-1)
			p.x0[x], p.x1[x], p.tx[x] = int32(x0), int32(min(x0+1, sw-1)), min(max(fx-float64(x0), 0), 1)
		}
	}
	workers := min(runtime.NumCPU(), max(sh/2, 1))
	band := (sh + workers - 1) / workers
	nb := (sh + band - 1) / band
	p.row = slices.Grow(p.row[:0], nb*sw)[:nb*sw]
	inv := 1 / float64(bloomShrink*bloomShrink)
	// Shrinks the bright share of each block of pixels into a small row and blurs it across
	parallel(nb, workers, func(b int) {
		row := p.row[b*sw : (b+1)*sw]
		for sy := b * band; sy < min((b+1)*band, sh); sy++ {
			clear(row)
			for y := sy * bloomShrink; y < min((sy+1)*bloomShrink, fr.H); y++ {
				for x, c := range fr.Color[y*fr.W : (y+1)*fr.W] {
					if l := c.Luma(); l > threshold {
						row[x/bloomShrink] = row[x/bloomShrink].Add(c.Scale((l - threshold) / l * inv))
					}
				}
			}
			dst := p.small[sy*sw : (sy+1)*sw]
			for x := range sw {
				var acc RGB
				for k, wt := range bloomWeights {
					acc = acc.Add(row[min(max(x+k-4, 0), sw-1)].Scale(wt))
				}
				dst[x] = acc
			}
		}
	})
	workers = min(runtime.NumCPU(), max(fr.H/8, 1))
	band = (fr.H + workers - 1) / workers
	nb = (fr.H + band - 1) / band
	need := band/bloomShrink + 3
	p.vert = slices.Grow(p.vert[:0], nb*need*sw)[:nb*need*sw]
	p.lit = slices.Grow(p.lit[:0], nb*need)[:nb*need]
	smallRow := func(y int) int { return min(max(int(math.Floor((float64(y)+0.5)/bloomShrink-0.5)), 0), sh-1) }
	// Blurs down the grid the small rows a band of pixel rows needs, then lays them over its pixels
	parallel(nb, workers, func(b int) {
		r0, r1 := b*band, min((b+1)*band, fr.H)
		top, bottom := smallRow(r0), min(smallRow(r1-1)+1, sh-1)
		rows, lit := p.vert[b*need*sw:(b+1)*need*sw], p.lit[b*need:(b+1)*need]
		for sy := top; sy <= bottom; sy++ {
			dst := rows[(sy-top)*sw : (sy-top+1)*sw]
			on := false
			for x := range sw {
				var acc RGB
				for k, wt := range bloomWeights {
					acc = acc.Add(p.small[min(max(sy+k-4, 0), sh-1)*sw+x].Scale(wt * strength))
				}
				dst[x] = acc
				on = on || acc != RGB{}
			}
			lit[sy-top] = on
		}
		var mixed [512]RGB
		buf := mixed[:0]
		if sw > len(mixed) {
			buf = make([]RGB, 0, sw)
		}
		buf = buf[:sw]
		for y := r0; y < r1; y++ {
			fy := (float64(y)+0.5)/bloomShrink - 0.5
			y0 := smallRow(y)
			y1 := min(y0+1, sh-1)
			if !lit[y0-top] && !lit[y1-top] {
				continue
			}
			a, c := rows[(y0-top)*sw:(y0-top+1)*sw], rows[(y1-top)*sw:(y1-top+1)*sw]
			ty := min(max(fy-float64(y0), 0), 1)
			for x := range sw {
				buf[x] = RGB{a[x].R + (c[x].R-a[x].R)*ty, a[x].G + (c[x].G-a[x].G)*ty, a[x].B + (c[x].B-a[x].B)*ty}
			}
			row := fr.Color[y*fr.W : (y+1)*fr.W]
			for x := range row {
				l, r := buf[p.x0[x]], buf[p.x1[x]]
				if l == (RGB{}) && r == (RGB{}) {
					continue
				}
				t := p.tx[x]
				row[x] = row[x].Add(RGB{l.R + (r.R-l.R)*t, l.G + (r.G-l.G)*t, l.B + (r.B-l.B)*t})
			}
		}
	})
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
