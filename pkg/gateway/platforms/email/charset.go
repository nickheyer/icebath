package email

import (
	"encoding/base64"
	"io"
	"mime"
	"strings"
	"unicode/utf16"
	"unicode/utf8"

	"golang.org/x/text/encoding"
	"golang.org/x/text/encoding/htmlindex"
	"golang.org/x/text/encoding/ianaindex"
	"golang.org/x/text/encoding/korean"
	"golang.org/x/text/encoding/simplifiedchinese"
	"golang.org/x/text/encoding/unicode/utf32"
)

// Charset labels seen in mail that the encoding indexes do not resolve to the decoder mail clients use
var charsetAliases = map[string]string{
	"unknown-8bit": "utf-8", "unknown": "utf-8", "x-unknown": "utf-8", "default": "utf-8",
	"ansi_x3.110-1983": "iso-8859-1", "cp-850": "ibm850", "latin-1": "iso-8859-1", "latin_1": "iso-8859-1",
	"gb2312": "gb18030", "gbk": "gb18030", "x-gbk": "gb18030", "cp936": "gb18030", "ms936": "gb18030", "euc-cn": "gb18030",
	"ks_c_5601-1987": "euc-kr", "cp949": "euc-kr", "uhc": "euc-kr",
	"iso-2022-jp-1": "iso-2022-jp", "iso-2022-jp-2": "iso-2022-jp", "iso-2022-jp-3": "iso-2022-jp",
	"iso-2022-jp-ext": "iso-2022-jp", "iso-2022-jp-2004": "iso-2022-jp",
	"shift_jis_2004": "shift_jis", "shift_jisx0213": "shift_jis", "sjis": "shift_jis", "cp932": "shift_jis", "ms932": "shift_jis",
	"euc_jis_2004": "euc-jp", "euc_jisx0213": "euc-jp", "eucjp": "euc-jp",
	"big5hkscs": "big5", "big5-hkscs": "big5", "cp950": "big5",
	"utf8": "utf-8", "utf_8": "utf-8", "utf7": "utf-7", "utf_7": "utf-7", "unicode-1-1-utf-7": "utf-7",
	"csunicode11utf7": "utf-7", "csiso2022kr": "iso-2022-kr", "hz": "hz-gb-2312", "utf32": "utf-32", "utf_32": "utf-32",
	"utf_32_le": "utf-32le", "utf_32_be": "utf-32be",
}

// Text of the payload in the labelled charset, never failing and replacing undecodable bytes
func decodeText(payload []byte, charset string) string {
	label := strings.ToLower(strings.Trim(strings.TrimSpace(charset), `"'`))
	if alias, ok := charsetAliases[label]; ok {
		label = alias
	}
	switch label {
	case "", "utf-8":
		return validUTF8(payload)
	case "utf-7":
		return decodeUTF7(payload)
	case "iso-2022-kr":
		return decodeISO2022KR(payload)
	case "hz-gb-2312":
		return decodeHZ(payload)
	case "utf-32":
		return decodeWith(utf32.UTF32(utf32.LittleEndian, utf32.UseBOM), payload)
	case "utf-32le":
		return decodeWith(utf32.UTF32(utf32.LittleEndian, utf32.IgnoreBOM), payload)
	case "utf-32be":
		return decodeWith(utf32.UTF32(utf32.BigEndian, utf32.IgnoreBOM), payload)
	}
	if enc := lookupEncoding(label); enc != nil {
		return decodeWith(enc, payload)
	}
	return validUTF8(payload)
}

// Decoder of a charset label from the WHATWG index, then the IANA and MIME indexes, nil when none knows it
func lookupEncoding(label string) encoding.Encoding {
	for _, name := range []string{label, strings.ReplaceAll(label, "_", "-")} {
		if enc, err := htmlindex.Get(name); err == nil && enc != encoding.Replacement {
			return enc
		}
		for _, index := range []*ianaindex.Index{ianaindex.IANA, ianaindex.MIME} {
			if enc, err := index.Encoding(name); err == nil && enc != nil {
				return enc
			}
		}
	}
	return nil
}

// Payload decoded with the encoding, the UTF-8 reading when the decoder rejects it
func decodeWith(enc encoding.Encoding, payload []byte) string {
	out, err := enc.NewDecoder().Bytes(payload)
	if err != nil {
		return validUTF8(payload)
	}
	return validUTF8(out)
}

