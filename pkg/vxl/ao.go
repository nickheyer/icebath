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

// Cells past its reach a cell may still sample, against the rounding of sample offsets and cell centers
const aoSlack = 1

// Blurred crowding change under which a cell keeps the crowding of the layer, under one step of 255
const aoStill = 1.0 / 1024

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

// Turn of the sample table a cell of the grid reads
func aoTurn(lx, ly int) *[aoMaxSamples][3]float64 {
	return &aoTable[int(Hash2(lx, ly)*aoRotations)%aoRotations]
}

// Pixel a sample offset lands on around a pixel at a reach across and down
func aoSampleAt(o *[3]float64, x, y int, rpx, rpy float64) (int, int) {
	return x + int(math.Floor(o[0]*o[2]*rpx+0.5)), y + int(math.Floor(o[1]*o[2]*rpy+0.5))
}

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

// Four cells around a pixel weighted by place and by the gap between its depth and theirs
func (lg *lowGrid) around(x, y int, z float64, Z []float32) (cells [4]int, weights [4]float64) {
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
		dz := math.Abs(float64(Z[cells[k]]) - z)
		weights[k] /= 1 + dz/(aoBlurDepth*z+1e-6)
	}
	return cells, weights
}

// Grid value at a pixel blended from the cells around it, the cell itself at no shrink
func (lg *lowGrid) spread(vals, Z []float32, x, y, i int, z float64) float32 {
	if lg.shrink == 1 {
		return vals[i]
	}
	cells, weights := lg.around(x, y, z, Z)
	sum, wt := 0.0, 0.0
	for k := range 4 {
		sum += float64(vals[cells[k]]) * weights[k]
		wt += weights[k]
	}
	return float32(sum / wt)
}

// Marks the cells holding a drawn pixel and the ones sampling such a cell, every cell without a layer grid
func (r *Raster) markRedo(l *Layer) {
	fr := r.fr
	w, h := fr.W, fr.H
	lg := &r.aoGrid
	lg.fit(w, h, r.AO.Shrink)
	n := lg.w * lg.h
	r.redo = slices.Grow(r.redo[:0], n)[:n]
	if !l.hasAO(lg) {
		for i := range r.redo {
			r.redo[i] = true
		}
		return
	}
	clear(r.redo)
	r.aoDirty = slices.Grow(r.aoDirty[:0], n)[:n]
	clear(r.aoDirty)
	r.aoNearZ = slices.Grow(r.aoNearZ[:0], n)[:n]
	for i := range r.aoNearZ {
		r.aoNearZ[i] = float32(math.Inf(1))
	}
	D, N := fr.Depth, fr.Normal
	for y := range h {
		row := y * w
		r.ch.Drawn.Each(y, func(xa, xb int) {
			for x := xa; x <= xb; x++ {
				if r.stamp[row+x] != r.gen {
					continue
				}
				c := lg.cell(x, y)
				r.aoDirty[c] = true
				if d := D[row+x]; d > 0 {
					r.aoNearZ[c] = min(r.aoNearZ[c], 1/d)
				}
			}
		})
	}
	radius := r.AO.Radius
	scale := radius * max(r.fx*0.5*float64(w), r.fy*0.5*float64(h))
	r.aoReach = slices.Grow(r.aoReach[:0], n)[:n]
	clear(r.aoReach)
	for c := range n {
		if !r.aoDirty[c] {
			continue
		}
		r.redo[c] = true
		z := max(float64(r.aoNearZ[c])-2*radius, nearPlane)
		reach := int32(math.Ceil(scale/z/float64(lg.shrink))) + aoSlack
		lx, ly := c%lg.w, c/lg.w
		row := r.aoReach[ly*lg.w : (ly+1)*lg.w]
		for x := max(lx-int(reach), 0); x <= min(lx+int(reach), lg.w-1); x++ {
			row[x] = max(row[x], reach)
		}
	}
	r.aoNear = slices.Grow(r.aoNear[:0], n)[:n]
	clear(r.aoNear)
	for c := range n {
		reach := int(r.aoReach[c])
		if reach == 0 {
			continue
		}
		lx, ly := c%lg.w, c/lg.w
		for y := max(ly-reach, 0); y <= min(ly+reach, lg.h-1); y++ {
			r.aoNear[y*lg.w+lx] = true
		}
	}
	samples := min(max(r.AO.Samples, 4), aoMaxSamples)
	stride := aoMaxSamples / samples
	sx, sy := r.fx*0.5*float64(w), r.fy*0.5*float64(h)
	parallelBands(lg.h, 2, func(y0, y1 int) {
		for ly := y0; ly < y1; ly++ {
			for lx := range lg.w {
				li := ly*lg.w + lx
				if r.redo[li] || !r.aoNear[li] {
					continue
				}
				x, y := lg.pixel(lx, ly, w, h)
				i := y*w + x
				if D[i] == 0 || N[i]&normalLit == 0 {
					continue
				}
				z := 1 / float64(D[i])
				rpx, rpy := radius*sx/z, radius*sy/z
				if rpx < 0.75 && rpy < 0.75 {
					continue
				}
				table := aoTurn(lx, ly)
				for s := range samples {
					qx, qy := aoSampleAt(&table[s*stride], x, y, rpx, rpy)
					if qx < 0 || qy < 0 || qx >= w || qy >= h {
						continue
					}
					if r.aoDirty[lg.cell(qx, qy)] {
						r.redo[li] = true
						break
					}
				}
			}
		}
	})
}

