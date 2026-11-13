package imaging

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"image"

	"github.com/gen2brain/avif"
	"github.com/gen2brain/heic"
)

// One ISO-BMFF box with its payload after the header
type bmffBox struct {
	typ  string
	body []byte
}

// Boxes laid end to end in a byte run, stopping at the first malformed one
func bmffBoxes(data []byte) []bmffBox {
	var out []bmffBox
	for pos := 0; pos+8 <= len(data); {
		size := uint64(binary.BigEndian.Uint32(data[pos:]))
		typ := string(data[pos+4 : pos+8])
		head := uint64(8)
		switch size {
		case 0:
			size = uint64(len(data) - pos)
		case 1:
			if pos+16 > len(data) {
				return out
			}
			size, head = binary.BigEndian.Uint64(data[pos+8:]), 16
		}
		if size < head || size > uint64(len(data)-pos) {
			return out
		}
		out = append(out, bmffBox{typ: typ, body: data[uint64(pos)+head : uint64(pos)+size]})
		pos += int(size)
	}
	return out
}

// First box of a type among a run of boxes
func findBox(boxes []bmffBox, typ string) (bmffBox, bool) {
	for _, b := range boxes {
		if b.typ == typ {
			return b, true
		}
	}
	return bmffBox{}, false
}

// ICC profile of the colour property attached to the primary item of an HEIF or AVIF file
func heifICC(data []byte) []byte {
	meta, ok := findBox(bmffBoxes(data), "meta")
	if !ok || len(meta.body) < 4 {
		return nil
	}
	children := bmffBoxes(meta.body[4:])
	primary := uint32(0)
	if pitm, ok := findBox(children, "pitm"); ok && len(pitm.body) >= 6 {
		if pitm.body[0] == 0 {
			primary = uint32(binary.BigEndian.Uint16(pitm.body[4:]))
		} else if len(pitm.body) >= 8 {
			primary = binary.BigEndian.Uint32(pitm.body[4:])
		}
	}
	iprp, ok := findBox(children, "iprp")
	if !ok {
		return nil
	}
	props := bmffBoxes(iprp.body)
	ipco, ok := findBox(props, "ipco")
	if !ok {
		return nil
	}
	properties := bmffBoxes(ipco.body)
	for _, b := range props {
		if b.typ != "ipma" {
			continue
		}
		for _, idx := range ipmaIndices(b.body, primary) {
			if idx < 1 || idx > len(properties) {
				continue
			}
			p := properties[idx-1]
			if p.typ == "colr" && len(p.body) > 4 && (string(p.body[:4]) == "prof" || string(p.body[:4]) == "rICC") {
				return p.body[4:]
			}
		}
	}
	return nil
}

// Property indices an item association box lists for one item
func ipmaIndices(body []byte, item uint32) []int {
	if len(body) < 8 {
		return nil
	}
	version, flags := body[0], body[3]
	n := binary.BigEndian.Uint32(body[4:])
	pos := 8
	for i := uint32(0); i < n; i++ {
		var id uint32
		if version < 1 {
			if pos+2 > len(body) {
				return nil
			}
			id, pos = uint32(binary.BigEndian.Uint16(body[pos:])), pos+2
		} else {
			if pos+4 > len(body) {
				return nil
			}
			id, pos = binary.BigEndian.Uint32(body[pos:]), pos+4
		}
		if pos >= len(body) {
			return nil
		}
		count := int(body[pos])
		pos++
		var out []int
		for j := 0; j < count; j++ {
			if flags&1 != 0 {
				if pos+2 > len(body) {
					return nil
				}
				out, pos = append(out, int(binary.BigEndian.Uint16(body[pos:])&0x7fff)), pos+2
			} else {
				if pos+1 > len(body) {
					return nil
				}
				out, pos = append(out, int(body[pos]&0x7f)), pos+1
			}
		}
		if id == item {
			return out
		}
	}
	return nil
}

// Runs a call into an ISO-BMFF codec library, a runtime fault it raises on a malformed file returned as corruption
func heifCall(f Format, fn func() error) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = &CorruptError{Format: f, Err: fmt.Errorf("%s: decoder fault: %v", f, r)}
		}
	}()
	return corrupt(f, fn())
}

