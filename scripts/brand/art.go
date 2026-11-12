package main

import (
	"fmt"
	"image"
	"image/color"
	"image/png"
	"math"
	"os"
	"path/filepath"
	"strconv"

	"golang.org/x/image/draw"
	"golang.org/x/image/math/f64"
	"golang.org/x/image/vector"
)

const (
	logoFile        = "ICE_BATH_SQUARE_LOGO.png"
	tubFile         = "ICE_BATH_SQUARE_BG.png"
	bannerFile      = "ICE_BATH_BANNER_ART.png"
	textWhiteFile   = "ICE_BATH_BANNER_TEXT_WHITE.png"
	textBlackFile   = "ICE_BATH_BANNER_TEXT_BLACK.png"
	canvasSide      = 1024.0
	markMargin      = 6
	badgeMinSize    = 32
	safeZoneRadius  = 0.4
	monoInkTone     = 0.13
	monoInkOpacity  = 0.90
	inkAlphaCutoff  = 128
	inkLineMinCount = 3
)

// Decoded source art with the measurements every renderer shares
type sources struct {
	logo      *image.NRGBA
	markWhite *image.NRGBA
	markBlack *image.NRGBA
	accent    *image.Alpha
	tub       *image.NRGBA
	tubTile   image.Rectangle
	banner    *image.NRGBA
	textWhite *image.NRGBA
	textBox   image.Rectangle
	letter    color.NRGBA
	red       color.NRGBA
	tubDark   color.NRGBA
}

// One build identity, the plate fills and the commit printed on desktop tiles
type art struct {
	src    *sources
	light  color.NRGBA
	dark   color.NRGBA
	commit string
}

func newArt(src *sources, plates [2]string, commit string) *art {
	return &art{src: src, light: mustHex(plates[0]), dark: mustHex(plates[1]), commit: commit}
}

func mustHex(s string) color.NRGBA {
	v, err := strconv.ParseUint(s[1:], 16, 32)
	if err != nil || len(s) != 7 {
		panic("bad colour " + s)
	}
	return color.NRGBA{uint8(v >> 16), uint8(v >> 8), uint8(v), 255}
}

func hexOf(c color.NRGBA) string { return fmt.Sprintf("#%02x%02x%02x", c.R, c.G, c.B) }

func readPNG(path string) (*image.NRGBA, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	img, err := png.Decode(f)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", filepath.Base(path), err)
	}
	out := image.NewNRGBA(image.Rect(0, 0, img.Bounds().Dx(), img.Bounds().Dy()))
	draw.Draw(out, out.Bounds(), img, img.Bounds().Min, draw.Src)
	return out, nil
}

// Decodes the six source images and derives the marks, palettes and the full bleed tub
func loadSources(root string) (*sources, error) {
	dir := filepath.Join(root, "assets")
	imgs := map[string]*image.NRGBA{}
	for _, name := range []string{logoFile, tubFile, bannerFile, textWhiteFile, textBlackFile} {
		img, err := readPNG(filepath.Join(dir, name))
		if err != nil {
			return nil, err
		}
		imgs[name] = img
	}
	s := &sources{logo: imgs[logoFile], banner: imgs[bannerFile], textWhite: imgs[textWhiteFile]}
	logoBox := inkBox(s.logo)
	textBlack := imgs[textBlackFile]
	whiteBox, blackBox := inkBox(s.textWhite), inkBox(textBlack)
	if logoBox.Empty() || whiteBox.Empty() || blackBox.Empty() {
		return nil, fmt.Errorf("source art without ink: logo %v, white text %v, black text %v", logoBox, whiteBox, blackBox)
	}
	s.textBox = whiteBox
	var err error
	if s.letter, s.red, err = inkPalette(s.textWhite); err != nil {
		return nil, fmt.Errorf("%s: %w", textWhiteFile, err)
	}
	inkBlack, blueBlack, err := inkPalette(textBlack)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", textBlackFile, err)
	}
	logoLetter, logoRed, err := inkPalette(s.logo)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", logoFile, err)
	}
	s.markWhite, s.markBlack, s.accent = splitMark(s.logo, logoBox, logoLetter, logoRed, inkBlack, blueBlack)
	if s.tub, s.tubTile, s.tubDark, err = fullBleedTub(imgs[tubFile]); err != nil {
		return nil, fmt.Errorf("%s: %w", tubFile, err)
	}
	return s, nil
}

