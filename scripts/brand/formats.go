package main

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"image"
	"image/jpeg"
	"image/png"
	"io"
	"math"
	"sort"

	"github.com/HugoSmits86/nativewebp"
	"golang.org/x/image/tiff"
	"golang.org/x/image/webp"
)

// Expected shape of one output, used by the post write verify and by --check
type shape struct {
	format  string
	w, h    int
	frames  []int
	corners bool
}

func encodePNG(img image.Image) ([]byte, error) {
	var buf bytes.Buffer
	enc := png.Encoder{CompressionLevel: png.BestCompression}
	if err := enc.Encode(&buf, img); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// Multi size ICO of PNG entries, 32 bits per pixel with zero planes, each frame resized from src
func encodeICO(src *image.NRGBA, sizes []int) ([]byte, error) {
	var head, body bytes.Buffer
	head.Write([]byte{0, 0, 1, 0})
	binary.Write(&head, binary.LittleEndian, uint16(len(sizes)))
	offset := 6 + 16*len(sizes)
	for _, size := range sizes {
		frame := src
		if size != src.Bounds().Dx() {
			frame = resize(src, size, size)
		}
		data, err := encodePNG(frame)
		if err != nil {
			return nil, err
		}
		side := uint8(size)
		if size >= 256 {
			side = 0
		}
		head.Write([]byte{side, side, 0, 0})
		binary.Write(&head, binary.LittleEndian, [2]uint16{0, 32})
		binary.Write(&head, binary.LittleEndian, [2]uint32{uint32(len(data)), uint32(offset + body.Len())})
		body.Write(data)
	}
	head.Write(body.Bytes())
	return head.Bytes(), nil
}

// ICNS PNG representations in the order the originals carry them, pixel size per type
var icnsTypes = []struct {
	kind string
	size int
}{{"ic07", 128}, {"ic08", 256}, {"ic09", 512}, {"ic10", 1024}, {"ic11", 32}, {"ic12", 64}, {"ic13", 256}, {"ic14", 512}}

// ICNS container with a table of contents and every PNG type of the originals, frames resized from src
func encodeICNS(src *image.NRGBA) ([]byte, error) {
	var toc, body bytes.Buffer
	for _, t := range icnsTypes {
		frame := src
		if t.size != src.Bounds().Dx() {
			frame = resize(src, t.size, t.size)
		}
		data, err := encodePNG(frame)
		if err != nil {
			return nil, err
		}
		toc.WriteString(t.kind)
		binary.Write(&toc, binary.BigEndian, uint32(8+len(data)))
		body.WriteString(t.kind)
		binary.Write(&body, binary.BigEndian, uint32(8+len(data)))
		body.Write(data)
	}
	var out bytes.Buffer
	out.WriteString("icns")
	binary.Write(&out, binary.BigEndian, uint32(8+8+toc.Len()+body.Len()))
	out.WriteString("TOC ")
	binary.Write(&out, binary.BigEndian, uint32(8+toc.Len()))
	out.Write(toc.Bytes())
	out.Write(body.Bytes())
	return out.Bytes(), nil
}

// Pixel sizes of each ICNS PNG representation keyed by type
func readICNS(data []byte) (map[string]image.Image, error) {
	if len(data) < 8 || string(data[:4]) != "icns" || int(binary.BigEndian.Uint32(data[4:8])) != len(data) {
		return nil, errors.New("not an icns container of its declared length")
	}
	out := map[string]image.Image{}
	for i := 8; i < len(data); {
		if i+8 > len(data) {
			return nil, errors.New("truncated icns chunk")
		}
		kind, n := string(data[i:i+4]), int(binary.BigEndian.Uint32(data[i+4:i+8]))
		if n < 8 || i+n > len(data) {
			return nil, fmt.Errorf("bad icns chunk %q", kind)
		}
		if kind != "TOC " {
			img, err := png.Decode(bytes.NewReader(data[i+8 : i+n]))
			if err != nil {
				return nil, fmt.Errorf("icns %s: %w", kind, err)
			}
			out[kind] = img
		}
		i += n
	}
	return out, nil
}

// PNG frames of an ICO keyed by the directory width, 256 for a zero entry
func readICO(data []byte) ([]image.Image, []int, error) {
	if len(data) < 6 || binary.LittleEndian.Uint16(data[2:4]) != 1 {
		return nil, nil, errors.New("not an icon directory")
	}
	n := int(binary.LittleEndian.Uint16(data[4:6]))
	var imgs []image.Image
	var sizes []int
	for i := 0; i < n; i++ {
		e := 6 + 16*i
		if e+16 > len(data) {
			return nil, nil, errors.New("truncated icon directory")
		}
		size := int(data[e])
		if size == 0 {
			size = 256
		}
		length, offset := binary.LittleEndian.Uint32(data[e+8:]), binary.LittleEndian.Uint32(data[e+12:])
		if uint64(offset)+uint64(length) > uint64(len(data)) {
			return nil, nil, fmt.Errorf("icon entry %d overruns the file", i)
		}
		img, err := png.Decode(bytes.NewReader(data[offset : offset+length]))
		if err != nil {
			return nil, nil, fmt.Errorf("icon entry %d: %w", i, err)
		}
		if img.Bounds().Dx() != size || img.Bounds().Dy() != size {
			return nil, nil, fmt.Errorf("icon entry %d declares %d but holds %v", i, size, img.Bounds().Size())
		}
		imgs, sizes = append(imgs, img), append(sizes, size)
	}
	return imgs, sizes, nil
}

func encodeJPEG(img image.Image, quality int) ([]byte, error) {
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: quality}); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func encodeWebP(img image.Image) ([]byte, error) {
	var buf bytes.Buffer
	if err := nativewebp.Encode(&buf, img, &nativewebp.Options{CompressionLevel: nativewebp.BestCompression}); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// TIFF LZW codes per libtiff, nine to twelve bits most significant first with early change
const (
	lzwClear = 256
	lzwEOI   = 257
	lzwFirst = 258
	lzwMax   = 4095
)

type bitSink struct {
	out   bytes.Buffer
	acc   uint32
	nacc  uint
	nbits uint
}

func (b *bitSink) put(code int) {
	b.acc = b.acc<<b.nbits | uint32(code)
	b.nacc += b.nbits
	for b.nacc >= 8 {
		b.out.WriteByte(byte(b.acc >> (b.nacc - 8)))
		b.nacc -= 8
	}
	b.acc &= 1<<b.nacc - 1
}

func (b *bitSink) flush() {
	if b.nacc > 0 {
		b.out.WriteByte(byte(b.acc << (8 - b.nacc)))
	}
	b.acc, b.nacc = 0, 0
}

// LZW encoding of one strip exactly as libtiff emits it, clear first and end of information last
func lzwEncode(data []byte) []byte {
	s := &bitSink{nbits: 9}
	table := map[int]int{}
	free, maxcode := lzwFirst, 511
	s.put(lzwClear)
	if len(data) == 0 {
		s.put(lzwEOI)
		s.flush()
		return s.out.Bytes()
	}
	ent := int(data[0])
	for _, c := range data[1:] {
		key := ent<<8 | int(c)
		if code, ok := table[key]; ok {
			ent = code
			continue
		}
		s.put(ent)
		ent = int(c)
		table[key] = free
		free++
		if free == lzwMax-1 {
			s.put(lzwClear)
			table = map[int]int{}
			free, maxcode, s.nbits = lzwFirst, 511, 9
		} else if free > maxcode {
			s.nbits++
			maxcode = 1<<s.nbits - 1
		}
	}
	s.put(ent)
	free++
	if free == lzwMax-1 {
		s.put(lzwClear)
		s.nbits = 9
	} else if free > maxcode {
		s.nbits++
	}
	s.put(lzwEOI)
	s.flush()
	return s.out.Bytes()
}

// Version 2 sRGB display profile, D50 adapted primaries and a 1024 point sRGB tone curve
func srgbProfile() []byte {
	s15 := func(v float64) []byte {
		var b [4]byte
		binary.BigEndian.PutUint32(b[:], uint32(int32(math.Round(v*65536))))
		return b[:]
	}
	xyz := func(x, y, z float64) []byte {
		out := append([]byte("XYZ \x00\x00\x00\x00"), s15(x)...)
		return append(append(out, s15(y)...), s15(z)...)
	}
	text := func(sig, s string) []byte {
		out := append([]byte(sig), 0, 0, 0, 0)
		if sig == "desc" {
			out = binary.BigEndian.AppendUint32(out, uint32(len(s)+1))
			out = append(append(out, s...), 0)
			out = append(out, make([]byte, 4+4+2+1+67)...)
			return out
		}
		return append(append(out, s...), 0)
	}
	curve := append([]byte("curv\x00\x00\x00\x00"), binary.BigEndian.AppendUint32(nil, 1024)...)
	for i := 0; i < 1024; i++ {
		v := float64(i) / 1023
		if v <= 0.04045 {
			v /= 12.92
		} else {
			v = math.Pow((v+0.055)/1.055, 2.4)
		}
		curve = binary.BigEndian.AppendUint16(curve, uint16(math.Round(v*65535)))
	}
	tags := []struct {
		sig  string
		data []byte
	}{
		{"desc", text("desc", "sRGB IEC61966-2.1")},
		{"cprt", text("text", "No copyright, use freely")},
		{"wtpt", xyz(0.9642, 1.0, 0.8249)},
		{"rXYZ", xyz(0.4360747, 0.2225045, 0.0139322)},
		{"gXYZ", xyz(0.3850649, 0.7168786, 0.0971045)},
		{"bXYZ", xyz(0.1430804, 0.0606169, 0.7141733)},
		{"rTRC", curve}, {"gTRC", curve}, {"bTRC", curve},
	}
	var body bytes.Buffer
	table := binary.BigEndian.AppendUint32(nil, uint32(len(tags)))
	start := 128 + 4 + 12*len(tags)
	shared := -1
	for _, t := range tags {
		off := start + body.Len()
		if t.sig == "gTRC" || t.sig == "bTRC" {
			off = shared
		} else {
			if t.sig == "rTRC" {
				shared = off
			}
			body.Write(t.data)
			for body.Len()%4 != 0 {
				body.WriteByte(0)
			}
		}
		table = append(table, t.sig...)
		table = binary.BigEndian.AppendUint32(table, uint32(off))
		table = binary.BigEndian.AppendUint32(table, uint32(len(t.data)))
	}
	head := make([]byte, 128)
	binary.BigEndian.PutUint32(head[0:], uint32(start+body.Len()))
	binary.BigEndian.PutUint32(head[8:], 0x02100000)
	copy(head[12:], "mntrRGB XYZ ")
	binary.BigEndian.PutUint16(head[24:], 2026)
	binary.BigEndian.PutUint16(head[26:], 1)
	binary.BigEndian.PutUint16(head[28:], 1)
	copy(head[36:], "acsp")
	copy(head[68:], s15(0.9642))
	copy(head[72:], s15(1.0))
	copy(head[76:], s15(0.8249))
	return append(append(head, table...), body.Bytes()...)
}

// One TIFF page, opaque RGB with its resolution in dots per inch
type tiffPage struct {
	img *image.NRGBA
	dpi uint32
}

// Big endian multi page RGB TIFF, LZW with horizontal predictor, sRGB profile per page
func encodeTIFF(pages []tiffPage) []byte {
	var out bytes.Buffer
	out.Write([]byte{'M', 'M', 0, 42, 0, 0, 0, 0})
	icc := srgbProfile()
	prevNext := 4
	for _, p := range pages {
		w, h := p.img.Bounds().Dx(), p.img.Bounds().Dy()
		rowBytes := w * 3
		rows := max(1, 131072/rowBytes)
		var offsets, counts []uint32
		for y0 := 0; y0 < h; y0 += rows {
			var strip []byte
			for y := y0; y < min(h, y0+rows); y++ {
				row := p.img.Pix[y*p.img.Stride:]
				prev := [3]byte{}
				for x := 0; x < w; x++ {
					for c := 0; c < 3; c++ {
						v := row[x*4+c]
						strip = append(strip, v-prev[c])
						prev[c] = v
					}
				}
			}
			enc := lzwEncode(strip)
			offsets = append(offsets, uint32(out.Len()))
			counts = append(counts, uint32(len(enc)))
			out.Write(enc)
			if out.Len()%2 != 0 {
				out.WriteByte(0)
			}
		}
		extra := func(data []byte) uint32 {
			off := uint32(out.Len())
			out.Write(data)
			if out.Len()%2 != 0 {
				out.WriteByte(0)
			}
			return off
		}
		u32s := func(v []uint32) []byte {
			var b []byte
			for _, x := range v {
				b = binary.BigEndian.AppendUint32(b, x)
			}
			return b
		}
		bps := extra([]byte{0, 8, 0, 8, 0, 8})
		sf := extra([]byte{0, 1, 0, 1, 0, 1})
		res := extra(u32s([]uint32{p.dpi, 1}))
		iccOff := extra(icc)
		offOff, cntOff := uint32(0), uint32(0)
		if len(offsets) > 1 {
			offOff, cntOff = extra(u32s(offsets)), extra(u32s(counts))
		} else {
			offOff, cntOff = offsets[0], counts[0]
		}
		type entry struct {
			tag, typ uint16
			count    uint32
			value    uint32
		}
		const short, long, rational, undefined = 3, 4, 5, 7
		entries := []entry{
			{256, long, 1, uint32(w)}, {257, long, 1, uint32(h)}, {258, short, 3, bps},
			{259, short, 1, 5 << 16}, {262, short, 1, 2 << 16}, {266, short, 1, 1 << 16},
			{273, long, uint32(len(offsets)), offOff}, {274, short, 1, 1 << 16}, {277, short, 1, 3 << 16},
			{278, long, 1, uint32(rows)}, {279, long, uint32(len(counts)), cntOff},
			{282, rational, 1, res}, {283, rational, 1, res}, {284, short, 1, 1 << 16},
			{296, short, 1, 2 << 16}, {317, short, 1, 2 << 16}, {339, short, 3, sf},
			{34675, undefined, uint32(len(icc)), iccOff},
		}
		sort.Slice(entries, func(i, j int) bool { return entries[i].tag < entries[j].tag })
		ifd := out.Len()
		b := out.Bytes()
		binary.BigEndian.PutUint32(b[prevNext:], uint32(ifd))
		out.Write(binary.BigEndian.AppendUint16(nil, uint16(len(entries))))
		for _, e := range entries {
			var rec []byte
			rec = binary.BigEndian.AppendUint16(rec, e.tag)
			rec = binary.BigEndian.AppendUint16(rec, e.typ)
			rec = binary.BigEndian.AppendUint32(rec, e.count)
			rec = binary.BigEndian.AppendUint32(rec, e.value)
			out.Write(rec)
		}
		prevNext = out.Len()
		out.Write([]byte{0, 0, 0, 0})
	}
	return out.Bytes()
}

// Sizes of every page of a TIFF, read from each directory's width and length tags
func tiffPageSizes(data []byte) ([][2]int, error) {
	if len(data) < 8 || string(data[:4]) != "MM\x00*" {
		return nil, errors.New("not a big endian tiff")
	}
	var sizes [][2]int
	for next := binary.BigEndian.Uint32(data[4:]); next != 0; {
		at := int(next)
		if at+2 > len(data) {
			return nil, errors.New("tiff directory past the end")
		}
		n := int(binary.BigEndian.Uint16(data[at:]))
		if at+2+12*n+4 > len(data) {
			return nil, errors.New("truncated tiff directory")
		}
		var size [2]int
		for i := 0; i < n; i++ {
			e := data[at+2+12*i:]
			tag, typ := binary.BigEndian.Uint16(e), binary.BigEndian.Uint16(e[2:])
			v := int(binary.BigEndian.Uint32(e[8:]))
			if typ == 3 {
				v = int(binary.BigEndian.Uint16(e[8:]))
			}
			switch tag {
			case 256:
				size[0] = v
			case 257:
				size[1] = v
			}
		}
		sizes = append(sizes, size)
		next = binary.BigEndian.Uint32(data[at+2+12*n:])
	}
	return sizes, nil
}

func decodeOne(format string, data []byte) (image.Image, error) {
	switch format {
	case "PNG":
		return png.Decode(bytes.NewReader(data))
	case "JPEG":
		return jpeg.Decode(bytes.NewReader(data))
	case "WEBP":
		return webp.Decode(bytes.NewReader(data))
	case "TIFF":
		return tiff.Decode(bytes.NewReader(data))
	}
	return nil, fmt.Errorf("no decoder for %s", format)
}

// One line summary of an output after checking it against its shape
func describe(want shape, data []byte) (string, error) {
	if err := inspect(want, data); err != nil {
		return "", err
	}
	switch want.format {
	case "ICO":
		return fmt.Sprintf("ICO %v", want.frames), nil
	case "ICNS":
		return fmt.Sprintf("ICNS (%d, %d) (container)", want.w, want.h), nil
	case "TIFF":
		return fmt.Sprintf("TIFF %d pages (%d, %d)", len(want.frames), want.w, want.h), nil
	case "SVG", "JSON", "TEXT":
		return fmt.Sprintf("%d bytes %s", len(data), want.format), nil
	}
	return fmt.Sprintf("%s (%d, %d)", want.format, want.w, want.h), nil
}

// Checks an output's container, frame set, pixel size and transparent corners against its shape
func inspect(want shape, data []byte) error {
	switch want.format {
	case "ICO":
		_, sizes, err := readICO(data)
		if err != nil {
			return err
		}
		if fmt.Sprint(sizes) != fmt.Sprint(want.frames) {
			return fmt.Errorf("ICO frames %v, expected %v", sizes, want.frames)
		}
		return nil
	case "ICNS":
		reps, err := readICNS(data)
		if err != nil {
			return err
		}
		for _, t := range icnsTypes {
			img, ok := reps[t.kind]
			if !ok || img.Bounds().Dx() != t.size || img.Bounds().Dy() != t.size {
				return fmt.Errorf("ICNS %s missing or not %dx%d", t.kind, t.size, t.size)
			}
		}
		if len(reps) != len(icnsTypes) {
			return fmt.Errorf("ICNS holds %d representations, expected %d", len(reps), len(icnsTypes))
		}
		return nil
	case "TIFF":
		sizes, err := tiffPageSizes(data)
		if err != nil {
			return err
		}
		if len(sizes) != len(want.frames) {
			return fmt.Errorf("TIFF pages %v, expected %d", sizes, len(want.frames))
		}
		for i, s := range sizes {
			if s[0] != want.frames[i] || s[1] != want.frames[i]*want.h/want.w {
				return fmt.Errorf("TIFF page %d is %v", i, s)
			}
		}
		_, err = tiff.Decode(bytes.NewReader(data))
		return err
	case "SVG":
		d := xml.NewDecoder(bytes.NewReader(data))
		for {
			if _, err := d.Token(); err == io.EOF {
				return nil
			} else if err != nil {
				return fmt.Errorf("SVG: %w", err)
			}
		}
	case "JSON":
		var v any
		return json.Unmarshal(data, &v)
	case "TEXT":
		if len(data) == 0 {
			return errors.New("empty")
		}
		return nil
	}
	img, err := decodeOne(want.format, data)
	if err != nil {
		return err
	}
	if img.Bounds().Dx() != want.w || img.Bounds().Dy() != want.h {
		return fmt.Errorf("got %s (%d, %d), expected %s (%d, %d)", want.format, img.Bounds().Dx(), img.Bounds().Dy(), want.format, want.w, want.h)
	}
	if want.corners {
		lo, hi := uint32(0xffff), uint32(0)
		b := img.Bounds()
		for y := b.Min.Y; y < b.Max.Y; y++ {
			for x := b.Min.X; x < b.Max.X; x++ {
				_, _, _, a := img.At(x, y).RGBA()
				lo, hi = min(lo, a), max(hi, a)
			}
		}
		if lo != 0 || hi != 0xffff {
			return fmt.Errorf("alpha extrema (%d, %d), expected (0, 255) transparent corners", lo>>8, hi>>8)
		}
	}
	return nil
}
