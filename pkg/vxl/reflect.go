package vxl

import (
	"math"
	"slices"
)

// Mirror images glossy faces show of what the frame holds, found by marching the mirrored view ray through the depth buffer
type Reflection struct {
	// Share of the mirrored light a face of full gloss adds, none at zero
	Strength float64
	// Steps a ray marches and the world distance it covers
	Steps    int
	Distance float64
	// Color a ray that leaves the frame or finds nothing mirrors, black for none
	Sky RGB
	// Pixels across and down each cell of the grid the rays are marched on, one pixel at zero
	Shrink int
}

// Share of the frame edge over which a mirrored hit fades out
const reflectEdge = 0.1

// Mirrored light change under which a cell keeps the reflection of the layer, a quarter step of 255
const reflectStill = 1.0 / 1024

// Marches the reflection of every glossy pixel on the grid, over a layer only where the frame changed since the layer was lit, spreads it back and adds it to the lit color
func (r *Raster) reflect(workers int, l *Layer) {
	fr := r.fr
	w, h := fr.W, fr.H
	lg := &r.reflGrid
	lg.fit(w, h, r.Reflect.Shrink)
	n := lg.w * lg.h
	r.refl = slices.Grow(r.refl[:0], n)[:n]
	r.reflHit = slices.Grow(r.reflHit[:0], n)[:n]
	steps := max(r.Reflect.Steps, 4)
	dist := r.Reflect.Distance
	if !(dist > 0) {
		dist = 20
	}
	D, N, C := fr.Depth, fr.Normal, fr.Color
	fw, fh := float64(w), float64(h)
	k := r.Reflect.Strength
	cached := l != nil && l.reflW == lg.w && l.reflH == lg.h && l.reflShrink == lg.shrink && len(l.refl) == n
	var stale *Dirt
	var sx0, sy0, sx1, sy1 int
	if cached {
		stale = &r.stale
		stale.CopyFrom(&r.ch.Lit)
		stale.Union(&l.prev.Lit)
		stale.Norm()
		var any bool
		if sx0, sy0, sx1, sy1, any = stale.Bounds(); !any {
			sx0, sy0, sx1, sy1 = 1, 1, 0, 0
		}
	}
	lit := func(j int) RGB {
		if l != nil && r.stamp[j] != r.gen {
			return l.fr.Color[j].Add(C[j]).Sub(l.full[j])
		}
		return C[j]
	}
	parallelBandsWith(lg.h, 2, workers, func(y0, y1 int) {
		for ly := y0; ly < y1; ly++ {
			for lx := range lg.w {
				li := ly*lg.w + lx
				x, y := lg.pixel(lx, ly, w, h)
				i := y*w + x
				gloss := unpackGloss(N[i])
				if D[i] == 0 {
					lg.z[li], r.refl[li], r.reflHit[li] = 0, RGB{}, -1
					continue
				}
				z := 1 / float64(D[i])
				lg.z[li] = float32(z)
				if gloss == 0 {
					r.refl[li], r.reflHit[li] = RGB{}, -1
					continue
				}
				if cached && !stale.Has(x, y) {
					hit := int(l.reflHit[li])
					if hit < 0 || !stale.Has(hit%w, hit/w) && !crosses(stale, sx0, sy0, sx1, sy1, x, y, hit%w, hit/w) {
						r.refl[li], r.reflHit[li] = l.refl[li], l.reflHit[li]
						continue
					}
				}
				p := Vec3{r.cols[x] * z, r.rows[y] * z, z}
				nw := unpackNormal(N[i])
				nv := Vec3{nw.Dot(r.right), nw.Dot(r.up), nw.Dot(r.forward)}
				view := p.Norm()
				cos := -view.Dot(nv)
				if cos <= 0 {
					r.refl[li], r.reflHit[li] = RGB{}, -1
					continue
				}
				dir := view.Sub(nv.Mul(2 * view.Dot(nv)))
				f := 1 - cos
				f *= f
				f *= f
				f *= 1 - cos
				weight := gloss * (0.15 + 0.85*f)
				step := dist / float64(steps)
				t0 := step * (0.25 + 0.5*Hash2(lx*3+1, ly*5+2))
				hit, miss, found := RGB{}, true, int32(-1)
				prev := 0.0
				for s := range steps {
					t := t0 + step*float64(s)
					q := p.Add(dir.Mul(t))
					if q.Z < nearPlane {
						break
					}
					sx, sy, _ := r.screen(q)
					if sx < 0 || sy < 0 || sx >= fw || sy >= fh {
						break
					}
					j := int(sy)*w + int(sx)
					if D[j] == 0 {
						prev = t
						continue
					}
					zs := 1 / float64(D[j])
					if q.Z > zs && q.Z-zs < step*1.5+zs*0.02 {
						lo, hi := prev, t
						for range 4 {
							mid := (lo + hi) / 2
							m := p.Add(dir.Mul(mid))
							mx, my, _ := r.screen(m)
							k := int(my)*w + int(mx)
							if mx < 0 || my < 0 || mx >= fw || my >= fh || D[k] == 0 || m.Z < 1/float64(D[k]) {
								lo = mid
							} else {
								hi, j, sx, sy = mid, k, mx, my
							}
						}
						edge := min(min(sx, fw-sx)/(fw*reflectEdge), min(sy, fh-sy)/(fh*reflectEdge), 1)
						hit, miss, found = lit(j).Scale(weight*edge*(1-hi/dist*0.5)), false, int32(j)
						break
					}
					prev = t
				}
				if miss {
					hit = r.Reflect.Sky.Scale(weight)
				}
				r.refl[li], r.reflHit[li] = hit, found
			}
		}
	})
	if l == nil {
		parallelBandsWith(h, 4, workers, func(y0, y1 int) {
			for y := y0; y < y1; y++ {
				for x := range w {
					i := y*w + x
					if D[i] == 0 || unpackGloss(N[i]) == 0 {
						continue
					}
					C[i] = C[i].Add(r.spreadReflect(lg, x, y, i, nil).Scale(k))
				}
			}
		})
		return
	}
	var old []RGB
	if cached {
		old = l.refl
	}
	r.reflShift = slices.Grow(r.reflShift[:0], n)[:n]
	r.reflMoved = slices.Grow(r.reflMoved[:0], n)[:n]
	clear(r.reflMoved)
	still := reflectStill / k
	for li := range n {
		d := r.refl[li]
		if old != nil {
			d = d.Sub(old[li])
		}
		r.reflShift[li] = math.Abs(d.R) > still || math.Abs(d.G) > still || math.Abs(d.B) > still
	}
	for ly := range lg.h {
		for lx := range lg.w {
			if !r.reflShift[ly*lg.w+lx] {
				continue
			}
			for y := max(ly-1, 0); y <= min(ly+1, lg.h-1); y++ {
				for x := max(lx-1, 0); x <= min(lx+1, lg.w-1); x++ {
					r.reflMoved[y*lg.w+x] = true
				}
			}
		}
	}
	markCells(&r.ch.Color, r.reflMoved, lg.w, lg.h, lg.shrink)
	r.ch.Color.Norm()
	parallelBandsWith(h, 4, workers, func(y0, y1 int) {
		for y := y0; y < y1; y++ {
			r.ch.Color.Each(y, func(xa, xb int) {
				for x := xa; x <= xb; x++ {
					i := y*w + x
					if D[i] == 0 || unpackGloss(N[i]) == 0 {
						continue
					}
					if r.stamp[i] == r.gen {
						C[i] = C[i].Add(r.spreadReflect(lg, x, y, i, nil).Scale(k))
						continue
					}
					if !r.reflMoved[lg.cell(x, y)] {
						continue
					}
					if c := r.spreadReflect(lg, x, y, i, old); c != (RGB{}) {
						C[i] = C[i].Add(c.Scale(k))
					}
				}
			})
		}
	})
}

