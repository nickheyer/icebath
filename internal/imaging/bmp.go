package imaging

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"image"
	"image/color"
	"io"
)

// Header facts of a device independent bitmap
type dibInfo struct {
	headerSize  int
	width       int
	height      int
	topDown     bool
	bits        int
	compression uint32
	colors      int
	padding     int
	masks       [4]uint32
	paletteAt   int
}

// BMP compression codes the decoder handles
const (
	bmpRaw       = 0
	bmpRLE8      = 1
	bmpRLE4      = 2
	bmpBitfields = 3
)

// Reads a bitmap header starting at an offset, the palette position following it and any separate masks
func readDIB(data []byte, at int) (*dibInfo, error) {
	if at+4 > len(data) {
		return nil, io.ErrUnexpectedEOF
	}
	d := &dibInfo{headerSize: int(binary.LittleEndian.Uint32(data[at:]))}
	if d.headerSize < 4 || at+d.headerSize > len(data) {
		return nil, io.ErrUnexpectedEOF
	}
	h := data[at+4 : at+d.headerSize]
	pos := at + d.headerSize
	switch d.headerSize {
	case 12:
		d.width = int(binary.LittleEndian.Uint16(h[0:]))
		d.height = int(binary.LittleEndian.Uint16(h[2:]))
		d.bits = int(binary.LittleEndian.Uint16(h[6:]))
		d.padding = 3
	case 40, 52, 56, 64, 108, 124:
		d.topDown = h[7] == 0xff
		d.width = int(binary.LittleEndian.Uint32(h[0:]))
		raw := binary.LittleEndian.Uint32(h[4:])
		d.height = int(raw)
		if d.topDown {
			d.height = int(uint32(0) - raw)
		}
		d.bits = int(binary.LittleEndian.Uint16(h[10:]))
		d.compression = binary.LittleEndian.Uint32(h[12:])
		d.colors = int(binary.LittleEndian.Uint32(h[28:]))
		d.padding = 4
		if d.compression == bmpBitfields {
			if len(h) >= 48 {
				for i := 0; i < 3; i++ {
					d.masks[i] = binary.LittleEndian.Uint32(h[36+4*i:])
				}
				if len(h) >= 52 {
					d.masks[3] = binary.LittleEndian.Uint32(h[48:])
				}
			} else {
				if pos+12 > len(data) {
					return nil, io.ErrUnexpectedEOF
				}
				for i := 0; i < 3; i++ {
					d.masks[i] = binary.LittleEndian.Uint32(data[pos+4*i:])
				}
				pos += 12
			}
		}
	default:
		return nil, fmt.Errorf("%w: bmp header type %d", ErrUnsupported, d.headerSize)
	}
	if d.colors == 0 {
		d.colors = 1 << min(d.bits, 30)
	}
	d.paletteAt = pos
	return d, nil
}

// Bitfield layouts the decoder accepts per bit depth
var bmpMaskLayouts = map[int][][4]uint32{
	32: {
		{0xff0000, 0xff00, 0xff, 0}, {0xff000000, 0xff0000, 0xff00, 0}, {0xff000000, 0xff00, 0xff, 0},
		{0xff000000, 0xff0000, 0xff00, 0xff}, {0xff, 0xff00, 0xff0000, 0xff000000}, {0xff0000, 0xff00, 0xff, 0xff000000},
		{0xff000000, 0xff00, 0xff, 0xff0000}, {0, 0, 0, 0},
	},
	24: {{0xff0000, 0xff00, 0xff, 0}},
	16: {{0xf800, 0x7e0, 0x1f, 0}, {0x7c00, 0x3e0, 0x1f, 0}},
}

