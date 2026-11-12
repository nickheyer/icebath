// Command glyphgen writes pkg/vxl/glyphs.go, every encoder glyph rasterized from the geometry its Unicode name describes
package main

import (
	"fmt"
	"math"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"golang.org/x/text/unicode/runenames"
)

const (
	cols, rows, sub         = 4, 8, 8
	light, heavy, diagonal  = 1.0 / 8, 1.0 / 4, 1.0 / 6
	sideWords, epsilonCross = "(UPPER|LOWER|LEFT|RIGHT)", 1e-12
)

// Point of the unit cell, x to the right and y downward
type pt struct{ x, y float64 }

type poly []pt

var (
	verticalOrder = []string{"UPPER MIDDLE", "LOWER MIDDLE", "MIDDLE", "UPPER", "LOWER"}
	vertical      = map[string]float64{"UPPER": 0, "UPPER MIDDLE": 1.0 / 3, "MIDDLE": 0.5, "LOWER MIDDLE": 2.0 / 3, "LOWER": 1}
	horizontal    = map[string]float64{"LEFT": 0, "CENTRE": 0.5, "RIGHT": 1}
	shareOrder    = []string{"ONE EIGHTH", "ONE QUARTER", "THREE EIGHTHS", "HALF", "FIVE EIGHTHS", "THREE QUARTERS", "SEVEN EIGHTHS"}
	shares        = map[string]float64{"ONE EIGHTH": 1.0 / 8, "ONE QUARTER": 1.0 / 4, "THREE EIGHTHS": 3.0 / 8, "HALF": 1.0 / 2,
		"FIVE EIGHTHS": 5.0 / 8, "THREE QUARTERS": 3.0 / 4, "SEVEN EIGHTHS": 7.0 / 8}
	shareWords = "(" + strings.Join(shareOrder, "|") + ")"
	centre     = pt{0.5, 0.5}
	corners    = map[string]pt{"UPPER LEFT": {0, 0}, "UPPER RIGHT": {1, 0}, "LOWER LEFT": {0, 1}, "LOWER RIGHT": {1, 1}}
	edgeLines  = map[string][2]pt{"UPPER": {{0, 0}, {1, 0}}, "RIGHT": {{1, 0}, {1, 1}}, "LOWER": {{1, 1}, {0, 1}}, "LEFT": {{0, 1}, {0, 0}}}
	square     = rect(0, 0, 1, 1)
)

// Anchored pattern matching the whole name like re.fullmatch
func full(pattern string) *regexp.Regexp { return regexp.MustCompile("^(?:" + pattern + ")$") }

var (
	reSide       = full(sideWords + " " + shareWords + " BLOCK")
	reSideSide   = full(sideWords + " " + shareWords + " " + sideWords + " " + shareWords + " BLOCK")
	reMiddle     = full("MIDDLE (LEFT|RIGHT) ONE QUARTER BLOCK")
	reQuadrant   = full("QUADRANT (.+)")
	reCellBlock  = full("BLOCK (SEXTANT|OCTANT)-([1-8]+)")
	reEighth     = full("(VERTICAL|HORIZONTAL) ONE EIGHTH BLOCK-([1-8])")
	reDiagonal   = full("((?:UPPER|LOWER) (?:LEFT|RIGHT)) BLOCK DIAGONAL (.+) TO (.+)")
	reTriangular = full("(.+) TRIANGULAR (ONE QUARTER|HALF|THREE QUARTERS) BLOCK")
	reTriangle   = full("BLACK ((?:UPPER|LOWER) (?:LEFT|RIGHT)) TRIANGLE")
	reStroke     = full("BOX DRAWINGS LIGHT DIAGONAL (.+) TO (.+)")
	reBox        = full("BOX DRAWINGS (LIGHT|HEAVY) (.+)")
)

// Axis aligned rectangle as a polygon
func rect(x0, y0, x1, y1 float64) poly { return poly{{x0, y0}, {x1, y0}, {x1, y1}, {x0, y1}} }

// Rectangle covering a share of the cell against one side
func side(word string, f float64) poly {
	return map[string]poly{"UPPER": rect(0, 0, 1, f), "LOWER": rect(0, 1-f, 1, 1), "LEFT": rect(0, 0, f, 1), "RIGHT": rect(1-f, 0, 1, 1)}[word]
}

// Overlap of two axis aligned rectangles
func overlap(a, b poly) poly {
	return rect(max(a[0].x, b[0].x), max(a[0].y, b[0].y), min(a[2].x, b[2].x), min(a[2].y, b[2].y))
}

