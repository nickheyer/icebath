package pythonutil

import (
	"bytes"
	"encoding/binary"
	"strings"
	"unicode/utf8"
)

// Interpreter path a Windows console script launcher executable embeds: the loader stub, a "#!<python> -I" line, then a zip archive; false for any other layout
func LauncherInterpreter(data []byte) (string, bool) {
	start, ok := zipStart(data)
	if !ok {
		return "", false
	}
	prefix := data[:start]
	line := prefix[bytes.LastIndexByte(prefix, 0)+1:]
	at := bytes.Index(line, []byte("#!"))
	if at < 0 {
		return "", false
	}
	shebang := bytes.TrimRight(line[at+2:], "\r\n")
	exe, ok := bytes.CutSuffix(shebang, []byte(" -I"))
	if !ok || bytes.IndexByte(shebang, '\n') >= 0 || !utf8.Valid(exe) {
		return "", false
	}
	return strings.Trim(string(exe), `"`), true
}

// Offset of the first local file header of the zip archive that ends data, found through its end of central directory record
func zipStart(data []byte) (int, bool) {
	const eocdSize, entrySize, locatorSize, eocd64Size, maxComment = 22, 46, 20, 56, 0xffff
	size := int64(len(data))
	if size < eocdSize {
		return 0, false
	}
	eocd := size - eocdSize
	if tail := data[eocd:]; !bytes.HasPrefix(tail, []byte("PK\x05\x06")) || tail[eocdSize-2] != 0 || tail[eocdSize-1] != 0 {
		from := max(size-eocdSize-maxComment, 0)
		at := bytes.LastIndex(data[from:], []byte("PK\x05\x06"))
		if at < 0 || size-(from+int64(at)) < eocdSize {
			return 0, false
		}
		eocd = from + int64(at)
	}
	rec := data[eocd : eocd+eocdSize]
	dirSize := uint64(binary.LittleEndian.Uint32(rec[12:16]))
	dirOffset := uint64(binary.LittleEndian.Uint32(rec[16:20]))
	dirEnd := eocd
	if loc := eocd - locatorSize; loc >= eocd64Size && bytes.HasPrefix(data[loc:], []byte("PK\x06\x07")) {
		rec64 := data[loc-eocd64Size : loc]
		if !bytes.HasPrefix(rec64, []byte("PK\x06\x06")) {
			return 0, false
		}
		dirSize = binary.LittleEndian.Uint64(rec64[40:48])
		dirOffset = binary.LittleEndian.Uint64(rec64[48:56])
		dirEnd = loc - eocd64Size
	}
	if dirSize < entrySize || dirSize > uint64(dirEnd) || dirOffset > uint64(dirEnd)-dirSize {
		return 0, false
	}
	base := uint64(dirEnd) - dirSize - dirOffset
	entry := data[base+dirOffset : base+dirOffset+entrySize]
	if !bytes.HasPrefix(entry, []byte("PK\x01\x02")) {
		return 0, false
	}
	headerOffset := uint64(binary.LittleEndian.Uint32(entry[42:46]))
	if headerOffset == 0xffffffff {
		nameLen := uint64(binary.LittleEndian.Uint16(entry[28:30]))
		extraLen := uint64(binary.LittleEndian.Uint16(entry[30:32]))
		extraAt := base + dirOffset + entrySize + nameLen
		if extraAt+extraLen > uint64(dirEnd) {
			return 0, false
		}
		var ok bool
		if headerOffset, ok = zip64HeaderOffset(data[extraAt:extraAt+extraLen], entry); !ok {
			return 0, false
		}
	}
	if headerOffset > uint64(size)-4-base {
		return 0, false
	}
	start := base + headerOffset
	if !bytes.HasPrefix(data[start:], []byte("PK\x03\x04")) {
		return 0, false
	}
	return int(start), true
}

// Local header offset a zip64 extra field records for a central directory entry whose 32-bit offset is saturated
func zip64HeaderOffset(extra, entry []byte) (uint64, bool) {
	for len(extra) >= 4 {
		tag := binary.LittleEndian.Uint16(extra[0:2])
		n := int(binary.LittleEndian.Uint16(extra[2:4]))
		if n+4 > len(extra) {
			return 0, false
		}
		field := extra[4 : 4+n]
		if tag == 0x0001 {
			if binary.LittleEndian.Uint32(entry[24:28]) == 0xffffffff {
				if len(field) < 8 {
					return 0, false
				}
				field = field[8:]
			}
			if binary.LittleEndian.Uint32(entry[20:24]) == 0xffffffff {
				if len(field) < 8 {
					return 0, false
				}
				field = field[8:]
			}
			if len(field) < 8 {
				return 0, false
			}
			return binary.LittleEndian.Uint64(field[:8]), true
		}
		extra = extra[4+n:]
	}
	return 0, false
}
