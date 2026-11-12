package main

import (
	"fmt"
	"image"
	"image/color"
	"math"
	"strings"

	"golang.org/x/image/draw"
	"golang.org/x/image/math/f64"
	"golang.org/x/image/vector"
)

const mascotFrames = 8

// Crops of the tub and banner art behind each billing tier feature
var tierCrops = map[string]struct {
	banner bool
	rect   image.Rectangle
}{
	"automation": {true, image.Rect(40, 388, 420, 768)},
	"connect":    {true, image.Rect(1330, 40, 1880, 590)},
	"memory":     {false, image.Rect(390, 400, 870, 880)},
}

// One frame of the mascot loop, the logo bobbing, tilting, squashing and blinking its red accents
func (s *sources) mascotFrame(w, h, index int) *image.NRGBA {
	theta := 2 * math.Pi * float64(index) / mascotFrames
	lift := (1 - math.Cos(theta)) / 2
	tilt := 5 * math.Pi / 180 * math.Sin(theta)
	sy := 1 - 0.06*math.Cos(theta)
	sx := 1 / sy
	mark := s.markWhite
	if index%4 == 2 {
		mark = s.blinkMark()
	}
	mw, mh := float64(mark.Bounds().Dx()-2*markMargin), float64(mark.Bounds().Dy()-2*markMargin)
	k := math.Min(0.78*float64(w)/mw, 0.6*float64(h)/mh)
	base := 0.8*float64(h) - 0.05*float64(h)*lift
	dst := image.NewRGBA(image.Rect(0, 0, w, h))
	shadow := vector.NewRasterizer(w, h)
	addEllipse(shadow, float64(w)/2, 0.83*float64(h), 0.32*float64(w)*(1-0.3*lift), 0.025*float64(h))
	m := image.NewAlpha(dst.Bounds())
	shadow.Draw(m, m.Bounds(), image.Opaque, image.Point{})
	ink := s.tubDark
	ink.A = uint8(math.Round(255 * 0.35 * (1 - 0.4*lift)))
	draw.DrawMask(dst, dst.Bounds(), image.NewUniform(ink), image.Point{}, m, image.Point{}, draw.Over)
	px, py := float64(markMargin)+mw/2, float64(markMargin)+mh
	cos, sin := math.Cos(tilt), math.Sin(tilt)
	a, b, c, d := cos*sx*k, -sin*sy*k, sin*sx*k, cos*sy*k
	tx, ty := float64(w)/2-(a*px+b*py), base-(c*px+d*py)
	draw.CatmullRom.Transform(dst, f64.Aff3{a, b, tx, c, d, ty}, mark, mark.Bounds(), draw.Over, nil)
	return toNRGBA(dst)
}

// Logo with its red accents faded to a quarter, the closed eye of the blink frames
func (s *sources) blinkMark() *image.NRGBA {
	out := image.NewNRGBA(s.markWhite.Bounds())
	copy(out.Pix, s.markWhite.Pix)
	for i, acc := range s.accent.Pix {
		if acc > 0 {
			out.Pix[i*4+3] = uint8(math.Round(float64(out.Pix[i*4+3]) - 0.75*float64(acc)))
		}
	}
	return out
}

// Sheet of every mascot frame on a four by two grid of cells
func (s *sources) mascotSheet(w, h int) *image.NRGBA {
	cw, ch := w/4, h/2
	sheet := image.NewNRGBA(image.Rect(0, 0, w, h))
	for i := 0; i < mascotFrames; i++ {
		at := image.Pt(i%4*cw, i/4*ch)
		draw.Draw(sheet, image.Rectangle{at, at.Add(image.Pt(cw, ch))}, s.mascotFrame(cw, ch, i), image.Point{}, draw.Src)
	}
	return sheet
}

