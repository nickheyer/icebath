package vxl

import (
	"math"
	"runtime"
)

// Orbit camera looking at a target from a yaw, a pitch and a distance
type Camera struct {
	Target Vec3
	Yaw    float64
	Pitch  float64
	Dist   float64
	FOV    float64
}

// Position of the camera
func (c Camera) Eye() Vec3 {
	cp, sp := math.Cos(c.Pitch), math.Sin(c.Pitch)
	return c.Target.Add(Vec3{cp * math.Sin(c.Yaw), sp, cp * math.Cos(c.Yaw)}.Mul(c.Dist))
}

// Nearest view depth a face may have and still be drawn
const nearPlane = 0.25

// Flat face in the world with a color, a light per corner, a pattern and how it blends
type Quad struct {
	P     [4]Vec3
	Color RGB
	// Light each corner reflects, multiplied into the color
	Light [4]RGB
	// Pattern scaling the color and its place at every corner, in world units for the tiling ones
	Tex Texture
	UV  [4][2]float64
	// Share of the face laid over what is behind it, one covering it
	Alpha float64
	// Light added instead of laid over, for glows
	Additive bool
	// Face drawn from either side, for decals and billboards
	TwoSided bool
}

// Face lit evenly by one light level
func FlatQuad(p [4]Vec3, col RGB, light float64) Quad {
	l := Grey(light)
	return Quad{P: p, Color: col, Light: [4]RGB{l, l, l, l}, Alpha: 1}
}

// Point light coloring faces within its radius
type Light struct {
	Pos    Vec3
	Color  RGB
	Radius float64
}

// Corner of a face in view space with its light and pattern place
type vertex struct {
	p     Vec3
	light RGB
	uv    [2]float64
}

// Screen space triangle with everything its pixels need, binned by row band
type tri struct {
	x, y, z [3]float64
	l       [3]RGB
	// Pattern place of each corner over its depth
	u, v   [3]float64
	col    RGB
	alpha  float64
	x0, x1 int32
	y0, y1 int32
	normal uint32
	flags  uint8
	tex    Texture
}

// Flags of a triangle
const (
	triAdditive uint8 = 1 << iota
	triWrite
	// The third corner lies on an edge shared with the triangle before, so a zero weight there is outside
	triStrict
)

// Draws faces into a frame through a camera, lit by a sun, an ambient level and point lights, with fog
type Raster struct {
	fr      *Frame
	cam     Camera
	eye     Vec3
	right   Vec3
	up      Vec3
	forward Vec3
	fx, fy  float64
	// Fog toward a color between two view depths
	FogColor RGB
	FogNear  float64
	FogFar   float64
	// Direction toward the key light, unit length, and its color
	Sun      Vec3
	SunColor RGB
	// Light every face gets from everywhere
	Ambient RGB
	// Point lights Shade adds, binned again by Begin and SetLights
	Lights []Light
	// Goroutines Flush draws with, the CPU count when zero
	Workers int
	scratch []vertex
	tris    []tri
	bands   [][]int32
	bandH   int
	bins    lightBins
	cand    []int32
	inBox   []int32
	queued  int
}

// Points the raster at the frame through the camera, dropping anything queued
func (r *Raster) Begin(fr *Frame, cam Camera) {
	r.fr, r.cam = fr, cam
	r.eye = cam.Eye()
	r.forward = cam.Target.Sub(r.eye).Norm()
	r.right = r.forward.Cross(Vec3{0, 1, 0}).Norm()
	if r.right.Len() == 0 {
		r.right = Vec3{1, 0, 0}
	}
	r.up = r.right.Cross(r.forward)
	f := 1 / math.Tan(cam.FOV/2)
	aspect := float64(max(fr.W, 1)) * fr.PixelAspect / float64(max(fr.H, 1))
	r.fx, r.fy = f/aspect, f
	r.tris = r.tris[:0]
	r.queued = 0
	r.bins.build(r.Lights)
}

// Count of faces handed to Quad since Begin
func (r *Raster) Queued() int { return r.queued }

// Camera basis the raster draws through
func (r *Raster) Basis() (right, up, forward Vec3) { return r.right, r.up, r.forward }

// Where the camera stands
func (r *Raster) Eye() Vec3 { return r.eye }

