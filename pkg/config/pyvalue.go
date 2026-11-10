package config

import (
	"fmt"
	"math"
	"math/big"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode"
)

// Python repr of a YAML shaped value with sorted mapping keys
func PyRepr(v any) string {
	switch x := v.(type) {
	case nil:
		return "None"
	case bool:
		if x {
			return "True"
		}
		return "False"
	case string:
		return pyStringRepr(x)
	case int:
		return strconv.Itoa(x)
	case int64:
		return strconv.FormatInt(x, 10)
	case uint64:
		return strconv.FormatUint(x, 10)
	case float64:
		return pyFloatRepr(x)
	case float32:
		return pyFloatRepr(float64(x))
	case []any:
		parts := make([]string, len(x))
		for i, item := range x {
			parts[i] = PyRepr(item)
		}
		return "[" + strings.Join(parts, ", ") + "]"
	case []string:
		parts := make([]string, len(x))
		for i, item := range x {
			parts[i] = pyStringRepr(item)
		}
		return "[" + strings.Join(parts, ", ") + "]"
	}
	if m := AsMap(v); m != nil {
		keys := SortedKeys(m)
		parts := make([]string, len(keys))
		for i, k := range keys {
			parts[i] = pyStringRepr(k) + ": " + PyRepr(m[k])
		}
		return "{" + strings.Join(parts, ", ") + "}"
	}
	return fmt.Sprint(v)
}

// Python str of a value, the text itself for a string and the repr otherwise
func PyStr(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return PyRepr(v)
}

// Single or double quoted Python string literal with Python escapes
func pyStringRepr(s string) string {
	quote := byte('\'')
	if strings.ContainsRune(s, '\'') && !strings.ContainsRune(s, '"') {
		quote = '"'
	}
	var b strings.Builder
	b.WriteByte(quote)
	for _, r := range s {
		switch {
		case r == '\\':
			b.WriteString(`\\`)
		case r == rune(quote):
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
		case r < 0x80 || unicode.IsPrint(r):
			b.WriteRune(r)
		case r <= 0xff:
			fmt.Fprintf(&b, `\x%02x`, r)
		case r <= 0xffff:
			fmt.Fprintf(&b, `\u%04x`, r)
		default:
			fmt.Fprintf(&b, `\U%08x`, r)
		}
	}
	b.WriteByte(quote)
	return b.String()
}

// Shortest round trip float text in Python repr form
func pyFloatRepr(f float64) string {
	switch {
	case math.IsNaN(f):
		return "nan"
	case math.IsInf(f, 1):
		return "inf"
	case math.IsInf(f, -1):
		return "-inf"
	}
	sci := strconv.FormatFloat(f, 'e', -1, 64)
	exp, _ := strconv.Atoi(sci[strings.IndexByte(sci, 'e')+1:])
	if f != 0 && (exp < -4 || exp >= 16) {
		return sci
	}
	s := strconv.FormatFloat(f, 'f', -1, 64)
	if !strings.ContainsRune(s, '.') {
		s += ".0"
	}
	return s
}

// Python type name of a YAML shaped value
func PyTypeName(v any) string {
	switch v.(type) {
	case nil:
		return "NoneType"
	case bool:
		return "bool"
	case string:
		return "str"
	case int, int64, uint64, int32, *big.Int:
		return "int"
	case float64, float32:
		return "float"
	}
	if AsMap(v) != nil {
		return "dict"
	}
	if rv := reflect.ValueOf(v); rv.Kind() == reflect.Slice {
		return "list"
	}
	return fmt.Sprintf("%T", v)
}

// Python truthiness of a YAML shaped value
func PyTruthy(v any) bool {
	switch x := v.(type) {
	case nil:
		return false
	case bool:
		return x
	case string:
		return x != ""
	case int:
		return x != 0
	case int64:
		return x != 0
	case uint64:
		return x != 0
	case float64:
		return x != 0
	}
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.Slice, reflect.Map:
		return rv.Len() > 0
	}
	return true
}

var shlexUnsafe = regexp.MustCompile(`[^\w@%+=:,./-]`)

// Shell quoting that matches Python shlex quote
func ShlexQuote(s string) string {
	if s == "" {
		return "''"
	}
	if !shlexUnsafe.MatchString(s) {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'"'"'`) + "'"
}

// Sorted repr list of a string set in Python list form
func pySortedList(set map[string]bool) string {
	keys := make([]string, 0, len(set))
	for k := range set {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return PyRepr(keys)
}
