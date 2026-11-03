package vxl

import (
	"math"
	"runtime"
	"slices"
)

// Radiance baked over a grid of cells from emitters with a line of sight, varying the ambient light
type Volume struct {
	lo         Vec3
	cell, inv  float64
	nx, ny, nz int
	rad        []rgb32
	solid      []bool
	// Whether the solid cells hold the light of their neighbors, so a sample needs no solid checks
	sealed bool
}

// Color held in single precision
type rgb32 [3]float32

// Volume of cells of a side covering a world box, one cell of padding around it
func NewVolume(lo, hi Vec3, cell float64) *Volume {
	if !(cell > 0) {
		cell = 1
	}
	v := &Volume{cell: cell, inv: 1 / cell}
	v.lo = Vec3{lo.X - cell, lo.Y - cell, lo.Z - cell}
	v.nx = max(int(math.Ceil((hi.X-lo.X)/cell))+2, 1)
	v.ny = max(int(math.Ceil((hi.Y-lo.Y)/cell))+2, 1)
	v.nz = max(int(math.Ceil((hi.Z-lo.Z)/cell))+2, 1)
	v.rad = make([]rgb32, v.nx*v.ny*v.nz)
	v.solid = make([]bool, v.nx*v.ny*v.nz)
	return v
}

// World box the cells cover
func (v *Volume) Bounds() (lo, hi Vec3) {
	return v.lo, v.lo.Add(Vec3{float64(v.nx) * v.cell, float64(v.ny) * v.cell, float64(v.nz) * v.cell})
}

// Index of a cell, negative outside the volume
func (v *Volume) index(x, y, z int) int {
	if x < 0 || y < 0 || z < 0 || x >= v.nx || y >= v.ny || z >= v.nz {
		return -1
	}
	return (y*v.nz+z)*v.nx + x
}

// Cell holding a world point
func (v *Volume) cellOf(p Vec3) (x, y, z int) {
	return int(math.Floor((p.X - v.lo.X) * v.inv)), int(math.Floor((p.Y - v.lo.Y) * v.inv)), int(math.Floor((p.Z - v.lo.Z) * v.inv))
}

// Center of a cell in the world
func (v *Volume) center(x, y, z int) Vec3 {
	return Vec3{v.lo.X + (float64(x)+0.5)*v.cell, v.lo.Y + (float64(y)+0.5)*v.cell, v.lo.Z + (float64(z)+0.5)*v.cell}
}

// Drops every light, keeping the solid cells
func (v *Volume) Clear() {
	clear(v.rad)
	v.sealed = false
}

// Marks solid every cell a voxel of the grid lies in
func (v *Volume) Occupy(g Grid) {
	for c := range g {
		lo := Vec3{float64(c[0]), float64(c[1]), float64(c[2])}
		x0, y0, z0 := v.cellOf(lo.Add(Vec3{1e-6, 1e-6, 1e-6}))
		x1, y1, z1 := v.cellOf(lo.Add(Vec3{1 - 1e-6, 1 - 1e-6, 1 - 1e-6}))
		for y := y0; y <= y1; y++ {
			for z := z0; z <= z1; z++ {
				for x := x0; x <= x1; x++ {
					if i := v.index(x, y, z); i >= 0 {
						v.solid[i] = true
					}
				}
			}
		}
	}
}

// Reports whether the cell holding a point is solid, false outside the volume
func (v *Volume) Solid(p Vec3) bool {
	i := v.index(v.cellOf(p))
	return i >= 0 && v.solid[i]
}

// Adds light to the cell holding a point
func (v *Volume) Add(p Vec3, c RGB) {
	if i := v.index(v.cellOf(p)); i >= 0 {
		v.rad[i][0] += float32(c.R)
		v.rad[i][1] += float32(c.G)
		v.rad[i][2] += float32(c.B)
	}
}

