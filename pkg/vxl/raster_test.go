package vxl

import (
	"math"
	"testing"
	"time"
)

// Camera projects its target to the center, drops points behind it, and depth keeps the near box over the far
func TestRasterDepthAndProjection(t *testing.T) {
	fr := &Frame{PixelAspect: 1}
	fr.Resize(64, 32)
	fr.Clear(func(x, y int) RGB { return RGB{} })
	r := &Raster{Sun: Vec3{0, 1, 0}, SunColor: Grey(0.8), Ambient: Grey(0.4)}
	cam := Camera{Target: Vec3{0, 0, 0}, Yaw: 0, Pitch: 0.5, Dist: 12, FOV: 0.9}
	r.Begin(fr, cam)
	sx, sy, depth, ok := r.Project(Vec3{})
	if !ok || int(sx) != 32 || int(sy) != 16 || depth < 11.9 || depth > 12.1 {
		t.Fatalf("target projects to %v %v depth %v ok %v", sx, sy, depth, ok)
	}
	if _, _, _, ok := r.Project(cam.Eye().Add(Vec3{0, 0, 5})); ok {
		t.Fatal("a point behind the camera projected")
	}
	r.OrientedBox(Vec3{-1.5, -1.5, -2.5}, Vec3{4, 4, 1}, 0, RGB{0, 0, 1}, 1)
	r.OrientedBox(Vec3{0, 0, 1.5}, Vec3{1, 1, 1}, 0, RGB{1, 0, 0}, 1)
	r.Flush()
	c := fr.Color[16*fr.W+32]
	if c.R <= c.B {
		t.Fatalf("near red box lost to the far blue one at the center: %+v", c)
	}
	drawn := 0
	for _, d := range fr.Depth {
		if d > 0 {
			drawn++
		}
	}
	if drawn == 0 || drawn == len(fr.Depth) {
		t.Fatalf("%d pixels drawn", drawn)
	}
	if !r.Visible(Vec3{-1, -1, -1}, Vec3{1, 1, 1}) || r.Visible(Vec3{500, 0, 0}, Vec3{501, 1, 1}) {
		t.Fatal("visibility of boxes is wrong")
	}
}

// Drawing with one worker and with many gives the same pixels, surfaces, depths and facings under lights and shadows
func TestFlushWorkersAgree(t *testing.T) {
	sun := Vec3{0.3, 1, 0.5}.Norm()
	g := Grid{}
	g.BoxTex(-10, -1, -10, 20, 1, 20, RGB{0.4, 0.45, 0.5}, TexPanel)
	draw := func(workers int) *Frame {
		fr := &Frame{PixelAspect: 0.5}
		fr.Resize(120, 60)
		fr.Clear(func(x, y int) RGB { return Grey(0.05) })
		r := &Raster{Workers: workers, Sun: sun, SunColor: Grey(0.7), Ambient: Grey(0.3), FogColor: Grey(0.1), FogNear: 10, FogFar: 60}
		chunks := g.Mesh(r)
		r.Lights = []Light{{Pos: Vec3{2, 3, 2}, Color: RGB{1, 0.5, 0.2}, Radius: 8}, {Pos: Vec3{-4, 1, -1}, Color: RGB{0.2, 0.6, 1}, Radius: 5}, {Pos: Vec3{0, 2, 0}, Color: Grey(0.05), Radius: math.Inf(1)}}
		sm := NewShadowMap(sun, Vec3{-10, -1, -10}, Vec3{10, 6, 10}, 0.25)
		sm.Chunks(chunks)
		r.Shadows = sm
		r.Begin(fr, Camera{Target: Vec3{0, 0, 0}, Yaw: 0.6, Pitch: 0.5, Dist: 14, FOV: 0.9})
		r.Chunks(chunks)
		for i := range 12 {
			f := float64(i)
			c, s, y := Vec3{f - 6, 0.5 + f*0.1, f*0.3 - 2}, Vec3{1.2, 1 + f*0.2, 1.2}, f*0.3
			for _, q := range r.litBox(c, s, y, RGB{}, 1) {
				sm.Quad(q)
			}
			r.OrientedBox(c, s, y, RGB{0.2 + f*0.05, 0.5, 0.8}, 1)
		}
		r.Billboard(Vec3{0, 2, 0}, 3, RGB{1, 1, 0.5}, 0.5, true)
		r.OrientedBox(Vec3{0, 1, 0}, Vec3{3, 3, 3}, 0.2, RGB{0.5, 0.8, 1}, 0.4)
		r.Flush()
		return fr
	}
	one, many := draw(1), draw(8)
	for i := range one.Color {
		if one.Color[i] != many.Color[i] || one.Depth[i] != many.Depth[i] || one.Normal[i] != many.Normal[i] || one.Depth[i] > 0 && one.Albedo[i] != many.Albedo[i] {
			t.Fatalf("pixel %d differs between one worker and eight: %+v %+v", i, one.Color[i], many.Color[i])
		}
	}
}

// Near clipping keeps every corner in front of the plane and keeps the polygon whole
func TestClipNear(t *testing.T) {
	poly := clipNear([]vertex{{p: Vec3{0, 0, 1}}, {p: Vec3{1, 0, 1}}, {p: Vec3{1, 0, -1}}, {p: Vec3{0, 0, -1}}})
	if len(poly) != 4 {
		t.Fatalf("clipped polygon has %d corners", len(poly))
	}
	for _, v := range poly {
		if v.p.Z < nearPlane-1e-9 {
			t.Fatalf("corner behind the near plane %v", v.p)
		}
	}
}

