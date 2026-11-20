package termlab

import (
	"encoding/binary"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/png"
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

// Fixed part of an XWD file header, twenty five big endian words
const xwdHeaderSize = 100

// Decodes a region of a 32 bit ZPixmap XWD image, the whole image when empty, as Xvfb keeps its framebuffer
func decodeXWD(b []byte, region image.Rectangle) (*image.RGBA, error) {
	if len(b) < xwdHeaderSize {
		return nil, fmt.Errorf("xwd header: %d bytes", len(b))
	}
	word := func(i int) uint32 { return binary.BigEndian.Uint32(b[4*i:]) }
	headerSize, version, format, depth := int(word(0)), word(1), word(2), word(3)
	w, h, byteOrder, bpp, stride := int(word(4)), int(word(5)), word(7), word(11), int(word(12))
	rmask, gmask, bmask, ncolors := word(14), word(15), word(16), int(word(19))
	if version != 7 || format != 2 || bpp != 32 || depth < 24 || w <= 0 || h <= 0 || stride < 4*w {
		return nil, fmt.Errorf("xwd version %d format %d depth %d at %d bits is not a 32 bit ZPixmap", version, format, depth, bpp)
	}
	shift := func(mask uint32) (uint32, error) {
		for s := uint32(0); s < 32; s += 8 {
			if mask == 0xff<<s {
				return s, nil
			}
		}
		return 0, fmt.Errorf("xwd channel mask %#x is not one byte", mask)
	}
	rs, err := shift(rmask)
	if err != nil {
		return nil, err
	}
	gs, err := shift(gmask)
	if err != nil {
		return nil, err
	}
	bs, err := shift(bmask)
	if err != nil {
		return nil, err
	}
	offset := headerSize + 12*ncolors
	if headerSize < xwdHeaderSize || offset+stride*h > len(b) {
		return nil, fmt.Errorf("xwd pixels: %d bytes hold no %dx%d image at offset %d", len(b), w, h, offset)
	}
	order := binary.ByteOrder(binary.LittleEndian)
	if byteOrder != 0 {
		order = binary.BigEndian
	}
	bounds := image.Rect(0, 0, w, h)
	if !region.Empty() {
		bounds = region.Intersect(bounds)
	}
	img := image.NewRGBA(bounds)
	for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
		row := b[offset+y*stride:]
		for x := bounds.Min.X; x < bounds.Max.X; x++ {
			px := order.Uint32(row[4*x:])
			j := img.PixOffset(x, y)
			img.Pix[j], img.Pix[j+1], img.Pix[j+2], img.Pix[j+3] = uint8(px>>rs), uint8(px>>gs), uint8(px>>bs), 255
		}
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