// Adds the light of every emitter reaching the volume to every open cell it can see within its reach
func (v *Volume) Splat(srcs []Light) {
	if len(srcs) == 0 {
		return
	}
	type src struct {
		pos        Vec3
		col        RGB
		radius, r2 float64
		y0, y1     int
	}
	list := make([]src, 0, len(srcs))
	lo, hi := v.Bounds()
	for _, l := range srcs {
		if !(l.Radius > 0) || !bounded(&l) {
			continue
		}
		if l.Pos.X+l.Radius < lo.X || l.Pos.X-l.Radius > hi.X || l.Pos.Y+l.Radius < lo.Y || l.Pos.Y-l.Radius > hi.Y || l.Pos.Z+l.Radius < lo.Z || l.Pos.Z-l.Radius > hi.Z {
			continue
		}
		_, y0, _ := v.cellOf(Vec3{l.Pos.X, l.Pos.Y - l.Radius, l.Pos.Z})
		_, y1, _ := v.cellOf(Vec3{l.Pos.X, l.Pos.Y + l.Radius, l.Pos.Z})
		list = append(list, src{pos: l.Pos, col: l.Color, radius: l.Radius, r2: l.Radius * l.Radius, y0: y0, y1: y1})
	}
	near := v.cell * 1.5
	parallel(v.ny, runtime.NumCPU(), func(y int) {
		for z := range v.nz {
			for x := range v.nx {
				i := v.index(x, y, z)
				if v.solid[i] {
					continue
				}
				c := v.center(x, y, z)
				var acc RGB
				for k := range list {
					s := &list[k]
					if y < s.y0 || y > s.y1 {
						continue
					}
					d := c.Sub(s.pos)
					d2 := d.Dot(d)
					if d2 >= s.r2 {
						continue
					}
					dist := math.Sqrt(d2)
					if dist > near && !v.open(s.pos, c) {
						continue
					}
					acc = acc.Add(s.col.Scale(falloff(dist, d2, s.r2)))
				}
				if acc != (RGB{}) {
					v.rad[i][0] += float32(acc.R)
					v.rad[i][1] += float32(acc.G)
					v.rad[i][2] += float32(acc.B)
				}
			}
		}
	})
}

// Share of an emitter reaching a distance, the inverse square windowed smoothly to nothing at the reach
func falloff(dist, d2, r2 float64) float64 {
	w := 1 - d2/r2
	return w * w / (1 + d2)
}

// Reports whether no solid cell lies on the straight line between two points, the cells at either end not counted
func (v *Volume) open(a, b Vec3) bool {
	ax, ay, az := (a.X-v.lo.X)*v.inv, (a.Y-v.lo.Y)*v.inv, (a.Z-v.lo.Z)*v.inv
	bx, by, bz := (b.X-v.lo.X)*v.inv, (b.Y-v.lo.Y)*v.inv, (b.Z-v.lo.Z)*v.inv
	x, y, z := int(math.Floor(ax)), int(math.Floor(ay)), int(math.Floor(az))
	ex, ey, ez := int(math.Floor(bx)), int(math.Floor(by)), int(math.Floor(bz))
	dx, dy, dz := bx-ax, by-ay, bz-az
	stepX, tMaxX, tDeltaX := ddaAxis(ax, dx)
	stepY, tMaxY, tDeltaY := ddaAxis(ay, dy)
	stepZ, tMaxZ, tDeltaZ := ddaAxis(az, dz)
	for n := 0; n < v.nx+v.ny+v.nz; n++ {
		if x == ex && y == ey && z == ez {
			return true
		}
		switch {
		case tMaxX < tMaxY && tMaxX < tMaxZ:
			x += stepX
			tMaxX += tDeltaX
		case tMaxY < tMaxZ:
			y += stepY
			tMaxY += tDeltaY
		default:
			z += stepZ
			tMaxZ += tDeltaZ
		}
		if tMaxX > 1 && tMaxY > 1 && tMaxZ > 1 {
			return true
		}
		if x == ex && y == ey && z == ez {
			return true
		}
		i := v.index(x, y, z)
		if i >= 0 && v.solid[i] {
			return false
		}
	}
	return true
}

