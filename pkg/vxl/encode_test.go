package vxl

import (
	"image/color"
	"math"
	"testing"

	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"
)

var allModes = []CellMode{ModeHalf, ModeQuadrant, ModeSextant, ModeOctant, ModeShapes, ModeLegacy, ModeAll}

// Cell of pixels painted by a function of the pixel position
func cellOf(f func(x, y int) RGB) *[cellPixels]RGB {
	var px [cellPixels]RGB
	for i := range px {
		px[i] = f(i%cellCols, i/cellCols)
	}
	return &px
}

// Solid cells paint blank, half cells pick a half block with exact colors, and a lit corner picks a quadrant
func TestPartition(t *testing.T) {
	red, white, black, green := RGB{1, 0, 0}, RGB{1, 1, 1}, RGB{}, RGB{0, 1, 0}
	for _, mode := range allModes {
		lib := mode.library()
		if g, _, bg := lib.fit(cellOf(func(x, y int) RGB { return red }), -1); g != spaceGlyph || bg != red {
			t.Fatalf("mode %d solid cell %q %v", mode, glyphs[g].str, bg)
		}
		for _, split := range []struct {
			lit      func(x, y int) bool
			on, off  string
			required bool
		}{
			{func(x, y int) bool { return y < cellRows/2 }, "▀", "▄", mode != ModeSextant},
			{func(x, y int) bool { return x < cellCols/2 }, "▌", "▐", mode != ModeHalf},
		} {
			if !split.required {
				continue
			}
			g, fg, bg := lib.fit(cellOf(func(x, y int) RGB {
				if split.lit(x, y) {
					return white
				}
				return black
			}), -1)
			if s := glyphs[g].str; (s != split.on || fg != white || bg != black) && (s != split.off || fg != black || bg != white) {
				t.Fatalf("mode %d half cell %q %v %v", mode, s, fg, bg)
			}
		}
		corner, on, off := cellRows/2, "▘", "▟"
		switch mode {
		case ModeHalf:
			continue
		case ModeSextant:
			corner, on, off = 3, "\U0001FB00", "\U0001FB3B"
		}
		g, fg, bg := lib.fit(cellOf(func(x, y int) RGB {
			if x < cellCols/2 && y < corner {
				return green
			}
			return black
		}), -1)
		near := func(a, b RGB) bool { return a.Dist(b) < 0.01 }
		if s := glyphs[g].str; (s != on || !near(fg, green) || !near(bg, black)) && (s != off || !near(fg, black) || !near(bg, green)) {
			t.Fatalf("mode %d corner cell %q %v %v", mode, s, fg, bg)
		}
	}
	column := cellOf(func(x, y int) RGB { return Grey(float64(1 - x/2)) })
	var sum RGB
	for _, p := range column {
		sum = sum.Add(p)
	}
	if mask := partition(column, sum, sum.Scale(1.0/cellPixels)); mask != 0x33333333 && mask != 0xcccccccc {
		t.Fatalf("column cell split %032b", mask)
	}
}

// Mask of a grid of block cells spread over the pixels, each pixel lit by the block holding its centre
func expandMask(mask, across, down int) uint32 {
	out := uint32(0)
	for i := range cellPixels {
		bx, by := (2*(i%cellCols)+1)*across/(2*cellCols), (2*(i/cellCols)+1)*down/(2*cellRows)
		if mask>>(by*across+bx)&1 != 0 {
			out |= 1 << i
		}
	}
	return out
}