// Channel masks the pixels use, refusing depths, compressions, bitfield arrangements and palettes the decoder lacks
func (d *dibInfo) layout() ([4]uint32, error) {
	var masks [4]uint32
	switch d.bits {
	case 1, 4, 8, 16, 24, 32:
	default:
		return masks, fmt.Errorf("%w: bmp pixel depth %d", ErrUnsupported, d.bits)
	}
	switch d.compression {
	case bmpRaw:
		switch d.bits {
		case 16:
			masks = [4]uint32{0x7c00, 0x3e0, 0x1f, 0}
		case 24, 32:
			masks = [4]uint32{0xff0000, 0xff00, 0xff, 0}
		}
	case bmpBitfields:
		ok := false
		check := d.masks
		if d.bits != 32 {
			check[3] = 0
		}
		for _, l := range bmpMaskLayouts[d.bits] {
			if l == check {
				ok = true
			}
		}
		if !ok {
			return masks, fmt.Errorf("%w: bmp bitfields layout", ErrUnsupported)
		}
		masks = check
		if masks == [4]uint32{} {
			masks = [4]uint32{0xff0000, 0xff00, 0xff, 0xff000000}
		}
	case bmpRLE8, bmpRLE4:
		if d.bits > 8 {
			return masks, fmt.Errorf("%w: bmp run length depth %d", ErrUnsupported, d.bits)
		}
	default:
		return masks, fmt.Errorf("%w: bmp compression %d", ErrUnsupported, d.compression)
	}
	if d.bits <= 8 && (d.colors <= 0 || d.colors > 65536) {
		return masks, fmt.Errorf("%w: bmp palette size %d", ErrUnsupported, d.colors)
	}
	return masks, nil
}

// Decodes a bitmap whose pixels start at an offset, palettes matching the grey ramp read as grey
func (d *dibInfo) decode(data []byte, pixelAt int, height int) (image.Image, error) {
	w, h := d.width, height
	if err := checkBomb(w, h); err != nil {
		return nil, err
	}
	masks, err := d.layout()
	if err != nil {
		return nil, err
	}
	if d.bits <= 8 {
		return d.decodeIndexed(data, pixelAt, w, h)
	}
	stride := ((w*d.bits + 31) >> 3) &^ 3
	if pixelAt < 0 || pixelAt+stride*h > len(data) || stride*h < 0 {
		return nil, errors.New("bmp: not enough image data")
	}
	alpha := masks[3] != 0
	out := image.NewNRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		srcY := h - 1 - y
		if d.topDown {
			srcY = y
		}
		row := data[pixelAt+srcY*stride:]
		o := out.Pix[y*out.Stride:]
		for x := 0; x < w; x++ {
			var px uint32
			switch d.bits {
			case 16:
				px = uint32(binary.LittleEndian.Uint16(row[2*x:]))
			case 24:
				px = uint32(row[3*x]) | uint32(row[3*x+1])<<8 | uint32(row[3*x+2])<<16
			case 32:
				px = binary.LittleEndian.Uint32(row[4*x:])
			}
			o[4*x], o[4*x+1], o[4*x+2] = maskValue(px, masks[0]), maskValue(px, masks[1]), maskValue(px, masks[2])
			o[4*x+3] = 255
			if alpha {
				o[4*x+3] = maskValue(px, masks[3])
			}
		}
	}
	if !alpha {
		return ToRGB(out), nil
	}
	return out, nil
}

// Channel value under a bitfield mask scaled to eight bits
func maskValue(px, mask uint32) uint8 {
	if mask == 0 {
		return 0
	}
	shift := 0
	for mask>>shift&1 == 0 {
		shift++
	}
	bits := 0
	for mask>>(shift+bits)&1 == 1 {
		bits++
	}
	v := px & mask >> shift
	if bits == 8 {
		return uint8(v)
	}
	return uint8(v * 255 / (1<<bits - 1))
}

