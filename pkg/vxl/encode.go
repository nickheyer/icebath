package vxl

import (
	"image/color"
	"math"
	"math/bits"
	"sync"

	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"
)

//go:generate python3 -I ../../scripts/glyphs.py

// Glyph library a cell may pick its glyph from
type CellMode int

const (
	ModeHalf CellMode = iota
	ModeQuadrant
	ModeSextant
	ModeOctant
	ModeShapes
	ModeLegacy
	ModeAll
)

// Cell mode by its setting name, shapes for an unknown one
func CellModeOf(name string) CellMode {
	switch name {
	case "half":
		return ModeHalf
	case "quadrant":
		return ModeQuadrant
	case "sextant":
		return ModeSextant
	case "octant":
		return ModeOctant
	case "legacy":
		return ModeLegacy
	case "all":
		return ModeAll
	}
	return ModeShapes
}

// Pixels across and down every cell samples whatever its mode
const cellCols, cellRows = 4, 8

// Pixels one cell samples
const cellPixels = cellCols * cellRows

// Pixels across and down one cell samples
func (m CellMode) Pixels() (px, py int) { return cellCols, cellRows }

// Width over height of one pixel on screen, a cell being half as wide as it is tall
func (m CellMode) PixelAspect() float64 {
	px, py := m.Pixels()
	return float64(py) / float64(2*px)
}

// Coverage of each pixel of a glyph from 0 to 255, read across then down
type cover [cellPixels]uint8

// Glyph of the generated table with a bit per mode whose library holds it and its lit pixel mask
type glyph struct {
	str  string
	libs uint8
	mask uint32
	cov  cover
}

// Table indices of the blank and the full glyph, the first two the generator writes
const spaceGlyph, fullGlyph = 0, 1

// Coverage of a glyph as shares with the sums color fitting needs
type glyphFit struct {
	cov [cellPixels]float64
	// Sum of the coverage shares and of their squares
	sum, sq float64
	// Sum of the coverage bytes, how much of the cell the glyph covers
	weight int
}

// Masks of the glyphs a mode may pick beside their table indices
type library struct {
	ids   []uint16
	masks []uint32
}

var (
	fits       [len(glyphs)]glyphFit
	modeLibs   [ModeAll + 1]library
	glyphIndex = make(map[string]uint16, len(glyphs))
)

// Builds coverage shares, the glyph lookup and the mask list of every library from the library bits of each glyph
func init() {
	for i := range glyphs {
		g, f := &glyphs[i], &fits[i]
		for p, c := range g.cov {
			f.cov[p] = float64(c) / 255
			f.sum += f.cov[p]
			f.sq += f.cov[p] * f.cov[p]
			f.weight += int(c)
		}
		glyphIndex[g.str] = uint16(i)
		for m := range modeLibs {
			if g.libs>>m&1 != 0 {
				modeLibs[m].ids = append(modeLibs[m].ids, uint16(i))
				modeLibs[m].masks = append(modeLibs[m].masks, g.mask)
			}
		}
	}
}

// Lit pixel mask of a glyph in any library, bit i the pixel i%4 across and i/4 down
func GlyphMask(glyph string) (mask uint32, ok bool) {
	i, ok := glyphIndex[glyph]
	if !ok {
		return 0, false
	}
	return glyphs[i].mask, true
}

// Library of the mode, shapes for a mode outside the known ones
func (m CellMode) library() *library {
	if m < 0 || int(m) >= len(modeLibs) {
		m = ModeShapes
	}
	return &modeLibs[m]
}

// Writes the frame into cells, each the glyph and two colors nearest its pixels, remembering no earlier frame
func (fr *Frame) Encode(buf *uv.Buffer, x0, y0, cols, rows int, mode CellMode, quantize func(RGB) color.Color) {
	e := encoders.Get().(*Encoder)
	e.Reset()
	e.Encode(fr, buf, x0, y0, cols, rows, mode, quantize)
	encoders.Put(e)
}

// Encoders the stateless encode borrows, reset before each use
var encoders = sync.Pool{New: func() any { return new(Encoder) }}

// Writes frames into cells, holding each cell to its glyph and colors of the frame before while they still fit
type Encoder struct {
	cols, rows int
	mode       CellMode
	// Whether the cells hold colors boxed straight from their RGB rather than through a quantizer
	direct bool
	gen    uint32
	cells  []encCell
}

// Glyph and colors a cell showed, valid while its generation matches the encoder
type encCell struct {
	fg, bg   RGB
	qfg, qbg color.Color
	// Hash of the raw pixels the cell was last encoded from
	sum   uint64
	gen   uint32
	glyph uint16
}