// Canvas size the codec library reads from the container, refused over the bomb limit
func heifConfig(data []byte, f Format) (image.Config, error) {
	var cfg image.Config
	err := heifCall(f, func() error {
		var err error
		if f == HEIC {
			cfg, err = heic.DecodeConfig(bytes.NewReader(data))
		} else {
			cfg, err = avif.DecodeConfig(bytes.NewReader(data))
		}
		return err
	})
	if err != nil {
		return cfg, err
	}
	return cfg, checkBomb(cfg.Width, cfg.Height)
}

// Decodes the primary HEIC image with the container transformations applied
func decodeHEIC(data []byte) (image.Image, error) {
	if _, err := heifConfig(data, HEIC); err != nil {
		return nil, err
	}
	var img image.Image
	err := heifCall(HEIC, func() error {
		var err error
		if img, err = heic.Decode(bytes.NewReader(data)); err != nil {
			return err
		}
		img, err = ownPixels(img)
		return err
	})
	if err != nil {
		return nil, err
	}
	return normalizeDecoded(img), nil
}

// Decodes the primary AVIF image, rotated and mirrored by its container properties when asked
func decodeAVIF(data []byte, orient bool) (image.Image, error) {
	if _, err := heifConfig(data, AVIF); err != nil {
		return nil, err
	}
	var img image.Image
	err := heifCall(AVIF, func() error {
		var err error
		if img, err = avif.Decode(bytes.NewReader(data), avif.Options{AutoRotate: orient}); err != nil {
			return err
		}
		img, err = ownPixels(img)
		return err
	})
	if err != nil {
		return nil, err
	}
	if !hasAlpha(img) {
		return ToRGB(img), nil
	}
	return normalizeDecoded(img), nil
}

// Error for decoder output whose pixel buffer stops short of its bounds
var errShortPixels = errors.New("decoder returned fewer pixels than its bounds")

// Reports whether a buffer of rows at a stride holds every byte the bounds need
func covers(pix []uint8, stride, w, h, bpp int) bool {
	return w <= 0 || h <= 0 || (stride >= w*bpp && len(pix) >= stride*(h-1)+w*bpp)
}

// Library decoder output checked to hold every pixel its bounds promise, layouts of other types copied through their pixel reader
func ownPixels(img image.Image) (image.Image, error) {
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	ok := true
	switch m := img.(type) {
	case *image.RGBA:
		ok = covers(m.Pix, m.Stride, w, h, 4)
	case *image.NRGBA:
		ok = covers(m.Pix, m.Stride, w, h, 4)
	case *image.RGBA64:
		ok = covers(m.Pix, m.Stride, w, h, 8)
	case *image.NRGBA64:
		ok = covers(m.Pix, m.Stride, w, h, 8)
	case *image.Gray:
		ok = covers(m.Pix, m.Stride, w, h, 1)
	case *image.Gray16:
		ok = covers(m.Pix, m.Stride, w, h, 2)
	case *image.CMYK:
		ok = covers(m.Pix, m.Stride, w, h, 4)
	case *image.YCbCr:
		cw, ch := chromaSize(m.SubsampleRatio, b)
		ok = covers(m.Y, m.YStride, w, h, 1) && covers(m.Cb, m.CStride, cw, ch, 1) && covers(m.Cr, m.CStride, cw, ch, 1)
	case *image.NYCbCrA:
		cw, ch := chromaSize(m.SubsampleRatio, b)
		ok = covers(m.Y, m.YStride, w, h, 1) && covers(m.Cb, m.CStride, cw, ch, 1) && covers(m.Cr, m.CStride, cw, ch, 1) && covers(m.A, m.AStride, w, h, 1)
	default:
		out := image.NewNRGBA(image.Rect(0, 0, w, h))
		for y := 0; y < h; y++ {
			for x := 0; x < w; x++ {
				out.Set(x, y, img.At(b.Min.X+x, b.Min.Y+y))
			}
		}
		return out, nil
	}
	if !ok {
		return nil, errShortPixels
	}
	return img, nil
}

