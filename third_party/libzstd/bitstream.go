package libzstd

import (
	"encoding/binary"
	"math/bits"
)

// BIT_DStream_status values
const (
	bitUnfinished = iota
	bitEndOfBuffer
	bitCompleted
	bitOverflow
)

// BIT_DStream_t reading a backward bitstream held in buf between start and its end
type bitDStream struct {
	buf          []byte
	bitContainer uint64
	bitsConsumed uint32
	ptr          int
	start        int
	limitPtr     int
	zeroFilled   bool
}

// Little endian load of n bytes at i, bytes past the slice taken from its backing array and zero past that
func readLEn(b []byte, i, n int) uint64 {
	if i+n > len(b) {
		b = b[:cap(b)]
	}
	var v uint64
	for k := 0; k < n && i+k < len(b); k++ {
		v |= uint64(b[i+k]) << (8 * uint(k))
	}
	return v
}

func readLE16(b []byte, i int) uint32 { return uint32(readLEn(b, i, 2)) }
func readLE24(b []byte, i int) uint32 { return uint32(readLEn(b, i, 3)) }
func readLE32(b []byte, i int) uint32 {
	if i+4 <= len(b) {
		return binary.LittleEndian.Uint32(b[i:])
	}
	return uint32(readLEn(b, i, 4))
}
func readLE64(b []byte, i int) uint64 {
	if i+8 <= len(b) {
		return binary.LittleEndian.Uint64(b[i:])
	}
	return readLEn(b, i, 8)
}

// ZSTD_highbit32, position of the highest set bit of a nonzero value
func highbit32(v uint32) uint32 { return uint32(bits.Len32(v)) - 1 }

// BIT_initDStream over srcSize bytes of buf from start, the size read or the error of an empty or unterminated stream
func (d *bitDStream) init(buf []byte, start, srcSize int) (int, ErrorCode) {
	if srcSize < 1 {
		*d = bitDStream{}
		return 0, ErrSrcSizeWrong
	}
	d.buf, d.start, d.limitPtr, d.zeroFilled = buf, start, start+8, false
	lastByte := buf[start+srcSize-1]
	if srcSize >= 8 {
		d.ptr = start + srcSize - 8
		d.bitContainer = readLE64(buf, d.ptr)
		d.bitsConsumed = 0
		if lastByte != 0 {
			d.bitsConsumed = 8 - highbit32(uint32(lastByte))
		}
		if lastByte == 0 {
			return 0, ErrGeneric
		}
		return srcSize, 0
	}
	d.ptr = start
	d.bitContainer = uint64(buf[start])
	for i := srcSize - 1; i >= 1; i-- {
		d.bitContainer += uint64(buf[start+i]) << uint(8*i)
	}
	d.bitsConsumed = 0
	if lastByte != 0 {
		d.bitsConsumed = 8 - highbit32(uint32(lastByte))
	}
	if lastByte == 0 {
		return 0, ErrCorruptionDetected
	}
	d.bitsConsumed += uint32(8-srcSize) * 8
	return srcSize, 0
}

// BIT_lookBits, the next nbBits without consuming them
func (d *bitDStream) lookBits(nbBits uint32) uint64 {
	return (d.bitContainer >> ((64 - d.bitsConsumed - nbBits) & 63)) & (uint64(1)<<nbBits - 1)
}

// BIT_lookBitsFast, valid only for one bit or more
func (d *bitDStream) lookBitsFast(nbBits uint32) uint64 {
	return (d.bitContainer << (d.bitsConsumed & 63)) >> ((64 - nbBits) & 63)
}

func (d *bitDStream) skipBits(nbBits uint32) { d.bitsConsumed += nbBits }

// BIT_readBits
func (d *bitDStream) readBits(nbBits uint32) uint64 {
	v := d.lookBits(nbBits)
	d.bitsConsumed += nbBits
	return v
}

// BIT_readBitsFast
func (d *bitDStream) readBitsFast(nbBits uint32) uint64 {
	v := d.lookBitsFast(nbBits)
	d.bitsConsumed += nbBits
	return v
}

func (d *bitDStream) reloadInternal() int {
	d.ptr -= int(d.bitsConsumed >> 3)
	d.bitsConsumed &= 7
	d.bitContainer = readLE64(d.buf, d.ptr)
	return bitUnfinished
}

// BIT_reloadDStreamFast, refusing once the container sits within its first eight bytes
func (d *bitDStream) reloadFast() int {
	if d.zeroFilled || d.ptr < d.limitPtr {
		return bitOverflow
	}
	return d.reloadInternal()
}

// BIT_reloadDStream refilling the container, the status telling how much of the stream is left
func (d *bitDStream) reload() int {
	if d.bitsConsumed > 64 {
		d.zeroFilled = true
		return bitOverflow
	}
	if d.ptr >= d.limitPtr {
		return d.reloadInternal()
	}
	if d.ptr == d.start {
		if d.bitsConsumed < 64 {
			return bitEndOfBuffer
		}
		return bitCompleted
	}
	nbBytes := int(d.bitsConsumed >> 3)
	result := bitUnfinished
	if d.ptr-nbBytes < d.start {
		nbBytes = d.ptr - d.start
		result = bitEndOfBuffer
	}
	d.ptr -= nbBytes
	d.bitsConsumed -= uint32(nbBytes) * 8
	d.bitContainer = readLE64(d.buf, d.ptr)
	return result
}

// BIT_endOfDStream, every bit consumed and none beyond
func (d *bitDStream) endOfDStream() bool {
	return !d.zeroFilled && d.ptr == d.start && d.bitsConsumed == 64
}
