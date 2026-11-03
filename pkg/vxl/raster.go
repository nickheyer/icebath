package vxl

import (
	"math"
	"runtime"
	"slices"
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

// Bits of a packed facing, its marker, its lit mark and the gloss above them
const (
	normalMarker uint32 = 1 << 24
	normalLit    uint32 = 1 << 25
	normalGloss  uint32 = 63 << 26
	normalDir    uint32 = 1<<25 - 1
)

// Flat face in the world with a color, a light per corner, a pattern and how it blends
type Quad struct {
	P     [4]Vec3
	Color RGB
	// Light each corner reflects, multiplied into the color, only the occlusion once the face is lit
	Light [4]RGB
	// Pattern scaling the color and its place at every corner, in world units for the tiling ones
	Tex Texture
	UV  [4][2]float64
	// Share of the face laid over what is behind it, one covering it
	Alpha float64
	// Share of the light the face mirrors, highlights and reflections growing with it
	Gloss float64
	// Light added instead of laid over, for glows
	Additive bool
	// Face drawn from either side, for decals and billboards
	TwoSided bool
	// Lit by the raster, per pixel when opaque and per corner when see through, set by Shade
	Lit bool
	// Facing Shade gave the face, the one its light is reckoned along
	n Vec3
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

// Point light with the pixel box its reach covers, bounds inclusive
type lightSpan struct {
	pos            Vec3
	col            RGB
	radius, r2     float64
	x0, x1, y0, y1 int
}

// World point, unit facing, view direction, gloss and gathered light of one lit pixel in the row being resolved
type surfel struct {
	p, n, view Vec3
	l, spec    RGB
	gloss      float64
	// Whether the pixel is lit anew, else only the lights the layer under it lacks are added
	full bool
}

// Draws faces through a camera, lit per pixel by sun, ambient, radiance and point lights, with fog, occlusion, reflections and haze
type Raster struct {
	fr      *Frame
	eye     Vec3
	right   Vec3
	up      Vec3
	forward Vec3
	fx, fy  float64
	// Fog toward a color between two view depths
	FogColor RGB
	FogNear  float64
	FogFar   float64
	// Light the fog adds looking toward the sun, none when zero
	FogSun RGB
	// Direction toward the key light, unit length, and its color
	Sun      Vec3
	SunColor RGB
	// Light every face gets from everywhere
	Ambient RGB
	// Baked radiance the ambient light varies by from place to place, nil for even ambient
	GI *Volume
	// Crowding read off the depth buffer darkening the ambient light, off at zero strength
	AO Occlusion
	// Mirror images glossy faces show of what the frame holds, off at zero strength
	Reflect Reflection
	// Light the air scatters toward the camera, off at zero density
	Haze Haze
	// Point lights every lit face takes, binned again by Begin and SetLights
	Lights []Light
	// Depth map the sun is shaded by, nil leaving every face in full sun
	Shadows *ShadowMap
	// Goroutines Flush draws with, the usable CPU count when zero
	Workers int
	scratch []vertex
	tris    []tri
	// Opaque and blended triangles of each row band, in queued order
	solid, blend [][]int32
	bandH        int
	bins         lightBins
	cand         []int32
	// Point lights that may reach a drawn pixel this flush and the ones meeting each band
	spans []lightSpan
	near  [][]int32
	// Sideways and upward view slope of each pixel column and row, and one row of lit pixels per band
	cols   []float64
	rows   []float64
	surf   []surfel
	queued int
	// Occlusion of every pixel, the grid it is read on, its cells before and after the blur and a scratch copy
	ao, aoRaw, ao2, ao3 []float32
	aoGrid              lowGrid
	// Cells of the occlusion grid read again this flush, and the ones a drawn pixel lies in
	redo  []bool
	dirty []int32
	// Generation of the flush that last drew each opaque pixel, and the generation of this flush
	stamp []uint32
	gen   uint32
	// Reflection every cell of the grid gathered
	refl     []RGB
	reflGrid lowGrid
	// Inscattered light along the ray of every cell of the haze grid
	hz hazeField
}

// Points the raster at the frame through the camera, dropping anything queued
func (r *Raster) Begin(fr *Frame, cam Camera) {
	r.fr = fr
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

// Unit direction into the world through a place on the frame, in pixels from its top left corner
func (r *Raster) Ray(x, y float64) Vec3 {
	vx := (x/float64(max(r.fr.W, 1)) - 0.5) * 2 / r.fx
	vy := (0.5 - y/float64(max(r.fr.H, 1))) * 2 / r.fy
	return r.forward.Add(r.right.Mul(vx)).Add(r.up.Mul(vy)).Norm()
}

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

// Marks the face lit by the raster along a facing, every corner light reset to one for occlusion to scale
func (r *Raster) Shade(q *Quad, n Vec3) {
	w := Grey(1)
	q.Lit, q.n, q.Light = true, n, [4]RGB{w, w, w, w}
}

// Corner lights of a face from the ambient, the radiance, the sun and the binned point lights at each corner
func (r *Raster) cornerLight(q *Quad) [4]RGB {
	near := r.lightsNear(q)
	base := r.Ambient.Add(r.SunColor.Scale(max(q.n.Dot(r.Sun), 0)))
	var out [4]RGB
	for c := range 4 {
		l := base
		if r.GI != nil {
			l = l.Add(r.GI.Sample(q.P[c].Add(q.n.Mul(giOffset))))
		}
		for _, i := range near {
			lt := &r.Lights[i]
			d := lt.Pos.Sub(q.P[c])
			d2 := d.Dot(d)
			if d2 >= lt.Radius*lt.Radius || d2 == 0 {
				continue
			}
			dist := math.Sqrt(d2)
			att := 1 - dist/lt.Radius
			l = l.Add(lt.Color.Scale(max(d.Dot(q.n)/dist, 0) * att * att))
		}
		out[c] = q.Light[c].Mul(l)
	}
	return out
}

// Queues a face, clipped to the near plane, back faces skipped unless it is two sided
func (r *Raster) Quad(q Quad) { r.quad(&q) }

// Corner of a face on the screen, its light and pattern place divided by its depth
type svert struct {
	x, y, z float64
	l       RGB
	u, v    float64
}

// Queues a face held by pointer, see through lit faces lit at their corners, each corner projected once
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
	light := q.Light
	var normal uint32
	switch {
	case !q.Additive && q.Alpha >= 1:
		if q.Lit {
			n = q.n
		}
		if facing < 0 {
			n = n.Mul(-1)
		}
		normal = packNormal(n) | packGloss(q.Gloss)
		if q.Lit {
			normal |= normalLit
		}
	case q.Lit && !q.Additive:
		light = r.cornerLight(q)
	}
	poly := r.scratch[:0]
	behind := 0
	for i := range 4 {
		v := r.view(q.P[i])
		if v.Z < nearPlane {
			behind++
		}
		poly = append(poly, vertex{p: v, light: light[i], uv: q.UV[i]})
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
	return b(n.X) | b(n.Y)<<8 | b(n.Z)<<16 | normalMarker
}

// Unit facing held in a packed one
func unpackNormal(v uint32) Vec3 {
	return Vec3{float64(int8(v)), float64(int8(v >> 8)), float64(int8(v >> 16))}.Norm()
}

// Gloss packed into the six bits above the facing
func packGloss(g float64) uint32 { return uint32(min(max(g, 0), 1)*63+0.5) << 26 }

// Gloss held in a packed facing
func unpackGloss(v uint32) float64 { return float64(v>>26&63) / 63 }

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

// Draws opaque faces, reads crowding, lights and fogs them, mirrors the glossy ones, scatters the air, then lays blended faces
func (r *Raster) Flush() { r.flush(nil, nil) }

// Flushes and keeps the frame as lit, before mirroring and haze, in a layer later flushes draw over
func (r *Raster) FlushInto(l *Layer) { r.flush(nil, l) }

// Flushes over a restored layer, lighting anew only the pixels drawn or crowded this flush and adding the lights the layer lacks over the rest
func (r *Raster) FlushOver(l *Layer) {
	if l == nil || !l.Fits(r.fr.W, r.fr.H) {
		r.flush(nil, nil)
		return
	}
	r.flush(l, nil)
}

// Fills, occludes, lights, mirrors, scatters and blends, over a layer when one is given, keeping the lit frame in another when asked
func (r *Raster) flush(l, into *Layer) {
	fr := r.fr
	if fr == nil || fr.W == 0 || fr.H == 0 {
		r.tris = r.tris[:0]
		return
	}
	workers := r.Workers
	if workers <= 0 {
		workers = runtime.GOMAXPROCS(0)
	}
	n := min(max(fr.H/6, 1), max(workers*3, 1))
	r.bandH = (fr.H + n - 1) / n
	n = (fr.H + r.bandH - 1) / r.bandH
	r.solid, r.blend, r.near = resetBands(r.solid, n), resetBands(r.blend, n), resetBands(r.near, n)
	for i := range r.tris {
		t := &r.tris[i]
		bands := r.blend
		if t.flags&triWrite != 0 {
			bands = r.solid
		}
		for b := int(t.y0) / r.bandH; b <= int(t.y1)/r.bandH; b++ {
			bands[b] = append(bands[b], int32(i))
		}
	}
	r.prepLights()
	dyn := make([]bool, len(r.spans))
	for k := range r.spans {
		dyn[k] = !l.has(&r.spans[k])
	}
	r.cols = slices.Grow(r.cols[:0], fr.W)[:fr.W]
	for x := range r.cols {
		r.cols[x] = ((float64(x)+0.5)/float64(fr.W) - 0.5) * 2 / r.fx
	}
	r.rows = slices.Grow(r.rows[:0], fr.H)[:fr.H]
	for y := range r.rows {
		r.rows[y] = (0.5 - (float64(y)+0.5)/float64(fr.H)) * 2 / r.fy
	}
	r.surf = slices.Grow(r.surf[:0], n*fr.W)[:n*fr.W]
	if len(r.stamp) != fr.W*fr.H {
		r.stamp = make([]uint32, fr.W*fr.H)
		r.gen = 0
	}
	r.gen++
	if r.gen == 0 {
		clear(r.stamp)
		r.gen = 1
	}
	parallel(n, workers, func(b int) {
		y0, y1 := b*r.bandH, min((b+1)*r.bandH, fr.H)-1
		for _, i := range r.solid[b] {
			r.fill(&r.tris[i], y0, y1)
		}
	})
	r.markRedo(l)
	if r.AO.Strength > 0 {
		r.occlude(workers, l)
	} else {
		r.ao = r.ao[:0]
	}
	parallel(n, workers, func(b int) {
		y0, y1 := b*r.bandH, min((b+1)*r.bandH, fr.H)-1
		r.resolve(b, y0, y1, l, dyn)
	})
	if into != nil {
		into.keep(r)
	}
	if r.Reflect.Strength > 0 {
		r.reflect(workers)
	}
	if r.Haze.Density > 0 {
		r.scatter(workers, l, dyn, into != nil)
	} else {
		r.hz.fit(0, 0, 1, 1)
	}
	if into != nil {
		into.keepHaze(r)
	}
	parallel(n, workers, func(b int) {
		y0, y1 := b*r.bandH, min((b+1)*r.bandH, fr.H)-1
		for _, i := range r.blend[b] {
			r.fill(&r.tris[i], y0, y1)
		}
	})
	r.tris = r.tris[:0]
}

// Band lists grown to a count and emptied, their storage kept
func resetBands(bands [][]int32, n int) [][]int32 {
	for len(bands) < n {
		bands = append(bands, nil)
	}
	for i := range n {
		bands[i] = bands[i][:0]
	}
	return bands
}

// Lists in index order every point light that may reach a drawn pixel, with the pixel box it covers
func (r *Raster) prepLights() {
	fr := r.fr
	r.spans = r.spans[:0]
	for i := range r.Lights {
		l := &r.Lights[i]
		if l.Radius == 0 {
			continue
		}
		s := lightSpan{pos: l.Pos, col: l.Color, radius: l.Radius, r2: l.Radius * l.Radius, x1: fr.W - 1, y1: fr.H - 1}
		if bounded(l) && !r.reach(&s) {
			continue
		}
		r.spans = append(r.spans, s)
	}
}

// Narrows a light to the pixels its sphere covers past the near plane, false when it covers none
func (r *Raster) reach(s *lightSpan) bool {
	v := r.view(s.pos)
	rad := math.Abs(s.radius)
	zlo, zhi := max(v.Z-rad, nearPlane), v.Z+rad
	if zhi < nearPlane {
		return false
	}
	x0, x1, y0, y1 := math.Inf(1), math.Inf(-1), math.Inf(1), math.Inf(-1)
	for i := range 8 {
		c := Vec3{v.X - rad, v.Y - rad, zlo}
		if i&1 != 0 {
			c.X = v.X + rad
		}
		if i&2 != 0 {
			c.Y = v.Y + rad
		}
		if i&4 != 0 {
			c.Z = zhi
		}
		sx, sy, _ := r.screen(c)
		x0, x1, y0, y1 = min(x0, sx), max(x1, sx), min(y0, sy), max(y1, sy)
	}
	if t := x0 + x1 + y0 + y1; math.IsNaN(t) || math.IsInf(t, 0) {
		return true
	}
	w, h := float64(r.fr.W-1), float64(r.fr.H-1)
	x0, x1, y0, y1 = math.Floor(x0)-1, math.Ceil(x1)+1, math.Floor(y0)-1, math.Ceil(y1)+1
	if x1 < 0 || y1 < 0 || x0 > w || y0 > h {
		return false
	}
	s.x0, s.x1, s.y0, s.y1 = int(max(x0, 0)), int(min(x1, w)), int(max(y0, 0)), int(min(y1, h))
	return true
}

// World units a radiance lookup steps off its surface along the facing
const giOffset = 0.5

// Steps of the half vector cosine the highlight table holds
const specSteps = 256

// Highlight of every packed gloss level at every cosine step, the cosine raised to a power growing with the gloss
var specTable = func() (out [64][specSteps + 1]float32) {
	for g := range 64 {
		gloss := float64(g) / 63
		for i := range specSteps + 1 {
			out[g][i] = float32(math.Pow(float64(i)/specSteps, 4+60*gloss) * gloss)
		}
	}
	return out
}()

// Share of the full highlight a face of some gloss throws, along the half vector of a light and the view
func specular(n, l, view Vec3, gloss float64) float64 {
	hx, hy, hz := l.X+view.X, l.Y+view.Y, l.Z+view.Z
	d := n.X*hx + n.Y*hy + n.Z*hz
	if d <= 0 {
		return 0
	}
	d /= math.Sqrt(hx*hx + hy*hy + hz*hz)
	row := &specTable[int(gloss*63+0.5)]
	f := min(d, 1) * specSteps
	i := int(f)
	if i >= specSteps {
		return float64(row[specSteps])
	}
	t := f - float64(i)
	return float64(row[i]) + (float64(row[i+1])-float64(row[i]))*t
}

// Color of the fog along a unit view direction, the sun glow added toward it
func (r *Raster) fogToward(dir Vec3) RGB {
	k := dir.Dot(r.Sun)
	if k <= 0 {
		return r.FogColor
	}
	k *= k
	k *= k
	k *= k
	return r.FogColor.Add(r.FogSun.Scale(k))
}

// Share of the way a view depth lies through the fog, none before it and all past it
func (r *Raster) fogShare(depth float32) float64 {
	if r.FogFar <= r.FogNear {
		return 0
	}
	return min(max((1/float64(depth)-r.FogNear)/(r.FogFar-r.FogNear), 0), 1)
}

// Lights and fogs the opaque pixels of a band, every one without a layer, else the drawn and crowded ones anew and the rest by the lights the layer lacks
func (r *Raster) resolve(b, rowLo, rowHi int, l *Layer, dyn []bool) {
	fr := r.fr
	w := fr.W
	surf := r.surf[b*w : (b+1)*w]
	near := r.near[b][:0]
	for i := range r.spans {
		if s := &r.spans[i]; s.y0 <= rowHi && s.y1 >= rowLo {
			near = append(near, int32(i))
		}
	}
	r.near[b] = near
	fog := r.FogFar > r.FogNear
	sunFog := r.FogSun != RGB{}
	sm, gi := r.Shadows, r.GI
	var ao []float32
	if len(r.ao) == w*fr.H {
		ao = r.ao
	}
	lg := &r.aoGrid
	for y := rowLo; y <= rowHi; y++ {
		at := y * w
		depth, normal, albedo, color := fr.Depth[at:at+w], fr.Normal[at:at+w], fr.Albedo[at:at+w], fr.Color[at:at+w]
		base := r.forward.Add(r.up.Mul(r.rows[y]))
		var pack uint32
		var n Vec3
		for x, nv := range normal {
			if nv&normalLit == 0 {
				continue
			}
			if nv&normalDir != pack {
				pack, n = nv&normalDir, unpackNormal(nv)
			}
			dir := base.Add(r.right.Mul(r.cols[x]))
			p := r.eye.Add(dir.Mul(1 / float64(depth[x])))
			sf := surfel{p: p, n: n, gloss: unpackGloss(nv), full: l == nil || r.stamp[at+x] == r.gen || r.redo[lg.cell(x, y)]}
			if sf.gloss > 0 {
				sf.view = dir.Mul(-1).Norm()
			}
			if sf.full {
				amb := r.Ambient
				if gi != nil {
					amb = amb.Add(gi.Sample(Vec3{p.X + n.X*giOffset, p.Y + n.Y*giOffset, p.Z + n.Z*giOffset}))
				}
				if ao != nil {
					amb = amb.Scale(float64(ao[at+x]))
				}
				sf.l = amb
				if ndl := n.Dot(r.Sun); ndl > 0 {
					if sm != nil {
						ndl *= sm.Lit(p, n)
					}
					sf.l = sf.l.Add(r.SunColor.Scale(ndl))
					if sf.gloss > 0 && ndl > 0 {
						sf.spec = r.SunColor.Scale(specular(n, r.Sun, sf.view, sf.gloss) * ndl)
					}
				}
			}
			surf[x] = sf
		}
		for _, k := range near {
			s := &r.spans[k]
			if y < s.y0 || y > s.y1 {
				continue
			}
			fresh := dyn[k]
			for x := s.x0; x <= s.x1; x++ {
				if normal[x]&normalLit == 0 {
					continue
				}
				sf := &surf[x]
				if !fresh && !sf.full {
					continue
				}
				d := s.pos.Sub(sf.p)
				d2 := d.Dot(d)
				if d2 >= s.r2 || d2 == 0 {
					continue
				}
				dist := math.Sqrt(d2)
				att := 1 - dist/s.radius
				ndl := d.Dot(sf.n) / dist
				if ndl <= 0 {
					continue
				}
				sf.l = sf.l.Add(s.col.Scale(ndl * att * att))
				if sf.gloss > 0 {
					sf.spec = sf.spec.Add(s.col.Scale(specular(sf.n, d.Mul(1/dist), sf.view, sf.gloss) * ndl * att * att))
				}
			}
		}
		for x, d := range depth {
			if d <= 0 {
				continue
			}
			lit := normal[x]&normalLit != 0
			if lit && !surf[x].full {
				sf := &surf[x]
				if sf.l != (RGB{}) || sf.spec != (RGB{}) {
					color[x] = color[x].Add(albedo[x].Mul(sf.l).Add(sf.spec).Scale(1 - r.fogShare(d)))
				}
				continue
			}
			if !lit && l != nil && r.stamp[at+x] != r.gen {
				continue
			}
			c := albedo[x]
			if lit {
				sf := &surf[x]
				c = c.Mul(sf.l).Add(sf.spec)
			}
			if fog {
				fc := r.FogColor
				if sunFog {
					fc = r.fogToward(base.Add(r.right.Mul(r.cols[x])).Norm())
				}
				c = c.Mix(fc, r.fogShare(d))
			}
			color[x] = c
		}
	}
}

// Fills one triangle between two rows, opaque ones storing their surface, blended ones fogged and laid over
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
	sunFog := r.FogSun != RGB{}
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
		var base Vec3
		if sunFog && !write {
			base = r.forward.Add(r.up.Mul(r.rows[py]))
		}
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
			if write {
				fr.Albedo[i] = col
				fr.Depth[i] = float32(invz)
				fr.Normal[i] = t.normal
				r.stamp[i] = r.gen
				continue
			}
			if fog {
				fc := r.FogColor
				if sunFog {
					fc = r.fogToward(base.Add(r.right.Mul(r.cols[px])).Norm())
				}
				col = col.Mix(fc, (k-r.FogNear)/fogSpan)
			}
			if additive {
				fr.Color[i] = fr.Color[i].Add(col.Scale(t.alpha))
			} else {
				fr.Color[i] = fr.Color[i].Mix(col, t.alpha)
			}
		}
	}
}

// Box turned by a yaw around its own center, lit by the raster on its turned sides
func (r *Raster) OrientedBox(center, size Vec3, yaw float64, col RGB, alpha float64) {
	faces := r.litBox(center, size, yaw, col, alpha)
	for i := range faces {
		r.quad(&faces[i])
	}
}

// Six faces of a box turned by a yaw about its center, wound outward and marked lit along their facings
func (r *Raster) litBox(center, size Vec3, yaw float64, col RGB, alpha float64) [6]Quad {
	corners := boxCorners(center, size, yaw)
	sn, cs := math.Sincos(yaw)
	var out [6]Quad
	for i := range boxFaces {
		n := boxNormals[i]
		q := &out[i]
		for c := range 4 {
			q.P[c] = corners[boxCornerOf[i][c]]
		}
		q.Color, q.Alpha = col, alpha
		r.Shade(q, Vec3{n.X*cs + n.Z*sn, n.Y, -n.X*sn + n.Z*cs})
	}
	return out
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
