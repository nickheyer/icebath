package imaging

import (
	"bufio"
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"image"
	"image/color"
	"image/png"
	"io"
	"strings"
)

// One PNG chunk located inside the file
type pngChunk struct {
	typ  string
	data []byte
	end  int
}

// Reports a chunk type made of four letters, digits or underscores
func pngChunkName(b []byte) bool {
	if len(b) != 4 {
		return false
	}
	for _, c := range b {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_') {
			return false
		}
	}
	return true
}

// Chunk header and body at an offset, the CRC verified when asked
func readPNGChunk(data []byte, off int, checkCRC bool) (pngChunk, error) {
	if off+8 > len(data) {
		return pngChunk{}, errors.New("png: truncated file")
	}
	length := int(binary.BigEndian.Uint32(data[off : off+4]))
	name := data[off+4 : off+8]
	if !pngChunkName(name) {
		return pngChunk{}, fmt.Errorf("png: broken chunk %q", name)
	}
	bodyStart := off + 8
	if length < 0 || bodyStart+length > len(data) {
		return pngChunk{}, fmt.Errorf("png: truncated %s chunk", name)
	}
	body := data[bodyStart : bodyStart+length]
	end := bodyStart + length
	if checkCRC {
		if end+4 > len(data) {
			return pngChunk{}, fmt.Errorf("png: incomplete checksum in %s chunk", name)
		}
		crc := crc32.NewIEEE()
		crc.Write(name)
		crc.Write(body)
		if crc.Sum32() != binary.BigEndian.Uint32(data[end:end+4]) {
			return pngChunk{}, fmt.Errorf("png: bad checksum in %s chunk", name)
		}
	}
	return pngChunk{typ: string(name), data: body, end: end + 4}, nil
}

// Header facts gathered while opening a PNG up to its first image data chunk
type pngInfo struct {
	width, height uint32
	depth, color  byte
	interlace     byte
	firstIDAT     int
	plte, trns    []byte
	numFrames     uint32
	defaultImage  bool
	preFcTL       []byte
	seq           int64
}

// Bit depths each PNG colour type allows
var pngDepths = map[byte][]byte{0: {1, 2, 4, 8, 16}, 2: {8, 16}, 3: {1, 2, 4, 8}, 4: {8, 16}, 6: {8, 16}}

// Parses chunks up to the first image data, checking header values, checksums, animation control and the bomb limit
func pngOpen(data []byte) (*pngInfo, error) {
	if !bytes.HasPrefix(data, pngSignature) {
		return nil, ErrUnknownFormat
	}
	info := &pngInfo{seq: -1}
	haveHeader := false
	off := 8
	for {
		if off+8 > len(data) {
			return nil, ErrUnknownFormat
		}
		name := data[off+4 : off+8]
		if !pngChunkName(name) {
			return nil, &CorruptError{Format: PNG, Err: fmt.Errorf("png: broken chunk %q", name)}
		}
		switch string(name) {
		case "IDAT":
			if !haveHeader {
				return nil, ErrUnknownFormat
			}
			info.firstIDAT = off
			if info.numFrames > 0 && info.preFcTL == nil {
				info.defaultImage = true
			}
			if err := checkBomb(int(info.width), int(info.height)); err != nil {
				return nil, err
			}
			return info, info.checkText(data)
		case "IEND":
			return nil, ErrUnknownFormat
		}
		ch, err := readPNGChunk(data, off, true)
		if err != nil {
			return nil, &CorruptError{Format: PNG, Err: err}
		}
		if err := info.accept(ch); err != nil {
			return nil, &CorruptError{Format: PNG, Err: err}
		}
		if ch.typ == "IHDR" {
			haveHeader = true
		}
		off = ch.end
	}
}

