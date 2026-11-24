package ziputil

import (
	"encoding/binary"
	"slices"
	"time"
	"unicode/utf8"
)

// Method number of Deflate64 compressed members, which readers decode
const Deflate64 uint16 = 9

// Method number of bzip2 compressed members, which readers decode
const Bzip2 uint16 = 12

// Method number of LZMA compressed members, which readers decode
const LZMA uint16 = 14

// Method number of Zstandard compressed members, which readers decode and writers produce
const Zstd uint16 = 93

// Method number of XZ compressed members, which readers decode
const XZ uint16 = 95

const (
	uint16max = 1<<16 - 1
	uint32max = 1<<32 - 1

	// Method number archives made before the Zstandard number was settled use
	zstdPKWare = 20

	sigLocal      = 0x04034b50
	sigCentral    = 0x02014b50
	sigEnd        = 0x06054b50
	sigEnd64      = 0x06064b50
	sigLocator64  = 0x07064b50
	sigDescriptor = 0x08074b50

	localLen      = 30
	end64Len      = 56
	locator64Len  = 20
	descriptorLen = 24

	flagEncrypted        = 0x1
	flagEndMarker        = 0x2
	flagDataDescriptor   = 0x8
	flagPatched          = 0x20
	flagStrongEncryption = 0x40
	flagUTF8             = 0x800

	extraZip64          = 0x0001
	extraTimestamp      = 0x5455
	extraUnicodePath    = 0x7075
	extraUnicodeComment = 0x6375

	zip64Version = 45

	// Length of the zip64 extra record a local header reserves for the two sizes, and of the largest one a directory record carries, with the offset too
	zip64LocalLen = 4 + 16
	zip64ExtraMax = 4 + 24

	// Size hint from which a local header reserves room for 64-bit sizes, below the limit by more than any compressor overhead
	zip64Reserve = uint32max - 1<<25
)

// Version of the zip specification needed to extract a member of method
func methodVersion(method uint16) uint16 {
	switch method {
	case Deflate64:
		return 21
	case Bzip2:
		return 46
	case LZMA, Zstd, XZ:
		return 63
	}
	return 20
}

// Whether s is valid UTF-8, and whether it needs the UTF-8 flag because it holds characters outside the range code page 437 and the legacy multibyte sets agree on
func detectUTF8(s string) (valid, require bool) {
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		i += size
		if r < 0x20 || r > 0x7d || r == 0x5c {
			if r == utf8.RuneError && size == 1 {
				return false, false
			}
			require = true
		}
	}
	return true, require
}

// MS-DOS date and time fields of t in its own zone, clamped to the 1980 to 2107 range they hold, the zero time giving 1980-01-01 00:00
func dosDateTime(t time.Time) (date, clock uint16) {
	switch {
	case t.Year() < 1980:
		return 1<<5 | 1, 0
	case t.Year() > 2107:
		return 127<<9 | 12<<5 | 31, 23<<11 | 59<<5 | 29
	}
	return uint16((t.Year()-1980)<<9 | int(t.Month())<<5 | t.Day()), uint16(t.Hour()<<11 | t.Minute()<<5 | t.Second()/2)
}

// Extra field with an extended timestamp record of t as Unix seconds appended, unchanged when t is zero or outside what 32 bits hold
func appendTimestamp(extra []byte, t time.Time) []byte {
	if t.IsZero() {
		return extra
	}
	sec := t.Unix()
	if sec < 0 || sec > uint32max {
		return extra
	}
	extra = binary.LittleEndian.AppendUint16(extra, extraTimestamp)
	extra = binary.LittleEndian.AppendUint16(extra, 5)
	extra = append(extra, 1)
	return binary.LittleEndian.AppendUint32(extra, uint32(sec))
}

// Copy of an extra field without its records of the given ids, a malformed tail kept as it is
func stripExtra(extra []byte, ids ...uint16) []byte {
	out := make([]byte, 0, len(extra)+9)
	for len(extra) >= 4 {
		id, size := binary.LittleEndian.Uint16(extra), int(binary.LittleEndian.Uint16(extra[2:]))
		if 4+size > len(extra) {
			break
		}
		if !slices.Contains(ids, id) {
			out = append(out, extra[:4+size]...)
		}
		extra = extra[4+size:]
	}
	return append(out, extra...)
}
