package vxl

import (
	"math"
	"slices"
)

// Side in world units of the floor cells lights are binned into
const lightCell = 8.0

// Most cells the light grid holds, its cells widened past that
const maxLightCells = 4096

// Lights binned over a grid on the floor, each listed in index order in every cell its reach overlaps
type lightBins struct {
	src    *Light
	n      int
	x0, z0 float64
	cell   float64
	nx, nz int
	start  []int32
	items  []int32
	cursor []int32
	// Lights of unbounded reach, tested for every face
	wide []int32
}

// Reports whether a light has a finite reach on the floor, zero reach counting as none at all
func bounded(l *Light) bool {
	return !math.IsNaN(l.Radius) && !math.IsInf(l.Radius, 0) && !math.IsNaN(l.Pos.X) && !math.IsInf(l.Pos.X, 0) && !math.IsNaN(l.Pos.Z) && !math.IsInf(l.Pos.Z, 0)
}

// Bins a set of lights over a grid spanning the reach of the bounded ones
func (lb *lightBins) build(ls []Light) {
	lb.n, lb.src, lb.nx, lb.nz, lb.wide = len(ls), nil, 0, 0, lb.wide[:0]
	if len(ls) == 0 {
		return
	}
	lb.src = &ls[0]
	lo, hi := Vec3{math.Inf(1), 0, math.Inf(1)}, Vec3{math.Inf(-1), 0, math.Inf(-1)}
	for i := range ls {
		l := &ls[i]
		if !bounded(l) {
			lb.wide = append(lb.wide, int32(i))
			continue
		}
		if l.Radius == 0 {
			continue
		}
		r := math.Abs(l.Radius)
		lo.X, lo.Z = min(lo.X, l.Pos.X-r), min(lo.Z, l.Pos.Z-r)
		hi.X, hi.Z = max(hi.X, l.Pos.X+r), max(hi.Z, l.Pos.Z+r)
	}
	if !(hi.X >= lo.X) || math.IsInf(hi.X-lo.X, 0) || math.IsInf(hi.Z-lo.Z, 0) {
		return
	}
	lb.x0, lb.z0, lb.cell = lo.X, lo.Z, lightCell
	for {
		lb.nx = int((hi.X-lo.X)/lb.cell) + 1
		lb.nz = int((hi.Z-lo.Z)/lb.cell) + 1
		if lb.nx*lb.nz <= maxLightCells {
			break
		}
		lb.cell *= 2
	}
	cells := lb.nx * lb.nz
	lb.start = slices.Grow(lb.start[:0], cells+1)[:cells+1]
	clear(lb.start)
	for i := range ls {
		if x0, x1, z0, z1, ok := lb.span(&ls[i]); ok {
			for z := z0; z <= z1; z++ {
				for x := x0; x <= x1; x++ {
					lb.start[z*lb.nx+x+1]++
				}
			}
		}
	}
	for c := range cells {
		lb.start[c+1] += lb.start[c]
	}
	lb.items = slices.Grow(lb.items[:0], int(lb.start[cells]))[:lb.start[cells]]
	lb.cursor = slices.Grow(lb.cursor[:0], cells)[:cells]
	copy(lb.cursor, lb.start[:cells])
	for i := range ls {
		if x0, x1, z0, z1, ok := lb.span(&ls[i]); ok {
			for z := z0; z <= z1; z++ {
				for x := x0; x <= x1; x++ {
					c := z*lb.nx + x
					lb.items[lb.cursor[c]] = int32(i)
					lb.cursor[c]++
				}
			}
		}
	}
}

// Cells a bounded light of some reach overlaps, false for any other light
func (lb *lightBins) span(l *Light) (x0, x1, z0, z1 int, ok bool) {
	if l.Radius == 0 || !bounded(l) {
		return 0, 0, 0, 0, false
	}
	r := math.Abs(l.Radius)
	x0, x1, okx := lb.cells(l.Pos.X-r, l.Pos.X+r, lb.x0, lb.nx)
	z0, z1, okz := lb.cells(l.Pos.Z-r, l.Pos.Z+r, lb.z0, lb.nz)
	return x0, x1, z0, z1, okx && okz
}

// Grid columns along one axis a span covers, clamped to the grid, false when it misses
func (lb *lightBins) cells(lo, hi, origin float64, n int) (int, int, bool) {
	a, b := math.Floor((lo-origin)/lb.cell), math.Floor((hi-origin)/lb.cell)
	if !(b >= 0 && a < float64(n)) {
		return 0, 0, false
	}
	return int(max(a, 0)), int(min(b, float64(n-1))), true
}

// Replaces the point lights and bins them for the corner light of see through faces
func (r *Raster) SetLights(ls []Light) {
	r.Lights = ls
	r.bins.build(ls)
}

// Indices in ascending order of the lights binned where the floor box of a face lies, plus every unbounded one
func (r *Raster) lightsNear(q *Quad) []int32 {
	if len(r.Lights) == 0 {
		return nil
	}
	lb := &r.bins
	if lb.n != len(r.Lights) || lb.src != &r.Lights[0] {
		lb.build(r.Lights)
	}
	lo, hi := q.P[0], q.P[0]
	for _, p := range q.P[1:] {
		lo.X, lo.Z = min(lo.X, p.X), min(lo.Z, p.Z)
		hi.X, hi.Z = max(hi.X, p.X), max(hi.Z, p.Z)
	}
	x0, x1, okx := lb.cells(lo.X, hi.X, lb.x0, lb.nx)
	z0, z1, okz := lb.cells(lo.Z, hi.Z, lb.z0, lb.nz)
	hit := okx && okz && lb.nx > 0
	if !hit {
		return lb.wide
	}
	if x0 == x1 && z0 == z1 && len(lb.wide) == 0 {
		c := z0*lb.nx + x0
		return lb.items[lb.start[c]:lb.start[c+1]]
	}
	cand := append(r.cand[:0], lb.wide...)
	for z := z0; z <= z1; z++ {
		for x := x0; x <= x1; x++ {
			c := z*lb.nx + x
			cand = append(cand, lb.items[lb.start[c]:lb.start[c+1]]...)
		}
	}
	slices.Sort(cand)
	cand = slices.Compact(cand)
	r.cand = cand
	return cand
}
