package imaging

import (
	"bytes"
	"encoding/binary"
	"errors"
	"image"
	"image/jpeg"
	"sort"
)

// One marker segment of a JPEG header with its payload
type jpegSegment struct {
	marker byte
	start  int
	body   []byte
}

// Marker segments ahead of the first scan, stopping at the first malformed one
func jpegSegments(data []byte) []jpegSegment {
	var out []jpegSegment
	pos := 2
	for pos+4 <= len(data) {
		if data[pos] != 0xff {
			return out
		}
		marker := data[pos+1]
		if marker == 0xff {
			pos++
			continue
		}
		if marker == 0xd8 || marker == 0x01 || (marker >= 0xd0 && marker <= 0xd7) {
			pos += 2
			continue
		}
		if marker == 0xda || marker == 0xd9 {
			return out
		}
		length := int(binary.BigEndian.Uint16(data[pos+2 : pos+4]))
		end := pos + 2 + length
		if length < 2 || end > len(data) {
			return out
		}
		out = append(out, jpegSegment{marker: marker, start: pos + 4, body: data[pos+4 : end]})
		pos = end
	}
	return out
}

// Exif, XMP and ICC profile carried by the JPEG header
type jpegMeta struct {
	exif []byte
	xmp  []byte
	icc  []byte
}

// Metadata of a JPEG, the first Exif block and the ICC profile reassembled from its numbered parts
func jpegMetadata(data []byte) jpegMeta {
	var m jpegMeta
	var icc [][]byte
	for _, s := range jpegSegments(data) {
		switch {
		case s.marker == 0xe1 && bytes.HasPrefix(s.body, []byte("Exif\x00\x00")) && m.exif == nil:
			m.exif = s.body
		case s.marker == 0xe1 && bytes.HasPrefix(s.body, []byte("http://ns.adobe.com/xap/1.0/\x00")):
			m.xmp = s.body[len("http://ns.adobe.com/xap/1.0/\x00"):]
		case s.marker == 0xe2 && bytes.HasPrefix(s.body, []byte("ICC_PROFILE\x00")):
			icc = append(icc, s.body)
		}
	}
	if len(icc) > 0 {
		sort.SliceStable(icc, func(i, j int) bool { return bytes.Compare(icc[i], icc[j]) < 0 })
		if len(icc[0]) > 13 && int(icc[0][13]) == len(icc) {
			for _, p := range icc {
				if len(p) >= 14 {
					m.icc = append(m.icc, p[14:]...)
				}
			}
		}
	}
	return m
}

// Offsets of every picture a multi picture index lists, false for a plain JPEG or an HDR gain map file
func mpoOffsets(data []byte) ([]int, bool) {
	var mp []byte
	mpOffset := 0
	for _, s := range jpegSegments(data) {
		if s.marker == 0xe1 && bytes.Contains(s.body, []byte(` hdrgm:Version="`)) {
			return nil, false
		}
		if s.marker == 0xe2 && bytes.HasPrefix(s.body, []byte("MPF\x00")) {
			mp = s.body[4:]
			mpOffset = s.start + 4
		}
	}
	r, ok := newTIFFReader(mp)
	if !ok {
		return nil, false
	}
	entries, _, ok := r.ifd(r.first())
	if !ok {
		return nil, false
	}
	count, ok := r.int1(entries, 0xb001)
	if !ok || count <= 1 {
		return nil, false
	}
	list := r.bytesOf(entries[0xb002])
	if len(list) < count*16 {
		return nil, false
	}
	offs := make([]int, count)
	for i := range offs {
		offs[i] = int(r.order.Uint32(list[i*16+8:])) + mpOffset
	}
	offs[0] = 0
	return offs, true
}

// Pixels a JPEG reaches at most per byte, every 8 by 8 block costing at least one entropy coded bit
const jpegMaxPixelsPerByte = 512

// Frame header of the JPEG stream within the bomb limit and the pixels its bytes can carry
func jpegConfig(data []byte) (image.Config, error) {
	cfg, err := jpeg.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return cfg, corrupt(JPEG, err)
	}
	if err := checkBomb(cfg.Width, cfg.Height); err != nil {
		return cfg, err
	}
	return cfg, nil
}

// Decodes a JPEG stream once its frame header has passed the bomb limit and the truncation bound
func decodeJPEGStream(data []byte) (image.Image, error) {
	cfg, err := jpegConfig(data)
	if err != nil {
		return nil, err
	}
	if cfg.Width*cfg.Height > jpegMaxPixelsPerByte*len(data)+4096 {
		return nil, &CorruptError{Format: JPEG, Err: errors.New("jpeg: image file is truncated")}
	}
	img, err := jpeg.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, corrupt(JPEG, err)
	}
	return img, nil
}

// Decodes the first picture of a JPEG with its metadata
func decodeJPEG(data []byte) (image.Image, jpegMeta, error) {
	img, err := decodeJPEGStream(data)
	if err != nil {
		return nil, jpegMeta{}, err
	}
	return normalizeDecoded(img), jpegMetadata(data), nil
}

// Pictures of a JPEG, every picture a multi picture index lists judged at the size its own frame header declares
type jpegFrames struct {
	data    []byte
	offsets []int
	index   int
}

// Frame source over a JPEG after its first frame header parses
func newJPEGFrames(data []byte) (*jpegFrames, error) {
	if _, err := jpegConfig(data); err != nil {
		return nil, err
	}
	j := &jpegFrames{data: data, offsets: []int{0}, index: -1}
	if offs, ok := mpoOffsets(data); ok {
		j.offsets = offs
	}
	return j, nil
}

// Moves to the following picture and reads its frame header
func (j *jpegFrames) next() (int, int, bool, error) {
	j.index++
	if j.index >= len(j.offsets) {
		return 0, 0, false, nil
	}
	off := j.offsets[j.index]
	if off < 0 || off+2 >= len(j.data) {
		return 0, 0, false, &CorruptError{Format: JPEG, Err: errNoFrameData}
	}
	cfg, err := jpegConfig(j.data[off:])
	if err != nil {
		return 0, 0, false, err
	}
	return cfg.Width, cfg.Height, true, nil
}

// Decodes the current picture
func (j *jpegFrames) decode() error {
	_, err := decodeJPEGStream(j.data[j.offsets[j.index]:])
	return err
}