// Every library holds distinct single width glyphs including blank and full, and the grid libraries match their block tables
func TestGlyphTables(t *testing.T) {
	if glyphs[spaceGlyph].str != " " || glyphs[fullGlyph].str != "█" {
		t.Fatalf("table starts %q %q", glyphs[spaceGlyph].str, glyphs[fullGlyph].str)
	}
	for m, lib := range modeLibs {
		ids := lib.ids
		seen := map[string]bool{}
		for _, id := range ids {
			g := glyphs[id]
			if cluster, _ := ansi.FirstGraphemeCluster(g.str, ansi.GraphemeWidth); cluster != g.str || ansi.StringWidth(g.str) != 1 {
				t.Fatalf("mode %d glyph %q is not one single width grapheme", m, g.str)
			}
			if seen[g.str] {
				t.Fatalf("mode %d lists glyph %q twice", m, g.str)
			}
			seen[g.str] = true
			if g.libs>>m&1 == 0 {
				t.Fatalf("mode %d lists glyph %q without its library bit", m, g.str)
			}
			if mask, ok := GlyphMask(g.str); !ok || mask != g.mask {
				t.Fatalf("glyph %q mask %08x ok %v, want %08x", g.str, mask, ok, g.mask)
			}
		}
		bits := 0
		for _, g := range glyphs {
			bits += int(g.libs >> m & 1)
		}
		if bits != len(ids) || !seen[" "] || !seen["█"] {
			t.Fatalf("mode %d lists %d glyphs, %d carry its bit, blank %v full %v", m, len(ids), bits, seen[" "], seen["█"])
		}
	}
	if _, ok := GlyphMask("x"); ok {
		t.Fatal("a letter has a glyph mask")
	}
	grids := []struct {
		mode         CellMode
		table        []rune
		across, down int
	}{
		{ModeHalf, []rune(" ▀▄█"), 1, 2},
		{ModeQuadrant, []rune(" ▘▝▀▖▌▞▛▗▚▐▜▄▙▟█"), 2, 2},
		{ModeSextant, sextants[:], 2, 3},
		{ModeOctant, octants[:], 2, 4},
	}
	for _, gr := range grids {
		if len(modeLibs[gr.mode].ids) != len(gr.table) {
			t.Fatalf("mode %d holds %d glyphs, its table %d", gr.mode, len(modeLibs[gr.mode].ids), len(gr.table))
		}
		for mask, r := range gr.table {
			id, ok := glyphIndex[string(r)]
			if !ok || glyphs[id].libs>>gr.mode&1 == 0 {
				t.Fatalf("mode %d lacks %U", gr.mode, r)
			}
			if got, want := glyphs[id].mask, expandMask(mask, gr.across, gr.down); got != want {
				t.Fatalf("mode %d glyph %U mask %032b, want %032b", gr.mode, r, got, want)
			}
		}
	}
	w := glyphs[glyphIndex["\U0001FB3C"]]
	total, corner := 0, 0
	for i, c := range w.cov {
		total += int(c)
		if i%cellCols < 2 && i/cellCols >= 5 {
			corner += int(c)
		} else if c != 0 {
			t.Fatalf("wedge U+1FB3C covers pixel %d,%d by %d", i%cellCols, i/cellCols, c)
		}
	}
	if w.cov[cellPixels-cellCols] != 255 || w.cov[cellPixels-cellCols+1] == 255 || total == 0 || corner != total {
		t.Fatalf("wedge U+1FB3C coverage %v", w.cov)
	}
	for name, want := range map[string]CellMode{"half": ModeHalf, "quadrant": ModeQuadrant, "sextant": ModeSextant, "octant": ModeOctant, "shapes": ModeShapes, "legacy": ModeLegacy, "all": ModeAll, "?": ModeShapes} {
		if got := CellModeOf(name); got != want {
			t.Fatalf("mode %q is %d, want %d", name, got, want)
		}
	}
}

// Every mode fills every cell of a small frame with and without a quantizer
func TestQuantizerAndEncode(t *testing.T) {
	q := ANSIQuantizer([16]color.Color{})
	if c := q(RGB{1, 1, 1}); c != ansi.IndexedColor(15) {
		t.Fatalf("white quantized to %v", c)
	}
	fr := &Frame{PixelAspect: 1}
	fr.Resize(8, 16)
	for i := range fr.Color {
		fr.Color[i] = Grey(float64(i%fr.W+i/fr.W) / 22)
	}
	for _, mode := range allModes {
		px, py := mode.Pixels()
		cols, rows := 8/px, 16/py
		for _, quant := range []func(RGB) color.Color{nil, q} {
			buf := uv.NewBuffer(cols, rows)
			fr.Encode(buf, 0, 0, cols, rows, mode, quant)
			for i := range cols * rows {
				if c := buf.CellAt(i%cols, i/cols); c == nil || c.Style.Bg == nil || c.Content == "" {
					t.Fatalf("mode %d left cell %d empty", mode, i)
				}
			}
		}
	}
}

