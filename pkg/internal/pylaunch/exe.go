package pylaunch

import (
	"bytes"
	"encoding/binary"
	"hash/crc32"
	"math"
	"strings"
	"unicode/utf8"
)

// ExeInterpreter is the interpreter a distlib native launcher embeds, loader stub then a "#!<python> -I" shebang then the zip, false for any other layout
func ExeInterpreter(data []byte) (string, bool) {
	offset, ok := zipFirstHeaderOffset(data)
	if !ok {
		return "", false
	}
	prefix := data[:sliceEnd(offset, len(data))]
	start := bytes.LastIndexByte(prefix, 0) + 1
	at := bytes.Index(prefix[start:], []byte("#!"))
	var shebang []byte
	if at >= 0 {
		shebang = bytes.TrimRight(prefix[start+at+2:], "\r\n")
	}
	if !bytes.HasSuffix(shebang, []byte(" -I")) || bytes.Contains(shebang, []byte("\n")) {
		return "", false
	}
	exe := shebang[:len(shebang)-3]
	if !utf8.Valid(exe) {
		return "", false
	}
	return strings.Trim(string(exe), `"`), true
}

// End index of a Python slice up to an offset, a negative offset counting from the end
func sliceEnd(offset int64, length int) int {
	if offset < 0 {
		offset += int64(length)
		if offset < 0 {
			return 0
		}
	}
	if offset > int64(length) {
		return length
	}
	return int(offset)
}

// File offset of the first central directory entry's local header the way zipfile reads it, false when it would raise
func zipFirstHeaderOffset(data []byte) (int64, bool) {
	const sizeEndCentDir, sizeCentralDir, sizeLocator64, sizeEnd64, maxComment = 22, 46, 20, 56, 65535
	size := int64(len(data))
	if size < sizeEndCentDir {
		return 0, false
	}
	var rec []byte
	location := size - sizeEndCentDir
	if tail := data[location:]; bytes.HasPrefix(tail, []byte("PK\x05\x06")) && bytes.HasSuffix(tail, []byte{0, 0}) {
		rec = tail
	} else {
		maxStart := max(size-maxComment-sizeEndCentDir, 0)
		window := data[maxStart:]
		start := bytes.LastIndex(window, []byte("PK\x05\x06"))
		if start < 0 || int64(len(window)-start) < sizeEndCentDir {
			return 0, false
		}
		rec = window[start : start+sizeEndCentDir]
		location = maxStart + int64(start)
	}
	cdSize := int64(binary.LittleEndian.Uint32(rec[12:16]))
	cdOffset := int64(binary.LittleEndian.Uint32(rec[16:20]))
	if loc := location - sizeLocator64; loc >= 0 {
		locator := data[loc : loc+sizeLocator64]
		if bytes.HasPrefix(locator, []byte("PK\x06\x07")) {
			diskno := binary.LittleEndian.Uint32(locator[4:8])
			rawReloff := binary.LittleEndian.Uint64(locator[8:16])
			disks := binary.LittleEndian.Uint32(locator[16:20])
			if diskno != 0 || disks > 1 {
				return 0, false
			}
			offset := loc - sizeEnd64
			if rawReloff > math.MaxInt64 || int64(rawReloff) > offset {
				return 0, false
			}
			reloff := int64(rawReloff)
			extra := offset - reloff
			record := pyRead(data, reloff, sizeEnd64)
			if len(record) != sizeEnd64 {
				return 0, false
			}
			if !bytes.HasPrefix(record, []byte("PK\x06\x06")) && reloff != offset {
				extra = 0
				record = pyRead(data, offset, sizeEnd64)
				if len(record) != sizeEnd64 {
					return 0, false
				}
			}
			if !bytes.HasPrefix(record, []byte("PK\x06\x06")) {
				return 0, false
			}
			sz := binary.LittleEndian.Uint64(record[4:12])
			dirsize := binary.LittleEndian.Uint64(record[40:48])
			diroffset := binary.LittleEndian.Uint64(record[48:56])
			if diroffset > math.MaxUint64-dirsize || diroffset+dirsize != uint64(reloff) || sz > math.MaxUint64-12 || sz+12 != uint64(sizeEnd64+extra) {
				return 0, false
			}
			cdSize, cdOffset, location = int64(dirsize), int64(diroffset), offset-extra
		}
	}
	concat := location - cdSize - cdOffset
	startDir := cdOffset + concat
	if startDir < 0 {
		return 0, false
	}
	dir := pyRead(data, startDir, cdSize)
	first, found := int64(0), false
	pos := int64(0)
	for total := int64(0); total < cdSize; {
		entry := pyRead(dir, pos, sizeCentralDir)
		pos += int64(len(entry))
		if len(entry) != sizeCentralDir || !bytes.HasPrefix(entry, []byte("PK\x01\x02")) {
			return 0, false
		}
		flags := binary.LittleEndian.Uint16(entry[8:10])
		compressSize := uint64(binary.LittleEndian.Uint32(entry[20:24]))
		fileSize := uint64(binary.LittleEndian.Uint32(entry[24:28]))
		nameLen := int64(binary.LittleEndian.Uint16(entry[28:30]))
		extraLen := int64(binary.LittleEndian.Uint16(entry[30:32]))
		commentLen := int64(binary.LittleEndian.Uint16(entry[32:34]))
		headerOffset := uint64(binary.LittleEndian.Uint32(entry[42:46]))
		name := pyRead(dir, pos, nameLen)
		pos += int64(len(name))
		if flags&0x800 != 0 && !utf8.Valid(name) {
			return 0, false
		}
		extra := pyRead(dir, pos, extraLen)
		pos += int64(len(extra))
		pos += int64(len(pyRead(dir, pos, commentLen)))
		if entry[6] > 63 {
			return 0, false
		}
		var ok bool
		if headerOffset, ok = zipDecodeExtra(extra, fileSize, compressSize, headerOffset, crc32.ChecksumIEEE(name)); !ok {
			return 0, false
		}
		if !found {
			first, found = int64(headerOffset)+concat, true
		}
		total += sizeCentralDir + nameLen + extraLen + commentLen
	}
	return first, found
}

