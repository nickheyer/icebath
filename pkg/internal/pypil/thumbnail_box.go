package pypil

import (
	"image"
	"math"
)

// Size fitting a width by height box with the aspect ratio kept, ok false when already inside it
func ThumbnailSizeBox(w, h, boxW, boxH int) (int, int, bool) {
	x, y := boxW, boxH
	if x >= w && y >= h {
		return w, h, false
	}
	aspect := float64(w) / float64(h)
	roundAspect := func(number float64, key func(float64) float64) int {
		lo, hi := math.Floor(number), math.Ceil(number)
		pick := lo
		if key(hi) < key(lo) {
			pick = hi
		}
		return max(int(pick), 1)
	}
	if float64(x)/float64(y) >= aspect {
		x = roundAspect(float64(y)*aspect, func(n float64) float64 { return math.Abs(aspect - n/float64(y)) })
	} else {
		y = roundAspect(float64(x)/aspect, func(n float64) float64 {
			if n == 0 {
				return 0
			}
			return math.Abs(aspect - float64(x)/n)
		})
	}
	return x, y, true
}

// Opaque copy shrunk to fit the width by height box, bicubic with a reducing gap of two
func ThumbnailBox(img *image.NRGBA, boxW, boxH int) *image.NRGBA {
	w, h := img.Rect.Dx(), img.Rect.Dy()
	fw, fh, ok := ThumbnailSizeBox(w, h, boxW, boxH)
	if !ok || (fw == w && fh == h) {
		out := image.NewNRGBA(img.Rect)
		copy(out.Pix, img.Pix)
		return out
	}
	box := [4]float64{0, 0, float64(w), float64(h)}
	fx, fy := max(int(float64(w)/float64(fw)/2.0), 1), max(int(float64(h)/float64(fh)/2.0), 1)
	src := img
	if fx > 1 || fy > 1 {
		src = pilReduce(img, fx, fy)
		box = [4]float64{0, 0, float64(w) / float64(fx), float64(h) / float64(fy)}
	}
	return pilResample(src, fw, fh, box)
}
