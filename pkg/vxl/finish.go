package vxl

import (
	"math"
	"slices"
)

// Look of a finished frame, the outline, the bloom, the grade and the vignette
type Finish struct {
	Outline        float64
	BloomThreshold float64
	BloomStrength  float64
	Grade          Grade
	Vignette       float64
}

// Shown glow change under which a bloom cell keeps the glow it was finished with, a quarter step of 255
const glowSlack = 1.0 / 1024

// Finished pixels of a frame kept between frames, so each finish reworks only the pixels that changed
type Output struct {
	W, H int
	// Finished color of every pixel and the same as bytes of red, green and blue, row by row
	Color []RGB
	Bytes []byte
	// Outline darkening of every pixel
	fac []float32
	// Outlined color with the glow added, kept only while the grade fringes
	pre []RGB
	// Bright light of every pair of pixels before the blur, the blur chain over it, and the glow of the last finish
	raw          []RGB
	post         Post
	glow         []RGB
	glowW, glowH int
	vig          []float32
	vigS         float64
	fin          Finish
	aspect       float64
	// Color every pixel was last finished from
	src []RGB
	// Pixels whose input changed, the ones whose outline may have, and the pixels the finish changed
	in    Dirt
	edged Dirt
	dirt  Dirt
	// Pixels whose finished bytes the last finish changed, counted per row
	moved     int
	movedRows []int32
}

// Pixels the last finish changed
func (o *Output) Dirt() *Dirt { return &o.dirt }

// Pixels whose finished bytes the last finish changed
func (o *Output) Moved() int { return o.moved }

// Finishes the changed pixels whose color moved since the last finish and whatever they touch, every pixel for nil
func (o *Output) Finish(fr *Frame, fin Finish, c *Changes) {
	w, h := fr.W, fr.H
	n := w * h
	all := c == nil || c.All() || c.Color.W != w || c.Color.H != h || o.W != w || o.H != h || o.fin != fin || o.aspect != fr.PixelAspect || len(o.Color) != n || len(o.src) != n
	o.W, o.H, o.fin, o.aspect = w, h, fin, fr.PixelAspect
	resized := len(o.Bytes) != n*3
	o.Color = slices.Grow(o.Color[:0], n)[:n]
	o.Bytes = slices.Grow(o.Bytes[:0], n*3)[:n*3]
	o.fac = slices.Grow(o.fac[:0], n)[:n]
	o.src = slices.Grow(o.src[:0], n)[:n]
	o.edged.Reset(w, h)
	o.in.Reset(w, h)
	if all {
		o.in.Fill()
		o.edged.Fill()
	} else {
		parallelRows(h, func(y0, y1 int) {
			for y := y0; y < y1; y++ {
				row := y * w
				c.Color.Each(y, func(xa, xb int) {
					for x := xa; x <= xb; x++ {
						if fr.Color[row+x] != o.src[row+x] {
							o.in.Add(y, x, x)
						}
					}
				})
			}
		})
		if fin.Outline > 0 {
			o.edged.CopyFrom(&c.Drawn)
			o.edged.Grow(1, 1)
			o.in.Union(&o.edged)
		}
		o.in.Norm()
	}
	parallelRows(h, func(y0, y1 int) {
		for y := y0; y < y1; y++ {
			row := y * w
			o.in.Each(y, func(xa, xb int) {
				copy(o.src[row+xa:row+xb+1], fr.Color[row+xa:row+xb+1])
			})
		}
	})
	o.dirt.Reset(w, h)
	o.moved = 0
	if n == 0 {
		return
	}
	o.outline(fr, fin.Outline)
	if fin.BloomStrength > 0 {
		o.bloom(fr, fin, all)
	} else {
		o.glow, o.glowW, o.glowH = o.glow[:0], 0, 0
	}
	o.dirt.Union(&o.in)
	if ab := fin.Grade.Aberration; ab > 0 {
		o.dirt.Grow(int(math.Ceil(2*ab))+1, int(math.Ceil(2*ab*fr.PixelAspect))+1)
	}
	o.dirt.Norm()
	o.vignette(fin.Vignette)
	o.finish(fr, fin)
	if resized {
		o.moved = n
	}
}

