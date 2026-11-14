package redact

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

const approvalSpace = `[ \t\x{a0}\x{1680}\x{2000}-\x{200a}\x{202f}\x{205f}\x{3000}]`

var reApprovalBlank = regexp.MustCompile(`\n(?:` + approvalSpace + `*\n){3,}`)

func isDisplayControl(r rune, w int) bool {
	if r == utf8.RuneError && w == 1 {
		return false
	}
	return r <= 0x1f || (r >= 0x7f && r <= 0x9f) || (r >= 0x200b && r <= 0x200f) || (r >= 0x202a && r <= 0x202e) || (r >= 0x2060 && r <= 0x2064)
}

func maskSecret(value string, head, tail, floor int, placeholder, empty string) string {
	var b strings.Builder
	for i := 0; i < len(value); {
		r, w := utf8.DecodeRuneInString(value[i:])
		if !isDisplayControl(r, w) {
			b.WriteString(value[i : i+w])
		}
		i += w
	}
	value = b.String()
	if value == "" {
		return empty
	}
	n := utf8.RuneCountInString(value)
	if n < floor {
		return placeholder
	}
	return value[:runeOffset(value, head)] + "..." + value[runeOffset(value, n-tail):]
}

func MaskSecret(value, empty string) string {
	return maskSecret(value, 4, 4, 12, "***", empty)
}

func isApprovalSpace(r rune) bool {
	return r == ' ' || r == '\t' || r == 0xa0 || r == 0x1680 || (r >= 0x2000 && r <= 0x200a) || r == 0x202f || r == 0x205f || r == 0x3000
}

func isBlankGlyph(r rune) bool {
	switch r {
	case 0x034f, 0x115f, 0x1160, 0x17b4, 0x17b5, 0x180b, 0x180c, 0x180d, 0x180f, 0x2800, 0x3164, 0xffa0:
		return true
	}
	return (r >= 0xfe00 && r <= 0xfe0f) || (r >= 0xe0100 && r <= 0xe01ef)
}

func isProseBidiControl(r rune) bool {
	return (r >= 0x202a && r <= 0x202e) || (r >= 0x2066 && r <= 0x2069)
}

func isOtherCategory(r rune) bool {
	if unicode.Is(unicode.C, r) {
		return true
	}
	return !unicode.In(r, unicode.L, unicode.M, unicode.N, unicode.P, unicode.S, unicode.Z)
}

func hiddenChar(r rune, prose bool) bool {
	if prose {
		return unicode.Is(unicode.Cc, r) || r == 0x2028 || r == 0x2029 || isProseBidiControl(r)
	}
	return isBlankGlyph(r) || isOtherCategory(r) || r == 0x2028 || r == 0x2029
}

func escapeChar(r rune) string {
	switch {
	case r == '\t':
		return `\t`
	case r == '\n':
		return `\n`
	case r == '\r':
		return `\r`
	case r == '\\':
		return `\\`
	case r < 0x20 || (r >= 0x7f && r < 0x100):
		return fmt.Sprintf(`\x%02x`, r)
	case r < 0x80:
		return string(r)
	case r < 0x10000:
		return fmt.Sprintf(`\u%04x`, r)
	}
	return fmt.Sprintf(`\U%08x`, r)
}

func decodeDisplay(s string, i int) (rune, int) {
	r, w := utf8.DecodeRuneInString(s[i:])
	if r == utf8.RuneError && w == 1 {
		return 0xdc00 + rune(s[i]), 1
	}
	return r, w
}

func escapeHidden(text string, prose bool) string {
	var b strings.Builder
	for i := 0; i < len(text); {
		c := text[i]
		if c == '\t' || c == '\n' || (c >= 0x20 && c <= 0x7e) {
			b.WriteByte(c)
			i++
			continue
		}
		r, w := decodeDisplay(text, i)
		j, n := i+w, 1
		for j < len(text) {
			r2, w2 := decodeDisplay(text, j)
			if r2 != r {
				break
			}
			j += w2
			n++
		}
		if !hiddenChar(r, prose) {
			b.WriteString(text[i:j])
		} else if esc := escapeChar(r); n >= 4 {
			b.WriteString("⟨" + esc + " ×" + strconv.Itoa(n) + "⟩")
		} else {
			b.WriteString(strings.Repeat(esc, n))
		}
		i = j
	}
	return b.String()
}

func collapsePadding(text string) string {
	var b strings.Builder
	last := 0
	for i := 0; i < len(text); {
		r, w := utf8.DecodeRuneInString(text[i:])
		if !isApprovalSpace(r) {
			i += w
			continue
		}
		j, n := i, 0
		for j < len(text) {
			r2, w2 := utf8.DecodeRuneInString(text[j:])
			if !isApprovalSpace(r2) {
				break
			}
			j += w2
			n++
		}
		midLine := i > 0 && !isPySpace(prevRune(text, i)) && n >= 21
		lineStart := (i == 0 || text[i-1] == '\n') && n >= 65
		if midLine || lineStart {
			b.WriteString(text[last:i])
			b.WriteString(" ⟨+" + strconv.Itoa(n) + " whitespace chars⟩ ")
			last = j
		}
		i = j
	}
	if last == 0 {
		return text
	}
	b.WriteString(text[last:])
	return b.String()
}

func Command(text string, prose bool) string {
	if text == "" {
		return ""
	}
	out := escapeHidden(text, prose)
	out = reApprovalBlank.ReplaceAllStringFunc(out, func(m string) string {
		return "\n⟨+" + strconv.Itoa(strings.Count(m, "\n")-1) + " blank lines⟩\n"
	})
	return collapsePadding(out)
}