func addEllipse(z *vector.Rasterizer, cx, cy, rx, ry float64) {
	k := 0.5522847498
	p := func(x, y float64) (float32, float32) { return float32(x), float32(y) }
	z.MoveTo(p(cx+rx, cy))
	x1, y1 := p(cx+rx, cy+ry*k)
	x2, y2 := p(cx+rx*k, cy+ry)
	x3, y3 := p(cx, cy+ry)
	z.CubeTo(x1, y1, x2, y2, x3, y3)
	x1, y1 = p(cx-rx*k, cy+ry)
	x2, y2 = p(cx-rx, cy+ry*k)
	x3, y3 = p(cx-rx, cy)
	z.CubeTo(x1, y1, x2, y2, x3, y3)
	x1, y1 = p(cx-rx, cy-ry*k)
	x2, y2 = p(cx-rx*k, cy-ry)
	x3, y3 = p(cx, cy-ry)
	z.CubeTo(x1, y1, x2, y2, x3, y3)
	x1, y1 = p(cx+rx*k, cy-ry)
	x2, y2 = p(cx+rx, cy-ry*k)
	x3, y3 = p(cx+rx, cy)
	z.CubeTo(x1, y1, x2, y2, x3, y3)
	z.ClosePath()
}

// Opaque square feature art for one billing tier, cropped from the banner or the tub
func (s *sources) tier(name string, size int) *image.NRGBA {
	if name == "sandbox" {
		return cover(s.tub, s.tubTile, size, size, s.tubDark)
	}
	c := tierCrops[name]
	src := s.tub
	if c.banner {
		src = s.banner
	}
	return cover(src, c.rect, size, size, s.tubDark)
}

// Tub artwork on the macOS grid plate at size, the disk image volume and file icon for every channel
func (s *sources) volumeIcon(size int) *image.NRGBA {
	k := float64(size) / canvasSide
	dst := image.NewRGBA(image.Rect(0, 0, size, size))
	plate := cover(s.tub, s.tubTile, int(math.Round(macPlate.w*k)), int(math.Round(macPlate.h*k)), s.tubDark)
	sil := roundedMask(size, size, macPlate, k)
	at := image.Pt(-int(math.Round(macPlate.x*k)), -int(math.Round(macPlate.y*k)))
	draw.DrawMask(dst, dst.Bounds(), plate, at, sil, image.Point{}, draw.Over)
	return toNRGBA(dst)
}

// Disk image window background, banner scene dimmed under the wordmark, the electron one with an arrow between its icons
func (s *sources) dmgBackground(w, h int, arrowScale float64) *image.NRGBA {
	scene := cover(s.banner, s.banner.Bounds(), w, h, s.tubDark)
	dst := image.NewRGBA(scene.Bounds())
	draw.Draw(dst, dst.Bounds(), scene, image.Point{}, draw.Src)
	dim := s.tubDark
	dim.A = 140
	draw.Draw(dst, dst.Bounds(), image.NewUniform(dim), image.Point{}, draw.Over)
	box := s.textBox
	k := 0.34 * float64(w) / float64(box.Dx())
	drawScaled(dst, s.textWhite.SubImage(box), k, (float64(w)-float64(box.Dx())*k)/2-float64(box.Min.X)*k, 0.07*float64(h)-float64(box.Min.Y)*k)
	if arrowScale == 0 {
		return toNRGBA(dst)
	}
	f := arrowScale
	cx, cy, half := (253.0+512.0)/2*f, (238.0+235.0)/2*f, 34*f
	z := vector.NewRasterizer(w, h)
	shaft, head := 2*f, 9*f
	z.MoveTo(float32(cx-half), float32(cy-shaft))
	z.LineTo(float32(cx+half-head), float32(cy-shaft))
	z.LineTo(float32(cx+half-head), float32(cy-head))
	z.LineTo(float32(cx+half), float32(cy))
	z.LineTo(float32(cx+half-head), float32(cy+head))
	z.LineTo(float32(cx+half-head), float32(cy+shaft))
	z.LineTo(float32(cx-half), float32(cy+shaft))
	z.ClosePath()
	arrow := image.NewAlpha(dst.Bounds())
	z.Draw(arrow, arrow.Bounds(), image.Opaque, image.Point{})
	draw.DrawMask(dst, dst.Bounds(), image.NewUniform(color.NRGBA{255, 255, 255, 230}), image.Point{}, arrow, image.Point{}, draw.Over)
	return toNRGBA(dst)
}

