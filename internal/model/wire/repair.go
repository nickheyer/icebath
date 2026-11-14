package wire

import (
	"log/slog"
	"math"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Longest slice of unrepairable arguments the warning keeps, the last surviving copy of what the model wrote
const fullArgsLogBound = 100_000

// Escapes raw control characters inside the string values of malformed JSON
func escapeControls(raw string) string {
	var b strings.Builder
	inString := false
	for i := 0; i < len(raw); i++ {
		c := raw[i]
		if inString && c == '\\' && i+1 < len(raw) {
			_, size := utf8.DecodeRuneInString(raw[i+1:])
			b.WriteString(raw[i : i+1+size])
			i += size
			continue
		}
		if c == '"' {
			inString = !inString
		}
		if inString && c < 0x20 {
			b.WriteString(`\u00`)
			b.WriteByte(hexDigits[c>>4])
			b.WriteByte(hexDigits[c&0xf])
			continue
		}
		b.WriteByte(c)
	}
	return b.String()
}

// Closes open braces and brackets in stack order, false when the text ends inside a string
func rebalanceClosers(raw string) (string, bool) {
	var b strings.Builder
	var stack []byte
	unclosed := map[byte]int{}
	closer := func(open byte) byte { return open + 2 }
	inString := false
	for i := 0; i < len(raw); i++ {
		c := raw[i]
		switch {
		case inString && c == '\\':
			_, size := utf8.DecodeRuneInString(raw[min(i+1, len(raw)):])
			b.WriteString(raw[i : i+1+size])
			i += size
			continue
		case inString:
			inString = c != '"'
		case c == '"':
			inString = true
		case c == '{' || c == '[':
			stack = append(stack, c)
			unclosed[c+2]++
		case c == '}' || c == ']':
			if unclosed[c] > 0 {
				for closer(stack[len(stack)-1]) != c {
					b.WriteByte(closer(stack[len(stack)-1]))
					unclosed[closer(stack[len(stack)-1])]--
					stack = stack[:len(stack)-1]
				}
				stack = stack[:len(stack)-1]
				unclosed[c]--
			}
		}
		b.WriteByte(c)
	}
	if inString {
		return "", false
	}
	for i := len(stack) - 1; i >= 0; i-- {
		b.WriteByte(closer(stack[i]))
	}
	return b.String(), true
}

// Removes every comma that only whitespace separates from a closing brace or bracket
func dropTrailingCommas(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == ',' {
			j := i + 1
			for j < len(s) {
				r, size := utf8.DecodeRuneInString(s[j:])
				if !isSpace(r) {
					break
				}
				j += size
			}
			if j < len(s) && (s[j] == '}' || s[j] == ']') {
				i = j - 1
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// Cuts a text to its first n characters for a log line
func preview(s string, n int) string {
	for i := range s {
		if n == 0 {
			return s[:i]
		}
		n--
	}
	return s
}

// Repairs malformed tool call argument JSON, an empty object standing in for arguments beyond repair
func RepairArguments(raw, tool string) string {
	stripped := trimSpace(raw)
	if stripped == "" {
		slog.Warn("sanitized empty tool call arguments", "tool", tool)
		return "{}"
	}
	if stripped == "None" {
		slog.Warn("sanitized null-literal tool call arguments", "tool", tool)
		return "{}"
	}
	if compact, ok := FormatJSON([]byte(stripped), TextLayout{}); ok {
		if compact != stripped {
			slog.Warn("repaired unescaped control characters in tool call arguments", "tool", tool)
		}
		return compact
	}
	fixed := dropTrailingCommas(stripped)
	if balanced, ok := rebalanceClosers(fixed); ok && balanced != "" {
		fixed = balanced
	}
	for range 50 {
		surplus := (strings.HasSuffix(fixed, "}") && strings.Count(fixed, "}") > strings.Count(fixed, "{")) ||
			(strings.HasSuffix(fixed, "]") && strings.Count(fixed, "]") > strings.Count(fixed, "["))
		if looseValid(fixed, true) || !surplus {
			break
		}
		fixed = fixed[:len(fixed)-1]
	}
	if looseValid(fixed, true) {
		slog.Warn("repaired malformed tool call arguments", "tool", tool, "was", preview(stripped, 80), "now", preview(fixed, 80))
		return fixed
	}
	if escaped := escapeControls(fixed); escaped != fixed && looseValid(escaped, true) {
		slog.Warn("repaired control characters in malformed tool call arguments", "tool", tool, "was", preview(stripped, 80), "now", preview(escaped, 80))
		return escaped
	}
	slog.Warn("unrepairable tool call arguments replaced with an empty object", "tool", tool, "was", preview(stripped, fullArgsLogBound))
	return "{}"
}

// Coerces a replayed tool name to the 64 letters, digits, underscores and hyphens every wire accepts
func CoerceToolName(name, fallback string) string {
	valid := len(name) >= 1 && len(name) <= 64
	for i := 0; i < len(name) && valid; i++ {
		valid = toolNameByte(name[i])
	}
	if valid {
		return name
	}
	var b strings.Builder
	for _, r := range trimSpace(name) {
		switch {
		case r < utf8.RuneSelf && toolNameByte(byte(r)) && r != '_':
			b.WriteByte(byte(r))
		case !strings.HasSuffix(b.String(), "_"):
			b.WriteByte('_')
		}
	}
	coerced := strings.Trim(b.String(), "_")
	if len(coerced) > 64 {
		coerced = coerced[:64]
	}
	if coerced == "" {
		return fallback
	}
	return coerced
}

// Reports whether c may appear in a tool name
func toolNameByte(c byte) bool {
	return c == '_' || c == '-' || (c >= '0' && c <= '9') || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

// Integer a text spells with an optional sign and digits of any script, single underscores allowed between digits
func parseInteger(s string) (int64, bool) {
	s = trimSpace(s)
	negative := strings.HasPrefix(s, "-")
	if negative || strings.HasPrefix(s, "+") {
		s = s[1:]
	}
	var v uint64
	seen, gap := false, false
	for _, r := range s {
		if r == '_' {
			if !seen || gap {
				return 0, false
			}
			gap = true
			continue
		}
		d, ok := digitValue(r)
		if !ok {
			return 0, false
		}
		seen, gap = true, false
		if v > (math.MaxInt64-uint64(d))/10 {
			v = math.MaxInt64
			continue
		}
		v = v*10 + uint64(d)
	}
	if !seen || gap {
		return 0, false
	}
	if negative {
		return -int64(v), true
	}
	return int64(v), true
}

// Rewrites lone surrogate escapes and invalid UTF-8 in JSON text to U+FFFD, returning the input when it is clean
func ScrubSurrogates(body []byte) []byte {
	var out []byte
	last := 0
	replace := func(at, width int, with string) {
		if out == nil {
			out = make([]byte, 0, len(body)+8)
		}
		out = append(append(out, body[last:at]...), with...)
		last = at + width
	}
	unit := func(i int) (rune, bool) {
		if i+6 > len(body) || body[i] != '\\' || body[i+1] != 'u' {
			return 0, false
		}
		v, err := strconv.ParseUint(string(body[i+2:i+6]), 16, 16)
		return rune(v), err == nil
	}
	for i := 0; i < len(body); {
		c := body[i]
		switch {
		case c == '\\':
			v, ok := unit(i)
			switch {
			case !ok:
				i += 2
			case v >= 0xd800 && v <= 0xdbff:
				if low, ok := unit(i + 6); ok && low >= 0xdc00 && low <= 0xdfff {
					i += 12
					continue
				}
				replace(i, 6, `\u`+"fffd")
				i += 6
			case v >= 0xdc00 && v <= 0xdfff:
				replace(i, 6, `\u`+"fffd")
				i += 6
			default:
				i += 6
			}
		case c < utf8.RuneSelf:
			i++
		default:
			r, size := utf8.DecodeRune(body[i:])
			if r == utf8.RuneError && size == 1 {
				replace(i, 1, string(utf8.RuneError))
			}
			i += size
		}
	}
	if out == nil {
		return body
	}
	return append(out, body[last:]...)
}
