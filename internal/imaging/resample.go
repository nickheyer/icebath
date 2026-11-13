package imaging

import (
	"image"
	"math"
)

// Resampling kernel of a resize
type Filter uint8

// Kernels the resizes use
const (
	// Source pixel under the centre of each output pixel
	Nearest Filter = iota
	// Triangle kernel of radius one
	Bilinear
	// Cubic convolution with a of minus one half and radius two
	Bicubic
	// Three lobe windowed sinc
	Lanczos
)

// Kernel function and radius of a filter
type kernel struct {
	fn      func(float64) float64
	support float64
}

// Kernel behind a filter other than nearest
func (f Filter) kernel() kernel {
	switch f {
	case Bilinear:
		return kernel{func(x float64) float64 {
			x = math.Abs(x)
			if x < 1 {
				return 1 - x
			}
			return 0
		}, 1}
	case Bicubic:
		return kernel{func(x float64) float64 {
			const a = -0.5
			x = math.Abs(x)
			if x < 1 {
				return ((a+2)*x-(a+3))*x*x + 1
			}
			if x < 2 {
				return (((x-5)*x+8)*x - 4) * a
			}
			return 0
		}, 2}
	}
	return kernel{func(x float64) float64 {
		if -3 <= x && x < 3 {
			return sinc(x) * sinc(x/3)
		}
		return 0
	}, 3}
}

// Normalised cardinal sine
func sinc(x float64) float64 {
	if x == 0 {
		return 1
	}
	x *= math.Pi
	return math.Sin(x) / x
}

// Fixed point precision of eight bit resampling weights
const precisionBits = 32 - 8 - 2

// Source window and normalised weights for every output index along one axis
type coeffs struct {
	ksize  int
	bounds []int
	kk     []float64
}

// Windows over the span of the source with the kernel widened by the downscale factor for antialiasing
func precomputeCoeffs(inSize int, in0, in1 float64, outSize int, k kernel) coeffs {
	scale := (in1 - in0) / float64(outSize)
	filterscale := max(scale, 1.0)
	support := k.support * filterscale
	ksize := int(math.Ceil(support))*2 + 1
	c := coeffs{ksize: ksize, bounds: make([]int, outSize*2), kk: make([]float64, outSize*ksize)}
	for xx := 0; xx < outSize; xx++ {
		center := in0 + (float64(xx)+0.5)*scale
		ss := 1.0 / filterscale
		xmin := max(int(center-support+0.5), 0)
		xmax := min(int(center+support+0.5), inSize) - xmin
		row := c.kk[xx*ksize : (xx+1)*ksize]
		ww := 0.0
		for x := 0; x < xmax; x++ {
			w := k.fn((float64(x+xmin) - center + 0.5) * ss)
			row[x] = w
			ww += w
		}
		for x := 0; x < xmax; x++ {
			if ww != 0 {
				row[x] /= ww
			}
		}
		c.bounds[xx*2], c.bounds[xx*2+1] = xmin, xmax
	}
	return c
}

// Weights as integers of 22 fraction bits, rounded away from zero
func (c coeffs) fixed() []int32 {
	out := make([]int32, len(c.kk))
	for i, v := range c.kk {
		if v < 0 {
			out[i] = int32(-0.5 + v*(1<<precisionBits))
		} else {
			out[i] = int32(0.5 + v*(1<<precisionBits))
		}
	}
	return out
}

// Fixed point accumulator clamped back to one byte
func clip8(in int) uint8 {
	if in >= 1<<precisionBits<<8 {
		return 255
	}
	if in <= 0 {
		return 0
	}
	return uint8(in >> precisionBits)
}

// Interleaved eight bit pixels of one layout, bpp bytes per pixel of which nch are resampled
type plane struct {
	pix      []uint8
	w, h     int
	bpp, nch int
}