// Chroma plane size of a YCbCr image over its bounds
func chromaSize(r image.YCbCrSubsampleRatio, b image.Rectangle) (int, int) {
	switch r {
	case image.YCbCrSubsampleRatio422:
		return (b.Max.X+1)/2 - b.Min.X/2, b.Dy()
	case image.YCbCrSubsampleRatio420:
		return (b.Max.X+1)/2 - b.Min.X/2, (b.Max.Y+1)/2 - b.Min.Y/2
	case image.YCbCrSubsampleRatio440:
		return b.Dx(), (b.Max.Y+1)/2 - b.Min.Y/2
	case image.YCbCrSubsampleRatio411:
		return (b.Max.X+3)/4 - b.Min.X/4, b.Dy()
	case image.YCbCrSubsampleRatio410:
		return (b.Max.X+3)/4 - b.Min.X/4, (b.Max.Y+1)/2 - b.Min.Y/2
	}
	return b.Dx(), b.Dy()
}

// Frames an ISO-BMFF file holds, the sample count of its longest track or one for a still item
func heifFrameCount(data []byte) (int, error) {
	moov, ok := findBox(bmffBoxes(data), "moov")
	if !ok {
		return 1, nil
	}
	most := 0
	for _, trak := range bmffBoxes(moov.body) {
		if trak.typ != "trak" {
			continue
		}
		box := trak
		for _, typ := range []string{"mdia", "minf", "stbl"} {
			if box, ok = findBox(bmffBoxes(box.body), typ); !ok {
				break
			}
		}
		if !ok {
			continue
		}
		for _, b := range bmffBoxes(box.body) {
			if (b.typ == "stsz" || b.typ == "stz2") && len(b.body) >= 12 {
				most = max(most, int(binary.BigEndian.Uint32(b.body[8:12])))
			}
		}
	}
	if most == 0 {
		return 0, errors.New("heif: image sequence without samples")
	}
	return most, nil
}

// Frame source over the images of an HEIC or AVIF sequence, decoded together once the container shows the limits hold
type heifFrames struct {
	frames []image.Image
	index  int
}

// Every frame of an HEIC or AVIF file, refused undecoded when its frame count or summed area passes a bound
func newHEIFFrames(data []byte, f Format, lim Limits) (*heifFrames, error) {
	cfg, err := heifConfig(data, f)
	if err != nil {
		return nil, err
	}
	n, err := heifFrameCount(data)
	if err != nil {
		return nil, &CorruptError{Format: f, Err: err}
	}
	area := cfg.Width * cfg.Height
	for frame := 1; frame <= n; frame++ {
		if lim.MaxFrames > 0 && frame > lim.MaxFrames {
			return nil, &FrameLimitError{Frame: frame, Max: lim.MaxFrames}
		}
		if lim.MaxPixels > 0 && frame*area > lim.MaxPixels {
			return nil, &PixelLimitError{Frame: frame, Pixels: frame * area, Max: lim.MaxPixels}
		}
	}
	var frames []image.Image
	err = heifCall(f, func() error {
		var decoded []image.Image
		if f == HEIC {
			all, err := heic.DecodeAll(bytes.NewReader(data))
			if err != nil {
				return err
			}
			decoded = all.Image
		} else {
			all, err := avif.DecodeAll(bytes.NewReader(data))
			if err != nil {
				return err
			}
			decoded = all.Image
		}
		for _, frame := range decoded {
			own, err := ownPixels(frame)
			if err != nil {
				return err
			}
			frames = append(frames, own)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if len(frames) == 0 {
		return nil, &CorruptError{Format: f, Err: errors.New("heif: no decodable frame")}
	}
	return &heifFrames{frames: frames, index: -1}, nil
}

// Moves to the following frame at its decoded size
func (h *heifFrames) next() (int, int, bool, error) {
	h.index++
	if h.index >= len(h.frames) {
		return 0, 0, false, nil
	}
	b := h.frames[h.index].Bounds()
	return b.Dx(), b.Dy(), true, nil
}

// Frames decoded when the source opened within its limits, so each one here is already whole
func (h *heifFrames) decode() error { return nil }