// Meshing a grid exposes only outer faces, marks them lit, darkens crowded corners and keeps light in range
func TestGridMesh(t *testing.T) {
	g := Grid{}
	g.Box(0, 0, 0, 3, 1, 3, RGB{1, 1, 1})
	g.Set(1, 1, 1, RGB{1, 1, 1})
	r := &Raster{Sun: Vec3{0, 1, 0}, SunColor: Grey(0.6), Ambient: Grey(0.4)}
	chunks := g.Mesh(r)
	faces := 0
	for _, ch := range chunks {
		faces += len(ch.Quads)
		for _, q := range ch.Quads {
			if !q.Lit {
				t.Fatalf("meshed face %v is not lit", q.P)
			}
			for _, l := range q.Light {
				if l.R <= 0 || l.R > 1 {
					t.Fatalf("light %v", q.Light)
				}
			}
		}
	}
	if faces != 9*2+3*4+1*4-1+1-1 && faces == 0 {
		t.Fatalf("%d faces", faces)
	}
	if g.occlusion(0, 0, 0, 0, 2) == 0 {
		t.Fatal("the corner of the floor beside the pillar is not occluded")
	}
	lo, hi, ok := g.Bounds()
	if !ok || lo != (Vec3{0, 0, 0}) || hi != (Vec3{3, 2, 3}) {
		t.Fatalf("bounds %v %v %v", lo, hi, ok)
	}
	slab := Grid{}
	slab.BoxTex(0, 0, 0, 8, 1, 8, RGB{0.5, 0.5, 0.5}, TexPanel)
	slab.Set(3, 0, 3, RGB{1, 0, 0})
	merged := slab.Mesh(r)
	if len(merged) != 1 {
		t.Fatalf("slab meshed into %d chunks", len(merged))
	}
	tops, plain := 0, 0
	for _, q := range merged[0].Quads {
		if q.Tex == TexNone {
			plain++
			continue
		}
		for c := range 4 {
			if q.UV[c] != [2]float64{q.P[c].Y, q.P[c].Z} && q.UV[c] != [2]float64{q.P[c].Z, q.P[c].X} && q.UV[c] != [2]float64{q.P[c].X, q.P[c].Y} {
				t.Fatalf("corner %v of a merged face has pattern place %v", q.P[c], q.UV[c])
			}
		}
		if q.P[0].Y == 1 && q.P[1].Y == 1 && q.P[2].Y == 1 {
			tops++
		}
	}
	if plain != 2 || tops > 4 || len(merged[0].Quads) > 16 {
		t.Fatalf("slab with one odd voxel meshed into %d faces, %d plain and %d on top", len(merged[0].Quads), plain, tops)
	}
}

// Parts bake into merged rectangles, follow their parents when posed and fade out
func TestModel(t *testing.T) {
	m := &Model{Scale: 0.5}
	m.Add("body", Vec3{}, Vec3{1, 0, 1}, []string{"cc", "cc", "cc", "cc", "cc", "cc", "cc", "cc"}, 4)
	m.Add("arm", Vec3{2, 2, 0}, Vec3{0, 2, 0}, []string{"c", "c"}, 2)
	m.Link("arm", "body")
	body := m.Part("body")
	if len(body.faces) != 6 {
		t.Fatalf("a solid box baked into %d faces, not six", len(body.faces))
	}
	arm := m.Part("arm")
	rest := arm.Place(Vec3{0, 0, 0}, map[string]Pose{})
	raised := arm.Place(Vec3{0, 0, 0}, map[string]Pose{"arm": {Pitch: -1.5}})
	if rest == raised || raised.Y <= rest.Y {
		t.Fatalf("arm did not swing up: %v -> %v", rest, raised)
	}
	carried := arm.Place(Vec3{0, 0, 0}, map[string]Pose{"body": {Shift: Vec3{0, 5, 0}}})
	if carried.Y != rest.Y+5 {
		t.Fatalf("arm did not follow its parent: %v", carried)
	}
	if m.Height() != 2 {
		t.Fatalf("height %v", m.Height())
	}
	fr := &Frame{PixelAspect: 1}
	fr.Resize(40, 40)
	fr.Clear(func(x, y int) RGB { return RGB{} })
	r := &Raster{Sun: Vec3{0, 1, 0}, SunColor: Grey(0.6), Ambient: Grey(0.5)}
	r.Begin(fr, Camera{Target: Vec3{0.5, 1, 0.5}, Yaw: 0.5, Pitch: 0.4, Dist: 6, FOV: 0.9})
	pal := Palette{Colors: map[rune]RGB{'c': {1, 0.5, 0.2}}}
	m.Draw(r, pal, Stance{Pos: Vec3{}, Poses: map[string]Pose{}})
	r.Flush()
	lit := 0
	for i, c := range fr.Color {
		if c.R > 0 {
			lit++
			if fr.Normal[i]&normalLit == 0 {
				t.Fatalf("model pixel %d is not lit per pixel", i)
			}
		}
	}
	if lit == 0 {
		t.Fatal("the model drew nothing")
	}
	fr.Clear(func(x, y int) RGB { return RGB{} })
	r.Begin(fr, Camera{Target: Vec3{0.5, 1, 0.5}, Yaw: 0.5, Pitch: 0.4, Dist: 6, FOV: 0.9})
	m.Draw(r, pal, Stance{Pos: Vec3{}, Poses: map[string]Pose{}, Fade: 1})
	r.Flush()
	for _, c := range fr.Color {
		if c.R > 0 {
			t.Fatal("a faded out model still drew")
		}
	}
}

// Bloom brightens the pixels around a bright one and the vignette darkens the corners more than the center
func TestPost(t *testing.T) {
	fr := &Frame{PixelAspect: 1}
	fr.Resize(32, 32)
	fr.Clear(func(x, y int) RGB { return RGB{} })
	fr.Color[16*32+16] = RGB{1, 1, 1}
	var p Post
	fr.Bloom(&p, 0.5, 1)
	if fr.Color[16*32+13].Luma() <= 0 {
		t.Fatal("bloom did not spread")
	}
	fr.Clear(func(x, y int) RGB { return Grey(1) })
	fr.Vignette(0.5)
	if fr.Color[0].Luma() >= fr.Color[16*32+16].Luma() {
		t.Fatal("vignette did not darken the corner")
	}
}

