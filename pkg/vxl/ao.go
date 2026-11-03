package vxl

import (
	"math"
	"slices"
)

// Crowding read off the depth buffer, darkening the ambient light of surfaces hemmed in by nearer ones
type Occlusion struct {
	// World units the crowding is looked for within
	Radius float64
	// Share of the ambient light a fully crowded surface loses, none at zero
	Strength float64
	// Depth samples taken around every pixel, at least four
	Samples int
	// Pixels across and down each cell of the grid the crowding is read on, one pixel at zero
	Shrink int
}

// Turns of the sample spiral around each pixel
const aoTurns = 3.0

// Share of the depth a sample must rise above the plane through the pixel to count, against depth rounding
const aoBias = 0.004

// Share of the depth two pixels may differ by and still blur together
const aoBlurDepth = 0.08

// Turns of the spiral the sample table holds, each a whole set of samples
const aoRotations = 16

// Most samples the table holds per pixel
const aoMaxSamples = 32

// Cells the blur of the crowding grid reaches on either side
const aoBlurReach = 2

// Offsets of every sample of every turn, as the cosine, the sine and the root of the share along the spiral
var aoTable = func() (out [aoRotations][aoMaxSamples][3]float64) {
	for k := range aoRotations {
		for s := range aoMaxSamples {
			t := (float64(s) + 0.5) / aoMaxSamples
			sn, cs := math.Sincos((float64(k)/aoRotations + t*aoTurns) * 2 * math.Pi)
			out[k][s] = [3]float64{cs, sn, math.Sqrt(t)}
		}
	}
	return out
}()

// Grid of one value per block of pixels, spread back over the pixels by likeness of depth
type lowGrid struct {
	w, h, shrink int
	z            []float32
}

// Grid over a frame at a shrink, every block read at its middle pixel
func (lg *lowGrid) fit(w, h, shrink int) {
	shrink = max(shrink, 1)
	lg.w, lg.h, lg.shrink = (w+shrink-1)/shrink, (h+shrink-1)/shrink, shrink
	lg.z = slices.Grow(lg.z[:0], lg.w*lg.h)[:lg.w*lg.h]
}

// Pixel a grid cell is read at
func (lg *lowGrid) pixel(lx, ly, w, h int) (int, int) {
	return min(lx*lg.shrink+lg.shrink/2, w-1), min(ly*lg.shrink+lg.shrink/2, h-1)
}

// Cell holding a pixel
func (lg *lowGrid) cell(x, y int) int { return (y/lg.shrink)*lg.w + x/lg.shrink }

// Four grid cells around a pixel with the weight of each, bilinear by place and shrunk by any gap in depth
func (lg *lowGrid) around(x, y int, z float64) (cells [4]int, weights [4]float64) {
	fx := (float64(x)+0.5)/float64(lg.shrink) - 0.5
	fy := (float64(y)+0.5)/float64(lg.shrink) - 0.5
	lx0 := min(max(int(math.Floor(fx)), 0), lg.w-1)
	ly0 := min(max(int(math.Floor(fy)), 0), lg.h-1)
	lx1, ly1 := min(lx0+1, lg.w-1), min(ly0+1, lg.h-1)
	tx := min(max(fx-float64(lx0), 0), 1)
	ty := min(max(fy-float64(ly0), 0), 1)
	cells = [4]int{ly0*lg.w + lx0, ly0*lg.w + lx1, ly1*lg.w + lx0, ly1*lg.w + lx1}
	weights = [4]float64{(1 - tx) * (1 - ty), tx * (1 - ty), (1 - tx) * ty, tx * ty}
	for k := range 4 {
		if weights[k] <= 0 {
			continue
		}
		dz := math.Abs(float64(lg.z[cells[k]]) - z)
		weights[k] /= 1 + dz/(aoBlurDepth*z+1e-6)
	}
	return cells, weights
}

// Marks the cells of the occlusion grid holding a pixel this flush drew, widened by the sample reach and the blur, every cell without a layer
func (r *Raster) markRedo(l *Layer) {
	fr := r.fr
	w, h := fr.W, fr.H
	lg := &r.aoGrid
	lg.fit(w, h, r.AO.Shrink)
	n := lg.w * lg.h
	r.redo = slices.Grow(r.redo[:0], n)[:n]
	if l == nil || !l.ok || l.aoW != lg.w || l.aoH != lg.h || l.aoShrink != lg.shrink || len(l.aoOcc) != n {
		for i := range r.redo {
			r.redo[i] = true
		}
		return
	}
	clear(r.redo)
	r.dirty = r.dirty[:0]
	for y := range h {
		row := y * w
		for x := range w {
			if r.stamp[row+x] != r.gen {
				continue
			}
			if c := lg.cell(x, y); !r.redo[c] {
				r.redo[c] = true
				r.dirty = append(r.dirty, int32(c))
			}
		}
	}
	if r.AO.Strength <= 0 {
		return
	}
	sx, sy := r.fx*0.5*float64(w), r.fy*0.5*float64(h)
	D := fr.Depth
	for _, c := range r.dirty {
		lx, ly := int(c)%lg.w, int(c)/lg.w
		px, py := lg.pixel(lx, ly, w, h)
		z := 1.0
		if d := D[py*w+px]; d > 0 {
			z = 1 / float64(d)
		}
		reach := int(math.Ceil(r.AO.Radius*max(sx, sy)/z/float64(lg.shrink))) + aoBlurReach
		for dy := -reach; dy <= reach; dy++ {
			ry := ly + dy
			if ry < 0 || ry >= lg.h {
				continue
			}
			for dx := -reach; dx <= reach; dx++ {
				if rx := lx + dx; rx >= 0 && rx < lg.w {
					r.redo[ry*lg.w+rx] = true
				}
			}
		}
	}
}

