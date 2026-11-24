package plistutil

import (
	"encoding/binary"
	"fmt"
	"math"
	"time"
)

// Seconds from the Unix epoch to the reference date of binary plist dates, 2001-01-01 UTC
const appleEpochUnix = 978307200

// States of an object while a binary plist is read, the zero state meaning not read yet
const (
	reading uint8 = iota + 1
	read
)

// Reader of one bplist00 document
type binaryReader struct {
	data       []byte
	offsets    []byte
	offsetSize int
	refSize    int
	objects    []any
	state      []uint8
	depth      int
}

func corrupt(format string, args ...any) error {
	return &Error{Msg: "corrupt binary property list: " + fmt.Sprintf(format, args...)}
}

// Value of a bplist00 document, shared objects read once and reference cycles refused
func decodeBinary(data []byte) (any, error) {
	const trailerSize = 32
	if len(data) < len(binaryMagic)+trailerSize+1 {
		return nil, corrupt("too short")
	}
	body := uint64(len(data) - trailerSize)
	trailer := data[body:]
	offsetSize, refSize := int(trailer[6]), int(trailer[7])
	count := binary.BigEndian.Uint64(trailer[8:])
	top := binary.BigEndian.Uint64(trailer[16:])
	table := binary.BigEndian.Uint64(trailer[24:])
	switch {
	case offsetSize < 1 || offsetSize > 8 || refSize < 1 || refSize > 8:
		return nil, corrupt("offset size %d or reference size %d out of range", offsetSize, refSize)
	case count == 0 || top >= count:
		return nil, corrupt("top object %d of %d objects", top, count)
	case table < uint64(len(binaryMagic)) || table > body || count > (body-table)/uint64(offsetSize):
		return nil, corrupt("offset table out of bounds")
	}
	r := &binaryReader{
		data:       data[:body],
		offsets:    data[table : table+count*uint64(offsetSize)],
		offsetSize: offsetSize,
		refSize:    refSize,
		objects:    make([]any, count),
		state:      make([]uint8, count),
	}
	return r.object(top)
}

// Big endian unsigned integer of up to eight bytes
func readUint(b []byte) uint64 {
	var v uint64
	for _, c := range b {
		v = v<<8 | uint64(c)
	}
	return v
}

// Object a reference points at, read on first use
func (r *binaryReader) object(ref uint64) (any, error) {
	if ref >= uint64(len(r.state)) {
		return nil, corrupt("reference %d out of range", ref)
	}
	switch r.state[ref] {
	case read:
		return r.objects[ref], nil
	case reading:
		return nil, corrupt("reference cycle through object %d", ref)
	}
	at := ref * uint64(r.offsetSize)
	offset := readUint(r.offsets[at : at+uint64(r.offsetSize)])
	if offset < uint64(len(binaryMagic)) || offset >= uint64(len(r.data)) {
		return nil, corrupt("object %d offset out of bounds", ref)
	}
	r.state[ref] = reading
	v, err := r.parse(int(offset))
	if err != nil {
		return nil, err
	}
	r.objects[ref], r.state[ref] = v, read
	return v, nil
}

// Next n bytes from pos
func (r *binaryReader) bytes(pos, n int) ([]byte, error) {
	if n < 0 || pos > len(r.data) || n > len(r.data)-pos {
		return nil, corrupt("object at %d runs past the end", pos)
	}
	return r.data[pos : pos+n], nil
}

// Element count of an object whose marker holds low, read from the following integer when low is 0xF, and the position after it
func (r *binaryReader) length(low byte, pos int) (int, int, error) {
	if low != 0x0F {
		return int(low), pos, nil
	}
	marker, err := r.bytes(pos, 1)
	if err != nil {
		return 0, 0, err
	}
	if marker[0]>>4 != 0x1 || marker[0]&0x0F > 3 {
		return 0, 0, corrupt("invalid length marker 0x%02x at %d", marker[0], pos)
	}
	size := 1 << (marker[0] & 0x0F)
	raw, err := r.bytes(pos+1, size)
	if err != nil {
		return 0, 0, err
	}
	n := readUint(raw)
	if n > uint64(len(r.data)) {
		return 0, 0, corrupt("length %d at %d beyond the document", n, pos)
	}
	return int(n), pos + 1 + size, nil
}

// Object references of a container: n of them from pos
func (r *binaryReader) refs(pos, n int) ([]uint64, error) {
	if n > (len(r.data)-pos)/r.refSize {
		return nil, corrupt("references at %d run past the end", pos)
	}
	raw, err := r.bytes(pos, n*r.refSize)
	if err != nil {
		return nil, err
	}
	out := make([]uint64, n)
	for i := range out {
		out[i] = readUint(raw[i*r.refSize : (i+1)*r.refSize])
	}
	return out, nil
}

