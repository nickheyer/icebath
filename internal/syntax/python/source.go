package python

import (
	"bytes"
	"fmt"
	"strings"
	"unicode/utf8"

	"golang.org/x/text/encoding"
	"golang.org/x/text/encoding/charmap"
	"golang.org/x/text/encoding/japanese"
	"golang.org/x/text/encoding/korean"
	"golang.org/x/text/encoding/simplifiedchinese"
	"golang.org/x/text/encoding/traditionalchinese"
	"golang.org/x/text/encoding/unicode"
)

// Error the reference raises for source holding a NUL byte, which carries no line
func nullBytesError() *SyntaxError {
	return &SyntaxError{Msg: "source code string cannot contain null bytes", unplaced: true}
}

// Converts CRLF and CR to LF and terminates the last line only for file input
func translateLines(src []byte, exec bool) []byte {
	out := make([]byte, 0, len(src)+1)
	for i := 0; i < len(src); i++ {
		c := src[i]
		if c == '\r' {
			if i+1 < len(src) && src[i+1] == '\n' {
				i++
			}
			c = '\n'
		}
		out = append(out, c)
	}
	if exec && (len(out) == 0 || out[len(out)-1] != '\n') {
		out = append(out, '\n')
	}
	return out
}

// Encoding named by a coding declaration on one line, false when the line declares none
func codingSpec(line []byte) (string, bool) {
	i := 0
	for ; i < len(line)-6; i++ {
		if line[i] == '#' {
			break
		}
		if line[i] != ' ' && line[i] != '\t' && line[i] != '\014' {
			return "", false
		}
	}
	for ; i < len(line)-6; i++ {
		if !bytes.HasPrefix(line[i:], []byte("coding")) {
			continue
		}
		t := i + 6
		if t >= len(line) || (line[t] != ':' && line[t] != '=') {
			continue
		}
		t++
		for t < len(line) && (line[t] == ' ' || line[t] == '\t') {
			t++
		}
		begin := t
		for t < len(line) && (isAlnumByte(line[t]) || line[t] == '-' || line[t] == '_' || line[t] == '.') {
			t++
		}
		if begin < t {
			return normalName(string(line[begin:t])), true
		}
	}
	return "", false
}

func isAlnumByte(c byte) bool {
	return (c >= '0' && c <= '9') || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

// Canonical spelling of utf-8 and latin-1 declarations, other declarations kept as written
func normalName(s string) string {
	buf := make([]byte, 0, 12)
	for i := 0; i < len(s) && i < 12; i++ {
		c := s[i]
		if c == '_' {
			c = '-'
		} else if c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		}
		buf = append(buf, c)
	}
	b := string(buf)
	switch {
	case b == "utf-8" || strings.HasPrefix(b, "utf-8-"):
		return "utf-8"
	case b == "latin-1" || b == "iso-8859-1" || b == "iso-latin-1" || strings.HasPrefix(b, "latin-1-") || strings.HasPrefix(b, "iso-8859-1-") || strings.HasPrefix(b, "iso-latin-1-"):
		return "iso-8859-1"
	}
	return s
}

// Reports whether a line holds only blanks and an optional comment, so the declaration search continues
func onlyCommentLine(line []byte) bool {
	for _, c := range line {
		if c == '#' || c == '\n' || c == '\r' {
			return true
		}
		if c != ' ' && c != '\t' && c != '\014' {
			return false
		}
	}
	return true
}

// End of the line starting at start and the start of the line after it
func splitLine(src []byte, start int) (int, int) {
	end := start
	for end < len(src) && src[end] != '\n' {
		end++
	}
	next := end
	if next < len(src) {
		next++
	}
	return end, next
}

// Number of characters in UTF-8 bytes, the unit of error columns
func charCount(b []byte) int {
	return utf8.RuneCount(b)
}

// Decodes source bytes to UTF-8 by the byte order mark, a coding declaration on the first two lines, or UTF-8
func decodeSource(src []byte) ([]byte, *SyntaxError) {
	bom := bytes.HasPrefix(src, []byte("\xef\xbb\xbf"))
	if bom {
		src = src[3:]
	}
	enc := ""
	pos := 0
	for lineNo := 1; lineNo <= 2 && pos < len(src); lineNo++ {
		end, next := splitLine(src, pos)
		line := src[pos:end]
		if spec, ok := codingSpec(line); ok {
			if bom && spec != "utf-8" {
				return nil, &SyntaxError{Msg: "encoding problem: " + spec + " with BOM", Line: lineNo, Col: 0, EndLine: lineNo, EndCol: charCount(line), Text: string(line)}
			}
			enc = spec
			break
		}
		if !onlyCommentLine(line) {
			break
		}
		pos = next
	}
	if enc == "" || enc == "utf-8" {
		if bad := firstInvalidUTF8(src); bad >= 0 {
			lineno := bytes.Count(src[:bad], []byte("\n")) + 1
			ls := bytes.LastIndexByte(src[:bad], '\n') + 1
			le, _ := splitLine(src, ls)
			off := charCount(src[ls:bad]) + 1
			msg := fmt.Sprintf("Non-UTF-8 code starting with '\\x%02x' on line %d, but no encoding declared; see https://peps.python.org/pep-0263/ for details", src[bad], lineno)
			return nil, &SyntaxError{Msg: msg, Line: lineno, Col: off, EndLine: lineno, EndCol: off, Text: string(bytes.ToValidUTF8(src[ls:le], []byte("�")))}
		}
		return src, nil
	}
	return decodeWith(enc, src)
}

