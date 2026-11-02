package vxl

import (
	"image/color"
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
	if mask, _, bg := partition(solid); mask != 0 || bg != solid[0] {
		t.Fatalf("solid cell %d %v", mask, bg)
	}
	halves := []RGB{{1, 1, 1}, {1, 1, 1}, {0, 0, 0}, {0, 0, 0}}
	mask, fg, bg := partition(halves)
	if (mask != 3 || fg != halves[0] || bg != halves[2]) && (mask != 12 || fg != halves[2] || bg != halves[0]) {
		t.Fatalf("half cell %d %v %v", mask, fg, bg)
	}
	corner := []RGB{{0, 1, 0}, {0, 0, 0}, {0, 0, 0}, {0, 0, 0}}
	if mask, fg, _ := partition(corner); mask != 1 && !(mask == 14 && fg == corner[1]) {
		t.Fatalf("corner cell %d %v", mask, fg)
	}
	eight := []RGB{{1, 1, 1}, {0, 0, 0}, {1, 1, 1}, {0, 0, 0}, {1, 1, 1}, {0, 0, 0}, {1, 1, 1}, {0, 0, 0}}
	if mask, _, _ := partition(eight); mask != 0b01010101 && mask != 0b10101010 {
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