// Bounding box of rows and columns holding at least three mostly opaque pixels, ignoring stray specks
func inkBox(img *image.NRGBA) image.Rectangle {
	b := img.Bounds()
	rows, cols := make([]int, b.Dy()), make([]int, b.Dx())
	for y := 0; y < b.Dy(); y++ {
		for x := 0; x < b.Dx(); x++ {
			if img.Pix[y*img.Stride+x*4+3] >= inkAlphaCutoff {
				rows[y]++
				cols[x]++
			}
		}
	}
	first := func(v []int) int {
		for i, n := range v {
			if n >= inkLineMinCount {
				return i
			}
		}
		return -1
	}
	last := func(v []int) int {
		for i := len(v) - 1; i >= 0; i-- {
			if v[i] >= inkLineMinCount {
				return i
			}
		}
		return -1
	}
	x0, x1, y0, y1 := first(cols), last(cols), first(rows), last(rows)
	if x0 < 0 || y0 < 0 {
		return image.Rectangle{}
	}
	return image.Rect(x0, y0, x1+1, y1+1)
}

func dist(a, b color.NRGBA) float64 {
	dr, dg, db := float64(a.R)-float64(b.R), float64(a.G)-float64(b.G), float64(a.B)-float64(b.B)
	return math.Sqrt(dr*dr + dg*dg + db*db)
}

// Letter colour from the dominant opaque bucket and accent colour from the opaque pixels far from it
func inkPalette(img *image.NRGBA) (color.NRGBA, color.NRGBA, error) {
	counts := map[[3]uint8]int{}
	var best [3]uint8
	for i := 0; i+3 < len(img.Pix); i += 4 {
		if img.Pix[i+3] != 255 {
			continue
		}
		k := [3]uint8{img.Pix[i] >> 3, img.Pix[i+1] >> 3, img.Pix[i+2] >> 3}
		counts[k]++
		if counts[k] > counts[best] || counts[k] == counts[best] && less(k, best) {
			best = k
		}
	}
	if len(counts) == 0 {
		return color.NRGBA{}, color.NRGBA{}, fmt.Errorf("no opaque ink")
	}
	seed := color.NRGBA{best[0]<<3 | 4, best[1]<<3 | 4, best[2]<<3 | 4, 255}
	var letter, accent [4]float64
	for i := 0; i+3 < len(img.Pix); i += 4 {
		if img.Pix[i+3] != 255 {
			continue
		}
		c := color.NRGBA{img.Pix[i], img.Pix[i+1], img.Pix[i+2], 255}
		switch d := dist(c, seed); {
		case d <= 60:
			letter = [4]float64{letter[0] + float64(c.R), letter[1] + float64(c.G), letter[2] + float64(c.B), letter[3] + 1}
		case d > 120:
			accent = [4]float64{accent[0] + float64(c.R), accent[1] + float64(c.G), accent[2] + float64(c.B), accent[3] + 1}
		}
	}
	if accent[3] == 0 {
		return color.NRGBA{}, color.NRGBA{}, fmt.Errorf("no accent colour apart from the letters")
	}
	mean := func(v [4]float64) color.NRGBA {
		return color.NRGBA{uint8(math.Round(v[0] / v[3])), uint8(math.Round(v[1] / v[3])), uint8(math.Round(v[2] / v[3])), 255}
	}
	return mean(letter), mean(accent), nil
}

func less(a, b [3]uint8) bool {
	for i := range a {
		if a[i] != b[i] {
			return a[i] < b[i]
		}
	}
	return false
}