// Step, distance to the first cell edge and distance between edges along one axis of a grid walk
func ddaAxis(a, d float64) (step int, tMax, tDelta float64) {
	switch {
	case d > 0:
		return 1, (math.Floor(a) + 1 - a) / d, 1 / d
	case d < 0:
		return -1, (a - math.Floor(a)) / -d, 1 / -d
	}
	return 0, math.Inf(1), math.Inf(1)
}

// Blends every open cell toward the mean of its open neighbors, over a count of passes
func (v *Volume) Smooth(passes int) {
	if passes <= 0 {
		return
	}
	next := make([]rgb32, len(v.rad))
	for range passes {
		parallel(v.ny, runtime.NumCPU(), func(y int) {
			for z := range v.nz {
				for x := range v.nx {
					i := v.index(x, y, z)
					if v.solid[i] {
						continue
					}
					sum := [3]float32{v.rad[i][0] * 2, v.rad[i][1] * 2, v.rad[i][2] * 2}
					wt := float32(2)
					for _, j := range [6]int{v.index(x-1, y, z), v.index(x+1, y, z), v.index(x, y-1, z), v.index(x, y+1, z), v.index(x, y, z-1), v.index(x, y, z+1)} {
						if j < 0 || v.solid[j] {
							continue
						}
						sum[0] += v.rad[j][0]
						sum[1] += v.rad[j][1]
						sum[2] += v.rad[j][2]
						wt++
					}
					next[i] = rgb32{sum[0] / wt, sum[1] / wt, sum[2] / wt}
				}
			}
		})
		v.rad, next = next, v.rad
	}
}

// Fills solid cells from their open neighbors over three passes, so sampling needs no solid checks
func (v *Volume) Seal() {
	filled := make([]bool, len(v.solid))
	for i := range v.solid {
		filled[i] = !v.solid[i]
	}
	next := make([]rgb32, len(v.rad))
	nextFilled := make([]bool, len(filled))
	for range 3 {
		copy(next, v.rad)
		copy(nextFilled, filled)
		parallel(v.ny, runtime.NumCPU(), func(y int) {
			for z := range v.nz {
				for x := range v.nx {
					i := v.index(x, y, z)
					if filled[i] {
						continue
					}
					var sum [3]float32
					n := float32(0)
					for _, j := range [6]int{v.index(x-1, y, z), v.index(x+1, y, z), v.index(x, y-1, z), v.index(x, y+1, z), v.index(x, y, z-1), v.index(x, y, z+1)} {
						if j < 0 || !filled[j] {
							continue
						}
						sum[0] += v.rad[j][0]
						sum[1] += v.rad[j][1]
						sum[2] += v.rad[j][2]
						n++
					}
					if n > 0 {
						next[i] = rgb32{sum[0] / n, sum[1] / n, sum[2] / n}
						nextFilled[i] = true
					}
				}
			}
		})
		v.rad, next = next, v.rad
		filled, nextFilled = nextFilled, filled
	}
	v.sealed = true
}

// Scales every cell by a factor
func (v *Volume) Scale(k float64) {
	f := float32(k)
	for i := range v.rad {
		v.rad[i][0] *= f
		v.rad[i][1] *= f
		v.rad[i][2] *= f
	}
}

