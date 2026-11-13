package imaging

import (
	"bytes"
	"encoding/binary"
	"regexp"
	"strconv"
)

// One entry of a TIFF image file directory
type ifdEntry struct {
	tag   uint16
	typ   uint16
	count uint32
	raw   []byte
}

// TIFF structure read from a byte stream, its byte order and data
type tiffReader struct {
	data  []byte
	order binary.ByteOrder
}

// Reader over a TIFF header, false when the byte order mark or the magic number is missing
func newTIFFReader(t []byte) (*tiffReader, bool) {
	if len(t) < 8 {
		return nil, false
	}
	var r *tiffReader
	switch string(t[:2]) {
	case "II":
		r = &tiffReader{data: t, order: binary.LittleEndian}
	case "MM":
		r = &tiffReader{data: t, order: binary.BigEndian}
	default:
		return nil, false
	}
	if binary.BigEndian.Uint16(t[2:4]) != 42 && binary.LittleEndian.Uint16(t[2:4]) != 42 {
		return nil, false
	}
	return r, true
}

// Directory field whose twelve bytes start at an offset
func (r *tiffReader) entry(e int) ifdEntry {
	return ifdEntry{tag: r.order.Uint16(r.data[e:]), typ: r.order.Uint16(r.data[e+2:]), count: r.order.Uint32(r.data[e+4:]), raw: r.data[e+8 : e+12]}
}

// Offset of the directory the header points at
func (r *tiffReader) first() int { return int(r.order.Uint32(r.data[4:8])) }

// Entries of the directory at an offset with later duplicates winning, and the next offset, zero after the end
func (r *tiffReader) ifd(off int) (map[uint16]ifdEntry, int, bool) {
	if off < 8 || off+2 > len(r.data) {
		return nil, 0, false
	}
	n := int(r.order.Uint16(r.data[off:]))
	entries := make(map[uint16]ifdEntry, min(n, (len(r.data)-off-2)/12))
	for i := 0; i < n; i++ {
		e := off + 2 + 12*i
		if e+12 > len(r.data) {
			return entries, 0, true
		}
		entry := r.entry(e)
		size := tiffTypeSize(entry.typ) * int(entry.count)
		if size == 0 {
			continue
		}
		if size > 4 && r.bytesOf(entry) == nil {
			return entries, 0, true
		}
		entries[entry.tag] = entry
	}
	next := off + 2 + 12*n
	if next+4 > len(r.data) {
		return entries, 0, true
	}
	return entries, int(r.order.Uint32(r.data[next:])), true
}

// Largest integral value any entry of a tag carries in the directory at an offset
func (r *tiffReader) largest(off int, tag uint16) int {
	if off < 8 || off+2 > len(r.data) {
		return 0
	}
	most := 0
	for i := 0; i < int(r.order.Uint16(r.data[off:])); i++ {
		e := off + 2 + 12*i
		if e+12 > len(r.data) {
			break
		}
		entry := r.entry(e)
		if entry.tag == tag {
			for _, v := range r.ints(entry) {
				most = max(most, v)
			}
		}
	}
	return most
}

// Byte size of one value of a TIFF field type
func tiffTypeSize(typ uint16) int {
	switch typ {
	case 1, 2, 6, 7:
		return 1
	case 3, 8:
		return 2
	case 4, 9, 11, 13:
		return 4
	case 5, 10, 12:
		return 8
	}
	return 0
}

// Raw value bytes of an entry, inline or at its offset, nil when they leave the data
func (r *tiffReader) bytesOf(e ifdEntry) []byte {
	size := tiffTypeSize(e.typ) * int(e.count)
	if size <= 0 {
		return nil
	}
	if size <= 4 {
		return e.raw[:size]
	}
	off := int(r.order.Uint32(e.raw))
	if off < 0 || off+size > len(r.data) || off+size < off {
		return nil
	}
	return r.data[off : off+size]
}

// Integer values of a byte, short or long entry
func (r *tiffReader) ints(e ifdEntry) []int {
	b := r.bytesOf(e)
	if b == nil {
		return nil
	}
	out := make([]int, e.count)
	for i := range out {
		switch e.typ {
		case 1, 6, 7:
			out[i] = int(b[i])
		case 3, 8:
			out[i] = int(r.order.Uint16(b[2*i:]))
		case 4, 9, 13:
			out[i] = int(r.order.Uint32(b[4*i:]))
		default:
			return nil
		}
	}
	return out
}

// First integer value of a tag, false when absent or not integral
func (r *tiffReader) int1(entries map[uint16]ifdEntry, tag uint16) (int, bool) {
	e, ok := entries[tag]
	if !ok {
		return 0, false
	}
	v := r.ints(e)
	if len(v) == 0 {
		return 0, false
	}
	return v[0], true
}

// Orientation tag of an Exif block, false when the block carries none
func exifOrientation(block []byte) (int, bool) {
	block = bytes.TrimPrefix(block, []byte("Exif\x00\x00"))
	r, ok := newTIFFReader(block)
	if !ok {
		return 0, false
	}
	entries, _, ok := r.ifd(r.first())
	if !ok {
		return 0, false
	}
	return r.int1(entries, 0x0112)
}

// Orientation an XMP packet declares in either attribute or element form
var xmpOrientation = regexp.MustCompile(`tiff:Orientation(="|>)([0-9])`)

// Orientation from Exif, else from XMP, one when neither declares it
func orientationOf(exif, xmp []byte) int {
	if v, ok := exifOrientation(exif); ok {
		return v
	}
	if m := xmpOrientation.FindSubmatch(xmp); m != nil {
		v, _ := strconv.Atoi(string(m[2]))
		return v
	}
	return 1
}