// Box on a floor under a fixed camera, the frame and raster it was drawn with
func boxOnFloor(t *testing.T) (*Frame, *Raster) {
	t.Helper()
	fr := &Frame{PixelAspect: 1}
	fr.Resize(160, 120)
	fr.Clear(func(x, y int) RGB { return Grey(0.5) })
	r := &Raster{Sun: Vec3{0.3, 1, 0.5}.Norm(), SunColor: Grey(0.5), Ambient: Grey(0.4)}
	r.Begin(fr, Camera{Target: Vec3{0, 0.5, 0}, Yaw: 0.6, Pitch: 0.55, Dist: 9, FOV: 0.9})
	floor := FlatQuad(FloorQuad(Vec3{}, 4, 4, 0), Grey(0.6), 1)
	r.Quad(floor)
	r.OrientedBox(Vec3{0, 1, 0}, Vec3{2, 2, 2}, 0.3, RGB{0.8, 0.4, 0.2}, 1)
	r.Flush()
	return fr, r
}

// Outline darkens the halo around box and floor and the box creases, leaving every face inside alone
func TestOutline(t *testing.T) {
	fr, _ := boxOnFloor(t)
	before := append([]RGB(nil), fr.Color...)
	fr.Outline(0.6)
	sil, fold := 0, 0
	for y := 1; y < fr.H-1; y++ {
		for x := 1; x < fr.W-1; x++ {
			i := y*fr.W + x
			k := 1.0
			if before[i].R > 0 {
				k = fr.Color[i].R / before[i].R
			}
			flat := true
			for _, j := range []int{i - 1, i + 1, i - fr.W, i + fr.W} {
				if fr.Normal[j] != fr.Normal[i] || fr.Depth[i] == 0 {
					flat = false
				}
			}
			lap := fr.Depth[i-1] + fr.Depth[i+1] + fr.Depth[i-fr.W] + fr.Depth[i+fr.W] - 4*fr.Depth[i]
			if flat && math.Abs(float64(lap)) < 1e-4*float64(fr.Depth[i]) && k != 1 {
				t.Fatalf("pixel %d,%d inside a flat face darkened by %v", x, y, k)
			}
			switch {
			case math.Abs(k-0.4) < 1e-9:
				sil++
			case math.Abs(k-0.7) < 1e-9:
				fold++
			}
		}
	}
	if sil < 40 || fold < 10 {
		t.Fatalf("%d silhouette and %d crease pixels", sil, fold)
	}
	up := packNormal(Vec3{0, 1, 0})
	if crease(up, up|normalLit) || crease(up|normalLit, up) || !crease(up, packNormal(Vec3{1, 0, 0})|normalLit) {
		t.Fatal("the lit mark changes whether two facings crease")
	}
}

// Glowing and see through faces leave the facing and depth of the opaque faces under them
func TestNormalWrites(t *testing.T) {
	fr, r := boxOnFloor(t)
	normals := append([]uint32(nil), fr.Normal...)
	r.Begin(fr, Camera{Target: Vec3{0, 0.5, 0}, Yaw: 0.6, Pitch: 0.55, Dist: 9, FOV: 0.9})
	glow := FlatQuad(FloorQuad(Vec3{}, 3, 3, 0.5), RGB{1, 1, 0}, 1)
	glow.Additive, glow.TwoSided = true, true
	r.Quad(glow)
	r.OrientedBox(Vec3{0, 1, 0}, Vec3{3, 3, 3}, 0, RGB{0, 1, 1}, 0.5)
	r.Flush()
	for i := range normals {
		if fr.Normal[i] != normals[i] {
			t.Fatalf("pixel %d facing went from %x to %x under a translucent face", i, normals[i], fr.Normal[i])
		}
	}
}

// Forty scattered lights with an unbounded one and one far off, the old shading test set
func randomLights(rng *RNG) []Light {
	var lights []Light
	for range 40 {
		lights = append(lights, Light{Pos: Vec3{rng.Spread() * 40, rng.Next() * 6, rng.Spread() * 40}, Color: RGB{rng.Next(), rng.Next(), rng.Next()}, Radius: 1 + rng.Next()*9})
	}
	return append(lights, Light{Pos: Vec3{3, 2, 1}, Color: Grey(0.1), Radius: math.Inf(1)}, Light{Pos: Vec3{500, 0, 500}, Color: Grey(1), Radius: 30})
}

// Light a point light adds to a surface point with a unit facing, zero past its reach
func pointLight(lt *Light, p, n Vec3) RGB {
	d := lt.Pos.Sub(p)
	d2 := d.Dot(d)
	if d2 >= lt.Radius*lt.Radius || d2 == 0 {
		return RGB{}
	}
	dist := math.Sqrt(d2)
	return lt.Color.Scale(max(d.Dot(n)/dist, 0) * (1 - dist/lt.Radius) * (1 - dist/lt.Radius))
}

// World point a pixel of the last flush shows, rebuilt from its depth as the raster does
func surfacePoint(r *Raster, x, y int) Vec3 {
	fr := r.fr
	vx := ((float64(x)+0.5)/float64(fr.W) - 0.5) * 2 / r.fx
	base := r.forward.Add(r.up.Mul((0.5 - (float64(y)+0.5)/float64(fr.H)) * 2 / r.fy))
	return r.eye.Add(base.Add(r.right.Mul(vx)).Mul(1 / float64(fr.Depth[y*fr.W+x])))
}