// Signed parallelogram area from a to b and a to p, positive when p lies left of the line
func cross(a, b, p pt) float64 { return (b.x-a.x)*(p.y-a.y) - (b.y-a.y)*(p.x-a.x) }

// Polygon cut by the line through a and b, keeping the side where the reference point lies
func clip(in poly, a, b, ref pt) (poly, error) {
	s := cross(a, b, ref)
	if math.Abs(s) < epsilonCross {
		return nil, fmt.Errorf("reference point lies on the cutting line")
	}
	var out poly
	for i, p := range in {
		q := in[(i+1)%len(in)]
		fp, fq := cross(a, b, p)*s, cross(a, b, q)*s
		if fp >= 0 {
			out = append(out, p)
		}
		if (fp >= 0) != (fq >= 0) {
			t := fp / (fp - fq)
			out = append(out, pt{p.x + (q.x-p.x)*t, p.y + (q.y-p.y)*t})
		}
	}
	return out, nil
}

// Border point named by a vertical word then a horizontal word
func point(words string) (pt, error) {
	for _, v := range verticalOrder {
		if rest, ok := strings.CutPrefix(words, v+" "); ok {
			if h, known := horizontal[rest]; known {
				return pt{h, vertical[v]}, nil
			}
		}
	}
	return pt{}, fmt.Errorf("unknown border point %s", words)
}

// Band of a width around the line through two points, kept inside the cell
func stroke(a, b pt, width float64) (poly, error) {
	dx, dy := b.x-a.x, b.y-a.y
	n := math.Sqrt(dx*dx + dy*dy)
	ox, oy := -dy/n*width/2, dx/n*width/2
	band, err := clip(square, pt{a.x + ox, a.y + oy}, pt{b.x + ox, b.y + oy}, pt{a.x - ox, a.y - oy})
	if err != nil {
		return nil, err
	}
	return clip(band, pt{a.x - ox, a.y - oy}, pt{b.x - ox, b.y - oy}, pt{a.x + ox, a.y + oy})
}

// Arm of a box drawing line from the cell centre to one edge
func arm(word string, t float64) poly {
	lo, hi := 0.5-t/2, 0.5+t/2
	return map[string]poly{"UP": rect(lo, 0, hi, hi), "DOWN": rect(lo, lo, hi, 1), "LEFT": rect(0, lo, hi, hi), "RIGHT": rect(lo, lo, 1, hi)}[word]
}

// Filled polygons of a character in unit cell coordinates, parsed from its Unicode name
func shape(cp rune) ([]poly, error) {
	name := runenames.Name(cp)
	switch name {
	case "SPACE":
		return nil, nil
	case "FULL BLOCK":
		return []poly{square}, nil
	}
	if m := reSide.FindStringSubmatch(name); m != nil {
		return []poly{side(m[1], shares[m[2]])}, nil
	}
	if m := reSideSide.FindStringSubmatch(name); m != nil {
		return []poly{overlap(side(m[1], shares[m[2]]), side(m[3], shares[m[4]]))}, nil
	}
	if m := reMiddle.FindStringSubmatch(name); m != nil {
		return []poly{overlap(side(m[1], 0.5), rect(0, 0.25, 1, 0.75))}, nil
	}
	if m := reQuadrant.FindStringSubmatch(name); m != nil {
		var out []poly
		for _, q := range strings.Split(m[1], " AND ") {
			words := strings.Fields(q)
			out = append(out, overlap(side(words[0], 0.5), side(words[1], 0.5)))
		}
		return out, nil
	}
	if m := reCellBlock.FindStringSubmatch(name); m != nil {
		down := 4.0
		if m[1] == "SEXTANT" {
			down = 3
		}
		var out []poly
		for _, c := range m[2] {
			d := int(c - '0')
			out = append(out, rect(float64((d-1)%2)/2, float64((d-1)/2)/down, float64((d-1)%2+1)/2, float64((d-1)/2+1)/down))
		}
		return out, nil
	}
	if m := reEighth.FindStringSubmatch(name); m != nil {
		k := float64(m[2][0] - '0')
		if m[1] == "VERTICAL" {
			return []poly{rect((k-1)/8, 0, k/8, 1)}, nil
		}
		return []poly{rect(0, (k-1)/8, 1, k/8)}, nil
	}
	if m := reDiagonal.FindStringSubmatch(name); m != nil {
		a, err := point(m[2])
		if err != nil {
			return nil, err
		}
		b, err := point(m[3])
		if err != nil {
			return nil, err
		}
		cut, err := clip(square, a, b, corners[m[1]])
		return []poly{cut}, err
	}
	if m := reTriangular.FindStringSubmatch(name); m != nil {
		edges := strings.Split(m[1], " AND ")
		if float64(len(edges))/4 != shares[m[2]] {
			return nil, fmt.Errorf("triangle count disagrees with the share in %s", name)
		}
		var out []poly
		for _, e := range edges {
			out = append(out, poly{edgeLines[e][0], edgeLines[e][1], centre})
		}
		return out, nil
	}
	if m := reTriangle.FindStringSubmatch(name); m != nil {
		c := corners[m[1]]
		return []poly{{c, {1 - c.x, c.y}, {c.x, 1 - c.y}}}, nil
	}
	if m := reStroke.FindStringSubmatch(name); m != nil {
		band, err := stroke(corners[m[1]], corners[m[2]], diagonal)
		return []poly{band}, err
	}
	if name == "BOX DRAWINGS LIGHT DIAGONAL CROSS" {
		a, err := stroke(corners["UPPER LEFT"], corners["LOWER RIGHT"], diagonal)
		if err != nil {
			return nil, err
		}
		b, err := stroke(corners["UPPER RIGHT"], corners["LOWER LEFT"], diagonal)
		return []poly{a, b}, err
	}
	if m := reBox.FindStringSubmatch(name); m != nil {
		t := heavy
		if m[1] == "LIGHT" {
			t = light
		}
		var out []poly
		for _, w := range strings.Split(m[2], " AND ") {
			switch w {
			case "VERTICAL":
				out = append(out, arm("UP", t), arm("DOWN", t))
			case "HORIZONTAL":
				out = append(out, arm("LEFT", t), arm("RIGHT", t))
			default:
				out = append(out, arm(w, t))
			}
		}
		return out, nil
	}
	return nil, fmt.Errorf("no geometry for U+%04X %s", cp, name)
}

