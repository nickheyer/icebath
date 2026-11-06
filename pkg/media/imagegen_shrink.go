package media

import (
	"bytes"
	"compress/zlib"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/gif"
	"image/jpeg"
	"image/png"
	"io"
	"math"
	"strings"

	"github.com/gen2brain/avif"
	"github.com/gen2brain/heic"
	"golang.org/x/image/bmp"
	"golang.org/x/image/tiff"
	"golang.org/x/image/webp"

	"github.com/nickheyer/icebath/pkg/internal/pybase64"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Pillow MAX_IMAGE_PIXELS doubled, the size past which open raises DecompressionBombError
const pilBombPixels = 2 * (1024 * 1024 * 1024 / 4 / 3)

// Decoded raster with the alpha and EXIF orientation facts Pillow reads at open
type shrinkSource struct {
	img         image.Image
	hasAlpha    bool
	orientation int
}

// Re-encodes a data URL image as JPEG or PNG when it has alpha, shrinking the long edge until it fits
func shrinkDataURL(env *tools.Env, u string, budget int) string {
	_, payload, _ := strings.Cut(u, ",")
	data, err := pybase64.B64Decode(payload)
	if err != nil {
		return u
	}
	src, err := openShrinkSource(data)
	if err != nil {
		return u
	}
	img := toNRGBAImage(src.img)
	img = exifTranspose(img, src.orientation)
	mime := "image/jpeg"
	if src.hasAlpha {
		mime = "image/png"
	} else {
		for i := 3; i < len(img.Pix); i += 4 {
			img.Pix[i] = 0xff
		}
	}
	side := max(img.Rect.Dx(), img.Rect.Dy())
	for {
		frame := pilThumbnail(img, side, src.hasAlpha)
		var buf bytes.Buffer
		if src.hasAlpha {
			err = png.Encode(&buf, frame)
		} else {
			err = jpeg.Encode(&buf, frame, &jpeg.Options{Quality: 90})
		}
		if err != nil {
			return u
		}
		out := "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(buf.Bytes())
		if len(out) <= budget || side <= 256 {
			logOf(env).Info(fmt.Sprintf("Downscaled FAL source image to (%d, %d) (%d bytes inline)", frame.Rect.Dx(), frame.Rect.Dy(), len(out)))
			return out
		}
		side = int(float64(side) * 0.75)
	}
}

// Decodes a raster Pillow opens, refusing decompression bombs and formats it cannot read
func openShrinkSource(data []byte) (*shrinkSource, error) {
	var decode func([]byte) (image.Image, error)
	var config func([]byte) (image.Config, error)
	src := &shrinkSource{orientation: 1}
	switch {
	case bytes.HasPrefix(data, []byte("\x89PNG\r\n\x1a\n")):
		decode, config = wrapDecoder(png.Decode), wrapConfig(png.DecodeConfig)
		src.hasAlpha, src.orientation = pngFacts(data)
	case bytes.HasPrefix(data, []byte{0xff, 0xd8}):
		decode, config = wrapDecoder(jpeg.Decode), wrapConfig(jpeg.DecodeConfig)
		src.orientation = jpegOrientation(data)
	case bytes.HasPrefix(data, []byte("GIF87a")) || bytes.HasPrefix(data, []byte("GIF89a")):
		decode, config = wrapDecoder(gif.Decode), wrapConfig(gif.DecodeConfig)
	case bytes.HasPrefix(data, []byte("BM")):
		decode, config = wrapDecoder(bmp.Decode), wrapConfig(bmp.DecodeConfig)
		src.hasAlpha = bmpHasAlpha(data)
	case len(data) >= 12 && string(data[:4]) == "RIFF" && string(data[8:12]) == "WEBP":
		decode, config = wrapDecoder(webp.Decode), wrapConfig(webp.DecodeConfig)
		src.hasAlpha, src.orientation = webpFacts(data)
	case bytes.HasPrefix(data, []byte("II*\x00")) || bytes.HasPrefix(data, []byte("MM\x00*")):
		decode, config = wrapDecoder(tiff.Decode), wrapConfig(tiff.DecodeConfig)
		src.hasAlpha = tiffHasAlpha(data)
		src.orientation = exifOrientation(data)
	case len(data) >= 12 && string(data[4:8]) == "ftyp":
		brand := string(data[8:12])
		switch brand {
		case "avif", "avis":
			decode = func(b []byte) (image.Image, error) { return avif.Decode(bytes.NewReader(b)) }
			config = wrapConfig(avif.DecodeConfig)
		case "heic", "heix", "hevc", "hevx", "heim", "heis", "mif1", "msf1":
			decode, config = wrapDecoder(heic.Decode), wrapConfig(heic.DecodeConfig)
		default:
			return nil, errors.New("cannot identify image file")
		}
		src.hasAlpha = bytes.Contains(data, []byte("urn:mpeg:mpegB:cicp:systems:auxiliary:alpha")) ||
			bytes.Contains(data, []byte("urn:mpeg:hevc:2015:auxid:1"))
	default:
		return nil, errors.New("cannot identify image file")
	}
	cfg, err := config(data)
	if err != nil {
		return nil, err
	}
	if cfg.Width*cfg.Height > pilBombPixels {
		return nil, fmt.Errorf("Image size (%d pixels) exceeds limit of %d pixels, could be decompression bomb DOS attack.", cfg.Width*cfg.Height, pilBombPixels)
	}
	img, err := decode(data)
	if err != nil {
		return nil, err
	}
	if p, ok := img.(*image.Paletted); ok && !src.hasAlpha {
		for _, c := range p.Palette {
			if _, _, _, a := c.RGBA(); a != 0xffff {
				src.hasAlpha = true
				break
			}
		}
	}
	src.img = img
	return src, nil
}

func wrapDecoder(fn func(r io.Reader) (image.Image, error)) func([]byte) (image.Image, error) {
	return func(b []byte) (image.Image, error) { return fn(bytes.NewReader(b)) }
}

func wrapConfig(fn func(r io.Reader) (image.Config, error)) func([]byte) (image.Config, error) {
	return func(b []byte) (image.Config, error) { return fn(bytes.NewReader(b)) }
}

// Alpha by color type or tRNS chunk, orientation from eXIf or the raw exif text profile
func pngFacts(data []byte) (bool, int) {
	hasAlpha, exif, rawProfile := false, []byte(nil), ""
	for off := 8; off+8 <= len(data); {
		n := int(binary.BigEndian.Uint32(data[off:]))
		typ := string(data[off+4 : off+8])
		if n < 0 || off+12+n > len(data) {
			break
		}
		body := data[off+8 : off+8+n]
		switch typ {
		case "IHDR":
			if len(body) >= 10 && (body[9] == 4 || body[9] == 6) {
				hasAlpha = true
			}
		case "tRNS":
			hasAlpha = true
		case "eXIf":
			exif = body
		case "tEXt", "zTXt", "iTXt":
			if key, rest, ok := bytes.Cut(body, []byte{0}); ok && string(key) == "Raw profile type exif" {
				rawProfile = pngTextValue(typ, rest)
			}
		}
		off += 12 + n
	}
	if exif == nil && rawProfile != "" {
		lines := strings.Split(rawProfile, "\n")
		if len(lines) > 3 {
			exif = hexBytes(strings.Join(lines[3:], ""))
		}
	}
	return hasAlpha, exifOrientation(exif)
}

// Text of a PNG text chunk after its keyword, inflating compressed forms
func pngTextValue(typ string, rest []byte) string {
	switch typ {
	case "tEXt":
		return string(rest)
	case "zTXt":
		if len(rest) < 1 {
			return ""
		}
		return string(zlibInflate(rest[1:]))
	}
	if len(rest) < 2 {
		return ""
	}
	compressed := rest[0] == 1
	rest = rest[2:]
	for i := 0; i < 2; i++ {
		_, after, ok := bytes.Cut(rest, []byte{0})
		if !ok {
			return ""
		}
		rest = after
	}
	if compressed {
		return string(zlibInflate(rest))
	}
	return string(rest)
}

// Bytes of a hex string ignoring whitespace, nil when it is not valid hex
func hexBytes(s string) []byte {
	s = strings.Join(strings.Fields(s), "")
	if len(s)%2 != 0 {
		return nil
	}
	out := make([]byte, len(s)/2)
	for i := range out {
		var v byte
		for _, c := range []byte(s[2*i : 2*i+2]) {
			v <<= 4
			switch {
			case c >= '0' && c <= '9':
				v |= c - '0'
			case c >= 'a' && c <= 'f':
				v |= c - 'a' + 10
			case c >= 'A' && c <= 'F':
				v |= c - 'A' + 10
			default:
				return nil
			}
		}
		out[i] = v
	}
	return out
}

// Orientation of the first APP1 Exif segment before the scan data
func jpegOrientation(data []byte) int {
	for off := 2; off+4 <= len(data); {
		if data[off] != 0xff {
			return 1
		}
		marker := data[off+1]
		if marker == 0xd8 || marker == 0x01 || (marker >= 0xd0 && marker <= 0xd7) || marker == 0xff {
			off += 2
			if marker == 0xff {
				off--
			}
			continue
		}
		if marker == 0xda || marker == 0xd9 {
			return 1
		}
		n := int(binary.BigEndian.Uint16(data[off+2:]))
		if n < 2 || off+2+n > len(data) {
			return 1
		}
		seg := data[off+4 : off+2+n]
		if marker == 0xe1 && bytes.HasPrefix(seg, []byte("Exif\x00\x00")) {
			return exifOrientation(seg)
		}
		off += 2 + n
	}
	return 1
}

// Alpha from the VP8X flag or the VP8L alpha bit, orientation from the EXIF chunk
func webpFacts(data []byte) (bool, int) {
	hasAlpha, orientation := false, 1
	for off := 12; off+8 <= len(data); {
		typ := string(data[off : off+4])
		n := int(binary.LittleEndian.Uint32(data[off+4:]))
		if n < 0 || off+8+n > len(data) {
			break
		}
		body := data[off+8 : off+8+n]
		switch typ {
		case "VP8X":
			if len(body) > 0 && body[0]&0x10 != 0 {
				hasAlpha = true
			}
		case "VP8L":
			if len(body) >= 5 && body[0] == 0x2f && binary.LittleEndian.Uint32(body[1:5])&(1<<28) != 0 {
				hasAlpha = true
			}
		case "EXIF":
			orientation = exifOrientation(body)
		}
		off += 8 + n + n%2
	}
	return hasAlpha, orientation
}

// Thirty two bit bitfield BMP whose alpha mask is set
func bmpHasAlpha(data []byte) bool {
	if len(data) < 34 {
		return false
	}
	headerSize := binary.LittleEndian.Uint32(data[14:])
	bpp := binary.LittleEndian.Uint16(data[28:])
	compression := binary.LittleEndian.Uint32(data[30:])
	if bpp != 32 || (compression != 3 && compression != 6) || len(data) < 70 {
		return false
	}
	if headerSize < 56 && compression != 6 {
		return false
	}
	return binary.LittleEndian.Uint32(data[66:]) != 0
}

// IFD0 entries of a TIFF stream as tag to raw entry
func tiffIFD0(t []byte) (binary.ByteOrder, map[uint16][]byte) {
	if len(t) < 8 {
		return nil, nil
	}
	var bo binary.ByteOrder
	switch string(t[:2]) {
	case "II":
		bo = binary.LittleEndian
	case "MM":
		bo = binary.BigEndian
	default:
		return nil, nil
	}
	off := int(bo.Uint32(t[4:8]))
	if off < 0 || off+2 > len(t) {
		return nil, nil
	}
	entries := map[uint16][]byte{}
	count := int(bo.Uint16(t[off:]))
	for i := 0; i < count; i++ {
		e := off + 2 + 12*i
		if e+12 > len(t) {
			break
		}
		entries[bo.Uint16(t[e:])] = t[e : e+12]
	}
	return bo, entries
}

// TIFF whose ExtraSamples tag marks an associated or unassociated alpha channel
func tiffHasAlpha(data []byte) bool {
	bo, entries := tiffIFD0(data)
	e, ok := entries[338]
	if !ok {
		return false
	}
	count := bo.Uint32(e[4:])
	if count == 0 || count > 2 {
		return false
	}
	for i := uint32(0); i < count; i++ {
		if v := bo.Uint16(e[8+2*i:]); v == 1 || v == 2 {
			return true
		}
	}
	return false
}

// Orientation tag of an Exif or TIFF block, one when absent
func exifOrientation(t []byte) int {
	t = bytes.TrimPrefix(t, []byte("Exif\x00\x00"))
	bo, entries := tiffIFD0(t)
	e, ok := entries[0x0112]
	if !ok {
		return 1
	}
	switch bo.Uint16(e[2:]) {
	case 3:
		return int(bo.Uint16(e[8:]))
	case 4:
		return int(bo.Uint32(e[8:]))
	}
	return 1
}

// Straight alpha RGBA copy of any decoded image
func toNRGBAImage(src image.Image) *image.NRGBA {
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

// Pixels rotated or flipped so the EXIF orientation reads as one
func exifTranspose(img *image.NRGBA, orientation int) *image.NRGBA {
	if orientation < 2 || orientation > 8 {
		return img
	}
	w, h := img.Rect.Dx(), img.Rect.Dy()
	dw, dh := w, h
	if orientation >= 5 {
		dw, dh = h, w
	}
	out := image.NewNRGBA(image.Rect(0, 0, dw, dh))
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
			copy(out.Pix[out.PixOffset(x, y):out.PixOffset(x, y)+4], img.Pix[img.PixOffset(sx, sy):img.PixOffset(sx, sy)+4])
		}
	}
	return out
}