// Records one chunk read before the image data
func (info *pngInfo) accept(ch pngChunk) error {
	switch ch.typ {
	case "IHDR":
		if len(ch.data) < 13 {
			return errors.New("png: truncated IHDR chunk")
		}
		info.width = binary.BigEndian.Uint32(ch.data[0:4])
		info.height = binary.BigEndian.Uint32(ch.data[4:8])
		info.depth, info.color = ch.data[8], ch.data[9]
		if !bytes.Contains(pngDepths[info.color], []byte{info.depth}) {
			return fmt.Errorf("png: unknown mode (%d, %d)", info.depth, info.color)
		}
		if ch.data[10] != 0 {
			return errors.New("png: unknown compression method")
		}
		if ch.data[11] != 0 {
			return errors.New("png: unknown filter category")
		}
		info.interlace = ch.data[12]
	case "PLTE":
		info.plte = ch.data
	case "tRNS":
		info.trns = ch.data
	case "acTL":
		if len(ch.data) < 8 {
			return errors.New("png: truncated acTL chunk")
		}
		n := binary.BigEndian.Uint32(ch.data[0:4])
		switch {
		case info.numFrames > 0:
			info.numFrames = 0
		case n == 0 || n > 0x80000000:
		default:
			info.numFrames = n
		}
	case "fcTL":
		if err := info.acceptFrame(ch.data, &info.seq); err != nil {
			return err
		}
		info.preFcTL = ch.data
	}
	return nil
}

// Checks a PNG opens and that every chunk checksum from the first image data through IEND holds
func verifyPNG(data []byte) error {
	info, err := pngOpen(data)
	if err != nil {
		return err
	}
	return pngVerify(data, info)
}

// Walks chunks from the first image data to IEND verifying each checksum
func pngVerify(data []byte, info *pngInfo) error {
	off := info.firstIDAT
	for {
		if off+8 > len(data) {
			return &CorruptError{Format: PNG, Err: errors.New("png: truncated file")}
		}
		if !pngChunkName(data[off+4 : off+8]) {
			return &CorruptError{Format: PNG, Err: fmt.Errorf("png: broken chunk %q", data[off+4:off+8])}
		}
		if string(data[off+4:off+8]) == "IEND" {
			return nil
		}
		ch, err := readPNGChunk(data, off, true)
		if err != nil {
			return &CorruptError{Format: PNG, Err: err}
		}
		off = ch.end
	}
}

// Copy whose chunks from the image data on carry fresh checksums and end in IEND
func pngLenient(data []byte, info *pngInfo) ([]byte, error) {
	out := make([]byte, 0, len(data)+12)
	out = append(out, data[:info.firstIDAT]...)
	off, inData := info.firstIDAT, true
	for off+8 <= len(data) {
		name := data[off+4 : off+8]
		if !pngChunkName(name) {
			if inData {
				return nil, &CorruptError{Format: PNG, Err: fmt.Errorf("png: broken chunk %q", name)}
			}
			break
		}
		ch, err := readPNGChunk(data, off, false)
		if err != nil {
			return nil, &CorruptError{Format: PNG, Err: errors.New("png: image file is truncated")}
		}
		inData = inData && ch.typ == "IDAT"
		out = appendPNGChunk(out, ch.typ, ch.data)
		if ch.typ == "IEND" {
			return out, nil
		}
		off = ch.end
	}
	return appendPNGChunk(out, "IEND", nil), nil
}

// Appends one chunk with its length and checksum
func appendPNGChunk(out []byte, typ string, body []byte) []byte {
	out = binary.BigEndian.AppendUint32(out, uint32(len(body)))
	out = append(out, typ...)
	out = append(out, body...)
	crc := crc32.NewIEEE()
	crc.Write([]byte(typ))
	crc.Write(body)
	return binary.BigEndian.AppendUint32(out, crc.Sum32())
}

// Metadata of a PNG gathered from every chunk
type pngMeta struct {
	icc  []byte
	exif []byte
	xmp  []byte
}

// Inflated bytes one text or profile chunk may reach, and the text all chunks together may hold
const (
	pngChunkInflateMax = 1 << 20
	pngTextMemoryMax   = 64 << 20
)

// Running total of decoded text, refusing a file once its text chunks pass the memory bound
type pngTextBudget struct{ used int }

// Keyword and value one textual chunk carries, compressed forms inflated within the per chunk and total bounds
func (t *pngTextBudget) text(typ string, body []byte) (string, []byte, error) {
	key, rest, _ := bytes.Cut(body, []byte{0})
	switch typ {
	case "zTXt":
		if len(rest) > 0 && rest[0] != 0 {
			return "", nil, fmt.Errorf("png: unknown compression method %d in zTXt chunk", rest[0])
		}
		if len(rest) > 0 {
			v, err := inflate(rest[1:])
			if err != nil {
				return "", nil, err
			}
			rest = v
		}
	case "iTXt":
		if len(rest) < 2 {
			return "", nil, nil
		}
		compressed, method := rest[0] != 0, rest[1]
		parts := bytes.SplitN(rest[2:], []byte{0}, 3)
		if len(parts) < 3 {
			return "", nil, nil
		}
		rest = parts[2]
		if compressed {
			if method != 0 {
				return "", nil, nil
			}
			v, err := inflate(rest)
			if err != nil {
				return "", nil, err
			}
			rest = v
		}
	}
	if len(key) == 0 {
		return "", nil, nil
	}
	if t.used += len(rest); t.used > pngTextMemoryMax {
		return "", nil, errors.New("png: too much memory used in text chunks")
	}
	return string(key), rest, nil
}

