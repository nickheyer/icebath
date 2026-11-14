package vxl

import (
	"math"
	"slices"
	"testing"

	uv "github.com/charmbracelet/ultraviolet"
)

// Scene of a glossy floor, a wall and a glowing block, with a box walking across it frame after frame
type walkScene struct {
	chunks []Chunk
	cam    Camera
	sun    Vec3
	steady []Light
	sky    func(x, y int) RGB
	// Depth map of the platform alone
	shadow *ShadowMap
	// Passes switched off the raster of every frame
	bare func(r *Raster)
}

func newWalkScene() *walkScene {
	g := Grid{}
	for x := -8; x < 8; x++ {
		for z := -8; z < 8; z++ {
			g.Put(x, -1, z, Voxel{Col: Grey(0.4), Tex: TexPanel, Gloss: 0.6})
		}
	}
	for x := -8; x < 8; x++ {
		for y := 0; y < 4; y++ {
			g.Put(x, y, -8, Voxel{Col: RGB{0.3, 0.3, 0.35}})
		}
	}
	g.Box(2, 0, 2, 2, 3, 2, RGB{0.7, 0.3, 0.3})
	g.Put(-4, 0, -4, Voxel{Col: RGB{1, 0.5, 0.2}, Emit: 1.2})
	w := &walkScene{cam: Camera{Target: Vec3{0, 1, 0}, Yaw: 0.6, Pitch: 0.55, Dist: 16, FOV: 0.9}, sun: Vec3{-0.45, 0.75, -0.35}.Norm()}
	w.steady = []Light{{Pos: Vec3{-3, 2, -3}, Color: RGB{0.9, 0.5, 0.2}, Radius: 7}, {Pos: Vec3{4, 3, 4}, Color: RGB{0.2, 0.4, 0.9}, Radius: 3}}
	w.sky = func(x, y int) RGB { return RGB{0.02, 0.02, 0.05} }
	w.bare = func(*Raster) {}
	w.chunks = g.Mesh(w.raster())
	w.shadow = NewShadowMap(w.sun, Vec3{-8, -1, -8}, Vec3{8, 4, 8}, 0.25)
	w.shadow.Chunks(w.chunks)
	return w
}

func (w *walkScene) raster() *Raster {
	r := &Raster{Sun: w.sun, SunColor: RGB{0.6, 0.55, 0.5}, Ambient: Grey(0.15), FogColor: RGB{0.05, 0.05, 0.08}, FogNear: 12, FogFar: 40, Workers: 4}
	r.AO = Occlusion{Radius: 1.2, Strength: 1.2, Samples: 8, Shrink: 2}
	r.Reflect = Reflection{Strength: 0.8, Steps: 16, Distance: 20, Shrink: 2}
	r.Haze = Haze{Density: 0.03, Steps: 12, Shrink: 4, Reach: 40, Lights: 0.5, Ambient: 0.3, Color: RGB{0.01, 0.01, 0.02}}
	r.Shadows = w.shadow
	w.bare(r)
	return r
}

// Depth map of frame k, the platform with the walking box over it
func (w *walkScene) cast(k int) *ShadowMap {
	sm := NewShadowMap(w.sun, Vec3{-8, -1, -8}, Vec3{8, 4, 8}, 0.25)
	sm.CopyFrom(w.shadow)
	x := -5 + float64(k)*0.8
	sm.Box(Vec3{x, 0.75, 1}, Vec3{1, 1.5, 1}, 0.4*float64(k))
	return sm
}

// Pixels of a set outside the boxes of the lights the layer lacks
func (w *walkScene) beyondLights(r *Raster, l *Layer, d *Dirt) int {
	var box Dirt
	box.Reset(d.W, d.H)
	for k := range r.spans {
		if s := &r.spans[k]; !l.has(s) {
			box.AddRect(s.x0, s.y0, s.x1, s.y1)
		}
	}
	n := 0
	for y := range d.H {
		d.Each(y, func(xa, xb int) {
			for x := xa; x <= xb; x++ {
				if !box.Has(x, y) {
					n++
				}
			}
		})
	}
	return n
}