// Crops the logo as the white mark, recolours it onto the black text palette and records accent weight
func splitMark(logo *image.NRGBA, box image.Rectangle, letter, red, ink, blue color.NRGBA) (*image.NRGBA, *image.NRGBA, *image.Alpha) {
	r := box.Inset(-markMargin).Intersect(logo.Bounds())
	w, h := box.Dx()+2*markMargin, box.Dy()+2*markMargin
	white, black := image.NewNRGBA(image.Rect(0, 0, w, h)), image.NewNRGBA(image.Rect(0, 0, w, h))
	accent := image.NewAlpha(image.Rect(0, 0, w, h))
	from := [3]float64{float64(letter.R), float64(letter.G), float64(letter.B)}
	to := [3]float64{float64(red.R), float64(red.G), float64(red.B)}
	for y := r.Min.Y; y < r.Max.Y; y++ {
		for x := r.Min.X; x < r.Max.X; x++ {
			c := logo.NRGBAAt(x, y)
			if c.A == 0 {
				continue
			}
			dx, dy := x-box.Min.X+markMargin, y-box.Min.Y+markMargin
			t := accentT(c.R, c.G, c.B, from, to)
			white.SetNRGBA(dx, dy, c)
			black.SetNRGBA(dx, dy, color.NRGBA{lerp8(ink.R, blue.R, t), lerp8(ink.G, blue.G, t), lerp8(ink.B, blue.B, t), c.A})
			accent.SetAlpha(dx, dy, color.Alpha{uint8(math.Round(float64(c.A) * t))})
		}
	}
	return white, black, accent
}

func lerp8(a, b uint8, t float64) uint8 {
	return uint8(math.Round(float64(a) + (float64(b)-float64(a))*t))
}

// Replaces the white surround of the tub tile with the tile's own dark so the art bleeds to every edge
func fullBleedTub(src *image.NRGBA) (*image.NRGBA, image.Rectangle, color.NRGBA, error) {
	b := src.Bounds()
	w, h := b.Dx(), b.Dy()
	outside := make([]bool, w*h)
	light := func(i int) bool {
		p := src.Pix[i*4 : i*4+3]
		return min(p[0], p[1], p[2]) >= 200
	}
	var stack []int
	for _, i := range []int{0, w - 1, (h - 1) * w, h*w - 1} {
		if light(i) {
			outside[i] = true
			stack = append(stack, i)
		}
	}
	if len(stack) == 0 {
		return nil, image.Rectangle{}, color.NRGBA{}, fmt.Errorf("tile corners are not light, cannot find the tile outline")
	}
	for len(stack) > 0 {
		i := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		x, y := i%w, i/w
		for _, n := range [4][2]int{{x - 1, y}, {x + 1, y}, {x, y - 1}, {x, y + 1}} {
			if n[0] < 0 || n[1] < 0 || n[0] >= w || n[1] >= h {
				continue
			}
			j := n[1]*w + n[0]
			if !outside[j] && light(j) {
				outside[j] = true
				stack = append(stack, j)
			}
		}
	}
	grown := make([]bool, w*h)
	tile := image.Rectangle{}
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			hit := false
			for dy := -3; dy <= 3 && !hit; dy++ {
				for dx := -3; dx <= 3 && !hit; dx++ {
					xx, yy := x+dx, y+dy
					hit = xx >= 0 && yy >= 0 && xx < w && yy < h && outside[yy*w+xx]
				}
			}
			grown[y*w+x] = hit
			if !outside[y*w+x] {
				tile = tile.Union(image.Rect(x, y, x+1, y+1))
			}
		}
	}
	var sum [4]float64
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			if grown[y*w+x] || !grownWithin(grown, w, h, x, y, 10) {
				continue
			}
			p := src.Pix[(y*w+x)*4:]
			sum = [4]float64{sum[0] + float64(p[0]), sum[1] + float64(p[1]), sum[2] + float64(p[2]), sum[3] + 1}
		}
	}
	if sum[3] == 0 {
		return nil, image.Rectangle{}, color.NRGBA{}, fmt.Errorf("no tile edge to sample")
	}
	dark := color.NRGBA{uint8(math.Round(sum[0] / sum[3])), uint8(math.Round(sum[1] / sum[3])), uint8(math.Round(sum[2] / sum[3])), 255}
	out := image.NewNRGBA(b)
	copy(out.Pix, src.Pix)
	for i, g := range grown {
		if g {
			out.Pix[i*4], out.Pix[i*4+1], out.Pix[i*4+2], out.Pix[i*4+3] = dark.R, dark.G, dark.B, 255
		}
	}
	return out, tile, dark, nil
}

