package charsetutil

import (
	"strings"
	"unicode/utf16"
	"unicode/utf8"
)

// UTF-7 of RFC 2152: ASCII written as itself, every other character as base64 of its UTF-16 between a plus sign and an optional minus sign
type utf7Codec struct{}

const base64Chars = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"

// Six bit value of a base64 byte, -1 for any other byte
func base64Value(c byte) int {
	switch {
	case c >= 'A' && c <= 'Z':
		return int(c - 'A')
	case c >= 'a' && c <= 'z':
		return int(c-'a') + 26
	case c >= '0' && c <= '9':
		return int(c-'0') + 52
	case c == '+':
		return 62
	case c == '/':
		return 63
	}
	return -1
}

func (utf7Codec) decode(data []byte, replace bool) (string, int, string) {
	out := make([]byte, 0, len(data))
	for p := 0; p < len(data); {
		c := data[p]
		if c >= utf8.RuneSelf {
			if !replace {
				return "", p, "byte above 0x7f"
			}
			out, p = append(out, replacement...), p+1
			continue
		}
		if c != '+' {
			out, p = append(out, c), p+1
			continue
		}
		start := p
		p++
		if p < len(data) && data[p] == '-' {
			out, p = append(out, '+'), p+1
			continue
		}
		run := p
		for p < len(data) && base64Value(data[p]) >= 0 {
			p++
		}
		if p == run {
			if !replace {
				return "", start, "plus sign followed by neither base64 nor a minus sign"
			}
			out = append(out, replacement...)
			continue
		}
		var problem string
		if out, problem = utf7Shifted(out, data[run:p]); problem != "" && !replace {
			return "", start, problem
		}
		if p < len(data) && data[p] == '-' {
			p++
		}
	}
	return string(out), -1, ""
}

// Out with the characters of the base64 run of one shift sequence appended, U+FFFD for each broken part, and the first problem found
func utf7Shifted(out, run []byte) ([]byte, string) {
	problem := ""
	fail := func(why string) {
		if problem == "" {
			problem = why
		}
		out = append(out, replacement...)
	}
	var bits uint32
	nbits := 0
	var high rune
	for _, c := range run {
		bits, nbits = bits<<6|uint32(base64Value(c)), nbits+6
		if nbits < 16 {
			continue
		}
		nbits -= 16
		u := rune(bits>>nbits) & 0xffff
		bits &= 1<<nbits - 1
		if high != 0 {
			if u >= 0xdc00 && u <= 0xdfff {
				out, high = utf8.AppendRune(out, utf16.DecodeRune(high, u)), 0
				continue
			}
			high = 0
			fail("unpaired surrogate")
		}
		switch {
		case u >= 0xd800 && u <= 0xdbff:
			high = u
		case u >= 0xdc00 && u <= 0xdfff:
			fail("unpaired surrogate")
		default:
			out = utf8.AppendRune(out, u)
		}
	}
	if high != 0 {
		fail("unpaired surrogate")
	}
	switch {
	case nbits >= 6:
		fail("partial character in shift sequence")
	case bits != 0:
		fail("non-zero padding bits in shift sequence")
	}
	return out, problem
}

// Whether the encoder writes an ASCII character as itself: letters, digits, the direct and optional direct characters of RFC 2152, space, tab, CR and LF, leaving plus sign, backslash, tilde and other controls to base64
func utf7Direct(r rune) bool {
	switch {
	case r >= 'A' && r <= 'Z', r >= 'a' && r <= 'z', r >= '0' && r <= '9':
		return true
	case r == ' ', r == '\t', r == '\r', r == '\n':
		return true
	}
	return r < utf8.RuneSelf && r > 0 && strings.ContainsRune("'(),-./:?!\"#$%&*;<=>@[]^_`{|}", r)
}

func (utf7Codec) encode(text string) ([]byte, int) {
	out := make([]byte, 0, len(text)+len(text)/2)
	var bits uint32
	nbits := 0
	shifted := false
	push := func(u rune) {
		bits, nbits = bits<<16|uint32(u), nbits+16
		for nbits >= 6 {
			nbits -= 6
			out = append(out, base64Chars[bits>>nbits&0x3f])
		}
		bits &= 1<<nbits - 1
	}
	flush := func() {
		if nbits > 0 {
			out = append(out, base64Chars[bits<<(6-nbits)&0x3f])
		}
		bits, nbits = 0, 0
	}
	for _, r := range text {
		switch {
		case utf7Direct(r):
			if shifted {
				flush()
				if base64Value(byte(r)) >= 0 || r == '-' {
					out = append(out, '-')
				}
				shifted = false
			}
			out = append(out, byte(r))
			continue
		case r == '+' && !shifted:
			out = append(out, '+', '-')
			continue
		case !shifted:
			out, shifted = append(out, '+'), true
		}
		if r >= 0x10000 {
			high, low := utf16.EncodeRune(r)
			push(high)
			push(low)
			continue
		}
		push(r)
	}
	if shifted {
		flush()
		out = append(out, '-')
	}
	return out, -1
}