// Ways the passes of the scene are cut down, by name
var walkLooks = map[string]func(r *Raster){
	"full":    func(*Raster) {},
	"plain":   func(r *Raster) { r.AO.Strength, r.Reflect.Strength, r.Haze.Density = 0, 0, 0 },
	"crowded": func(r *Raster) { r.Reflect.Strength, r.Haze.Density = 0, 0 },
	"unshaded": func(r *Raster) {
		r.Shadows, r.AO.Strength, r.Reflect.Strength, r.Haze.Density = nil, 0, 0, 0
	},
}

// Lights of frame k, the steady ones with the second gone on odd frames, and one sliding along the floor with the box
func (w *walkScene) lights(k int) []Light {
	x := -5 + float64(k)*0.8
	out := append([]Light{}, w.steady[:1+(k+1)%2]...)
	return append(out, Light{Pos: Vec3{x, 1.5, 1}, Color: RGB{0.2, 0.6, 1}, Radius: 5})
}

func (w *walkScene) dynamic(r *Raster, k int) {
	x := -5 + float64(k)*0.8
	r.OrientedBox(Vec3{x, 0.75, 1}, Vec3{1, 1.5, 1}, 0.4*float64(k), RGB{0.9, 0.8, 0.3}, 1)
	r.EmissiveBox(Vec3{1, 1, -3}, Vec3{0.6, 0.6, 0.6}, 0.2, RGB{0.3, 0.9, 1}, 1, 1.3)
	r.OrientedBox(Vec3{0, 1, 3}, Vec3{2, 2, 2}, 0, RGB{0.5, 0.8, 1}, 0.4)
	r.Billboard(Vec3{x, 2.5, 1}, 1.5, RGB{0.2, 0.6, 1}, 0.3, true)
}

// Frame k drawn whole in one flush
func (w *walkScene) whole(k int) *Frame {
	fr := &Frame{PixelAspect: 1}
	fr.Resize(160, 100)
	fr.Clear(w.sky)
	r := w.raster()
	if r.Shadows != nil {
		r.Shadows = w.cast(k)
	}
	r.SetLights(w.lights(k))
	r.Begin(fr, w.cam)
	r.Chunks(w.chunks)
	w.dynamic(r, k)
	r.Flush()
	return fr
}

// Frames over a layer match frames drawn whole, frame after frame, with every pass and with passes cut
func TestLayeredFramesMatchWhole(t *testing.T) {
	for name, look := range walkLooks {
		w := newWalkScene()
		w.bare = look
		r := w.raster()
		fr := &Frame{PixelAspect: 1}
		fr.Resize(160, 100)
		fr.Clear(w.sky)
		r.SetLights(w.steady)
		r.Begin(fr, w.cam)
		r.Chunks(w.chunks)
		var layer Layer
		r.FlushInto(&layer)
		var changed Changes
		changed.Reset(160, 100)
		for k := range 6 {
			layer.Restore(fr, &changed)
			if r.Shadows != nil {
				r.Shadows = w.cast(k)
			}
			r.SetLights(w.lights(k))
			r.Begin(fr, w.cam)
			w.dynamic(r, k)
			r.FlushOver(&layer)
			changed.CopyFrom(r.Changes())
			if changed.All() || changed.Color.Count() >= 160*100 || changed.Drawn.Count() >= changed.Color.Count() {
				t.Fatalf("%s frame %d over the layer changed %d pixels and drew on %d", name, k, changed.Color.Count(), changed.Drawn.Count())
			}
			if beyond := w.beyondLights(r, &layer, &changed.Lit); beyond > 160*100/8 {
				t.Fatalf("%s frame %d relit %d pixels past the reach of the moving lights for one walking box", name, k, beyond)
			}
			one := w.whole(k)
			worst, where := 0.0, 0
			for i := range one.Color {
				if d := math.Sqrt(one.Color[i].Dist(fr.Color[i])); d > worst {
					worst, where = d, i
				}
				if one.Depth[i] != fr.Depth[i] || one.Normal[i] != fr.Normal[i] {
					t.Fatalf("%s frame %d pixel %d has depth %v facing %v whole and %v %v over the layer", name, k, i, one.Depth[i], one.Normal[i], fr.Depth[i], fr.Normal[i])
				}
			}
			if worst > 0.03 {
				t.Fatalf("%s frame %d pixel %d differs by %v, %v whole and %v over the layer", name, k, where, worst, one.Color[where], fr.Color[where])
			}
		}
	}
}

