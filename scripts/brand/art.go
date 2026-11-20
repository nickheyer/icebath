package main

import (
	"fmt"
	"image"
	"image/color"
	"image/png"
	"math"
	"os"
	"path/filepath"

	"golang.org/x/image/draw"
)

const (
	logoFile        = "ICE_BATH_SQUARE_LOGO.png"
	textWhiteFile   = "ICE_BATH_BANNER_TEXT_WHITE.png"
	markMargin      = 6
	inkAlphaCutoff  = 128
	inkLineMinCount = 3
)

// Decoded source art with the measurements every renderer shares
type sources struct {
	logo      *image.NRGBA
	markWhite *image.NRGBA
	accent    *image.Alpha
	textWhite *image.NRGBA
	textBox   image.Rectangle
	letter    color.NRGBA
	red       color.NRGBA
}

// Source art every target renders from
type art struct {
	src *sources
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

// Decodes the logo and the white wordmark and derives the mark and the palette
func loadSources(root string) (*sources, error) {
	dir := filepath.Join(root, "assets")
	imgs := map[string]*image.NRGBA{}
	for _, name := range []string{logoFile, textWhiteFile} {
		img, err := readPNG(filepath.Join(dir, name))
		if err != nil {
			return nil, err
		}
		imgs[name] = img
	}
	s := &sources{logo: imgs[logoFile], textWhite: imgs[textWhiteFile]}
	logoBox, whiteBox := inkBox(s.logo), inkBox(s.textWhite)
	if logoBox.Empty() || whiteBox.Empty() {
		return nil, fmt.Errorf("source art without ink: logo %v, white text %v", logoBox, whiteBox)
	}
	s.textBox = whiteBox
	var err error
	if s.letter, s.red, err = inkPalette(s.textWhite); err != nil {
		return nil, fmt.Errorf("%s: %w", textWhiteFile, err)
	}
	logoLetter, logoRed, err := inkPalette(s.logo)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", logoFile, err)
	}
	s.markWhite, s.accent = splitMark(s.logo, logoBox, logoLetter, logoRed)
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

// Crops the logo as the white mark and records the accent weight of every pixel
func splitMark(logo *image.NRGBA, box image.Rectangle, letter, red color.NRGBA) (*image.NRGBA, *image.Alpha) {
	r := box.Inset(-markMargin).Intersect(logo.Bounds())
	w, h := box.Dx()+2*markMargin, box.Dy()+2*markMargin
	white := image.NewNRGBA(image.Rect(0, 0, w, h))
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
			accent.SetAlpha(dx, dy, color.Alpha{uint8(math.Round(float64(c.A) * t))})
		}
	}
	return white, accent
}
