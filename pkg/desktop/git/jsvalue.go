package git

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"math"
	"sort"
	"strconv"
	"strings"
	"unicode/utf16"
	"unicode/utf8"
)

// JavaScript truthiness of a value decoded from renderer JSON
func jsTruthy(v any) bool {
	switch x := v.(type) {
	case nil:
		return false
	case bool:
		return x
	case float64:
		return x != 0 && !math.IsNaN(x)
	case int:
		return x != 0
	case json.Number:
		f, err := x.Float64()
		return err == nil && f != 0
	case string:
		return x != ""
	}
	return true
}

// JavaScript String() of a value decoded from renderer JSON, null reading as null
func jsString(v any) string {
	switch x := v.(type) {
	case nil:
		return "null"
	case bool:
		if x {
			return "true"
		}
		return "false"
	case float64:
		return jsNumberString(x)
	case int:
		return strconv.Itoa(x)
	case json.Number:
		f, err := x.Float64()
		if err != nil {
			return x.String()
		}
		return jsNumberString(f)
	case string:
		return x
	case []any:
		parts := make([]string, len(x))
		for i, e := range x {
			if e != nil {
				parts[i] = jsString(e)
			}
		}
		return strings.Join(parts, ",")
	case []string:
		return strings.Join(x, ",")
	}
	return "[object Object]"
}

// JavaScript Number() of a value decoded from renderer JSON
func jsNumber(v any) float64 {
	switch x := v.(type) {
	case nil:
		return 0
	case bool:
		if x {
			return 1
		}
		return 0
	case float64:
		return x
	case int:
		return float64(x)
	case json.Number:
		return jsStringToNumber(x.String())
	case string:
		return jsStringToNumber(x)
	case []any:
		return jsStringToNumber(jsString(x))
	}
	return math.NaN()
}

// Number() of a string, whitespace trimmed, empty as zero, hex octal and binary prefixes read
func jsStringToNumber(s string) float64 {
	s = jsTrim(s)
	if s == "" {
		return 0
	}
	if len(s) > 2 && s[0] == '0' {
		base := 0
		switch s[1] {
		case 'x', 'X':
			base = 16
		case 'o', 'O':
			base = 8
		case 'b', 'B':
			base = 2
		}
		if base != 0 {
			n, err := strconv.ParseUint(s[2:], base, 64)
			if err != nil {
				if ne, ok := err.(*strconv.NumError); ok && ne.Err == strconv.ErrRange {
					f := 0.0
					for _, c := range strings.ToLower(s[2:]) {
						d := strings.IndexRune("0123456789abcdef", c)
						f = f*float64(base) + float64(d)
					}
					return f
				}
				return math.NaN()
			}
			return float64(n)
		}
	}
	body := strings.TrimLeft(s, "+-")
	switch body {
	case "Infinity":
		if strings.HasPrefix(s, "-") {
			return math.Inf(-1)
		}
		if len(s)-len(body) <= 1 {
			return math.Inf(1)
		}
	}
	if len(s)-len(body) > 1 {
		return math.NaN()
	}
	for _, c := range body {
		if !strings.ContainsRune("0123456789.eE+-", c) {
			return math.NaN()
		}
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		if ne, ok := err.(*strconv.NumError); ok && ne.Err == strconv.ErrRange {
			return f
		}
		return math.NaN()
	}
	return f
}

// Number.prototype.toString of a double, the shortest round trip digits in ECMAScript layout
func jsNumberString(f float64) string {
	switch {
	case math.IsNaN(f):
		return "NaN"
	case math.IsInf(f, 1):
		return "Infinity"
	case math.IsInf(f, -1):
		return "-Infinity"
	case f == 0:
		return "0"
	}
	sign := ""
	if f < 0 {
		sign, f = "-", -f
	}
	e := strconv.FormatFloat(f, 'e', -1, 64)
	mant, exp, _ := strings.Cut(e, "e")
	digits := strings.Replace(mant, ".", "", 1)
	x, _ := strconv.Atoi(exp)
	k, n := len(digits), x+1
	switch {
	case k <= n && n <= 21:
		return sign + digits + strings.Repeat("0", n-k)
	case 0 < n && n <= 21:
		return sign + digits[:n] + "." + digits[n:]
	case -6 < n && n <= 0:
		return sign + "0." + strings.Repeat("0", -n) + digits
	}
	es := "+"
	if n-1 < 0 {
		es = "-"
	}
	ev := strconv.Itoa(abs(n - 1))
	if k == 1 {
		return sign + digits + "e" + es + ev
	}
	return sign + digits[:1] + "." + digits[1:] + "e" + es + ev
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}

// Code points String.prototype.trim removes
func isJSSpace(r rune) bool {
	switch r {
	case '\t', '\n', '\v', '\f', '\r', ' ', 0xA0, 0x1680, 0x2028, 0x2029, 0x202F, 0x205F, 0x3000, 0xFEFF:
		return true
	}
	return r >= 0x2000 && r <= 0x200A
}

// JavaScript trim of leading and trailing whitespace and line terminators
func jsTrim(s string) string { return strings.TrimFunc(s, isJSSpace) }

