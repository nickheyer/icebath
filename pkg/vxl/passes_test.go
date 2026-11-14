package vxl

import (
	"math"
	"testing"

	uv "github.com/charmbracelet/ultraviolet"
)

// Floor with a tall box on it under a slanted camera, every pass off, the raster ready to flush again
func floorAndBox(w, h int) (*Frame, *Raster, Grid) {
	g := Grid{}
	g.Box(-12, -1, -12, 24, 1, 24, Grey(0.7))
	g.Box(-1, 0, -1, 2, 4, 2, Grey(0.6))
	fr := &Frame{PixelAspect: 1}
	fr.Resize(w, h)
	fr.Clear(func(x, y int) RGB { return RGB{} })
	r := &Raster{Sun: Vec3{0, 1, 0}, Ambient: Grey(0.5)}
	return fr, r, g
}

// Occlusion darkens the floor against the foot of a box more than the open floor, leaves sky and glowing pixels alone and agrees across worker counts
func TestOcclusion(t *testing.T) {
	draw := func(workers int, strength float64) (*Frame, *Raster) {
		fr, r, g := floorAndBox(200, 150)
		r.Workers = workers
		r.AO = Occlusion{Radius: 1.5, Strength: strength, Samples: 12}
		chunks := g.Mesh(r)
		r.Begin(fr, Camera{Target: Vec3{0, 1, 0}, Yaw: 0.7, Pitch: 0.9, Dist: 14, FOV: 0.9})
		r.Chunks(chunks)
		r.EmissiveBox(Vec3{4, 0.5, 4}, Vec3{1, 1, 1}, 0, RGB{1, 1, 0}, 1, 1.5)
		r.Flush()
		return fr, r
	}
	plain, _ := draw(1, 0)
	fr, r := draw(1, 1.5)
	many, _ := draw(8, 1.5)
	for i := range fr.Color {
		if fr.Color[i] != many.Color[i] {
			t.Fatalf("pixel %d differs between one worker and eight with occlusion on", i)
		}
	}
	at := func(p Vec3) (int, RGB, RGB) {
		sx, sy, _, _ := r.Project(p)
		i := int(sy)*fr.W + int(sx)
		return i, fr.Color[i], plain.Color[i]
	}
	_, foot, footPlain := at(Vec3{1.15, 0, 0})
	_, open, openPlain := at(Vec3{6, 0, -6})
	if foot.R >= footPlain.R*0.9 {
		t.Fatalf("floor at the foot of the box shades %v against %v without occlusion", foot, footPlain)
	}
	if math.Abs(open.R-openPlain.R) > 0.02*openPlain.R {
		t.Fatalf("open floor shades %v against %v without occlusion", open, openPlain)
	}
	i, glow, glowPlain := at(Vec3{4, 1, 4})
	if fr.Depth[i] == 0 || glow != glowPlain {
		t.Fatalf("glowing box shades %v against %v without occlusion", glow, glowPlain)
	}
	for i := range fr.Color {
		if fr.Depth[i] == 0 && fr.Color[i] != plain.Color[i] {
			t.Fatalf("sky pixel %d changed under occlusion", i)
		}
		if a := r.ao[i]; a < 0 || a > 1 {
			t.Fatalf("occlusion %v at pixel %d", a, i)
		}
	}
}

