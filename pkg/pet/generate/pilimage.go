package generate

import (
	"image"
	"math"
	"os"

	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"

	"github.com/nickheyer/icebath/pkg/internal/pypil"
	_ "golang.org/x/image/bmp"
	_ "golang.org/x/image/webp"
)

// Box is a left, top, right, bottom pixel rectangle as PIL boxes are
type Box [4]int

// Transparent RGBA canvas of the size
func blank(w, h int) *image.NRGBA { return image.NewNRGBA(image.Rect(0, 0, w, h)) }

// Straight alpha RGBA copy of an image
func toRGBA(img image.Image) *image.NRGBA {
	out := pypil.ToNRGBA(img)
	if out == img {
		cp := blank(out.Rect.Dx(), out.Rect.Dy())
		copy(cp.Pix, out.Pix)
		return cp
	}
	return out
}

// Opens an image file as RGBA
func loadRGBA(path string) (*image.NRGBA, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	img, _, err := image.Decode(f)
	if err != nil {
		return nil, err
	}
	return toRGBA(img), nil
}

// Offset of pixel x, y in the pixel slice
func pixOff(img *image.NRGBA, x, y int) int { return y*img.Stride + x*4 }

// Width and height of an image
func size(img *image.NRGBA) (int, int) { return img.Rect.Dx(), img.Rect.Dy() }

// Pillow integer divide by 255 with rounding bias already added
func shiftForDiv255(a uint32) uint32 { return ((a >> 8) + a) >> 8 }

// Composites src over dst at the offset in place with Pillow's integer alpha composite
func alphaComposite(dst, src *image.NRGBA, dx, dy int) {
	dw, dh := size(dst)
	sw, sh := size(src)
	const precisionBits = 7
	for y := 0; y < sh; y++ {
		ty := y + dy
		if ty < 0 || ty >= dh {
			continue
		}
		for x := 0; x < sw; x++ {
			tx := x + dx
			if tx < 0 || tx >= dw {
				continue
			}
			s := src.Pix[pixOff(src, x, y):]
			d := dst.Pix[pixOff(dst, tx, ty):]
			sa := uint32(s[3])
			if sa == 0 {
				continue
			}
			blend := uint32(d[3]) * (255 - sa)
			outa255 := sa*255 + blend
			coef1 := sa * 255 * 255 * (1 << precisionBits) / outa255
			coef2 := 255*(1<<precisionBits) - coef1
			for c := 0; c < 3; c++ {
				tmp := uint32(s[c])*coef1 + uint32(d[c])*coef2
				d[c] = uint8(shiftForDiv255(tmp+(0x80<<precisionBits)) >> precisionBits)
			}
			d[3] = uint8(shiftForDiv255(outa255 + 0x80))
		}
	}
}

// Sprite alpha composited at the offset onto a new transparent canvas
func place(sprite *image.NRGBA, w, h, ox, oy int) *image.NRGBA {
	canvas := blank(w, h)
	alphaComposite(canvas, sprite, ox, oy)
	return canvas
}

// Region of an image as a new image, transparent where the box leaves the source
func crop(img *image.NRGBA, b Box) *image.NRGBA {
	out := blank(b[2]-b[0], b[3]-b[1])
	w, h := size(img)
	for y := b[1]; y < b[3]; y++ {
		if y < 0 || y >= h {
			continue
		}
		for x := b[0]; x < b[2]; x++ {
			if x < 0 || x >= w {
				continue
			}
			copy(out.Pix[pixOff(out, x-b[0], y-b[1]):pixOff(out, x-b[0], y-b[1])+4], img.Pix[pixOff(img, x, y):pixOff(img, x, y)+4])
		}
	}
	return out
}

// Makes a box of the image fully transparent in place, clipped to the image
func clearRegion(img *image.NRGBA, b Box) {
	w, h := size(img)
	for y := max(0, b[1]); y < min(h, b[3]); y++ {
		for x := max(0, b[0]); x < min(w, b[2]); x++ {
			o := pixOff(img, x, y)
			img.Pix[o], img.Pix[o+1], img.Pix[o+2], img.Pix[o+3] = 0, 0, 0, 0
		}
	}
}

// Alpha only bounding box of the non transparent pixels, false for an empty image
func getbbox(img *image.NRGBA) (Box, bool) {
	w, h := size(img)
	l, t, r, b := w, h, 0, 0
	found := false
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			if img.Pix[pixOff(img, x, y)+3] != 0 {
				found = true
				l, t, r, b = min(l, x), min(t, y), max(r, x+1), max(b, y+1)
			}
		}
	}
	return Box{l, t, r, b}, found
}