// Two pass resampling of a source box, horizontal first over just the rows the vertical pass reads
func (p plane) resample(dw, dh int, box [4]float64, k kernel) plane {
	needH := dw != p.w || box[0] != 0 || box[2] != float64(dw)
	needV := dh != p.h || box[1] != 0 || box[3] != float64(dh)
	horiz := precomputeCoeffs(p.w, box[0], box[2], dw, k)
	vert := precomputeCoeffs(p.h, box[1], box[3], dh, k)
	cur := p
	if needH {
		yFirst := vert.bounds[0]
		yLast := vert.bounds[dh*2-2] + vert.bounds[dh*2-1]
		for i := 0; i < dh; i++ {
			vert.bounds[i*2] -= yFirst
		}
		kk := horiz.fixed()
		tmp := plane{pix: make([]uint8, dw*(yLast-yFirst)*p.bpp), w: dw, h: yLast - yFirst, bpp: p.bpp, nch: p.nch}
		for yy := 0; yy < tmp.h; yy++ {
			src := p.pix[(yy+yFirst)*p.w*p.bpp:]
			dst := tmp.pix[yy*dw*p.bpp:]
			for xx := 0; xx < dw; xx++ {
				xmin, xmax := horiz.bounds[xx*2], horiz.bounds[xx*2+1]
				w := kk[xx*horiz.ksize:]
				for ch := 0; ch < p.nch; ch++ {
					ss := 1 << (precisionBits - 1)
					for x := 0; x < xmax; x++ {
						ss += int(src[(x+xmin)*p.bpp+ch]) * int(w[x])
					}
					dst[xx*p.bpp+ch] = clip8(ss)
				}
			}
		}
		cur = tmp
	}
	if needV {
		kk := vert.fixed()
		out := plane{pix: make([]uint8, cur.w*dh*p.bpp), w: cur.w, h: dh, bpp: p.bpp, nch: p.nch}
		for yy := 0; yy < dh; yy++ {
			ymin, ymax := vert.bounds[yy*2], vert.bounds[yy*2+1]
			w := kk[yy*vert.ksize:]
			dst := out.pix[yy*cur.w*p.bpp:]
			for xx := 0; xx < cur.w; xx++ {
				for ch := 0; ch < p.nch; ch++ {
					ss := 1 << (precisionBits - 1)
					for y := 0; y < ymax; y++ {
						ss += int(cur.pix[((y+ymin)*cur.w+xx)*p.bpp+ch]) * int(w[y])
					}
					dst[xx*p.bpp+ch] = clip8(ss)
				}
			}
		}
		cur = out
	}
	if !needH && !needV {
		cur.pix = append([]uint8(nil), p.pix...)
	}
	return cur
}

// Block average over factor cells inside a box, partial edge cells averaged over the pixels they hold
func (p plane) reduce(fx, fy int, box image.Rectangle) plane {
	bw, bh := box.Dx(), box.Dy()
	ow, oh := (bw+fx-1)/fx, (bh+fy-1)/fy
	out := plane{pix: make([]uint8, ow*oh*p.bpp), w: ow, h: oh, bpp: p.bpp, nch: p.nch}
	for oy := 0; oy < oh; oy++ {
		y0, y1 := box.Min.Y+oy*fy, min(box.Min.Y+(oy+1)*fy, box.Max.Y)
		for ox := 0; ox < ow; ox++ {
			x0, x1 := box.Min.X+ox*fx, min(box.Min.X+(ox+1)*fx, box.Max.X)
			count := uint32((y1 - y0) * (x1 - x0))
			mult := reduceMultiplier(count)
			for ch := 0; ch < p.nch; ch++ {
				ss := count / 2
				for y := y0; y < y1; y++ {
					row := p.pix[y*p.w*p.bpp:]
					for x := x0; x < x1; x++ {
						ss += uint32(row[x*p.bpp+ch])
					}
				}
				out.pix[(oy*ow+ox)*p.bpp+ch] = uint8((ss * mult) >> 24)
			}
		}
	}
	return out
}