// Paints hard edged shapes over a gradient sky and floor, light scaled by flicker and the disc moved by drift
func paintScene(fr *Frame, flicker, drift float64) {
	w, h := float64(fr.W), float64(fr.H)
	sa, ca := math.Sincos(0.5)
	for y := range fr.H {
		for x := range fr.W {
			fx, fy := float64(x)+0.5, float64(y)+0.5
			c := RGB{0.1, 0.15, 0.3}.Mix(RGB{0.35, 0.45, 0.6}, fy/h)
			if fy > 0.55*h {
				c = RGB{0.3, 0.33, 0.38}
				if int(fx)%24 < 2 || int(fy)%12 < 1 {
					c = c.Scale(0.6)
				}
			}
			if dx, dy, r := fx-0.3*w-drift, fy-0.5*h, 0.2*h; dx*dx+dy*dy < r*r {
				c = RGB{0.8, 0.3, 0.2}.Scale(0.5 + 0.35*max(0, -(dx+dy))/r)
			}
			if u, v := (fx-0.65*w)*ca+(fy-0.6*h)*sa, -(fx-0.65*w)*sa+(fy-0.6*h)*ca; math.Abs(u) < 0.12*w && math.Abs(v) < 0.1*h {
				c = RGB{0.2, 0.6, 0.4}.Scale(0.6 + 0.4*(u/(0.24*w)+0.5))
			}
			if fy > 0.15*h && fy < 0.85*h && math.Abs(fx-0.85*w-0.3*(fy-0.15*h)) < 1 {
				c = RGB{0.9, 0.9, 0.8}
			}
			if fy > 0.1*h && fy < 0.35*h && math.Abs(fx-0.12*w) < (fy-0.1*h)*0.8 {
				c = RGB{0.7, 0.65, 0.2}
			}
			lx, ly := fx-0.5*w, fy-0.3*h
			fr.Color[y*fr.W+x] = c.Add(RGB{1, 0.6, 0.3}.Scale(0.35 * flicker / (1 + (lx*lx+ly*ly)/(0.16*w*w))))
		}
	}
}

// Frame of a size painted with the scene
func sceneFrame(w, h int, flicker, drift float64) *Frame {
	fr := &Frame{PixelAspect: 1}
	fr.Resize(w, h)
	paintScene(fr, flicker, drift)
	return fr
}

// Cells that differ between two buffers of the same size
func changedCells(a, b *uv.Buffer) int {
	n := 0
	for y := range a.Height() {
		for x := range a.Width() {
			if !a.CellAt(x, y).Equal(b.CellAt(x, y)) {
				n++
			}
		}
	}
	return n
}

// Held encoders change far fewer cells over a flicker than fresh ones and start and reset like the stateless encode
func TestEncoderHoldsCells(t *testing.T) {
	cols, rows := 80, 30
	a, b := sceneFrame(cols*cellCols, rows*cellRows, 1, 0), sceneFrame(cols*cellCols, rows*cellRows, 1.03, 0)
	for _, mode := range allModes {
		fa, fb := uv.NewBuffer(cols, rows), uv.NewBuffer(cols, rows)
		a.Encode(fa, 0, 0, cols, rows, mode, nil)
		b.Encode(fb, 0, 0, cols, rows, mode, nil)
		var e Encoder
		ha, hb := uv.NewBuffer(cols, rows), uv.NewBuffer(cols, rows)
		e.Encode(a, ha, 0, 0, cols, rows, mode, nil)
		e.Encode(b, hb, 0, 0, cols, rows, mode, nil)
		fresh, held := changedCells(fa, fb), changedCells(ha, hb)
		if fresh < 200 || held*5 > fresh {
			t.Fatalf("mode %d fresh encoders changed %d cells, the held one %d", mode, fresh, held)
		}
		if n := changedCells(fa, ha); n != 0 {
			t.Fatalf("mode %d first frame through a new encoder differs from the stateless one in %d cells", mode, n)
		}
		e.Reset()
		hc := uv.NewBuffer(cols, rows)
		e.Encode(b, hc, 0, 0, cols, rows, mode, nil)
		if changedCells(fb, hc) != 0 {
			t.Fatalf("mode %d reset encoder still held cells", mode)
		}
		q := ANSIQuantizer([16]color.Color{})
		pa, pb := uv.NewBuffer(cols, rows), uv.NewBuffer(cols, rows)
		e.Encode(b, pa, 0, 0, cols, rows, mode, q)
		b.Encode(pb, 0, 0, cols, rows, mode, q)
		if changedCells(pa, pb) != 0 {
			t.Fatalf("mode %d switching to a quantizer kept colors boxed for the direct path", mode)
		}
	}
}

// Reports whether a glyph has a slanted edge, the corner triangles, diagonals and legacy wedges
func slanted(s string) bool {
	r := []rune(s)
	return len(r) == 1 && (r[0] >= 0x25E2 && r[0] <= 0x25E5 || r[0] >= 0x2571 && r[0] <= 0x2573 || r[0] >= 0x1FB3C && r[0] <= 0x1FB6F || r[0] == 0x1FB9A || r[0] == 0x1FB9B)
}

