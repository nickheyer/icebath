package files

import (
	"regexp"
	"strings"
)

// Windows device names, matched against an ASCII lowercased candidate as the JS i flag folds them
var winReservedRE = regexp.MustCompile(`^(?:con|prn|aux|nul|com[0-9\x{b9}\x{b2}\x{b3}]|lpt[0-9\x{b9}\x{b2}\x{b3}]|conin\$|conout\$|clock\$)(?:\.|$)`)

// True when the name opens a Windows device instead of a file
func winReserved(name string) bool { return winReservedRE.MatchString(asciiLower(name)) }

func asciiLower(s string) string {
	b := []byte(s)
	for i, c := range b {
		if c >= 'A' && c <= 'Z' {
			b[i] = c + 32
		}
	}
	return string(b)
}

// Characters Python str.strip removes
func pyStripSpace(r rune) bool {
	switch {
	case r >= '\t' && r <= '\r', r >= 0x1c && r <= 0x20, r == 0x85, r == 0xa0, r == 0x1680,
		r >= 0x2000 && r <= 0x200a, r == 0x2028, r == 0x2029, r == 0x202f, r == 0x205f, r == 0x3000:
		return true
	}
	return false
}

// Text without the leading and trailing whitespace Python str.strip removes
func StripLikePython(text string) string { return strings.TrimFunc(text, pyStripSpace) }

// Characters JS String.prototype.trim removes
func jsSpace(r rune) bool {
	switch {
	case r >= '\t' && r <= '\r', r == ' ', r == 0xa0, r == 0x1680, r >= 0x2000 && r <= 0x200a,
		r == 0x2028, r == 0x2029, r == 0x202f, r == 0x205f, r == 0x3000, r == 0xfeff:
		return true
	}
	return false
}

// JS trim of text
func jsTrim(text string) string { return strings.TrimFunc(text, jsSpace) }
