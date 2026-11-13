package imaging

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"io"

	"golang.org/x/image/tiff"
)

// TIFF tags the decoder reads itself
const (
	tagWidth           = 256
	tagLength          = 257
	tagCompression     = 259
	tagPhotometric     = 262
	tagStripOffsets    = 273
	tagOrientation     = 274
	tagRowsPerStrip    = 278
	tagStripByteCounts = 279
	tagXMP             = 700
	tagTileWidth       = 322
	tagTileLength      = 323
	tagTileOffsets     = 324
	tagTileByteCounts  = 325
	tagExtraSamples    = 338
	tagJPEGTables      = 347
	tagJPEGIF          = 513
	tagJPEGIFLength    = 514
	tagICC             = 34675
)

// Walk over the directory chain of a TIFF that stops at a loop or a broken directory
type tiffChain struct {
	r    *tiffReader
	next int
	seen map[int]bool
}

// Chain starting at the directory the header points at
func newTIFFChain(r *tiffReader) *tiffChain {
	return &tiffChain{r: r, next: r.first(), seen: map[int]bool{}}
}

// Offset of the following directory, false past the last one
func (c *tiffChain) page() (int, bool) {
	off := c.next
	if off == 0 || c.seen[off] {
		return 0, false
	}
	c.seen[off] = true
	_, next, ok := c.r.ifd(off)
	if !ok {
		return 0, false
	}
	c.next = next
	return off, true
}

// TIFF bytes served as though the header pointed at one directory, the data itself never copied
type tiffPage struct {
	data   []byte
	header [8]byte
	pos    int64
}

// View of the file whose header points at the directory at an offset
func newTIFFPage(r *tiffReader, off int) *tiffPage {
	p := &tiffPage{data: r.data}
	copy(p.header[:], r.data[:8])
	r.order.PutUint32(p.header[4:8], uint32(off))
	return p
}

// Copies bytes from an offset with the patched header in front
func (p *tiffPage) ReadAt(b []byte, off int64) (int, error) {
	if off < 0 {
		return 0, errors.New("tiff: negative offset")
	}
	if off >= int64(len(p.data)) {
		return 0, io.EOF
	}
	n := copy(b, p.data[off:])
	if off < 8 {
		copy(b, p.header[off:min(8, off+int64(n))])
	}
	if n < len(b) {
		return n, io.EOF
	}
	return n, nil
}

// Reads sequentially through the patched view
func (p *tiffPage) Read(b []byte) (int, error) {
	n, err := p.ReadAt(b, p.pos)
	p.pos += int64(n)
	return n, err
}

// Tags the TIFF library reads from a directory, whose values it loads at their declared length
var tiffLibraryTags = map[uint16]bool{256: true, 257: true, 258: true, 259: true, 262: true, 266: true, 273: true, 277: true, 278: true,
	279: true, 284: true, 292: true, 293: true, 317: true, 320: true, 322: true, 323: true, 324: true, 325: true, 338: true, 339: true}

// Reports whether every value the TIFF library would load from the directory lies inside the file
func (r *tiffReader) valuesInside(off int) bool {
	n := int(r.order.Uint16(r.data[off:]))
	for i := 0; i < n && off+2+12*i+12 <= len(r.data); i++ {
		e := off + 2 + 12*i
		entry := r.entry(e)
		if tiffLibraryTags[entry.tag] && tiffTypeSize(entry.typ)*int(entry.count) > 4 && r.bytesOf(entry) == nil {
			return false
		}
	}
	return true
}

// Decodes one page of a TIFF, JPEG compressed pages through the JPEG decoder strip by strip
func decodeTIFFPage(r *tiffReader, off int) (image.Image, error) {
	entries, _, ok := r.ifd(off)
	if !ok || !r.valuesInside(off) {
		return nil, &CorruptError{Format: TIFF, Err: errors.New("tiff: broken directory")}
	}
	if err := checkBomb(r.largest(off, tagWidth), r.largest(off, tagLength)); err != nil {
		return nil, err
	}
	w, _ := r.int1(entries, tagWidth)
	h, _ := r.int1(entries, tagLength)
	var img image.Image
	var err error
	switch c, _ := r.int1(entries, tagCompression); c {
	case 6:
		img, err = tiffOldJPEG(r, entries, w, h)
	case 7:
		img, err = tiffJPEG(r, entries, w, h)
	default:
		img, err = tiff.Decode(newTIFFPage(r, off))
		var ue tiff.UnsupportedError
		if errors.As(err, &ue) {
			err = fmt.Errorf("%w: tiff %s", ErrUnsupported, string(ue))
		}
	}
	if err != nil {
		return nil, corrupt(TIFF, err)
	}
	if extra := r.ints(entries[tagExtraSamples]); len(extra) > 0 && (extra[0] == 1 || extra[0] == 2) {
		return ToNRGBA(img), nil
	}
	return normalizeDecoded(img), nil
}