// A volume lit from one emitter is brightest beside it, fades with distance, stays dark behind a wall and smooths without losing its shape
func TestVolume(t *testing.T) {
	g := Grid{}
	g.Box(0, -1, 0, 20, 1, 20, Grey(0.5))
	g.Box(10, 0, 0, 1, 6, 20, Grey(0.5))
	g.Put(3, 0, 3, Voxel{Col: RGB{1, 0.5, 0.2}, Emit: 2})
	v := NewVolume(Vec3{0, -1, 0}, Vec3{20, 8, 20}, 1)
	v.Occupy(g)
	if !v.Solid(Vec3{10.5, 2, 5}) || v.Solid(Vec3{5, 2, 5}) || v.Solid(Vec3{-3, 2, 5}) {
		t.Fatal("solid cells do not follow the grid")
	}
	srcs := g.Emitters(1, 12)
	if len(srcs) != 5 {
		t.Fatalf("%d emitters for one glowing voxel with five open faces", len(srcs))
	}
	for _, s := range srcs {
		if s.Color.R <= 0 || s.Radius != 12 || s.Pos.Sub(Vec3{3.5, 0.5, 3.5}).Len() > 1 {
			t.Fatalf("emitter %+v", s)
		}
	}
	v.Splat(srcs)
	near, mid, far := v.Sample(Vec3{4.5, 1.5, 3.5}), v.Sample(Vec3{7.5, 1.5, 3.5}), v.Sample(Vec3{9.5, 1.5, 9.5})
	if !(near.R > mid.R && mid.R > far.R && far.R > 0) {
		t.Fatalf("radiance %v near, %v mid, %v far", near, mid, far)
	}
	if near.R/near.G < 1.5 {
		t.Fatalf("radiance %v lost the color of its emitter", near)
	}
	if behind := v.Sample(Vec3{13.5, 1.5, 3.5}); behind.R > 0 {
		t.Fatalf("radiance %v leaked through the wall", behind)
	}
	if side := v.Sample(Vec3{10.5, 1.5, 3.5}); side != (RGB{}) {
		t.Fatalf("a solid cell samples %v", side)
	}
	if out := v.Sample(Vec3{-5, 1.5, 3.5}); out != (RGB{}) {
		t.Fatalf("a point outside the volume samples %v", out)
	}
	v.Smooth(2)
	sNear, sMid := v.Sample(Vec3{4.5, 1.5, 3.5}), v.Sample(Vec3{7.5, 1.5, 3.5})
	if !(sNear.R > sMid.R) || math.Abs(sNear.R-near.R) > near.R*0.5 || math.Abs(sMid.R-mid.R) > mid.R*0.5 {
		t.Fatalf("smoothing gave %v near and %v mid from %v and %v", sNear, sMid, near, mid)
	}
	if behind := v.Sample(Vec3{13.5, 1.5, 3.5}); behind.R > 0 {
		t.Fatalf("smoothing leaked %v through the wall", behind)
	}
	sun := Vec3{0.3, 1, 0.2}.Norm()
	bounce := g.SunBounce(sun, Grey(1), nil, 4, 6, 0.5)
	if len(bounce) == 0 {
		t.Fatal("no sun bounce emitters")
	}
	tops := 0
	for _, b := range bounce {
		if b.Pos.Y > 0 && b.Pos.Y < 0.5 {
			tops++
		}
		if b.Color.R <= 0 || b.Color.R > 20 {
			t.Fatalf("bounce emitter %+v", b)
		}
	}
	if tops == 0 {
		t.Fatal("no bounce from the floor")
	}
	before := v.Sample(Vec3{7.5, 1.5, 3.5})
	v.Seal()
	if sealed := v.Sample(Vec3{7.5, 1.5, 3.5}); sealed.Dist(before) > 1e-9 {
		t.Fatalf("sealing changed an open sample from %v to %v", before, sealed)
	}
	if wall := v.Sample(Vec3{10.5, 1.5, 3.5}); wall.R <= 0 {
		t.Fatalf("a sealed wall cell samples %v", wall)
	}
	if beside := v.Sample(Vec3{9.9, 1.5, 3.5}); beside.R <= 0 || beside.R > v.Sample(Vec3{9.5, 1.5, 3.5}).R*1.5 {
		t.Fatalf("beside the sealed wall samples %v", beside)
	}
	v.Clear()
	v.Add(Vec3{5.5, 1.5, 5.5}, RGB{1, 2, 3})
	if c := v.Sample(Vec3{5.5, 1.5, 5.5}); c != (RGB{1, 2, 3}) {
		t.Fatalf("added light samples as %v", c)
	}
	fr := &Frame{PixelAspect: 1}
	fr.Resize(120, 90)
	fr.Clear(func(x, y int) RGB { return RGB{} })
	r := &Raster{Sun: Vec3{0, 1, 0}, Ambient: Grey(0.1), GI: v}
	v.Clear()
	v.Splat(srcs)
	chunks := g.Mesh(r)
	r.Begin(fr, Camera{Target: Vec3{6, 0, 6}, Yaw: 0.6, Pitch: 0.9, Dist: 16, FOV: 0.9})
	r.Chunks(chunks)
	r.Flush()
	pixel := func(p Vec3) RGB {
		sx, sy, _, _ := r.Project(p)
		return fr.Color[int(sy)*fr.W+int(sx)]
	}
	if close, away := pixel(Vec3{4.5, 0, 3.5}), pixel(Vec3{8.5, 0, 8.5}); close.R <= away.R*1.3 || away.R < 0.05 {
		t.Fatalf("floor shades %v beside the emitter and %v away from it", close, away)
	}
}