// Light at a point, blended from the eight open cells around it, none outside the volume
func (v *Volume) Sample(p Vec3) RGB {
	fx, fy, fz := (p.X-v.lo.X)*v.inv-0.5, (p.Y-v.lo.Y)*v.inv-0.5, (p.Z-v.lo.Z)*v.inv-0.5
	x0, y0, z0 := int(math.Floor(fx)), int(math.Floor(fy)), int(math.Floor(fz))
	tx, ty, tz := fx-float64(x0), fy-float64(y0), fz-float64(z0)
	var acc [3]float64
	wt := 0.0
	if x0 >= 0 && y0 >= 0 && z0 >= 0 && x0+1 < v.nx && y0+1 < v.ny && z0+1 < v.nz {
		base := (y0*v.nz+z0)*v.nx + x0
		dy := v.nz * v.nx
		if v.sealed {
			c000, c100, c010, c110 := &v.rad[base], &v.rad[base+1], &v.rad[base+dy], &v.rad[base+dy+1]
			c001, c101, c011, c111 := &v.rad[base+v.nx], &v.rad[base+v.nx+1], &v.rad[base+dy+v.nx], &v.rad[base+dy+v.nx+1]
			w000, w100, w010, w110 := (1-tx)*(1-ty)*(1-tz), tx*(1-ty)*(1-tz), (1-tx)*ty*(1-tz), tx*ty*(1-tz)
			w001, w101, w011, w111 := (1-tx)*(1-ty)*tz, tx*(1-ty)*tz, (1-tx)*ty*tz, tx*ty*tz
			return RGB{
				w000*float64(c000[0]) + w100*float64(c100[0]) + w010*float64(c010[0]) + w110*float64(c110[0]) + w001*float64(c001[0]) + w101*float64(c101[0]) + w011*float64(c011[0]) + w111*float64(c111[0]),
				w000*float64(c000[1]) + w100*float64(c100[1]) + w010*float64(c010[1]) + w110*float64(c110[1]) + w001*float64(c001[1]) + w101*float64(c101[1]) + w011*float64(c011[1]) + w111*float64(c111[1]),
				w000*float64(c000[2]) + w100*float64(c100[2]) + w010*float64(c010[2]) + w110*float64(c110[2]) + w001*float64(c001[2]) + w101*float64(c101[2]) + w011*float64(c011[2]) + w111*float64(c111[2]),
			}
		}
		for k := range 8 {
			i, w := base, 1.0
			if k&1 != 0 {
				i++
				w *= tx
			} else {
				w *= 1 - tx
			}
			if k&2 != 0 {
				i += dy
				w *= ty
			} else {
				w *= 1 - ty
			}
			if k&4 != 0 {
				i += v.nx
				w *= tz
			} else {
				w *= 1 - tz
			}
			if w <= 0 || v.solid[i] {
				continue
			}
			c := &v.rad[i]
			acc[0] += w * float64(c[0])
			acc[1] += w * float64(c[1])
			acc[2] += w * float64(c[2])
			wt += w
		}
		if wt <= 0 {
			return RGB{}
		}
		return RGB{acc[0] / wt, acc[1] / wt, acc[2] / wt}
	}
	for k := range 8 {
		x, y, z := x0, y0, z0
		w := 1.0
		if k&1 != 0 {
			x++
			w *= tx
		} else {
			w *= 1 - tx
		}
		if k&2 != 0 {
			y++
			w *= ty
		} else {
			w *= 1 - ty
		}
		if k&4 != 0 {
			z++
			w *= tz
		} else {
			w *= 1 - tz
		}
		if w <= 0 {
			continue
		}
		i := v.index(x, y, z)
		if i < 0 || v.solid[i] {
			continue
		}
		c := &v.rad[i]
		acc[0] += w * float64(c[0])
		acc[1] += w * float64(c[1])
		acc[2] += w * float64(c[2])
		wt += w
	}
	if wt <= 0 {
		return RGB{}
	}
	return RGB{acc[0] / wt, acc[1] / wt, acc[2] / wt}
}