// Old style JPEG page whose interchange format offset holds a whole JPEG stream covering the page, cropped to it
func tiffOldJPEG(r *tiffReader, entries map[uint16]ifdEntry, w, h int) (image.Image, error) {
	at, ok1 := r.int1(entries, tagJPEGIF)
	n, ok2 := r.int1(entries, tagJPEGIFLength)
	if !ok1 || !ok2 || at < 0 || at+n > len(r.data) || n <= 0 {
		return nil, fmt.Errorf("%w: tiff old style JPEG without an interchange stream", ErrUnsupported)
	}
	stream := r.data[at : at+n]
	cfg, err := jpegConfig(stream)
	if err != nil {
		return nil, err
	}
	if cfg.Width < w || cfg.Height < h {
		return nil, errors.New("tiff: JPEG stream smaller than the page")
	}
	img, err := decodeJPEGStream(stream)
	if err != nil || (cfg.Width == w && cfg.Height == h) {
		return img, err
	}
	return Crop(img, image.Rect(0, 0, w, h)), nil
}

// Bytes a shared JPEG table set takes at most, four sixteen bit quantization tables and four full Huffman tables
const jpegTablesMax = 2 + 4*(4+1+128) + 4*(4+1+16+256) + 2

// New style JPEG page decoded strip or tile wise with the shared tables spliced into each stream
func tiffJPEG(r *tiffReader, entries map[uint16]ifdEntry, w, h int) (image.Image, error) {
	tables := r.bytesOf(entries[tagJPEGTables])
	offsets, counts := r.ints(entries[tagStripOffsets]), r.ints(entries[tagStripByteCounts])
	tw, th := w, 0
	if tile := r.ints(entries[tagTileOffsets]); len(tile) > 0 {
		offsets, counts = tile, r.ints(entries[tagTileByteCounts])
		tw, _ = r.int1(entries, tagTileWidth)
		th, _ = r.int1(entries, tagTileLength)
	} else {
		th, _ = r.int1(entries, tagRowsPerStrip)
		if th <= 0 || th > h {
			th = h
		}
	}
	if len(offsets) == 0 || len(offsets) != len(counts) || tw <= 0 || th <= 0 {
		return nil, errors.New("tiff: missing JPEG strips")
	}
	if len(tables) > jpegTablesMax {
		return nil, errors.New("tiff: JPEG tables larger than any table set")
	}
	rgb, _ := r.int1(entries, tagPhotometric)
	across := (w + tw - 1) / tw
	var canvas draw.Image
	var spliced []byte
	for i, off := range offsets {
		n := counts[i]
		if off < 0 || n <= 0 || off+n > len(r.data) {
			return nil, errors.New("tiff: JPEG strip outside the file")
		}
		seg := r.data[off : off+n]
		stream := seg
		if len(tables) > 4 && bytes.HasPrefix(seg, []byte{0xff, 0xd8}) {
			spliced = append(append(spliced[:0], tables[:len(tables)-2]...), seg[2:]...)
			stream = spliced
		}
		cfg, err := jpegConfig(stream)
		if err != nil {
			return nil, err
		}
		if cfg.Width > tw || cfg.Height > th {
			return nil, errors.New("tiff: JPEG strip larger than its tile")
		}
		part, err := decodeJPEGStream(stream)
		if err != nil {
			return nil, err
		}
		if yc, ok := part.(*image.YCbCr); ok && rgb == 2 {
			part = ycbcrPlanesAsRGB(yc)
		}
		if canvas == nil {
			switch part.(type) {
			case *image.Gray:
				canvas = image.NewGray(image.Rect(0, 0, w, h))
			case *image.CMYK:
				canvas = image.NewCMYK(image.Rect(0, 0, w, h))
			default:
				canvas = image.NewRGBA(image.Rect(0, 0, w, h))
			}
		}
		at := image.Pt((i%across)*tw, (i/across)*th)
		draw.Draw(canvas, image.Rectangle{Min: at, Max: at.Add(part.Bounds().Size())}.Intersect(canvas.Bounds()), part, part.Bounds().Min, draw.Src)
	}
	return canvas, nil
}