// A glossy floor mirrors the glowing box above it, rays leaving the frame take the sky color and a matte floor stays as it was
func TestReflection(t *testing.T) {
	draw := func(gloss float64, workers int, sky RGB) (*Frame, *Raster) {
		fr := &Frame{PixelAspect: 1}
		fr.Resize(200, 150)
		fr.Clear(func(x, y int) RGB { return RGB{} })
		r := &Raster{Sun: Vec3{0, 1, 0}, Ambient: Grey(0.3), Workers: workers}
		r.Reflect = Reflection{Strength: 1, Steps: 32, Distance: 20, Sky: sky}
		r.Begin(fr, Camera{Target: Vec3{0, 1, 0}, Yaw: 0.3, Pitch: 0.35, Dist: 12, FOV: 0.9})
		floor := Quad{P: FloorQuad(Vec3{}, 12, 12, 0), Color: Grey(0.3), Alpha: 1, Gloss: gloss}
		r.Shade(&floor, Vec3{0, 1, 0})
		r.Quad(floor)
		r.EmissiveBox(Vec3{0, 1.5, 0}, Vec3{2, 3, 2}, 0, RGB{1, 0.2, 0.1}, 1, 1.5)
		r.Flush()
		return fr, r
	}
	matte, _ := draw(0, 1, RGB{})
	glossy, r := draw(1, 1, RGB{})
	many, _ := draw(1, 8, RGB{})
	for i := range glossy.Color {
		if glossy.Color[i] != many.Color[i] {
			t.Fatalf("pixel %d differs between one worker and eight with reflections on", i)
		}
	}
	reflected, unchanged := 0, 0
	for i := range glossy.Color {
		if glossy.Depth[i] == 0 {
			continue
		}
		if unpackGloss(glossy.Normal[i]) == 0 {
			continue
		}
		if glossy.Color[i].R > matte.Color[i].R+0.1 && glossy.Color[i].R > glossy.Color[i].G*2 {
			reflected++
		}
	}
	for i := range matte.Color {
		if matte.Depth[i] != 0 && unpackGloss(matte.Normal[i]) == 0 && matte.Color[i] == glossy.Color[i] {
			unchanged++
		}
	}
	if reflected < 200 {
		t.Fatalf("only %d floor pixels mirror the red box", reflected)
	}
	sx, sy, _, _ := r.Project(Vec3{0.5, 0, 1.4})
	if i := int(sy)*glossy.W + int(sx); glossy.Color[i].R <= matte.Color[i].R+0.2 {
		t.Fatalf("floor beside the box shades %v glossy and %v matte", glossy.Color[i], matte.Color[i])
	}
	for i := range matte.Color {
		if matte.Normal[i] != 0 && unpackGloss(matte.Normal[i]) != 0 {
			t.Fatal("a matte floor packed a gloss")
		}
	}
	skyed, _ := draw(1, 1, RGB{0, 0, 1})
	blue := 0
	for i := range skyed.Color {
		if skyed.Depth[i] != 0 && skyed.Color[i].B > glossy.Color[i].B+0.05 {
			blue++
		}
	}
	if blue < 500 {
		t.Fatalf("only %d floor pixels mirror the sky", blue)
	}
}