// Adds the pixels under every marked cell of a grid at a shrink to a set, cell runs added as boxes
func markCells(d *Dirt, on []bool, lw, lh, shrink int) {
	for ly := range lh {
		row := on[ly*lw : (ly+1)*lw]
		for lx := 0; lx < lw; {
			if !row[lx] {
				lx++
				continue
			}
			end := lx
			for end+1 < lw && row[end+1] {
				end++
			}
			d.AddRect(lx*shrink, ly*shrink, (end+1)*shrink-1, (ly+1)*shrink-1)
			lx = end + 1
		}
	}
}

// Samples the marked cells, takes the rest from the layer, blurs, and marks the cells that moved with their neighbors
func (r *Raster) occludeGrid(workers int, l *Layer) {
	fr := r.fr
	w, h := fr.W, fr.H
	lg := &r.aoGrid
	n := lg.w * lg.h
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
				table := aoTurn(lx, ly)
				occ := 0.0
				for s := range samples {
					qx, qy := aoSampleAt(&table[s*stride], x, y, rpx, rpy)
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
	r.shift = slices.Grow(r.shift[:0], n)[:n]
	r.moved = slices.Grow(r.moved[:0], n)[:n]
	if !l.hasAO(lg) {
		for i := range r.moved {
			r.shift[i], r.moved[i] = true, true
		}
		return
	}
	for li := range n {
		r.shift[li] = math.Abs(float64(r.ao2[li]-l.aoBlur[li])) > aoStill || lg.z[li] != l.aoZ[li]
	}
	clear(r.moved)
	for ly := range lg.h {
		for lx := range lg.w {
			if !r.shift[ly*lg.w+lx] {
				continue
			}
			for y := max(ly-1, 0); y <= min(ly+1, lg.h-1); y++ {
				for x := max(lx-1, 0); x <= min(lx+1, lg.w-1); x++ {
					r.moved[y*lg.w+x] = true
				}
			}
		}
	}
}

// Spreads the crowding over the relit pixels beside the crowding the layer lit them by, equal where no cell moved
func (r *Raster) occludeSpread(workers int, l *Layer) {
	fr := r.fr
	w, h := fr.W, fr.H
	lg := &r.aoGrid
	r.ao = slices.Grow(r.ao[:0], w*h)[:w*h]
	r.aoOld = slices.Grow(r.aoOld[:0], w*h)[:w*h]
	D, N := fr.Depth, fr.Normal
	cached := l.hasAO(lg)
	parallelBandsWith(h, 4, workers, func(y0, y1 int) {
		for y := y0; y < y1; y++ {
			r.ch.Lit.Each(y, func(xa, xb int) {
				for x := xa; x <= xb; x++ {
					i := y*w + x
					if D[i] == 0 || N[i]&normalLit == 0 {
						r.ao[i], r.aoOld[i] = 1, 1
						continue
					}
					fresh := l == nil || r.stamp[i] == r.gen
					if !fresh && !r.moved[lg.cell(x, y)] {
						r.ao[i], r.aoOld[i] = 1, 1
						continue
					}
					z := 1 / float64(D[i])
					r.ao[i] = lg.spread(r.ao2, lg.z, x, y, lg.cell(x, y), z)
					if fresh || !cached {
						r.aoOld[i] = r.ao[i]
						continue
					}
					r.aoOld[i] = lg.spread(l.aoBlur, l.aoZ, x, y, lg.cell(x, y), z)
				}
			})
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
