package textutil

import (
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// First n characters of s, all of s when it has no more and nothing when n is not positive
func First(s string, n int) string {
	if n <= 0 {
		return ""
	}
	for i := range s {
		if n == 0 {
			return s[:i]
		}
		n--
	}
	return s
}

// Last n characters of s, all of s when it has no more and nothing when n is not positive
func Last(s string, n int) string {
	if n <= 0 {
		return ""
	}
	i := len(s)
	for ; i > 0 && n > 0; n-- {
		_, size := utf8.DecodeLastRuneInString(s[:i])
		i -= size
	}
	return s[i:]
}

// What is left of s without its first n characters
func DropFirst(s string, n int) string { return s[len(First(s, n)):] }

// What is left of s without its last n characters
func DropLast(s string, n int) string { return s[:len(s)-len(Last(s, n))] }

// Characters of s from position start up to but not including position end, both counted in characters and kept inside s
func Between(s string, start, end int) string { return First(DropFirst(s, start), end-max(start, 0)) }

// Lines of s without their line ends, split at \n with a \r before it dropped; a line end at the very end starts no empty line
func Lines(s string) []string {
	lines := []string{}
	for line := range strings.Lines(s) {
		lines = append(lines, strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r"))
	}
	return lines
}

// Words of s like strings.Fields, stopping after n words: what follows them is the last item, whole, its leading space cut
func FieldsN(s string, n int) []string {
	fields := []string{}
	for len(fields) < n {
		s = strings.TrimLeftFunc(s, unicode.IsSpace)
		if s == "" {
			return fields
		}
		end := strings.IndexFunc(s, unicode.IsSpace)
		if end < 0 {
			return append(fields, s)
		}
		fields, s = append(fields, s[:end]), s[end:]
	}
	if s = strings.TrimLeftFunc(s, unicode.IsSpace); s != "" {
		fields = append(fields, s)
	}
	return fields
}

// Whole number in decimal with a comma between each group of three digits
func Thousands(n int64) string {
	digits := strconv.FormatInt(n, 10)
	sign := ""
	if n < 0 {
		sign, digits = "-", digits[1:]
	}
	var b strings.Builder
	b.WriteString(sign)
	for i := range len(digits) {
		if i > 0 && (len(digits)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteByte(digits[i])
	}
	return b.String()
}