// JSON.stringify of a string, escaping quotes, backslashes and control characters only
func jsJSONString(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		switch {
		case r == '"':
			b.WriteString(`\"`)
		case r == '\\':
			b.WriteString(`\\`)
		case r == '\b':
			b.WriteString(`\b`)
		case r == '\f':
			b.WriteString(`\f`)
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\r':
			b.WriteString(`\r`)
		case r == '\t':
			b.WriteString(`\t`)
		case r < 0x20:
			b.WriteString(`\u00`)
			b.WriteString(strconv.FormatInt(int64(r)>>4, 16))
			b.WriteString(strconv.FormatInt(int64(r)&15, 16))
		case r == utf8.RuneError && size == 1:
			b.WriteString(`�`)
		default:
			b.WriteString(s[i : i+size])
		}
		i += size
	}
	b.WriteByte('"')
	return b.String()
}

// Property of a JSON object value, nil for a missing key or a non object
func jsGet(v any, key string) any {
	switch o := v.(type) {
	case map[string]any:
		return o[key]
	case *jsObj:
		return o.vals[key]
	}
	return nil
}

// Map of an options argument, empty for null or any non object value
func jsObject(v any) map[string]any {
	if m, ok := v.(map[string]any); ok {
		return m
	}
	return map[string]any{}
}

// JSON object as JSON.parse builds it, members in Object.keys order
type jsObj struct {
	keys []string
	vals map[string]any
}

// Members written in Object.keys order
func (o *jsObj) MarshalJSON() ([]byte, error) {
	var b bytes.Buffer
	b.WriteByte('{')
	for i, k := range o.keys {
		if i > 0 {
			b.WriteByte(',')
		}
		kb, _ := json.Marshal(k)
		b.Write(kb)
		b.WriteByte(':')
		vb, err := json.Marshal(o.vals[k])
		if err != nil {
			return nil, err
		}
		b.Write(vb)
	}
	b.WriteByte('}')
	return b.Bytes(), nil
}

// Object.keys of a parsed JSON value, indices for arrays and strings and none for other values
func jsKeys(v any) []string {
	switch o := v.(type) {
	case *jsObj:
		return o.keys
	case []any:
		out := make([]string, len(o))
		for i := range o {
			out[i] = strconv.Itoa(i)
		}
		return out
	case string:
		n := len(utf16.Encode([]rune(o)))
		out := make([]string, n)
		for i := range out {
			out[i] = strconv.Itoa(i)
		}
		return out
	}
	return nil
}

// Member of a parsed JSON value by key, array elements and string code units by index
func jsIndex(v any, key string) any {
	switch o := v.(type) {
	case *jsObj:
		return o.vals[key]
	case []any:
		if i, err := strconv.Atoi(key); err == nil && i >= 0 && i < len(o) {
			return o[i]
		}
	case string:
		units := utf16.Encode([]rune(o))
		if i, err := strconv.Atoi(key); err == nil && i >= 0 && i < len(units) {
			return string(utf16.Decode(units[i : i+1]))
		}
	}
	return nil
}

// JSON.parse with objects keeping their member order, numbers as float64
func parseJSON(text string) (any, error) {
	dec := json.NewDecoder(strings.NewReader(text))
	v, err := decodeJSONValue(dec)
	if err != nil {
		return nil, err
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, errors.New("Unexpected non-whitespace character after JSON")
	}
	return v, nil
}

func decodeJSONValue(dec *json.Decoder) (any, error) {
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	d, ok := tok.(json.Delim)
	if !ok {
		return tok, nil
	}
	if d == '[' {
		arr := []any{}
		for dec.More() {
			v, err := decodeJSONValue(dec)
			if err != nil {
				return nil, err
			}
			arr = append(arr, v)
		}
		_, err := dec.Token()
		return arr, err
	}
	o := &jsObj{vals: map[string]any{}}
	var order []string
	for dec.More() {
		kt, err := dec.Token()
		if err != nil {
			return nil, err
		}
		key, _ := kt.(string)
		v, err := decodeJSONValue(dec)
		if err != nil {
			return nil, err
		}
		if _, seen := o.vals[key]; !seen {
			order = append(order, key)
		}
		o.vals[key] = v
	}
	if _, err := dec.Token(); err != nil {
		return nil, err
	}
	var indexKeys, named []string
	for _, k := range order {
		if isArrayIndexKey(k) {
			indexKeys = append(indexKeys, k)
		} else {
			named = append(named, k)
		}
	}
	sort.Slice(indexKeys, func(i, j int) bool {
		a, _ := strconv.ParseUint(indexKeys[i], 10, 32)
		b, _ := strconv.ParseUint(indexKeys[j], 10, 32)
		return a < b
	})
	o.keys = append(indexKeys, named...)
	return o, nil
}

// True for a canonical array index key, which Object.keys lists first in ascending order
func isArrayIndexKey(k string) bool {
	if k == "" || (len(k) > 1 && k[0] == '0') {
		return false
	}
	n, err := strconv.ParseUint(k, 10, 32)
	return err == nil && n < math.MaxUint32
}
