package vxl

import "math"

// Most texels a shadow map spans along either side
const maxShadowTexels = 1024

// Texels a lookup steps off its surface along the facing, past the slope a flat face shows across the taps
const shadowOffset = 1.5

// Texels of depth below the nearest face a lookup still counts as lit, the least tolerance a texel gives
const shadowBias = 1.5

// Texels of depth two faces at one texel may differ by and still count as one surface
const shadowSame = 0.01

// Edge weight a texel center may fall below zero and still count as inside, closing seams between faces
const shadowSeam = 1e-9

// Depths toward the sun of the two faces nearest it across a grid, shading what lies past their midpoint
type ShadowMap struct {
	// Axes across the sun and the direction toward it
	right, up, dir Vec3
	// Place across the sun of the outer corner of the first texel
	u0, v0 float64
	// World units per texel and its inverse
	texel, inv float64
	w, h       int
	// Depth toward the sun of the near and far sides of the world box
	d0, d1 float64
	// Nearest and second nearest depth toward the sun per texel, side by side, minus infinity where none was drawn
	depth []float32
}

// Depth of a texel layer no face was drawn into
var noDepth = float32(math.Inf(-1))

// Orthographic depth map looking down the sun over a world box, texels widened so no side passes the cap
func NewShadowMap(sun Vec3, lo, hi Vec3, texel float64) *ShadowMap {
	dir := sun.Norm()
	if dir == (Vec3{}) {
		dir = Vec3{0, 1, 0}
	}
	ref := Vec3{0, 1, 0}
	if math.Abs(dir.Y) > 0.99 {
		ref = Vec3{1, 0, 0}
	}
	right := ref.Cross(dir).Norm()
	up := dir.Cross(right)
	inf := math.Inf(1)
	u0, u1, v0, v1 := inf, -inf, inf, -inf
	d0, d1 := inf, -inf
	for i := range 8 {
		p := lo
		if i&1 != 0 {
			p.X = hi.X
		}
		if i&2 != 0 {
			p.Y = hi.Y
		}
		if i&4 != 0 {
			p.Z = hi.Z
		}
		u, v, d := p.Dot(right), p.Dot(up), p.Dot(dir)
		u0, u1, v0, v1 = min(u0, u), max(u1, u), min(v0, v), max(v1, v)
		d0, d1 = min(d0, d), max(d1, d)
	}
	if !(texel > 0) {
		texel = 0
	}
	texel = max(texel, max(u1-u0, v1-v0)/(maxShadowTexels-2))
	if texel == 0 {
		texel = 1
	}
	w := min(max(int(math.Ceil((u1-u0)/texel))+2, 2), maxShadowTexels)
	h := min(max(int(math.Ceil((v1-v0)/texel))+2, 2), maxShadowTexels)
	sm := &ShadowMap{right: right, up: up, dir: dir, u0: u0 - texel, v0: v0 - texel, texel: texel, inv: 1 / texel, w: w, h: h, d0: d0, d1: d1, depth: make([]float32, 2*w*h)}
	sm.Clear()
	return sm
}

// Empties every texel so nothing hides the sun
func (sm *ShadowMap) Clear() {
	for i := range sm.depth {
		sm.depth[i] = noDepth
	}
}

// Takes the axes, extent and depths of another map, keeping its own texel storage
func (sm *ShadowMap) CopyFrom(o *ShadowMap) {
	depth := sm.depth
	*sm = *o
	sm.depth = append(depth[:0], o.depth...)
}

// Draws the depth of an opaque lit face from both sides, skipping unlit, glowing and see through faces
func (sm *ShadowMap) Quad(q Quad) { sm.quad(&q) }

// Draws the depth of every face of every chunk, in view or not
func (sm *ShadowMap) Chunks(chunks []Chunk) {
	for i := range chunks {
		for j := range chunks[i].Quads {
			sm.quad(&chunks[i].Quads[j])
		}
	}
}

// Draws the depth of a face held by pointer as two triangles in texel space
func (sm *ShadowMap) quad(q *Quad) {
	if !q.Lit || q.Additive || q.Alpha < 1 {
		return
	}
	var u, v, d [4]float64
	for c, p := range q.P {
		u[c] = (p.Dot(sm.right) - sm.u0) * sm.inv
		v[c] = (p.Dot(sm.up) - sm.v0) * sm.inv
		d[c] = p.Dot(sm.dir)
	}
	sm.tri(u[0], v[0], d[0], u[1], v[1], d[1], u[2], v[2], d[2])
	sm.tri(u[0], v[0], d[0], u[2], v[2], d[2], u[3], v[3], d[3])
}

