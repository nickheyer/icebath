package main

import (
	"fmt"
	"image"
	"math"
	"strings"
)

const (
	heroCols, heroRows = 30, 15
	logoRows           = 6
	inkCoverage        = 0.4
	accentCoverage     = 0.12
)

// Summed area tables of ink alpha and accent alpha, both in the zero to one range
type inkTable struct {
	w, h          int
	alpha, accent []float64
}

func newInkTable(img *image.NRGBA, r image.Rectangle, accent func(x, y int) float64) *inkTable {
	t := &inkTable{w: r.Dx(), h: r.Dy(), alpha: make([]float64, (r.Dx()+1)*(r.Dy()+1)), accent: make([]float64, (r.Dx()+1)*(r.Dy()+1))}
	for y := 0; y < r.Dy(); y++ {
		var rowA, rowC float64
		for x := 0; x < r.Dx(); x++ {
			a := float64(img.NRGBAAt(r.Min.X+x, r.Min.Y+y).A) / 255
			rowA += a
			rowC += a * accent(r.Min.X+x, r.Min.Y+y)
			i := (y+1)*(t.w+1) + x + 1
			t.alpha[i] = t.alpha[i-t.w-1] + rowA
			t.accent[i] = t.accent[i-t.w-1] + rowC
		}
	}
	return t
}

// Mean ink and accent over the source rectangle, clipped to the table
func (t *inkTable) mean(x0, y0, x1, y1 float64) (float64, float64) {
	ix0, iy0 := max(0, int(math.Floor(x0))), max(0, int(math.Floor(y0)))
	ix1, iy1 := min(t.w, int(math.Ceil(x1))), min(t.h, int(math.Ceil(y1)))
	if ix1 <= ix0 || iy1 <= iy0 {
		return 0, 0
	}
	area := float64((ix1 - ix0) * (iy1 - iy0))
	sum := func(v []float64) float64 {
		w := t.w + 1
		return v[iy1*w+ix1] - v[iy0*w+ix1] - v[iy1*w+ix0] + v[iy0*w+ix0]
	}
	return sum(t.alpha) / area, sum(t.accent) / area
}

// Dot grid of gw by gh fitted into the table with aspect kept for dots of width over height pa
func (t *inkTable) grid(gw, gh int, pa float64) (on, accent [][]bool) {
	ky := math.Min(float64(gh)/float64(t.h), float64(gw)*pa/float64(t.w))
	kx := ky / pa
	ox, oy := (float64(gw)-float64(t.w)*kx)/2, (float64(gh)-float64(t.h)*ky)/2
	on, accent = make([][]bool, gh), make([][]bool, gh)
	for y := 0; y < gh; y++ {
		on[y], accent[y] = make([]bool, gw), make([]bool, gw)
		for x := 0; x < gw; x++ {
			a, c := t.mean((float64(x)-ox)/kx, (float64(y)-oy)/ky, (float64(x)+1-ox)/kx, (float64(y)+1-oy)/ky)
			on[y][x] = a >= inkCoverage || c >= accentCoverage
			accent[y][x] = on[y][x] && (2*c > a || c >= accentCoverage)
		}
	}
	return on, accent
}

// Position of a colour on the letter to accent axis, zero for letters and one for accents
func accentT(r, g, b uint8, letter, red [3]float64) float64 {
	axis := [3]float64{red[0] - letter[0], red[1] - letter[1], red[2] - letter[2]}
	t := ((float64(r)-letter[0])*axis[0] + (float64(g)-letter[1])*axis[1] + (float64(b)-letter[2])*axis[2]) /
		(axis[0]*axis[0] + axis[1]*axis[1] + axis[2]*axis[2])
	return math.Max(0, math.Min(1, t))
}

// One terminal cell, its glyph and whether its ink is accent
type cell struct {
	glyph  rune
	inked  bool
	accent bool
}

// Braille hero of the stacked logo, thirty columns by fifteen rows of two by four dots
func (s *sources) heroCells() [][]cell {
	m := s.markWhite
	tight := m.Bounds().Inset(markMargin)
	t := newInkTable(m, tight, func(x, y int) float64 {
		if a := m.NRGBAAt(x, y).A; a > 0 {
			return float64(s.accent.AlphaAt(x, y).A) / float64(a)
		}
		return 0
	})
	on, acc := t.grid(heroCols*2, heroRows*4, 1)
	bits := [4][2]rune{{0x01, 0x08}, {0x02, 0x10}, {0x04, 0x20}, {0x40, 0x80}}
	rows := make([][]cell, heroRows)
	for cy := range rows {
		rows[cy] = make([]cell, heroCols)
		for cx := range rows[cy] {
			var code rune
			inked, accents := 0, 0
			for dy := 0; dy < 4; dy++ {
				for dx := 0; dx < 2; dx++ {
					if on[cy*4+dy][cx*2+dx] {
						code |= bits[dy][dx]
						inked++
						if acc[cy*4+dy][cx*2+dx] {
							accents++
						}
					}
				}
			}
			rows[cy][cx] = cell{glyph: 0x2800 + code, inked: inked > 0, accent: 2*accents > inked}
		}
	}
	return rows
}