// One directed edge of a convex polygon, positive on the inside
type edge struct {
	p, q pt
	s    float64
}

// Edge equations of a convex polygon, nil for a degenerate one
func edgesOf(in poly) []edge {
	area := 0.0
	for i, p := range in {
		q := in[(i+1)%len(in)]
		area += p.x*q.y - q.x*p.y
	}
	if math.Abs(area) < epsilonCross {
		return nil
	}
	s := -1.0
	if area > 0 {
		s = 1
	}
	var out []edge
	for i, p := range in {
		if q := in[(i+1)%len(in)]; p != q {
			out = append(out, edge{p, q, s})
		}
	}
	return out
}

// Subsamples of each pixel inside any polygon, out of sub by sub
func raster(polys []poly) ([cols * rows]int, error) {
	var shapes [][]edge
	for _, p := range polys {
		if e := edgesOf(p); e != nil {
			shapes = append(shapes, e)
		}
	}
	var counts [cols * rows]int
	for py := range rows {
		for px := range cols {
			n := 0
			for sy := range sub {
				y := (float64(py) + (float64(sy)+0.5)/sub) / rows
				for sx := range sub {
					x := (float64(px) + (float64(sx)+0.5)/sub) / cols
					for _, es := range shapes {
						inside := true
						for _, e := range es {
							c := cross(e.p, e.q, pt{x, y}) * e.s
							if math.Abs(c) < epsilonCross {
								return counts, fmt.Errorf("subsample lies on a polygon edge")
							}
							if c < 0 {
								inside = false
								break
							}
						}
						if inside {
							n++
							break
						}
					}
				}
			}
			counts[py*cols+px] = n
		}
	}
	return counts, nil
}

// Code points from lo up to but excluding hi, stepping by one in either direction
func span(lo, hi rune) []rune {
	var out []rune
	if lo <= hi {
		for c := lo; c < hi; c++ {
			out = append(out, c)
		}
		return out
	}
	for c := lo; c > hi; c-- {
		out = append(out, c)
	}
	return out
}

// Characters whose geometry is a union of block cells, any of which may fill a grid library
func blocks() []rune {
	out := []rune{0x20}
	for _, c := range span(0x2580, 0x25A0) {
		if !strings.Contains(runenames.Name(c), "SHADE") {
			out = append(out, c)
		}
	}
	out = append(out, span(0x1FB00, 0x1FB3C)...)
	out = append(out, span(0x1CD00, 0x1CDE6)...)
	out = append(out, span(0x1CEA0, 0x1CEB0)...)
	out = append(out, span(0x1FB82, 0x1FB8C)...)
	return append(out, 0x1FBE6, 0x1FBE7)
}

