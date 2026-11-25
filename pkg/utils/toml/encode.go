package tomlutil

import (
	"errors"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/pelletier/go-toml/v2"

	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
)

// Error of a value TOML has no form for, wrapped with the value's Go type
var ErrUnsupportedType = errors.New("toml: unsupported value type")

// Longest line an array of tables may take written inline before it is written as [[header]] tables
const maxInlineLine = 100

const indentUnit = "    "

// Most fraction digits a local time is written with; go-toml slices past the end of its nine digits for a larger precision
const maxFraction = 9

// Key and value of one table entry
type entry struct {
	key   string
	value any
}

// Ordered document as TOML text: each table's plain values first, then its subtables under [header] lines in key order, arrays one item per line indented by four spaces, an array of tables inline when every table fits on one line and as [[header]] tables otherwise; tables may be *jsonutil.Object or map[string]any (written in sorted key order), an error wrapping ErrUnsupportedType for a value of a type TOML has no form for, and an error for an integer past 64 signed bits or an offset date-time outside the years 0 to 9999
func Encode(doc *jsonutil.Object) (string, error) {
	var b strings.Builder
	if err := writeTable(&b, entriesOf(doc), "", false); err != nil {
		return "", err
	}
	return b.String(), nil
}

// Writes one table: its header when it needs one, its plain values, then its subtables and arrays of tables
func writeTable(b *strings.Builder, table []entry, name string, inArray bool) error {
	type subtable struct {
		key     string
		table   []entry
		inArray bool
	}
	var literals []entry
	var tables []subtable
	for _, e := range table {
		if sub, ok := tableEntries(e.value); ok {
			tables = append(tables, subtable{e.key, sub, false})
			continue
		}
		if items, ok := tableArray(e.value); ok {
			inline, err := fitsInline(items)
			if err != nil {
				return err
			}
			if !inline {
				for _, item := range items {
					tables = append(tables, subtable{e.key, item, true})
				}
				continue
			}
		}
		literals = append(literals, e)
	}
	written := false
	if inArray || (name != "" && (len(literals) > 0 || len(tables) == 0)) {
		written = true
		if inArray {
			b.WriteString("[[" + name + "]]\n")
		} else {
			b.WriteString("[" + name + "]\n")
		}
	}
	for _, e := range literals {
		written = true
		b.WriteString(formatKey(e.key))
		b.WriteString(" = ")
		if err := writeValue(b, e.value, 0); err != nil {
			return err
		}
		b.WriteByte('\n')
	}
	for _, t := range tables {
		if written {
			b.WriteByte('\n')
		}
		written = true
		header := formatKey(t.key)
		if name != "" {
			header = name + "." + header
		}
		if err := writeTable(b, t.table, header, t.inArray); err != nil {
			return err
		}
	}
	return nil
}

// Whether every table of an array of tables fits inline on one indented line of the array
func fitsInline(items [][]entry) (bool, error) {
	var line strings.Builder
	for _, item := range items {
		line.Reset()
		line.WriteString(indentUnit)
		if err := writeInlineTable(&line, item); err != nil {
			return false, err
		}
		line.WriteByte(',')
		text := line.String()
		if utf8.RuneCountInString(text) > maxInlineLine || strings.Contains(text, "\n") {
			return false, nil
		}
	}
	return true, nil
}

// Entries of a table value in their written order, false for a value that is no table
func tableEntries(v any) ([]entry, bool) {
	switch t := v.(type) {
	case *jsonutil.Object:
		return entriesOf(t), true
	case map[string]any:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		slices.Sort(keys)
		out := make([]entry, len(keys))
		for i, k := range keys {
			out[i] = entry{k, t[k]}
		}
		return out, true
	}
	return nil, false
}

func entriesOf(o *jsonutil.Object) []entry {
	out := make([]entry, 0, o.Len())
	for k, v := range o.All() {
		out = append(out, entry{k, v})
	}
	return out
}

// Tables of a nonempty array whose every item is a table, false otherwise
func tableArray(v any) ([][]entry, bool) {
	items, ok := arrayItems(v)
	if !ok || len(items) == 0 {
		return nil, false
	}
	out := make([][]entry, len(items))
	for i, item := range items {
		if out[i], ok = tableEntries(item); !ok {
			return nil, false
		}
	}
	return out, true
}

// Items of an array value, false for a value that is no array
func arrayItems(v any) ([]any, bool) {
	switch t := v.(type) {
	case []any:
		return t, true
	case []string:
		return anySlice(t), true
	case []int:
		return anySlice(t), true
	case []int64:
		return anySlice(t), true
	case []float64:
		return anySlice(t), true
	case []bool:
		return anySlice(t), true
	case []*jsonutil.Object:
		return anySlice(t), true
	case []map[string]any:
		return anySlice(t), true
	case []time.Time:
		return anySlice(t), true
	}
	return nil, false
}

func anySlice[E any](s []E) []any {
	out := make([]any, len(s))
	for i, v := range s {
		out[i] = v
	}
	return out
}