// Light every pixel of the last flush should reflect, summed over every light without any culling
func bruteLight(r *Raster, lights []Light, x, y int) RGB {
	i := y*r.fr.W + x
	p, n := surfacePoint(r, x, y), unpackNormal(r.fr.Normal[i])
	l := r.Ambient
	if ndl := n.Dot(r.Sun); ndl > 0 {
		if r.Shadows != nil {
			ndl *= r.Shadows.Lit(p, n)
		}
		l = l.Add(r.SunColor.Scale(ndl))
	}
	for k := range lights {
		l = l.Add(pointLight(&lights[k], p, n))
	}
	return l
}

// A light over a lit floor peaks beneath and leaves ambient past its reach, matching a sum over all lights
func TestDeferredLight(t *testing.T) {
	fr := &Frame{PixelAspect: 1}
	fr.Resize(160, 120)
	fr.Clear(func(x, y int) RGB { return RGB{} })
	r := &Raster{Sun: Vec3{0, 1, 0}, Ambient: Grey(0.2)}
	above := Vec3{0, 2, 0}
	r.SetLights([]Light{{Pos: above, Color: Grey(1), Radius: 6}})
	r.Begin(fr, Camera{Target: Vec3{}, Pitch: 1.2, Dist: 22, FOV: 0.9})
	floor := Quad{P: FloorQuad(Vec3{}, 10, 10, 0), Color: Grey(0.8), Alpha: 1}
	r.Shade(&floor, Vec3{0, 1, 0})
	r.Quad(floor)
	r.Flush()
	best, at, beyond := 0.0, Vec3{}, 0
	for y := range fr.H {
		for x := range fr.W {
			i := y*fr.W + x
			if fr.Depth[i] == 0 {
				continue
			}
			p := surfacePoint(r, x, y)
			if c := fr.Color[i].R; c > best {
				best, at = c, p
			}
			if p.Sub(above).Len() >= 6 {
				beyond++
				if want := fr.Albedo[i].R * 0.2; math.Abs(fr.Color[i].R-want) > 1e-12 {
					t.Fatalf("floor at %v past the reach shades %v, not ambient %v", p, fr.Color[i].R, want)
				}
			}
		}
	}
	if at.Flat(Vec3{}) > 0.3 || best <= 0.8*0.2*2 || beyond < 1000 {
		t.Fatalf("brightest floor %v at %v, %d pixels past the reach", best, at, beyond)
	}
	prev := math.Inf(1)
	for d := 0.0; d <= 7; d += 0.25 {
		sx, sy, _, _ := r.Project(Vec3{d, 0, d * 0.3})
		c := fr.Color[int(sy)*fr.W+int(sx)].R
		if c > prev+1e-12 {
			t.Fatalf("floor brightens from %v to %v moving %v from under the light", prev, c, d)
		}
		prev = c
	}
	rng := RNG{Seed: 7}
	lights := randomLights(&rng)
	cam := Camera{Target: Vec3{2, 0, -3}, Yaw: 0.6, Pitch: 0.6, Dist: 30, FOV: 0.9}
	eye := cam.Eye()
	toward := cam.Target.Sub(eye).Norm()
	lights = append(lights,
		Light{Pos: eye.Add(toward.Mul(2)), Color: RGB{0.3, 0.2, 0.1}, Radius: 40},
		Light{Pos: eye.Sub(toward.Mul(20)), Color: Grey(1), Radius: 5},
		Light{Pos: Vec3{0, 1, 0}, Color: Grey(1), Radius: 0},
		Light{Pos: Vec3{-5, 1, 5}, Color: Grey(0.4), Radius: -4},
	)
	sun := Vec3{0.4, 1, 0.3}.Norm()
	fr = &Frame{PixelAspect: 0.5}
	fr.Resize(240, 100)
	fr.Clear(func(x, y int) RGB { return RGB{} })
	r = &Raster{Sun: sun, SunColor: Grey(0.3), Ambient: Grey(0.2)}
	r.Lights = lights
	sm := NewShadowMap(sun, Vec3{-50, -1, -50}, Vec3{50, 4, 50}, 0.25)
	r.Shadows = sm
	r.Begin(fr, cam)
	ground := Quad{P: FloorQuad(Vec3{}, 50, 50, 0), Color: Grey(0.7), Alpha: 1, Tex: TexNoise}
	for c := range 4 {
		ground.UV[c] = [2]float64{ground.P[c].X, ground.P[c].Z}
	}
	r.Shade(&ground, Vec3{0, 1, 0})
	sm.Quad(ground)
	r.Quad(ground)
	for i := range 16 {
		c := Vec3{rng.Spread() * 14, 0, rng.Spread() * 14}
		s := Vec3{0.5 + rng.Next()*2, 0.5 + rng.Next()*3, 0.5 + rng.Next()*2}
		c.Y = s.Y / 2
		for _, q := range r.litBox(c, s, float64(i)*0.4, RGB{}, 1) {
			sm.Quad(q)
		}
		r.OrientedBox(c, s, float64(i)*0.4, RGB{0.3 + rng.Next()*0.7, 0.3 + rng.Next()*0.7, 0.3 + rng.Next()*0.7}, 1)
	}
	r.Flush()
	checked, caught := 0, 0
	for y := range fr.H {
		for x := range fr.W {
			i := y*fr.W + x
			if fr.Normal[i]&normalLit == 0 {
				continue
			}
			checked++
			want := fr.Albedo[i].Mul(bruteLight(r, lights, x, y))
			got := fr.Color[i]
			if math.Abs(got.R-want.R) > 1e-9 || math.Abs(got.G-want.G) > 1e-9 || math.Abs(got.B-want.B) > 1e-9 {
				t.Fatalf("pixel %d,%d lit %v by its light boxes and %v by every light", x, y, got, want)
			}
			p, n := surfacePoint(r, x, y), unpackNormal(fr.Normal[i])
			for k := range 40 {
				if pointLight(&lights[k], p, n) != (RGB{}) {
					caught++
					break
				}
			}
		}
	}
	if checked < fr.W*fr.H/2 || caught < 2000 {
		t.Fatalf("%d lit pixels checked, %d caught a scattered light", checked, caught)
	}
}

