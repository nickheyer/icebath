package file

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/nickheyer/icebath/internal/tool"
)

const (
	DefaultReadOffset   = 1
	DefaultReadLimit    = 2000
	DefaultSearchOffset = 0
	DefaultSearchLimit  = 50
)

const (
	bom             = "\xef\xbb\xbf"
	replacementChar = "\xef\xbf\xbd"
	fenceToken      = "__ICEBATH_FENCE_"
	spaceClass      = `\t\n\v\f\r \x1c-\x1f\x{85}\x{a0}\x{1680}\x{2000}-\x{200a}\x{2028}\x{2029}\x{202f}\x{205f}\x{3000}`
)

var (
	oscSequence   = regexp.MustCompile("\x1b\\][^\x07\x1b]*(?:\x07|\x1b\\\\)")
	fenceMarker   = regexp.MustCompile("'?\x07?" + fenceToken + "[A-Za-z0-9]+__\x07?'?")
	conflictOpen  = regexp.MustCompile(`(?m)^[` + spaceClass + `]*\p{Nd}+\|<<<<<<< `)
	conflictClose = regexp.MustCompile(`(?m)^[` + spaceClass + `]*\p{Nd}+\|>>>>>>> `)
)

func isSpace(r rune) bool {
	return unicode.IsSpace(r) || (r >= 0x1c && r <= 0x1f)
}

func trimSpace(s string) string {
	return strings.TrimFunc(s, isSpace)
}

func trimLeftSpace(s string) string {
	return strings.TrimLeftFunc(s, isSpace)
}

func isBlank(s string) bool {
	return trimSpace(s) == ""
}

func spaceFields(s string) []string {
	return strings.FieldsFunc(s, isSpace)
}

func splitLines(s string, keep bool) []string {
	var out []string
	start := 0
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		brk := 0
		switch r {
		case '\n', '\v', '\f', 0x1c, 0x1d, 0x1e, 0x85, 0x2028, 0x2029:
			brk = size
		case '\r':
			brk = 1
			if i+1 < len(s) && s[i+1] == '\n' {
				brk = 2
			}
		}
		if brk == 0 {
			i += size
			continue
		}
		end := i
		if keep {
			end = i + brk
		}
		out = append(out, s[start:end])
		i += brk
		start = i
	}
	if start < len(s) {
		out = append(out, s[start:])
	}
	return out
}

func quote(s string) string {
	q := byte('\'')
	if strings.Contains(s, "'") && !strings.Contains(s, `"`) {
		q = '"'
	}
	var b strings.Builder
	b.WriteByte(q)
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		if r == utf8.RuneError && size == 1 {
			fmt.Fprintf(&b, `\udc%02x`, s[i])
			i++
			continue
		}
		switch {
		case r == rune(q) || r == '\\':
			b.WriteByte('\\')
			b.WriteRune(r)
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\r':
			b.WriteString(`\r`)
		case r == '\t':
			b.WriteString(`\t`)
		case r < 0x20 || r == 0x7f:
			fmt.Fprintf(&b, `\x%02x`, r)
		case r < 0x7f || unicode.IsPrint(r):
			b.WriteRune(r)
		case r < 0x100:
			fmt.Fprintf(&b, `\x%02x`, r)
		case r < 0x10000:
			fmt.Fprintf(&b, `\u%04x`, r)
		default:
			fmt.Fprintf(&b, `\U%08x`, r)
		}
		i += size
	}
	b.WriteByte(q)
	return b.String()
}

func charLen(s string) int {
	return utf8.RuneCountInString(s)
}

func charPrefix(s string, n int) string {
	if n <= 0 {
		return ""
	}
	count := 0
	for i := range s {
		if count == n {
			return s[:i]
		}
		count++
	}
	return s
}

func charOffsetBack(s string, pos, n int) int {
	for ; n > 0 && pos > 0; n-- {
		_, size := utf8.DecodeLastRuneInString(s[:pos])
		pos -= size
	}
	return pos
}

func charOffsetForward(s string, pos, n int) int {
	for ; n > 0 && pos < len(s); n-- {
		_, size := utf8.DecodeRuneInString(s[pos:])
		pos += size
	}
	return pos
}

func decodeRunes(s string) []rune {
	out := make([]rune, 0, len(s))
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		if r == utf8.RuneError && size == 1 {
			r = 0xdc00 + rune(s[i])
		}
		out = append(out, r)
		i += size
	}
	return out
}

