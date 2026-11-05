package i18n

import (
	"fmt"
	"reflect"
	"strconv"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

// Python format failure tagged with the exception class it raises
type FormatError = pyrepr.FormatError

func fmtErr(class, format string, args ...any) error {
	return &pyrepr.FormatError{Class: class, Msg: fmt.Sprintf(format, args...)}
}

// PyFormat applies Python str.format with keyword arguments only and no positional arguments
func PyFormat(template string, kwargs map[string]any) (string, error) {
	var out strings.Builder
	rs := []rune(template)
	auto := 0
	for i := 0; i < len(rs); i++ {
		c := rs[i]
		if c == '}' {
			if i+1 < len(rs) && rs[i+1] == '}' {
				out.WriteRune('}')
				i++
				continue
			}
			return "", fmtErr("ValueError", "Single '}' encountered in format string")
		}
		if c != '{' {
			out.WriteRune(c)
			continue
		}
		if i+1 < len(rs) && rs[i+1] == '{' {
			out.WriteRune('{')
			i++
			continue
		}
		depth, j := 1, i+1
		for ; j < len(rs); j++ {
			if rs[j] == '[' {
				for j < len(rs) && rs[j] != ']' {
					j++
				}
				if j >= len(rs) {
					break
				}
				continue
			}
			if rs[j] == '{' {
				depth++
			} else if rs[j] == '}' {
				depth--
				if depth == 0 {
					break
				}
			}
		}
		if j >= len(rs) {
			if depth > 1 || i+1 >= len(rs) {
				return "", fmtErr("ValueError", "Single '{' encountered in format string")
			}
			return "", fmtErr("ValueError", "expected '}' before end of string")
		}
		text, err := formatField(string(rs[i+1:j]), kwargs, &auto, 2)
		if err != nil {
			return "", err
		}
		out.WriteString(text)
		i = j
	}
	return out.String(), nil
}

// Renders one replacement field, resolving nested fields inside its format spec
func formatField(field string, kwargs map[string]any, auto *int, depth int) (string, error) {
	if depth <= 0 {
		return "", fmtErr("ValueError", "Max string recursion exceeded")
	}
	rs := []rune(field)
	end := len(rs)
	for k := 0; k < len(rs); k++ {
		if rs[k] == '[' {
			for k < len(rs) && rs[k] != ']' {
				k++
			}
			if k >= len(rs) {
				break
			}
			continue
		}
		if rs[k] == '{' {
			return "", fmtErr("ValueError", "unexpected '{' in field name")
		}
		if rs[k] == ':' || rs[k] == '!' {
			end = k
			break
		}
	}
	name := string(rs[:end])
	conv := rune(0)
	spec := ""
	if end < len(rs) {
		rest := rs[end:]
		if rest[0] == '!' {
			if len(rest) < 2 {
				return "", fmtErr("ValueError", "end of string while looking for conversion specifier")
			}
			conv = rest[1]
			rest = rest[2:]
			if len(rest) > 0 && rest[0] != ':' {
				return "", fmtErr("ValueError", "expected ':' after conversion specifier")
			}
		}
		if len(rest) > 0 {
			spec = string(rest[1:])
		}
	}
	value, err := resolveFieldName(name, kwargs, auto)
	if err != nil {
		return "", err
	}
	switch conv {
	case 0:
	case 'r':
		value = pyrepr.Repr(value)
	case 's':
		value = pyrepr.Str(value)
	case 'a':
		value = pyrepr.EscapeNonASCII(pyrepr.Repr(value))
	default:
		return "", fmtErr("ValueError", "Unknown conversion specifier %c", conv)
	}
	if strings.ContainsAny(spec, "{}") {
		spec, err = expandNestedSpec(spec, kwargs, auto, depth-1)
		if err != nil {
			return "", err
		}
	}
	return pyrepr.Format(value, spec)
}

// Expands replacement fields nested inside a format spec
func expandNestedSpec(spec string, kwargs map[string]any, auto *int, depth int) (string, error) {
	var out strings.Builder
	rs := []rune(spec)
	for i := 0; i < len(rs); i++ {
		if rs[i] != '{' {
			out.WriteRune(rs[i])
			continue
		}
		j := i + 1
		for j < len(rs) && rs[j] != '}' {
			j++
		}
		if j >= len(rs) {
			return "", fmtErr("ValueError", "expected '}' before end of string")
		}
		text, err := formatField(string(rs[i+1:j]), kwargs, auto, depth)
		if err != nil {
			return "", err
		}
		out.WriteString(text)
		i = j
	}
	return out.String(), nil
}

// Looks up the argument and walks attribute and index accessors
func resolveFieldName(name string, kwargs map[string]any, auto *int) (any, error) {
	first := len(name)
	for k, c := range name {
		if c == '.' || c == '[' {
			first = k
			break
		}
	}
	arg := name[:first]
	var value any
	switch {
	case arg == "":
		idx := *auto
		*auto++
		return nil, fmtErr("IndexError", "Replacement index %d out of range for positional args tuple", idx)
	case isDigits(arg):
		return nil, fmtErr("IndexError", "Replacement index %s out of range for positional args tuple", arg)
	default:
		v, ok := kwargs[arg]
		if !ok {
			return nil, fmtErr("KeyError", "%s", pyrepr.Repr(arg))
		}
		value = v
	}
	rest := name[first:]
	for rest != "" {
		switch rest[0] {
		case '.':
			k := 1
			for k < len(rest) && rest[k] != '.' && rest[k] != '[' {
				k++
			}
			attr := rest[1:k]
			if attr == "" {
				return nil, fmtErr("ValueError", "Empty attribute in format string")
			}
			v, err := pyGetAttr(value, attr)
			if err != nil {
				return nil, err
			}
			value, rest = v, rest[k:]
		case '[':
			k := strings.IndexByte(rest, ']')
			if k < 0 {
				return nil, fmtErr("ValueError", "Missing ']' in format string")
			}
			key := rest[1:k]
			if key == "" {
				return nil, fmtErr("ValueError", "Empty attribute in format string")
			}
			v, err := pyGetItem(value, key)
			if err != nil {
				return nil, err
			}
			value, rest = v, rest[k+1:]
			if rest != "" && rest[0] != '.' && rest[0] != '[' {
				return nil, fmtErr("ValueError", "Only '.' or '[' may follow ']' in format field specifier")
			}
		default:
			return nil, fmtErr("ValueError", "Only '.' or '[' may follow ']' in format field specifier")
		}
	}
	return value, nil
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// Struct field or map method lookup standing in for getattr
func pyGetAttr(value any, attr string) (any, error) {
	rv := reflect.ValueOf(value)
	for rv.IsValid() && (rv.Kind() == reflect.Pointer || rv.Kind() == reflect.Interface) && !rv.IsNil() {
		rv = rv.Elem()
	}
	if rv.IsValid() && rv.Kind() == reflect.Struct {
		if f := rv.FieldByName(attr); f.IsValid() && f.CanInterface() {
			return f.Interface(), nil
		}
	}
	return nil, fmtErr("AttributeError", "'%s' object has no attribute '%s'", pyrepr.TypeName(value), attr)
}

// Sequence index for digit keys, mapping lookup otherwise, like format field subscripts
func pyGetItem(value any, key string) (any, error) {
	rv := reflect.ValueOf(value)
	if !rv.IsValid() {
		return nil, fmtErr("TypeError", "'NoneType' object is not subscriptable")
	}
	if s, ok := value.(string); ok {
		if !isDigits(key) {
			return nil, fmtErr("TypeError", "string indices must be integers, not 'str'")
		}
		rs := []rune(s)
		n, _ := strconv.Atoi(key)
		if n >= len(rs) {
			return nil, fmtErr("IndexError", "string index out of range")
		}
		return string(rs[n]), nil
	}
	switch rv.Kind() {
	case reflect.Slice, reflect.Array:
		if !isDigits(key) {
			return nil, fmtErr("TypeError", "list indices must be integers or slices, not str")
		}
		n, _ := strconv.Atoi(key)
		if n >= rv.Len() {
			return nil, fmtErr("IndexError", "list index out of range")
		}
		return rv.Index(n).Interface(), nil
	case reflect.Map:
		var k reflect.Value
		kt := rv.Type().Key()
		if isDigits(key) {
			n, _ := strconv.ParseInt(key, 10, 64)
			switch kt.Kind() {
			case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
				k = reflect.ValueOf(n).Convert(kt)
			case reflect.Interface:
				k = reflect.ValueOf(int(n))
			default:
				return nil, fmtErr("KeyError", "%s", key)
			}
		} else if kt.Kind() == reflect.String || kt.Kind() == reflect.Interface {
			k = reflect.ValueOf(key).Convert(kt)
		} else {
			return nil, fmtErr("KeyError", "%s", pyrepr.Repr(key))
		}
		v := rv.MapIndex(k)
		if !v.IsValid() {
			if isDigits(key) {
				return nil, fmtErr("KeyError", "%s", key)
			}
			return nil, fmtErr("KeyError", "%s", pyrepr.Repr(key))
		}
		return v.Interface(), nil
	}
	return nil, fmtErr("TypeError", "'%s' object is not subscriptable", pyrepr.TypeName(value))
}