// Object whose marker byte sits at pos
func (r *binaryReader) parse(pos int) (any, error) {
	marker := r.data[pos]
	high, low := marker>>4, marker&0x0F
	pos++
	switch high {
	case 0x0:
		switch marker {
		case 0x00, 0x0F:
			return nil, nil
		case 0x08:
			return false, nil
		case 0x09:
			return true, nil
		}
	case 0x1:
		if low > 4 {
			break
		}
		raw, err := r.bytes(pos, 1<<low)
		if err != nil {
			return nil, err
		}
		return binaryInteger(raw, pos)
	case 0x2:
		switch low {
		case 2:
			raw, err := r.bytes(pos, 4)
			if err != nil {
				return nil, err
			}
			return float64(math.Float32frombits(binary.BigEndian.Uint32(raw))), nil
		case 3:
			raw, err := r.bytes(pos, 8)
			if err != nil {
				return nil, err
			}
			return math.Float64frombits(binary.BigEndian.Uint64(raw)), nil
		}
	case 0x3:
		if marker != 0x33 {
			break
		}
		raw, err := r.bytes(pos, 8)
		if err != nil {
			return nil, err
		}
		return appleTime(math.Float64frombits(binary.BigEndian.Uint64(raw)), pos)
	case 0x4, 0x5, 0x6:
		n, start, err := r.length(low, pos)
		if err != nil {
			return nil, err
		}
		if high == 0x6 {
			if n > len(r.data)/2 {
				return nil, corrupt("string length %d at %d beyond the document", n, pos)
			}
			n *= 2
		}
		raw, err := r.bytes(start, n)
		if err != nil {
			return nil, err
		}
		switch high {
		case 0x4:
			return append([]byte(nil), raw...), nil
		case 0x5:
			for _, c := range raw {
				if c >= 0x80 {
					return nil, corrupt("non-ASCII byte in an ASCII string at %d", start)
				}
			}
			return string(raw), nil
		}
		return string(appendUTF16(make([]byte, 0, len(raw)), raw, binary.BigEndian)), nil
	case 0x8:
		raw, err := r.bytes(pos, int(low)+1)
		if err != nil {
			return nil, err
		}
		if len(raw) > 8 {
			for _, c := range raw[:len(raw)-8] {
				if c != 0 {
					return nil, corrupt("UID at %d beyond 64 bits", pos)
				}
			}
			raw = raw[len(raw)-8:]
		}
		return UID(readUint(raw)), nil
	case 0xA, 0xC:
		return r.array(low, pos)
	case 0xD:
		return r.dict(low, pos)
	}
	return nil, corrupt("unknown object marker 0x%02x at %d", marker, pos-1)
}

// Integer of 1, 2, 4, 8 or 16 big endian bytes: the short ones unsigned, eight bytes signed, sixteen bytes as int64 or, above its range, uint64
func binaryInteger(raw []byte, pos int) (any, error) {
	switch len(raw) {
	case 8:
		return int64(binary.BigEndian.Uint64(raw)), nil
	case 16:
		hi, lo := binary.BigEndian.Uint64(raw), binary.BigEndian.Uint64(raw[8:])
		switch {
		case hi == 0 && lo > math.MaxInt64:
			return lo, nil
		case hi == 0:
			return int64(lo), nil
		case hi == math.MaxUint64 && lo > math.MaxInt64:
			return int64(lo), nil
		}
		return nil, corrupt("integer at %d beyond 64 bits", pos)
	}
	return int64(readUint(raw)), nil
}

// Time of a binary plist date, seconds since 2001-01-01 UTC kept to the microsecond
func appleTime(seconds float64, pos int) (time.Time, error) {
	if math.IsNaN(seconds) || math.IsInf(seconds, 0) || math.Abs(seconds) > 4e11 {
		return time.Time{}, corrupt("date at %d out of range", pos)
	}
	whole := math.Floor(seconds)
	micros := math.RoundToEven((seconds - whole) * 1e6)
	t := time.Unix(appleEpochUnix+int64(whole), int64(micros)*1000).UTC()
	if t.Year() < 1 || t.Year() > 9999 {
		return time.Time{}, corrupt("date at %d out of range", pos)
	}
	return t, nil
}

func (r *binaryReader) enter(pos int) error {
	if r.depth++; r.depth > maxDepth {
		return corrupt("nesting too deep at %d", pos)
	}
	return nil
}

// Array or set whose marker holds low, its elements following pos
func (r *binaryReader) array(low byte, pos int) ([]any, error) {
	if err := r.enter(pos); err != nil {
		return nil, err
	}
	defer func() { r.depth-- }()
	n, start, err := r.length(low, pos)
	if err != nil {
		return nil, err
	}
	refs, err := r.refs(start, n)
	if err != nil {
		return nil, err
	}
	items := make([]any, len(refs))
	for i, ref := range refs {
		if items[i], err = r.object(ref); err != nil {
			return nil, err
		}
	}
	return items, nil
}

// Dictionary whose marker holds low, its key references and then its value references following pos
func (r *binaryReader) dict(low byte, pos int) (map[string]any, error) {
	if err := r.enter(pos); err != nil {
		return nil, err
	}
	defer func() { r.depth-- }()
	n, start, err := r.length(low, pos)
	if err != nil {
		return nil, err
	}
	if n > len(r.data)/2 {
		return nil, corrupt("dictionary size %d at %d beyond the document", n, pos)
	}
	refs, err := r.refs(start, 2*n)
	if err != nil {
		return nil, err
	}
	d := make(map[string]any, n)
	for i := range n {
		k, err := r.object(refs[i])
		if err != nil {
			return nil, err
		}
		key, ok := k.(string)
		if !ok {
			return nil, corrupt("dictionary key at %d is not a string", start)
		}
		if d[key], err = r.object(refs[n+i]); err != nil {
			return nil, err
		}
	}
	return d, nil
}
