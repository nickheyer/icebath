package imaging

import (
	"image"
	"image/color"
	"image/draw"
)

// Decoder output mapped to the package conventions, opaque colour as RGBA, alpha as NRGBA, eight bits per channel
func normalizeDecoded(img image.Image) image.Image {
	switch m := img.(type) {
	case *image.RGBA64:
		if m.Opaque() {
			return ToRGB(m)
		}
		return ToNRGBA(m)
	case *image.NRGBA64:
		return ToNRGBA(m)
	case *image.NYCbCrA:
		return ToNRGBA(m)
	case *image.RGBA:
		if !m.Opaque() {
			return ToNRGBA(m)
		}
	}
	return img
}

// Reports whether the image carries an alpha channel or a palette entry that is not opaque
func HasAlpha(img image.Image) bool { return hasAlpha(img) }

// Alpha channel test by pixel layout, palettes judged by their entries
func hasAlpha(img image.Image) bool {
	switch m := img.(type) {
	case *image.NRGBA, *image.NRGBA64, *image.NYCbCrA, *image.Alpha, *image.Alpha16:
		return true
	case *image.Paletted:
		for _, c := range m.Palette {
			if _, _, _, a := c.RGBA(); a != 0xffff {
				return true
			}
		}
		return false
	case *image.RGBA:
		return !m.Opaque()
	case *image.RGBA64:
		return !m.Opaque()
	case *image.Gray, *image.Gray16, *image.CMYK, *image.YCbCr:
		return false
	}
	if o, ok := img.(interface{ Opaque() bool }); ok {
		return !o.Opaque()
	}
	return true
}

// Straight alpha copy with a zero origin, CMYK through the inverted product and sixteen bit samples by high byte
func ToNRGBA(img image.Image) *image.NRGBA {
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	out := image.NewNRGBA(image.Rect(0, 0, w, h))
	switch m := img.(type) {
	case *image.NRGBA:
		for y := 0; y < h; y++ {
			copy(out.Pix[y*out.Stride:y*out.Stride+w*4], m.Pix[m.PixOffset(b.Min.X, b.Min.Y+y):])
		}
	case *image.Paletted:
		lut := make([]color.NRGBA, 256)
		for i, c := range m.Palette {
			if i < 256 {
				lut[i] = color.NRGBAModel.Convert(c).(color.NRGBA)
			}
		}
		for y := 0; y < h; y++ {
			row := m.Pix[m.PixOffset(b.Min.X, b.Min.Y+y):]
			o := out.Pix[y*out.Stride:]
			for x := 0; x < w; x++ {
				c := lut[row[x]]
				o[4*x], o[4*x+1], o[4*x+2], o[4*x+3] = c.R, c.G, c.B, c.A
			}
		}
	case *image.Gray:
		for y := 0; y < h; y++ {
			row := m.Pix[m.PixOffset(b.Min.X, b.Min.Y+y):]
			o := out.Pix[y*out.Stride:]
			for x := 0; x < w; x++ {
				v := row[x]
				o[4*x], o[4*x+1], o[4*x+2], o[4*x+3] = v, v, v, 255
			}
		}
	case *image.Gray16:
		for y := 0; y < h; y++ {
			row := m.Pix[m.PixOffset(b.Min.X, b.Min.Y+y):]
			o := out.Pix[y*out.Stride:]
			for x := 0; x < w; x++ {
				v := row[2*x]
				o[4*x], o[4*x+1], o[4*x+2], o[4*x+3] = v, v, v, 255
			}
		}
	case *image.NRGBA64:
		for y := 0; y < h; y++ {
			row := m.Pix[m.PixOffset(b.Min.X, b.Min.Y+y):]
			o := out.Pix[y*out.Stride:]
			for x := 0; x < w; x++ {
				o[4*x], o[4*x+1], o[4*x+2], o[4*x+3] = row[8*x], row[8*x+2], row[8*x+4], row[8*x+6]
			}
		}
	case *image.CMYK:
		for y := 0; y < h; y++ {
			row := m.Pix[m.PixOffset(b.Min.X, b.Min.Y+y):]
			o := out.Pix[y*out.Stride:]
			for x := 0; x < w; x++ {
				r, g, bl := cmykToRGB(row[4*x], row[4*x+1], row[4*x+2], row[4*x+3])
				o[4*x], o[4*x+1], o[4*x+2], o[4*x+3] = r, g, bl, 255
			}
		}
	case *image.YCbCr:
		rgba := image.NewRGBA(out.Rect)
		draw.Draw(rgba, rgba.Rect, m, b.Min, draw.Src)
		copy(out.Pix, rgba.Pix)
	case *image.RGBA:
		for y := 0; y < h; y++ {
			row := m.Pix[m.PixOffset(b.Min.X, b.Min.Y+y):]
			o := out.Pix[y*out.Stride:]
			for x := 0; x < w; x++ {
				a := row[4*x+3]
				o[4*x+3] = a
				switch a {
				case 0:
				case 255:
					o[4*x], o[4*x+1], o[4*x+2] = row[4*x], row[4*x+1], row[4*x+2]
				default:
					for c := 0; c < 3; c++ {
						o[4*x+c] = uint8(min(255, int(row[4*x+c])*255/int(a)))
					}
				}
			}
		}
	default:
		for y := 0; y < h; y++ {
			o := out.Pix[y*out.Stride:]
			for x := 0; x < w; x++ {
				c := color.NRGBAModel.Convert(img.At(b.Min.X+x, b.Min.Y+y)).(color.NRGBA)
				o[4*x], o[4*x+1], o[4*x+2], o[4*x+3] = c.R, c.G, c.B, c.A
			}
		}
	}
	return out
}