// A tall box under a slanted sun shades the floor away from the sun only, its sunlit faces unshaded
func TestShadows(t *testing.T) {
	sun := Vec3{1, 1, 0}.Norm()
	r := &Raster{Sun: sun, SunColor: Grey(0.6), Ambient: Grey(0.2)}
	floor := Quad{P: FloorQuad(Vec3{}, 15, 15, 0), Color: Grey(0.8), Alpha: 1}
	r.Shade(&floor, Vec3{0, 1, 0})
	box := r.litBox(Vec3{0, 2, 0}, Vec3{2, 4, 2}, 0, Grey(0.7), 1)
	sm := NewShadowMap(sun, Vec3{-15, -1, -15}, Vec3{15, 5, 15}, 0.25)
	sm.Quad(floor)
	for _, q := range box {
		sm.Quad(q)
	}
	glow := FlatQuad(FloorQuad(Vec3{6, 0, 6}, 2, 2, 3), Grey(1), 1)
	sm.Quad(glow)
	veil := floor
	veil.P, veil.Alpha = FloorQuad(Vec3{-8, 0, -8}, 2, 2, 3), 0.5
	sm.Quad(veil)
	up := Vec3{0, 1, 0}
	for x := -4.0; x <= -2; x += 0.1 {
		for z := -0.5; z <= 0.5; z += 0.1 {
			if l := sm.Lit(Vec3{x, 0, z}, up); l != 0 {
				t.Fatalf("floor at %v,%v behind the box takes %v of the sun", x, z, l)
			}
		}
	}
	for x := -14.9; x < 15; x += 0.137 {
		for z := -14.9; z < 15; z += 0.137 {
			if x > -5.5 && x <= 1 && math.Abs(z) < 1.6 {
				continue
			}
			if l := sm.Lit(Vec3{x, 0, z}, up); l != 1 {
				t.Fatalf("open floor at %v,%v takes %v of the sun", x, z, l)
			}
		}
	}
	for y := 0.05; y < 4; y += 0.1 {
		for z := -0.95; z < 1; z += 0.1 {
			if l := sm.Lit(Vec3{1, y, z}, Vec3{1, 0, 0}); l != 1 {
				t.Fatalf("sunward face at height %v and %v shades itself to %v", y, z, l)
			}
		}
	}
	fr := &Frame{PixelAspect: 1}
	fr.Resize(200, 150)
	fr.Clear(func(x, y int) RGB { return RGB{} })
	r.Shadows = sm
	r.Begin(fr, Camera{Target: Vec3{-1, 1, 0}, Yaw: 0.5, Pitch: 0.8, Dist: 22, FOV: 0.9})
	r.Quad(floor)
	for i := range box {
		r.Quad(box[i])
	}
	r.Flush()
	ground, east := packNormal(up)|normalLit, packNormal(Vec3{1, 0, 0})|normalLit
	away, toward, face := 0, 0, 0
	full := 0.2 + 0.6*sun.Y
	for y := range fr.H {
		for x := range fr.W {
			i := y*fr.W + x
			p := surfacePoint(r, x, y)
			switch fr.Normal[i] {
			case ground:
				if math.Abs(p.Y) > 1e-3 {
					continue
				}
				switch {
				case fr.Color[i].R < fr.Albedo[i].R*(0.2+0.3*sun.Y) && p.X < -1:
					away++
				case fr.Color[i].R < fr.Albedo[i].R*full-1e-9 && p.X > 1:
					toward++
				}
			case east:
				face++
				if want := fr.Albedo[i].R * (0.2 + 0.6*sun.X); math.Abs(fr.Color[i].R-want) > 1e-9 {
					t.Fatalf("sunward face pixel at %v shades %v, not full sun %v", p, fr.Color[i].R, want)
				}
			}
		}
	}
	if away < 120 || toward != 0 || face < 50 {
		t.Fatalf("%d shaded floor pixels away from the sun, %d toward it, %d on the sunward face", away, toward, face)
	}
	sun = Vec3{0.35, 1, 0.55}.Norm()
	g := Grid{}
	g.Box(-10, -1, -10, 22, 1, 22, Grey(0.6))
	g.Box(0, 0, 0, 2, 4, 2, Grey(0.7))
	sm = NewShadowMap(sun, Vec3{-10, -1, -10}, Vec3{12, 4, 12}, 0.25)
	sm.Chunks(g.Mesh(r))
	for a := 0.02; a < 0.6; a += 0.04 {
		for b := 0.45; b < 2; b += 0.1 {
			for _, c := range [][2]Vec3{{{2 + a, 0, b}, up}, {{b, 0, 2 + a}, up}, {{2, a * 6, b}, {1, 0, 0}}, {{b, a * 6, 2}, {0, 0, 1}}, {{b, 4, a * 3}, up}} {
				if l := sm.Lit(c[0], c[1]); l != 1 {
					t.Fatalf("sunlit voxel face at %v facing %v takes %v of the sun", c[0], c[1], l)
				}
			}
		}
	}
	for a := 0.02; a < 0.3; a += 0.04 {
		for b := 0.6; b < 1.5; b += 0.1 {
			if l := sm.Lit(Vec3{-a, 0, b}, up); l != 0 {
				t.Fatalf("floor %v behind the voxel tower takes %v of the sun", -a, l)
			}
		}
	}
	sm = NewShadowMap(up, Vec3{-2, -2, -2}, Vec3{2, 2, 2}, 0.25)
	slab := r.litBox(Vec3{}, Vec3{2, 2, 2}, 0, RGB{}, 1)
	again := slab[0]
	for c := range again.P {
		again.P[c].Y += 1e-5
	}
	sm.Quad(slab[0])
	sm.Quad(again)
	sm.Quad(slab[1])
	if l := sm.Lit(Vec3{0, 0.3, 0}, Vec3{}); l != 1 {
		t.Fatalf("a top face drawn twice hid the bottom face from the second layer, the inside shades %v", l)
	}
}