// Inflated ICC profile of an iCCP chunk, nil for a corrupt stream
func pngProfile(body []byte) ([]byte, error) {
	i := bytes.IndexByte(body, 0)
	if i+2 > len(body) {
		return nil, errors.New("png: truncated iCCP chunk")
	}
	if body[i+1] != 0 {
		return nil, fmt.Errorf("png: unknown compression method %d in iCCP chunk", body[i+1])
	}
	return inflate(body[i+2:])
}

// Checks the text and profile chunks ahead of the image data inflate within the bounds a PNG opens under
func (info *pngInfo) checkText(data []byte) error {
	var budget pngTextBudget
	for off := 8; off < info.firstIDAT; {
		ch, err := readPNGChunk(data, off, false)
		if err != nil {
			return nil
		}
		switch ch.typ {
		case "tEXt", "zTXt", "iTXt":
			_, _, err = budget.text(ch.typ, ch.data)
		case "iCCP":
			_, err = pngProfile(ch.data)
		}
		if err != nil {
			return &CorruptError{Format: PNG, Err: err}
		}
		off = ch.end
	}
	return nil
}

// ICC profile, Exif and XMP of a PNG, later chunks winning, text Exif used when no eXIf chunk exists
func pngMetadata(data []byte) (pngMeta, error) {
	var m pngMeta
	var budget pngTextBudget
	raw := ""
	for off := 8; ; {
		ch, err := readPNGChunk(data, off, false)
		if err != nil {
			break
		}
		switch ch.typ {
		case "iCCP":
			m.icc, err = pngProfile(ch.data)
		case "eXIf":
			m.exif = ch.data
		case "tEXt", "zTXt", "iTXt":
			var key string
			var value []byte
			key, value, err = budget.text(ch.typ, ch.data)
			switch key {
			case "Raw profile type exif":
				raw = string(value)
			case "XML:com.adobe.xmp":
				m.xmp = value
			}
		}
		if err != nil {
			return pngMeta{}, &CorruptError{Format: PNG, Err: err}
		}
		if ch.typ == "IEND" {
			break
		}
		off = ch.end
	}
	if m.exif == nil && raw != "" {
		lines := strings.Split(raw, "\n")
		if len(lines) > 3 {
			m.exif = hexBytes(strings.Join(lines[3:], ""))
		}
	}
	return m, nil
}

// Bytes of hex text with whitespace ignored, nil when it is not valid hex
func hexBytes(s string) []byte {
	s = strings.Join(strings.Fields(s), "")
	if len(s)%2 != 0 {
		return nil
	}
	out := make([]byte, len(s)/2)
	for i := range out {
		hi, ok1 := hexNibble(s[2*i])
		lo, ok2 := hexNibble(s[2*i+1])
		if !ok1 || !ok2 {
			return nil
		}
		out[i] = hi<<4 | lo
	}
	return out
}

// Value of one hex digit
func hexNibble(c byte) (byte, bool) {
	switch {
	case c >= '0' && c <= '9':
		return c - '0', true
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10, true
	case c >= 'A' && c <= 'F':
		return c - 'A' + 10, true
	}
	return 0, false
}

// Inflated zlib stream of one chunk, nil for a corrupt stream and an error past the per chunk bound
func inflate(b []byte) ([]byte, error) {
	r, err := zlib.NewReader(bytes.NewReader(b))
	if err != nil {
		return nil, nil
	}
	out, err := io.ReadAll(io.LimitReader(r, pngChunkInflateMax+1))
	if len(out) > pngChunkInflateMax {
		return nil, errors.New("png: decompressed data too large")
	}
	if err != nil {
		return nil, nil
	}
	return out, nil
}

