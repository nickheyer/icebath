package vxl

import (
	"math"
	"runtime"
	"sync"
	"sync/atomic"
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

// Flat face in the world with a color, a light per corner and how it blends
type Quad struct {
	P     [4]Vec3
	Color RGB
	// Light each corner reflects, multiplied into the color
	Light [4]RGB
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

// Corner of a face in view space with its light
type vertex struct {
	p     Vec3
	light RGB
}

// Screen space triangle with everything its pixels need, binned by row band
type tri struct {
	x, y, z [3]float64
	l       [3]RGB
	col     RGB
	alpha   float64
	x0, x1  int32
	y0, y1  int32
	flags   uint8
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
	// Point lights Shade adds
	Lights []Light
	// Goroutines Flush draws with, the CPU count when zero
	Workers int
	scratch []vertex
	tris    []tri
	bands   [][]int32
	bandH   int
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
}

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

// Fills the light of every corner from the sun, the ambient level and the point lights in reach
func (r *Raster) Shade(q *Quad, n Vec3) {
	base := r.Ambient.Add(r.SunColor.Scale(max(n.Dot(r.Sun), 0)))
	for c := range 4 {
		l := base
		for i := range r.Lights {
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
func (r *Raster) Quad(q Quad) {
	if q.Alpha <= 0 {
		return
	}
	if !q.TwoSided {
		n := q.P[1].Sub(q.P[0]).Cross(q.P[3].Sub(q.P[0]))
		if n.Dot(r.eye.Sub(q.P[0])) <= 0 {
			return
		}
	}
	poly := r.scratch[:0]
	behind := 0
	for i := range 4 {
		v := r.view(q.P[i])
		if v.Z < nearPlane {
			behind++
		}
		poly = append(poly, vertex{p: v, light: q.Light[i]})
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
	for i := 1; i+1 < len(poly); i++ {
		r.tri(poly[0], poly[i], poly[i+1], &q, i > 1)
	}
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
			out = append(out, vertex{p: a.p.Lerp(b.p, t), light: a.light.Mix(b.light, t)})
		}
	}
	return append(in[:0], out...)
}

// Queues one view space triangle with its screen corners and bounds
func (r *Raster) tri(a, b, c vertex, q *Quad, strict bool) {
	fr := r.fr
	var t tri
	t.x[0], t.y[0], t.z[0] = r.screen(a.p)
	t.x[1], t.y[1], t.z[1] = r.screen(b.p)
	t.x[2], t.y[2], t.z[2] = r.screen(c.p)
	x0 := int(math.Floor(min(t.x[0], t.x[1], t.x[2])))
	x1 := int(math.Ceil(max(t.x[0], t.x[1], t.x[2])))
	y0 := int(math.Floor(min(t.y[0], t.y[1], t.y[2])))
	y1 := int(math.Ceil(max(t.y[0], t.y[1], t.y[2])))
	if x1 < 0 || y1 < 0 || x0 >= fr.W || y0 >= fr.H {
		return
	}
	t.x0, t.x1 = int32(max(x0, 0)), int32(min(x1, fr.W-1))
	t.y0, t.y1 = int32(max(y0, 0)), int32(min(y1, fr.H-1))
	t.l[0], t.l[1], t.l[2] = a.light.Scale(t.z[0]), b.light.Scale(t.z[1]), c.light.Scale(t.z[2])
	t.col, t.alpha = q.Color, q.Alpha
	if q.Additive {
		t.flags |= triAdditive
	} else if q.Alpha >= 1 {
		t.flags |= triWrite
	}
	if strict {
		t.flags |= triStrict
	}
	r.tris = append(r.tris, t)
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
	var next atomic.Int32
	var wg sync.WaitGroup
	for range min(workers, n) {
		wg.Go(func() {
			for {
				b := int(next.Add(1)) - 1
				if b >= n {
					return
				}
				y0, y1 := b*r.bandH, min((b+1)*r.bandH, fr.H)-1
				for _, i := range r.bands[b] {
					r.fill(&r.tris[i], y0, y1)
				}
			}
		})
	}
	wg.Wait()
	r.tris = r.tris[:0]
}

// Fills the rows of one triangle between two rows with depth test, perspective correct light, fog and blending
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
			}
		}
	}
}

// Box turned by a yaw around its own center, shaded by the raster on its turned sides
func (r *Raster) OrientedBox(center, size Vec3, yaw float64, col RGB, alpha float64) {
	hx, hy, hz := size.X/2, size.Y/2, size.Z/2
	corner := func(sx, sy, sz float64) Vec3 {
		return Vec3{sx * hx, sy * hy, sz * hz}.RotateY(yaw).Add(center)
	}
	for i, f := range boxFaces {
		n := boxNormals[i].RotateY(yaw)
		var q Quad
		for c := range 4 {
			q.P[c] = corner(f[c][0], f[c][1], f[c][2])
		}
		q.Color, q.Alpha = col, alpha
		r.Shade(&q, n)
		r.Quad(q)
	}
}

// Box lit evenly, for things that glow on their own
func (r *Raster) EmissiveBox(center, size Vec3, yaw float64, col RGB, alpha float64, light float64) {
	hx, hy, hz := size.X/2, size.Y/2, size.Z/2
	corner := func(sx, sy, sz float64) Vec3 {
		return Vec3{sx * hx, sy * hy, sz * hz}.RotateY(yaw).Add(center)
	}
	for _, f := range boxFaces {
		var p [4]Vec3
		for c := range 4 {
			p[c] = corner(f[c][0], f[c][1], f[c][2])
		}
		q := FlatQuad(p, col, light)
		q.Alpha = alpha
		r.Quad(q)
	}
}

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
