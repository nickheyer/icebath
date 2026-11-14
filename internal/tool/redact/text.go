package redact

import (
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/cases"
)

const pyWS = `\x{09}-\x{0d}\x{1c}-\x{20}\x{85}\x{a0}\x{1680}\x{2000}-\x{200a}\x{2028}\x{2029}\x{202f}\x{205f}\x{3000}`

const (
	sp  = `[` + pyWS + `]`
	nsp = `[^` + pyWS + `]`
)

const foldedI = `[i\x{131}\x{130}]`

func fi(word string) string {
	return strings.ReplaceAll(word, "i", foldedI)
}

func isPySpace(r rune) bool {
	switch {
	case r >= 0x09 && r <= 0x0d, r >= 0x1c && r <= 0x20, r == 0x85, r == 0xa0, r == 0x1680,
		r >= 0x2000 && r <= 0x200a, r == 0x2028, r == 0x2029, r == 0x202f, r == 0x205f, r == 0x3000:
		return true
	}
	return false
}

func pyStrip(s string) string {
	return strings.TrimFunc(s, func(r rune) bool { return isPySpace(r) })
}

func pyFields(s string) []string {
	return strings.FieldsFunc(s, func(r rune) bool { return isPySpace(r) })
}

func pyLower(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r == 0x130 {
			b.WriteString("i̇")
			continue
		}
		b.WriteRune(unicode.ToLower(r))
	}
	return b.String()
}

func casefold(s string) string {
	return cases.Fold().String(s)
}

func pyIsUpper(r rune) bool {
	return unicode.IsUpper(r) || unicode.Is(unicode.Other_Uppercase, r)
}

func pyIsLower(r rune) bool {
	return unicode.IsLower(r) || unicode.Is(unicode.Other_Lowercase, r)
}

func pyIsWord(r rune) bool {
	return r == '_' || unicode.IsLetter(r) || unicode.IsNumber(r)
}

func pyIsUpperString(s string) bool {
	cased := false
	for _, r := range s {
		if pyIsLower(r) || unicode.IsTitle(r) {
			return false
		}
		if pyIsUpper(r) {
			cased = true
		}
	}
	return cased
}

func foldASCII(r rune) rune {
	switch {
	case r >= 'A' && r <= 'Z':
		return r + 'a' - 'A'
	case r == 0x130 || r == 0x131:
		return 'i'
	case r == 0x17f:
		return 's'
	case r == 0x212a:
		return 'k'
	}
	return r
}

func matchFold(s, word string) int {
	i := 0
	for _, w := range word {
		if i >= len(s) {
			return -1
		}
		r, n := utf8.DecodeRuneInString(s[i:])
		if foldASCII(r) != w {
			return -1
		}
		i += n
	}
	return i
}

func isFoldLetter(r rune) bool {
	return (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z') || r == 0x130 || r == 0x131 || r == 0x17f || r == 0x212a
}

func isFoldWord(r rune) bool {
	return isFoldLetter(r) || (r >= '0' && r <= '9') || r == '_'
}

func isCfgKey(r rune) bool {
	return isFoldWord(r) || r == '-' || r == '.'
}

func isTokenChar(c byte) bool {
	return (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '_' || c == '-'
}

func isAlnum(c byte) bool {
	return (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9')
}

func isHexDigit(c byte) bool {
	return (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')
}

func isLowerHex(c byte) bool {
	return (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')
}

func runeOffset(s string, k int) int {
	i := 0
	for ; k > 0 && i < len(s); k-- {
		_, w := utf8.DecodeRuneInString(s[i:])
		i += w
	}
	return i
}

func prevRune(s string, i int) rune {
	r, _ := utf8.DecodeLastRuneInString(s[:i])
	return r
}

func group(text string, m []int, i int) string {
	if m[2*i] < 0 {
		return ""
	}
	return text[m[2*i]:m[2*i+1]]
}

func replaceSubmatch(re *regexp.Regexp, text string, fn func(m []int) string) string {
	locs := re.FindAllStringSubmatchIndex(text, -1)
	if locs == nil {
		return text
	}
	var b strings.Builder
	last := 0
	for _, m := range locs {
		b.WriteString(text[last:m[0]])
		b.WriteString(fn(m))
		last = m[1]
	}
	b.WriteString(text[last:])
	return b.String()
}

func mapLines(text string, fn func(line string) string) string {
	var b strings.Builder
	changed := false
	for start := 0; ; {
		end := strings.IndexByte(text[start:], '\n')
		line := text[start:]
		if end >= 0 {
			line = text[start : start+end]
		}
		out := fn(line)
		if out != line {
			changed = true
		}
		b.WriteString(out)
		if end < 0 {
			break
		}
		b.WriteByte('\n')
		start += end + 1
	}
	if !changed {
		return text
	}
	return b.String()
}