// Size fitting a side by side box with the aspect ratio kept, ok false when already inside it
func thumbnailSize(w, h, side int) (int, int, bool) {
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
func pilThumbnail(img *image.NRGBA, side int, hasAlpha bool) *image.NRGBA {
	w, h := img.Rect.Dx(), img.Rect.Dy()
	fw, fh, ok := thumbnailSize(w, h, side)
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

// Fixed point bicubic weights and source bounds per output index over the box span
func pilCoeffs(inSize int, in0, in1 float64, outSize int) ([]int, [][]int32) {
	scale := (in1 - in0) / float64(outSize)
	filterscale := max(scale, 1.0)
	support := 2.0 * filterscale
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
			k[x] = bicubicFilter((float64(x+xmin) - center + 0.5) * ss)
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
	inW, inH := img.Rect.Dx(), img.Rect.Dy()
	needH := xsize != inW || box[0] != 0 || box[2] != float64(xsize)
	needV := ysize != inH || box[1] != 0 || box[3] != float64(ysize)
	hb, hw := pilCoeffs(inW, box[0], box[2], xsize)
	vb, vw := pilCoeffs(inH, box[1], box[3], ysize)
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

// Inflated zlib stream, the bytes read before any corruption
func zlibInflate(b []byte) []byte {
	r, err := zlib.NewReader(bytes.NewReader(b))
	if err != nil {
		return nil
	}
	out, _ := io.ReadAll(r)
	return out
}