// An edge along y = 2x picks slanted glyphs in every cell it crosses rather than quadrant steps
func TestDiagonalEdge(t *testing.T) {
	cols, rows := 24, 12
	fr := &Frame{PixelAspect: 1}
	fr.Resize(cols*cellCols, rows*cellRows)
	for i := range fr.Color {
		if x, y := float64(i%fr.W)+0.5, float64(i/fr.W)+0.5; y > 2*x {
			fr.Color[i] = RGB{1, 1, 1}
		}
	}
	for _, mode := range []CellMode{ModeShapes, ModeLegacy, ModeAll} {
		buf := uv.NewBuffer(cols, rows)
		fr.Encode(buf, 0, 0, cols, rows, mode, nil)
		edge := 0
		for cy := range rows {
			for cx := range cols {
				lit := 0
				for j := range cellRows {
					for i := range cellCols {
						if fr.At(cx*cellCols+i, cy*cellRows+j).R > 0.5 {
							lit++
						}
					}
				}
				if lit == 0 || lit == cellPixels {
					continue
				}
				edge++
				if c := buf.CellAt(cx, cy); !slanted(c.Content) {
					t.Fatalf("mode %d edge cell %d,%d shows %q", mode, cx, cy, c.Content)
				}
			}
		}
		if edge < rows {
			t.Fatalf("mode %d edge crossed only %d cells", mode, edge)
		}
	}
}

// Pixels a cell paints, its glyph coverage blending its background into its foreground
func paintBack(c *uv.Cell) (px [cellPixels]RGB) {
	g := glyphs[glyphIndex[c.Content]]
	fg, bg := FromColor(c.Style.Fg), FromColor(c.Style.Bg)
	for i := range px {
		px[i] = bg.Mix(fg, float64(g.cov[i])/255)
	}
	return px
}

// A bright line one pixel wide on a dark field picks a thin bar that paints it back in place
func TestThinLine(t *testing.T) {
	cols, rows := 16, 4
	fr := &Frame{PixelAspect: 1}
	fr.Resize(cols*cellCols, rows*cellRows)
	left, right := 5*cellCols, 9*cellCols+cellCols-1
	for i := range fr.Color {
		fr.Color[i] = Grey(0.05)
		if x := i % fr.W; x == left || x == right {
			fr.Color[i] = Grey(1)
		}
	}
	buf := uv.NewBuffer(cols, rows)
	fr.Encode(buf, 0, 0, cols, rows, ModeShapes, nil)
	for cy := range rows {
		for _, cx := range []int{left / cellCols, right / cellCols} {
			c := buf.CellAt(cx, cy)
			if r := []rune(c.Content); len(r) != 1 || (r[0] < 0x2589 || r[0] > 0x258F) && r[0] != 0x2595 && r[0] != 0x2502 && r[0] != 0x2503 {
				t.Fatalf("line cell %d,%d shows %q", cx, cy, c.Content)
			}
			for i, p := range paintBack(c) {
				if want := fr.At(cx*cellCols+i%cellCols, cy*cellRows+i/cellCols); p.Dist(want) > 0.01 {
					t.Fatalf("line cell %d,%d %q paints pixel %d as %v, want %v", cx, cy, c.Content, i, p, want)
				}
			}
		}
	}
}

// Fitted colors are the coverage weighted means and the error is the summed squared distance to the painted pixels
func TestFitGlyphError(t *testing.T) {
	r := RNG{Seed: 7}
	for range 50 {
		px := cellOf(func(x, y int) RGB { return RGB{r.Next(), r.Next(), r.Next()} })
		var sum RGB
		sq := 0.0
		for _, p := range px {
			sum = sum.Add(p)
			sq += p.Dot3(p)
		}
		for g := range glyphs {
			fg, bg, err := fitGlyph(px, sum, sq, &fits[g])
			var wf, wb RGB
			cf, cb := 0.0, 0.0
			for i, p := range px {
				c := fits[g].cov[i]
				wf, wb, cf, cb = wf.Add(p.Scale(c)), wb.Add(p.Scale(1-c)), cf+c, cb+1-c
			}
			if cf > 0 && fg.Dist(wf.Scale(1/cf)) > 1e-18 || cb > 0 && bg.Dist(wb.Scale(1/cb)) > 1e-18 {
				t.Fatalf("glyph %q colors %v %v, want %v %v", glyphs[g].str, fg, bg, wf.Scale(1/cf), wb.Scale(1/cb))
			}
			want := 0.0
			for i, p := range px {
				want += p.Dist(bg.Add(fg.Sub3(bg).Scale(fits[g].cov[i])))
			}
			if math.Abs(err-want) > 1e-9 {
				t.Fatalf("glyph %q error %v, want %v", glyphs[g].str, err, want)
			}
		}
	}
}

