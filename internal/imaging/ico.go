package imaging

import (
	"bytes"
	"encoding/binary"
	"errors"
	"image"
	"io"
	"math"
	"sort"
)

// Directory entry of an icon file
type icoEntry struct {
	w, h   int
	depth  int
	bpp    int
	size   int
	offset int
}

// Directory entries sorted largest area first, lower colour depth first among equal areas
func icoEntries(data []byte) ([]icoEntry, error) {
	if len(data) < 6 || !bytes.HasPrefix(data, []byte{0, 0, 1, 0}) {
		return nil, ErrUnknownFormat
	}
	n := int(binary.LittleEndian.Uint16(data[4:]))
	entries := make([]icoEntry, 0, n)
	for i := 0; i < n; i++ {
		s := data[min(len(data), 6+16*i):min(len(data), 6+16*(i+1))]
		if len(s) < 16 {
			return nil, &CorruptError{Format: ICO, Err: io.ErrUnexpectedEOF}
		}
		e := icoEntry{w: int(s[0]), h: int(s[1]), bpp: int(binary.LittleEndian.Uint16(s[6:])),
			size: int(binary.LittleEndian.Uint32(s[8:])), offset: int(binary.LittleEndian.Uint32(s[12:]))}
		if e.w == 0 {
			e.w = 256
		}
		if e.h == 0 {
			e.h = 256
		}
		switch colors := int(s[2]); {
		case e.bpp != 0:
			e.depth = e.bpp
		case colors > 1:
			e.depth = int(math.Ceil(math.Log2(float64(colors))))
		default:
			e.depth = 256
		}
		entries = append(entries, e)
	}
	if len(entries) == 0 {
		return nil, &CorruptError{Format: ICO, Err: errors.New("ico: no images")}
	}
	sort.SliceStable(entries, func(i, j int) bool { return entries[i].depth < entries[j].depth })
	sort.SliceStable(entries, func(i, j int) bool { return entries[i].w*entries[i].h > entries[j].w*entries[j].h })
	return entries, nil
}

// Decodes the largest icon, bitmap entries taking alpha from 32 bit pixels or the AND mask
func decodeICO(data []byte) (image.Image, pngMeta, error) {
	entries, err := icoEntries(data)
	if err != nil {
		return nil, pngMeta{}, err
	}
	e := entries[0]
	if e.offset >= len(data) {
		return nil, pngMeta{}, &CorruptError{Format: ICO, Err: io.ErrUnexpectedEOF}
	}
	body := data[e.offset:]
	if bytes.HasPrefix(body, pngSignature) {
		return decodePNG(body)
	}
	d, err := readDIB(body, 0)
	if err != nil {
		return nil, pngMeta{}, corrupt(ICO, err)
	}
	h := d.height / 2
	pixelAt := d.paletteAt
	if d.bits <= 8 {
		pixelAt += d.padding * d.colors
	}
	img, err := d.decode(body, pixelAt, h)
	if err != nil {
		return nil, pngMeta{}, corrupt(ICO, err)
	}
	out := ToNRGBA(img)
	w := d.width
	if e.bpp == 32 {
		if pixelAt+w*h*4 > len(body) {
			return nil, pngMeta{}, &CorruptError{Format: ICO, Err: errors.New("ico: not enough image data")}
		}
		for y := 0; y < h; y++ {
			for x := 0; x < w; x++ {
				out.Pix[out.PixOffset(x, y)+3] = body[pixelAt+((h-1-y)*w+x)*4+3]
			}
		}
		return out, pngMeta{}, nil
	}
	padded := w
	if w%32 > 0 {
		padded += 32 - w%32
	}
	total := padded * h / 8
	start := e.offset + e.size - total
	if start < 0 || e.offset+e.size > len(data) || total < 0 {
		return nil, pngMeta{}, &CorruptError{Format: ICO, Err: errors.New("ico: not enough mask data")}
	}
	mask := data[start : e.offset+e.size]
	stride := padded / 8
	for y := 0; y < h; y++ {
		row := mask[(h-1-y)*stride:]
		for x := 0; x < w; x++ {
			a := uint8(255)
			if row[x/8]>>(7-x%8)&1 == 1 {
				a = 0
			}
			out.Pix[out.PixOffset(x, y)+3] = a
		}
	}
	return out, pngMeta{}, nil
}

// Size of the largest icon as it decodes, its pixels loaded since an icon opens only by decoding
func configICO(data []byte) (int, int, error) {
	img, _, err := decodeICO(data)
	if err != nil {
		return 0, 0, err
	}
	return img.Bounds().Dx(), img.Bounds().Dy(), nil
}

// Icon sizes written by default, those larger than the image or 256 dropped
var icoSizes = []int{16, 24, 32, 48, 64, 128, 256}

// Icon of PNG entries fitted by Lanczos into each default size the image reaches, its own size when none
func encodeICO(w io.Writer, img image.Image) error {
	b := img.Bounds()
	var sizes []int
	for _, s := range icoSizes {
		if s <= b.Dx() && s <= b.Dy() {
			sizes = append(sizes, s)
		}
	}
	if len(sizes) == 0 {
		sizes = []int{min(256, max(b.Dx(), b.Dy()))}
	}
	var frames [][]byte
	var dims [][2]int
	for _, s := range sizes {
		t := img
		if fw, fh, ok := FitSize(b.Dx(), b.Dy(), s, s); ok {
			t = resizeBox(img, fw, fh, Lanczos, [4]float64{0, 0, float64(b.Dx()), float64(b.Dy())}, false)
		}
		var buf bytes.Buffer
		if err := EncodePNG(&buf, t, PNGOptions{}); err != nil {
			return err
		}
		frames = append(frames, buf.Bytes())
		dims = append(dims, [2]int{t.Bounds().Dx(), t.Bounds().Dy()})
	}
	head := []byte{0, 0, 1, 0}
	head = binary.LittleEndian.AppendUint16(head, uint16(len(frames)))
	offset := 6 + 16*len(frames)
	for i, f := range frames {
		head = append(head, byte(dims[i][0]&0xff), byte(dims[i][1]&0xff), 0, 0, 0, 0, 32, 0)
		head = binary.LittleEndian.AppendUint32(head, uint32(len(f)))
		head = binary.LittleEndian.AppendUint32(head, uint32(offset))
		offset += len(f)
	}
	if _, err := w.Write(head); err != nil {
		return err
	}
	for _, f := range frames {
		if _, err := w.Write(f); err != nil {
			return err
		}
	}
	return nil
}