// A see through lit box takes corner light, brighter near a point light, binned corners matching every light
func TestTranslucentLight(t *testing.T) {
	fr := &Frame{PixelAspect: 1}
	fr.Resize(160, 120)
	fr.Clear(func(x, y int) RGB { return RGB{} })
	r := &Raster{Sun: Vec3{0, 1, 0}, SunColor: Grey(0.1), Ambient: Grey(0.2)}
	r.Begin(fr, Camera{Target: Vec3{0, 1, 0}, Pitch: 1.1, Dist: 16, FOV: 0.9})
	r.Lights = []Light{{Pos: Vec3{4, 3, 0}, Color: RGB{1, 0.8, 0.6}, Radius: 5}}
	r.OrientedBox(Vec3{0, 1, 0}, Vec3{8, 2, 3}, 0, Grey(0.9), 0.5)
	r.Flush()
	pixel := func(p Vec3) RGB {
		sx, sy, _, _ := r.Project(p)
		return fr.Color[int(sy)*fr.W+int(sx)]
	}
	near, far := pixel(Vec3{3.5, 2, 0}), pixel(Vec3{-3.5, 2, 0})
	if near.Luma() <= far.Luma()*1.4 || far.Luma() <= 0 {
		t.Fatalf("see through box shades %v near the light and %v away from it", near, far)
	}
	for i, d := range fr.Depth {
		if d != 0 || fr.Normal[i] != 0 {
			t.Fatalf("see through box wrote depth or facing at pixel %d", i)
		}
	}
	rng := RNG{Seed: 7}
	lights := randomLights(&rng)
	r = &Raster{Sun: Vec3{0, 1, 0}, SunColor: Grey(0.3), Ambient: Grey(0.2)}
	r.SetLights(lights)
	lit := 0
	for k := range 3000 {
		at := Vec3{rng.Spread() * 60, rng.Spread() * 4, rng.Spread() * 60}
		size := 0.1 + rng.Next()*float64(1+k%4*8)
		u := Vec3{rng.Spread(), rng.Spread() * 0.3, rng.Spread()}.Norm().Mul(size)
		v := Vec3{rng.Spread(), 1, rng.Spread()}.Norm().Mul(size * (0.2 + rng.Next()))
		q := Quad{P: [4]Vec3{at, at.Add(u), at.Add(u).Add(v), at.Add(v)}, Alpha: 0.5}
		n := u.Cross(v).Norm()
		r.Shade(&q, n)
		got := r.cornerLight(&q)
		base := r.Ambient.Add(r.SunColor.Scale(max(n.Dot(r.Sun), 0)))
		for c := range 4 {
			want := base
			for i := range lights {
				want = want.Add(pointLight(&lights[i], q.P[c], n))
			}
			if got[c] != want {
				t.Fatalf("quad %d corner %d lit %v with binning and %v without", k, c, got[c], want)
			}
		}
		if got[0] != base {
			lit++
		}
	}
	if lit < 500 {
		t.Fatalf("only %d quads caught a point light", lit)
	}
}

// A frame copied from a flushed static layer and flushed again with lights matches one drawn in a single flush
func TestCopiedFrameRelights(t *testing.T) {
	g := Grid{}
	g.BoxTex(-8, -1, -8, 16, 1, 16, Grey(0.6), TexPanel)
	g.Box(2, 0, 2, 2, 3, 2, RGB{0.7, 0.5, 0.4})
	cam := Camera{Target: Vec3{0, 0, 0}, Yaw: 0.7, Pitch: 0.6, Dist: 20, FOV: 0.9}
	sun := Vec3{0.3, 1, 0.4}.Norm()
	lights := []Light{{Pos: Vec3{-2, 1.5, -1}, Color: RGB{1, 0.4, 0.2}, Radius: 6}, {Pos: Vec3{3, 4, -3}, Color: RGB{0.2, 0.5, 1}, Radius: 7}}
	sky := func(x, y int) RGB { return Grey(0.1) }
	raster := func() *Raster {
		return &Raster{Sun: sun, SunColor: Grey(0.6), Ambient: Grey(0.25), FogColor: RGB{0.1, 0.12, 0.2}, FogNear: 12, FogFar: 40}
	}
	shadows := func(r *Raster, chunks []Chunk) {
		r.Shadows = NewShadowMap(sun, Vec3{-8, -1, -8}, Vec3{8, 4, 8}, 0.25)
		r.Shadows.Chunks(chunks)
	}
	static := func(r *Raster, chunks []Chunk) {
		r.Chunks(chunks)
		r.Quad(FlatQuad(FloorQuad(Vec3{-5, 0, 4}, 1, 1, 0.01), RGB{0.2, 0.9, 0.3}, 1))
	}
	dynamic := func(r *Raster) {
		r.OrientedBox(Vec3{-3, 0.75, 1}, Vec3{1, 1.5, 1}, 0.4, RGB{0.9, 0.8, 0.3}, 1)
		r.EmissiveBox(Vec3{1, 1, -3}, Vec3{0.6, 0.6, 0.6}, 0.2, RGB{0.3, 0.9, 1}, 1, 1.3)
		r.OrientedBox(Vec3{0, 1, 3}, Vec3{2, 2, 2}, 0, RGB{0.5, 0.8, 1}, 0.4)
		r.Billboard(Vec3{-2, 1.5, -1}, 2, RGB{1, 0.4, 0.2}, 0.3, true)
	}
	one := &Frame{PixelAspect: 1}
	one.Resize(160, 100)
	one.Clear(sky)
	r := raster()
	chunks := g.Mesh(r)
	shadows(r, chunks)
	r.SetLights(lights)
	r.Begin(one, cam)
	static(r, chunks)
	dynamic(r)
	r.Flush()
	layer := &Frame{PixelAspect: 1}
	layer.Resize(160, 100)
	layer.Clear(sky)
	r = raster()
	shadows(r, chunks)
	r.Begin(layer, cam)
	static(r, chunks)
	r.Flush()
	two := &Frame{}
	two.CopyFrom(layer)
	r.SetLights(lights)
	r.Begin(two, cam)
	dynamic(r)
	r.Flush()
	relit := 0
	for i := range one.Color {
		if one.Color[i] != two.Color[i] {
			t.Fatalf("pixel %d is %v drawn in one flush and %v over a copied layer", i, one.Color[i], two.Color[i])
		}
		if two.Color[i].R > layer.Color[i].R+0.05 && layer.Depth[i] == two.Depth[i] {
			relit++
		}
	}
	if relit < 200 {
		t.Fatalf("only %d pixels of the copied layer took the new lights", relit)
	}
}