// Opaque copy with a zero origin whose colour keeps the straight values and drops any alpha
func ToRGB(img image.Image) *image.RGBA {
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	out := image.NewRGBA(image.Rect(0, 0, w, h))
	switch m := img.(type) {
	case *image.YCbCr:
		draw.Draw(out, out.Rect, m, b.Min, draw.Src)
		return out
	case *image.RGBA:
		if m.Opaque() {
			for y := 0; y < h; y++ {
				copy(out.Pix[y*out.Stride:y*out.Stride+w*4], m.Pix[m.PixOffset(b.Min.X, b.Min.Y+y):])
			}
			return out
		}
	}
	n := ToNRGBA(img)
	copy(out.Pix, n.Pix)
	for i := 3; i < len(out.Pix); i += 4 {
		out.Pix[i] = 255
	}
	return out
}

// Opaque copy with transparency blended over the background colour by the alpha of each pixel
func Flatten(img image.Image, background color.Color) *image.RGBA {
	if !hasAlpha(img) {
		return ToRGB(img)
	}
	bg := color.RGBAModel.Convert(background).(color.RGBA)
	n := ToNRGBA(img)
	out := image.NewRGBA(n.Rect)
	back := [3]uint32{uint32(bg.R), uint32(bg.G), uint32(bg.B)}
	for i := 0; i < len(n.Pix); i += 4 {
		a := uint32(n.Pix[i+3])
		for c := 0; c < 3; c++ {
			t := uint32(n.Pix[i+c])*a + back[c]*(255-a) + 128
			out.Pix[i+c] = uint8(((t >> 8) + t) >> 8)
		}
		out.Pix[i+3] = 255
	}
	return out
}

// Product of two bytes divided by 255 with rounding
func mulDiv255(a, b uint32) uint32 {
	t := a*b + 128
	return ((t >> 8) + t) >> 8
}

// RGB of one CMYK sample, each channel the inverted key less the ink share of it
func cmykToRGB(c, m, y, k uint8) (uint8, uint8, uint8) {
	nk := uint32(255 - k)
	ch := func(v uint8) uint8 {
		r := int(nk) - int(mulDiv255(uint32(v), nk))
		return uint8(max(0, min(255, r)))
	}
	return ch(c), ch(m), ch(y)
}

// Single channel grey copy weighting red, green and blue by 299, 587 and 114 thousandths
func toGray(img image.Image) *image.Gray {
	if g, ok := img.(*image.Gray); ok {
		b := g.Bounds()
		out := image.NewGray(image.Rect(0, 0, b.Dx(), b.Dy()))
		for y := 0; y < b.Dy(); y++ {
			copy(out.Pix[y*out.Stride:y*out.Stride+b.Dx()], g.Pix[g.PixOffset(b.Min.X, b.Min.Y+y):])
		}
		return out
	}
	rgb := ToRGB(img)
	out := image.NewGray(rgb.Rect)
	for i, j := 0, 0; i < len(rgb.Pix); i, j = i+4, j+1 {
		out.Pix[j] = uint8((uint32(rgb.Pix[i])*19595 + uint32(rgb.Pix[i+1])*38470 + uint32(rgb.Pix[i+2])*7471 + 0x8000) >> 16)
	}
	return out
}