// Hazy air glows around a point light and along the sun, brighter near the light than far from it, the sky taking it too
func TestHaze(t *testing.T) {
	draw := func(density float64, workers int) (*Frame, *Raster) {
		fr := &Frame{PixelAspect: 1}
		fr.Resize(160, 120)
		fr.Clear(func(x, y int) RGB { return RGB{} })
		r := &Raster{Sun: Vec3{0, 1, 0}, Ambient: Grey(0.2), Workers: workers}
		r.Haze = Haze{Density: density, Steps: 16, Shrink: 4, Reach: 40, Lights: 1}
		r.SetLights([]Light{{Pos: Vec3{3, 2, 0}, Color: RGB{0, 1, 0}, Radius: 6}})
		r.Begin(fr, Camera{Target: Vec3{0, 1, 0}, Yaw: 0.4, Pitch: 0.4, Dist: 14, FOV: 0.9})
		floor := Quad{P: FloorQuad(Vec3{}, 12, 12, 0), Color: Grey(0.3), Alpha: 1}
		r.Shade(&floor, Vec3{0, 1, 0})
		r.Quad(floor)
		r.Flush()
		return fr, r
	}
	clear, _ := draw(0, 1)
	hazy, r := draw(0.1, 1)
	many, _ := draw(0.1, 8)
	for i := range hazy.Color {
		if hazy.Color[i] != many.Color[i] {
			t.Fatalf("pixel %d differs between one worker and eight with haze on", i)
		}
	}
	pixel := func(fr *Frame, p Vec3) RGB {
		sx, sy, _, _ := r.Project(p)
		return fr.Color[int(sy)*fr.W+int(sx)]
	}
	near, far := pixel(hazy, Vec3{3, 1, 0}), pixel(hazy, Vec3{-9, 0, -9})
	if near.G <= pixel(clear, Vec3{3, 1, 0}).G+0.02 || near.G <= far.G*1.5 {
		t.Fatalf("air shades %v by the light and %v far from it", near, far)
	}
	sky := 0
	for i := range hazy.Color {
		if hazy.Depth[i] == 0 && hazy.Color[i].G > 0 {
			sky++
		}
	}
	if sky == 0 {
		t.Fatal("the sky took no haze")
	}
	sun := Vec3{0.5, 0.4, 0.3}.Norm()
	fr := &Frame{PixelAspect: 1}
	fr.Resize(160, 120)
	fr.Clear(func(x, y int) RGB { return RGB{} })
	rs := &Raster{Sun: sun, SunColor: RGB{1, 0.8, 0.5}, Ambient: Grey(0.1)}
	rs.Haze = Haze{Density: 0.05, Steps: 16, Shrink: 2, Reach: 60, Sun: 1, Forward: 0.7}
	rs.Begin(fr, Camera{Target: Vec3{}, Yaw: math.Atan2(sun.X, sun.Z) + math.Pi, Pitch: 0.1, Dist: 10, FOV: 1.2})
	rs.Flush()
	toward, away := fr.Color[60*fr.W+80], fr.Color[60*fr.W+2]
	if toward.R <= away.R*1.2 || toward.R <= 0 {
		t.Fatalf("sky toward the sun shades %v and away from it %v", toward, away)
	}
}