// UTF-8 text with every invalid byte replaced by U+FFFD
func validUTF8(b []byte) string {
	if utf8.Valid(b) {
		return string(b)
	}
	var out strings.Builder
	for len(b) > 0 {
		r, size := utf8.DecodeRune(b)
		if r == utf8.RuneError && size == 1 {
			out.WriteRune(utf8.RuneError)
		} else {
			out.Write(b[:size])
		}
		b = b[size:]
	}
	return out.String()
}

// RFC 2152 UTF-7 text, base64 runs of UTF-16 between plus and minus
func decodeUTF7(b []byte) string {
	var out strings.Builder
	for i := 0; i < len(b); i++ {
		if b[i] != '+' {
			if b[i] < utf8.RuneSelf {
				out.WriteByte(b[i])
			} else {
				out.WriteRune(utf8.RuneError)
			}
			continue
		}
		j := i + 1
		for j < len(b) && isBase64Char(b[j]) {
			j++
		}
		if j == i+1 {
			out.WriteByte('+')
			if j < len(b) && b[j] == '-' {
				i = j
			}
			continue
		}
		seg := b[i+1 : j]
		if len(seg)%4 == 1 {
			seg = seg[:len(seg)-1]
		}
		raw, _ := base64.RawStdEncoding.DecodeString(string(seg))
		units := make([]uint16, 0, len(raw)/2)
		for k := 0; k+1 < len(raw); k += 2 {
			units = append(units, uint16(raw[k])<<8|uint16(raw[k+1]))
		}
		out.WriteString(string(utf16.Decode(units)))
		if j < len(b) && b[j] == '-' {
			j++
		}
		i = j - 1
	}
	return out.String()
}

// Whether the byte belongs to the base64 alphabet without padding
func isBase64Char(c byte) bool {
	return c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '+' || c == '/'
}

// RFC 1557 ISO-2022-KR text, shifted runs decoded as KS X 1001 through EUC-KR
func decodeISO2022KR(b []byte) string {
	var out strings.Builder
	var run []byte
	flush := func() {
		if len(run) > 0 {
			decoded, err := korean.EUCKR.NewDecoder().Bytes(run)
			if err != nil {
				decoded = []byte(strings.Repeat(string(utf8.RuneError), len(run)/2))
			}
			out.Write(decoded)
			run = run[:0]
		}
	}
	shifted := false
	for i := 0; i < len(b); i++ {
		switch c := b[i]; {
		case c == 0x1b && i+3 < len(b) && b[i+1] == '$' && b[i+2] == ')' && b[i+3] == 'C':
			i += 3
		case c == 0x0e:
			shifted = true
		case c == 0x0f:
			flush()
			shifted = false
		case c == '\r' || c == '\n':
			flush()
			shifted = false
			out.WriteByte(c)
		case shifted:
			run = append(run, c|0x80)
		default:
			out.WriteByte(c)
		}
	}
	flush()
	return validUTF8([]byte(out.String()))
}

// RFC 1843 HZ text, tilde brace runs decoded as GB2312 through GB18030
func decodeHZ(b []byte) string {
	var out strings.Builder
	var run []byte
	flush := func() {
		if len(run) > 0 {
			decoded, err := simplifiedchinese.GB18030.NewDecoder().Bytes(run)
			if err != nil {
				decoded = []byte(strings.Repeat(string(utf8.RuneError), len(run)/2))
			}
			out.Write(decoded)
			run = run[:0]
		}
	}
	gb := false
	for i := 0; i < len(b); i++ {
		c := b[i]
		if c == '~' && i+1 < len(b) {
			switch b[i+1] {
			case '~':
				if gb {
					run = append(run, '~'|0x80)
				} else {
					out.WriteByte('~')
				}
				i++
				continue
			case '{':
				gb = true
				i++
				continue
			case '}':
				flush()
				gb = false
				i++
				continue
			case '\n':
				i++
				continue
			}
		}
		if gb && c >= 0x21 && c <= 0x7e {
			run = append(run, c|0x80)
			continue
		}
		flush()
		out.WriteByte(c)
	}
	flush()
	return validUTF8([]byte(out.String()))
}

// RFC 2047 decoder whose charsets go through the mail charset table
var wordDecoder = &mime.WordDecoder{CharsetReader: func(charset string, input io.Reader) (io.Reader, error) {
	raw, err := io.ReadAll(input)
	if err != nil {
		return nil, err
	}
	return strings.NewReader(decodeText(raw, charset)), nil
}}

// Header value with RFC 2047 words decoded, the raw value when the words do not decode
func decodeHeader(raw string) string {
	out, err := wordDecoder.DecodeHeader(raw)
	if err != nil {
		return validUTF8([]byte(raw))
	}
	return validUTF8([]byte(out))
}
