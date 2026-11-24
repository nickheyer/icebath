package jsonschemautil

import (
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"

	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
)

const (
	// Bytes of a value from the instance quoted in a message before it is cut short
	instanceQuoteLimit = 200
	// Bytes of a value from the schema quoted in a message before it is cut short
	schemaQuoteLimit = 1000
	// Names or items a message lists before it only counts the rest
	maxListed = 20
	// Alternatives a summary of anyOf or oneOf failures describes
	maxAlternatives = 6
	// Bytes of a summary of alternatives past which no further alternative is described
	maxSummary = 1200
	// Bytes of one alternative's message inside a summary
	maxAlternativeMessage = 400
)

// Compact JSON text of a value, cut at a character boundary and ended with ... once longer than limit bytes when limit is positive; only about limit bytes of it are ever written
func quote(v any, limit int) string {
	var b []byte
	if limit > 0 {
		b = appendQuoted(make([]byte, 0, min(limit, 64)), v, limit, 0)
	} else {
		b = appendJSON(nil, v)
	}
	if limit > 0 && len(b) > limit {
		return cut(string(b), limit) + "..."
	}
	return string(b)
}

// buf with the compact JSON text of a value appended as jsonutil writes it, a value it cannot encode written as Go prints it
func appendJSON(buf []byte, v any) []byte {
	out, err := jsonutil.Append(buf, v, jsonutil.Convert(func(x any) (any, error) { return fmt.Sprint(x), nil }))
	if err != nil {
		return fmt.Append(buf, v)
	}
	return out
}

// buf with the compact JSON text of a value appended, stopping once buf holds more than limit bytes: strings are cut before they are escaped and containers end early, so a huge value costs no more than its first limit bytes
func appendQuoted(buf []byte, v any, limit, depth int) []byte {
	if len(buf) > limit {
		return buf
	}
	switch x := v.(type) {
	case string:
		// The text is cut past the limit: once quoted it still runs past the limit, so the caller marks it cut
		if room := limit - len(buf) + 8; len(x) > room {
			x = cut(x, room)
		}
		return appendJSON(buf, x)
	case []any:
		if len(x) == 0 || depth >= maxValueDepth {
			return appendJSON(buf, x)
		}
		buf = append(buf, '[')
		for i, item := range x {
			if len(buf) > limit {
				return buf
			}
			if i > 0 {
				buf = append(buf, ',')
			}
			buf = appendQuoted(buf, item, limit, depth+1)
		}
		return append(buf, ']')
	case *jsonutil.Object, map[string]any:
		obj, ok := objectOf(v)
		if !ok || obj.len() == 0 || depth >= maxValueDepth {
			return appendJSON(buf, v)
		}
		buf = append(buf, '{')
		for i, k := range obj.keys() {
			if len(buf) > limit {
				return buf
			}
			if i > 0 {
				buf = append(buf, ',')
			}
			buf = appendQuoted(buf, k, limit, depth+1)
			buf = append(buf, ':')
			val, _ := obj.get(k)
			buf = appendQuoted(buf, val, limit, depth+1)
		}
		return append(buf, '}')
	}
	return appendJSON(buf, v)
}

// Instance value quoted for a message
func quoteValue(v any) string { return quote(v, instanceQuoteLimit) }

// Schema value quoted for a message
func quoteSchema(v any) string { return quote(v, schemaQuoteLimit) }

// At most n bytes of s, ending on a character boundary when s is valid UTF-8 there
func cut(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for back := 0; back < utf8.UTFMax-1 && n > 0 && !utf8.RuneStart(s[n]); back++ {
		n--
	}
	return s[:n]
}

// Strings quoted as JSON and joined with commas, conj before the last one when conj is not empty
func quoteList(items []string, conj string) string {
	parts := make([]string, len(items))
	for i, s := range items {
		parts[i] = quote(s, schemaQuoteLimit)
	}
	return joinList(parts, conj)
}

// Parts joined with commas, conj before the last one when conj is not empty
func joinList(parts []string, conj string) string {
	if conj == "" || len(parts) < 2 {
		return strings.Join(parts, ", ")
	}
	return strings.Join(parts[:len(parts)-1], ", ") + " " + conj + " " + parts[len(parts)-1]
}

// Property names quoted as JSON, the ones past maxListed only counted
func nameList(names []string) string {
	shown := names[:min(len(names), maxListed)]
	text := quoteList(shown, "")
	if rest := len(names) - len(shown); rest > 0 {
		text += " and " + strconv.Itoa(rest) + " more"
	}
	return text
}

// Instance values quoted as JSON, the ones past maxListed only counted
func valueList(values []any) string {
	shown := values[:min(len(values), maxListed)]
	parts := make([]string, len(shown))
	for i, v := range shown {
		parts[i] = quoteValue(v)
	}
	text := strings.Join(parts, ", ")
	if rest := len(values) - len(shown); rest > 0 {
		text += " and " + strconv.Itoa(rest) + " more"
	}
	return text
}

// n with the singular or plural noun
func count(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return strconv.Itoa(n) + " " + many
}

func wasWere(n int) string {
	if n == 1 {
		return "was"
	}
	return "were"
}

// Positions joined with commas and a final "and"
func positionList(positions []int) string {
	parts := make([]string, len(positions))
	for i, p := range positions {
		parts[i] = strconv.Itoa(p)
	}
	return joinList(parts, "and")
}

// Message on a count of characters, items or properties outside a minimum or maximum
func countMessage(v any, n, limit int, lower bool, one, many string) string {
	q := quoteValue(v)
	switch {
	case lower && limit == 1 && n == 0:
		return q + " should be non-empty"
	case !lower && limit == 0:
		return q + " is expected to be empty"
	case lower:
		return q + " has " + count(n, one, many) + ", fewer than the minimum of " + strconv.Itoa(limit)
	}
	return q + " has " + count(n, one, many) + ", more than the maximum of " + strconv.Itoa(limit)
}

// Head followed by the most relevant failure of each failed alternative under its label; ends marks where each alternative's failures end in causes, and depth is the length of the path to the value they share
func alternatives(head string, causes []*ValidationError, ends []int, labels []string, depth int) string {
	if len(ends) == 0 {
		return head
	}
	var b strings.Builder
	b.WriteString(head)
	b.WriteString(" (")
	start := 0
	for i, end := range ends {
		if i > 0 {
			b.WriteString("; ")
		}
		if i == maxAlternatives || b.Len() > maxSummary {
			b.WriteString(strconv.Itoa(len(ends) - i))
			b.WriteString(" more")
			break
		}
		if i < len(labels) {
			b.WriteString(labels[i])
			b.WriteString(": ")
		}
		if best := BestMatch(causes[start:end]); best != nil {
			if depth < len(best.InstancePath) {
				b.WriteString("at ")
				b.WriteString(pointer(best.InstancePath[depth:]))
				b.WriteString(": ")
			}
			if len(best.Message) > maxAlternativeMessage {
				b.WriteString(cut(best.Message, maxAlternativeMessage))
				b.WriteString("...")
			} else {
				b.WriteString(best.Message)
			}
		}
		start = end
	}
	b.WriteByte(')')
	return b.String()
}
