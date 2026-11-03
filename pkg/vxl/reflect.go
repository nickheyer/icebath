package vxl

import (
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

// Marches the reflection of every glossy pixel on the grid, spreads it back and adds it to the lit color
func (r *Raster) reflect(workers int) {
	fr := r.fr
	w, h := fr.W, fr.H
	lg := &r.reflGrid
	lg.fit(w, h, r.Reflect.Shrink)
	n := lg.w * lg.h
	r.refl = slices.Grow(r.refl[:0], n)[:n]
	steps := max(r.Reflect.Steps, 4)
	dist := r.Reflect.Distance
	if !(dist > 0) {
		dist = 20
	}
	D, N, C := fr.Depth, fr.Normal, fr.Color
	fw, fh := float64(w), float64(h)
	parallelBandsWith(lg.h, 2, workers, func(y0, y1 int) {
		for ly := y0; ly < y1; ly++ {
			for lx := range lg.w {
				li := ly*lg.w + lx
				x, y := lg.pixel(lx, ly, w, h)
				i := y*w + x
				gloss := unpackGloss(N[i])
				if D[i] == 0 {
					lg.z[li], r.refl[li] = 0, RGB{}
					continue
				}
				z := 1 / float64(D[i])
				lg.z[li] = float32(z)
				if gloss == 0 {
					r.refl[li] = RGB{}
					continue
				}
				p := Vec3{r.cols[x] * z, r.rows[y] * z, z}
				nw := unpackNormal(N[i])
				nv := Vec3{nw.Dot(r.right), nw.Dot(r.up), nw.Dot(r.forward)}
				view := p.Norm()
				cos := -view.Dot(nv)
				if cos <= 0 {
					r.refl[li] = RGB{}
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
				hit, miss := RGB{}, true
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
						hit, miss = C[j].Scale(weight*edge*(1-hi/dist*0.5)), false
						break
					}
					prev = t
				}
				if miss {
					hit = r.Reflect.Sky.Scale(weight)
				}
				r.refl[li] = hit
			}
		}
	})
	k := r.Reflect.Strength
	parallelBandsWith(h, 4, workers, func(y0, y1 int) {
		for y := y0; y < y1; y++ {
			for x := range w {
				i := y*w + x
				if D[i] == 0 || unpackGloss(N[i]) == 0 {
					continue
				}
				if lg.shrink == 1 {
					if c := r.refl[i]; c != (RGB{}) {
						C[i] = C[i].Add(c.Scale(k))
					}
					continue
				}
				cells, weights := lg.around(x, y, 1/float64(D[i]))
				var acc RGB
				wt := 0.0
				for m := range 4 {
					acc = acc.Add(r.refl[cells[m]].Scale(weights[m]))
					wt += weights[m]
				}
				if wt > 0 {
					C[i] = C[i].Add(acc.Scale(k / wt))
				}
			}
		}
	})
}
