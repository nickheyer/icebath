package vxl

import (
	"math"
	"slices"
)

// Light the air scatters toward the camera, marched at a shrunken resolution and laid over the frame by depth
type Haze struct {
	// Share of light the air scatters per world unit, none at zero
	Density float64
	// Samples taken along every ray, at least four
	Steps int
	// Pixels across and down each cell of the shrunken grid one ray stands for
	Shrink int
	// Farthest world distance a ray marches
	Reach float64
	// Glow of the air everywhere, the share of the baked radiance, the point lights and the sun it takes
	Color   RGB
	Ambient float64
	Lights  float64
	Sun     float64
	// Forward scattering of the sun, from none at zero toward a tight halo near one
	Forward float64
}

// Inscattered light along the ray of every cell of a shrunken grid, cumulative after every step
type hazeField struct {
	w, h, shrink, steps int
	// Distance the march of each cell started at and the distance between its steps
	t0, step []float32
	// Light gathered by the end of each step of each cell, cell major
	acc []rgb32
	// Whether a cell was marched
	on []bool
}

// Sizes the field for a grid, every cell off
func (f *hazeField) fit(w, h, shrink, steps int) {
	f.w, f.h, f.shrink, f.steps = w, h, shrink, steps
	n := w * h
	f.t0 = slices.Grow(f.t0[:0], n)[:n]
	f.step = slices.Grow(f.step[:0], n)[:n]
	f.acc = slices.Grow(f.acc[:0], n*steps)[:n*steps]
	f.on = slices.Grow(f.on[:0], n)[:n]
	clear(f.on)
}

// Takes the size and every cell of another field
func (f *hazeField) copyFrom(o *hazeField) {
	f.w, f.h, f.shrink, f.steps = o.w, o.h, o.shrink, o.steps
	f.t0 = append(f.t0[:0], o.t0...)
	f.step = append(f.step[:0], o.step...)
	f.acc = append(f.acc[:0], o.acc...)
	f.on = append(f.on[:0], o.on...)
}

// Light gathered along the ray of a cell up to a distance, between the steps on either side of it
func (f *hazeField) at(cell int, z float64) RGB {
	if !f.on[cell] {
		return RGB{}
	}
	u := (z - float64(f.t0[cell])) / float64(f.step[cell])
	if u <= 0 {
		return RGB{}
	}
	base := cell * f.steps
	i := int(u)
	if i >= f.steps {
		c := f.acc[base+f.steps-1]
		return RGB{float64(c[0]), float64(c[1]), float64(c[2])}
	}
	t := u - float64(i)
	var a rgb32
	if i > 0 {
		a = f.acc[base+i-1]
	}
	b := f.acc[base+i]
	return RGB{float64(a[0]) + (float64(b[0])-float64(a[0]))*t, float64(a[1]) + (float64(b[1])-float64(a[1]))*t, float64(a[2]) + (float64(b[2])-float64(a[2]))*t}
}

// Distance the march of a cell reached
func (f *hazeField) end(cell int) float64 {
	return float64(f.t0[cell]) + float64(f.step[cell])*float64(f.steps)
}

