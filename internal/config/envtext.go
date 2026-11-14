package config

import (
	"bytes"
	"strings"
	"unicode"
	"unicode/utf16"
	"unicode/utf8"
)

// Byte order marks an env file may open with
const (
	bomUTF8    = "\xef\xbb\xbf"
	bomUTF16LE = "\xff\xfe"
	bomUTF16BE = "\xfe\xff"
	bomUTF32LE = "\xff\xfe\x00\x00"
	bomUTF32BE = "\x00\x00\xfe\xff"
)

// Reports whitespace as env files count it, Unicode spaces plus the four information separators
func isEnvSpace(r rune) bool {
	return unicode.IsSpace(r) || (r >= 0x1c && r <= 0x1f)
}

// Reports whitespace that stays on its line
func isEnvBlank(r rune) bool {
	return isEnvSpace(r) && r != '\n' && r != '\r'
}

// Text without the env whitespace around it
func trimEnvSpace(s string) string { return strings.TrimFunc(s, isEnvSpace) }

// Text of bytes read as one byte per character
func latin1Text(raw []byte) string {
	var b strings.Builder
	b.Grow(len(raw))
	for _, c := range raw {
		b.WriteRune(rune(c))
	}
	return b.String()
}

// Env file text for the loader, one mark dropped and bytes that are not UTF-8 read one per character
func envLoaderText(raw []byte) string {
	raw = bytes.TrimPrefix(raw, []byte(bomUTF8))
	if utf8.Valid(raw) {
		return string(raw)
	}
	return latin1Text(raw)
}

// Reports a file opening with a UTF-16 byte order mark
func hasUTF16Mark(raw []byte) bool {
	return bytes.HasPrefix(raw, []byte(bomUTF16LE)) || bytes.HasPrefix(raw, []byte(bomUTF16BE))
}

// Reports a file opening with a UTF-32 byte order mark
func hasUTF32Mark(raw []byte) bool {
	return bytes.HasPrefix(raw, []byte(bomUTF32LE)) || bytes.HasPrefix(raw, []byte(bomUTF32BE))
}

// Text of UTF-16 bytes behind their byte order mark, ok false for a cut unit or an unpaired surrogate
func utf16Text(raw []byte) (string, bool) {
	big := bytes.HasPrefix(raw, []byte(bomUTF16BE))
	body := raw[2:]
	if len(body)%2 != 0 {
		return "", false
	}
	units := make([]uint16, len(body)/2)
	for i := range units {
		a, b := body[2*i], body[2*i+1]
		if big {
			a, b = b, a
		}
		units[i] = uint16(b)<<8 | uint16(a)
	}
	for i := 0; i < len(units); i++ {
		switch u := units[i]; {
		case u >= 0xd800 && u <= 0xdbff:
			if i+1 >= len(units) || units[i+1] < 0xdc00 || units[i+1] > 0xdfff {
				return "", false
			}
			i++
		case u >= 0xdc00 && u <= 0xdfff:
			return "", false
		}
	}
	return string(utf16.Decode(units)), true
}

// Length of the valid UTF-8 sequence opening b, or minus the length of the longest broken prefix
func utf8Step(b []byte) int {
	c := b[0]
	if c < 0x80 {
		return 1
	}
	lo, hi, need := byte(0x80), byte(0xbf), 0
	switch {
	case c >= 0xc2 && c <= 0xdf:
		need = 1
	case c == 0xe0:
		lo, need = 0xa0, 2
	case (c >= 0xe1 && c <= 0xec) || c == 0xee || c == 0xef:
		need = 2
	case c == 0xed:
		hi, need = 0x9f, 2
	case c == 0xf0:
		lo, need = 0x90, 3
	case c >= 0xf1 && c <= 0xf3:
		need = 3
	case c == 0xf4:
		hi, need = 0x8f, 3
	default:
		return -1
	}
	n := 1
	for k := 0; k < need; k++ {
		if n >= len(b) || b[n] < lo || b[n] > hi {
			return -n
		}
		lo, hi = 0x80, 0xbf
		n++
	}
	return n
}

// Text of UTF-8 bytes with one mark dropped and each broken sequence read as one replacement character
func utf8ReplacedText(raw []byte) string {
	raw = bytes.TrimPrefix(raw, []byte(bomUTF8))
	if utf8.Valid(raw) {
		return string(raw)
	}
	var b strings.Builder
	for i := 0; i < len(raw); {
		n := utf8Step(raw[i:])
		if n < 0 {
			b.WriteRune(utf8.RuneError)
			i -= n
			continue
		}
		b.Write(raw[i : i+n])
		i += n
	}
	return b.String()
}

// Lines of text with every CRLF and CR read as LF, each keeping its line break
func textLines(text string) []string {
	text = strings.ReplaceAll(strings.ReplaceAll(text, "\r\n", "\n"), "\r", "\n")
	lines := strings.SplitAfter(text, "\n")
	if n := len(lines); n > 0 && lines[n-1] == "" {
		lines = lines[:n-1]
	}
	return lines
}

// Rows of env text cut at every line boundary the tokenizer honors, the boundaries dropped
func tokenizerRows(text string) []string {
	var rows []string
	start := 0
	for i := 0; i < len(text); {
		r, size := utf8.DecodeRuneInString(text[i:])
		switch r {
		case '\r':
			rows = append(rows, text[start:i])
			if strings.HasPrefix(text[i+size:], "\n") {
				size++
			}
			start = i + size
		case '\n', '\v', '\f', 0x1c, 0x1d, 0x1e, 0x85, 0x2028, 0x2029:
			rows = append(rows, text[start:i])
			start = i + size
		}
		i += size
	}
	if start < len(text) {
		rows = append(rows, text[start:])
	}
	return rows
}