// Writes the occlusion of every opaque lit pixel, read on the marked cells of the grid, the rest taken from the layer, and spread back over the pixels
func (r *Raster) occlude(workers int, l *Layer) {
	fr := r.fr
	w, h := fr.W, fr.H
	lg := &r.aoGrid
	n := lg.w * lg.h
	r.ao = slices.Grow(r.ao[:0], w*h)[:w*h]
	r.aoRaw = slices.Grow(r.aoRaw[:0], n)[:n]
	r.ao2 = slices.Grow(r.ao2[:0], n)[:n]
	r.ao3 = slices.Grow(r.ao3[:0], n)[:n]
	samples := min(max(r.AO.Samples, 4), aoMaxSamples)
	stride := aoMaxSamples / samples
	radius, strength := r.AO.Radius, r.AO.Strength
	D, N := fr.Depth, fr.Normal
	sx, sy := r.fx*0.5*float64(w), r.fy*0.5*float64(h)
	r2 := radius * radius * 4
	parallelBandsWith(lg.h, 2, workers, func(y0, y1 int) {
		for ly := y0; ly < y1; ly++ {
			for lx := range lg.w {
				li := ly*lg.w + lx
				if !r.redo[li] {
					lg.z[li], r.aoRaw[li] = l.aoZ[li], l.aoOcc[li]
					continue
				}
				x, y := lg.pixel(lx, ly, w, h)
				i := y*w + x
				d := D[i]
				if d == 0 {
					lg.z[li], r.aoRaw[li] = 0, 1
					continue
				}
				z := 1 / float64(d)
				lg.z[li] = float32(z)
				if N[i]&normalLit == 0 {
					r.aoRaw[li] = 1
					continue
				}
				rpx, rpy := radius*sx/z, radius*sy/z
				if rpx < 0.75 && rpy < 0.75 {
					r.aoRaw[li] = 1
					continue
				}
				p := Vec3{r.cols[x] * z, r.rows[y] * z, z}
				nw := unpackNormal(N[i])
				nv := Vec3{nw.Dot(r.right), nw.Dot(r.up), nw.Dot(r.forward)}
				table := &aoTable[int(Hash2(lx, ly)*aoRotations)%aoRotations]
				occ := 0.0
				for s := range samples {
					o := &table[s*stride]
					qx, qy := x+int(math.Floor(o[0]*o[2]*rpx+0.5)), y+int(math.Floor(o[1]*o[2]*rpy+0.5))
					if qx < 0 || qy < 0 || qx >= w || qy >= h || qx == x && qy == y {
						continue
					}
					j := qy*w + qx
					dq := D[j]
					if dq == 0 {
						continue
					}
					zq := 1 / float64(dq)
					v := Vec3{r.cols[qx]*zq - p.X, r.rows[qy]*zq - p.Y, zq - p.Z}
					vv := v.Dot(v)
					if vv >= r2 {
						continue
					}
					occ += max(v.Dot(nv)-aoBias*z, 0) / (vv + 1e-6) * radius * (1 - vv/r2)
				}
				r.aoRaw[li] = float32(max(1-strength*occ/float64(samples), 0))
			}
		}
	})
	parallelBandsWith(lg.h, 2, workers, func(y0, y1 int) {
		for ly := y0; ly < y1; ly++ {
			for lx := range lg.w {
				li := ly*lg.w + lx
				r.ao3[li] = blurAO(r.aoRaw, lg.z, li, 1, lx, lg.w-1-lx)
			}
		}
	})
	parallelBandsWith(lg.h, 2, workers, func(y0, y1 int) {
		for ly := y0; ly < y1; ly++ {
			for lx := range lg.w {
				li := ly*lg.w + lx
				r.ao2[li] = blurAO(r.ao3, lg.z, li, lg.w, ly, lg.h-1-ly)
			}
		}
	})
	parallelBandsWith(h, 4, workers, func(y0, y1 int) {
		for y := y0; y < y1; y++ {
			for x := range w {
				i := y*w + x
				if D[i] == 0 || N[i]&normalLit == 0 || !r.redo[lg.cell(x, y)] {
					r.ao[i] = 1
					continue
				}
				if lg.shrink == 1 {
					r.ao[i] = r.ao2[i]
					continue
				}
				cells, weights := lg.around(x, y, 1/float64(D[i]))
				sum, wt := 0.0, 0.0
				for k := range 4 {
					sum += float64(r.ao2[cells[k]]) * weights[k]
					wt += weights[k]
				}
				r.ao[i] = float32(sum / wt)
			}
		}
	})
}

// Mean of the occlusion over five cells along a stride, taken only over the ones at a like depth
func blurAO(src []float32, Z []float32, i, stride, before, after int) float32 {
	z := Z[i]
	if z == 0 {
		return src[i]
	}
	sum, wt := src[i]*2, float32(2)
	for k := 1; k <= aoBlurReach; k++ {
		if k <= before {
			if j := i - k*stride; Z[j] != 0 && math.Abs(float64(Z[j]-z)) < aoBlurDepth*float64(z) {
				sum += src[j]
				wt++
			}
		}
		if k <= after {
			if j := i + k*stride; Z[j] != 0 && math.Abs(float64(Z[j]-z)) < aoBlurDepth*float64(z) {
				sum += src[j]
				wt++
			}
		}
	}
	return sum / wt
}
