package imageutil

import (
	"image"
	"image/color"
	"image/draw"
)

// Straight alpha RGBA copy of any image with its top left corner moved to the origin
func ToNRGBA(src image.Image) *image.NRGBA {
	b := src.Bounds()
	w, h := b.Dx(), b.Dy()
	out := image.NewNRGBA(image.Rect(0, 0, w, h))
	if w == 0 || h == 0 {
		return out
	}
	switch s := src.(type) {
	case *image.NRGBA:
		for y := range h {
			i := s.PixOffset(b.Min.X, b.Min.Y+y)
			copy(out.Pix[y*out.Stride:y*out.Stride+4*w], s.Pix[i:i+4*w])
		}
	case *image.RGBA:
		for y := range h {
			sp := s.Pix[s.PixOffset(b.Min.X, b.Min.Y+y):]
			dp := out.Pix[y*out.Stride : y*out.Stride+4*w]
			for i := 0; i < len(dp); i += 4 {
				a := uint32(sp[i+3])
				dp[i+3] = sp[i+3]
				switch a {
				case 0:
				case 0xff:
					dp[i], dp[i+1], dp[i+2] = sp[i], sp[i+1], sp[i+2]
				default:
					dp[i] = straight(uint32(sp[i])*0x101, a*0x101)
					dp[i+1] = straight(uint32(sp[i+1])*0x101, a*0x101)
					dp[i+2] = straight(uint32(sp[i+2])*0x101, a*0x101)
				}
			}
		}
	case *image.YCbCr:
		draw.Draw(&image.RGBA{Pix: out.Pix, Stride: out.Stride, Rect: out.Rect}, out.Rect, s, b.Min, draw.Src)
	case *image.NYCbCrA:
		for y := range h {
			dp := out.Pix[y*out.Stride : y*out.Stride+4*w]
			for x := range w {
				px, py := b.Min.X+x, b.Min.Y+y
				yi, ci := s.YOffset(px, py), s.COffset(px, py)
				dp[4*x], dp[4*x+1], dp[4*x+2] = color.YCbCrToRGB(s.Y[yi], s.Cb[ci], s.Cr[ci])
				dp[4*x+3] = s.A[s.AOffset(px, py)]
			}
		}
	case *image.NRGBA64:
		for y := range h {
			sp := s.Pix[s.PixOffset(b.Min.X, b.Min.Y+y):]
			dp := out.Pix[y*out.Stride : y*out.Stride+4*w]
			for i := range dp {
				dp[i] = sp[2*i]
			}
		}
	case *image.Gray:
		for y := range h {
			sp := s.Pix[s.PixOffset(b.Min.X, b.Min.Y+y):]
			dp := out.Pix[y*out.Stride : y*out.Stride+4*w]
			for x := range w {
				v := sp[x]
				dp[4*x], dp[4*x+1], dp[4*x+2], dp[4*x+3] = v, v, v, 0xff
			}
		}
	case *image.Paletted:
		var lut [256][4]uint8
		for i, c := range s.Palette[:min(len(s.Palette), 256)] {
			n := color.NRGBAModel.Convert(c).(color.NRGBA)
			lut[i] = [4]uint8{n.R, n.G, n.B, n.A}
		}
		for y := range h {
			sp := s.Pix[s.PixOffset(b.Min.X, b.Min.Y+y):]
			dp := out.Pix[y*out.Stride : y*out.Stride+4*w]
			for x := range w {
				copy(dp[4*x:4*x+4], lut[sp[x]][:])
			}
		}
	case image.RGBA64Image:
		for y := range h {
			dp := out.Pix[y*out.Stride : y*out.Stride+4*w]
			for x := range w {
				c := s.RGBA64At(b.Min.X+x, b.Min.Y+y)
				setStraight(dp[4*x:4*x+4], uint32(c.R), uint32(c.G), uint32(c.B), uint32(c.A))
			}
		}
	default:
		for y := range h {
			dp := out.Pix[y*out.Stride : y*out.Stride+4*w]
			for x := range w {
				r, g, bl, a := src.At(b.Min.X+x, b.Min.Y+y).RGBA()
				setStraight(dp[4*x:4*x+4], r, g, bl, a)
			}
		}
	}
	return out
}

// Writes a 16 bit premultiplied colour into four bytes of straight alpha RGBA as color.NRGBAModel converts it
func setStraight(dp []uint8, r, g, b, a uint32) {
	switch a {
	case 0:
		dp[0], dp[1], dp[2], dp[3] = 0, 0, 0, 0
	case 0xffff:
		dp[0], dp[1], dp[2], dp[3] = uint8(r>>8), uint8(g>>8), uint8(b>>8), 0xff
	default:
		dp[0], dp[1], dp[2], dp[3] = straight(r, a), straight(g, a), straight(b, a), uint8(a>>8)
	}
}

// Eight bit straight channel of a 16 bit premultiplied channel under a 16 bit alpha that is neither zero nor full, saturating when the channel exceeds its alpha
func straight(c, a uint32) uint8 { return uint8(min(c*0xffff/a, 0xffff) >> 8) }