// Point in view space, z growing away from the camera
func (r *Raster) view(p Vec3) Vec3 {
	d := p.Sub(r.eye)
	return Vec3{d.Dot(r.right), d.Dot(r.up), d.Dot(r.forward)}
}

// Pixel of a view space point and one over its depth
func (r *Raster) screen(v Vec3) (sx, sy, invz float64) {
	invz = 1 / v.Z
	sx = (v.X*invz*r.fx*0.5 + 0.5) * float64(r.fr.W)
	sy = (0.5 - v.Y*invz*r.fy*0.5) * float64(r.fr.H)
	return sx, sy, invz
}

// Pixel of a world point and its view depth, false behind the camera
func (r *Raster) Project(p Vec3) (sx, sy, depth float64, ok bool) {
	v := r.view(p)
	if v.Z < nearPlane {
		return 0, 0, 0, false
	}
	sx, sy, _ = r.screen(v)
	return sx, sy, v.Z, true
}

// Pixels one world unit spans at a view depth, sideways
func (r *Raster) Scale(depth float64) float64 {
	if depth <= 0 {
		return 0
	}
	return r.fx * 0.5 * float64(r.fr.W) / depth
}

// Pixels one world unit spans at a view depth, upward
func (r *Raster) ScaleY(depth float64) float64 {
	if depth <= 0 {
		return 0
	}
	return r.fy * 0.5 * float64(r.fr.H) / depth
}

// Reports whether a box may show on the frame, false with every corner behind the camera or past one edge
func (r *Raster) Visible(lo, hi Vec3) bool {
	left, right, above, below, behind := true, true, true, true, true
	for i := range 8 {
		p := Vec3{lo.X, lo.Y, lo.Z}
		if i&1 != 0 {
			p.X = hi.X
		}
		if i&2 != 0 {
			p.Y = hi.Y
		}
		if i&4 != 0 {
			p.Z = hi.Z
		}
		v := r.view(p)
		if v.Z < nearPlane {
			continue
		}
		behind = false
		sx, sy, _ := r.screen(v)
		left = left && sx < 0
		right = right && sx >= float64(r.fr.W)
		above = above && sy < 0
		below = below && sy >= float64(r.fr.H)
	}
	return !(behind || left || right || above || below)
}

// Fills the light of every corner from the sun, the ambient level and the binned point lights near the face
func (r *Raster) Shade(q *Quad, n Vec3) { r.shadeFrom(q, n, r.lightsNear(q)) }

// Fills the light of every corner from the sun, the ambient level and the listed point lights in index order
func (r *Raster) shadeFrom(q *Quad, n Vec3, near []int32) {
	base := r.Ambient.Add(r.SunColor.Scale(max(n.Dot(r.Sun), 0)))
	for c := range 4 {
		l := base
		for _, i := range near {
			lt := &r.Lights[i]
			d := lt.Pos.Sub(q.P[c])
			d2 := d.Dot(d)
			if d2 >= lt.Radius*lt.Radius || d2 == 0 {
				continue
			}
			dist := math.Sqrt(d2)
			nd := max(d.Dot(n)/dist, 0)
			att := 1 - dist/lt.Radius
			l = l.Add(lt.Color.Scale(nd * att * att))
		}
		q.Light[c] = l
	}
}

// Queues a face, clipped to the near plane, back faces skipped unless it is two sided
func (r *Raster) Quad(q Quad) { r.quad(&q) }

// Corner of a face on the screen, its light and pattern place divided by its depth
type svert struct {
	x, y, z float64
	l       RGB
	u, v    float64
}

// Queues a face held by pointer, each corner projected once and shared by the triangles of its fan
func (r *Raster) quad(q *Quad) {
	r.queued++
	if q.Alpha <= 0 {
		return
	}
	n := q.P[1].Sub(q.P[0]).Cross(q.P[3].Sub(q.P[0]))
	facing := n.Dot(r.eye.Sub(q.P[0]))
	if !q.TwoSided && facing <= 0 {
		return
	}
	var normal uint32
	if !q.Additive && q.Alpha >= 1 {
		if facing < 0 {
			n = n.Mul(-1)
		}
		normal = packNormal(n)
	}
	poly := r.scratch[:0]
	behind := 0
	for i := range 4 {
		v := r.view(q.P[i])
		if v.Z < nearPlane {
			behind++
		}
		poly = append(poly, vertex{p: v, light: q.Light[i], uv: q.UV[i]})
	}
	if behind == 4 {
		r.scratch = poly
		return
	}
	if behind > 0 {
		poly = clipNear(poly)
	}
	r.scratch = poly
	if len(poly) < 3 {
		return
	}
	var sv [8]svert
	for i, v := range poly {
		s := &sv[i]
		s.x, s.y, s.z = r.screen(v.p)
		s.l = v.light.Scale(s.z)
		s.u, s.v = v.uv[0]*s.z, v.uv[1]*s.z
	}
	for i := 1; i+1 < len(poly); i++ {
		r.tri(&sv[0], &sv[i], &sv[i+1], q, normal, i > 1)
	}
}

