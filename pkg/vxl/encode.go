package vxl

import (
	"image/color"
	"math"
	"sort"

	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"
)

// How many pixels a cell holds and which glyphs it shows them with
type CellMode int

const (
	ModeHalf CellMode = iota
	ModeQuadrant
	ModeSextant
	ModeOctant
)

// Cell mode by its setting name, quadrant for an unknown one
func CellModeOf(name string) CellMode {
	switch name {
	case "half":
		return ModeHalf
	case "sextant":
		return ModeSextant
	case "octant":
		return ModeOctant
	}
	return ModeQuadrant
}

// Pixels across and down one cell holds
func (m CellMode) Pixels() (px, py int) {
	switch m {
	case ModeHalf:
		return 1, 2
	case ModeSextant:
		return 2, 3
	case ModeOctant:
		return 2, 4
	}
	return 2, 2
}

// Width over height of one pixel on screen, a cell being half as wide as it is tall
func (m CellMode) PixelAspect() float64 {
	px, py := m.Pixels()
	return float64(py) / float64(2*px)
}

// Glyph showing a mask of lit pixels, bit i the pixel i read across then down
func (m CellMode) Glyph(mask int) string {
	switch m {
	case ModeHalf:
		return halves[mask&3]
	case ModeSextant:
		return string(sextants[mask&63])
	case ModeOctant:
		return string(octants[mask&255])
	}
	return quadrants[mask&15]
}

// Half block glyphs by lit pixel mask, top one, bottom two
var halves = [4]string{" ", "▀", "▄", "█"}

// Quadrant glyphs by lit pixel mask, top left one, top right two, bottom left four, bottom right eight
var quadrants = [16]string{" ", "▘", "▝", "▀", "▖", "▌", "▞", "▛", "▗", "▚", "▐", "▜", "▄", "▙", "▟", "█"}

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

// Writes the frame into cells, each cell the two colors and glyph nearest its pixels
func (fr *Frame) Encode(buf *uv.Buffer, x0, y0, cols, rows int, mode CellMode, quantize func(RGB) color.Color) {
	if quantize == nil {
		quantize = func(c RGB) color.Color { return c.Color() }
	}
	px, py := mode.Pixels()
	n := px * py
	parallelRows(rows, func(r0, r1 int) {
		var pix [8]RGB
		for cy := r0; cy < r1; cy++ {
			for cx := range cols {
				for j := range py {
					for i := range px {
						pix[j*px+i] = fr.At(cx*px+i, cy*py+j).Clamp()
					}
				}
				mask, fg, bg := partition(pix[:n])
				c := uv.Cell{Content: mode.Glyph(mask), Width: 1, Style: uv.Style{Bg: quantize(bg)}}
				if mask != 0 {
					c.Style.Fg = quantize(fg)
				}
				buf.SetCell(x0+cx, y0+cy, &c)
			}
		}
	})
}

// Error below which pixels paint as one solid color rather than a pattern
const solidSlack = 0.0025

// Split of pixels into lit and unlit groups with their mean colors, cut along the axis they spread most along
func partition(px []RGB) (mask int, fg, bg RGB) {
	n := len(px)
	var mean RGB
	for _, p := range px {
		mean = mean.Add(p)
	}
	mean = mean.Scale(1 / float64(n))
	spread, total := 0.0, 0.0
	far := 0
	for i, p := range px {
		d := p.Dist(mean)
		total += d
		if d > spread {
			spread, far = d, i
		}
	}
	if total <= solidSlack*float64(n)/4 {
		return 0, mean, mean
	}
	axis := px[far].Sub3(mean)
	var order [8]int
	var proj [8]float64
	for i, p := range px {
		order[i] = i
		proj[i] = p.Sub3(mean).Dot3(axis)
	}
	sort.Slice(order[:n], func(i, j int) bool { return proj[order[i]] < proj[order[j]] })
	var prefix [9]RGB
	var prefix2 [9]float64
	for i := range n {
		p := px[order[i]]
		prefix[i+1] = prefix[i].Add(p)
		prefix2[i+1] = prefix2[i] + p.Dist(RGB{})
	}
	best := math.Inf(1)
	cut := 0
	for k := 1; k < n; k++ {
		lo, hi := prefix[k].Scale(1/float64(k)), prefix[n].Sub3(prefix[k]).Scale(1/float64(n-k))
		errLo := prefix2[k] - float64(k)*lo.Dist(RGB{})
		errHi := prefix2[n] - prefix2[k] - float64(n-k)*hi.Dist(RGB{})
		if e := errLo + errHi; e < best {
			best, cut = e, k
		}
	}
	bg = prefix[cut].Scale(1 / float64(cut))
	fg = prefix[n].Sub3(prefix[cut]).Scale(1 / float64(n-cut))
	for i := cut; i < n; i++ {
		mask |= 1 << order[i]
	}
	if mask == (1<<n)-1 {
		return 0, fg, fg
	}
	return mask, fg, bg
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