// Fog toward the sun warms along the sun direction and stays the fog color away from it
func TestSunFog(t *testing.T) {
	sun := Vec3{0.6, 0.3, 0.4}.Norm()
	fr := &Frame{PixelAspect: 1}
	fr.Resize(200, 100)
	fr.Clear(func(x, y int) RGB { return RGB{} })
	r := &Raster{Sun: sun, SunColor: Grey(0.5), Ambient: Grey(0.3), FogColor: RGB{0.1, 0.1, 0.2}, FogSun: RGB{1, 0.5, 0}, FogNear: 2, FogFar: 30}
	r.Begin(fr, Camera{Target: Vec3{}, Yaw: math.Atan2(sun.X, sun.Z) + math.Pi, Pitch: 0.2, Dist: 6, FOV: 1.3})
	floor := Quad{P: FloorQuad(Vec3{}, 60, 60, 0), Color: Grey(0.5), Alpha: 1}
	r.Shade(&floor, Vec3{0, 1, 0})
	r.Quad(floor)
	r.Billboard(Vec3{0, 1, -25}, 3, Grey(0.5), 0.5, false)
	r.Flush()
	var left, mid, right RGB
	for y := range fr.H {
		for x := range fr.W {
			i := y*fr.W + x
			if fr.Depth[i] == 0 || 1/float64(fr.Depth[i]) < 20 {
				continue
			}
			switch {
			case x < 10:
				left = left.Add(fr.Color[i])
			case x > fr.W-10:
				right = right.Add(fr.Color[i])
			case x > fr.W/2-5 && x < fr.W/2+5:
				mid = mid.Add(fr.Color[i])
			}
		}
	}
	warm := func(c RGB) float64 { return (c.R - c.B) / max(c.R+c.B, 1e-9) }
	if warm(mid) < 0.1 || warm(left) > 0.02 || warm(right) > 0.02 || mid.R <= left.R || mid.R <= right.R {
		t.Fatalf("far fog toward the sun sums %v, left %v, right %v", mid, left, right)
	}
}

// Specular highlights show on a glossy floor under a point light and leave a matte one unchanged
func TestSpecular(t *testing.T) {
	draw := func(gloss float64) (*Frame, *Raster) {
		fr := &Frame{PixelAspect: 1}
		fr.Resize(160, 120)
		fr.Clear(func(x, y int) RGB { return RGB{} })
		r := &Raster{Sun: Vec3{0, 1, 0}, Ambient: Grey(0.2)}
		r.SetLights([]Light{{Pos: Vec3{0, 3, 0}, Color: Grey(1), Radius: 12}})
		r.Begin(fr, Camera{Target: Vec3{0, 0, 0}, Yaw: 0, Pitch: 0.5, Dist: 10, FOV: 0.9})
		floor := Quad{P: FloorQuad(Vec3{}, 10, 10, 0), Color: Grey(0.3), Alpha: 1, Gloss: gloss}
		r.Shade(&floor, Vec3{0, 1, 0})
		r.Quad(floor)
		r.Flush()
		return fr, r
	}
	matte, _ := draw(0)
	glossy, _ := draw(0.8)
	brighter, darker := 0, 0
	for i := range matte.Color {
		if matte.Depth[i] == 0 {
			continue
		}
		switch {
		case glossy.Color[i].R > matte.Color[i].R+0.05:
			brighter++
		case glossy.Color[i].R < matte.Color[i].R:
			darker++
		}
	}
	if brighter < 50 || darker != 0 {
		t.Fatalf("%d pixels brighter and %d darker on the glossy floor", brighter, darker)
	}
}