// Writes one value, arrays one item per line indented by their nesting level
func writeValue(b *strings.Builder, v any, nest int) error {
	switch t := v.(type) {
	case bool:
		b.WriteString(strconv.FormatBool(t))
	case string:
		writeString(b, t)
	case int:
		b.WriteString(strconv.FormatInt(int64(t), 10))
	case int8:
		b.WriteString(strconv.FormatInt(int64(t), 10))
	case int16:
		b.WriteString(strconv.FormatInt(int64(t), 10))
	case int32:
		b.WriteString(strconv.FormatInt(int64(t), 10))
	case int64:
		b.WriteString(strconv.FormatInt(t, 10))
	case uint8:
		b.WriteString(strconv.FormatUint(uint64(t), 10))
	case uint16:
		b.WriteString(strconv.FormatUint(uint64(t), 10))
	case uint32:
		b.WriteString(strconv.FormatUint(uint64(t), 10))
	case uint:
		return writeUint(b, uint64(t))
	case uint64:
		return writeUint(b, t)
	case float64:
		b.WriteString(formatFloat(t, 64))
	case float32:
		b.WriteString(formatFloat(float64(t), 32))
	case time.Time:
		text, err := t.MarshalText()
		if err != nil {
			return fmt.Errorf("toml: %w", err)
		}
		b.Write(text)
	case toml.LocalDate:
		b.WriteString(t.String())
	case toml.LocalTime:
		t.Precision = min(t.Precision, maxFraction)
		b.WriteString(t.String())
	case toml.LocalDateTime:
		t.Precision = min(t.Precision, maxFraction)
		b.WriteString(t.String())
	default:
		if table, ok := tableEntries(v); ok {
			return writeInlineTable(b, table)
		}
		if items, ok := arrayItems(v); ok {
			return writeArray(b, items, nest)
		}
		return fmt.Errorf("%w %T", ErrUnsupportedType, v)
	}
	return nil
}

func writeUint(b *strings.Builder, n uint64) error {
	if n > math.MaxInt64 {
		return fmt.Errorf("toml: integer %d does not fit in 64 signed bits", n)
	}
	b.WriteString(strconv.FormatUint(n, 10))
	return nil
}

// Writes an array as [] when empty, otherwise each item on its own line followed by a comma
func writeArray(b *strings.Builder, items []any, nest int) error {
	if len(items) == 0 {
		b.WriteString("[]")
		return nil
	}
	b.WriteString("[\n")
	for _, item := range items {
		for range nest + 1 {
			b.WriteString(indentUnit)
		}
		if err := writeValue(b, item, nest+1); err != nil {
			return err
		}
		b.WriteString(",\n")
	}
	for range nest {
		b.WriteString(indentUnit)
	}
	b.WriteByte(']')
	return nil
}

// Writes a table inline as { key = value, ... }, an empty one as {}
func writeInlineTable(b *strings.Builder, table []entry) error {
	if len(table) == 0 {
		b.WriteString("{}")
		return nil
	}
	b.WriteString("{ ")
	for i, e := range table {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(formatKey(e.key))
		b.WriteString(" = ")
		if err := writeValue(b, e.value, 0); err != nil {
			return err
		}
	}
	b.WriteString(" }")
	return nil
}

// TOML text of a key as Encode writes it: bare when it is nonempty and every byte is an ASCII letter, digit, dash or underscore, otherwise a quoted basic string
func FormatKey(key string) string { return formatKey(key) }

// TOML basic string of s as Encode writes it: quotes, backslashes and control characters escaped, tabs kept, invalid UTF-8 replaced by U+FFFD
func FormatString(s string) string {
	var b strings.Builder
	writeString(&b, s)
	return b.String()
}

// TOML float text of f as Encode writes it, bits 32 or 64 as for strconv.FormatFloat: reads back as the same value and as a float, inf and nan spelled as TOML has them
func FormatFloat(f float64, bits int) string { return formatFloat(f, bits) }

// Key as written: bare when it is nonempty and every byte is a letter, digit, dash or underscore, otherwise quoted
func formatKey(key string) string {
	if key == "" {
		return `""`
	}
	for i := range len(key) {
		c := key[i]
		if !('a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' || '0' <= c && c <= '9' || c == '-' || c == '_') {
			var b strings.Builder
			writeString(&b, key)
			return b.String()
		}
	}
	return key
}

// Writes s as a basic string: quotes, backslashes and control characters escaped, tabs kept, invalid UTF-8 replaced by U+FFFD
func writeString(b *strings.Builder, s string) {
	b.Grow(len(s) + 2)
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
			if r < 0x20 && r != '\t' || r == 0x7f {
				b.WriteString(`\u00`)
				b.WriteByte("0123456789abcdef"[r>>4])
				b.WriteByte("0123456789abcdef"[r&0xf])
			} else {
				b.WriteRune(r)
			}
		}
	}
	b.WriteByte('"')
}

// Shortest float text that reads back as the same value and as a float: a fraction or exponent always present, inf and nan spelled as TOML has them
func formatFloat(f float64, bits int) string {
	switch {
	case math.IsInf(f, 1):
		return "inf"
	case math.IsInf(f, -1):
		return "-inf"
	case math.IsNaN(f):
		return "nan"
	}
	if abs := math.Abs(f); abs != 0 && (abs < 1e-6 || abs >= 1e21) {
		s := strconv.FormatFloat(f, 'e', -1, bits)
		if n := len(s); n >= 4 && s[n-4] == 'e' && s[n-3] == '-' && s[n-2] == '0' {
			s = s[:n-2] + s[n-1:]
		}
		return s
	}
	s := strconv.FormatFloat(f, 'f', -1, bits)
	if !strings.ContainsRune(s, '.') {
		s += ".0"
	}
	return s
}