// Unit facing packed as three signed bytes under a marker bit, so no facing packs to zero
func packNormal(n Vec3) uint32 {
	n = n.Norm()
	b := func(f float64) uint32 { return uint32(uint8(int8(math.Round(f * 127)))) }
	return b(n.X) | b(n.Y)<<8 | b(n.Z)<<16 | 1<<24
}

// Polygon cut to the part before the near plane, lights lerped along cut edges, written over the input
func clipNear(in []vertex) []vertex {
	var tmp [8]vertex
	out := tmp[:0]
	for i := range in {
		a, b := in[i], in[(i+1)%len(in)]
		ain, bin := a.p.Z >= nearPlane, b.p.Z >= nearPlane
		if ain {
			out = append(out, a)
		}
		if ain != bin {
			t := (nearPlane - a.p.Z) / (b.p.Z - a.p.Z)
			uv := [2]float64{a.uv[0] + (b.uv[0]-a.uv[0])*t, a.uv[1] + (b.uv[1]-a.uv[1])*t}
			out = append(out, vertex{p: a.p.Lerp(b.p, t), light: a.light.Mix(b.light, t), uv: uv})
		}
	}
	return append(in[:0], out...)
}

// Queues one screen space triangle with its bounds
func (r *Raster) tri(a, b, c *svert, q *Quad, normal uint32, strict bool) {
	fr := r.fr
	x0 := int(math.Floor(min(a.x, b.x, c.x)))
	x1 := int(math.Ceil(max(a.x, b.x, c.x)))
	y0 := int(math.Floor(min(a.y, b.y, c.y)))
	y1 := int(math.Ceil(max(a.y, b.y, c.y)))
	if x1 < 0 || y1 < 0 || x0 >= fr.W || y0 >= fr.H {
		return
	}
	r.tris = append(r.tris, tri{})
	t := &r.tris[len(r.tris)-1]
	t.x, t.y, t.z = [3]float64{a.x, b.x, c.x}, [3]float64{a.y, b.y, c.y}, [3]float64{a.z, b.z, c.z}
	t.x0, t.x1 = int32(max(x0, 0)), int32(min(x1, fr.W-1))
	t.y0, t.y1 = int32(max(y0, 0)), int32(min(y1, fr.H-1))
	t.l = [3]RGB{a.l, b.l, c.l}
	t.col, t.alpha, t.normal = q.Color, q.Alpha, normal
	if q.Tex != TexNone {
		t.tex = q.Tex
		t.u, t.v = [3]float64{a.u, b.u, c.u}, [3]float64{a.v, b.v, c.v}
	}
	if q.Additive {
		t.flags |= triAdditive
	} else if q.Alpha >= 1 {
		t.flags |= triWrite
	}
	if strict {
		t.flags |= triStrict
	}
}

// Draws a camera facing square of the size at a point, for particles and glows
func (r *Raster) Billboard(center Vec3, size float64, col RGB, alpha float64, additive bool) {
	h := size / 2
	rt, up := r.right.Mul(h), r.up.Mul(h)
	q := Quad{Color: col, Alpha: alpha, Additive: additive, TwoSided: true}
	w := Grey(1)
	q.Light = [4]RGB{w, w, w, w}
	q.P = [4]Vec3{center.Sub(rt).Add(up), center.Add(rt).Add(up), center.Add(rt).Sub(up), center.Sub(rt).Sub(up)}
	r.Quad(q)
}

