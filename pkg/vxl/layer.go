package vxl

import (
	"math"
	"slices"
)

// Lit frame later flushes draw over, lit once and then only where something changed in front of it
type Layer struct {
	// Surface, depth, facing and lit color of every pixel before mirroring and haze
	fr Frame
	// Color of every pixel once mirrored, hazed and blended, what a restored frame starts from
	full []RGB
	// Point lights the layer was lit by, every other light added over it per frame
	lights []Light
	// Crowding of every cell of the occlusion grid before and after its blur, and the depth it was read at
	aoW, aoH, aoShrink int
	aoZ, aoOcc, aoBlur []float32
	// Depth map the sun was shaded by and how deep toward the sun the lit pixels reach
	shadows *ShadowMap
	sunLo   float64
	// Reflection of every cell of the mirror grid and the pixel each ray hit
	reflW, reflH, reflShrink int
	refl                     []RGB
	reflHit                  []int32
	// Inscattered light along the ray of every haze cell, cumulative at every step
	haze hazeField
	// Changes the last restore put back, the ones of the frame before
	prev Changes
	// Whether a flush has filled the layer
	ok bool
}

// Reports whether the layer holds a lit frame of the size
func (l *Layer) Fits(w, h int) bool { return l.ok && l.fr.W == w && l.fr.H == h }

// Reports whether the layer keeps a crowding grid of the shape of a grid
func (l *Layer) hasAO(lg *lowGrid) bool {
	n := lg.w * lg.h
	return l != nil && l.ok && n > 0 && l.aoW == lg.w && l.aoH == lg.h && l.aoShrink == lg.shrink && len(l.aoOcc) == n && len(l.aoBlur) == n && len(l.aoZ) == n
}

// Writes the finished color of the layer back over the pixels whose color changed and its surfaces over the pixels drawn on, every pixel for nil, which a flush over the layer then draws on
func (l *Layer) Restore(fr *Frame, c *Changes) {
	fr.PixelAspect = l.fr.PixelAspect
	fr.Resize(l.fr.W, l.fr.H)
	w, h := fr.W, fr.H
	if c == nil || c.All() || c.Color.W != w || c.Color.H != h {
		parallelRows(h, func(y0, y1 int) {
			a, b := y0*w, y1*w
			copy(fr.Color[a:b], l.full[a:b])
			copy(fr.Albedo[a:b], l.fr.Albedo[a:b])
			copy(fr.Depth[a:b], l.fr.Depth[a:b])
			copy(fr.Normal[a:b], l.fr.Normal[a:b])
		})
		l.prev.Reset(w, h)
		l.prev.Fill()
		return
	}
	c.Norm()
	parallelRows(h, func(y0, y1 int) {
		for y := y0; y < y1; y++ {
			row := y * w
			c.Color.Each(y, func(xa, xb int) {
				copy(fr.Color[row+xa:row+xb+1], l.full[row+xa:row+xb+1])
			})
			c.Drawn.Each(y, func(xa, xb int) {
				a, b := row+xa, row+xb+1
				copy(fr.Albedo[a:b], l.fr.Albedo[a:b])
				copy(fr.Depth[a:b], l.fr.Depth[a:b])
				copy(fr.Normal[a:b], l.fr.Normal[a:b])
			})
		}
	})
	l.prev.CopyFrom(c)
}

// Keeps the frame as lit so far with the lights and the crowding grid of the flush
func (l *Layer) keep(r *Raster) {
	l.fr.CopyFrom(r.fr)
	l.lights = append(l.lights[:0], r.Lights...)
	lg := &r.aoGrid
	if len(r.ao) == r.fr.W*r.fr.H {
		l.aoW, l.aoH, l.aoShrink = lg.w, lg.h, lg.shrink
		l.aoZ = append(l.aoZ[:0], lg.z...)
		l.aoOcc = append(l.aoOcc[:0], r.aoRaw...)
		l.aoBlur = append(l.aoBlur[:0], r.ao2...)
	} else {
		l.aoW, l.aoH, l.aoShrink = 0, 0, 0
		l.aoZ, l.aoOcc, l.aoBlur = l.aoZ[:0], l.aoOcc[:0], l.aoBlur[:0]
	}
	l.keepShadows(r)
	l.prev.Reset(r.fr.W, r.fr.H)
	l.ok = true
}