// Grading scales by exposure, rolls bright light off below one, keeps grey under saturation and fringes the corners apart
func TestGrade(t *testing.T) {
	fr := &Frame{PixelAspect: 1}
	fr.Resize(64, 64)
	fr.Clear(func(x, y int) RGB { return RGB{0.5, 0.5, 0.5} })
	fr.Grade(Grade{Exposure: 2})
	if fr.Color[0] != (RGB{1, 1, 1}) {
		t.Fatalf("exposure gave %v", fr.Color[0])
	}
	fr.Clear(func(x, y int) RGB { return RGB{4, 0.2, 0.2} })
	fr.Grade(Grade{Tonemap: true})
	if c := fr.Color[0]; c.R > 1 || c.R < 0.9 || c.G >= 0.4 || c.G <= 0.2 {
		t.Fatalf("tonemap gave %v", c)
	}
	if aces(0) != 0 || aces(0.5) < 0.5 || aces(0.5) > 0.7 || aces(100) != 1 || aces(0.3) <= aces(0.2) {
		t.Fatalf("curve %v %v %v", aces(0), aces(0.5), aces(100))
	}
	fr.Clear(func(x, y int) RGB { return RGB{0.6, 0.3, 0.3} })
	fr.Grade(Grade{Saturation: 2})
	if c := fr.Color[0]; c.R <= 0.6 || c.G >= 0.3 {
		t.Fatalf("saturation gave %v", c)
	}
	fr.Clear(func(x, y int) RGB { return RGB{0.4, 0.4, 0.4} })
	fr.Grade(Grade{Saturation: 0.5})
	if c := fr.Color[0]; c.Dist(RGB{0.4, 0.4, 0.4}) > 1e-9 {
		t.Fatalf("saturation moved grey to %v", c)
	}
	fr.Clear(func(x, y int) RGB {
		if x == 32 && y == 32 || x == 8 && y == 8 {
			return Grey(1)
		}
		return RGB{}
	})
	fr.Grade(Grade{Aberration: 3})
	if center := fr.Color[32*fr.W+32]; center.Dist(RGB{1, 1, 1}) > 1e-6 {
		t.Fatalf("center pixel %v after fringing", center)
	}
	if c := fr.Color[8*fr.W+8]; c.G != 1 || c.R != 0 || c.B != 0 {
		t.Fatalf("corner pixel %v after fringing", c)
	}
	red, blue := 0, 0
	for y := 4; y < 13; y++ {
		for x := 4; x < 13; x++ {
			switch c := fr.Color[y*fr.W+x]; {
			case c.R > 0 && c.B == 0:
				red++
			case c.B > 0 && c.R == 0:
				blue++
			}
		}
	}
	if red == 0 || blue == 0 {
		t.Fatalf("%d red and %d blue fringe pixels around the corner dot", red, blue)
	}
}

// A frame drawn at twice the cell grid encodes to the same cells as its box filtered half, pixel by pixel means
func TestSupersample(t *testing.T) {
	cols, rows := 20, 6
	big := sceneFrame(cols*cellCols*2, rows*cellRows*2, 1, 0)
	small := &Frame{PixelAspect: 1}
	small.Resize(cols*cellCols, rows*cellRows)
	for y := range small.H {
		for x := range small.W {
			small.Color[y*small.W+x] = big.At(2*x, 2*y).Add(big.At(2*x+1, 2*y)).Add(big.At(2*x, 2*y+1)).Add(big.At(2*x+1, 2*y+1)).Scale(0.25)
		}
	}
	var px, want [cellPixels]RGB
	big.gather(&px, 3, 2, 2)
	for j := range cellRows {
		for i := range cellCols {
			want[j*cellCols+i] = small.At(3*cellCols+i, 2*cellRows+j)
		}
	}
	for i := range px {
		if px[i].Dist(want[i]) > 1e-12 {
			t.Fatalf("gathered pixel %d is %v, want %v", i, px[i], want[i])
		}
	}
	a, b := uv.NewBuffer(cols, rows), uv.NewBuffer(cols, rows)
	big.Encode(a, 0, 0, cols, rows, ModeAll, nil)
	small.Encode(b, 0, 0, cols, rows, ModeAll, nil)
	if n := changedCells(a, b); n != 0 {
		t.Fatalf("supersampled frame encodes %d cells differently from its mean", n)
	}
}