// Luminance of the tub tile resampled to n by n and softened, the field the catalog art is traced from
func (s *sources) tubField(n int) []float64 {
	small := resize(toNRGBA(s.tub.SubImage(s.tubTile)), n, n)
	raw := make([]float64, n*n)
	for i := range raw {
		p := small.Pix[i*4:]
		raw[i] = (0.2126*float64(p[0]) + 0.7152*float64(p[1]) + 0.0722*float64(p[2])) / 255
	}
	field := make([]float64, n*n)
	for y := 0; y < n; y++ {
		for x := 0; x < n; x++ {
			sum, cnt := 0.0, 0.0
			for dy := -1; dy <= 1; dy++ {
				for dx := -1; dx <= 1; dx++ {
					if xx, yy := x+dx, y+dy; xx >= 0 && yy >= 0 && xx < n && yy < n {
						sum, cnt = sum+raw[yy*n+xx], cnt+1
					}
				}
			}
			field[y*n+x] = sum / cnt
		}
	}
	return field
}

// Marching squares segments per case, corner bits tl 8 tr 4 br 2 bl 1, edges top right bottom left
var marchCases = [16][][2]byte{
	1: {{'L', 'B'}}, 2: {{'B', 'R'}}, 3: {{'L', 'R'}}, 4: {{'T', 'R'}}, 6: {{'T', 'B'}},
	7: {{'L', 'T'}}, 8: {{'L', 'T'}}, 9: {{'T', 'B'}}, 11: {{'T', 'R'}}, 12: {{'L', 'R'}},
	13: {{'B', 'R'}}, 14: {{'L', 'B'}},
}

// Closed outlines of the region where the field reaches iso, edge crossings linearly interpolated
func traceContours(field []float64, n int, iso float64) [][][2]float64 {
	at := func(x, y int) float64 {
		if x < 0 || y < 0 || x >= n || y >= n {
			return 0
		}
		return field[y*n+x]
	}
	type seg struct{ a, b int }
	points := map[int][2]float64{}
	edge := func(x, y int, side byte) int {
		var key int
		var p [2]float64
		switch side {
		case 'T', 'B':
			if side == 'B' {
				y++
			}
			v0, v1 := at(x, y), at(x+1, y)
			key = ((y+1)*(n+2)+(x+1))*2 + 0
			p = [2]float64{float64(x) + (iso-v0)/(v1-v0) + 0.5, float64(y) + 0.5}
		default:
			if side == 'R' {
				x++
			}
			v0, v1 := at(x, y), at(x, y+1)
			key = ((y+1)*(n+2)+(x+1))*2 + 1
			p = [2]float64{float64(x) + 0.5, float64(y) + (iso-v0)/(v1-v0) + 0.5}
		}
		points[key] = p
		return key
	}
	var segs []seg
	for y := -1; y < n; y++ {
		for x := -1; x < n; x++ {
			tl, tr, br, bl := at(x, y) >= iso, at(x+1, y) >= iso, at(x+1, y+1) >= iso, at(x, y+1) >= iso
			c := 0
			for i, in := range []bool{tl, tr, br, bl} {
				if in {
					c |= 8 >> i
				}
			}
			pairs := marchCases[c]
			if c == 5 || c == 10 {
				centre := (at(x, y)+at(x+1, y)+at(x+1, y+1)+at(x, y+1))/4 >= iso
				switch {
				case c == 5 && centre, c == 10 && !centre:
					pairs = [][2]byte{{'L', 'T'}, {'B', 'R'}}
				default:
					pairs = [][2]byte{{'L', 'B'}, {'T', 'R'}}
				}
			}
			for _, p := range pairs {
				segs = append(segs, seg{edge(x, y, p[0]), edge(x, y, p[1])})
			}
		}
	}
	byPoint := map[int][]int{}
	for i, s := range segs {
		byPoint[s.a] = append(byPoint[s.a], i)
		byPoint[s.b] = append(byPoint[s.b], i)
	}
	used := make([]bool, len(segs))
	var loops [][][2]float64
	for start := range segs {
		if used[start] {
			continue
		}
		used[start] = true
		first, cur := segs[start].a, segs[start].b
		loop := [][2]float64{points[first]}
		for cur != first {
			loop = append(loop, points[cur])
			next := -1
			for _, j := range byPoint[cur] {
				if !used[j] {
					next = j
					break
				}
			}
			if next < 0 {
				break
			}
			used[next] = true
			if segs[next].a == cur {
				cur = segs[next].b
			} else {
				cur = segs[next].a
			}
		}
		if len(loop) >= 3 {
			loops = append(loops, loop)
		}
	}
	return loops
}

