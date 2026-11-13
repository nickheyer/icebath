package imaging

import (
	"bufio"
	"bytes"
	"compress/lzw"
	"encoding/binary"
	"errors"
	"image"
	"image/color"
	"image/gif"
	"io"
)

// GIF stream walked block by block so every frame decodes on its own
type gifStream struct {
	data       []byte
	canvasW    int
	canvasH    int
	global     color.Palette
	globalRamp bool
	pos        int
	frame      gifFrame
}

// One frame descriptor with its colour table and compressed data
type gifFrame struct {
	rect        image.Rectangle
	interlace   bool
	palette     color.Palette
	ramp        bool
	hasTable    bool
	transparent int
	litWidth    int
	lzw         []byte
}

// Header, logical screen and global colour table of a GIF
func newGIFStream(data []byte) (*gifStream, error) {
	if len(data) < 13 || !(bytes.HasPrefix(data, []byte("GIF87a")) || bytes.HasPrefix(data, []byte("GIF89a"))) {
		return nil, ErrUnknownFormat
	}
	g := &gifStream{data: data, pos: 13}
	g.canvasW = int(binary.LittleEndian.Uint16(data[6:8]))
	g.canvasH = int(binary.LittleEndian.Uint16(data[8:10]))
	if flags := data[10]; flags&0x80 != 0 {
		n := 3 << ((flags & 7) + 1)
		if 13+n > len(data) {
			return nil, &CorruptError{Format: GIF, Err: io.ErrUnexpectedEOF}
		}
		g.global, g.globalRamp = gifPalette(data[13 : 13+n])
		g.pos += n
	}
	return g, nil
}

// Palette of a raw colour table and whether it is the identity grey ramp
func gifPalette(raw []byte) (color.Palette, bool) {
	p := make(color.Palette, len(raw)/3)
	ramp := true
	for i := range p {
		r, g, b := raw[3*i], raw[3*i+1], raw[3*i+2]
		p[i] = color.RGBA{r, g, b, 255}
		if !(int(r) == i && r == g && g == b) {
			ramp = false
		}
	}
	return p, ramp
}

// Offset just past a run of data sub blocks with their concatenated payload
func gifSubBlocks(data []byte, pos int) (int, []byte) {
	var payload []byte
	for pos < len(data) {
		n := int(data[pos])
		pos++
		if n == 0 {
			return pos, payload
		}
		if pos+n > len(data) {
			return len(data), append(payload, data[pos:]...)
		}
		payload = append(payload, data[pos:pos+n]...)
		pos += n
	}
	return pos, payload
}

// Reads blocks up to the following image descriptor, false at the trailer or the end of data
func (g *gifStream) next() (bool, error) {
	transparent := -1
	for g.pos < len(g.data) {
		switch g.data[g.pos] {
		case ';':
			return false, nil
		case '!':
			if g.pos+1 >= len(g.data) {
				return false, nil
			}
			label := g.data[g.pos+1]
			end, payload := gifSubBlocks(g.data, g.pos+2)
			if label == 0xf9 && len(payload) >= 4 && payload[0]&1 != 0 {
				transparent = int(payload[3])
			}
			g.pos = end
		case ',':
			if g.pos+10 > len(g.data) {
				return false, nil
			}
			d := g.data[g.pos+1 : g.pos+10]
			x0, y0 := int(binary.LittleEndian.Uint16(d[0:])), int(binary.LittleEndian.Uint16(d[2:]))
			w, h := int(binary.LittleEndian.Uint16(d[4:])), int(binary.LittleEndian.Uint16(d[6:]))
			f := gifFrame{rect: image.Rect(x0, y0, x0+w, y0+h), interlace: d[8]&0x40 != 0, transparent: transparent}
			p := g.pos + 10
			if d[8]&0x80 != 0 {
				n := 3 << ((d[8] & 7) + 1)
				if p+n > len(g.data) {
					return false, &CorruptError{Format: GIF, Err: io.ErrUnexpectedEOF}
				}
				f.palette, f.ramp = gifPalette(g.data[p : p+n])
				f.hasTable = true
				p += n
			}
			if p >= len(g.data) {
				return false, &CorruptError{Format: GIF, Err: io.ErrUnexpectedEOF}
			}
			f.litWidth = int(g.data[p])
			end, payload := gifSubBlocks(g.data, p+1)
			f.lzw = payload
			g.pos = end
			if f.rect.Max.X > g.canvasW || f.rect.Max.Y > g.canvasH {
				g.canvasW, g.canvasH = max(g.canvasW, f.rect.Max.X), max(g.canvasH, f.rect.Max.Y)
				if err := checkBomb(g.canvasW, g.canvasH); err != nil {
					return false, err
				}
			}
			g.frame = f
			return true, nil
		default:
			g.pos++
		}
	}
	return false, nil
}