// Decodes the default image of a PNG ignoring image data checksums, bilevel grey as a two entry palette
func decodePNG(data []byte) (image.Image, pngMeta, error) {
	info, err := pngOpen(data)
	if err != nil {
		return nil, pngMeta{}, err
	}
	lenient, err := pngLenient(data, info)
	if err != nil {
		return nil, pngMeta{}, err
	}
	if err := info.fits(pngIDATSize(data, info.firstIDAT), info.width, info.height); err != nil {
		return nil, pngMeta{}, err
	}
	meta, err := pngMetadata(data)
	if err != nil {
		return nil, pngMeta{}, err
	}
	img, err := png.Decode(bytes.NewReader(lenient))
	if err != nil {
		return nil, pngMeta{}, &CorruptError{Format: PNG, Err: err}
	}
	if info.color == 0 && info.depth == 1 {
		if g, ok := img.(*image.Gray); ok {
			img = grayToBilevel(g)
		}
	}
	return normalizeDecoded(img), meta, nil
}

// Two entry black and white palette image of a bilevel grey image
func grayToBilevel(g *image.Gray) *image.Paletted {
	b := g.Bounds()
	out := image.NewPaletted(image.Rect(0, 0, b.Dx(), b.Dy()), color.Palette{color.Gray{0}, color.Gray{255}})
	for y := 0; y < b.Dy(); y++ {
		row := g.Pix[g.PixOffset(b.Min.X, b.Min.Y+y):]
		for x := 0; x < b.Dx(); x++ {
			if row[x] >= 128 {
				out.Pix[y*out.Stride+x] = 1
			}
		}
	}
	return out
}

// Inflated byte count a deflate stream reaches at most per compressed byte
const deflateMaxRatio = 1032

// Refuses a frame whose pixel bits outnumber what its compressed data can inflate to
func (info *pngInfo) fits(compressed int, w, h uint32) error {
	channels := [7]uint64{1, 0, 3, 1, 2, 0, 4}[info.color]
	need := uint64(w) * uint64(h) * channels * uint64(info.depth) / 8
	if need > uint64(compressed)*deflateMaxRatio+1024 {
		return &CorruptError{Format: PNG, Err: errors.New("png: image file is truncated")}
	}
	return nil
}

// Summed length of the run of image data chunks starting at an offset
func pngIDATSize(data []byte, off int) int {
	n := 0
	for {
		ch, err := readPNGChunk(data, off, false)
		if err != nil || ch.typ != "IDAT" {
			return n
		}
		n += len(ch.data)
		off = ch.end
	}
}

// Canvas size of a PNG from its header, after the open checks
func configPNG(data []byte) (int, int, error) {
	info, err := pngOpen(data)
	if err != nil {
		return 0, 0, err
	}
	return int(info.width), int(info.height), nil
}

// Frames of a PNG, the default image first and then every frame control and frame data group
type pngFrames struct {
	data    []byte
	info    *pngInfo
	index   int
	pos     int
	payload []byte
	frameW  uint32
	frameH  uint32
	total   int
	seq     int64
}

// Frame source over a PNG after the open checks
func newPNGFrames(data []byte) (*pngFrames, error) {
	info, err := pngOpen(data)
	if err != nil {
		return nil, err
	}
	return &pngFrames{data: data, info: info, index: -1}, nil
}

// Moves to the following frame, reporting the canvas size frames are judged by
func (a *pngFrames) next() (int, int, bool, error) {
	a.index++
	if a.index == 0 {
		a.total = 1
		if a.info.numFrames > 0 {
			a.total = int(a.info.numFrames)
			if a.info.defaultImage {
				a.total++
			}
		}
		a.seq = a.info.seq
		a.frameW, a.frameH = a.info.width, a.info.height
		if a.info.preFcTL != nil {
			a.frameW = binary.BigEndian.Uint32(a.info.preFcTL[4:8])
			a.frameH = binary.BigEndian.Uint32(a.info.preFcTL[8:12])
		}
		var idat []byte
		off := a.info.firstIDAT
		for {
			ch, err := readPNGChunk(a.data, off, false)
			if err != nil || ch.typ != "IDAT" {
				break
			}
			idat = append(idat, ch.data...)
			off = ch.end
		}
		a.payload, a.pos = idat, off
		return int(a.info.width), int(a.info.height), true, nil
	}
	if a.index >= a.total {
		return 0, 0, false, nil
	}
	return a.nextAnimated()
}