// Single precision reciprocal of the cell size scaled to twenty four fraction bits
func reduceMultiplier(divider uint32) uint32 {
	maxDividend := float32(256 * divider)
	return uint32(float32(1<<30*4.0) / maxDividend)
}

// Nearest neighbour sampling of a source box by the centre of each output pixel
func (p plane) nearest(dw, dh int, box [4]float64) plane {
	out := plane{pix: make([]uint8, dw*dh*p.bpp), w: dw, h: dh, bpp: p.bpp, nch: p.nch}
	ax := (box[2] - box[0]) / float64(dw)
	ay := (box[3] - box[1]) / float64(dh)
	coord := func(v float64) int {
		if v < 0 {
			return -1
		}
		return int(v)
	}
	xin := make([]int, dw)
	xo := box[0] + ax*0.5
	for x := 0; x < dw; x++ {
		xin[x] = coord(xo)
		xo += ax
	}
	yo := box[1] + ay*0.5
	for y := 0; y < dh; y++ {
		yi := coord(yo)
		yo += ay
		if yi < 0 || yi >= p.h {
			continue
		}
		src := p.pix[yi*p.w*p.bpp:]
		dst := out.pix[y*dw*p.bpp:]
		for x := 0; x < dw; x++ {
			if xin[x] >= 0 && xin[x] < p.w {
				copy(dst[x*p.bpp:(x+1)*p.bpp], src[xin[x]*p.bpp:(xin[x]+1)*p.bpp])
			}
		}
	}
	return out
}

// Pixel arrangement a resize works in for one image
type layout uint8

// Layouts by how a resize treats their channels
const (
	layoutPalette layout = iota
	layoutGray
	layoutGray16
	layoutRGB
	layoutRGBA
	layoutCMYK
)

// Layout of an image and its pixels as one interleaved plane, alpha left straight
func toPlane(img image.Image) (layout, plane) {
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	switch m := img.(type) {
	case *image.Paletted:
		pix := make([]uint8, w*h)
		for y := 0; y < h; y++ {
			copy(pix[y*w:(y+1)*w], m.Pix[m.PixOffset(b.Min.X, b.Min.Y+y):])
		}
		return layoutPalette, plane{pix: pix, w: w, h: h, bpp: 1, nch: 1}
	case *image.Gray:
		pix := make([]uint8, w*h)
		for y := 0; y < h; y++ {
			copy(pix[y*w:(y+1)*w], m.Pix[m.PixOffset(b.Min.X, b.Min.Y+y):])
		}
		return layoutGray, plane{pix: pix, w: w, h: h, bpp: 1, nch: 1}
	case *image.Gray16:
		pix := make([]uint8, w*h*2)
		for y := 0; y < h; y++ {
			copy(pix[y*w*2:(y+1)*w*2], m.Pix[m.PixOffset(b.Min.X, b.Min.Y+y):])
		}
		return layoutGray16, plane{pix: pix, w: w, h: h, bpp: 2, nch: 1}
	case *image.CMYK:
		pix := make([]uint8, w*h*4)
		for y := 0; y < h; y++ {
			copy(pix[y*w*4:(y+1)*w*4], m.Pix[m.PixOffset(b.Min.X, b.Min.Y+y):])
		}
		return layoutCMYK, plane{pix: pix, w: w, h: h, bpp: 4, nch: 4}
	}
	if hasAlpha(img) {
		n := ToNRGBA(img)
		return layoutRGBA, plane{pix: n.Pix, w: w, h: h, bpp: 4, nch: 4}
	}
	rgb := ToRGB(img)
	return layoutRGB, plane{pix: rgb.Pix, w: w, h: h, bpp: 4, nch: 3}
}