// The shadow of the walking box falls on the layer and lifts again as it moves on
func TestLayeredShadowFollowsCaster(t *testing.T) {
	w := newWalkScene()
	w.bare = walkLooks["crowded"]
	r := w.raster()
	fr := &Frame{PixelAspect: 1}
	fr.Resize(160, 100)
	fr.Clear(w.sky)
	r.SetLights(w.steady)
	r.Begin(fr, w.cam)
	r.Chunks(w.chunks)
	var layer Layer
	r.FlushInto(&layer)
	base := &Frame{}
	base.CopyFrom(fr)
	var changed Changes
	changed.Reset(160, 100)
	shaded := 0
	for k := range 4 {
		layer.Restore(fr, &changed)
		r.Shadows = w.cast(k)
		r.SetLights(w.lights(k))
		r.Begin(fr, w.cam)
		w.dynamic(r, k)
		r.FlushOver(&layer)
		changed.CopyFrom(r.Changes())
		darker := 0
		for i := range fr.Color {
			if fr.Depth[i] == base.Depth[i] && fr.Color[i].Luma() < base.Color[i].Luma()-0.02 {
				darker++
				if !changed.Lit.Has(i%160, i/160) {
					t.Fatalf("frame %d pixel %d darkened under the shadow without being relit", k, i)
				}
			}
		}
		if darker == 0 {
			t.Fatalf("frame %d cast no shadow on the layer", k)
		}
		shaded += darker
		one := w.whole(k)
		for i := range one.Color {
			if d := math.Sqrt(one.Color[i].Dist(fr.Color[i])); d > 0.03 {
				t.Fatalf("frame %d pixel %d differs by %v, %v whole and %v over the layer", k, i, d, one.Color[i], fr.Color[i])
			}
		}
	}
	if shaded == 0 {
		t.Fatal("no pixel of the layer ever fell under the shadow of the box")
	}
}

// The changed pixels of a flush over a layer cover every pixel whose color moved, within a cell of the grids
func TestDirtCoversChanges(t *testing.T) {
	w := newWalkScene()
	r := w.raster()
	fr := &Frame{PixelAspect: 1}
	fr.Resize(160, 100)
	fr.Clear(w.sky)
	r.SetLights(w.steady)
	r.Begin(fr, w.cam)
	r.Chunks(w.chunks)
	var layer Layer
	r.FlushInto(&layer)
	base := &Frame{}
	base.CopyFrom(fr)
	layer.Restore(fr, nil)
	r.SetLights(w.lights(2))
	r.Begin(fr, w.cam)
	w.dynamic(r, 2)
	r.FlushOver(&layer)
	c := r.Changes()
	if c.All() {
		t.Fatal("a flush over a layer marked every pixel")
	}
	missed, drawnMissed := 0, 0
	for i := range fr.Color {
		x, y := i%160, i/160
		if fr.Depth[i] != base.Depth[i] || fr.Normal[i] != base.Normal[i] || fr.Albedo[i] != base.Albedo[i] {
			if !c.Drawn.Has(x, y) {
				drawnMissed++
			}
		}
		if fr.Color[i] == base.Color[i] && fr.Depth[i] == base.Depth[i] {
			continue
		}
		if !c.Color.Has(x, y) {
			missed++
		}
	}
	if missed > 0 || drawnMissed > 0 {
		t.Fatalf("%d changed and %d drawn pixels were not marked, %d and %d marked of %d", missed, drawnMissed, c.Color.Count(), c.Drawn.Count(), 160*100)
	}
	if n := c.Color.Count(); n >= 160*100 {
		t.Fatalf("%d of %d pixels marked for one small box", n, 160*100)
	}
	if c.Drawn.Count() > c.Lit.Count() || c.Lit.Count() > c.Color.Count() {
		t.Fatalf("%d drawn, %d lit anew and %d changed", c.Drawn.Count(), c.Lit.Count(), c.Color.Count())
	}
}