// Offset of the first byte that does not start a valid UTF-8 sequence, -1 when every byte does
func firstInvalidUTF8(b []byte) int {
	for i := 0; i < len(b); {
		r, size := utf8.DecodeRune(b[i:])
		if r == utf8.RuneError && size <= 1 {
			return i
		}
		i += size
	}
	return -1
}

// Codec names a normalized declaration resolves to
var codecAliases = map[string]string{
	"646": "ascii", "us_ascii": "ascii", "us": "ascii", "ansi_x3.4_1968": "ascii", "iso646_us": "ascii",
	"latin_1": "latin_1", "iso8859_1": "latin_1", "iso_8859_1": "latin_1", "l1": "latin_1", "latin": "latin_1", "latin1": "latin_1", "8859": "latin_1", "cp819": "latin_1", "iso_ir_100": "latin_1",
	"utf_8": "utf_8", "u8": "utf_8", "utf": "utf_8", "utf8": "utf_8", "cp65001": "utf_8",
	"utf_8_sig": "utf_8_sig", "utf8_sig": "utf_8_sig",
	"utf_16": "utf_16", "u16": "utf_16", "utf16": "utf_16", "utf_16le": "utf_16_le", "utf_16_le": "utf_16_le", "utf_16be": "utf_16_be", "utf_16_be": "utf_16_be",
	"koi8_r": "koi8_r", "cskoi8r": "koi8_r", "koi8_u": "koi8_u",
	"mac_roman": "mac_roman", "macroman": "mac_roman", "macintosh": "mac_roman",
	"cp437": "cp437", "437": "cp437", "ibm437": "cp437", "cp850": "cp850", "850": "cp850", "ibm850": "cp850", "cp852": "cp852", "852": "cp852", "cp855": "cp855", "855": "cp855", "cp858": "cp858", "cp860": "cp860", "cp862": "cp862", "cp863": "cp863", "cp865": "cp865", "cp866": "cp866", "866": "cp866", "ibm866": "cp866",
	"euc_jp": "euc_jp", "eucjp": "euc_jp", "ujis": "euc_jp", "u_jis": "euc_jp",
	"shift_jis": "shift_jis", "sjis": "shift_jis", "s_jis": "shift_jis", "shiftjis": "shift_jis", "csshiftjis": "shift_jis",
	"iso2022_jp": "iso2022_jp", "iso2022jp": "iso2022_jp", "iso_2022_jp": "iso2022_jp", "csiso2022jp": "iso2022_jp",
	"euc_kr": "euc_kr", "euckr": "euc_kr", "korean": "euc_kr", "ksc5601": "euc_kr", "ks_c_5601": "euc_kr", "ks_c_5601_1987": "euc_kr",
	"gbk": "gbk", "936": "gbk", "cp936": "gbk", "ms936": "gbk", "gb18030": "gb18030", "gb18030_2000": "gb18030", "gb2312": "gbk", "chinese": "gbk", "euc_cn": "gbk", "euccn": "gbk", "hz": "hz", "hzgb": "hz", "hz_gb": "hz", "hz_gb_2312": "hz",
	"big5": "big5", "big5_tw": "big5", "csbig5": "big5",
}

// Single byte codecs by canonical name
var singleByte = map[string]*charmap.Charmap{
	"latin_1": charmap.ISO8859_1, "koi8_r": charmap.KOI8R, "koi8_u": charmap.KOI8U, "mac_roman": charmap.Macintosh,
	"cp437": charmap.CodePage437, "cp850": charmap.CodePage850, "cp852": charmap.CodePage852, "cp855": charmap.CodePage855, "cp858": charmap.CodePage858, "cp860": charmap.CodePage860, "cp862": charmap.CodePage862, "cp863": charmap.CodePage863, "cp865": charmap.CodePage865, "cp866": charmap.CodePage866,
	"cp1250": charmap.Windows1250, "cp1251": charmap.Windows1251, "cp1252": charmap.Windows1252, "cp1253": charmap.Windows1253, "cp1254": charmap.Windows1254, "cp1255": charmap.Windows1255, "cp1256": charmap.Windows1256, "cp1257": charmap.Windows1257, "cp1258": charmap.Windows1258,
	"iso8859_2": charmap.ISO8859_2, "iso8859_3": charmap.ISO8859_3, "iso8859_4": charmap.ISO8859_4, "iso8859_5": charmap.ISO8859_5, "iso8859_6": charmap.ISO8859_6, "iso8859_7": charmap.ISO8859_7, "iso8859_8": charmap.ISO8859_8, "iso8859_9": charmap.ISO8859_9, "iso8859_10": charmap.ISO8859_10, "iso8859_13": charmap.ISO8859_13, "iso8859_14": charmap.ISO8859_14, "iso8859_15": charmap.ISO8859_15, "iso8859_16": charmap.ISO8859_16,
}