// Image of the layout holding the plane, the palette taken from the source
func (l layout) image(p plane, src image.Image) image.Image {
	r := image.Rect(0, 0, p.w, p.h)
	switch l {
	case layoutPalette:
		out := image.NewPaletted(r, src.(*image.Paletted).Palette)
		copy(out.Pix, p.pix)
		return out
	case layoutGray:
		return &image.Gray{Pix: p.pix, Stride: p.w, Rect: r}
	case layoutGray16:
		return &image.Gray16{Pix: p.pix, Stride: p.w * 2, Rect: r}
	case layoutCMYK:
		return &image.CMYK{Pix: p.pix, Stride: p.w * 4, Rect: r}
	case layoutRGBA:
		return &image.NRGBA{Pix: p.pix, Stride: p.w * 4, Rect: r}
	}
	for i := 3; i < len(p.pix); i += 4 {
		p.pix[i] = 255
	}
	return &image.RGBA{Pix: p.pix, Stride: p.w * 4, Rect: r}
}

// Copy scaled to w by h, palettes always by nearest sampling and alpha through premultiplied channels
func Resize(img image.Image, w, h int, f Filter) image.Image {
	b := img.Bounds()
	return resizeBox(img, w, h, f, [4]float64{0, 0, float64(b.Dx()), float64(b.Dy())}, false)
}

// Resize of a source box, optionally block averaging first while the scale leaves a gap of two
func resizeBox(img image.Image, w, h int, f Filter, box [4]float64, gap bool) image.Image {
	l, p := toPlane(img)
	if w <= 0 || h <= 0 {
		return l.image(plane{w: 0, h: 0, bpp: p.bpp, nch: p.nch}, img)
	}
	box = [4]float64{float64(float32(box[0])), float64(float32(box[1])), float64(float32(box[2])), float64(float32(box[3]))}
	if w == p.w && h == p.h && box == [4]float64{0, 0, float64(p.w), float64(p.h)} {
		return l.image(p, img)
	}
	if l == layoutPalette {
		f = Nearest
	}
	if f == Nearest {
		return l.image(p.nearest(w, h, box), img)
	}
	if l == layoutGray16 {
		return l.image(p.resample16(w, h, box, f.kernel()), img)
	}
	if l == layoutRGBA {
		premultiply(p.pix)
		out := p.resample(w, h, box, f.kernel())
		unpremultiply(out.pix)
		return l.image(out, img)
	}
	if gap {
		fx := max(int((box[2]-box[0])/float64(w)/2), 1)
		fy := max(int((box[3]-box[1])/float64(h)/2), 1)
		if fx > 1 || fy > 1 {
			support := f.kernel().support - 0.5
			sx, sy := (box[2]-box[0])/float64(w)*support, (box[3]-box[1])/float64(h)*support
			rb := image.Rect(max(0, int(box[0]-sx)), max(0, int(box[1]-sy)),
				min(p.w, int(math.Ceil(box[2]+sx))), min(p.h, int(math.Ceil(box[3]+sy))))
			p = p.reduce(fx, fy, rb)
			box = [4]float64{
				float64(float32((box[0] - float64(rb.Min.X)) / float64(fx))), float64(float32((box[1] - float64(rb.Min.Y)) / float64(fy))),
				float64(float32((box[2] - float64(rb.Min.X)) / float64(fx))), float64(float32((box[3] - float64(rb.Min.Y)) / float64(fy))),
			}
		}
	}
	return l.image(p.resample(w, h, box, f.kernel()), img)
}

// Premultiplies colour by alpha in place with a rounded divide by 255
func premultiply(pix []uint8) {
	for i := 0; i < len(pix); i += 4 {
		a := uint32(pix[i+3])
		for c := 0; c < 3; c++ {
			pix[i+c] = uint8(mulDiv255(uint32(pix[i+c]), a))
		}
	}
}

// Restores straight colour in place, leaving fully opaque and fully clear pixels untouched
func unpremultiply(pix []uint8) {
	for i := 0; i < len(pix); i += 4 {
		a := int(pix[i+3])
		if a == 255 || a == 0 {
			continue
		}
		for c := 0; c < 3; c++ {
			pix[i+c] = uint8(min(255, 255*int(pix[i+c])/a))
		}
	}
}