// Emitters of a grid, the open faces of glowing voxels grouped in blocks of a side
func (g Grid) Emitters(cluster int, radius float64) []Light {
	cluster = max(cluster, 1)
	type key struct {
		side    int
		x, y, z int
	}
	type sum struct {
		pos Vec3
		col RGB
		n   float64
	}
	groups := map[key]*sum{}
	var order []key
	for c, vox := range g {
		if vox.Emit <= 0 {
			continue
		}
		for side := range 6 {
			n := sideNormals[side]
			if g.Solid(c[0]+n[0], c[1]+n[1], c[2]+n[2]) {
				continue
			}
			k := key{side, floorDiv(c[0], cluster), floorDiv(c[1], cluster), floorDiv(c[2], cluster)}
			s, ok := groups[k]
			if !ok {
				s = &sum{}
				groups[k] = s
				order = append(order, k)
			}
			at := Vec3{float64(c[0]) + 0.5 + 0.5*float64(n[0]), float64(c[1]) + 0.5 + 0.5*float64(n[1]), float64(c[2]) + 0.5 + 0.5*float64(n[2])}
			s.pos = s.pos.Add(at)
			s.col = s.col.Add(vox.Col.Scale(vox.Emit))
			s.n++
		}
	}
	slices.SortFunc(order, func(a, b key) int {
		switch {
		case a.side != b.side:
			return a.side - b.side
		case a.y != b.y:
			return a.y - b.y
		case a.z != b.z:
			return a.z - b.z
		}
		return a.x - b.x
	})
	out := make([]Light, 0, len(order))
	for _, k := range order {
		s := groups[k]
		n := sideNormals[k.side]
		pos := s.pos.Mul(1 / s.n).Add(Vec3{float64(n[0]), float64(n[1]), float64(n[2])}.Mul(0.25))
		out = append(out, Light{Pos: pos, Color: s.col, Radius: radius})
	}
	return out
}

// Emitters standing in for the sun bouncing off the grid, one per block of sunlit open faces
func (g Grid) SunBounce(sun Vec3, sunColor RGB, sm *ShadowMap, cluster int, radius, share float64) []Light {
	cluster = max(cluster, 1)
	sun = sun.Norm()
	type key struct {
		side    int
		x, y, z int
	}
	type sum struct {
		pos Vec3
		col RGB
		n   float64
	}
	groups := map[key]*sum{}
	var order []key
	for c, vox := range g {
		if vox.Emit > 0 {
			continue
		}
		for side := range 6 {
			n := sideNormals[side]
			nv := Vec3{float64(n[0]), float64(n[1]), float64(n[2])}
			ndl := nv.Dot(sun)
			if ndl <= 0 || g.Solid(c[0]+n[0], c[1]+n[1], c[2]+n[2]) {
				continue
			}
			at := Vec3{float64(c[0]) + 0.5 + 0.5*float64(n[0]), float64(c[1]) + 0.5 + 0.5*float64(n[1]), float64(c[2]) + 0.5 + 0.5*float64(n[2])}
			lit := 1.0
			if sm != nil {
				lit = sm.Lit(at, nv)
			}
			if lit <= 0 {
				continue
			}
			k := key{side, floorDiv(c[0], cluster), floorDiv(c[1], cluster), floorDiv(c[2], cluster)}
			s, ok := groups[k]
			if !ok {
				s = &sum{}
				groups[k] = s
				order = append(order, k)
			}
			s.pos = s.pos.Add(at)
			s.col = s.col.Add(vox.Col.Mul(sunColor).Scale(ndl * lit * share))
			s.n++
		}
	}
	slices.SortFunc(order, func(a, b key) int {
		switch {
		case a.side != b.side:
			return a.side - b.side
		case a.y != b.y:
			return a.y - b.y
		case a.z != b.z:
			return a.z - b.z
		}
		return a.x - b.x
	})
	out := make([]Light, 0, len(order))
	for _, k := range order {
		s := groups[k]
		n := sideNormals[k.side]
		pos := s.pos.Mul(1 / s.n).Add(Vec3{float64(n[0]), float64(n[1]), float64(n[2])}.Mul(0.25))
		out = append(out, Light{Pos: pos, Color: s.col, Radius: radius})
	}
	return out
}
