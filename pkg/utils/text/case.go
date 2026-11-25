package textutil

import (
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/cases"
)

// Unicode full case folding of s for caseless matching, ß as ss and every Cherokee letter as the capital Unicode folds it to, which golang.org/x/text/cases alone folds the other way round for the capitals; bytes that are not UTF-8 kept
func Fold(s string) string {
	folded := cases.Fold().String(s)
	if strings.IndexFunc(folded, isSmallCherokee) < 0 {
		return folded
	}
	var b strings.Builder
	b.Grow(len(folded))
	for i := 0; i < len(folded); {
		r, size := utf8.DecodeRuneInString(folded[i:])
		if isSmallCherokee(r) {
			b.WriteRune(unicode.ToUpper(r))
		} else {
			b.WriteString(folded[i : i+size])
		}
		i += size
	}
	return b.String()
}

// Whether r is a small Cherokee letter, which Unicode case folding maps to its capital
func isSmallCherokee(r rune) bool { return 0x13F8 <= r && r <= 0x13FD || 0xAB70 <= r && r <= 0xABBF }

// s with the first letter of each word in title case and the word's other letters in lower case, a word being a run of letters and numbers with its marks and any apostrophe between letters
func Title(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	inWord := false
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		mapped := r
		switch {
		case r == utf8.RuneError && size == 1:
			inWord = false
		case unicode.IsLetter(r):
			if inWord {
				mapped = unicode.ToLower(r)
			} else {
				mapped = unicode.ToTitle(r)
			}
			inWord = true
		case unicode.IsNumber(r), inWord && unicode.IsMark(r):
			inWord = true
		case inWord && (r == '\'' || r == '\u2019'):
			next, _ := utf8.DecodeRuneInString(s[i+size:])
			inWord = unicode.IsLetter(next)
		default:
			inWord = false
		}
		if mapped == r {
			b.WriteString(s[i : i+size])
		} else {
			b.WriteRune(mapped)
		}
		i += size
	}
	return b.String()
}

// s with its first character in title case and the rest left as it is
func UpperFirst(s string) string {
	r, size := utf8.DecodeRuneInString(s)
	if t := unicode.ToTitle(r); t != r {
		return string(t) + s[size:]
	}
	return s
}