// Two pass resample of big endian sixteen bit grey with floating weights, each sample rounded and clamped to sixteen bits
func (p plane) resample16(dw, dh int, box [4]float64, k kernel) plane {
	needH := dw != p.w || box[0] != 0 || box[2] != float64(dw)
	needV := dh != p.h || box[1] != 0 || box[3] != float64(dh)
	horiz := precomputeCoeffs(p.w, box[0], box[2], dw, k)
	vert := precomputeCoeffs(p.h, box[1], box[3], dh, k)
	put := func(dst []uint8, ss float64) {
		v := uint16(max(0, min(65535, math.Round(ss))))
		dst[0], dst[1] = uint8(v>>8), uint8(v)
	}
	sample := func(pix []uint8, i int) float64 { return float64(uint16(pix[i])<<8 | uint16(pix[i+1])) }
	cur := p
	if needH {
		yFirst := vert.bounds[0]
		yLast := vert.bounds[dh*2-2] + vert.bounds[dh*2-1]
		for i := 0; i < dh; i++ {
			vert.bounds[i*2] -= yFirst
		}
		tmp := plane{pix: make([]uint8, dw*(yLast-yFirst)*2), w: dw, h: yLast - yFirst, bpp: 2, nch: 1}
		for yy := 0; yy < tmp.h; yy++ {
			for xx := 0; xx < dw; xx++ {
				xmin, xmax := horiz.bounds[xx*2], horiz.bounds[xx*2+1]
				ss := 0.0
				for x := 0; x < xmax; x++ {
					ss += sample(p.pix, ((yy+yFirst)*p.w+x+xmin)*2) * horiz.kk[xx*horiz.ksize+x]
				}
				put(tmp.pix[(yy*dw+xx)*2:], ss)
			}
		}
		cur = tmp
	}
	if needV {
		out := plane{pix: make([]uint8, cur.w*dh*2), w: cur.w, h: dh, bpp: 2, nch: 1}
		for yy := 0; yy < dh; yy++ {
			ymin, ymax := vert.bounds[yy*2], vert.bounds[yy*2+1]
			for xx := 0; xx < cur.w; xx++ {
				ss := 0.0
				for y := 0; y < ymax; y++ {
					ss += sample(cur.pix, ((y+ymin)*cur.w+xx)*2) * vert.kk[yy*vert.ksize+y]
				}
				put(out.pix[(yy*cur.w+xx)*2:], ss)
			}
		}
		cur = out
	}
	if !needH && !needV {
		cur.pix = append([]uint8(nil), p.pix...)
	}
	return cur
}

// Size fitting inside a box with the aspect kept, ok false when the image already fits
func FitSize(w, h, boxW, boxH int) (int, int, bool) {
	x, y := boxW, boxH
	if x >= w && y >= h {
		return w, h, false
	}
	aspect := float64(w) / float64(h)
	round := func(number float64, key func(float64) float64) int {
		lo, hi := math.Floor(number), math.Ceil(number)
		pick := lo
		if key(hi) < key(lo) {
			pick = hi
		}
		return max(int(pick), 1)
	}
	if float64(x)/float64(y) >= aspect {
		x = round(float64(y)*aspect, func(n float64) float64 { return math.Abs(aspect - n/float64(y)) })
	} else {
		y = round(float64(x)/aspect, func(n float64) float64 {
			if n == 0 {
				return 0
			}
			return math.Abs(aspect - float64(x)/n)
		})
	}
	return x, y, true
}

// Image shrunk into the box keeping aspect, the input itself when it fits, opaque sources block averaged first
func Thumbnail(img image.Image, boxW, boxH int, f Filter) image.Image {
	b := img.Bounds()
	w, h, ok := FitSize(b.Dx(), b.Dy(), boxW, boxH)
	if !ok || (w == b.Dx() && h == b.Dy()) {
		return img
	}
	return resizeBox(img, w, h, f, [4]float64{0, 0, float64(b.Dx()), float64(b.Dy())}, true)
}
