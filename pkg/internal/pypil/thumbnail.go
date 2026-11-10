package pypil

import (
	"image"
	"image/color"
	"image/draw"
	"math"
)

// Straight alpha RGBA copy of any decoded image
func ToNRGBA(src image.Image) *image.NRGBA {
	b := src.Bounds()
	out := image.NewNRGBA(image.Rect(0, 0, b.Dx(), b.Dy()))
	if yc, ok := src.(*image.YCbCr); ok {
		rgba := image.NewRGBA(out.Rect)
		draw.Draw(rgba, rgba.Rect, yc, b.Min, draw.Src)
		copy(out.Pix, rgba.Pix)
		return out
	}
	for y := 0; y < b.Dy(); y++ {
		for x := 0; x < b.Dx(); x++ {
			c := color.NRGBAModel.Convert(src.At(b.Min.X+x, b.Min.Y+y)).(color.NRGBA)
			i := out.PixOffset(x, y)
			out.Pix[i], out.Pix[i+1], out.Pix[i+2], out.Pix[i+3] = c.R, c.G, c.B, c.A
		}
	}
	return out
}

// Size fitting a side by side box with the aspect ratio kept, ok false when already inside it
func ThumbnailSize(w, h, side int) (int, int, bool) {
	x, y := side, side
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

// Copy shrunk in place to fit the side box, bicubic with a reducing gap of two for opaque images
func Thumbnail(img *image.NRGBA, side int, hasAlpha bool) *image.NRGBA {
	w, h := img.Rect.Dx(), img.Rect.Dy()
	fw, fh, ok := ThumbnailSize(w, h, side)
	if !ok || (fw == w && fh == h) {
		out := image.NewNRGBA(img.Rect)
		copy(out.Pix, img.Pix)
		return out
	}
	if hasAlpha {
		pre := premultiply(img)
		res := pilResample(pre, fw, fh, [4]float64{0, 0, float64(w), float64(h)})
		return unpremultiply(res)
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

// Block averages over fx by fy cells, partial edge cells averaged over the pixels they hold
func pilReduce(img *image.NRGBA, fx, fy int) *image.NRGBA {
	w, h := img.Rect.Dx(), img.Rect.Dy()
	ow, oh := (w+fx-1)/fx, (h+fy-1)/fy
	out := image.NewNRGBA(image.Rect(0, 0, ow, oh))
	for oy := 0; oy < oh; oy++ {
		for ox := 0; ox < ow; ox++ {
			var sum [4]int
			count := 0
			for y := oy * fy; y < min((oy+1)*fy, h); y++ {
				for x := ox * fx; x < min((ox+1)*fx, w); x++ {
					i := img.PixOffset(x, y)
					for c := 0; c < 4; c++ {
						sum[c] += int(img.Pix[i+c])
					}
					count++
				}
			}
			o := out.PixOffset(ox, oy)
			for c := 0; c < 4; c++ {
				out.Pix[o+c] = uint8((sum[c] + count/2) / count)
			}
		}
	}
	return out
}

// Pillow bicubic kernel with a of minus one half
func bicubicFilter(x float64) float64 {
	const a = -0.5
	if x < 0 {
		x = -x
	}
	if x < 1 {
		return ((a+2)*x-(a+3))*x*x + 1
	}
	if x < 2 {
		return (((x-5)*x+8)*x - 4) * a
	}
	return 0
}

const pilPrecisionBits = 32 - 8 - 2

// pilFilter is one Pillow resampling kernel with the radius it reaches
type pilFilter struct {
	kernel  func(float64) float64
	support float64
}

var (
	pilBicubic = pilFilter{bicubicFilter, 2.0}
	pilLanczos = pilFilter{lanczosFilter, 3.0}
)

// Pillow truncated sinc kernel over three lobes
func lanczosFilter(x float64) float64 {
	sinc := func(v float64) float64 {
		if v == 0 {
			return 1
		}
		v *= math.Pi
		return math.Sin(v) / v
	}
	if -3.0 <= x && x < 3.0 {
		return sinc(x) * sinc(x/3)
	}
	return 0
}

// Fixed point kernel weights and source bounds per output index over the box span
func pilCoeffs(inSize int, in0, in1 float64, outSize int, filter pilFilter) ([]int, [][]int32) {
	scale := (in1 - in0) / float64(outSize)
	filterscale := max(scale, 1.0)
	support := filter.support * filterscale
	bounds := make([]int, 2*outSize)
	weights := make([][]int32, outSize)
	for xx := 0; xx < outSize; xx++ {
		center := in0 + (float64(xx)+0.5)*scale
		ss := 1.0 / filterscale
		xmin := max(int(center-support+0.5), 0)
		xmax := min(int(center+support+0.5), inSize) - xmin
		k := make([]float64, max(xmax, 0))
		ww := 0.0
		for x := range k {
			k[x] = filter.kernel((float64(x+xmin) - center + 0.5) * ss)
			ww += k[x]
		}
		fixed := make([]int32, len(k))
		for x := range k {
			if ww != 0 {
				k[x] /= ww
			}
			if k[x] < 0 {
				fixed[x] = int32(-0.5 + k[x]*(1<<pilPrecisionBits))
			} else {
				fixed[x] = int32(0.5 + k[x]*(1<<pilPrecisionBits))
			}
		}
		bounds[2*xx], bounds[2*xx+1] = xmin, xmax
		weights[xx] = fixed
	}
	return bounds, weights
}

// Fixed point accumulator clipped to one byte
func pilClip8(in int64) uint8 {
	if in >= 1<<pilPrecisionBits<<8 {
		return 255
	}
	if in <= 0 {
		return 0
	}
	return uint8(in >> pilPrecisionBits)
}

// Two pass Pillow resample of a box of the source, horizontal pass first over the rows the vertical pass reads
func pilResample(img *image.NRGBA, xsize, ysize int, box [4]float64) *image.NRGBA {
	return pilResampleWith(img, xsize, ysize, box, pilBicubic)
}

// Resize copies an RGBA image to w by h with the Lanczos kernel the way Image.resize does for alpha images
func ResizeLanczos(img *image.NRGBA, w, h int) *image.NRGBA {
	inW, inH := img.Rect.Dx(), img.Rect.Dy()
	if w == inW && h == inH {
		out := image.NewNRGBA(image.Rect(0, 0, inW, inH))
		for y := 0; y < inH; y++ {
			copy(out.Pix[y*out.Stride:y*out.Stride+4*inW], img.Pix[y*img.Stride:y*img.Stride+4*inW])
		}
		return out
	}
	return unpremultiply(pilResampleWith(premultiply(img), w, h, [4]float64{0, 0, float64(inW), float64(inH)}, pilLanczos))
}

// Two pass Pillow resample of a box of the source with the given kernel
func pilResampleWith(img *image.NRGBA, xsize, ysize int, box [4]float64, filter pilFilter) *image.NRGBA {
	inW, inH := img.Rect.Dx(), img.Rect.Dy()
	needH := xsize != inW || box[0] != 0 || box[2] != float64(xsize)
	needV := ysize != inH || box[1] != 0 || box[3] != float64(ysize)
	hb, hw := pilCoeffs(inW, box[0], box[2], xsize, filter)
	vb, vw := pilCoeffs(inH, box[1], box[3], ysize, filter)
	yFirst, yLast := vb[0], vb[2*ysize-2]+vb[2*ysize-1]
	cur := img
	if needH {
		for i := 0; i < ysize; i++ {
			vb[2*i] -= yFirst
		}
		tmp := image.NewNRGBA(image.Rect(0, 0, xsize, yLast-yFirst))
		for y := 0; y < tmp.Rect.Dy(); y++ {
			for xx := 0; xx < xsize; xx++ {
				xmin, n := hb[2*xx], hb[2*xx+1]
				var acc [4]int64
				for c := range acc {
					acc[c] = 1 << (pilPrecisionBits - 1)
				}
				for x := 0; x < n; x++ {
					i := cur.PixOffset(x+xmin, y+yFirst)
					k := int64(hw[xx][x])
					for c := 0; c < 4; c++ {
						acc[c] += int64(cur.Pix[i+c]) * k
					}
				}
				o := tmp.PixOffset(xx, y)
				for c := 0; c < 4; c++ {
					tmp.Pix[o+c] = pilClip8(acc[c])
				}
			}
		}
		cur = tmp
	}
	if needV {
		out := image.NewNRGBA(image.Rect(0, 0, xsize, ysize))
		for yy := 0; yy < ysize; yy++ {
			ymin, n := vb[2*yy], vb[2*yy+1]
			for x := 0; x < xsize; x++ {
				var acc [4]int64
				for c := range acc {
					acc[c] = 1 << (pilPrecisionBits - 1)
				}
				for y := 0; y < n; y++ {
					i := cur.PixOffset(x, y+ymin)
					k := int64(vw[yy][y])
					for c := 0; c < 4; c++ {
						acc[c] += int64(cur.Pix[i+c]) * k
					}
				}
				o := out.PixOffset(x, yy)
				for c := 0; c < 4; c++ {
					out.Pix[o+c] = pilClip8(acc[c])
				}
			}
		}
		cur = out
	}
	if cur == img {
		out := image.NewNRGBA(img.Rect)
		copy(out.Pix, img.Pix)
		return out
	}
	return cur
}

// Pillow RGBA to RGBa premultiplication with its rounded divide by 255
func premultiply(img *image.NRGBA) *image.NRGBA {
	out := image.NewNRGBA(img.Rect)
	for i := 0; i < len(img.Pix); i += 4 {
		a := int(img.Pix[i+3])
		for c := 0; c < 3; c++ {
			t := int(img.Pix[i+c])*a + 128
			out.Pix[i+c] = uint8((t + (t >> 8)) >> 8)
		}
		out.Pix[i+3] = img.Pix[i+3]
	}
	return out
}

// Pillow RGBa to RGBA conversion leaving fully opaque and fully clear pixels untouched
func unpremultiply(img *image.NRGBA) *image.NRGBA {
	for i := 0; i < len(img.Pix); i += 4 {
		a := int(img.Pix[i+3])
		if a == 255 || a == 0 {
			continue
		}
		for c := 0; c < 3; c++ {
			img.Pix[i+c] = uint8(min(255*int(img.Pix[i+c])/a, 255))
		}
	}
	return img
}