// Memoized raster of each character's named geometry
type rasterCache map[rune][cols * rows]int

func (c rasterCache) of(cp rune) ([cols * rows]int, error) {
	if r, ok := c[cp]; ok {
		return r, nil
	}
	polys, err := shape(cp)
	if err != nil {
		return [cols * rows]int{}, err
	}
	r, err := raster(polys)
	if err != nil {
		return r, err
	}
	c[cp] = r
	return r, nil
}

// Glyph for every mask of a grid of block cells, matched by raster against every block character
func grid(across, down int, cache rasterCache) ([]rune, error) {
	by := map[[cols * rows]int]rune{}
	for _, cp := range blocks() {
		r, err := cache.of(cp)
		if err != nil {
			return nil, err
		}
		if prev, ok := by[r]; ok {
			return nil, fmt.Errorf("U+%04X and U+%04X rasterize alike", prev, cp)
		}
		by[r] = cp
	}
	var out []rune
	for mask := range 1 << (across * down) {
		var cells []poly
		for i := range across * down {
			if mask>>i&1 == 1 {
				cells = append(cells, rect(float64(i%across)/float64(across), float64(i/across)/float64(down),
					float64(i%across+1)/float64(across), float64(i/across+1)/float64(down)))
			}
		}
		r, err := raster(cells)
		if err != nil {
			return nil, err
		}
		cp, ok := by[r]
		if !ok {
			return nil, fmt.Errorf("no character fills mask %d of the %d by %d grid", mask, across, down)
		}
		out = append(out, cp)
	}
	return out, nil
}

// Text of the Go glyph table, every library rasterized
func generate() (string, error) {
	cache := rasterCache{}
	var libs [4][]rune
	for i, size := range [][2]int{{1, 2}, {2, 2}, {2, 3}, {2, 4}} {
		lib, err := grid(size[0], size[1], cache)
		if err != nil {
			return "", err
		}
		libs[i] = lib
	}
	half, quadrant, sextant, octant := libs[0], libs[1], libs[2], libs[3]
	shapes := slices.Concat([]rune{0x20, 0x2588}, quadrant, span(0x258F, 0x2588), span(0x2581, 0x2588), []rune{0x2594, 0x2595},
		span(0x25E2, 0x25E6), span(0x2571, 0x2574),
		[]rune{0x2500, 0x2502, 0x250C, 0x2510, 0x2514, 0x2518, 0x251C, 0x2524, 0x252C, 0x2534, 0x253C, 0x2501, 0x2503})
	legacy := slices.Concat(shapes, sextant, span(0x1FB3C, 0x1FB7C), span(0x1FB82, 0x1FB8C), []rune{0x1FB9A, 0x1FB9B, 0x1FBE6, 0x1FBE7})
	every := slices.Concat(legacy, octant)
	all := [][]rune{half, quadrant, sextant, octant, shapes, legacy, every}
	var order []rune
	for _, cp := range every {
		if !slices.Contains(order, cp) {
			order = append(order, cp)
		}
	}
	lines := []string{
		"// Code generated by scripts/glyphs.py. DO NOT EDIT.",
		"",
		"package vxl",
		"",
		"// Every glyph with the libraries holding it, its lit pixel mask and the coverage of each pixel",
		"var glyphs = [...]glyph{",
	}
	for _, cp := range order {
		r, err := cache.of(cp)
		if err != nil {
			return "", err
		}
		bits := 0
		for m, lib := range all {
			if slices.Contains(lib, cp) {
				bits |= 1 << m
			}
		}
		mask := uint64(0)
		cover := make([]string, len(r))
		for i, n := range r {
			if n*2 >= sub*sub {
				mask |= 1 << i
			}
			cover[i] = strconv.Itoa((n*255 + sub*sub/2) / (sub * sub))
		}
		lines = append(lines, fmt.Sprintf("\t{\"%s\", 0x%02x, 0x%08x, cover{%s}},", string(cp), bits, mask, strings.Join(cover, ", ")))
	}
	lines = append(lines, "}")
	return strings.Join(lines, "\n") + "\n", nil
}

// Writes glyphs.go in the directory go generate runs from
func main() {
	text, err := generate()
	if err == nil {
		err = os.WriteFile("glyphs.go", []byte(text), 0o644)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "glyphgen:", err)
		os.Exit(1)
	}
}
