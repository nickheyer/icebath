package pm

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/pelletier/go-toml/v2"
)

const tomliWMaxLineLength = 100

// Renders an ordered document the way tomli_w dumps does with four space indents and no multiline strings
func tomliWDumps(document *OrderedObject) (string, error) {
	var b strings.Builder
	if err := tomliWTable(&b, document, "", false); err != nil {
		return "", err
	}
	return b.String(), nil
}

// Whether a value is a nonempty array whose every item is a table
func tomliWIsAOT(v any) bool {
	items, ok := v.([]any)
	if !ok || len(items) == 0 {
		return false
	}
	for _, item := range items {
		if _, ok := item.(*OrderedObject); !ok {
			return false
		}
	}
	return true
}

// Writes one table, its literals first then its subtables and arrays of tables
func tomliWTable(b *strings.Builder, table *OrderedObject, name string, insideAOT bool) error {
	type subtable struct {
		key   string
		table *OrderedObject
		aot   bool
	}
	var literals []string
	var tables []subtable
	for _, k := range table.Keys {
		v := table.Values[k]
		if sub, ok := v.(*OrderedObject); ok {
			tables = append(tables, subtable{k, sub, false})
			continue
		}
		if tomliWIsAOT(v) {
			suitable := true
			for _, item := range v.([]any) {
				inline, err := tomliWInlineTable(item.(*OrderedObject))
				if err != nil {
					return err
				}
				rendered := "    " + inline + ","
				if len(rendered) > tomliWMaxLineLength || strings.Contains(rendered, "\n") {
					suitable = false
				}
			}
			if !suitable {
				for _, item := range v.([]any) {
					tables = append(tables, subtable{k, item.(*OrderedObject), true})
				}
				continue
			}
		}
		literals = append(literals, k)
	}
	yielded := false
	if insideAOT || (name != "" && (len(literals) > 0 || len(tables) == 0)) {
		yielded = true
		if insideAOT {
			b.WriteString("[[" + name + "]]\n")
		} else {
			b.WriteString("[" + name + "]\n")
		}
	}
	for _, k := range literals {
		yielded = true
		literal, err := tomliWLiteral(table.Values[k], 0)
		if err != nil {
			return err
		}
		b.WriteString(tomliWKey(k) + " = " + literal + "\n")
	}
	for _, t := range tables {
		if yielded {
			b.WriteString("\n")
		} else {
			yielded = true
		}
		display := tomliWKey(t.key)
		if name != "" {
			display = name + "." + display
		}
		if err := tomliWTable(b, t.table, display, t.aot); err != nil {
			return err
		}
	}
	return nil
}

// Key text, bare when every character is a letter, digit, dash or underscore
func tomliWKey(key string) string {
	if key == "" {
		return tomliWString(key)
	}
	for i := 0; i < len(key); i++ {
		c := key[i]
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
			return tomliWString(key)
		}
	}
	return key
}

// Basic string with quotes, backslashes and control characters escaped, tab kept literal
func tomliWString(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '\b':
			b.WriteString(`\b`)
		case '\n':
			b.WriteString(`\n`)
		case '\f':
			b.WriteString(`\f`)
		case '\r':
			b.WriteString(`\r`)
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		default:
			if (r < 0x20 && r != '\t') || r == 0x7f {
				fmt.Fprintf(&b, `\u%04x`, r)
			} else {
				b.WriteRune(r)
			}
		}
	}
	b.WriteByte('"')
	return b.String()
}

// Inline table text, empty tables rendered as two braces
func tomliWInlineTable(table *OrderedObject) (string, error) {
	if len(table.Keys) == 0 {
		return "{}", nil
	}
	parts := make([]string, len(table.Keys))
	for i, k := range table.Keys {
		literal, err := tomliWLiteral(table.Values[k], 0)
		if err != nil {
			return "", err
		}
		parts[i] = tomliWKey(k) + " = " + literal
	}
	return "{ " + strings.Join(parts, ", ") + " }", nil
}

// Python str of a float, repr digits with inf and nan spelled as TOML accepts
func tomliWFloat(f float64) string {
	switch {
	case math.IsInf(f, 1):
		return "inf"
	case math.IsInf(f, -1):
		return "-inf"
	case math.IsNaN(f):
		return "nan"
	}
	return pyFloatRepr(f)
}

// Python str of a time of day, microseconds shown only when present
func tomliWClock(hour, minute, second, nanos int) string {
	out := fmt.Sprintf("%02d:%02d:%02d", hour, minute, second)
	if micro := nanos / 1000; micro != 0 {
		out += fmt.Sprintf(".%06d", micro)
	}
	return out
}

// One value as tomli_w formats it, arrays one item per line indented by nesting level
func tomliWLiteral(v any, nest int) (string, error) {
	switch t := v.(type) {
	case bool:
		if t {
			return "true", nil
		}
		return "false", nil
	case int64:
		return strconv.FormatInt(t, 10), nil
	case int:
		return strconv.Itoa(t), nil
	case float64:
		return tomliWFloat(t), nil
	case string:
		return tomliWString(t), nil
	case toml.LocalDate:
		return fmt.Sprintf("%04d-%02d-%02d", t.Year, t.Month, t.Day), nil
	case toml.LocalTime:
		return tomliWClock(t.Hour, t.Minute, t.Second, t.Nanosecond), nil
	case toml.LocalDateTime:
		return fmt.Sprintf("%04d-%02d-%02d %s", t.Year, t.Month, t.Day, tomliWClock(t.Hour, t.Minute, t.Second, t.Nanosecond)), nil
	case time.Time:
		_, offset := t.Zone()
		sign := "+"
		if offset < 0 {
			sign, offset = "-", -offset
		}
		zone := fmt.Sprintf("%s%02d:%02d", sign, offset/3600, offset/60%60)
		if offset%60 != 0 {
			zone += fmt.Sprintf(":%02d", offset%60)
		}
		return fmt.Sprintf("%04d-%02d-%02d %s%s", t.Year(), t.Month(), t.Day(), tomliWClock(t.Hour(), t.Minute(), t.Second(), t.Nanosecond()), zone), nil
	case []string:
		items := make([]any, len(t))
		for i, s := range t {
			items[i] = s
		}
		return tomliWLiteral(items, nest)
	case []any:
		if len(t) == 0 {
			return "[]", nil
		}
		indent := strings.Repeat("    ", nest+1)
		parts := make([]string, len(t))
		for i, item := range t {
			literal, err := tomliWLiteral(item, nest+1)
			if err != nil {
				return "", err
			}
			parts[i] = indent + literal
		}
		return "[\n" + strings.Join(parts, ",\n") + ",\n" + strings.Repeat("    ", nest) + "]", nil
	case *OrderedObject:
		return tomliWInlineTable(t)
	}
	return "", &PyError{Class: "TypeError", Message: fmt.Sprintf("Object of type %T is not TOML serializable", v)}
}