// Reports whether any grown pixel lies within r of (x, y) along the row or column
func grownWithin(grown []bool, w, h, x, y, r int) bool {
	for d := 1; d <= r; d++ {
		for _, p := range [4][2]int{{x - d, y}, {x + d, y}, {x, y - d}, {x, y + d}} {
			if p[0] >= 0 && p[1] >= 0 && p[0] < w && p[1] < h && grown[p[1]*w+p[0]] {
				return true
			}
		}
	}
	return false
}

// Rounded rectangle in canvas units, circular corners like the platform tile backgrounds
type rrect struct{ x, y, w, h, rx float64 }

var (
	squirclePlate = rrect{0, 0, 1024, 1024, 245}
	macPlate      = rrect{100, 100, 824, 824, 185.4}
	canvasPlate   = rrect{0, 0, 1024, 1024, 0}
)

// Box the mark fills, centred on the plate with its half diagonal inside the maskable safe circle
func markBox(plate rrect, mark *image.NRGBA) (x, y, w, h float64) {
	mw, mh := float64(mark.Bounds().Dx()-2*markMargin), float64(mark.Bounds().Dy()-2*markMargin)
	k := safeZoneRadius * plate.w / math.Hypot(mw/2, mh/2)
	w, h = mw*k, mh*k
	return plate.x + (plate.w-w)/2, plate.y + (plate.h-h)/2, w, h
}

// Anti aliased coverage of a rounded rectangle scaled by s into a size by size mask
func roundedMask(width, height int, r rrect, s float64) *image.Alpha {
	z := vector.NewRasterizer(width, height)
	addRounded(z, r, s)
	m := image.NewAlpha(image.Rect(0, 0, width, height))
	z.Draw(m, m.Bounds(), image.Opaque, image.Point{})
	return m
}

func addRounded(z *vector.Rasterizer, r rrect, s float64) {
	x0, y0, x1, y1, rx := float32(r.x*s), float32(r.y*s), float32((r.x+r.w)*s), float32((r.y+r.h)*s), float32(r.rx*s)
	if rx <= 0 {
		z.MoveTo(x0, y0)
		z.LineTo(x1, y0)
		z.LineTo(x1, y1)
		z.LineTo(x0, y1)
		z.ClosePath()
		return
	}
	k := rx * 0.5522847498
	z.MoveTo(x0+rx, y0)
	z.LineTo(x1-rx, y0)
	z.CubeTo(x1-rx+k, y0, x1, y0+rx-k, x1, y0+rx)
	z.LineTo(x1, y1-rx)
	z.CubeTo(x1, y1-rx+k, x1-rx+k, y1, x1-rx, y1)
	z.LineTo(x0+rx, y1)
	z.CubeTo(x0+rx-k, y1, x0, y1-rx+k, x0, y1-rx)
	z.LineTo(x0, y0+rx)
	z.CubeTo(x0, y0+rx-k, x0+rx-k, y0, x0+rx, y0)
	z.ClosePath()
}