// Reads the frame control and frame data chunks of the next animation frame
func (a *pngFrames) nextAnimated() (int, int, bool, error) {
	frameStart := false
	off := a.pos
	for {
		ch, err := readPNGChunk(a.data, off, false)
		if err != nil {
			return 0, 0, false, nil
		}
		switch ch.typ {
		case "IEND":
			return 0, 0, false, nil
		case "fcTL":
			if frameStart {
				return 0, 0, false, &CorruptError{Format: PNG, Err: errors.New("png: animation frame without data")}
			}
			if err := a.info.acceptFrame(ch.data, &a.seq); err != nil {
				return 0, 0, false, &CorruptError{Format: PNG, Err: err}
			}
			a.frameW, a.frameH = binary.BigEndian.Uint32(ch.data[4:8]), binary.BigEndian.Uint32(ch.data[8:12])
			frameStart = true
		case "fdAT":
			if len(ch.data) < 4 {
				return 0, 0, false, &CorruptError{Format: PNG, Err: errors.New("png: truncated fdAT chunk")}
			}
			if seq := int64(binary.BigEndian.Uint32(ch.data[0:4])); a.seq != seq-1 {
				return 0, 0, false, &CorruptError{Format: PNG, Err: errors.New("png: animation frame sequence error")}
			} else {
				a.seq = seq
			}
			if frameStart {
				return a.collectFrame(ch)
			}
		}
		off = ch.end
	}
}

// Gathers the run of frame data chunks that starts at one chunk
func (a *pngFrames) collectFrame(first pngChunk) (int, int, bool, error) {
	payload := append([]byte(nil), first.data[4:]...)
	next := first.end
	for {
		more, err := readPNGChunk(a.data, next, false)
		if err != nil || more.typ != "fdAT" || len(more.data) < 4 {
			break
		}
		seq := int64(binary.BigEndian.Uint32(more.data[0:4]))
		if a.seq != seq-1 {
			return 0, 0, false, &CorruptError{Format: PNG, Err: errors.New("png: animation frame sequence error")}
		}
		a.seq = seq
		payload = append(payload, more.data[4:]...)
		next = more.end
	}
	a.payload, a.pos = payload, next
	return int(a.info.width), int(a.info.height), true, nil
}

// Validates a frame control chunk against the canvas and the running sequence number
func (info *pngInfo) acceptFrame(s []byte, seqp *int64) error {
	if len(s) < 26 {
		return errors.New("png: truncated fcTL chunk")
	}
	seq := int64(binary.BigEndian.Uint32(s[0:4]))
	if (*seqp < 0 && seq != 0) || (*seqp >= 0 && *seqp != seq-1) {
		return errors.New("png: animation frame sequence error")
	}
	*seqp = seq
	w, h := uint64(binary.BigEndian.Uint32(s[4:8])), uint64(binary.BigEndian.Uint32(s[8:12]))
	px, py := uint64(binary.BigEndian.Uint32(s[12:16])), uint64(binary.BigEndian.Uint32(s[16:20]))
	if px+w > uint64(info.width) || py+h > uint64(info.height) {
		return errors.New("png: animation frame outside the canvas")
	}
	return nil
}

// Decodes the current frame from a standalone PNG of its dimensions and data
func (a *pngFrames) decode() error {
	var buf []byte
	buf = append(buf, pngSignature...)
	ihdr := make([]byte, 13)
	binary.BigEndian.PutUint32(ihdr[0:4], a.frameW)
	binary.BigEndian.PutUint32(ihdr[4:8], a.frameH)
	ihdr[8], ihdr[9], ihdr[12] = a.info.depth, a.info.color, a.info.interlace
	buf = appendPNGChunk(buf, "IHDR", ihdr)
	if a.info.plte != nil {
		buf = appendPNGChunk(buf, "PLTE", a.info.plte)
	}
	if a.info.trns != nil {
		buf = appendPNGChunk(buf, "tRNS", a.info.trns)
	}
	buf = appendPNGChunk(buf, "IDAT", a.payload)
	buf = appendPNGChunk(buf, "IEND", nil)
	if err := a.info.fits(len(a.payload), a.frameW, a.frameH); err != nil {
		return err
	}
	if _, err := png.Decode(bytes.NewReader(buf)); err != nil {
		return &CorruptError{Format: PNG, Err: err}
	}
	return nil
}