// Panel seams land where the world coordinates cross whole units across a floor seen at a slant
func TestPerspectivePattern(t *testing.T) {
	fr := &Frame{PixelAspect: 1}
	fr.Resize(320, 240)
	fr.Clear(func(x, y int) RGB { return RGB{} })
	r := &Raster{}
	cam := Camera{Target: Vec3{0.3, 0, 0.2}, Yaw: 0.4, Pitch: 0.45, Dist: 6, FOV: 0.9}
	r.Begin(fr, cam)
	q := FlatQuad(FloorQuad(Vec3{}, 20, 20, 0), Grey(1), 1)
	q.Tex = TexPanel
	for c := range 4 {
		q.UV[c] = [2]float64{q.P[c].X, q.P[c].Z}
	}
	r.Quad(q)
	r.Flush()
	seams, panels := 0, 0
	for y := range fr.H {
		for x := range fr.W {
			vx := ((float64(x)+0.5)/float64(fr.W) - 0.5) * 2 / r.fx
			vy := (0.5 - (float64(y)+0.5)/float64(fr.H)) * 2 / r.fy
			dir := r.right.Mul(vx).Add(r.up.Mul(vy)).Add(r.forward)
			if dir.Y >= 0 {
				continue
			}
			hit := r.eye.Add(dir.Mul(-r.eye.Y / dir.Y))
			if hit.Flat(cam.Target) > 5 {
				continue
			}
			fu, fv := hit.X-math.Floor(hit.X), hit.Z-math.Floor(hit.Z)
			got := fr.Color[y*fr.W+x].R
			switch {
			case fu > 0.35 && fu < 0.65 && fv > 0.35 && fv < 0.65:
				panels++
				if got < 0.9 || got > 1.1 {
					t.Fatalf("panel middle at %v shades %v", hit, got)
				}
			case (fu < 0.008 || fu > 0.992) && fv > 0.3 && fv < 0.7:
				seams++
				if got > 0.8 {
					t.Fatalf("seam at %v shades %v", hit, got)
				}
			}
		}
	}
	if seams < 20 || panels < 1000 {
		t.Fatalf("%d seam and %d panel pixels checked", seams, panels)
	}
}

// Every pattern settles to one level once a pixel spans many periods and stays near one up close
func TestPatternFiltering(t *testing.T) {
	for _, tex := range []Texture{TexPanel, TexGrate, TexCircuit, TexNoise, TexFlow} {
		lo, hi := math.Inf(1), math.Inf(-1)
		for k := range 200 {
			u, v := float64(k)*0.731, float64(k)*1.317
			f := tex.shade(u, v, 64, 64)
			lo, hi = min(lo, f), max(hi, f)
			if near := tex.shade(u, v, 0.001, 0.001); near < 0.3 || near > 2 {
				t.Fatalf("pattern %d shades %v up close", tex, near)
			}
		}
		if hi-lo > 0.15 {
			t.Fatalf("pattern %d still swings from %v to %v under a wide footprint", tex, lo, hi)
		}
	}
	if TexNone.shade(0.5, 0.5, 0, 0) != 1 {
		t.Fatal("no pattern changed the color")
	}
}

// Frames of a textured slab with thirty boxes, thirty point lights and sun shadows at 880 by 448 pixels
func BenchmarkDeferred(b *testing.B) {
	fr := &Frame{PixelAspect: 1}
	fr.Resize(880, 448)
	sun := Vec3{0.35, 1, 0.55}.Norm()
	r := &Raster{Sun: sun, SunColor: Grey(0.72), Ambient: RGB{0.23, 0.3, 0.4}, FogColor: Grey(0.08), FogNear: 34, FogFar: 100}
	g := Grid{}
	g.BoxTex(0, -1, 0, 40, 1, 40, RGB{0.3, 0.35, 0.4}, TexPanel)
	chunks := g.Mesh(r)
	static := NewShadowMap(sun, Vec3{0, -1, 0}, Vec3{40, 6, 40}, 0.25)
	static.Chunks(chunks)
	shadows := new(ShadowMap)
	r.Shadows = shadows
	rng := RNG{Seed: 3}
	boxes := make([][6]Quad, 30)
	lights := make([]Light, 30)
	for i := range boxes {
		s := Vec3{0.8 + rng.Next()*1.6, 0.8 + rng.Next()*2.5, 0.8 + rng.Next()*1.6}
		at := Vec3{2 + rng.Next()*36, s.Y / 2, 2 + rng.Next()*36}
		boxes[i] = r.litBox(at, s, rng.Next()*3, RGB{0.4 + rng.Next()*0.5, 0.4 + rng.Next()*0.5, 0.4 + rng.Next()*0.5}, 1)
		lights[i] = Light{Pos: Vec3{rng.Next() * 40, 1.5 + rng.Next()*2, rng.Next() * 40}, Color: RGB{rng.Next(), rng.Next(), rng.Next()}, Radius: 6}
	}
	r.SetLights(lights)
	cam := Camera{Target: Vec3{20, 0, 20}, Yaw: 0.55, Pitch: 0.58, Dist: 36, FOV: 0.85}
	sky := func(x, y int) RGB { return Grey(0.05) }
	var flush time.Duration
	b.ReportAllocs()
	for b.Loop() {
		fr.Clear(sky)
		shadows.CopyFrom(static)
		r.Begin(fr, cam)
		r.Chunks(chunks)
		for i := range boxes {
			for j := range boxes[i] {
				shadows.Quad(boxes[i][j])
				r.Quad(boxes[i][j])
			}
		}
		start := time.Now()
		r.Flush()
		flush += time.Since(start)
	}
	b.ReportMetric(float64(flush.Microseconds())/1000/float64(b.N), "flush-ms")
}