// RGB image of a JPEG whose three components hold red, green and blue rather than luma and chroma
func ycbcrPlanesAsRGB(yc *image.YCbCr) *image.RGBA {
	b := yc.Bounds()
	out := image.NewRGBA(image.Rect(0, 0, b.Dx(), b.Dy()))
	for y := 0; y < b.Dy(); y++ {
		for x := 0; x < b.Dx(); x++ {
			yi, ci := yc.YOffset(b.Min.X+x, b.Min.Y+y), yc.COffset(b.Min.X+x, b.Min.Y+y)
			o := out.PixOffset(x, y)
			out.Pix[o], out.Pix[o+1], out.Pix[o+2], out.Pix[o+3] = yc.Y[yi], yc.Cb[ci], yc.Cr[ci], 255
		}
	}
	return out
}

// Decodes the first page of a TIFF with its orientation tag, ICC profile and XMP packet
func decodeTIFF(data []byte) (image.Image, int, []byte, error) {
	r, ok := newTIFFReader(data)
	if !ok {
		return nil, 0, nil, ErrUnknownFormat
	}
	first, ok := newTIFFChain(r).page()
	if !ok {
		return nil, 0, nil, &CorruptError{Format: TIFF, Err: errors.New("tiff: no image directory")}
	}
	img, err := decodeTIFFPage(r, first)
	if err != nil {
		return nil, 0, nil, err
	}
	entries, _, _ := r.ifd(first)
	orientation := 1
	if v, ok := r.int1(entries, tagOrientation); ok {
		orientation = v
	} else if m := xmpOrientation.FindSubmatch(r.bytesOf(entries[tagXMP])); m != nil {
		orientation = int(m[2][0] - '0')
	}
	return img, orientation, r.bytesOf(entries[tagICC]), nil
}

// Size of the first page of a TIFF
func configTIFF(data []byte) (int, int, error) {
	r, ok := newTIFFReader(data)
	if !ok {
		return 0, 0, ErrUnknownFormat
	}
	first, ok := newTIFFChain(r).page()
	if !ok {
		return 0, 0, &CorruptError{Format: TIFF, Err: errors.New("tiff: no image directory")}
	}
	entries, _, _ := r.ifd(first)
	w, _ := r.int1(entries, tagWidth)
	h, _ := r.int1(entries, tagLength)
	return w, h, checkBomb(r.largest(first, tagWidth), r.largest(first, tagLength))
}

// Frame source over the pages of a TIFF, walked one directory at a time
type tiffFrames struct {
	r     *tiffReader
	chain *tiffChain
	page  int
}

// Frame source after the header parses
func newTIFFFrames(data []byte) (*tiffFrames, error) {
	if _, _, err := configTIFF(data); err != nil {
		return nil, err
	}
	r, _ := newTIFFReader(data)
	return &tiffFrames{r: r, chain: newTIFFChain(r)}, nil
}

// Moves to the following page at its own size
func (t *tiffFrames) next() (int, int, bool, error) {
	off, ok := t.chain.page()
	if !ok {
		return 0, 0, false, nil
	}
	t.page = off
	entries, _, _ := t.r.ifd(off)
	w, _ := t.r.int1(entries, tagWidth)
	h, _ := t.r.int1(entries, tagLength)
	return w, h, true, nil
}

// Decodes the current page
func (t *tiffFrames) decode() error {
	_, err := decodeTIFFPage(t.r, t.page)
	return err
}