// Longest string one LZW code of a twelve bit table expands to
const lzwMaxString = 4096

// Palette index of every pixel of the current frame in display order, refusing data too short to fill the frame
func (g *gifStream) indices() ([]uint8, error) {
	f := g.frame
	w, h := f.rect.Dx(), f.rect.Dy()
	if f.litWidth < 2 || f.litWidth > 8 {
		return nil, &CorruptError{Format: GIF, Err: errors.New("gif: code size out of range")}
	}
	if w*h > (8*len(f.lzw)/(f.litWidth+1)+1)*lzwMaxString {
		return nil, &CorruptError{Format: GIF, Err: errors.New("gif: image file is truncated")}
	}
	pix := make([]uint8, w*h)
	r := lzw.NewReader(bytes.NewReader(f.lzw), lzw.LSB, f.litWidth)
	defer r.Close()
	if _, err := io.ReadFull(r, pix); err != nil {
		return nil, &CorruptError{Format: GIF, Err: errors.New("gif: image file is truncated")}
	}
	if !f.interlace {
		return pix, nil
	}
	out := make([]uint8, w*h)
	src := 0
	for _, pass := range [4][2]int{{0, 8}, {4, 8}, {2, 4}, {1, 2}} {
		for y := pass[0]; y < h; y += pass[1] {
			copy(out[y*w:(y+1)*w], pix[src*w:(src+1)*w])
			src++
		}
	}
	return out, nil
}

// Decodes the first frame onto its canvas, the transparent index or zero filling the rest
func decodeGIF(data []byte) (image.Image, error) {
	g, err := newGIFStream(data)
	if err != nil {
		return nil, err
	}
	if err := checkBomb(g.canvasW, g.canvasH); err != nil {
		return nil, err
	}
	ok, err := g.next()
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, &CorruptError{Format: GIF, Err: errors.New("gif: no image data")}
	}
	idx, err := g.indices()
	if err != nil {
		return nil, err
	}
	f := g.frame
	pal, ramp := f.palette, f.ramp
	if !f.hasTable {
		pal, ramp = g.global, g.globalRamp
	}
	canvas := image.Rect(0, 0, g.canvasW, g.canvasH)
	fill := uint8(0)
	if f.transparent >= 0 {
		fill = uint8(f.transparent)
	}
	pix := make([]uint8, canvas.Dx()*canvas.Dy())
	for i := range pix {
		pix[i] = fill
	}
	fw := f.rect.Dx()
	for y := f.rect.Min.Y; y < f.rect.Max.Y && y < canvas.Dy(); y++ {
		for x := f.rect.Min.X; x < f.rect.Max.X && x < canvas.Dx(); x++ {
			pix[y*canvas.Dx()+x] = idx[(y-f.rect.Min.Y)*fw+x-f.rect.Min.X]
		}
	}
	if pal == nil || ramp {
		if f.transparent >= 0 {
			out := image.NewNRGBA(canvas)
			for i, v := range pix {
				a := uint8(255)
				if int(v) == f.transparent {
					a = 0
				}
				out.Pix[4*i], out.Pix[4*i+1], out.Pix[4*i+2], out.Pix[4*i+3] = v, v, v, a
			}
			return out, nil
		}
		return &image.Gray{Pix: pix, Stride: canvas.Dx(), Rect: canvas}, nil
	}
	full := make(color.Palette, max(len(pal), int(maxIndex(pix))+1))
	for i := range full {
		if i < len(pal) {
			full[i] = pal[i]
		} else {
			full[i] = color.RGBA{0, 0, 0, 255}
		}
	}
	if f.transparent >= 0 && f.transparent < len(full) {
		c := full[f.transparent].(color.RGBA)
		full[f.transparent] = color.NRGBA{c.R, c.G, c.B, 0}
	}
	return &image.Paletted{Pix: pix, Stride: canvas.Dx(), Rect: canvas, Palette: full}, nil
}