// Keeps at every texel center a triangle covers the two distinct depths nearest the sun, either winding
func (sm *ShadowMap) tri(ax, ay, az, bx, by, bz, cx, cy, cz float64) {
	area := (bx-ax)*(cy-ay) - (by-ay)*(cx-ax)
	if !(math.Abs(area) > 1e-12) {
		return
	}
	inv := 1 / area
	a0, b0, c0 := (bx*cy-by*cx)*inv, (by-cy)*inv, (cx-bx)*inv
	a1, b1, c1 := (cx*ay-cy*ax)*inv, (cy-ay)*inv, (ax-cx)*inv
	fx0, fx1 := max(math.Floor(min(ax, bx, cx)), 0), min(math.Ceil(max(ax, bx, cx)), float64(sm.w-1))
	fy0, fy1 := max(math.Floor(min(ay, by, cy)), 0), min(math.Ceil(max(ay, by, cy)), float64(sm.h-1))
	if !(fx0 <= fx1 && fy0 <= fy1) {
		return
	}
	x0, x1 := int(fx0), int(fx1)
	same := float32(sm.texel * shadowSame)
	for y := int(fy0); y <= int(fy1); y++ {
		yc, xc := float64(y)+0.5, float64(x0)+0.5
		w0, w1 := a0+b0*xc+c0*yc, a1+b1*xc+c1*yc
		row := sm.depth[2*y*sm.w : 2*(y+1)*sm.w]
		for x := x0; x <= x1; x, w0, w1 = x+1, w0+b0, w1+b1 {
			w2 := 1 - w0 - w1
			if w0 < -shadowSeam || w1 < -shadowSeam || w2 < -shadowSeam {
				continue
			}
			z, k := float32(w0*az+w1*bz+w2*cz), 2*x
			switch first := row[k]; {
			case z > first+same:
				row[k], row[k+1] = z, first
			case z < first-same && z > row[k+1]:
				row[k+1] = z
			}
		}
	}
}

// First texel a nudged point reads, its place among the four read and its depth toward the sun
func (sm *ShadowMap) place(p, n Vec3) (i, j int, tu, tv, d float64) {
	off := sm.texel * shadowOffset
	q := Vec3{p.X + n.X*off, p.Y + n.Y*off, p.Z + n.Z*off}
	u := (q.Dot(sm.right)-sm.u0)*sm.inv - 0.5
	v := (q.Dot(sm.up)-sm.v0)*sm.inv - 0.5
	fu, fv := math.Floor(u), math.Floor(v)
	return int(fu), int(fv), u - fu, v - fv, q.Dot(sm.dir)
}

// Share of the sun reaching a point with a unit facing, four texels filtered after a nudge off the surface
func (sm *ShadowMap) Lit(p, n Vec3) float64 {
	i, j, tu, tv, d := sm.place(p, n)
	a, b, c, e := sm.open(i, j, d), sm.open(i+1, j, d), sm.open(i, j+1, d), sm.open(i+1, j+1, d)
	switch a + b + c + e {
	case 4:
		return 1
	case 0:
		return 0
	}
	return (a*(1-tu)+b*tu)*(1-tv) + (c*(1-tu)+e*tu)*tv
}

// Share of the sun reaching a point under a map, all of it under none
func litBy(sm *ShadowMap, p, n Vec3) float64 {
	if sm == nil {
		return 1
	}
	return sm.Lit(p, n)
}

// Reports whether a point with a unit facing reads any marked texel of the map
func (sm *ShadowMap) touches(mark []bool, p, n Vec3) bool {
	i, j, _, _, _ := sm.place(p, n)
	for _, t := range [4][2]int{{i, j}, {i + 1, j}, {i, j + 1}, {i + 1, j + 1}} {
		if uint(t[0]) < uint(sm.w) && uint(t[1]) < uint(sm.h) && mark[t[1]*sm.w+t[0]] {
			return true
		}
	}
	return false
}

// Reports whether another map shares the axes, origin, texel size and extent
func (sm *ShadowMap) aligned(o *ShadowMap) bool {
	return o != nil && sm.right == o.right && sm.up == o.up && sm.dir == o.dir && sm.u0 == o.u0 && sm.v0 == o.v0 && sm.texel == o.texel && sm.w == o.w && sm.h == o.h
}

// World point at a place across the sun and a depth toward it
func (sm *ShadowMap) point(u, v, d float64) Vec3 {
	return sm.right.Mul(u).Add(sm.up.Mul(v)).Add(sm.dir.Mul(d))
}

// One off the map, or above the midpoint of the two nearest faces, or within tolerance of a lone face
func (sm *ShadowMap) open(i, j int, d float64) float64 {
	if uint(i) >= uint(sm.w) || uint(j) >= uint(sm.h) {
		return 1
	}
	k := 2 * (j*sm.w + i)
	first, second := float64(sm.depth[k]), sm.depth[k+1]
	ref := first - sm.texel*shadowBias
	if second != noDepth {
		ref = min(ref, (first+float64(second))/2)
	}
	if ref > d {
		return 0
	}
	return 1
}

// Casts a box turned by a yaw about its center, every face a solid caster whatever it is painted with
func (sm *ShadowMap) Box(center, size Vec3, yaw float64) {
	corners := boxCorners(center, size, yaw)
	for i := range boxFaces {
		q := Quad{Alpha: 1, Lit: true}
		for c := range 4 {
			q.P[c] = corners[boxCornerOf[i][c]]
		}
		sm.quad(&q)
	}
}