// A finish over only the changed pixels matches the passes run whole over the frame
func TestFinishIncremental(t *testing.T) {
	w := newWalkScene()
	for _, fin := range []Finish{
		{Outline: 0.6, BloomThreshold: 0.6, BloomStrength: 0.8, Grade: Grade{Exposure: 1.1, Tonemap: true, Saturation: 1.2}, Vignette: 0.4},
		{Outline: 0.6, BloomThreshold: 0.6, BloomStrength: 0.8, Grade: Grade{Exposure: 1.1, Tonemap: true, Saturation: 1.2, Aberration: 1.5}, Vignette: 0.4},
		{Grade: Grade{Tonemap: true}},
	} {
		var out Output
		var post Post
		for k := range 4 {
			fr := w.whole(k)
			var changed *Changes
			if k > 0 {
				prev := w.whole(k - 1)
				changed = &Changes{}
				changed.Reset(fr.W, fr.H)
				for i := range fr.Color {
					if fr.Depth[i] != prev.Depth[i] || fr.Normal[i] != prev.Normal[i] {
						changed.Drawn.Add(i/fr.W, i%fr.W, i%fr.W)
					}
					if fr.Color[i] != prev.Color[i] || fr.Depth[i] != prev.Depth[i] || fr.Normal[i] != prev.Normal[i] {
						changed.Color.Add(i/fr.W, i%fr.W, i%fr.W)
					}
				}
				changed.Norm()
			}
			out.Finish(fr, fin, changed)
			ref := &Frame{}
			ref.CopyFrom(fr)
			ref.Outline(fin.Outline)
			ref.Bloom(&post, fin.BloomThreshold, fin.BloomStrength)
			ref.Grade(fin.Grade)
			ref.Vignette(fin.Vignette)
			bytes := ref.Bytes(nil)
			for i := range ref.Color {
				if d := math.Sqrt(ref.Color[i].Dist(out.Color[i])); d > 0.004 {
					t.Fatalf("finish %v frame %d pixel %d is %v whole and %v incremental", fin, k, i, ref.Color[i], out.Color[i])
				}
				for c := range 3 {
					if a, b := int(bytes[i*3+c]), int(out.Bytes[i*3+c]); a-b > 1 || b-a > 1 {
						t.Fatalf("finish %v frame %d pixel %d byte %d is %d whole and %d incremental", fin, k, i, c, a, b)
					}
				}
			}
			if k > 0 && out.Dirt().Count() >= fr.W*fr.H {
				t.Fatalf("finish %v frame %d reworked every pixel", fin, k)
			}
		}
	}
}

// Cells over no changed pixel come out as they were, the rest as a fresh encode
func TestEncodeKeepsUnchangedCells(t *testing.T) {
	w := newWalkScene()
	a, b := w.whole(0), w.whole(3)
	var changed Dirt
	changed.Reset(a.W, a.H)
	for i := range a.Color {
		if a.Color[i] != b.Color[i] {
			changed.Add(i/a.W, i%a.W, i%a.W)
		}
	}
	changed.Norm()
	cols, rows := a.W/cellCols, a.H/cellRows
	var e Encoder
	e.Reset()
	ha, hb, hc := uv.NewBuffer(cols, rows), uv.NewBuffer(cols, rows), uv.NewBuffer(cols, rows)
	e.Encode(a, ha, 0, 0, cols, rows, ModeOctant, nil, nil)
	e.Encode(b, hb, 0, 0, cols, rows, ModeOctant, nil, &changed)
	var f Encoder
	f.Reset()
	f.Encode(a, uv.NewBuffer(cols, rows), 0, 0, cols, rows, ModeOctant, nil, nil)
	f.Encode(b, hc, 0, 0, cols, rows, ModeOctant, nil, nil)
	same, differ := 0, 0
	for y := range rows {
		for x := range cols {
			if *hb.CellAt(x, y) != *hc.CellAt(x, y) {
				t.Fatalf("cell %d,%d is %v with the changed pixels and %v without", x, y, hb.CellAt(x, y), hc.CellAt(x, y))
			}
			if *ha.CellAt(x, y) == *hb.CellAt(x, y) {
				same++
			} else {
				differ++
			}
		}
	}
	if differ == 0 || same == 0 {
		t.Fatalf("%d cells changed and %d stayed between the frames", differ, same)
	}
}