// Palette or grey image of a bitmap up to eight bits deep, raw or run length coded
func (d *dibInfo) decodeIndexed(data []byte, pixelAt, w, h int) (image.Image, error) {
	raw := data[min(d.paletteAt, len(data)):min(len(data), d.paletteAt+d.padding*d.colors)]
	n := len(raw) / d.padding
	pal := make(color.Palette, n)
	gray := true
	for i := 0; i < n; i++ {
		b, g, r := raw[i*d.padding], raw[i*d.padding+1], raw[i*d.padding+2]
		pal[i] = color.RGBA{r, g, b, 255}
	}
	for i := 0; i < d.colors; i++ {
		want := uint8(i)
		if d.colors == 2 {
			want = []uint8{0, 255}[i]
		}
		if i >= n || raw[i*d.padding] != want || raw[i*d.padding+1] != want || raw[i*d.padding+2] != want {
			gray = false
			break
		}
	}
	idx := make([]uint8, w*h)
	switch d.compression {
	case bmpRLE8, bmpRLE4:
		if !bmpRLE(idx, data, pixelAt, w, h, d.compression == bmpRLE4, d.topDown) {
			return nil, errors.New("bmp: not enough image data")
		}
	default:
		stride := ((w*d.bits + 31) >> 3) &^ 3
		if pixelAt < 0 || pixelAt+stride*h > len(data) || stride*h < 0 {
			return nil, errors.New("bmp: not enough image data")
		}
		for y := 0; y < h; y++ {
			srcY := h - 1 - y
			if d.topDown {
				srcY = y
			}
			row := data[pixelAt+srcY*stride:]
			for x := 0; x < w; x++ {
				bit := x * d.bits
				idx[y*w+x] = row[bit/8] >> (8 - d.bits - bit%8) & (1<<d.bits - 1)
			}
		}
	}
	r := image.Rect(0, 0, w, h)
	if gray {
		if d.colors == 2 {
			return &image.Paletted{Pix: idx, Stride: w, Rect: r, Palette: color.Palette{color.Gray{0}, color.Gray{255}}}, nil
		}
		return &image.Gray{Pix: idx, Stride: w, Rect: r}, nil
	}
	full := make(color.Palette, max(len(pal), int(maxIndex(idx))+1))
	for i := range full {
		full[i] = color.RGBA{0, 0, 0, 255}
		if i < len(pal) {
			full[i] = pal[i]
		}
	}
	return &image.Paletted{Pix: idx, Stride: w, Rect: r, Palette: full}, nil
}

// Unpacks run length coded indices into display rows, false when the stream ends before covering every pixel
func bmpRLE(out, data []byte, pos, w, h int, rle4, topDown bool) bool {
	x, y := 0, 0
	put := func(v uint8) {
		if x < w && y < h {
			row := h - 1 - y
			if topDown {
				row = y
			}
			out[row*w+x] = v
		}
		x++
	}
	covered := func() bool { return y*w+min(x, w) >= w*h }
	for pos+1 < len(data) && y < h {
		count, value := int(data[pos]), data[pos+1]
		pos += 2
		if count > 0 {
			for i := 0; i < count && x < w; i++ {
				if rle4 {
					if i%2 == 0 {
						put(value >> 4)
					} else {
						put(value & 0x0f)
					}
				} else {
					put(value)
				}
			}
			continue
		}
		switch value {
		case 0:
			x, y = 0, y+1
		case 1:
			return covered()
		case 2:
			if pos+1 >= len(data) {
				return covered()
			}
			x += int(data[pos])
			y += int(data[pos+1])
			pos += 2
		default:
			n := int(value)
			size := n
			if rle4 {
				size = (n + 1) / 2
			}
			if pos+size > len(data) {
				return covered()
			}
			for i := 0; i < n; i++ {
				if rle4 {
					b := data[pos+i/2]
					if i%2 == 0 {
						put(b >> 4)
					} else {
						put(b & 0x0f)
					}
				} else {
					put(data[pos+i])
				}
			}
			pos += size + size%2
		}
	}
	return covered()
}

// Decodes a BMP file through its file header and bitmap header
func decodeBMP(data []byte) (image.Image, error) {
	if len(data) < 14 || string(data[:2]) != "BM" {
		return nil, ErrUnknownFormat
	}
	d, err := readDIB(data, 14)
	if err != nil {
		return nil, corrupt(BMP, err)
	}
	offset := int(binary.LittleEndian.Uint32(data[10:]))
	if offset == 14+d.headerSize && d.bits <= 8 {
		offset += 4 * d.colors
	}
	if offset == 0 {
		offset = d.paletteAt
		if d.bits <= 8 {
			offset += d.padding * d.colors
		}
	}
	img, err := d.decode(data, offset, d.height)
	return img, corrupt(BMP, err)
}

// Size of a BMP from its headers once its pixel layout and palette are ones the decoder reads
func configBMP(data []byte) (int, int, error) {
	if len(data) < 14 || string(data[:2]) != "BM" {
		return 0, 0, ErrUnknownFormat
	}
	d, err := readDIB(data, 14)
	if err != nil {
		return 0, 0, corrupt(BMP, err)
	}
	if err := checkBomb(d.width, d.height); err != nil {
		return 0, 0, err
	}
	if _, err := d.layout(); err != nil {
		return 0, 0, err
	}
	return d.width, d.height, nil
}