// Multi byte and UTF-16 codecs by canonical name
var multiByte = map[string]encoding.Encoding{
	"euc_jp": japanese.EUCJP, "shift_jis": japanese.ShiftJIS, "iso2022_jp": japanese.ISO2022JP, "euc_kr": korean.EUCKR,
	"gbk": simplifiedchinese.GBK, "gb18030": simplifiedchinese.GB18030, "hz": simplifiedchinese.HZGB2312, "big5": traditionalchinese.Big5,
	"utf_16": unicode.UTF16(unicode.LittleEndian, unicode.ExpectBOM), "utf_16_le": unicode.UTF16(unicode.LittleEndian, unicode.IgnoreBOM), "utf_16_be": unicode.UTF16(unicode.BigEndian, unicode.IgnoreBOM),
}

// Canonical codec name of a declaration, lowercased with punctuation runs folded to underscores and aliases resolved
func codecName(spec string) string {
	var b strings.Builder
	pendingSep := false
	for _, r := range strings.ToLower(spec) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '.' {
			if pendingSep && b.Len() > 0 {
				b.WriteByte('_')
			}
			pendingSep = false
			b.WriteRune(r)
		} else {
			pendingSep = true
		}
	}
	n := b.String()
	if a, ok := codecAliases[n]; ok {
		return a
	}
	for _, prefix := range []string{"windows_", "cp"} {
		if num, ok := strings.CutPrefix(n, prefix); ok {
			if _, known := singleByte["cp"+num]; known {
				return "cp" + num
			}
		}
	}
	latin := map[string]string{"2": "2", "3": "3", "4": "4", "5": "9", "6": "10", "7": "13", "8": "14", "9": "15", "10": "16"}
	for _, prefix := range []string{"iso_8859_", "iso8859_", "latin"} {
		if num, ok := strings.CutPrefix(n, prefix); ok {
			if prefix == "latin" {
				num = latin[num]
			}
			if _, known := singleByte["iso8859_"+num]; known {
				return "iso8859_" + num
			}
		}
	}
	return n
}

// Decodes with a declared codec, failing for an unknown codec or bytes the codec rejects
func decodeWith(spec string, src []byte) ([]byte, *SyntaxError) {
	name := codecName(spec)
	switch name {
	case "utf_8", "utf_8_sig":
		if bad := firstInvalidUTF8(src); bad >= 0 {
			return nil, decodeFailure(fmt.Sprintf("'utf-8' codec can't decode byte 0x%02x in position %d: invalid start byte", src[bad], bad))
		}
		return src, nil
	case "ascii":
		for i, c := range src {
			if c >= 0x80 {
				return nil, decodeFailure(fmt.Sprintf("'ascii' codec can't decode byte 0x%02x in position %d: ordinal not in range(128)", c, i))
			}
		}
		return src, nil
	}
	if cm, ok := singleByte[name]; ok {
		out := make([]byte, 0, len(src))
		for i, c := range src {
			r := cm.DecodeByte(c)
			if r == utf8.RuneError {
				return nil, decodeFailure(fmt.Sprintf("'charmap' codec can't decode byte 0x%02x in position %d: character maps to <undefined>", c, i))
			}
			out = utf8.AppendRune(out, r)
		}
		return out, nil
	}
	if enc, ok := multiByte[name]; ok {
		out, err := enc.NewDecoder().Bytes(src)
		if err != nil {
			return nil, decodeFailure(err.Error())
		}
		return out, nil
	}
	return nil, &SyntaxError{Msg: "unknown encoding: " + spec, Col: -1}
}

// Decoding failure the reference reports with line 0 and column -1
func decodeFailure(msg string) *SyntaxError {
	return &SyntaxError{Msg: msg, Col: -1}
}

// Each byte that does not start a valid UTF-8 sequence replaced with U+FFFD
func validText(s string) string {
	if utf8.ValidString(s) {
		return s
	}
	var b strings.Builder
	b.Grow(len(s) + 8)
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		if r == utf8.RuneError && size <= 1 {
			b.WriteRune(utf8.RuneError)
			i++
			continue
		}
		b.WriteString(s[i : i+size])
		i += size
	}
	return b.String()
}
