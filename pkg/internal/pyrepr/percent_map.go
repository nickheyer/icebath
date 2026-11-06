package pyrepr

import (
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Python exception class and message a printf style interpolation raised
type PercentError struct {
	Class string
	Msg   string
}

func (e *PercentError) Error() string { return e.Class + ": " + e.Msg }

// Python printf style interpolation of a mapping format, the %(key)s form logging formatters render
func PercentMap(format string, lookup func(key string) (any, bool)) (string, error) {
	var b strings.Builder
	for i := 0; i < len(format); {
		if format[i] != '%' {
			b.WriteByte(format[i])
			i++
			continue
		}
		i++
		if i >= len(format) {
			return "", &PercentError{"ValueError", "incomplete format"}
		}
		if format[i] == '%' {
			b.WriteByte('%')
			i++
			continue
		}
		if format[i] != '(' {
			return "", &PercentError{"TypeError", "format requires a mapping"}
		}
		depth, j := 1, i+1
		for j < len(format) && depth > 0 {
			switch format[j] {
			case '(':
				depth++
			case ')':
				depth--
			}
			j++
		}
		if depth > 0 {
			return "", &PercentError{"ValueError", "incomplete format key"}
		}
		key := format[i+1 : j-1]
		i = j
		flags := ""
		for i < len(format) && strings.IndexByte("-+ #0", format[i]) >= 0 {
			flags += string(format[i])
			i++
		}
		width := ""
		for i < len(format) && format[i] >= '0' && format[i] <= '9' {
			width += string(format[i])
			i++
		}
		prec := -1
		if i < len(format) && format[i] == '.' {
			i++
			p := 0
			for i < len(format) && format[i] >= '0' && format[i] <= '9' {
				p = p*10 + int(format[i]-'0')
				i++
			}
			prec = p
		}
		for i < len(format) && strings.IndexByte("hlL", format[i]) >= 0 {
			i++
		}
		if i >= len(format) {
			return "", &PercentError{"ValueError", "incomplete format"}
		}
		conv := format[i]
		i++
		v, ok := lookup(key)
		if !ok {
			return "", &PercentError{"KeyError", StrRepr(key)}
		}
		text, err := percentConvert(conv, flags, width, prec, v, i-1)
		if err != nil {
			return "", err
		}
		b.WriteString(text)
	}
	return b.String(), nil
}

// One conversion of a printf style field with its flags, width and precision applied
func percentConvert(conv byte, flags, width string, prec int, v any, index int) (string, error) {
	switch conv {
	case 's', 'r', 'a':
		var text string
		switch conv {
		case 's':
			text = Str(v)
		case 'r':
			text = Repr(v)
		default:
			text = ASCII(v)
		}
		if prec >= 0 && utf8.RuneCountInString(text) > prec {
			text = string([]rune(text)[:prec])
		}
		return percentPad(text, flags, width), nil
	case 'd', 'i', 'u', 'x', 'X', 'o', 'c':
		n, ok := percentInt(v)
		if !ok {
			if conv == 'c' {
				if s, isStr := v.(string); isStr && utf8.RuneCountInString(s) == 1 {
					return percentPad(s, flags, width), nil
				}
				return "", &PercentError{"TypeError", "%c requires an int or a unicode character, not " + TypeName(v)}
			}
			kind := "a real number"
			if conv == 'x' || conv == 'X' || conv == 'o' {
				kind = "an integer"
			}
			return "", &PercentError{"TypeError", fmt.Sprintf("%%%c format: %s is required, not %s", conv, kind, TypeName(v))}
		}
		if conv == 'c' {
			return percentPad(string(rune(n)), flags, width), nil
		}
		verb := map[byte]string{'d': "d", 'i': "d", 'u': "d", 'x': "x", 'X': "X", 'o': "o"}[conv]
		spec := "%" + strings.ReplaceAll(flags, "#", "") + width
		if prec >= 0 {
			spec += "." + strconv.Itoa(prec)
		}
		text := fmt.Sprintf(spec+verb, n)
		if strings.Contains(flags, "#") && conv != 'd' && conv != 'i' && conv != 'u' {
			prefix := map[byte]string{'x': "0x", 'X': "0X", 'o': "0o"}[conv]
			trimmed := strings.TrimLeft(text, " ")
			sign := ""
			if strings.HasPrefix(trimmed, "-") {
				sign, trimmed = "-", trimmed[1:]
			}
			text = percentPad(sign+prefix+strings.TrimLeft(trimmed, "0"), flags, width)
			if strings.HasSuffix(text, prefix) {
				text += "0"
			}
		}
		return text, nil
	case 'f', 'F', 'e', 'E', 'g', 'G':
		f, ok := percentFloat(v)
		if !ok {
			return "", &PercentError{"TypeError", "must be real number, not " + TypeName(v)}
		}
		spec := "%" + flags + width
		if prec >= 0 {
			spec += "." + strconv.Itoa(prec)
		}
		return fmt.Sprintf(spec+string(conv), f), nil
	}
	return "", &PercentError{"ValueError", fmt.Sprintf("unsupported format character %s (0x%x) at index %d", StrRepr(string(conv)), conv, index)}
}

// Pads text to width with spaces, on the right when the minus flag is set
func percentPad(text, flags, width string) string {
	w, _ := strconv.Atoi(width)
	pad := w - utf8.RuneCountInString(text)
	if pad <= 0 {
		return text
	}
	if strings.Contains(flags, "-") {
		return text + strings.Repeat(" ", pad)
	}
	return strings.Repeat(" ", pad) + text
}

// Integer value of a numeric field, floats truncated toward zero as int() does
func percentInt(v any) (int64, bool) {
	switch n := v.(type) {
	case int:
		return int64(n), true
	case int64:
		return n, true
	case int32:
		return int64(n), true
	case uint64:
		return int64(n), true
	case uint32:
		return int64(n), true
	case bool:
		if n {
			return 1, true
		}
		return 0, true
	case float64:
		return int64(n), true
	}
	return 0, false
}

// Float value of a numeric field
func percentFloat(v any) (float64, bool) {
	if f, ok := v.(float64); ok {
		return f, true
	}
	n, ok := percentInt(v)
	return float64(n), ok
}