// A layer lit anew by lights that moved, left or arrived matches one lit whole by them, and so do the frames drawn over either
func TestRelightMatchesWhole(t *testing.T) {
	for name, look := range walkLooks {
		w := newWalkScene()
		w.bare = look
		r := w.raster()
		fr := &Frame{PixelAspect: 1}
		fr.Resize(160, 100)
		fr.Clear(w.sky)
		r.SetLights(w.steady)
		r.Begin(fr, w.cam)
		r.Chunks(w.chunks)
		var layer Layer
		r.FlushInto(&layer)
		var changed Changes
		changed.Reset(160, 100)
		layer.Restore(fr, nil)
		r.Begin(fr, w.cam)
		w.dynamic(r, 0)
		r.FlushOver(&layer)
		changed.CopyFrom(r.Changes())
		for _, k := range []int{3, 4} {
			lights := w.lights(k)
			layer.Restore(fr, &changed)
			r.SetLights(lights)
			r.Begin(fr, w.cam)
			r.Relight(&layer)
			relit := r.Changes().Color.Count()
			if relit == 0 || relit >= 160*100 {
				t.Fatalf("%s relight %d reworked %d pixels", name, k, relit)
			}
			if !slices.Equal(layer.lights, lights) {
				t.Fatalf("%s relight %d left the layer lit by %v", name, k, layer.lights)
			}
			two := w.raster()
			whole := &Frame{PixelAspect: 1}
			whole.Resize(160, 100)
			whole.Clear(w.sky)
			two.SetLights(lights)
			two.Begin(whole, w.cam)
			two.Chunks(w.chunks)
			var fresh Layer
			two.FlushInto(&fresh)
			for i := range fr.Color {
				if fr.Color[i] != layer.full[i] {
					t.Fatalf("%s relight %d pixel %d is %v on the frame and %v on the layer", name, k, i, fr.Color[i], layer.full[i])
				}
				if d := math.Sqrt(fresh.full[i].Dist(layer.full[i])); d > 0.02 {
					t.Fatalf("%s relight %d pixel %d finished %v whole and %v relit", name, k, i, fresh.full[i], layer.full[i])
				}
				if d := math.Sqrt(fresh.fr.Color[i].Dist(layer.fr.Color[i])); d > 0.02 {
					t.Fatalf("%s relight %d pixel %d lit %v whole and %v relit", name, k, i, fresh.fr.Color[i], layer.fr.Color[i])
				}
			}
			draw := func(r *Raster, l *Layer, f *Frame) {
				l.Restore(f, nil)
				if r.Shadows != nil {
					r.Shadows = w.cast(k)
				}
				r.SetLights(lights)
				r.Begin(f, w.cam)
				w.dynamic(r, k)
				r.FlushOver(l)
			}
			draw(r, &layer, fr)
			draw(two, &fresh, whole)
			for i := range fr.Color {
				if d := math.Sqrt(whole.Color[i].Dist(fr.Color[i])); d > 0.03 {
					t.Fatalf("%s frame %d pixel %d is %v over the whole layer and %v over the relit one", name, k, i, whole.Color[i], fr.Color[i])
				}
			}
			changed.CopyFrom(r.Changes())
		}
	}
}
