package pyrepr

import (
	"strconv"
	"strings"
)

// Python str.format_map over values, with item lookup, conversions, nested specs and doubled braces
func FormatMap(template string, values map[string]any) (string, error) {
	return formatMapDepth(template, values, 2)
}

func formatMapDepth(template string, values map[string]any, depth int) (string, error) {
	if depth < 0 {
		return "", fmtErr("ValueError", "Max string recursion exceeded")
	}
	var b strings.Builder
	rs := []rune(template)
	for i := 0; i < len(rs); i++ {
		switch r := rs[i]; r {
		case '}':
			if i+1 < len(rs) && rs[i+1] == '}' {
				b.WriteRune('}')
				i++
				continue
			}
			return "", fmtErr("ValueError", "Single '}' encountered in format string")
		case '{':
			if i+1 < len(rs) && rs[i+1] == '{' {
				b.WriteRune('{')
				i++
				continue
			}
			end, nest := i+1, 1
			for ; end < len(rs); end++ {
				if rs[end] == '{' {
					nest++
				} else if rs[end] == '}' {
					if nest--; nest == 0 {
						break
					}
				}
			}
			if end >= len(rs) {
				if nest > 1 {
					return "", fmtErr("ValueError", "unmatched '{' in format spec")
				}
				return "", fmtErr("ValueError", "expected '}' before end of string")
			}
			out, err := formatField(string(rs[i+1:end]), values, depth)
			if err != nil {
				return "", err
			}
			b.WriteString(out)
			i = end
		default:
			b.WriteRune(r)
		}
	}
	return b.String(), nil
}

func formatField(field string, values map[string]any, depth int) (string, error) {
	spec, conv, hasConv := "", "", false
	head := field
	for j, nest := 0, 0; j < len(field); j++ {
		c := field[j]
		if c == '[' {
			nest++
		} else if c == ']' && nest > 0 {
			nest--
		} else if nest == 0 && (c == ':' || c == '!') {
			head = field[:j]
			rest := field[j:]
			if c == '!' {
				if len(rest) < 2 {
					return "", fmtErr("ValueError", "end of string while looking for conversion specifier")
				}
				conv, hasConv, rest = rest[1:2], true, rest[2:]
				if rest != "" && rest[0] != ':' {
					return "", fmtErr("ValueError", "expected ':' after conversion specifier")
				}
			}
			if rest != "" {
				spec = rest[1:]
			}
			break
		}
	}
	value, err := lookupField(head, values)
	if err != nil {
		return "", err
	}
	if hasConv {
		switch conv {
		case "r":
			value = Repr(value)
		case "s":
			value = Str(value)
		case "a":
			value = ASCII(value)
		default:
			return "", fmtErr("ValueError", "Unknown conversion specifier %s", conv)
		}
	}
	if strings.ContainsRune(spec, '{') {
		if spec, err = formatMapDepth(spec, values, depth-1); err != nil {
			return "", err
		}
	}
	return Format(value, spec)
}

func lookupField(name string, values map[string]any) (any, error) {
	end := strings.IndexAny(name, ".[")
	if end < 0 {
		end = len(name)
	}
	first := name[:end]
	if first == "" || isDecimalDigits(first) {
		idx := 0
		if first != "" {
			idx, _ = strconv.Atoi(first)
		}
		return nil, fmtErr("IndexError", "Replacement index %d out of range for positional args tuple", idx)
	}
	value, ok := values[first]
	if !ok {
		return nil, fmtErr("KeyError", "%s", StrRepr(first))
	}
	rest := name[end:]
	for rest != "" {
		if rest[0] == '.' {
			attrEnd := strings.IndexAny(rest[1:], ".[")
			if attrEnd < 0 {
				attrEnd = len(rest) - 1
			}
			attr := rest[1 : 1+attrEnd]
			if attr == "" {
				return nil, fmtErr("ValueError", "Empty attribute in format string")
			}
			return nil, fmtErr("AttributeError", "'%s' object has no attribute %s", TypeName(value), StrRepr(attr))
		}
		if rest[0] != '[' {
			return nil, fmtErr("ValueError", "Only '.' or '[' may follow ']' in format field specifier")
		}
		closeAt := strings.IndexByte(rest, ']')
		if closeAt < 0 {
			return nil, fmtErr("ValueError", "Missing ']' in format string")
		}
		key := rest[1:closeAt]
		if key == "" {
			return nil, fmtErr("ValueError", "Empty attribute in format string")
		}
		var err error
		if value, err = itemOf(value, key); err != nil {
			return nil, err
		}
		rest = rest[closeAt+1:]
	}
	return value, nil
}

func itemOf(value any, key string) (any, error) {
	numeric := isDecimalDigits(key)
	idx, _ := strconv.Atoi(key)
	switch v := value.(type) {
	case map[string]any:
		if got, ok := v[key]; ok && !numeric {
			return got, nil
		}
	case Mapping:
		for k, got := range v.PyItems() {
			if k == key && !numeric {
				return got, nil
			}
		}
	case []any:
		if !numeric {
			return nil, fmtErr("TypeError", "list indices must be integers or slices, not str")
		}
		if idx < len(v) {
			return v[idx], nil
		}
		return nil, fmtErr("IndexError", "list index out of range")
	case string:
		if !numeric {
			return nil, fmtErr("TypeError", "string indices must be integers, not 'str'")
		}
		if rs := []rune(v); idx < len(rs) {
			return string(rs[idx]), nil
		}
		return nil, fmtErr("IndexError", "string index out of range")
	default:
		return nil, fmtErr("TypeError", "'%s' object is not subscriptable", TypeName(value))
	}
	if numeric {
		return nil, fmtErr("KeyError", "%d", idx)
	}
	return nil, fmtErr("KeyError", "%s", StrRepr(key))
}

func isDecimalDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}