// Settings of one PNG encoding
type PNGOptions struct {
	// ICC profile written as an iCCP chunk, none when empty
	ICC []byte
}

// Encodes the image as PNG with adaptive row filters at zlib level six, palette images packed to the fewest bits
func EncodePNG(w io.Writer, img image.Image, o PNGOptions) error {
	if err := requireArea(img); err != nil {
		return err
	}
	p := planPNG(img)
	bw := bufio.NewWriter(w)
	var head []byte
	head = append(head, pngSignature...)
	ihdr := make([]byte, 13)
	binary.BigEndian.PutUint32(ihdr[0:4], uint32(p.w))
	binary.BigEndian.PutUint32(ihdr[4:8], uint32(p.h))
	ihdr[8], ihdr[9] = p.depth, p.colorType
	head = appendPNGChunk(head, "IHDR", ihdr)
	if len(o.ICC) > 0 {
		var z bytes.Buffer
		zw := zlib.NewWriter(&z)
		zw.Write(o.ICC)
		zw.Close()
		head = appendPNGChunk(head, "iCCP", append([]byte("ICC Profile\x00\x00"), z.Bytes()...))
	}
	if p.plte != nil {
		head = appendPNGChunk(head, "PLTE", p.plte)
	}
	if p.trns != nil {
		head = appendPNGChunk(head, "tRNS", p.trns)
	}
	if _, err := bw.Write(head); err != nil {
		return err
	}
	idat := &pngIDATWriter{w: bw}
	zw, err := zlib.NewWriterLevel(idat, 6)
	if err != nil {
		return err
	}
	if err := p.writeRows(zw); err != nil {
		return err
	}
	if err := zw.Close(); err != nil {
		return err
	}
	if err := idat.flush(); err != nil {
		return err
	}
	if _, err := bw.Write(appendPNGChunk(nil, "IEND", nil)); err != nil {
		return err
	}
	return bw.Flush()
}

// Writer that frames compressed image data into chunks of at most 64 KiB
type pngIDATWriter struct {
	w   io.Writer
	buf []byte
	err error
}

// Buffers compressed bytes, emitting full chunks as they fill
func (iw *pngIDATWriter) Write(p []byte) (int, error) {
	n := len(p)
	for len(p) > 0 && iw.err == nil {
		take := min(len(p), 65536-len(iw.buf))
		iw.buf = append(iw.buf, p[:take]...)
		p = p[take:]
		if len(iw.buf) == 65536 {
			iw.err = iw.flush()
		}
	}
	return n, iw.err
}

// Emits the buffered bytes as one chunk
func (iw *pngIDATWriter) flush() error {
	if iw.err != nil {
		return iw.err
	}
	if len(iw.buf) == 0 {
		return nil
	}
	_, err := iw.w.Write(appendPNGChunk(nil, "IDAT", iw.buf))
	iw.buf = iw.buf[:0]
	return err
}

// Layout of one PNG encoding, its header values, palette chunks and raw rows
type pngPlan struct {
	w, h      int
	depth     byte
	colorType byte
	plte      []byte
	trns      []byte
	bpp       int
	rowBytes  int
	row       func(y int, dst []byte)
}

