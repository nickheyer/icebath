package ziputil

import (
	"archive/zip"
	"encoding/binary"
	"hash/crc32"
	"runtime"
	"strings"
	"unicode/utf8"

	"golang.org/x/text/encoding/charmap"
)

// Gives a member its name and comment as text: an Info-ZIP Unicode path or comment field when its checksum matches the stored bytes, else the stored bytes, which stay as they are when they are valid UTF-8 or the archive was made on a system other than MS-DOS, OS/2, NTFS or VFAT, and are read as code page 437 otherwise; the name is cut at a NUL, and backslashes become separators in archives made on those DOS-family systems and on Windows
func decodeName(f *zip.File) {
	dos := dosHost(f.CreatorVersion >> 8)
	name, ok := unicodeField(f.Extra, extraUnicodePath, f.Name)
	if !ok {
		name = legacyText(f.Name, dos)
	}
	comment, ok := unicodeField(f.Extra, extraUnicodeComment, f.Comment)
	if !ok {
		comment = legacyText(f.Comment, dos)
	}
	if i := strings.IndexByte(name, 0); i >= 0 {
		name = name[:i]
	}
	if dos || runtime.GOOS == "windows" {
		name = strings.ReplaceAll(name, `\`, "/")
	}
	f.Name, f.Comment = name, comment
	f.NonUTF8 = !utf8.ValidString(name) || !utf8.ValidString(comment)
}

// UTF-8 text an Info-ZIP Unicode path or comment extra field of the given id records, when its checksum matches the stored text and it is valid UTF-8
func unicodeField(extra []byte, id uint16, stored string) (string, bool) {
	for len(extra) >= 4 {
		tag, size := binary.LittleEndian.Uint16(extra), int(binary.LittleEndian.Uint16(extra[2:]))
		if 4+size > len(extra) {
			break
		}
		data := extra[4 : 4+size]
		extra = extra[4+size:]
		if tag == id && size > 5 && data[0] == 1 &&
			binary.LittleEndian.Uint32(data[1:]) == crc32.ChecksumIEEE([]byte(stored)) && utf8.Valid(data[5:]) {
			return string(data[5:]), true
		}
	}
	return "", false
}

// s itself when it is valid UTF-8 or comes from a system whose tools write their own local bytes, else s read as code page 437
func legacyText(s string, dos bool) string {
	if !dos || utf8.ValidString(s) {
		return s
	}
	var b strings.Builder
	b.Grow(2 * len(s))
	for i := range len(s) {
		b.WriteRune(charmap.CodePage437.DecodeByte(s[i]))
	}
	return b.String()
}

// Whether a creator host number names MS-DOS, OS/2, NTFS or VFAT, whose tools write code page 437 and backslashes between name elements
func dosHost(host uint16) bool {
	switch host {
	case 0, 6, 11, 14:
		return true
	}
	return false
}
