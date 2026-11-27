// Text helpers every part of icebath shares: cuts by characters, numbers, sizes and lengths of time in words, first filled values, sets and
// random tokens
package strs

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"
)

// First n characters of a text, its bytes kept as they are
func Head(s string, n int) string {
	i := 0
	for ; n > 0 && i < len(s); n-- {
		_, size := utf8.DecodeRuneInString(s[i:])
		i += size
	}
	return s[:i]
}

// Last n characters of a text, its bytes kept as they are
func Tail(s string, n int) string {
	i := len(s)
	for ; n > 0 && i > 0; n-- {
		_, size := utf8.DecodeLastRuneInString(s[:i])
		i -= size
	}
	return s[i:]
}

// First n characters of a text with an ellipsis after them when it was longer
func Clip(s string, n int) string {
	if head := Head(s, n); len(head) < len(s) {
		return head + "..."
	}
	return s
}

// First of the texts that is not blank, trimmed; empty when every one is
func Filled(texts ...string) string {
	for _, text := range texts {
		if text = strings.TrimSpace(text); text != "" {
			return text
		}
	}
	return ""
}

// Whole number with commas between its thousands
func Thousands[T ~int | ~int8 | ~int16 | ~int32 | ~int64 | ~uint | ~uint8 | ~uint16 | ~uint32 | ~uint64](n T) string {
	digits := strconv.FormatInt(int64(n), 10)
	if n > 0 {
		digits = strconv.FormatUint(uint64(n), 10)
	}
	sign := ""
	if rest, ok := strings.CutPrefix(digits, "-"); ok {
		sign, digits = "-", rest
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

// Set of the items
func Set(items ...string) map[string]bool {
	out := make(map[string]bool, len(items))
	for _, item := range items {
		out[item] = true
	}
	return out
}

// Items without their repeats, each where it first came, never nil
func Unique(items []string) []string {
	out := make([]string, 0, len(items))
	for _, item := range items {
		if !slices.Contains(out, item) {
			out = append(out, item)
		}
	}
	return out
}

// Hex digits of n random bytes
func Random(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// N random bytes as unpadded URL safe base64, the shape of a state, verifier or token handed out
func Token(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

// Text a pointer holds, empty for nil
func Deref(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// Text as a value JSON writes as null when it is empty
func Nullable(text string) any {
	if text == "" {
		return nil
	}
	return text
}

// First of the items, empty when there are none
func First(items []string) string {
	if len(items) == 0 {
		return ""
	}
	return items[0]
}

// The list, empty rather than nil so JSON writes it as [] and not null
func List(values []string) []string {
	if values == nil {
		return []string{}
	}
	return values
}