// Copy of an image
func copyImage(img *image.NRGBA) *image.NRGBA {
	w, h := size(img)
	out := blank(w, h)
	for y := 0; y < h; y++ {
		copy(out.Pix[y*out.Stride:y*out.Stride+w*4], img.Pix[y*img.Stride:y*img.Stride+w*4])
	}
	return out
}

// Pillow COORD, minus one for a negative coordinate else its truncation
func pilCoord(v float64) int {
	if v < 0 {
		return -1
	}
	return int(v)
}

// Nearest neighbour resize with Pillow's scale affine sampling
func resizeNearest(img *image.NRGBA, ow, oh int) *image.NRGBA {
	iw, ih := size(img)
	if ow == iw && oh == ih {
		return copyImage(img)
	}
	out := blank(ow, oh)
	ax, ay := float64(iw)/float64(ow), float64(ih)/float64(oh)
	xin := make([]int, ow)
	xo := ax * 0.5
	for x := 0; x < ow; x++ {
		xin[x] = pilCoord(xo)
		xo += ax
	}
	yo := ay * 0.5
	for y := 0; y < oh; y++ {
		yi := pilCoord(yo)
		yo += ay
		if yi < 0 || yi >= ih {
			continue
		}
		for x := 0; x < ow; x++ {
			if xin[x] < 0 || xin[x] >= iw {
				continue
			}
			copy(out.Pix[pixOff(out, x, y):pixOff(out, x, y)+4], img.Pix[pixOff(img, xin[x], yi):pixOff(img, xin[x], yi)+4])
		}
	}
	return out
}

// Horizontally mirrored copy
func flipLeftRight(img *image.NRGBA) *image.NRGBA {
	w, h := size(img)
	out := blank(w, h)
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			copy(out.Pix[pixOff(out, w-1-x, y):pixOff(out, w-1-x, y)+4], img.Pix[pixOff(img, x, y):pixOff(img, x, y)+4])
		}
	}
	return out
}

// Per column alpha mass of the alpha channel bilinearly reduced to one row as Pillow resamples it
func columnProfile(img *image.NRGBA) []int {
	w, h := size(img)
	out := make([]int, w)
	if h == 1 {
		for x := 0; x < w; x++ {
			out[x] = int(img.Pix[pixOff(img, x, 0)+3])
		}
		return out
	}
	const precisionBits = 32 - 8 - 2
	scale := float64(h)
	support := scale
	center := 0.5 * scale
	ss := 1.0 / scale
	ymin := max(int(center-support+0.5), 0)
	ymax := min(int(center+support+0.5), h) - ymin
	weights := make([]float64, ymax)
	total := 0.0
	for y := 0; y < ymax; y++ {
		t := math.Abs((float64(y+ymin) - center + 0.5) * ss)
		wv := 0.0
		if t < 1.0 {
			wv = 1.0 - t
		}
		weights[y] = wv
		total += wv
	}
	kk := make([]int64, ymax)
	for y := 0; y < ymax; y++ {
		k := weights[y]
		if total != 0 {
			k /= total
		}
		if k < 0 {
			kk[y] = int64(-0.5 + k*float64(int64(1)<<precisionBits))
		} else {
			kk[y] = int64(0.5 + k*float64(int64(1)<<precisionBits))
		}
	}
	for x := 0; x < w; x++ {
		ss0 := int64(1) << (precisionBits - 1)
		for y := 0; y < ymax; y++ {
			ss0 += int64(img.Pix[pixOff(img, x, y+ymin)+3]) * kk[y]
		}
		switch {
		case ss0 >= int64(1)<<precisionBits<<8:
			out[x] = 255
		case ss0 <= 0:
			out[x] = 0
		default:
			out[x] = int(ss0 >> precisionBits)
		}
	}
	return out
}

// Alpha channel eroded by a 3x3 minimum with replicated edges, as MinFilter(3) after expand
func erodeAlpha3(img *image.NRGBA) {
	w, h := size(img)
	src := make([]uint8, w*h)
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			src[y*w+x] = img.Pix[pixOff(img, x, y)+3]
		}
	}
	clampi := func(v, hi int) int { return max(0, min(hi-1, v)) }
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			m := uint8(255)
			for dy := -1; dy <= 1; dy++ {
				for dx := -1; dx <= 1; dx++ {
					m = min(m, src[clampi(y+dy, h)*w+clampi(x+dx, w)])
				}
			}
			img.Pix[pixOff(img, x, y)+3] = m
		}
	}
}

// Python round of a float, half to even
func pyRound(v float64) int { return int(math.RoundToEven(v)) }