// Draws every queued triangle into the frame, rows split into bands drawn side by side, queued order kept within each
func (r *Raster) Flush() {
	fr := r.fr
	if len(r.tris) == 0 || fr.H == 0 {
		return
	}
	workers := r.Workers
	if workers <= 0 {
		workers = runtime.NumCPU()
	}
	n := min(max(fr.H/6, 1), max(workers*3, 1))
	r.bandH = (fr.H + n - 1) / n
	n = (fr.H + r.bandH - 1) / r.bandH
	for len(r.bands) < n {
		r.bands = append(r.bands, nil)
	}
	for i := range n {
		r.bands[i] = r.bands[i][:0]
	}
	for i := range r.tris {
		t := &r.tris[i]
		for b := int(t.y0) / r.bandH; b <= int(t.y1)/r.bandH; b++ {
			r.bands[b] = append(r.bands[b], int32(i))
		}
	}
	parallel(n, workers, func(b int) {
		y0, y1 := b*r.bandH, min((b+1)*r.bandH, fr.H)-1
		for _, i := range r.bands[b] {
			r.fill(&r.tris[i], y0, y1)
		}
	})
	r.tris = r.tris[:0]
}

// Fills the rows of one triangle between two rows with depth test, perspective correct light and pattern, fog and blending
func (r *Raster) fill(t *tri, rowLo, rowHi int) {
	fr := r.fr
	ax, ay, az := t.x[0], t.y[0], t.z[0]
	bx, by, bz := t.x[1], t.y[1], t.z[1]
	cx, cy, cz := t.x[2], t.y[2], t.z[2]
	area := (bx-ax)*(cy-ay) - (by-ay)*(cx-ax)
	if math.Abs(area) < 1e-9 {
		return
	}
	inv := 1 / area
	// Edge weights as planes over the screen, each scaled by the area
	a0, b0, c0 := (bx*cy-by*cx)*inv, (by-cy)*inv, (cx-bx)*inv
	a1, b1, c1 := (cx*ay-cy*ax)*inv, (cy-ay)*inv, (ax-cx)*inv
	y0, y1 := max(int(t.y0), rowLo), min(int(t.y1), rowHi)
	x0, x1 := int(t.x0), int(t.x1)
	la, lb, lc := t.l[0], t.l[1], t.l[2]
	additive, write, strict := t.flags&triAdditive != 0, t.flags&triWrite != 0, t.flags&triStrict != 0
	fog := r.FogFar > r.FogNear
	fogSpan := r.FogFar - r.FogNear
	tex := t.tex
	ua, ub, uc := t.u[0], t.u[1], t.u[2]
	va, vb, vc := t.v[0], t.v[1], t.v[2]
	// Steps across and down of depth and of each pattern axis over depth, for the footprint of a pixel
	dzx, dzy := b0*(az-cz)+b1*(bz-cz), c0*(az-cz)+c1*(bz-cz)
	dux, duy := b0*(ua-uc)+b1*(ub-uc), c0*(ua-uc)+c1*(ub-uc)
	dvx, dvy := b0*(va-vc)+b1*(vb-vc), c0*(va-vc)+c1*(vb-vc)
	for py := y0; py <= y1; py++ {
		yc := float64(py) + 0.5
		xc := float64(x0) + 0.5
		w0 := a0 + b0*xc + c0*yc
		w1 := a1 + b1*xc + c1*yc
		row := py * fr.W
		for px := x0; px <= x1; px, w0, w1 = px+1, w0+b0, w1+b1 {
			w2 := 1 - w0 - w1
			if w0 < 0 || w1 < 0 || w2 < 0 || strict && w2 == 0 {
				continue
			}
			invz := w0*az + w1*bz + w2*cz
			i := row + px
			if float32(invz) <= fr.Depth[i] {
				continue
			}
			k := 1 / invz
			light := RGB{(w0*la.R + w1*lb.R + w2*lc.R) * k, (w0*la.G + w1*lb.G + w2*lc.G) * k, (w0*la.B + w1*lb.B + w2*lc.B) * k}
			col := t.col.Mul(light)
			if tex != TexNone {
				u := (w0*ua + w1*ub + w2*uc) * k
				v := (w0*va + w1*vb + w2*vc) * k
				du := (math.Abs(dux-u*dzx) + math.Abs(duy-u*dzy)) * k
				dv := (math.Abs(dvx-v*dzx) + math.Abs(dvy-v*dzy)) * k
				col = col.Scale(tex.shade(u, v, du, dv))
			}
			if fog {
				col = col.Mix(r.FogColor, (k-r.FogNear)/fogSpan)
			}
			switch {
			case additive:
				fr.Color[i] = fr.Color[i].Add(col.Scale(t.alpha))
			case t.alpha < 1:
				fr.Color[i] = fr.Color[i].Mix(col, t.alpha)
			default:
				fr.Color[i] = col
			}
			if write {
				fr.Depth[i] = float32(invz)
				fr.Normal[i] = t.normal
			}
		}
	}
}