// Reflection at a pixel blended from the cells around it, less the reflection the same cells held before when given
func (r *Raster) spreadReflect(lg *lowGrid, x, y, i int, old []RGB) RGB {
	if lg.shrink == 1 {
		c := r.refl[i]
		if old != nil {
			c = c.Sub(old[i])
		}
		return c
	}
	cells, weights := lg.around(x, y, 1/float64(r.fr.Depth[i]), lg.z)
	var acc RGB
	wt := 0.0
	for m := range 4 {
		c := r.refl[cells[m]]
		if old != nil {
			c = c.Sub(old[cells[m]])
		}
		acc = acc.Add(c.Scale(weights[m]))
		wt += weights[m]
	}
	if wt > 0 {
		return acc.Scale(1 / wt)
	}
	return RGB{}
}

// Reports whether the straight line between two pixels passes over a changed pixel, the line sampled every few pixels within the box around the changes
func crosses(d *Dirt, bx0, by0, bx1, by1, x0, y0, x1, y1 int) bool {
	if bx0 > bx1 || max(x0, x1) < bx0 || min(x0, x1) > bx1 || max(y0, y1) < by0 || min(y0, y1) > by1 {
		return false
	}
	n := max(max(x1-x0, x0-x1), max(y1-y0, y0-y1))/reflectSample + 1
	for k := range n + 1 {
		t := float64(k) / float64(n)
		x := x0 + int(math.Round(float64(x1-x0)*t))
		y := y0 + int(math.Round(float64(y1-y0)*t))
		if d.Has(x, y) {
			return true
		}
	}
	return false
}

// Pixels between the samples a mirrored ray is checked at for changes along its way
const reflectSample = 4
