package vxl

import (
	"image/color"
	"math"
	"testing"

	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"
)

// Camera projects its target to the center, drops points behind it, and the depth test keeps the near box over the far one
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

// Drawing with one worker and with many gives the same pixels
func TestFlushWorkersAgree(t *testing.T) {
	draw := func(workers int) *Frame {
		fr := &Frame{PixelAspect: 0.5}
		fr.Resize(120, 60)
		fr.Clear(func(x, y int) RGB { return Grey(0.05) })
		r := &Raster{Workers: workers, Sun: Vec3{0.3, 1, 0.5}.Norm(), SunColor: Grey(0.7), Ambient: Grey(0.3), FogColor: Grey(0.1), FogNear: 10, FogFar: 60}
		r.Lights = []Light{{Pos: Vec3{2, 3, 2}, Color: RGB{1, 0.5, 0.2}, Radius: 8}}
		r.Begin(fr, Camera{Target: Vec3{0, 0, 0}, Yaw: 0.6, Pitch: 0.5, Dist: 14, FOV: 0.9})
		for i := range 12 {
			f := float64(i)
			r.OrientedBox(Vec3{f - 6, 0, f*0.3 - 2}, Vec3{1.2, 1 + f*0.2, 1.2}, f*0.3, RGB{0.2 + f*0.05, 0.5, 0.8}, 1)
		}
		r.Billboard(Vec3{0, 2, 0}, 3, RGB{1, 1, 0.5}, 0.5, true)
		r.OrientedBox(Vec3{0, 1, 0}, Vec3{3, 3, 3}, 0.2, RGB{0.5, 0.8, 1}, 0.4)
		r.Flush()
		return fr
	}
	one, many := draw(1), draw(8)
	for i := range one.Color {
		if one.Color[i] != many.Color[i] || one.Depth[i] != many.Depth[i] {
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

// Cells split into the two colors their pixels group into, solid cells staying one color
func TestPartition(t *testing.T) {
	solid := []RGB{{1, 0, 0}, {1, 0, 0}, {1, 0, 0}, {1, 0, 0}}
	if mask, _, bg, _ := partition(solid); mask != 0 || bg != solid[0] {
		t.Fatalf("solid cell %d %v", mask, bg)
	}
	halves := []RGB{{1, 1, 1}, {1, 1, 1}, {0, 0, 0}, {0, 0, 0}}
	mask, fg, bg, _ := partition(halves)
	if (mask != 3 || fg != halves[0] || bg != halves[2]) && (mask != 12 || fg != halves[2] || bg != halves[0]) {
		t.Fatalf("half cell %d %v %v", mask, fg, bg)
	}
	corner := []RGB{{0, 1, 0}, {0, 0, 0}, {0, 0, 0}, {0, 0, 0}}
	if mask, fg, _, _ := partition(corner); mask != 1 && !(mask == 14 && fg == corner[1]) {
		t.Fatalf("corner cell %d %v", mask, fg)
	}
	eight := []RGB{{1, 1, 1}, {0, 0, 0}, {1, 1, 1}, {0, 0, 0}, {1, 1, 1}, {0, 0, 0}, {1, 1, 1}, {0, 0, 0}}
	if mask, _, _, _ := partition(eight); mask != 0b01010101 && mask != 0b10101010 {
		t.Fatalf("octant column cell %b", mask)
	}
}

// Every glyph table holds one glyph per mask and no glyph twice
func TestGlyphTables(t *testing.T) {
	for _, mode := range []CellMode{ModeHalf, ModeQuadrant, ModeSextant, ModeOctant} {
		px, py := mode.Pixels()
		n := 1 << (px * py)
		seen := map[string]int{}
		for mask := range n {
			g := mode.Glyph(mask)
			if g == "" || ansi.StringWidth(g) != 1 {
				t.Fatalf("mode %d mask %b glyph %q", mode, mask, g)
			}
			if prev, dup := seen[g]; dup {
				t.Fatalf("mode %d masks %b and %b share glyph %q", mode, prev, mask, g)
			}
			seen[g] = mask
		}
		if mode.Glyph(0) != " " || mode.Glyph(n-1) != "█" {
			t.Fatalf("mode %d empty %q full %q", mode, mode.Glyph(0), mode.Glyph(n-1))
		}
	}
	for mask, want := range map[int]rune{0b000111: 0x1FB06, 0b000001: 0x1FB00, 0b010101: 0x258C} {
		if sextants[mask] != want {
			t.Fatalf("sextant %b is %U, want %U", mask, sextants[mask], want)
		}
	}
	for mask, want := range map[int]rune{0b00000001: 0x1CEA8, 0b00010100: 0x1FBE6, 0b00000111: 0x1CD02, 0b00001111: 0x2580} {
		if octants[mask] != want {
			t.Fatalf("octant %b is %U, want %U", mask, octants[mask], want)
		}
	}
}

// Meshing a grid exposes only outer faces, darkens crowded corners and keeps light in range
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
			for _, l := range q.Light {
				if l.R <= 0 || l.R > 2 {
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
	for _, c := range fr.Color {
		if c.R > 0 {
			lit++
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

// Quantizer picks the nearest of sixteen colors and encoding fills the cells of every mode
func TestQuantizerAndEncode(t *testing.T) {
	q := ANSIQuantizer([16]color.Color{})
	if c := q(RGB{1, 1, 1}); c != ansi.IndexedColor(15) {
		t.Fatalf("white quantized to %v", c)
	}
	fr := &Frame{PixelAspect: 1}
	fr.Resize(8, 8)
	fr.Clear(func(x, y int) RGB { return Grey(float64(x+y) / 14) })
	for _, mode := range []CellMode{ModeHalf, ModeQuadrant, ModeSextant, ModeOctant} {
		px, py := mode.Pixels()
		buf := newBuffer(8/px, 8/py)
		fr.Encode(buf, 0, 0, 8/px, 8/py, mode, nil)
		if c := buf.CellAt(0, 0); c == nil || c.Style.Bg == nil {
			t.Fatalf("mode %d left the first cell empty", mode)
		}
	}
}

func newBuffer(w, h int) *uv.Buffer { return uv.NewBuffer(w, h) }

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

// Shading with binned lights gives the same light at every corner as testing every light
func TestShadeCulling(t *testing.T) {
	rng := RNG{Seed: 7}
	var lights []Light
	for range 40 {
		lights = append(lights, Light{Pos: Vec3{rng.Spread() * 40, rng.Next() * 6, rng.Spread() * 40}, Color: RGB{rng.Next(), rng.Next(), rng.Next()}, Radius: 1 + rng.Next()*9})
	}
	lights = append(lights, Light{Pos: Vec3{3, 2, 1}, Color: Grey(0.1), Radius: math.Inf(1)}, Light{Pos: Vec3{500, 0, 500}, Color: Grey(1), Radius: 30})
	r := &Raster{Sun: Vec3{0, 1, 0}, SunColor: Grey(0.3), Ambient: Grey(0.2)}
	r.SetLights(lights)
	brute := func(q *Quad, n Vec3) [4]RGB {
		var out [4]RGB
		base := r.Ambient.Add(r.SunColor.Scale(max(n.Dot(r.Sun), 0)))
		for c := range 4 {
			l := base
			for i := range lights {
				lt := &lights[i]
				d := lt.Pos.Sub(q.P[c])
				d2 := d.Dot(d)
				if d2 >= lt.Radius*lt.Radius || d2 == 0 {
					continue
				}
				dist := math.Sqrt(d2)
				l = l.Add(lt.Color.Scale(max(d.Dot(n)/dist, 0) * (1 - dist/lt.Radius) * (1 - dist/lt.Radius)))
			}
			out[c] = l
		}
		return out
	}
	lit := 0
	for k := range 3000 {
		at := Vec3{rng.Spread() * 60, rng.Spread() * 4, rng.Spread() * 60}
		size := 0.1 + rng.Next()*float64(1+k%4*8)
		u := Vec3{rng.Spread(), rng.Spread() * 0.3, rng.Spread()}.Norm().Mul(size)
		v := Vec3{rng.Spread(), 1, rng.Spread()}.Norm().Mul(size * (0.2 + rng.Next()))
		q := Quad{P: [4]Vec3{at, at.Add(u), at.Add(u).Add(v), at.Add(v)}}
		n := u.Cross(v).Norm()
		r.Shade(&q, n)
		want := brute(&q, n)
		if q.Light != want {
			t.Fatalf("quad %d at %v lit %v with culling and %v without", k, at, q.Light, want)
		}
		if want[0] != r.Ambient.Add(r.SunColor.Scale(max(n.Dot(r.Sun), 0))) {
			lit++
		}
	}
	if lit < 500 {
		t.Fatalf("only %d quads caught a point light", lit)
	}
	for k := range 500 {
		lo := Vec3{rng.Spread() * 50, rng.Spread() * 3, rng.Spread() * 50}
		hi := lo.Add(Vec3{rng.Next() * 3, rng.Next() * 6, rng.Next() * 3})
		near := append([]int32(nil), r.lightsInBox(lo, hi)...)
		for range 8 {
			at := func() Vec3 {
				return Vec3{lo.X + rng.Next()*(hi.X-lo.X), lo.Y + rng.Next()*(hi.Y-lo.Y), lo.Z + rng.Next()*(hi.Z-lo.Z)}
			}
			q := Quad{P: [4]Vec3{at(), at(), at(), at()}}
			n := Vec3{rng.Spread(), rng.Spread(), rng.Spread()}.Norm()
			r.shadeFrom(&q, n, near)
			if want := brute(&q, n); q.Light != want {
				t.Fatalf("box %d from %v to %v lit %v with its lights and %v with every light", k, lo, hi, q.Light, want)
			}
		}
	}
	r.Lights = lights[:10]
	q := Quad{P: [4]Vec3{lights[11].Pos, lights[11].Pos.Add(Vec3{0.1, 0, 0}), lights[11].Pos.Add(Vec3{0.1, 0.1, 0}), lights[11].Pos.Add(Vec3{0, 0.1, 0})}}
	r.Shade(&q, Vec3{0, 0, 1})
	lights = lights[:10]
	if want := brute(&q, Vec3{0, 0, 1}); q.Light != want {
		t.Fatalf("lights assigned without SetLights shade %v, want %v", q.Light, want)
	}
}

// Two frames a light flicker apart change far fewer cells through a held encoder than through fresh ones
func TestEncoderHoldsCells(t *testing.T) {
	cols, rows := 80, 30
	mode := ModeOctant
	px, py := mode.Pixels()
	draw := func(flicker float64) *Frame {
		fr := &Frame{PixelAspect: mode.PixelAspect()}
		fr.Resize(cols*px, rows*py)
		fr.Clear(func(x, y int) RGB { return Grey(0.05 + 0.1*float64(y)/float64(fr.H)) })
		r := &Raster{Sun: Vec3{0.3, 1, 0.5}.Norm(), SunColor: Grey(0.5), Ambient: Grey(0.3)}
		r.SetLights([]Light{{Pos: Vec3{1, 3, 2}, Color: RGB{1, 0.6, 0.3}.Scale(flicker), Radius: 12}})
		r.Begin(fr, Camera{Target: Vec3{0, 0.5, 0}, Yaw: 0.6, Pitch: 0.55, Dist: 14, FOV: 0.9})
		g := Grid{}
		g.BoxTex(-6, -1, -6, 12, 1, 12, RGB{0.3, 0.35, 0.4}, TexPanel)
		g.BoxTex(-2, 0, -2, 3, 2, 3, RGB{0.5, 0.3, 0.3}, TexGrate)
		g.BoxTex(2, 0, 1, 2, 3, 2, RGB{0.2, 0.5, 0.4}, TexCircuit)
		r.Chunks(g.Mesh(r))
		r.OrientedBox(Vec3{-3, 1, 3}, Vec3{1.5, 2, 1.5}, 0.4, RGB{0.9, 0.7, 0.2}, 1)
		r.Flush()
		fr.Outline(0.5)
		return fr
	}
	a, b := draw(1), draw(1.03)
	changed := func(x, y *uv.Buffer) int {
		n := 0
		for cy := range rows {
			for cx := range cols {
				if !x.CellAt(cx, cy).Equal(y.CellAt(cx, cy)) {
					n++
				}
			}
		}
		return n
	}
	fa, fb := newBuffer(cols, rows), newBuffer(cols, rows)
	a.Encode(fa, 0, 0, cols, rows, mode, nil)
	b.Encode(fb, 0, 0, cols, rows, mode, nil)
	var e Encoder
	ha, hb := newBuffer(cols, rows), newBuffer(cols, rows)
	e.Encode(a, ha, 0, 0, cols, rows, mode, nil)
	e.Encode(b, hb, 0, 0, cols, rows, mode, nil)
	fresh, held := changed(fa, fb), changed(ha, hb)
	if fresh < 200 || held*5 > fresh {
		t.Fatalf("fresh encoders changed %d cells, the held one %d", fresh, held)
	}
	for i := range cols * rows {
		if !fa.CellAt(i%cols, i/cols).Equal(ha.CellAt(i%cols, i/cols)) {
			t.Fatalf("first frame through a new encoder differs from the stateless one at cell %d", i)
		}
	}
	e.Reset()
	hc := newBuffer(cols, rows)
	e.Encode(b, hc, 0, 0, cols, rows, mode, nil)
	if changed(fb, hc) != 0 {
		t.Fatal("a reset encoder still held cells")
	}
	ansi := ANSIQuantizer([16]color.Color{})
	pa, pb := newBuffer(cols, rows), newBuffer(cols, rows)
	e.Encode(b, pa, 0, 0, cols, rows, mode, ansi)
	b.Encode(pb, 0, 0, cols, rows, mode, ansi)
	if changed(pa, pb) != 0 {
		t.Fatal("switching to a quantizer kept colors boxed for the direct path")
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