// Extra squared error per pixel a held or sparser glyph may carry over the best fit before it gives way
const holdSlack = 0.003

// Squared distance under which a held color stands in for the new one, about three steps of 255 per channel
const colorSlack = 3 * (3.0 / 255) * (3.0 / 255)

// Error below which pixels paint as one solid color rather than a pattern
const solidSlack = 0.0025

// Glyphs nearest the split mask that get an exact color fit
const nearest = 4

// Forgets every cell, so the next frame encodes as if none came before
func (e *Encoder) Reset() {
	e.gen++
	if e.gen == 0 {
		clear(e.cells)
		e.gen = 1
	}
}

// Writes the frame into cells, each keeping its glyph and colors while they stay within slack of the best fit
func (e *Encoder) Encode(fr *Frame, buf *uv.Buffer, x0, y0, cols, rows int, mode CellMode, quantize func(RGB) color.Color) {
	cols, rows = max(cols, 0), max(rows, 0)
	direct := quantize == nil
	if e.cols != cols || e.rows != rows || e.mode != mode || e.direct != direct || len(e.cells) != cols*rows {
		e.cols, e.rows, e.mode, e.direct = cols, rows, mode, direct
		if cap(e.cells) >= cols*rows {
			e.cells = e.cells[:cols*rows]
		} else {
			e.cells = make([]encCell, cols*rows)
		}
		e.Reset()
	}
	if e.gen == 0 {
		e.gen = 1
	}
	lib := mode.library()
	inside := fr.W >= cols*cellCols && fr.H >= rows*cellRows
	super := 1
	if cols > 0 && rows > 0 && fr.W%(cols*cellCols) == 0 && fr.H%(rows*cellRows) == 0 && fr.W/(cols*cellCols) == fr.H/(rows*cellRows) {
		super = fr.W / (cols * cellCols)
	}
	parallelBands(rows, 2, func(r0, r1 int) {
		var pix [cellPixels]RGB
		for cy := r0; cy < r1; cy++ {
			for cx := range cols {
				switch {
				case super > 1:
					fr.gather(&pix, cx, cy, super)
				case inside:
					for j := range cellRows {
						at := (cy*cellRows+j)*fr.W + cx*cellCols
						copy(pix[j*cellCols:(j+1)*cellCols], fr.Color[at:at+cellCols])
					}
				default:
					for j := range cellRows {
						for i := range cellCols {
							pix[j*cellCols+i] = fr.At(cx*cellCols+i, cy*cellRows+j)
						}
					}
				}
				st := &e.cells[cy*cols+cx]
				sum := pixelSum(&pix)
				held := st.gen == e.gen
				if held && st.sum == sum {
					var qfg, qbg color.Color
					switch {
					case direct:
						qfg, qbg = st.qfg, st.qbg
					default:
						qbg = quantize(st.bg)
						if st.glyph != spaceGlyph {
							qfg = quantize(st.fg)
						}
					}
					c := uv.Cell{Content: glyphs[st.glyph].str, Width: 1, Style: uv.Style{Bg: qbg, Fg: qfg}}
					buf.SetCell(x0+cx, y0+cy, &c)
					continue
				}
				for i := range pix {
					pix[i] = pix[i].Clamp()
				}
				prev := -1
				if held {
					prev = int(st.glyph)
				}
				g, fg, bg := lib.fit(&pix, prev)
				if held && g == prev {
					if fg.Dist(st.fg) <= colorSlack {
						fg = st.fg
					}
					if bg.Dist(st.bg) <= colorSlack {
						bg = st.bg
					}
				}
				switch g {
				case spaceGlyph:
					fg = bg
				case fullGlyph:
					bg = fg
				}
				var qfg, qbg color.Color
				switch {
				case !direct:
					qbg = quantize(bg)
					if g != spaceGlyph {
						qfg = quantize(fg)
					}
				default:
					qbg = st.qbg
					if !held || bg != st.bg || qbg == nil {
						qbg = bg.Color()
					}
					if g != spaceGlyph {
						qfg = st.qfg
						if !held || fg != st.fg || qfg == nil {
							qfg = fg.Color()
						}
					}
				}
				*st = encCell{fg: fg, bg: bg, sum: sum, gen: e.gen, glyph: uint16(g)}
				if direct {
					st.qfg, st.qbg = qfg, qbg
				}
				c := uv.Cell{Content: glyphs[g].str, Width: 1, Style: uv.Style{Bg: qbg, Fg: qfg}}
				buf.SetCell(x0+cx, y0+cy, &c)
			}
		}
	})
}