// Darkening of every pixel drawn on or beside one, beside a nearer surface or at a crease, one everywhere without an outline
func (o *Output) outline(fr *Frame, strength float64) {
	w, h := fr.W, fr.H
	if strength <= 0 {
		if o.in.All {
			for i := range o.fac {
				o.fac[i] = 1
			}
		}
		return
	}
	strength = min(strength, 1)
	sil, fold := float32(1-strength), float32(1-strength*creaseShare)
	D, N := fr.Depth, fr.Normal
	parallelRows(h, func(y0, y1 int) {
		for y := y0; y < y1; y++ {
			row, up, down := y*w, max(y-1, 0)*w, min(y+1, h-1)*w
			o.edged.Each(y, func(xa, xb int) {
				for x := xa; x <= xb; x++ {
					i := row + x
					l, r, u, b := row+max(x-1, 0), row+min(x+1, w-1), up+x, down+x
					d := D[i]
					f := float32(1)
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
					o.fac[i] = f
				}
			})
		}
	})
}

// Outlined color of a pixel, the nearest pixel past the edge of the frame
func (o *Output) lined(fr *Frame, x, y int) RGB {
	x, y = min(max(x, 0), fr.W-1), min(max(y, 0), fr.H-1)
	i := y*fr.W + x
	return fr.Color[i].Scale(float64(o.fac[i]))
}