// Header offset after the zip64 and unicode path extra fields of an entry, false where zipfile raises BadZipFile
func zipDecodeExtra(extra []byte, fileSize, compressSize, headerOffset uint64, nameCRC uint32) (uint64, bool) {
	for len(extra) >= 4 {
		tp := binary.LittleEndian.Uint16(extra[0:2])
		ln := int(binary.LittleEndian.Uint16(extra[2:4]))
		if ln+4 > len(extra) {
			return 0, false
		}
		field := extra[4 : ln+4]
		switch tp {
		case 0x0001:
			if fileSize == 0xffffffff || fileSize == 0xffffffffffffffff {
				if len(field) < 8 {
					return 0, false
				}
				fileSize, field = binary.LittleEndian.Uint64(field[:8]), field[8:]
			}
			if compressSize == 0xffffffff {
				if len(field) < 8 {
					return 0, false
				}
				compressSize, field = binary.LittleEndian.Uint64(field[:8]), field[8:]
			}
			if headerOffset == 0xffffffff {
				if len(field) < 8 {
					return 0, false
				}
				headerOffset = binary.LittleEndian.Uint64(field[:8])
			}
		case 0x7075:
			if len(field) < 5 {
				return 0, false
			}
			if field[0] == 1 && binary.LittleEndian.Uint32(field[1:5]) == nameCRC && !utf8.Valid(field[5:]) {
				return 0, false
			}
		}
		extra = extra[ln+4:]
	}
	return headerOffset, true
}

// Bytes a Python file read of length n at offset returns, short or empty past the end
func pyRead(data []byte, offset, n int64) []byte {
	if offset >= int64(len(data)) || n <= 0 {
		return nil
	}
	return data[offset:min(offset+n, int64(len(data)))]
}