// Reports a two entry palette of black then white, the bilevel layout decoders give one bit images
func isBilevel(p *image.Paletted) bool {
	if len(p.Palette) != 2 {
		return false
	}
	black, white := color.NRGBAModel.Convert(p.Palette[0]).(color.NRGBA), color.NRGBAModel.Convert(p.Palette[1]).(color.NRGBA)
	return black == color.NRGBA{0, 0, 0, 255} && white == color.NRGBA{255, 255, 255, 255}
}

// Writes an uncompressed BMP, bilevel at one bit, palettes and grey at eight, colour at 24, alpha at 32
func encodeBMP(w io.Writer, img image.Image) error {
	b := img.Bounds()
	width, height := b.Dx(), b.Dy()
	var bits int
	var palette []byte
	var row func(y int, dst []byte)
	switch m := img.(type) {
	case *image.Paletted:
		if isBilevel(m) {
			bits, palette = 1, []byte{0, 0, 0, 0, 255, 255, 255, 255}
			row = func(y int, dst []byte) {
				src := m.Pix[m.PixOffset(b.Min.X, b.Min.Y+y):]
				for x := 0; x < width; x++ {
					if src[x] != 0 {
						dst[x/8] |= 0x80 >> (x % 8)
					}
				}
			}
			break
		}
		bits = 8
		for _, c := range m.Palette[:min(len(m.Palette), 256)] {
			n := color.NRGBAModel.Convert(c).(color.NRGBA)
			palette = append(palette, n.B, n.G, n.R, 0)
		}
		row = func(y int, dst []byte) { copy(dst, m.Pix[m.PixOffset(b.Min.X, b.Min.Y+y):][:width]) }
	case *image.Gray, *image.Gray16:
		g := toGray(m)
		bits = 8
		for i := 0; i < 256; i++ {
			palette = append(palette, byte(i), byte(i), byte(i), byte(i))
		}
		row = func(y int, dst []byte) { copy(dst, g.Pix[y*g.Stride:][:width]) }
	default:
		if hasAlpha(img) {
			n := ToNRGBA(img)
			bits = 32
			row = func(y int, dst []byte) {
				src := n.Pix[y*n.Stride:]
				for x := 0; x < width; x++ {
					dst[4*x], dst[4*x+1], dst[4*x+2], dst[4*x+3] = src[4*x+2], src[4*x+1], src[4*x], src[4*x+3]
				}
			}
			break
		}
		rgb := ToRGB(img)
		bits = 24
		row = func(y int, dst []byte) {
			src := rgb.Pix[y*rgb.Stride:]
			for x := 0; x < width; x++ {
				dst[3*x], dst[3*x+1], dst[3*x+2] = src[4*x+2], src[4*x+1], src[4*x]
			}
		}
	}
	stride := ((width*bits+7)/8 + 3) &^ 3
	colors := len(palette) / 4
	offset := 14 + 40 + colors*4
	size := stride * height
	if uint64(offset)+uint64(size) > 1<<32-1 {
		return fmt.Errorf("%w: bmp file larger than 4 GiB", ErrUnsupported)
	}
	dpi := 96.0
	ppm := uint32(dpi*39.3701 + 0.5)
	head := make([]byte, 0, offset)
	head = append(head, 'B', 'M')
	head = binary.LittleEndian.AppendUint32(head, uint32(offset+size))
	head = binary.LittleEndian.AppendUint32(head, 0)
	head = binary.LittleEndian.AppendUint32(head, uint32(offset))
	head = binary.LittleEndian.AppendUint32(head, 40)
	head = binary.LittleEndian.AppendUint32(head, uint32(width))
	head = binary.LittleEndian.AppendUint32(head, uint32(height))
	head = binary.LittleEndian.AppendUint16(head, 1)
	head = binary.LittleEndian.AppendUint16(head, uint16(bits))
	head = binary.LittleEndian.AppendUint32(head, 0)
	head = binary.LittleEndian.AppendUint32(head, uint32(size))
	head = binary.LittleEndian.AppendUint32(head, ppm)
	head = binary.LittleEndian.AppendUint32(head, ppm)
	head = binary.LittleEndian.AppendUint32(head, uint32(colors))
	head = binary.LittleEndian.AppendUint32(head, uint32(colors))
	head = append(head, palette...)
	bw := bufio.NewWriter(w)
	if _, err := bw.Write(head); err != nil {
		return err
	}
	line := make([]byte, stride)
	for y := height - 1; y >= 0; y-- {
		clear(line)
		row(y, line)
		if _, err := bw.Write(line); err != nil {
			return err
		}
	}
	return bw.Flush()
}