// Keeps the depth map the frame was shaded by and the least depth toward its sun of any lit pixel
func (l *Layer) keepShadows(r *Raster) {
	l.shadows, l.sunLo = r.Shadows, math.Inf(1)
	if r.Shadows == nil {
		return
	}
	fr := r.fr
	w, h := fr.W, fr.H
	dir := r.Shadows.dir
	n := min(Workers(), max(h, 1))
	band := (h + n - 1) / n
	lows := make([]float64, n)
	parallel(n, n, func(b int) {
		lo := math.Inf(1)
		for y := b * band; y < min((b+1)*band, h); y++ {
			base := r.forward.Add(r.up.Mul(r.rows[y]))
			for x := range w {
				i := y*w + x
				if fr.Depth[i] == 0 || fr.Normal[i]&normalLit == 0 {
					continue
				}
				p := r.eye.Add(base.Add(r.right.Mul(r.cols[x])).Mul(1 / float64(fr.Depth[i])))
				lo = min(lo, p.Dot(dir))
			}
		}
		lows[b] = lo
	})
	for _, lo := range lows {
		l.sunLo = min(l.sunLo, lo)
	}
}

// Keeps the mirror grid of the flush, none when the flush drew no mirrors
func (l *Layer) keepReflect(r *Raster) {
	lg := &r.reflGrid
	if len(r.refl) != lg.w*lg.h || len(r.refl) == 0 {
		l.reflW, l.reflH, l.reflShrink = 0, 0, 0
		l.refl, l.reflHit = l.refl[:0], l.reflHit[:0]
		return
	}
	l.reflW, l.reflH, l.reflShrink = lg.w, lg.h, lg.shrink
	l.refl = append(l.refl[:0], r.refl...)
	l.reflHit = append(l.reflHit[:0], r.reflHit...)
}

// Keeps the haze the flush marched, once the march is done
func (l *Layer) keepHaze(r *Raster) { l.haze.copyFrom(&r.hz) }

// Haze of the layer when marched on a grid of the shape, nil otherwise
func (l *Layer) hazeUnder(w, h, steps int) *hazeField {
	if l == nil || l.haze.w != w || l.haze.h != h || l.haze.steps != steps {
		return nil
	}
	return &l.haze
}

// Folds the light a flush added over the pixels lit anew into the lit color of the layer and its finished color
func (l *Layer) bakeLight(r *Raster) {
	fr := r.fr
	w := fr.W
	parallelRows(fr.H, func(y0, y1 int) {
		for y := y0; y < y1; y++ {
			row := y * w
			r.ch.Lit.Each(y, func(xa, xb int) {
				for i := row + xa; i <= row+xb; i++ {
					c := fr.Color[i]
					l.fr.Color[i] = l.fr.Color[i].Add(c).Sub(l.full[i])
					l.full[i] = c
				}
			})
		}
	})
}

// Adds the light a flush marched near the lights the layer lacked or lost into its haze, cell by cell along the same steps
func (l *Layer) bakeHaze(r *Raster) {
	f := &r.hz
	u := l.hazeUnder(f.w, f.h, f.steps)
	if u == nil {
		return
	}
	for cell, on := range f.on {
		if !on {
			continue
		}
		if !u.on[cell] {
			u.on[cell], u.t0[cell], u.step[cell] = true, f.t0[cell], f.step[cell]
			clear(u.acc[cell*f.steps : (cell+1)*f.steps])
		}
		a, b := u.acc[cell*f.steps:(cell+1)*f.steps], f.acc[cell*f.steps:(cell+1)*f.steps]
		for k := range a {
			a[k] = rgb32{a[k][0] + b[k][0], a[k][1] + b[k][1], a[k][2] + b[k][2]}
		}
	}
}

// Keeps the color of every pixel a flush changed as the finished color of the layer, its lights as the ones the layer was lit by, with nothing left to put back
func (l *Layer) relit(r *Raster) {
	fr := r.fr
	w := fr.W
	parallelRows(fr.H, func(y0, y1 int) {
		for y := y0; y < y1; y++ {
			row := y * w
			r.ch.Color.Each(y, func(xa, xb int) {
				copy(l.full[row+xa:row+xb+1], fr.Color[row+xa:row+xb+1])
			})
		}
	})
	l.lights = append(l.lights[:0], r.Lights...)
	l.prev.Reset(fr.W, fr.H)
}

// Keeps the color of every pixel once the flush is done
func (l *Layer) keepFull(r *Raster) {
	n := r.fr.W * r.fr.H
	l.full = slices.Grow(l.full[:0], n)[:n]
	parallelRows(r.fr.H, func(y0, y1 int) {
		copy(l.full[y0*r.fr.W:y1*r.fr.W], r.fr.Color[y0*r.fr.W:y1*r.fr.W])
	})
}

// Reports whether a light of the flush was among the lights the layer was lit by, never for the negative of a lost one
func (l *Layer) has(s *lightSpan) bool {
	if l == nil || s.remove {
		return false
	}
	return slices.ContainsFunc(l.lights, func(k Light) bool { return k.Pos == s.pos && k.Color == s.col && k.Radius == s.radius })
}