// Box turned by a yaw around its own center, shaded by the raster on its turned sides
func (r *Raster) OrientedBox(center, size Vec3, yaw float64, col RGB, alpha float64) {
	corners := boxCorners(center, size, yaw)
	sn, cs := math.Sincos(yaw)
	for i := range boxFaces {
		n := boxNormals[i]
		var q Quad
		for c := range 4 {
			q.P[c] = corners[boxCornerOf[i][c]]
		}
		q.Color, q.Alpha = col, alpha
		r.Shade(&q, Vec3{n.X*cs + n.Z*sn, n.Y, -n.X*sn + n.Z*cs})
		r.quad(&q)
	}
}

// Box lit evenly, for things that glow on their own
func (r *Raster) EmissiveBox(center, size Vec3, yaw float64, col RGB, alpha float64, light float64) {
	corners := boxCorners(center, size, yaw)
	l := Grey(light)
	for i := range boxFaces {
		q := Quad{Color: col, Alpha: alpha, Light: [4]RGB{l, l, l, l}}
		for c := range 4 {
			q.P[c] = corners[boxCornerOf[i][c]]
		}
		r.quad(&q)
	}
}

// Eight corners of a box turned by a yaw about its center, indexed by their axis sign bits
func boxCorners(center, size Vec3, yaw float64) [8]Vec3 {
	sn, cs := math.Sincos(yaw)
	hx, hy, hz := size.X/2, size.Y/2, size.Z/2
	var out [8]Vec3
	for i := range 8 {
		x, y, z := -hx, -hy, -hz
		if i&1 != 0 {
			x = hx
		}
		if i&2 != 0 {
			y = hy
		}
		if i&4 != 0 {
			z = hz
		}
		out[i] = Vec3{x*cs + z*sn + center.X, y + center.Y, -x*sn + z*cs + center.Z}
	}
	return out
}

// Corner index of each face corner of the unit box, the face corners read as sign bits
var boxCornerOf = func() (out [6][4]int) {
	for i, f := range boxFaces {
		for c, p := range f {
			k := 0
			if p[0] > 0 {
				k |= 1
			}
			if p[1] > 0 {
				k |= 2
			}
			if p[2] > 0 {
				k |= 4
			}
			out[i][c] = k
		}
	}
	return out
}()

// Corners of the six faces of a unit box around the origin, wound to face outward
var boxFaces = [6][4][3]float64{
	{{-1, 1, -1}, {-1, 1, 1}, {1, 1, 1}, {1, 1, -1}},
	{{-1, -1, 1}, {-1, -1, -1}, {1, -1, -1}, {1, -1, 1}},
	{{-1, -1, -1}, {-1, 1, -1}, {1, 1, -1}, {1, -1, -1}},
	{{1, -1, 1}, {1, 1, 1}, {-1, 1, 1}, {-1, -1, 1}},
	{{-1, -1, 1}, {-1, 1, 1}, {-1, 1, -1}, {-1, -1, -1}},
	{{1, -1, -1}, {1, 1, -1}, {1, 1, 1}, {1, -1, 1}},
}

// Outward normal of each box face, top, bottom, north, south, west, east
var boxNormals = [6]Vec3{{0, 1, 0}, {0, -1, 0}, {0, 0, -1}, {0, 0, 1}, {-1, 0, 0}, {1, 0, 0}}

// Flat face on the floor around a point, for shadows and light pools
func FloorQuad(at Vec3, w, d, y float64) [4]Vec3 {
	return [4]Vec3{{at.X - w, y, at.Z - d}, {at.X - w, y, at.Z + d}, {at.X + w, y, at.Z + d}, {at.X + w, y, at.Z - d}}
}