// Fills the pixels of a cell with the means of the blocks of frame pixels it covers at a multiple
func (fr *Frame) gather(pix *[cellPixels]RGB, cx, cy, super int) {
	inv := 1 / float64(super*super)
	for j := range cellRows {
		for i := range cellCols {
			var sum RGB
			for dy := range super {
				at := ((cy*cellRows+j)*super+dy)*fr.W + (cx*cellCols+i)*super
				for _, c := range fr.Color[at : at+super] {
					sum = sum.Add(c)
				}
			}
			pix[j*cellCols+i] = sum.Scale(inv)
		}
	}
}

// Hash of the exact bits of a cell of pixels, mixed so any changed bit spreads through it all
func pixelSum(px *[cellPixels]RGB) uint64 {
	h0, h1, h2, h3 := uint64(0x9e3779b97f4a7c15), uint64(0xc2b2ae3d27d4eb4f), uint64(0x165667b19e3779f9), uint64(0x27d4eb2f165667c5)
	for i := 0; i < cellPixels; i += 4 {
		h0 = mixPixel(h0, &px[i])
		h1 = mixPixel(h1, &px[i+1])
		h2 = mixPixel(h2, &px[i+2])
		h3 = mixPixel(h3, &px[i+3])
	}
	h := bits.RotateLeft64(h0, 1) ^ bits.RotateLeft64(h1, 7) ^ bits.RotateLeft64(h2, 12) ^ bits.RotateLeft64(h3, 18)
	h ^= h >> 33
	h *= 0xff51afd7ed558ccd
	h ^= h >> 33
	h *= 0xc4ceb9fe1a85ec53
	return h ^ h>>33
}

// Lane of the pixel hash with the bits of one more pixel folded in
func mixPixel(h uint64, p *RGB) uint64 {
	h = bits.RotateLeft64(h^math.Float64bits(p.R)*0xc2b2ae3d27d4eb4f, 31) * 0x9e3779b185ebca87
	h = bits.RotateLeft64(h^math.Float64bits(p.G)*0xc2b2ae3d27d4eb4f, 31) * 0x9e3779b185ebca87
	return bits.RotateLeft64(h^math.Float64bits(p.B)*0xc2b2ae3d27d4eb4f, 31) * 0x9e3779b185ebca87
}

// Glyph and colors fitted to clamped pixels, the shown glyph kept while its error stays within slack of the best
func (l *library) fit(px *[cellPixels]RGB, prev int) (g int, fg, bg RGB) {
	var sum RGB
	sq := 0.0
	for i := range px {
		sum = sum.Add(px[i])
		sq += px[i].Dot3(px[i])
	}
	mean := sum.Scale(1.0 / cellPixels)
	var cand [nearest + 2]int
	n := 0
	if sq-cellPixels*mean.Dot3(mean) <= solidSlack*cellPixels/4 {
		cand[0], n = spaceGlyph, 1
	} else {
		var near [nearest]int
		n = copy(cand[:], near[:l.nearest(partition(px, sum, mean), &near)])
		for _, extra := range [2]int{spaceGlyph, fullGlyph} {
			if !containsGlyph(cand[:n], extra) {
				cand[n], n = extra, n+1
			}
		}
	}
	var fgs, bgs [nearest + 2]RGB
	var errs [nearest + 2]float64
	pick := 0
	for i := range n {
		fgs[i], bgs[i], errs[i] = fitGlyph(px, sum, sq, &fits[cand[i]])
		if errs[i] < errs[pick] {
			pick = i
		}
	}
	best := errs[pick]
	for i := range n {
		if errs[i] <= best+holdSlack*cellPixels && sparser(cand[i], cand[pick]) {
			pick = i
		}
	}
	g, fg, bg = cand[pick], fgs[pick], bgs[pick]
	if prev < 0 || prev == g {
		return g, fg, bg
	}
	pf, pb, perr := fitGlyph(px, sum, sq, &fits[prev])
	if perr <= best+holdSlack*cellPixels {
		return prev, pf, pb
	}
	return g, fg, bg
}

// Reports whether a glyph index is among the candidates
func containsGlyph(cand []int, g int) bool {
	for _, c := range cand {
		if c == g {
			return true
		}
	}
	return false
}

// Reports whether glyph a covers less of the cell than glyph b, the earlier glyph winning a tie
func sparser(a, b int) bool {
	wa, wb := fits[a].weight, fits[b].weight
	return wa < wb || wa == wb && a < b
}

