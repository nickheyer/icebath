package tools

import (
	"bytes"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"math"
	"os"
	"strings"
	"unicode/utf8"
)

// Embedded interpreter an existing launcher starts, false when the layout is not one icebath or hermes publishes
func launcherPython(target string) (string, bool) {
	data, err := os.ReadFile(target)
	if err != nil {
		return "", false
	}
	suffix := strings.ToLower(pathlibSuffix(target))
	if suffix == ".exe" {
		offset, ok := zipFirstHeaderOffset(data)
		if !ok {
			return "", false
		}
		prefix := data[:pySliceEnd(offset, len(data))]
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
	text, ok := decodeUTF8Sig(data)
	if !ok {
		return "", false
	}
	lines := pySplitLines(text)
	if suffix == ".cmd" {
		for _, entry := range lines[min(1, len(lines)):] {
			if strings.TrimFunc(entry, pyIsSpace) != "" {
				return launcherFirstToken(entry)
			}
		}
		return "", false
	}
	for _, entry := range lines {
		if strings.HasPrefix(entry, "exec ") {
			token, present, err := shlexPosixFirstToken(strings.TrimPrefix(entry, "exec "))
			if err != nil || !present || token == "" {
				return "", false
			}
			return token, true
		}
	}
	return "", false
}

// Strict UTF-8 text with one leading byte order mark dropped, false when the bytes are not UTF-8
func decodeUTF8Sig(data []byte) (string, bool) {
	data = bytes.TrimPrefix(data, []byte{0xef, 0xbb, 0xbf})
	if !utf8.Valid(data) {
		return "", false
	}
	return string(data), true
}

// End index of a Python slice up to an offset, a negative offset counting from the end
func pySliceEnd(offset int64, length int) int {
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

// First whitespace delimited token of a line, honouring one quote pair, false when the line is blank or unclosed
func launcherFirstToken(line string) (string, bool) {
	text := strings.TrimFunc(line, pyIsSpace)
	if text == "" {
		return "", false
	}
	if text[0] == '"' || text[0] == '\'' {
		end := strings.IndexByte(text[1:], text[0])
		if end == -1 {
			return "", false
		}
		return text[1 : end+1], true
	}
	return strings.FieldsFunc(text, pyIsSpace)[0], true
}

// Error shlex raises when input ends right after an escape character
var errShlexNoEscapedCharacter = errors.New("No escaped character")

// First token of a POSIX whitespace split shlex, false at end of input, an error on unclosed quotes or escapes
func shlexPosixFirstToken(s string) (string, bool, error) {
	const whitespace, quotes = " \t\r\n", `'"`
	runes := []rune(s)
	var token strings.Builder
	state, escaped := ' ', ' '
	quoted := false
	pos := 0
	next := func() (rune, bool) {
		if pos >= len(runes) {
			return 0, false
		}
		r := runes[pos]
		pos++
		return r, true
	}
	skipLine := func() {
		for pos < len(runes) {
			r := runes[pos]
			pos++
			if r == '\n' {
				return
			}
		}
	}
	for {
		c, ok := next()
		switch {
		case state == ' ':
			switch {
			case !ok:
				return shlexResult(token.String(), quoted)
			case strings.ContainsRune(whitespace, c):
				if token.Len() > 0 || quoted {
					return shlexResult(token.String(), quoted)
				}
			case c == '#':
				skipLine()
			case c == '\\':
				escaped, state = 'a', '\\'
			case strings.ContainsRune(quotes, c):
				state = c
			default:
				token.WriteRune(c)
				state = 'a'
			}
		case state == '\'' || state == '"':
			quoted = true
			switch {
			case !ok:
				return "", false, errUnbalancedQuotes
			case c == state:
				state = 'a'
			case c == '\\' && state == '"':
				escaped, state = state, '\\'
			default:
				token.WriteRune(c)
			}
		case state == '\\':
			if !ok {
				return "", false, errShlexNoEscapedCharacter
			}
			if (escaped == '\'' || escaped == '"') && c != state && c != escaped {
				token.WriteRune(state)
			}
			token.WriteRune(c)
			state = escaped
		case state == 'a':
			switch {
			case !ok:
				return shlexResult(token.String(), quoted)
			case strings.ContainsRune(whitespace, c):
				state = ' '
				if token.Len() > 0 || quoted {
					return shlexResult(token.String(), quoted)
				}
			case c == '#':
				skipLine()
				state = ' '
				if token.Len() > 0 || quoted {
					return shlexResult(token.String(), quoted)
				}
			case strings.ContainsRune(quotes, c):
				state = c
			case c == '\\':
				escaped, state = 'a', '\\'
			default:
				token.WriteRune(c)
			}
		}
	}
}

// Token shlex returns, an unquoted empty token meaning the end of input
func shlexResult(token string, quoted bool) (string, bool, error) {
	if !quoted && token == "" {
		return "", false, nil
	}
	return token, true, nil
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

// Last suffix of the final path component the way pathlib reports it, leading dots ignored
func pathlibSuffix(p string) string {
	name := strings.TrimLeft(pathlibName(p), ".")
	if i := strings.LastIndexByte(name, '.'); i != -1 {
		return name[i:]
	}
	return ""
}
