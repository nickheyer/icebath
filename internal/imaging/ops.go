package imaging

import "image"

// Copy of a region given from the top left corner of the image, zero filled where the region leaves it
func Crop(img image.Image, r image.Rectangle) image.Image {
	l, p := toPlane(img)
	w, h := max(0, r.Dx()), max(0, r.Dy())
	out := plane{pix: make([]uint8, w*h*p.bpp), w: w, h: h, bpp: p.bpp, nch: p.nch}
	for y := 0; y < h; y++ {
		sy := r.Min.Y + y
		if sy < 0 || sy >= p.h {
			continue
		}
		x0, x1 := max(r.Min.X, 0), min(r.Max.X, p.w)
		if x1 <= x0 {
			continue
		}
		copy(out.pix[(y*w+x0-r.Min.X)*p.bpp:], p.pix[(sy*p.w+x0)*p.bpp:(sy*p.w+x1)*p.bpp])
	}
	return l.image(out, img)
}

// Composites src over dst in place with its top left corner at the point, clipped to dst
func AlphaComposite(dst, src *image.NRGBA, at image.Point) {
	db, sb := dst.Bounds(), src.Bounds()
	const bits = 7
	for y := 0; y < sb.Dy(); y++ {
		ty := at.Y + y
		if ty < 0 || ty >= db.Dy() {
			continue
		}
		for x := 0; x < sb.Dx(); x++ {
			tx := at.X + x
			if tx < 0 || tx >= db.Dx() {
				continue
			}
			s := src.Pix[src.PixOffset(sb.Min.X+x, sb.Min.Y+y):]
			d := dst.Pix[dst.PixOffset(db.Min.X+tx, db.Min.Y+ty):]
			sa := uint32(s[3])
			if sa == 0 {
				continue
			}
			blend := uint32(d[3]) * (255 - sa)
			outa255 := sa*255 + blend
			coef1 := sa * 255 * 255 * (1 << bits) / outa255
			coef2 := 255*(1<<bits) - coef1
			for c := 0; c < 3; c++ {
				t := uint32(s[c])*coef1 + uint32(d[c])*coef2 + 0x80<<bits
				d[c] = uint8((((t >> 8) + t) >> 8) >> bits)
			}
			t := outa255 + 0x80
			d[3] = uint8(((t >> 8) + t) >> 8)
		}
	}
}

// Smallest rectangle from the top left corner holding every pixel whose alpha is not zero, false when none is
func AlphaBounds(img *image.NRGBA) (image.Rectangle, bool) {
	b := img.Bounds()
	l, t, r, bt := b.Dx(), b.Dy(), 0, 0
	found := false
	for y := 0; y < b.Dy(); y++ {
		row := img.Pix[img.PixOffset(b.Min.X, b.Min.Y+y):]
		for x := 0; x < b.Dx(); x++ {
			if row[4*x+3] != 0 {
				found = true
				l, t, r, bt = min(l, x), min(t, y), max(r, x+1), max(bt, y+1)
			}
		}
	}
	if !found {
		return image.Rectangle{}, false
	}
	return image.Rect(l, t, r, bt), true
}

// Mirror image across the vertical axis
func FlipHorizontal(img image.Image) image.Image { return transpose(img, 2) }

// Rotation or flip an EXIF orientation value calls for, the image itself for one and unknown values
func transpose(img image.Image, orientation int) image.Image {
	if orientation < 2 || orientation > 8 {
		return img
	}
	l, p := toPlane(img)
	w, h := p.w, p.h
	dw, dh := w, h
	if orientation >= 5 {
		dw, dh = h, w
	}
	out := plane{pix: make([]uint8, dw*dh*p.bpp), w: dw, h: dh, bpp: p.bpp, nch: p.nch}
	for y := 0; y < dh; y++ {
		for x := 0; x < dw; x++ {
			var sx, sy int
			switch orientation {
			case 2:
				sx, sy = w-1-x, y
			case 3:
				sx, sy = w-1-x, h-1-y
			case 4:
				sx, sy = x, h-1-y
			case 5:
				sx, sy = y, x
			case 6:
				sx, sy = y, h-1-x
			case 7:
				sx, sy = w-1-y, h-1-x
			case 8:
				sx, sy = w-1-y, x
			}
			copy(out.pix[(y*dw+x)*p.bpp:(y*dw+x+1)*p.bpp], p.pix[(sy*w+sx)*p.bpp:(sy*w+sx+1)*p.bpp])
		}
	}
	return l.image(out, img)
}

// Minimum over a size by size window with edges replicated, the size rounded down to odd
func MinFilter(img *image.Gray, size int) *image.Gray {
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	half := max(size, 1) / 2
	out := image.NewGray(image.Rect(0, 0, w, h))
	at := func(x, y int) uint8 {
		x, y = max(0, min(w-1, x)), max(0, min(h-1, y))
		return img.Pix[img.PixOffset(b.Min.X+x, b.Min.Y+y)]
	}
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			m := uint8(255)
			for dy := -half; dy <= half; dy++ {
				for dx := -half; dx <= half; dx++ {
					m = min(m, at(x+dx, y+dy))
				}
			}
			out.Pix[y*out.Stride+x] = m
		}
	}
	return out
}