// Ramer Douglas Peucker reduction of a closed outline to the points that matter at eps
func simplify(loop [][2]float64, eps float64) [][2]float64 {
	if len(loop) < 4 {
		return loop
	}
	keep := make([]bool, len(loop))
	keep[0], keep[len(loop)/2] = true, true
	var rdp func(i, j int)
	rdp = func(i, j int) {
		if j <= i+1 {
			return
		}
		a, b := loop[i], loop[j%len(loop)]
		dx, dy := b[0]-a[0], b[1]-a[1]
		norm := math.Hypot(dx, dy)
		far, at := 0.0, -1
		for k := i + 1; k < j; k++ {
			p := loop[k]
			var d float64
			if norm == 0 {
				d = math.Hypot(p[0]-a[0], p[1]-a[1])
			} else {
				d = math.Abs(dy*p[0]-dx*p[1]+b[0]*a[1]-b[1]*a[0]) / norm
			}
			if d > far {
				far, at = d, k
			}
		}
		if far > eps {
			keep[at] = true
			rdp(i, at)
			rdp(at, j)
		}
	}
	rdp(0, len(loop)/2)
	rdp(len(loop)/2, len(loop))
	var out [][2]float64
	for i, k := range keep {
		if k {
			out = append(out, loop[i])
		}
	}
	return out
}

// Even odd SVG path data of every outline at iso, scaled into a viewBox of side units
func contourPath(field []float64, n int, iso, side float64) string {
	var b strings.Builder
	k := side / float64(n)
	for _, loop := range traceContours(field, n, iso) {
		pts := simplify(loop, 0.35)
		if len(pts) < 3 {
			continue
		}
		for i, p := range pts {
			cmd := "L"
			if i == 0 {
				cmd = "M"
			}
			fmt.Fprintf(&b, "%s%s %s", cmd, num(p[0]*k), num(p[1]*k))
		}
		b.WriteString("Z")
	}
	return b.String()
}

func num(v float64) string {
	s := fmt.Sprintf("%.2f", v)
	s = strings.TrimRight(strings.TrimRight(s, "0"), ".")
	if s == "-0" || s == "" {
		return "0"
	}
	return s
}

// Two level stencil of the tub traced from its luminance, mid tones under highlights
func (s *sources) catalogSVG(mid, high color.NRGBA) string {
	const n, side = 240, 100.0
	field := s.tubField(n)
	return fmt.Sprintf(`<svg xmlns="http://www.w3.org/2000/svg" fill="none" viewBox="0 0 %d %d">`+
		`<path fill="%s" fill-rule="evenodd" d="%s"/><path fill="%s" fill-rule="evenodd" d="%s"/></svg>`+"\n",
		int(side), int(side), hexOf(mid), contourPath(field, n, 0.1, side), hexOf(high), contourPath(field, n, 0.5, side))
}
