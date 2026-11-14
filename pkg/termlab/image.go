package termlab

import (
	"bufio"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"os"
)

// Reports whether every pixel of the rectangle inside the image has one color
func Uniform(img image.Image, r image.Rectangle) bool {
	r = r.Intersect(img.Bounds())
	if r.Empty() {
		return true
	}
	first := rgba(img, r.Min.X, r.Min.Y)
	for y := r.Min.Y; y < r.Max.Y; y++ {
		for x := r.Min.X; x < r.Max.X; x++ {
			if rgba(img, x, y) != first {
				return false
			}
		}
	}
	return true
}

// Share of pixels of the rectangle whose color differs between the two images
func Differs(a, b image.Image, r image.Rectangle) float64 {
	r = r.Intersect(a.Bounds()).Intersect(b.Bounds())
	if r.Empty() {
		return 0
	}
	n := 0
	for y := r.Min.Y; y < r.Max.Y; y++ {
		for x := r.Min.X; x < r.Max.X; x++ {
			if rgba(a, x, y) != rgba(b, x, y) {
				n++
			}
		}
	}
	return float64(n) / float64(r.Dx()*r.Dy())
}

// Share of pixels of the rectangle whose color is not the background
func Coverage(img image.Image, r image.Rectangle, bg color.Color) float64 {
	r = r.Intersect(img.Bounds())
	if r.Empty() {
		return 0
	}
	want := color.RGBAModel.Convert(bg).(color.RGBA)
	n := 0
	for y := r.Min.Y; y < r.Max.Y; y++ {
		for x := r.Min.X; x < r.Max.X; x++ {
			if rgba(img, x, y) != want {
				n++
			}
		}
	}
	return float64(n) / float64(r.Dx()*r.Dy())
}

// Colors of every pixel inside any of the rectangles, each clipped to the image
func Sample(img image.Image, rects []image.Rectangle) []color.RGBA {
	var out []color.RGBA
	for _, r := range rects {
		r = r.Intersect(img.Bounds())
		for y := r.Min.Y; y < r.Max.Y; y++ {
			for x := r.Min.X; x < r.Max.X; x++ {
				out = append(out, rgba(img, x, y))
			}
		}
	}
	return out
}

// Bounding box of the pixels inside the rectangle that differ between the two images
func DiffBounds(a, b image.Image, r image.Rectangle) image.Rectangle {
	r = r.Intersect(a.Bounds()).Intersect(b.Bounds())
	out := image.Rectangle{}
	for y := r.Min.Y; y < r.Max.Y; y++ {
		for x := r.Min.X; x < r.Max.X; x++ {
			if rgba(a, x, y) != rgba(b, x, y) {
				out = out.Union(image.Rect(x, y, x+1, y+1))
			}
		}
	}
	return out
}

// Color of one pixel as opaque eight bit channels, read straight from the buffer of an RGBA image
func rgba(img image.Image, x, y int) color.RGBA {
	if m, ok := img.(*image.RGBA); ok {
		i := m.PixOffset(x, y)
		p := m.Pix[i : i+4 : i+4]
		return color.RGBA{p[0], p[1], p[2], 255}
	}
	c := color.RGBAModel.Convert(img.At(x, y)).(color.RGBA)
	c.A = 255
	return c
}

// Decodes a binary portable pixmap with eight bit channels
func decodePPM(r io.Reader) (*image.RGBA, error) {
	br := bufio.NewReaderSize(r, 1<<20)
	var magic string
	var w, h, maxval int
	if _, err := fmt.Fscan(br, &magic, &w, &h, &maxval); err != nil {
		return nil, fmt.Errorf("ppm header: %w", err)
	}
	if magic != "P6" || maxval != 255 || w <= 0 || h <= 0 {
		return nil, fmt.Errorf("ppm header %s %dx%d max %d is not an eight bit P6", magic, w, h, maxval)
	}
	if _, err := br.ReadByte(); err != nil {
		return nil, err
	}
	raw := make([]byte, w*h*3)
	if _, err := io.ReadFull(br, raw); err != nil {
		return nil, fmt.Errorf("ppm pixels: %w", err)
	}
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for i, j := 0, 0; i < len(raw); i, j = i+3, j+4 {
		img.Pix[j], img.Pix[j+1], img.Pix[j+2], img.Pix[j+3] = raw[i], raw[i+1], raw[i+2], 255
	}
	return img, nil
}

// Writes the image as a PNG file
func SavePNG(path string, img image.Image) error {
	if img == nil {
		return errors.New("no image to save")
	}
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	if err := png.Encode(f, img); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}