// Marches the haze of every cell, or over a layer only the cells near the lights the layer lacks, then adds it by depth to every pixel without a layer, else to the changed pixels, the ones not lit anew taking only the fresh march
func (r *Raster) scatter(workers int, l *Layer, dyn []bool) {
	fr := r.fr
	w, h := fr.W, fr.H
	hz := r.Haze
	shrink := max(hz.Shrink, 1)
	steps := max(hz.Steps, 4)
	reach := hz.Reach
	if !(reach > 0) {
		reach = r.FogFar
	}
	if !(reach > 0) {
		reach = 100
	}
	lw, lh := (w+shrink-1)/shrink, (h+shrink-1)/shrink
	r.hz.fit(lw, lh, shrink, steps)
	lights := make([]lightSpan, 0, len(r.spans))
	for k := range r.spans {
		s := &r.spans[k]
		if s.radius <= 0 || !dyn[k] {
			continue
		}
		lights = append(lights, lightSpan{pos: s.pos, col: s.col.Scale(hz.Lights), radius: s.radius, r2: s.r2})
	}
	base := l == nil
	sm, gi := r.Shadows, r.GI
	sunOn := base && hz.Sun > 0 && r.SunColor != RGB{}
	g := min(max(hz.Forward, 0), 0.95)
	phaseK := (1 - g*g)
	D := fr.Depth
	field := &r.hz
	parallelBandsWith(lh, 1, workers, func(y0, y1 int) {
		var nearBuf [64]int32
		nearLights := nearBuf[:0]
		if len(lights) > len(nearBuf) {
			nearLights = make([]int32, 0, len(lights))
		}
		for ly := y0; ly < y1; ly++ {
			for lx := range lw {
				cell := ly*lw + lx
				px, py := min(lx*shrink+shrink/2, w-1), min(ly*shrink+shrink/2, h-1)
				dir := r.forward.Add(r.right.Mul(r.cols[px])).Add(r.up.Mul(r.rows[py]))
				along := dir.Len()
				dir = dir.Mul(1 / along)
				end := reach
				if d := D[py*w+px]; d > 0 {
					end = min(along/float64(d), reach)
				}
				near := nearLights[:0]
				for k := range lights {
					s := &lights[k]
					rel := s.pos.Sub(r.eye)
					t := min(max(rel.Dot(dir), 0), end)
					if off := rel.Sub(dir.Mul(t)); off.Dot(off) < s.r2 {
						near = append(near, int32(k))
					}
				}
				if !base && len(near) == 0 {
					continue
				}
				step := end / float64(steps)
				t := step * Hash2(lx, ly)
				field.t0[cell], field.step[cell], field.on[cell] = float32(t), float32(step), true
				var acc RGB
				trans := 1.0
				fall := math.Exp(-hz.Density * step)
				cosSun := dir.Dot(r.Sun)
				phase := 1.0
				if sunOn && g > 0 {
					phase = phaseK / math.Pow(1+g*g-2*g*cosSun, 1.5)
				}
				for k := range steps {
					p := r.eye.Add(dir.Mul(t))
					var lt RGB
					if base {
						lt = hz.Color
						if gi != nil && hz.Ambient > 0 {
							lt = lt.Add(gi.Sample(p).Scale(hz.Ambient))
						}
					}
					for _, k := range near {
						s := &lights[k]
						d := s.pos.Sub(p)
						d2 := d.Dot(d)
						if d2 >= s.r2 {
							continue
						}
						att := 1 - math.Sqrt(d2)/s.radius
						lt = lt.Add(s.col.Scale(att * att))
					}
					if sunOn {
						lit := 1.0
						if sm != nil {
							lit = sm.Lit(p, Vec3{})
						}
						if lit > 0 {
							lt = lt.Add(r.SunColor.Scale(hz.Sun * lit * phase))
						}
					}
					acc = acc.Add(lt.Scale(trans * (1 - fall)))
					trans *= fall
					t += step
					field.acc[cell*steps+k] = rgb32{float32(acc.R), float32(acc.G), float32(acc.B)}
				}
			}
		}
	})
	under := l.hazeUnder(lw, lh, steps)
	if l != nil {
		markCells(&r.ch.Color, field.on, lw, lh, shrink)
		r.ch.Color.Norm()
	}
	C := fr.Color
	parallelBandsWith(h, 8, workers, func(y0, y1 int) {
		for y := y0; y < y1; y++ {
			fy := (float64(y)+0.5)/float64(shrink) - 0.5
			ly0 := min(max(int(math.Floor(fy)), 0), lh-1)
			ly1 := min(ly0+1, lh-1)
			ty := min(max(fy-float64(ly0), 0), 1)
			r.ch.Color.Each(y, func(xa, xb int) {
				for x := xa; x <= xb; x++ {
					i := y*w + x
					fx := (float64(x)+0.5)/float64(shrink) - 0.5
					lx0 := min(max(int(math.Floor(fx)), 0), lw-1)
					lx1 := min(lx0+1, lw-1)
					tx := min(max(fx-float64(lx0), 0), 1)
					if l != nil && r.stamp[i] != r.gen {
						if field.on[ly0*lw+lx0] || field.on[ly0*lw+lx1] || field.on[ly1*lw+lx0] || field.on[ly1*lw+lx1] {
							C[i] = C[i].Add(r.hazeAt(field, under, false, x, y, lx0, lx1, ly0, ly1, tx, ty, reach))
						}
						continue
					}
					C[i] = C[i].Add(r.hazeAt(field, under, true, x, y, lx0, lx1, ly0, ly1, tx, ty, reach))
				}
			})
		}
	})
}

// Haze at a pixel, blended from the four cells around it at the depth of the pixel, the layer cells under the frame cells, the layer only lending its reach when told
func (r *Raster) hazeAt(field, under *hazeField, addUnder bool, x, y, lx0, lx1, ly0, ly1 int, tx, ty, reach float64) RGB {
	lw := field.w
	i := y*r.fr.W + x
	along := r.forward.Add(r.right.Mul(r.cols[x])).Add(r.up.Mul(r.rows[y])).Len()
	z := reach
	if d := r.fr.Depth[i]; d > 0 {
		z = min(along/float64(d), reach)
	}
	var acc RGB
	wt := 0.0
	for k := range 4 {
		lx, ly, bw := lx0, ly0, (1-tx)*(1-ty)
		switch k {
		case 1:
			lx, bw = lx1, tx*(1-ty)
		case 2:
			ly, bw = ly1, (1-tx)*ty
		case 3:
			lx, ly, bw = lx1, ly1, tx*ty
		}
		if bw <= 0 {
			continue
		}
		j := ly*lw + lx
		var c RGB
		short := math.Inf(1)
		switch {
		case under != nil:
			short = under.end(j)
			if addUnder {
				c = under.at(j, z)
			}
		case field.on[j]:
			short = field.end(j)
		}
		if field.on[j] {
			c = c.Add(field.at(j, z))
		}
		if dz := z - short; dz > 0 {
			bw /= 1 + dz/(0.1*z+0.5)
		}
		acc = acc.Add(c.Scale(bw))
		wt += bw
	}
	if wt > 0 {
		return acc.Scale(1 / wt)
	}
	return RGB{}
}
