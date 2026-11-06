package pyhtml

import (
	"regexp"
	"strconv"
	"strings"
)

var charrefRe = regexp.MustCompile(`&(#[0-9]+;?|#[xX][0-9a-fA-F]+;?|[^\t\n\f <&#;]{1,32};?)`)

// Numeric references html.unescape maps to a fixed replacement instead of their code point
var invalidCharrefs = map[int64]string{
	0x00: "�", 0x0d: "\r", 0x80: "€", 0x81: "\u0081", 0x82: "‚", 0x83: "ƒ", 0x84: "„",
	0x85: "…", 0x86: "†", 0x87: "‡", 0x88: "ˆ", 0x89: "‰", 0x8a: "Š", 0x8b: "‹",
	0x8c: "Œ", 0x8d: "\u008d", 0x8e: "Ž", 0x8f: "\u008f", 0x90: "\u0090", 0x91: "‘", 0x92: "’",
	0x93: "“", 0x94: "”", 0x95: "•", 0x96: "–", 0x97: "—", 0x98: "˜", 0x99: "™",
	0x9a: "š", 0x9b: "›", 0x9c: "œ", 0x9d: "\u009d", 0x9e: "ž", 0x9f: "Ÿ",
}

// Reports the code points html.unescape drops from a numeric reference
func invalidCodepoint(n int64) bool {
	switch {
	case n >= 0x1 && n <= 0x8, n == 0xb, n >= 0xe && n <= 0x1f, n >= 0x7f && n <= 0x9f, n >= 0xfdd0 && n <= 0xfdef:
		return true
	}
	return n <= 0x10ffff && n&0xfffe == 0xfffe
}

// Replaces one character reference the way html._replace_charref does
func replaceCharref(s string) string {
	if s[0] == '#' {
		digits, base := strings.TrimRight(s[1:], ";"), 10
		if digits[0] == 'x' || digits[0] == 'X' {
			digits, base = digits[1:], 16
		}
		num, err := strconv.ParseInt(digits, base, 64)
		if err != nil {
			num = 0x110000
		}
		if r, ok := invalidCharrefs[num]; ok {
			return r
		}
		if (num >= 0xd800 && num <= 0xdfff) || num > 0x10ffff {
			return "�"
		}
		if invalidCodepoint(num) {
			return ""
		}
		return string(rune(num))
	}
	if r, ok := html5[s]; ok {
		return r
	}
	for x := len(s) - 1; x > 1; x-- {
		if r, ok := html5[s[:x]]; ok {
			return r + s[x:]
		}
	}
	return "&" + s
}

// Unescape is html.unescape, every named and numeric reference replaced by the characters it stands for
func Unescape(s string) string {
	if !strings.Contains(s, "&") {
		return s
	}
	return charrefRe.ReplaceAllStringFunc(s, func(m string) string { return replaceCharref(m[1:]) })
}
