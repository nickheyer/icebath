package imaging

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"image"
	"image/draw"
	"io"

	"github.com/HugoSmits86/nativewebp"
	"golang.org/x/image/webp"
)

// One chunk of a RIFF container
type riffChunk struct {
	fourcc string
	data   []byte
}

// Error for a WebP container a demuxer refuses whole
var errWebPContainer = errors.New("webp: invalid container")

// Chunks of a RIFF body, refusing any chunk or padding that runs past the body
func riffChunks(data []byte) ([]riffChunk, error) {
	var out []riffChunk
	for pos := 0; pos < len(data); {
		if pos+8 > len(data) {
			return nil, io.ErrUnexpectedEOF
		}
		size := int(binary.LittleEndian.Uint32(data[pos+4 : pos+8]))
		start := pos + 8
		if start+size+size&1 > len(data) {
			return nil, io.ErrUnexpectedEOF
		}
		out = append(out, riffChunk{fourcc: string(data[pos : pos+4]), data: data[start : start+size]})
		pos = start + size + size&1
	}
	return out, nil
}

// Size and alpha flag of a VP8 or VP8L bitstream header, false when the header is malformed
func webpBitstream(c riffChunk) (int, int, bool, bool) {
	b := c.data
	switch c.fourcc {
	case "VP8 ":
		if len(b) < 10 || !bytes.Equal(b[3:6], []byte{0x9d, 0x01, 0x2a}) {
			return 0, 0, false, false
		}
		tag := uint32(b[0]) | uint32(b[1])<<8 | uint32(b[2])<<16
		if tag&1 != 0 || tag>>1&7 > 3 || tag>>4&1 == 0 || int(tag>>5) >= len(b) {
			return 0, 0, false, false
		}
		w, h := int(binary.LittleEndian.Uint16(b[6:8])&0x3fff), int(binary.LittleEndian.Uint16(b[8:10])&0x3fff)
		return w, h, false, w > 0 && h > 0
	case "VP8L":
		if len(b) < 5 || b[0] != 0x2f {
			return 0, 0, false, false
		}
		bits := binary.LittleEndian.Uint32(b[1:5])
		return int(bits&0x3fff) + 1, int(bits>>14&0x3fff) + 1, bits&(1<<28) != 0, bits>>29 == 0
	}
	return 0, 0, false, false
}

// Image chunks of one frame, an optional alpha plane ahead of exactly one bitstream
type webpImage struct {
	alph, bitstream riffChunk
	w, h            int
	losslessAlpha   bool
}

// Collects the alpha and bitstream chunks of a frame, refusing alpha after the bitstream or a malformed header
func (im *webpImage) add(c riffChunk) error {
	switch c.fourcc {
	case "ALPH":
		if im.bitstream.fourcc == "" && im.alph.fourcc == "" {
			im.alph = c
		}
	case "VP8 ", "VP8L":
		if im.bitstream.fourcc != "" {
			return errWebPContainer
		}
		w, h, alpha, ok := webpBitstream(c)
		if !ok {
			return errWebPContainer
		}
		im.bitstream, im.w, im.h, im.losslessAlpha = c, w, h, alpha
	}
	return nil
}

// Layout of a WebP file, its canvas, flags, animation frames and metadata
type webpFile struct {
	data     []byte
	canvasW  int
	canvasH  int
	alpha    bool
	animated bool
	frames   []riffChunk
	exif     []byte
	xmp      []byte
	icc      []byte
	extended bool
	lossless bool
}

// Parses the container the way a demuxer accepts a whole file, refusing truncation, stray frames and bad headers
func parseWebP(data []byte) (*webpFile, error) {
	if len(data) < 12 || string(data[:4]) != "RIFF" || string(data[8:12]) != "WEBP" {
		return nil, ErrUnknownFormat
	}
	riff := uint64(binary.LittleEndian.Uint32(data[4:8]))
	if riff < 8 || riff+8 > uint64(len(data)) {
		return nil, &CorruptError{Format: WebP, Err: io.ErrUnexpectedEOF}
	}
	chunks, err := riffChunks(data[12 : 8+riff])
	if err != nil || len(chunks) == 0 {
		return nil, &CorruptError{Format: WebP, Err: io.ErrUnexpectedEOF}
	}
	f := &webpFile{data: data}
	first := chunks[0]
	switch first.fourcc {
	case "VP8X":
		err = f.parseExtended(first, chunks[1:])
	case "VP8 ", "VP8L":
		var ok bool
		f.canvasW, f.canvasH, f.alpha, ok = webpBitstream(first)
		f.lossless = first.fourcc == "VP8L"
		if !ok {
			err = errWebPContainer
		}
	default:
		err = errWebPContainer
	}
	if err != nil {
		return nil, &CorruptError{Format: WebP, Err: err}
	}
	return f, checkBomb(f.canvasW, f.canvasH)
}