// Colors and squared error painting pixels with a glyph, fg weighted by coverage and bg by the rest
func fitGlyph(px *[cellPixels]RGB, sum RGB, sq float64, f *glyphFit) (fg, bg RGB, err float64) {
	var t RGB
	for i := range px {
		c := f.cov[i]
		t.R += c * px[i].R
		t.G += c * px[i].G
		t.B += c * px[i].B
	}
	switch {
	case f.sum <= 0:
		bg = sum.Scale(1.0 / cellPixels)
		fg = bg
	case f.sum >= cellPixels:
		fg = sum.Scale(1.0 / cellPixels)
		bg = fg
	default:
		fg = t.Scale(1 / f.sum)
		bg = sum.Sub3(t).Scale(1 / (cellPixels - f.sum))
	}
	d := fg.Sub3(bg)
	err = sq - 2*bg.Dot3(sum) - 2*d.Dot3(t) + cellPixels*bg.Dot3(bg) + 2*f.sum*bg.Dot3(d) + f.sq*d.Dot3(d)
	return fg, bg, max(err, 0)
}

// Lit pixel mask of the best split of pixels into two groups, cut along their widest spread from the mean
func partition(px *[cellPixels]RGB, sum, mean RGB) uint32 {
	far, spread := 0, -1.0
	for i := range px {
		if d := px[i].Dist(mean); d > spread {
			spread, far = d, i
		}
	}
	axis := px[far].Sub3(mean)
	var keys [cellPixels]uint64
	for i := range px {
		b := math.Float64bits(px[i].Sub3(mean).Dot3(axis))
		if b>>63 != 0 {
			b = ^b
		} else {
			b |= 1 << 63
		}
		keys[i] = b&^(cellPixels-1) | uint64(i)
	}
	for i := 1; i < cellPixels; i++ {
		k := keys[i]
		j := i
		for ; j > 0 && keys[j-1] > k; j-- {
			keys[j] = keys[j-1]
		}
		keys[j] = k
	}
	var lo RGB
	best, cut := math.Inf(-1), 1
	for k := 1; k < cellPixels; k++ {
		lo = lo.Add(px[keys[k-1]&(cellPixels-1)])
		hi := sum.Sub3(lo)
		if s := lo.Dot3(lo)/float64(k) + hi.Dot3(hi)/float64(cellPixels-k); s > best {
			best, cut = s, k
		}
	}
	mask := uint32(0)
	for _, k := range keys[cut:] {
		mask |= 1 << (k & (cellPixels - 1))
	}
	return mask
}

// Table indices of the glyphs whose masks or complements lie fewest bits from the mask, ties to the earlier glyph
func (l *library) nearest(mask uint32, out *[nearest]int) int {
	var dist [nearest]int
	n, worst := 0, cellPixels+1
	for j, gm := range l.masks {
		h := bits.OnesCount32(mask ^ gm)
		h = min(h, cellPixels-h)
		if h >= worst {
			continue
		}
		k := min(n, nearest-1)
		for ; k > 0 && dist[k-1] > h; k-- {
			dist[k], out[k] = dist[k-1], out[k-1]
		}
		dist[k], out[k] = h, int(l.ids[j])
		if n < nearest {
			n++
		}
		if n == nearest {
			worst = dist[nearest-1]
		}
	}
	return n
}

// Difference of two colors channel by channel
func (c RGB) Sub3(o RGB) RGB { return RGB{c.R - o.R, c.G - o.G, c.B - o.B} }

// Dot product of two colors taken as vectors
func (c RGB) Dot3(o RGB) float64 { return c.R*o.R + c.G*o.G + c.B*o.B }

// Sixteen colors of a terminal with no palette of its own
var ansiPalette = [16]RGB{
	Hex(0x000000), Hex(0xaa0000), Hex(0x00aa00), Hex(0xaa5500), Hex(0x0000aa), Hex(0xaa00aa), Hex(0x00aaaa), Hex(0xaaaaaa),
	Hex(0x555555), Hex(0xff5555), Hex(0x55ff55), Hex(0xffff55), Hex(0x5555ff), Hex(0xff55ff), Hex(0x55ffff), Hex(0xffffff),
}

// Quantizer to the nearest of sixteen terminal colors, the given palette where it names them
func ANSIQuantizer(palette [16]color.Color) func(RGB) color.Color {
	table := ansiPalette
	for i, c := range palette {
		if c != nil {
			table[i] = FromColor(c)
		}
	}
	return func(c RGB) color.Color {
		best, at := math.Inf(1), 0
		for i, p := range table {
			if d := c.Dist(p); d < best {
				best, at = d, i
			}
		}
		return ansi.IndexedColor(uint8(at))
	}
}