// Largest palette index used
func maxIndex(pix []uint8) uint8 {
	var m uint8
	for _, v := range pix {
		m = max(m, v)
	}
	return m
}

// Canvas size a GIF opens at, the logical screen grown to hold the first frame, which must exist
func configGIF(data []byte) (int, int, error) {
	g, err := newGIFStream(data)
	if err != nil {
		return 0, 0, err
	}
	ok, err := g.next()
	if err != nil {
		return 0, 0, err
	}
	if !ok {
		return 0, 0, &CorruptError{Format: GIF, Err: errors.New("gif: image not found in GIF frame")}
	}
	if err := checkBomb(g.canvasW, g.canvasH); err != nil {
		return 0, 0, err
	}
	return g.canvasW, g.canvasH, nil
}

// Frame source over a GIF, every frame judged at the canvas size reached so far
type gifFrames struct{ g *gifStream }

// Frame source after the header parses within the bomb limit
func newGIFFrames(data []byte) (*gifFrames, error) {
	g, err := newGIFStream(data)
	if err != nil {
		return nil, err
	}
	if err := checkBomb(g.canvasW, g.canvasH); err != nil {
		return nil, err
	}
	return &gifFrames{g: g}, nil
}

// Moves to the following frame
func (f *gifFrames) next() (int, int, bool, error) {
	ok, err := f.g.next()
	if err != nil || !ok {
		return 0, 0, false, err
	}
	return f.g.canvasW, f.g.canvasH, true, nil
}

// Decodes the current frame
func (f *gifFrames) decode() error {
	_, err := f.g.indices()
	return err
}

// Encodes a single frame GIF through a 256 entry palette, the first clear entry kept transparent
func encodeGIF(w io.Writer, img image.Image) error {
	var p *image.Paletted
	switch m := img.(type) {
	case *image.Paletted:
		p = m
	case *image.Gray:
		b := m.Bounds()
		p = image.NewPaletted(image.Rect(0, 0, b.Dx(), b.Dy()), grayRamp())
		for y := 0; y < b.Dy(); y++ {
			copy(p.Pix[y*p.Stride:y*p.Stride+b.Dx()], m.Pix[m.PixOffset(b.Min.X, b.Min.Y+y):])
		}
	case *image.Gray16:
		g := toGray(m)
		p = &image.Paletted{Pix: g.Pix, Stride: g.Stride, Rect: g.Rect, Palette: grayRamp()}
	default:
		method := MedianCut
		if hasAlpha(img) {
			method = FastOctree
		}
		q, err := Quantize(img, 256, method)
		if err != nil {
			return err
		}
		p = q
	}
	p = gifTransparency(p)
	bw := bufio.NewWriter(w)
	if err := gif.Encode(bw, p, &gif.Options{NumColors: 256}); err != nil {
		return err
	}
	return bw.Flush()
}

// Palette whose first fully clear entry alone stays transparent, every other entry made opaque
func gifTransparency(p *image.Paletted) *image.Paletted {
	pal := make(color.Palette, len(p.Palette))
	found := false
	for i, c := range p.Palette {
		n := color.NRGBAModel.Convert(c).(color.NRGBA)
		if n.A == 0 && !found {
			found = true
			pal[i] = color.NRGBA{n.R, n.G, n.B, 0}
			continue
		}
		pal[i] = color.RGBA{n.R, n.G, n.B, 255}
	}
	return &image.Paletted{Pix: p.Pix, Stride: p.Stride, Rect: p.Rect, Palette: pal}
}

// Identity palette of 256 greys
func grayRamp() color.Palette {
	p := make(color.Palette, 256)
	for i := range p {
		p[i] = color.Gray{uint8(i)}
	}
	return p
}