// Reads the extended header and the chunks after it, one still image or an ANIM led run of frames
func (f *webpFile) parseExtended(head riffChunk, rest []riffChunk) error {
	if len(head.data) < 10 {
		return errors.New("webp: truncated VP8X chunk")
	}
	f.extended = true
	flags := head.data[0]
	if flags&^0x3e != 0 {
		return errWebPContainer
	}
	f.alpha = flags&0x10 != 0
	f.animated = flags&0x02 != 0
	f.canvasW = int(le24(head.data[4:7])) + 1
	f.canvasH = int(le24(head.data[7:10])) + 1
	var still webpImage
	anim := false
	for _, c := range rest {
		switch c.fourcc {
		case "VP8X":
			return errWebPContainer
		case "ALPH", "VP8 ", "VP8L":
			if f.animated || anim || still.bitstream.fourcc != "" {
				return errWebPContainer
			}
			if err := still.add(c); err != nil {
				return err
			}
			f.lossless = f.lossless || c.fourcc == "VP8L"
		case "ANIM":
			if len(c.data) < 6 {
				return errWebPContainer
			}
			anim = true
		case "ANMF":
			if !anim {
				return errWebPContainer
			}
			if err := f.checkFrame(c); err != nil {
				return err
			}
			if f.animated {
				f.frames = append(f.frames, c)
			}
		case "EXIF":
			f.exif = c.data
		case "XMP ":
			f.xmp = c.data
		case "ICCP":
			f.icc = c.data
		}
	}
	if f.animated {
		if len(f.frames) == 0 {
			return errors.New("webp: animation without frames")
		}
		return nil
	}
	if still.bitstream.fourcc == "" || still.w != f.canvasW || still.h != f.canvasH {
		return errWebPContainer
	}
	if still.bitstream.fourcc == "VP8L" {
		f.alpha = still.losslessAlpha
	}
	f.alpha = f.alpha || still.alph.fourcc != ""
	return nil
}

// Checks an animation frame holds a valid image inside the canvas
func (f *webpFile) checkFrame(c riffChunk) error {
	if len(c.data) < 16 {
		return errors.New("webp: truncated ANMF chunk")
	}
	x, y := 2*int(le24(c.data[0:3])), 2*int(le24(c.data[3:6]))
	w, h := int(le24(c.data[6:9]))+1, int(le24(c.data[9:12]))+1
	sub, err := riffChunks(c.data[16:])
	if err != nil {
		return err
	}
	var im webpImage
	for _, s := range sub {
		if err := im.add(s); err != nil {
			return err
		}
	}
	if im.bitstream.fourcc == "" || x+w > f.canvasW || y+h > f.canvasH {
		return errWebPContainer
	}
	return nil
}

// Decodes a still WebP stream, accepting a lossless bitstream under an alpha flag
func decodeWebPStill(data []byte, extendedLossless bool) (image.Image, error) {
	if extendedLossless {
		return nativewebp.DecodeIgnoreAlphaFlag(bytes.NewReader(data))
	}
	return webp.Decode(bytes.NewReader(data))
}