// Fitting a cell allocates nothing in any mode
func TestFitAllocations(t *testing.T) {
	fr := sceneFrame(64, 64, 1, 0)
	var px [cellPixels]RGB
	for _, mode := range allModes {
		lib := mode.library()
		allocs := testing.AllocsPerRun(50, func() {
			for cy := range 8 {
				for cx := range 16 {
					for i := range px {
						px[i] = fr.At(cx*cellCols+i%cellCols, cy*cellRows+i/cellCols).Clamp()
					}
					lib.fit(&px, spaceGlyph)
				}
			}
		})
		if allocs != 0 {
			t.Fatalf("mode %d fitting allocated %v times", mode, allocs)
		}
	}
}

// Encodes a full screen of scene frames through a held encoder, every pixel changing a little each frame
func BenchmarkEncode(b *testing.B) {
	cols, rows := 220, 56
	frames := make([]*Frame, 8)
	for i := range frames {
		frames[i] = sceneFrame(cols*cellCols, rows*cellRows, 1+0.03*math.Sin(float64(i)), 0.25*float64(i))
	}
	buf := uv.NewBuffer(cols, rows)
	var e Encoder
	e.Encode(frames[len(frames)-1], buf, 0, 0, cols, rows, ModeAll, nil)
	i := 0
	for b.Loop() {
		e.Encode(frames[i%len(frames)], buf, 0, 0, cols, rows, ModeAll, nil)
		i++
	}
}

// Sextant glyphs by lit pixel mask, two across and three down, bit i the pixel i read across then down
var sextants = [64]rune{
	0x0020, 0x1FB00, 0x1FB01, 0x1FB02, 0x1FB03, 0x1FB04, 0x1FB05, 0x1FB06,
	0x1FB07, 0x1FB08, 0x1FB09, 0x1FB0A, 0x1FB0B, 0x1FB0C, 0x1FB0D, 0x1FB0E,
	0x1FB0F, 0x1FB10, 0x1FB11, 0x1FB12, 0x1FB13, 0x258C, 0x1FB14, 0x1FB15,
	0x1FB16, 0x1FB17, 0x1FB18, 0x1FB19, 0x1FB1A, 0x1FB1B, 0x1FB1C, 0x1FB1D,
	0x1FB1E, 0x1FB1F, 0x1FB20, 0x1FB21, 0x1FB22, 0x1FB23, 0x1FB24, 0x1FB25,
	0x1FB26, 0x1FB27, 0x2590, 0x1FB28, 0x1FB29, 0x1FB2A, 0x1FB2B, 0x1FB2C,
	0x1FB2D, 0x1FB2E, 0x1FB2F, 0x1FB30, 0x1FB31, 0x1FB32, 0x1FB33, 0x1FB34,
	0x1FB35, 0x1FB36, 0x1FB37, 0x1FB38, 0x1FB39, 0x1FB3A, 0x1FB3B, 0x2588,
}

