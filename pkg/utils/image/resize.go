package imageutil

import (
	"image"
	"math"
)

// Resampling kernel Resize and Thumbnail scale with
type Filter int

const (
	// Three lobe windowed sinc, the sharpest result for photos and artwork scaled either way
	Lanczos Filter = iota
	// Cubic spline with a of minus one half, a little softer than Lanczos with less ringing at hard edges
	CatmullRom
	// Nearest source pixel, keeping hard edges and exact colours for palette images and pixel art
	Nearest
)

// Copy of img scaled to w by h with the filter, colours weighted by their alpha so transparent pixels never bleed into visible ones; an empty image for a size that is not positive and a clear one when img has no pixels
func Resize(img *image.NRGBA, w, h int, filter Filter) *image.NRGBA {
	w, h = max(w, 0), max(h, 0)
	sw, sh := img.Rect.Dx(), img.Rect.Dy()
	out := image.NewNRGBA(image.Rect(0, 0, w, h))
	switch {
	case w == 0 || h == 0 || sw == 0 || sh == 0:
	case w == sw && h == sh:
		for y := range h {
			copy(out.Pix[y*out.Stride:y*out.Stride+4*w], img.Pix[y*img.Stride:y*img.Stride+4*w])
		}
	case filter == Nearest:
		nearest(out, img)
	default:
		resample(out, img, filter.kernel())
	}
	return out
}

// Copy of img scaled down with the filter to fit inside a side by side box with its aspect ratio kept, a plain copy when it already fits
func Thumbnail(img *image.NRGBA, side int, filter Filter) *image.NRGBA {
	w, h, _ := FitSize(img.Rect.Dx(), img.Rect.Dy(), side)
	return Resize(img, w, h, filter)
}

// Width and height of a w by h image scaled down to fit inside a side by side box with its aspect ratio kept and neither below one; the size unchanged and false when it already fits
func FitSize(w, h, side int) (int, int, bool) {
	side = max(side, 1)
	if w <= side && h <= side {
		return w, h, false
	}
	if w >= h {
		return side, max(1, int(math.Round(float64(h)*float64(side)/float64(w)))), true
	}
	return max(1, int(math.Round(float64(w)*float64(side)/float64(h)))), side, true
}

// Fills dst with the source pixel under the centre of each destination pixel
func nearest(dst, src *image.NRGBA) {
	sw, sh := src.Rect.Dx(), src.Rect.Dy()
	dw, dh := dst.Rect.Dx(), dst.Rect.Dy()
	xs := make([]int, dw)
	for x := range xs {
		xs[x] = 4 * min(int((float64(x)+0.5)*float64(sw)/float64(dw)), sw-1)
	}
	for y := range dh {
		sy := min(int((float64(y)+0.5)*float64(sh)/float64(dh)), sh-1)
		sp := src.Pix[sy*src.Stride:]
		dp := dst.Pix[y*dst.Stride : y*dst.Stride+4*dw]
		for x, sx := range xs {
			copy(dp[4*x:4*x+4], sp[sx:sx+4])
		}
	}
}