func encodeRunes(rs []rune) string {
	var b strings.Builder
	b.Grow(len(rs))
	for _, r := range rs {
		if r >= 0xdc80 && r <= 0xdcff {
			b.WriteByte(byte(r - 0xdc00))
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

func invalidSpan(b []byte) int {
	n := len(b)
	c := b[0]
	lo, hi := byte(0x80), byte(0xbf)
	need := 0
	switch {
	case c >= 0xc2 && c <= 0xdf:
		need = 1
	case c == 0xe0:
		need, lo = 2, 0xa0
	case c >= 0xe1 && c <= 0xec, c == 0xee, c == 0xef:
		need = 2
	case c == 0xed:
		need, hi = 2, 0x9f
	case c == 0xf0:
		need, lo = 3, 0x90
	case c >= 0xf1 && c <= 0xf3:
		need = 3
	case c == 0xf4:
		need, hi = 3, 0x8f
	default:
		return 1
	}
	span := 1
	for k := 1; k <= need && k < n; k++ {
		if b[k] < lo || b[k] > hi {
			break
		}
		span++
		lo, hi = 0x80, 0xbf
	}
	return span
}

func decodeLossy(b []byte) string {
	if utf8.Valid(b) {
		return string(b)
	}
	var out strings.Builder
	out.Grow(len(b))
	for i := 0; i < len(b); {
		if b[i] < utf8.RuneSelf {
			out.WriteByte(b[i])
			i++
			continue
		}
		r, size := utf8.DecodeRune(b[i:])
		if r != utf8.RuneError || size > 1 {
			out.Write(b[i : i+size])
			i += size
			continue
		}
		out.WriteString(replacementChar)
		i += invalidSpan(b[i:])
	}
	return out.String()
}

func firstInvalid(b []byte) int {
	for i := 0; i < len(b); {
		if b[i] < utf8.RuneSelf {
			i++
			continue
		}
		r, size := utf8.DecodeRune(b[i:])
		if r == utf8.RuneError && size == 1 {
			return i
		}
		i += size
	}
	return -1
}

func commas(n int64) string {
	s := strconv.FormatInt(n, 10)
	neg := strings.HasPrefix(s, "-")
	if neg {
		s = s[1:]
	}
	var b strings.Builder
	for i, c := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(c)
	}
	if neg {
		return "-" + b.String()
	}
	return b.String()
}

func parseInt(s string) (int, bool) {
	s = trimSpace(s)
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0, false
	}
	return n, true
}

func allDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

func stripFenceLeaks(text string) string {
	if text == "" || !(strings.Contains(text, "\x07") || strings.Contains(text, "\x1b]") || strings.Contains(text, fenceToken)) {
		return text
	}
	var out strings.Builder
	out.Grow(len(text))
	for _, line := range splitLines(text, true) {
		wrapped := strings.Contains(line, fenceToken) || strings.Contains(line, "\x1b]")
		cleaned := strings.ReplaceAll(fenceMarker.ReplaceAllString(oscSequence.ReplaceAllString(line, ""), ""), "\x07", "")
		if wrapped && strings.Trim(cleaned, "'\r\n\t ") == "" {
			continue
		}
		out.WriteString(cleaned)
	}
	return out.String()
}

func detectLineEnding(sample string) string {
	head := charPrefix(sample, 4096)
	if strings.Contains(head, "\r\n") {
		return "\r\n"
	}
	if strings.Contains(head, "\n") {
		return "\n"
	}
	return ""
}

func foldLineEndings(text string) string {
	return strings.ReplaceAll(strings.ReplaceAll(text, "\r\n", "\n"), "\r", "\n")
}

func normalizeLineEndings(text, target string) string {
	folded := foldLineEndings(text)
	switch target {
	case "\n":
		return folded
	case "\r\n":
		return strings.ReplaceAll(folded, "\n", "\r\n")
	}
	return text
}

func hasBOM(text string) bool {
	return strings.HasPrefix(text, bom)
}

func stripBOM(text string) (string, bool) {
	if hasBOM(text) {
		return text[len(bom):], true
	}
	return text, false
}

func ReadPage(offset, limit int, limits tool.OutputLimits) (int, int) {
	return max(1, offset), max(1, min(limit, limits.Resolved().MaxLines))
}

func SearchPage(offset, limit int) (int, int) {
	return max(0, offset), max(1, limit)
}

func ConflictBlocks(formatted string) int {
	opens := len(conflictOpen.FindAllStringIndex(formatted, -1))
	if opens == 0 {
		return 0
	}
	return min(opens, len(conflictClose.FindAllStringIndex(formatted, -1)))
}