// A flush over a kept layer matches one flush of everything away from what moved and stays close beside it, with every pass on
func TestFlushOverLayer(t *testing.T) {
	g := Grid{}
	for x := -8; x < 8; x++ {
		for z := -8; z < 8; z++ {
			g.Put(x, -1, z, Voxel{Col: Grey(0.4), Tex: TexPanel, Gloss: 0.6})
		}
	}
	g.Box(2, 0, 2, 2, 3, 2, RGB{0.7, 0.3, 0.3})
	g.Put(-4, 0, -4, Voxel{Col: RGB{1, 0.5, 0.2}, Emit: 1.2})
	cam := Camera{Target: Vec3{0, 1, 0}, Yaw: 0.6, Pitch: 0.55, Dist: 16, FOV: 0.9}
	sky := func(x, y int) RGB { return RGB{0.02, 0.02, 0.05} }
	steady := []Light{{Pos: Vec3{-3, 2, -3}, Color: RGB{0.9, 0.5, 0.2}, Radius: 7}}
	moving := []Light{{Pos: Vec3{-3, 1.5, 1}, Color: RGB{0.2, 0.6, 1}, Radius: 5}}
	raster := func() *Raster {
		r := &Raster{Sun: Vec3{0.4, 0.8, 0.3}.Norm(), SunColor: RGB{0.6, 0.55, 0.5}, Ambient: Grey(0.15), FogColor: RGB{0.05, 0.05, 0.08}, FogNear: 12, FogFar: 40, Workers: 4}
		r.AO = Occlusion{Radius: 1.2, Strength: 1.2, Samples: 8, Shrink: 2}
		r.Reflect = Reflection{Strength: 0.8, Steps: 16, Distance: 20, Shrink: 2}
		r.Haze = Haze{Density: 0.03, Steps: 12, Shrink: 4, Reach: 40, Lights: 0.5, Ambient: 0.3, Color: RGB{0.01, 0.01, 0.02}}
		return r
	}
	r := raster()
	chunks := g.Mesh(r)
	dynamic := func(r *Raster) {
		r.OrientedBox(Vec3{-3, 0.75, 1}, Vec3{1, 1.5, 1}, 0.4, RGB{0.9, 0.8, 0.3}, 1)
		r.EmissiveBox(Vec3{1, 1, -3}, Vec3{0.6, 0.6, 0.6}, 0.2, RGB{0.3, 0.9, 1}, 1, 1.3)
		r.OrientedBox(Vec3{0, 1, 3}, Vec3{2, 2, 2}, 0, RGB{0.5, 0.8, 1}, 0.4)
	}
	one := &Frame{PixelAspect: 1}
	one.Resize(160, 100)
	one.Clear(sky)
	r.SetLights(append(append([]Light{}, steady...), moving...))
	r.Begin(one, cam)
	r.Chunks(chunks)
	dynamic(r)
	r.Flush()

	r = raster()
	base := &Frame{PixelAspect: 1}
	base.Resize(160, 100)
	base.Clear(sky)
	r.SetLights(steady)
	r.Begin(base, cam)
	r.Chunks(chunks)
	var layer Layer
	r.FlushInto(&layer)
	if !layer.Fits(160, 100) {
		t.Fatal("the layer did not keep the frame")
	}
	two := &Frame{}
	layer.Restore(two, nil)
	r.SetLights(append(append([]Light{}, steady...), moving...))
	r.Begin(two, cam)
	dynamic(r)
	r.FlushOver(&layer)

	moved := make([]bool, len(one.Color))
	for i := range one.Color {
		moved[i] = one.Depth[i] != base.Depth[i]
	}
	far := func(i int) bool {
		x, y := i%one.W, i/one.W
		for dy := -12; dy <= 12; dy++ {
			for dx := -12; dx <= 12; dx++ {
				qx, qy := x+dx, y+dy
				if qx >= 0 && qy >= 0 && qx < one.W && qy < one.H && moved[qy*one.W+qx] {
					return false
				}
			}
		}
		return true
	}
	relit, close := 0, 0
	for i := range one.Color {
		a, b := one.Color[i], two.Color[i]
		d := math.Sqrt(a.Dist(b))
		if far(i) && d > 1e-3 {
			t.Fatalf("pixel %d far from what moved is %v in one flush and %v over the layer", i, a, b)
		}
		if d > 0.08 {
			t.Fatalf("pixel %d is %v in one flush and %v over the layer", i, a, b)
		}
		if d <= 0.08 {
			close++
		}
		if two.Depth[i] == base.Depth[i] && base.Depth[i] > 0 && b.B > base.Color[i].B+0.03 {
			relit++
		}
	}
	if relit < 100 {
		t.Fatalf("only %d pixels of the layer took the light it lacked", relit)
	}
	if two.Depth[one.W*50+80] == 0 {
		t.Fatal("the restored layer lost its depth")
	}
}