// Gloss packs into the facing without touching its direction or lit mark and survives a round trip
func TestGlossPacking(t *testing.T) {
	for _, g := range []float64{0, 0.25, 0.5, 1} {
		n := packNormal(Vec3{0, 1, 0}) | normalLit | packGloss(g)
		if math.Abs(unpackGloss(n)-g) > 1/63.0 || unpackNormal(n) != (Vec3{0, 1, 0}) || n&normalLit == 0 {
			t.Fatalf("gloss %v packed to %x, unpacked %v", g, n, unpackGloss(n))
		}
		if crease(n, packNormal(Vec3{0, 1, 0})) {
			t.Fatal("gloss made a crease")
		}
	}
	if packGloss(2) != packGloss(1) || packGloss(-1) != 0 {
		t.Fatal("gloss is not clamped")
	}
	g := Grid{}
	g.Put(0, 0, 0, Voxel{Col: Grey(1), Gloss: 0.5})
	g.Put(2, 0, 0, Voxel{Col: RGB{1, 0, 0}, Emit: 2})
	r := &Raster{}
	glossy, glowing := 0, 0
	for _, ch := range g.Mesh(r) {
		for _, q := range ch.Quads {
			switch {
			case q.Gloss == 0.5 && q.Lit:
				glossy++
			case !q.Lit && q.Light[0] == Grey(2) && q.Color == (RGB{1, 0, 0}):
				glowing++
			default:
				t.Fatalf("face %+v", q)
			}
		}
	}
	if glossy != 6 || glowing != 6 {
		t.Fatalf("%d glossy and %d glowing faces", glossy, glowing)
	}
}

// Frames with every pass on, the deferred benchmark scene with haze, occlusion, reflections and a baked volume
func BenchmarkPasses(b *testing.B) {
	fr := &Frame{PixelAspect: 1}
	fr.Resize(880, 448)
	sun := Vec3{0.35, 1, 0.55}.Norm()
	r := &Raster{Sun: sun, SunColor: Grey(0.72), Ambient: RGB{0.23, 0.3, 0.4}, FogColor: Grey(0.08), FogNear: 34, FogFar: 100}
	r.AO = Occlusion{Radius: 1.2, Strength: 1.2, Samples: 10, Shrink: 2}
	r.Reflect = Reflection{Strength: 0.8, Steps: 20, Distance: 24, Shrink: 2}
	r.Haze = Haze{Density: 0.02, Steps: 16, Shrink: 4, Reach: 80, Lights: 0.6, Sun: 0.3, Ambient: 0.5, Forward: 0.5}
	g := Grid{}
	g.Fill(0, -1, 0, 40, 1, 40, Voxel{Col: RGB{0.3, 0.35, 0.4}, Tex: TexPanel, Gloss: 0.6})
	for x := 2; x < 40; x += 4 {
		g.Put(x, -1, 20, Voxel{Col: RGB{0.2, 0.9, 1}, Emit: 2})
	}
	chunks := g.Mesh(r)
	static := NewShadowMap(sun, Vec3{0, -1, 0}, Vec3{40, 6, 40}, 0.25)
	static.Chunks(chunks)
	v := NewVolume(Vec3{0, -1, 0}, Vec3{40, 12, 40}, 1)
	v.Occupy(g)
	v.Splat(g.Emitters(2, 10))
	v.Splat(g.SunBounce(sun, Grey(0.72), static, 4, 8, 0.3))
	v.Smooth(2)
	v.Seal()
	r.GI = v
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
	var post Post
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
		r.Flush()
		fr.Outline(0.5)
		fr.Bloom(&post, 0.6, 0.8)
		fr.Grade(Grade{Exposure: 1.1, Tonemap: true, Aberration: 1})
		fr.Vignette(0.2)
	}
}