// Encodes the image as a single strip uncompressed TIFF, colour as three samples and alpha as a fourth unassociated one
func encodeTIFF(w io.Writer, img image.Image) error {
	b := img.Bounds()
	width, height := b.Dx(), b.Dy()
	var pix []byte
	var bits []uint16
	photometric := uint16(2)
	var colorMap []uint16
	extra := false
	switch m := img.(type) {
	case *image.Gray:
		photometric, bits = 1, []uint16{8}
		for y := 0; y < height; y++ {
			pix = append(pix, m.Pix[m.PixOffset(b.Min.X, b.Min.Y+y):][:width]...)
		}
	case *image.Gray16:
		photometric, bits = 1, []uint16{16}
		for y := 0; y < height; y++ {
			row := m.Pix[m.PixOffset(b.Min.X, b.Min.Y+y):][:2*width]
			for x := 0; x < width; x++ {
				pix = append(pix, row[2*x+1], row[2*x])
			}
		}
	default:
		if p, ok := img.(*image.Paletted); ok && !hasAlpha(p) && len(p.Palette) <= 256 {
			photometric, bits = 3, []uint16{8}
			colorMap = make([]uint16, 3*256)
			for i, c := range p.Palette {
				n := color.NRGBAModel.Convert(c).(color.NRGBA)
				colorMap[i], colorMap[256+i], colorMap[512+i] = uint16(n.R)*257, uint16(n.G)*257, uint16(n.B)*257
			}
			for y := 0; y < height; y++ {
				pix = append(pix, p.Pix[p.PixOffset(b.Min.X, b.Min.Y+y):][:width]...)
			}
			break
		}
		if hasAlpha(img) {
			n := ToNRGBA(img)
			bits, extra, pix = []uint16{8, 8, 8, 8}, true, n.Pix
			break
		}
		rgb := ToRGB(img)
		bits = []uint16{8, 8, 8}
		pix = make([]byte, 0, width*height*3)
		for i := 0; i < len(rgb.Pix); i += 4 {
			pix = append(pix, rgb.Pix[i], rgb.Pix[i+1], rgb.Pix[i+2])
		}
	}
	type field struct {
		tag, typ uint16
		values   []uint32
		raw      []byte
	}
	var fields []field
	short := func(tag uint16, v ...uint16) {
		vals := make([]uint32, len(v))
		for i, x := range v {
			vals[i] = uint32(x)
		}
		fields = append(fields, field{tag: tag, typ: 3, values: vals})
	}
	long := func(tag uint16, v uint32) { fields = append(fields, field{tag: tag, typ: 4, values: []uint32{v}}) }
	long(tagWidth, uint32(width))
	long(tagLength, uint32(height))
	short(258, bits...)
	short(tagCompression, 1)
	short(tagPhotometric, photometric)
	long(tagStripOffsets, 0)
	short(277, uint16(len(bits)))
	long(tagRowsPerStrip, uint32(height))
	long(tagStripByteCounts, uint32(len(pix)))
	short(284, 1)
	if colorMap != nil {
		short(320, colorMap...)
	}
	if extra {
		short(tagExtraSamples, 2)
	}
	ifdSize := 2 + 12*len(fields) + 4
	dataAt := 8 + ifdSize
	var blob []byte
	for i := range fields {
		f := &fields[i]
		size := 2
		if f.typ == 4 {
			size = 4
		}
		buf := make([]byte, 0, size*len(f.values))
		for _, v := range f.values {
			if size == 2 {
				buf = binary.LittleEndian.AppendUint16(buf, uint16(v))
			} else {
				buf = binary.LittleEndian.AppendUint32(buf, v)
			}
		}
		if len(buf) > 4 {
			f.raw = binary.LittleEndian.AppendUint32(nil, uint32(dataAt+len(blob)))
			blob = append(blob, buf...)
			if len(blob)%2 == 1 {
				blob = append(blob, 0)
			}
		} else {
			f.raw = append(buf, make([]byte, 4-len(buf))...)
		}
	}
	stripAt := uint32(dataAt + len(blob))
	out := []byte{'I', 'I', 42, 0, 8, 0, 0, 0}
	out = binary.LittleEndian.AppendUint16(out, uint16(len(fields)))
	for _, f := range fields {
		out = binary.LittleEndian.AppendUint16(out, f.tag)
		out = binary.LittleEndian.AppendUint16(out, f.typ)
		out = binary.LittleEndian.AppendUint32(out, uint32(len(f.values)))
		if f.tag == tagStripOffsets {
			out = binary.LittleEndian.AppendUint32(out, stripAt)
			continue
		}
		out = append(out, f.raw...)
	}
	out = binary.LittleEndian.AppendUint32(out, 0)
	out = append(out, blob...)
	if _, err := w.Write(out); err != nil {
		return err
	}
	_, err := w.Write(pix)
	return err
}