// Octant glyphs by lit pixel mask, two across and four down
var octants = [256]rune{
	0x0020, 0x1CEA8, 0x1CEAB, 0x1FB82, 0x1CD00, 0x2598, 0x1CD01, 0x1CD02,
	0x1CD03, 0x1CD04, 0x259D, 0x1CD05, 0x1CD06, 0x1CD07, 0x1CD08, 0x2580,
	0x1CD09, 0x1CD0A, 0x1CD0B, 0x1CD0C, 0x1FBE6, 0x1CD0D, 0x1CD0E, 0x1CD0F,
	0x1CD10, 0x1CD11, 0x1CD12, 0x1CD13, 0x1CD14, 0x1CD15, 0x1CD16, 0x1CD17,
	0x1CD18, 0x1CD19, 0x1CD1A, 0x1CD1B, 0x1CD1C, 0x1CD1D, 0x1CD1E, 0x1CD1F,
	0x1FBE7, 0x1CD20, 0x1CD21, 0x1CD22, 0x1CD23, 0x1CD24, 0x1CD25, 0x1CD26,
	0x1CD27, 0x1CD28, 0x1CD29, 0x1CD2A, 0x1CD2B, 0x1CD2C, 0x1CD2D, 0x1CD2E,
	0x1CD2F, 0x1CD30, 0x1CD31, 0x1CD32, 0x1CD33, 0x1CD34, 0x1CD35, 0x1FB85,
	0x1CEA3, 0x1CD36, 0x1CD37, 0x1CD38, 0x1CD39, 0x1CD3A, 0x1CD3B, 0x1CD3C,
	0x1CD3D, 0x1CD3E, 0x1CD3F, 0x1CD40, 0x1CD41, 0x1CD42, 0x1CD43, 0x1CD44,
	0x2596, 0x1CD45, 0x1CD46, 0x1CD47, 0x1CD48, 0x258C, 0x1CD49, 0x1CD4A,
	0x1CD4B, 0x1CD4C, 0x259E, 0x1CD4D, 0x1CD4E, 0x1CD4F, 0x1CD50, 0x259B,
	0x1CD51, 0x1CD52, 0x1CD53, 0x1CD54, 0x1CD55, 0x1CD56, 0x1CD57, 0x1CD58,
	0x1CD59, 0x1CD5A, 0x1CD5B, 0x1CD5C, 0x1CD5D, 0x1CD5E, 0x1CD5F, 0x1CD60,
	0x1CD61, 0x1CD62, 0x1CD63, 0x1CD64, 0x1CD65, 0x1CD66, 0x1CD67, 0x1CD68,
	0x1CD69, 0x1CD6A, 0x1CD6B, 0x1CD6C, 0x1CD6D, 0x1CD6E, 0x1CD6F, 0x1CD70,
	0x1CEA0, 0x1CD71, 0x1CD72, 0x1CD73, 0x1CD74, 0x1CD75, 0x1CD76, 0x1CD77,
	0x1CD78, 0x1CD79, 0x1CD7A, 0x1CD7B, 0x1CD7C, 0x1CD7D, 0x1CD7E, 0x1CD7F,
	0x1CD80, 0x1CD81, 0x1CD82, 0x1CD83, 0x1CD84, 0x1CD85, 0x1CD86, 0x1CD87,
	0x1CD88, 0x1CD89, 0x1CD8A, 0x1CD8B, 0x1CD8C, 0x1CD8D, 0x1CD8E, 0x1CD8F,
	0x2597, 0x1CD90, 0x1CD91, 0x1CD92, 0x1CD93, 0x259A, 0x1CD94, 0x1CD95,
	0x1CD96, 0x1CD97, 0x2590, 0x1CD98, 0x1CD99, 0x1CD9A, 0x1CD9B, 0x259C,
	0x1CD9C, 0x1CD9D, 0x1CD9E, 0x1CD9F, 0x1CDA0, 0x1CDA1, 0x1CDA2, 0x1CDA3,
	0x1CDA4, 0x1CDA5, 0x1CDA6, 0x1CDA7, 0x1CDA8, 0x1CDA9, 0x1CDAA, 0x1CDAB,
	0x2582, 0x1CDAC, 0x1CDAD, 0x1CDAE, 0x1CDAF, 0x1CDB0, 0x1CDB1, 0x1CDB2,
	0x1CDB3, 0x1CDB4, 0x1CDB5, 0x1CDB6, 0x1CDB7, 0x1CDB8, 0x1CDB9, 0x1CDBA,
	0x1CDBB, 0x1CDBC, 0x1CDBD, 0x1CDBE, 0x1CDBF, 0x1CDC0, 0x1CDC1, 0x1CDC2,
	0x1CDC3, 0x1CDC4, 0x1CDC5, 0x1CDC6, 0x1CDC7, 0x1CDC8, 0x1CDC9, 0x1CDCA,
	0x1CDCB, 0x1CDCC, 0x1CDCD, 0x1CDCE, 0x1CDCF, 0x1CDD0, 0x1CDD1, 0x1CDD2,
	0x1CDD3, 0x1CDD4, 0x1CDD5, 0x1CDD6, 0x1CDD7, 0x1CDD8, 0x1CDD9, 0x1CDDA,
	0x2584, 0x1CDDB, 0x1CDDC, 0x1CDDD, 0x1CDDE, 0x2599, 0x1CDDF, 0x1CDE0,
	0x1CDE1, 0x1CDE2, 0x259F, 0x1CDE3, 0x2586, 0x1CDE4, 0x1CDE5, 0x2588,
}