// Blurs the bright changed pixels down the chain and marks the pixels whose glow moved past the slack
func (o *Output) bloom(fr *Frame, fin Finish, all bool) {
	w, h := fr.W, fr.H
	tw, th := (w+1)/2, (h+1)/2
	p := &o.post
	if p.w != w || p.h != h || len(p.levels) == 0 {
		p.w, p.h = w, h
		p.levels = p.levels[:0]
		lw, lh := tw, th
		for len(p.levels) < bloomLevels && lw >= 2 && lh >= 2 {
			p.levels = append(p.levels, bloomLevel{w: lw, h: lh, px: make([]RGB, lw*lh), tmp: make([]RGB, lw*lh)})
			lw, lh = (lw+1)/2, (lh+1)/2
		}
	}
	if len(p.levels) == 0 {
		o.glow, o.glowW, o.glowH = o.glow[:0], 0, 0
		return
	}
	fresh := all || len(o.raw) != tw*th
	o.raw = slices.Grow(o.raw[:0], tw*th)[:tw*th]
	threshold := fin.BloomThreshold
	cell := func(x, y int) RGB {
		var sum RGB
		for _, c := range [4]RGB{o.lined(fr, 2*x, 2*y), o.lined(fr, 2*x+1, 2*y), o.lined(fr, 2*x, 2*y+1), o.lined(fr, 2*x+1, 2*y+1)} {
			if l := c.Luma(); l > threshold {
				sum = sum.Add(c.Scale((l - threshold) / l))
			}
		}
		return sum.Scale(0.25)
	}
	parallelRows(th, func(y0, y1 int) {
		for y := y0; y < y1; y++ {
			if fresh {
				for x := range tw {
					o.raw[y*tw+x] = cell(x, y)
				}
				continue
			}
			for _, py := range [2]int{2 * y, 2*y + 1} {
				o.in.Each(py, func(xa, xb int) {
					for x := xa / 2; x <= xb/2; x++ {
						o.raw[y*tw+x] = cell(x, y)
					}
				})
			}
		}
	})
	top := &p.levels[0]
	parallelRows(th, func(y0, y1 int) {
		copy(top.px[y0*tw:y1*tw], o.raw[y0*tw:y1*tw])
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
	if !all && o.glowW == tw && o.glowH == th && len(o.glow) == tw*th {
		slack := glowSlack * float64(len(p.levels)) / fin.BloomStrength
		same := func(i int) bool {
			a, b := top.px[i], o.glow[i]
			return math.Abs(a.R-b.R) <= slack && math.Abs(a.G-b.G) <= slack && math.Abs(a.B-b.B) <= slack
		}
		for y := range th {
			row := y * tw
			for x := 0; x < tw; {
				if same(row + x) {
					x++
					continue
				}
				end := x
				for end+1 < tw && !same(row+end+1) {
					end++
				}
				copy(o.glow[row+x:row+end+1], top.px[row+x:row+end+1])
				o.dirt.AddRect(2*x-1, 2*y-1, 2*end+2, 2*y+2)
				x = end + 1
			}
		}
		return
	}
	if !all {
		o.dirt.Fill()
	}
	o.glow = append(o.glow[:0], top.px...)
	o.glowW, o.glowH = tw, th
}

// Darkening of every pixel toward the edges, made once per size and strength
func (o *Output) vignette(strength float64) {
	w, h := o.W, o.H
	if strength <= 0 {
		o.vig, o.vigS = o.vig[:0], 0
		return
	}
	if o.vigS == strength && len(o.vig) == w*h {
		return
	}
	o.vigS = strength
	o.vig = slices.Grow(o.vig[:0], w*h)[:w*h]
	parallelRows(h, func(y0, y1 int) {
		for y := y0; y < y1; y++ {
			dy := (float64(y)+0.5)/float64(h)*2 - 1
			for x := range w {
				dx := (float64(x)+0.5)/float64(w)*2 - 1
				o.vig[y*w+x] = float32(1 - strength*Smoothstep((dx*dx+dy*dy*0.7-0.35)/1.3))
			}
		}
	})
}

// Outlined color with the glow added at a pixel
func (o *Output) glowed(fr *Frame, x, y int, k float64) RGB {
	c := fr.Color[y*fr.W+x].Scale(float64(o.fac[y*fr.W+x]))
	if k > 0 && o.glowW > 0 {
		lv := bloomLevel{w: o.glowW, h: o.glowH, px: o.glow}
		c = c.Add(lv.lerp((float64(x)+0.5)/2, (float64(y)+0.5)/2).Scale(k))
	}
	return c
}

// Grades and vignettes every changed pixel into the output, the fringing read from the outlined and glowing colors around it
func (o *Output) finish(fr *Frame, fin Finish) {
	w, h := fr.W, fr.H
	k := 0.0
	if fin.BloomStrength > 0 && len(o.post.levels) > 0 {
		k = fin.BloomStrength / float64(len(o.post.levels))
	}
	g := fin.Grade
	exposure, sat := g.Exposure, g.Saturation
	if exposure == 0 {
		exposure = 1
	}
	if sat == 0 {
		sat = 1
	}
	var pre *Frame
	if g.Aberration > 0 {
		o.pre = slices.Grow(o.pre[:0], w*h)[:w*h]
		parallelRows(h, func(y0, y1 int) {
			for y := y0; y < y1; y++ {
				o.dirt.Each(y, func(xa, xb int) {
					for x := xa; x <= xb; x++ {
						o.pre[y*w+x] = o.glowed(fr, x, y, k)
					}
				})
			}
		})
		pre = &Frame{W: w, H: h, Color: o.pre}
	} else {
		o.pre = o.pre[:0]
	}
	cx, cy := float64(w)/2, float64(h)/2
	o.movedRows = slices.Grow(o.movedRows[:0], h)[:h]
	clear(o.movedRows)
	parallelRows(h, func(y0, y1 int) {
		for y := y0; y < y1; y++ {
			fy := (float64(y) + 0.5 - cy) / cy
			moved := int32(0)
			o.dirt.Each(y, func(xa, xb int) {
				for x := xa; x <= xb; x++ {
					i := y*w + x
					var c RGB
					if pre != nil {
						c = pre.Color[i]
						fx := (float64(x) + 0.5 - cx) / cx
						ab := g.Aberration * (fx*fx + fy*fy)
						ox, oy := fx*ab, fy*ab*fr.PixelAspect
						px, py := float64(x)+0.5, float64(y)+0.5
						c.R = pre.lerpAt(px-ox, py-oy).R
						c.B = pre.lerpAt(px+ox, py+oy).B
					} else {
						c = o.glowed(fr, x, y, k)
					}
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
					if len(o.vig) == w*h {
						if v := o.vig[i]; v < 1 {
							c = c.Scale(float64(v))
						}
					}
					o.Color[i] = c
					if r0, g0, b0 := channel(c.R), channel(c.G), channel(c.B); o.Bytes[i*3] != r0 || o.Bytes[i*3+1] != g0 || o.Bytes[i*3+2] != b0 {
						o.Bytes[i*3], o.Bytes[i*3+1], o.Bytes[i*3+2] = r0, g0, b0
						moved++
					}
				}
			})
			o.movedRows[y] = moved
		}
	})
	for _, m := range o.movedRows {
		o.moved += int(m)
	}
}