// Header values and row packer for the pixel layout of the image
func planPNG(img image.Image) *pngPlan {
	b := img.Bounds()
	p := &pngPlan{w: b.Dx(), h: b.Dy()}
	switch m := img.(type) {
	case *image.Paletted:
		n := max(1, min(len(m.Palette), 256))
		switch {
		case n <= 2:
			p.depth = 1
		case n <= 4:
			p.depth = 2
		case n <= 16:
			p.depth = 4
		default:
			p.depth = 8
		}
		p.colorType = 3
		last := -1
		for i := 0; i < n; i++ {
			var c color.NRGBA
			if i < len(m.Palette) {
				c = color.NRGBAModel.Convert(m.Palette[i]).(color.NRGBA)
			}
			p.plte = append(p.plte, c.R, c.G, c.B)
			if c.A != 255 {
				last = i
			}
		}
		if last >= 0 {
			for i := 0; i <= last; i++ {
				p.trns = append(p.trns, color.NRGBAModel.Convert(m.Palette[i]).(color.NRGBA).A)
			}
		}
		bits := int(p.depth)
		p.bpp, p.rowBytes = 1, (p.w*bits+7)/8
		p.row = func(y int, dst []byte) {
			clear(dst)
			src := m.Pix[m.PixOffset(b.Min.X, b.Min.Y+y):]
			for x := 0; x < p.w; x++ {
				v := src[x]
				if int(v) >= n {
					v = 0
				}
				bit := x * bits
				dst[bit/8] |= v << (8 - bits - bit%8)
			}
		}
	case *image.Gray:
		p.depth, p.colorType, p.bpp, p.rowBytes = 8, 0, 1, p.w
		p.row = func(y int, dst []byte) { copy(dst, m.Pix[m.PixOffset(b.Min.X, b.Min.Y+y):]) }
	case *image.Gray16:
		p.depth, p.colorType, p.bpp, p.rowBytes = 16, 0, 2, p.w*2
		p.row = func(y int, dst []byte) { copy(dst, m.Pix[m.PixOffset(b.Min.X, b.Min.Y+y):]) }
	default:
		if hasAlpha(img) {
			n := ToNRGBA(img)
			p.depth, p.colorType, p.bpp, p.rowBytes = 8, 6, 4, p.w*4
			p.row = func(y int, dst []byte) { copy(dst, n.Pix[y*n.Stride:]) }
			break
		}
		rgb := ToRGB(img)
		p.depth, p.colorType, p.bpp, p.rowBytes = 8, 2, 3, p.w*3
		p.row = func(y int, dst []byte) {
			src := rgb.Pix[y*rgb.Stride:]
			for x := 0; x < p.w; x++ {
				dst[3*x], dst[3*x+1], dst[3*x+2] = src[4*x], src[4*x+1], src[4*x+2]
			}
		}
	}
	return p
}

// Filters and compresses every row, palette rows unfiltered and others by the filter whose output sums nearest zero
func (p *pngPlan) writeRows(w io.Writer) error {
	prev := make([]byte, p.rowBytes)
	cur := make([]byte, p.rowBytes)
	cand := [5][]byte{}
	for i := range cand {
		cand[i] = make([]byte, p.rowBytes+1)
	}
	for y := 0; y < p.h; y++ {
		p.row(y, cur)
		best := cand[0]
		if p.colorType == 3 {
			best[0] = 0
			copy(best[1:], cur)
		} else {
			best = pngFilterRow(cur, prev, p.bpp, cand)
		}
		if _, err := w.Write(best); err != nil {
			return err
		}
		prev, cur = cur, prev
	}
	return nil
}

// Filtered row chosen among none, up, sub and paeth by the smallest sum of absolute signed bytes
func pngFilterRow(cur, prev []byte, bpp int, cand [5][]byte) []byte {
	score := func(b []byte) int {
		s := 0
		for _, v := range b[1:] {
			if v < 128 {
				s += int(v)
			} else {
				s += 256 - int(v)
			}
		}
		return s
	}
	none := cand[0]
	none[0] = 0
	copy(none[1:], cur)
	best, sum := none, score(none)
	if sum > 0 {
		up := cand[2]
		up[0] = 2
		for i := range cur {
			up[i+1] = cur[i] - prev[i]
		}
		if s := score(up); s < sum {
			best, sum = up, s
		}
	}
	if sum > 0 {
		sub := cand[1]
		sub[0] = 1
		for i := range cur {
			var left byte
			if i >= bpp {
				left = cur[i-bpp]
			}
			sub[i+1] = cur[i] - left
		}
		if s := score(sub); s < sum {
			best, sum = sub, s
		}
	}
	if sum > 0 {
		pa := cand[4]
		pa[0] = 4
		for i := range cur {
			var a, c byte
			if i >= bpp {
				a, c = cur[i-bpp], prev[i-bpp]
			}
			pa[i+1] = cur[i] - paeth(a, prev[i], c)
		}
		if s := score(pa); s < sum {
			best = pa
		}
	}
	return best
}

// Paeth predictor of the left, above and upper left bytes
func paeth(a, b, c byte) byte {
	p := int(a) + int(b) - int(c)
	pa, pb, pc := abs(p-int(a)), abs(p-int(b)), abs(p-int(c))
	if pa <= pb && pa <= pc {
		return a
	}
	if pb <= pc {
		return b
	}
	return c
}

// Absolute value of an int
func abs(v int) int {
	if v < 0 {
		return -v
	}
	return v
}