// Five by seven lowercase hexadecimal glyphs, one five bit row at a time
var hexGlyphs = map[byte][7]uint8{
	'0': {14, 17, 19, 21, 25, 17, 14}, '1': {4, 12, 4, 4, 4, 4, 14}, '2': {14, 17, 1, 2, 4, 8, 31},
	'3': {30, 1, 1, 14, 1, 1, 30}, '4': {2, 6, 10, 18, 31, 2, 2}, '5': {31, 16, 16, 30, 1, 1, 30},
	'6': {14, 16, 16, 30, 17, 17, 14}, '7': {31, 1, 2, 4, 8, 8, 8}, '8': {14, 17, 17, 14, 17, 17, 14},
	'9': {14, 17, 17, 15, 1, 1, 14}, 'a': {0, 0, 14, 1, 15, 17, 15}, 'b': {16, 16, 30, 17, 17, 17, 30},
	'c': {0, 0, 14, 16, 16, 17, 14}, 'd': {1, 1, 15, 17, 17, 17, 15}, 'e': {0, 0, 14, 17, 31, 16, 14},
	'f': {6, 9, 8, 28, 8, 8, 8},
}

// Commit badge plate and glyph origin on the canvas
var (
	badgeRect   = rrect{160, 28, 544, 124, 24}
	badgeOrigin = [2]float64{184, 48}
)

// Paints the seven digit commit badge, moved onto the mac grid for mac tiles
func paintBadge(dst *image.RGBA, commit string, s float64, mac bool) {
	size := dst.Bounds().Dx()
	place := func(r rrect) rrect {
		if !mac {
			return r
		}
		k := 824.0 / 1024.0
		return rrect{100 + r.x*k, 100 + r.y*k, r.w * k, r.h * k, r.rx * k}
	}
	plate := roundedMask(size, size, place(badgeRect), s)
	draw.DrawMask(dst, dst.Bounds(), image.NewUniform(color.Black), image.Point{}, plate, image.Point{}, draw.Over)
	z := vector.NewRasterizer(size, size)
	for index := 0; index < len(commit) && index < 7; index++ {
		rows := hexGlyphs[commit[index]]
		for row, bits := range rows {
			for col := 0; col < 5; col++ {
				if bits&(1<<(4-col)) != 0 {
					x := badgeOrigin[0] + float64((index*6+col)*16)
					y := badgeOrigin[1] + float64(row*16)
					addRounded(z, place(rrect{x, y, 16, 16, 0}), s)
				}
			}
		}
	}
	glyphs := image.NewAlpha(dst.Bounds())
	z.Draw(glyphs, glyphs.Bounds(), image.Opaque, image.Point{})
	draw.DrawMask(dst, dst.Bounds(), image.NewUniform(color.White), image.Point{}, glyphs, image.Point{}, draw.Over)
}

// Draws src scaled by k with its origin at (tx, ty) using Catmull Rom resampling
func drawScaled(dst *image.RGBA, src image.Image, k, tx, ty float64) {
	draw.CatmullRom.Transform(dst, f64.Aff3{k, 0, tx, 0, k, ty}, src, src.Bounds(), draw.Over, nil)
}

// Places the mark into its box on a size by size layer, origin and scale in canvas units
func markLayer(size int, mark *image.NRGBA, x, y, w float64) *image.RGBA {
	s := float64(size) / canvasSide
	layer := image.NewRGBA(image.Rect(0, 0, size, size))
	k := w * s / float64(mark.Bounds().Dx()-2*markMargin)
	drawScaled(layer, mark, k, x*s-markMargin*k, y*s-markMargin*k)
	return layer
}

// Renders one tile master at size, badge and mark painted on the opaque plate whose outline alone sets alpha
func (a *art) tile(size int, plate rrect, dark, badge bool) *image.NRGBA {
	s := float64(size) / canvasSide
	fill, mark := a.light, a.src.markBlack
	if dark {
		fill, mark = a.dark, a.src.markWhite
	}
	dst := image.NewRGBA(image.Rect(0, 0, size, size))
	draw.Draw(dst, dst.Bounds(), image.NewUniform(fill), image.Point{}, draw.Src)
	if a.commit != "" && badge {
		paintBadge(dst, a.commit, s, plate == macPlate)
	}
	x, y, w, _ := markBox(plate, mark)
	draw.Draw(dst, dst.Bounds(), markLayer(size, mark, x, y, w), image.Point{}, draw.Over)
	out := toNRGBA(dst)
	for i, v := range roundedMask(size, size, plate, s).Pix {
		out.Pix[i*4+3] = v
		if v == 0 {
			out.Pix[i*4], out.Pix[i*4+1], out.Pix[i*4+2] = 0, 0, 0
		}
	}
	return out
}

