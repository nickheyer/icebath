package jsonutil

import (
	"strconv"
	"unicode/utf16"
	"unicode/utf8"
)

type textEncoding int

const (
	encUTF8 textEncoding = iota
	encUTF16BE
	encUTF16LE
	encUTF32BE
	encUTF32LE
)

// Encoding of JSON bytes told by a byte order mark or the zero bytes ASCII leaves in the first characters, with the mark's length
func detectEncoding(b []byte) (textEncoding, int) {
	switch {
	case len(b) >= 4 && b[0] == 0 && b[1] == 0 && b[2] == 0xfe && b[3] == 0xff:
		return encUTF32BE, 4
	case len(b) >= 4 && b[0] == 0xff && b[1] == 0xfe && b[2] == 0 && b[3] == 0:
		return encUTF32LE, 4
	case len(b) >= 2 && b[0] == 0xfe && b[1] == 0xff:
		return encUTF16BE, 2
	case len(b) >= 2 && b[0] == 0xff && b[1] == 0xfe:
		return encUTF16LE, 2
	case len(b) >= 3 && b[0] == 0xef && b[1] == 0xbb && b[2] == 0xbf:
		return encUTF8, 3
	case len(b) >= 4 && b[0] == 0 && b[1] == 0:
		return encUTF32BE, 0
	case len(b) >= 4 && b[0] == 0:
		return encUTF16BE, 0
	case len(b) >= 4 && b[1] == 0 && b[2] == 0 && b[3] == 0:
		return encUTF32LE, 0
	case len(b) >= 4 && b[1] == 0:
		return encUTF16LE, 0
	case len(b) == 2 && b[0] == 0:
		return encUTF16BE, 0
	case len(b) == 2 && b[1] == 0:
		return encUTF16LE, 0
	}
	return encUTF8, 0
}

// UTF-8 form of JSON bytes in UTF-8, UTF-16 or UTF-32 without any byte order mark, with the first problem found
func toUTF8(b []byte) ([]byte, *SyntaxError) {
	enc, mark := detectEncoding(b)
	body := b[mark:]
	switch enc {
	case encUTF16BE, encUTF16LE:
		return fromUTF16(body, enc == encUTF16BE, mark)
	case encUTF32BE, encUTF32LE:
		return fromUTF32(body, enc == encUTF32BE, mark)
	}
	return body, nil
}

func fromUTF16(b []byte, bigEndian bool, mark int) ([]byte, *SyntaxError) {
	var problem *SyntaxError
	unit := func(i int) rune {
		if bigEndian {
			return rune(b[i])<<8 | rune(b[i+1])
		}
		return rune(b[i+1])<<8 | rune(b[i])
	}
	out := make([]byte, 0, len(b)/2+len(b)/8)
	i := 0
	for ; i+1 < len(b); i += 2 {
		r := unit(i)
		if utf16.IsSurrogate(r) {
			if r < 0xdc00 && i+3 < len(b) {
				if lo := unit(i + 2); lo >= 0xdc00 && lo <= 0xdfff {
					out = utf8.AppendRune(out, utf16.DecodeRune(r, lo))
					i += 2
					continue
				}
			}
			if problem == nil {
				problem = syntaxError(out, len(out), "unpaired UTF-16 surrogate at input byte "+strconv.Itoa(mark+i))
			}
			r = utf8.RuneError
		}
		out = utf8.AppendRune(out, r)
	}
	if i < len(b) {
		if problem == nil {
			problem = syntaxError(out, len(out), "UTF-16 input ends in half a code unit")
		}
		out = utf8.AppendRune(out, utf8.RuneError)
	}
	return out, problem
}

func fromUTF32(b []byte, bigEndian bool, mark int) ([]byte, *SyntaxError) {
	var problem *SyntaxError
	out := make([]byte, 0, len(b)/4+len(b)/16)
	i := 0
	for ; i+3 < len(b); i += 4 {
		var u uint32
		if bigEndian {
			u = uint32(b[i])<<24 | uint32(b[i+1])<<16 | uint32(b[i+2])<<8 | uint32(b[i+3])
		} else {
			u = uint32(b[i+3])<<24 | uint32(b[i+2])<<16 | uint32(b[i+1])<<8 | uint32(b[i])
		}
		r := rune(u)
		if u > utf8.MaxRune || utf16.IsSurrogate(r) {
			if problem == nil {
				problem = syntaxError(out, len(out), "invalid UTF-32 code point at input byte "+strconv.Itoa(mark+i))
			}
			r = utf8.RuneError
		}
		out = utf8.AppendRune(out, r)
	}
	if i < len(b) {
		if problem == nil {
			problem = syntaxError(out, len(out), "UTF-32 input ends in part of a code unit")
		}
		out = utf8.AppendRune(out, utf8.RuneError)
	}
	return out, problem
}

// UTF-8 text of JSON bytes written in UTF-8, UTF-16 or UTF-32, a byte order mark dropped; on bad input the text with U+FFFD in place of each bad sequence and a *SyntaxError naming the first
func Text(data []byte) (string, error) {
	out, problem := toUTF8(data)
	if problem != nil {
		return string(out), problem
	}
	if utf8.Valid(out) {
		return string(out), nil
	}
	bad := 0
	for bad < len(out) {
		r, size := utf8.DecodeRune(out[bad:])
		if r == utf8.RuneError && size == 1 {
			break
		}
		bad += size
	}
	return string(validUTF8(out)), syntaxError(out, bad, "invalid UTF-8 byte 0x"+hexByte(out[bad]))
}

// b with each byte that starts no valid UTF-8 sequence replaced by U+FFFD
func validUTF8(b []byte) []byte {
	if utf8.Valid(b) {
		return b
	}
	out := make([]byte, 0, len(b)+8)
	for i := 0; i < len(b); {
		r, size := utf8.DecodeRune(b[i:])
		if r == utf8.RuneError && size == 1 {
			out = utf8.AppendRune(out, utf8.RuneError)
		} else {
			out = append(out, b[i:i+size]...)
		}
		i += size
	}
	return out
}

func hexByte(c byte) string {
	const digits = "0123456789abcdef"
	return string([]byte{digits[c>>4], digits[c&0xf]})
}