// Still WebP file around one animation frame bitstream and its alpha plane
func standaloneWebP(fw, fh int, im webpImage) []byte {
	body := []byte("WEBP")
	add := func(fourcc string, data []byte) {
		body = append(body, fourcc...)
		body = binary.LittleEndian.AppendUint32(body, uint32(len(data)))
		body = append(body, data...)
		if len(data)%2 == 1 {
			body = append(body, 0)
		}
	}
	if im.alph.fourcc != "" && im.bitstream.fourcc == "VP8 " {
		vp8x := make([]byte, 10)
		vp8x[0] = 0x10
		wm, hm := uint32(fw-1), uint32(fh-1)
		vp8x[4], vp8x[5], vp8x[6] = byte(wm), byte(wm>>8), byte(wm>>16)
		vp8x[7], vp8x[8], vp8x[9] = byte(hm), byte(hm>>8), byte(hm>>16)
		add("VP8X", vp8x)
		add("ALPH", im.alph.data)
	}
	add(im.bitstream.fourcc, im.bitstream.data)
	out := []byte("RIFF")
	out = binary.LittleEndian.AppendUint32(out, uint32(len(body)))
	return append(out, body...)
}

// Decoded animation picture and its offset on the canvas
func (f *webpFile) frame(i int) (image.Image, image.Point, error) {
	anmf := f.frames[i].data
	at := image.Pt(2*int(le24(anmf[0:3])), 2*int(le24(anmf[3:6])))
	fw, fh := int(le24(anmf[6:9]))+1, int(le24(anmf[9:12]))+1
	sub, err := riffChunks(anmf[16:])
	if err != nil {
		return nil, image.Point{}, &CorruptError{Format: WebP, Err: err}
	}
	var im webpImage
	for _, c := range sub {
		if err := im.add(c); err != nil {
			return nil, image.Point{}, &CorruptError{Format: WebP, Err: err}
		}
	}
	img, err := decodeWebPStill(standaloneWebP(fw, fh, im), false)
	if err != nil {
		return nil, image.Point{}, corrupt(WebP, err)
	}
	return img, at, nil
}

// Decodes the still image or the first animation frame on a clear canvas, opaque unless the file flags alpha
func decodeWebP(data []byte) (image.Image, *webpFile, error) {
	f, err := parseWebP(data)
	if err != nil {
		return nil, nil, err
	}
	var img image.Image
	if f.animated {
		frame, at, err := f.frame(0)
		if err != nil {
			return nil, nil, err
		}
		canvas := image.NewNRGBA(image.Rect(0, 0, f.canvasW, f.canvasH))
		draw.Draw(canvas, image.Rectangle{Min: at, Max: at.Add(frame.Bounds().Size())}, frame, frame.Bounds().Min, draw.Src)
		img = canvas
	} else {
		img, err = decodeWebPStill(data, f.extended && f.lossless)
		if err != nil {
			return nil, nil, corrupt(WebP, err)
		}
	}
	if !f.alpha {
		return ToRGB(img), f, nil
	}
	return ToNRGBA(img), f, nil
}

// Frame source over a still or animated WebP judged at the canvas size
type webpFrames struct {
	f     *webpFile
	index int
}

// Frame source after the container parses
func newWebPFrames(data []byte) (*webpFrames, error) {
	f, err := parseWebP(data)
	if err != nil {
		return nil, err
	}
	return &webpFrames{f: f, index: -1}, nil
}

// Moves to the following frame
func (w *webpFrames) next() (int, int, bool, error) {
	w.index++
	if (!w.f.animated && w.index > 0) || (w.f.animated && w.index >= len(w.f.frames)) {
		return 0, 0, false, nil
	}
	return w.f.canvasW, w.f.canvasH, true, nil
}

// Decodes the current frame
func (w *webpFrames) decode() error {
	if !w.f.animated {
		_, err := decodeWebPStill(w.f.data, w.f.extended && w.f.lossless)
		return corrupt(WebP, err)
	}
	_, _, err := w.f.frame(w.index)
	return err
}

// Encodes the image as lossless WebP at the slowest and smallest setting, colour under clear pixels kept exactly
func EncodeWebP(w io.Writer, img image.Image) error {
	if err := requireArea(img); err != nil {
		return err
	}
	if b := img.Bounds(); b.Dx() > 16384 || b.Dy() > 16384 {
		return fmt.Errorf("%w: webp side over 16384 pixels", ErrUnsupported)
	}
	o := &nativewebp.Options{CompressionLevel: nativewebp.BestCompression}
	if p, ok := img.(*image.Paletted); ok {
		return nativewebp.Encode(w, p, o)
	}
	return nativewebp.Encode(w, ToNRGBA(img), o)
}