// Canvas filling mark layer of the layered macOS icon, commit badge riding in the top quarter
func (a *art) iconLayer(dark bool) *image.NRGBA {
	mark := a.src.markBlack
	if dark {
		mark = a.src.markWhite
	}
	dst := image.NewRGBA(image.Rect(0, 0, canvasSide, canvasSide))
	if a.commit != "" {
		paintBadge(dst, a.commit, 1, false)
	}
	x, y, w, _ := markBox(canvasPlate, mark)
	draw.Draw(dst, dst.Bounds(), markLayer(canvasSide, mark, x, y, w), image.Point{}, draw.Over)
	return toNRGBA(dst)
}

// Grayscale Clear and Tinted material, accents as frosted ink and letters as opaque white
func (a *art) iconMono() *image.NRGBA {
	all := alphaOf(a.iconLayer(false))
	x, y, w, _ := markBox(canvasPlate, a.src.markWhite)
	accentRGBA := image.NewNRGBA(a.src.accent.Bounds())
	for i, v := range a.src.accent.Pix {
		accentRGBA.Pix[i*4+3] = v
	}
	accent := alphaOf(toNRGBA(markLayer(canvasSide, accentRGBA, x, y, w)))
	out := image.NewNRGBA(image.Rect(0, 0, canvasSide, canvasSide))
	ink := uint8(math.Round(monoInkTone * 255))
	for i, v := range all.Pix {
		if v > 127 {
			out.Pix[i*4], out.Pix[i*4+1], out.Pix[i*4+2] = ink, ink, ink
			out.Pix[i*4+3] = uint8(math.Round(float64(v) * monoInkOpacity))
		}
		if letters := int(v) - int(accent.Pix[i]); letters > 127 {
			out.Pix[i*4], out.Pix[i*4+1], out.Pix[i*4+2], out.Pix[i*4+3] = 255, 255, 255, uint8(letters)
		}
	}
	return out
}

func alphaOf(img *image.NRGBA) *image.Alpha {
	m := image.NewAlpha(img.Bounds())
	for i := range m.Pix {
		m.Pix[i] = img.Pix[i*4+3]
	}
	return m
}

func toNRGBA(src image.Image) *image.NRGBA {
	out := image.NewNRGBA(image.Rect(0, 0, src.Bounds().Dx(), src.Bounds().Dy()))
	draw.Draw(out, out.Bounds(), src, src.Bounds().Min, draw.Src)
	return out
}

// Catmull Rom resize of a whole image, premultiplied so transparent pixels never bleed colour
func resize(src *image.NRGBA, w, h int) *image.NRGBA {
	dst := image.NewRGBA(image.Rect(0, 0, w, h))
	draw.CatmullRom.Scale(dst, dst.Bounds(), src, src.Bounds(), draw.Src, nil)
	return toNRGBA(dst)
}

// Scales src to cover a w by h frame around the focus point, then composites it over bg
func cover(src *image.NRGBA, crop image.Rectangle, w, h int, bg color.NRGBA) *image.NRGBA {
	dst := image.NewRGBA(image.Rect(0, 0, w, h))
	draw.Draw(dst, dst.Bounds(), image.NewUniform(bg), image.Point{}, draw.Src)
	k := math.Max(float64(w)/float64(crop.Dx()), float64(h)/float64(crop.Dy()))
	tx := (float64(w) - float64(crop.Dx())*k) / 2
	ty := (float64(h) - float64(crop.Dy())*k) / 2
	draw.CatmullRom.Transform(dst, f64.Aff3{k, 0, tx - float64(crop.Min.X)*k, 0, k, ty - float64(crop.Min.Y)*k}, src, crop, draw.Over, nil)
	return toNRGBA(dst)
}
