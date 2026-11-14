package process

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

type utf8Decoder struct {
	pending []byte
}

func sequenceShape(lead byte) (size int, lo, hi byte) {
	switch {
	case lead < 0x80:
		return 1, 0, 0
	case lead >= 0xC2 && lead <= 0xDF:
		return 2, 0x80, 0xBF
	case lead == 0xE0:
		return 3, 0xA0, 0xBF
	case lead >= 0xE1 && lead <= 0xEC, lead == 0xEE, lead == 0xEF:
		return 3, 0x80, 0xBF
	case lead == 0xED:
		return 3, 0x80, 0x9F
	case lead == 0xF0:
		return 4, 0x90, 0xBF
	case lead >= 0xF1 && lead <= 0xF3:
		return 4, 0x80, 0xBF
	case lead == 0xF4:
		return 4, 0x80, 0x8F
	}
	return 0, 0, 0
}

func (d *utf8Decoder) decode(data []byte, final bool) string {
	buf := data
	if len(d.pending) > 0 {
		buf = append(d.pending, data...)
		d.pending = nil
	}
	var out strings.Builder
	out.Grow(len(buf))
	i := 0
	for i < len(buf) {
		lead := buf[i]
		size, lo, hi := sequenceShape(lead)
		if size == 1 {
			out.WriteByte(lead)
			i++
			continue
		}
		if size == 0 {
			out.WriteRune(utf8.RuneError)
			i++
			continue
		}
		j := i + 1
		valid := true
		for k := 1; k < size; k++ {
			if j >= len(buf) {
				break
			}
			c := buf[j]
			if k == 1 && (c < lo || c > hi) || k > 1 && (c < 0x80 || c > 0xBF) {
				valid = false
				break
			}
			j++
		}
		if valid && j-i == size {
			out.Write(buf[i:j])
			i = j
			continue
		}
		if valid && j >= len(buf) && !final {
			d.pending = append([]byte(nil), buf[i:]...)
			break
		}
		out.WriteRune(utf8.RuneError)
		i = j
	}
	return out.String()
}

func outputSpace(r rune) bool {
	return unicode.IsSpace(r) || r >= 0x1c && r <= 0x1f
}

func blank(s string) bool {
	return strings.TrimFunc(s, outputSpace) == ""
}

func outputLines(text string) []string {
	var lines []string
	start := 0
	for i := 0; i < len(text); {
		r, size := utf8.DecodeRuneInString(text[i:])
		width := 0
		switch r {
		case '\n', '\v', '\f', 0x1c, 0x1d, 0x1e, 0x85, 0x2028, 0x2029:
			width = size
		case '\r':
			width = size
			if i+1 < len(text) && text[i+1] == '\n' {
				width++
			}
		}
		if width == 0 {
			i += size
			continue
		}
		lines = append(lines, text[start:i])
		i += width
		start = i
	}
	if start < len(text) {
		lines = append(lines, text[start:])
	}
	return lines
}

func runeTail(s string, n int) string {
	if n <= 0 {
		return ""
	}
	i := len(s)
	for count := 0; i > 0 && count < n; count++ {
		_, size := utf8.DecodeLastRuneInString(s[:i])
		i -= size
	}
	return s[i:]
}

func runeHead(s string, n int) string {
	i := 0
	for count := 0; i < len(s) && count < n; count++ {
		_, size := utf8.DecodeRuneInString(s[i:])
		i += size
	}
	return s[:i]
}