// Quadrant block logo of the wide wordmark, six rows of two by two pixels at the wordmark's own aspect
func (s *sources) logoCells() [][]cell {
	letter := [3]float64{float64(s.letter.R), float64(s.letter.G), float64(s.letter.B)}
	red := [3]float64{float64(s.red.R), float64(s.red.G), float64(s.red.B)}
	box := inkBox(s.textWhite)
	t := newInkTable(s.textWhite, box, func(x, y int) float64 {
		c := s.textWhite.NRGBAAt(x, y)
		return accentT(c.R, c.G, c.B, letter, red)
	})
	cols := int(math.Round(float64(logoRows*2) * float64(box.Dx()) / float64(box.Dy())))
	on, acc := t.grid(cols*2, logoRows*2, 0.5)
	glyphs := []rune(" ▘▝▀▖▌▞▛▗▚▐▜▄▙▟█")
	rows := make([][]cell, logoRows)
	for cy := range rows {
		rows[cy] = make([]cell, cols)
		for cx := range rows[cy] {
			code, accents := 0, 0
			for i, p := range [4][2]int{{0, 0}, {1, 0}, {0, 1}, {1, 1}} {
				x, y := cx*2+p[0], cy*2+p[1]
				if on[y][x] {
					code |= 1 << i
					if acc[y][x] {
						accents++
					}
				}
			}
			rows[cy][cx] = cell{glyph: glyphs[code], inked: code != 0, accent: accents > 0}
		}
	}
	return rows
}

func plainLines(rows [][]cell) []string {
	out := make([]string, len(rows))
	for i, row := range rows {
		var b strings.Builder
		for _, c := range row {
			b.WriteRune(c.glyph)
		}
		out[i] = b.String()
	}
	return out
}

// Rich markup lines, runs of letter cells in the letter colour and accent runs in red, blanks joining a run
func (s *sources) markupLines(rows [][]cell) []string {
	out := make([]string, len(rows))
	for i, row := range rows {
		var b strings.Builder
		cur, run := false, ""
		started := false
		for j, c := range row {
			class := cur
			if c.inked {
				class = c.accent
			} else if !started {
				for _, next := range row[j:] {
					if next.inked {
						class = next.accent
						break
					}
				}
			}
			if started && class != cur {
				b.WriteString(s.tag(cur, run))
				run = ""
			}
			cur, started = class, true
			run += string(c.glyph)
		}
		b.WriteString(s.tag(cur, run))
		out[i] = b.String()
	}
	return out
}

func (s *sources) tag(accent bool, text string) string {
	c := s.letter
	if accent {
		c = s.red
	}
	return fmt.Sprintf("[%s]%s[/]", hexOf(c), text)
}

// Go source holding the CLI banner logo and hero as rich markup constants
func (s *sources) bannerGo() []byte {
	var b strings.Builder
	b.WriteString("package app\n\n")
	writeConst := func(name string, lines []string) {
		fmt.Fprintf(&b, "const %s = ", name)
		for i, line := range lines {
			if i > 0 {
				b.WriteString("\t")
			}
			if i < len(lines)-1 {
				fmt.Fprintf(&b, "%q +\n", line+"\n")
			} else {
				fmt.Fprintf(&b, "%q\n", line)
			}
		}
	}
	writeConst("icebathAgentLogo", s.markupLines(s.logoCells()))
	b.WriteString("\n")
	writeConst("icebathHero", s.markupLines(s.heroCells()))
	return []byte(b.String())
}

// TypeScript module holding the TUI splash logo and hero as plain lines the theme colours
func (s *sources) bannerTS() []byte {
	var b strings.Builder
	writeArray := func(name string, lines []string) {
		fmt.Fprintf(&b, "export const %s = [\n", name)
		for i, line := range lines {
			sep := ","
			if i == len(lines)-1 {
				sep = ""
			}
			fmt.Fprintf(&b, "  '%s'%s\n", line, sep)
		}
		b.WriteString("]\n")
	}
	writeArray("LOGO_ART", plainLines(s.logoCells()))
	b.WriteString("\n")
	writeArray("HERO_ART", plainLines(s.heroCells()))
	return []byte(b.String())
}
