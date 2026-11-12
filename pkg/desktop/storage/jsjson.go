package storage

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"math"
	"strconv"
	"strings"
	"unicode/utf16"
)

// JavaScript plain object whose keys enumerate in JavaScript property order
type Object struct {
	keys []string
	vals map[string]any
}

// Empty object
func NewObject() *Object { return &Object{vals: map[string]any{}} }

// Value under key and whether the key is present
func (o *Object) Get(key string) (any, bool) {
	v, ok := o.vals[key]
	return v, ok
}

// Assigns key, keeping its position when present and appending it otherwise
func (o *Object) Set(key string, v any) {
	if _, ok := o.vals[key]; !ok {
		o.keys = append(o.keys, key)
	}
	o.vals[key] = v
}

// Removes key so a later assignment appends it again
func (o *Object) Delete(key string) {
	if _, ok := o.vals[key]; !ok {
		return
	}
	delete(o.vals, key)
	for i, k := range o.keys {
		if k == key {
			o.keys = append(o.keys[:i:i], o.keys[i+1:]...)
			break
		}
	}
}

// Number of own keys
func (o *Object) Len() int { return len(o.keys) }

// Own keys in JavaScript order, array index keys ascending first and then insertion order
func (o *Object) Keys() []string {
	var index []string
	var named []string
	for _, k := range o.keys {
		if isArrayIndex(k) {
			index = append(index, k)
		} else {
			named = append(named, k)
		}
	}
	for i := 1; i < len(index); i++ {
		for j := i; j > 0 && arrayIndexValue(index[j]) < arrayIndexValue(index[j-1]); j-- {
			index[j], index[j-1] = index[j-1], index[j]
		}
	}
	return append(index, named...)
}

// Shallow copy in the same key order as an object spread
func (o *Object) Copy() *Object {
	c := NewObject()
	for _, k := range o.Keys() {
		c.Set(k, o.vals[k])
	}
	return c
}

// Compact JSON.stringify text of the object
func (o *Object) MarshalJSON() ([]byte, error) { return []byte(jsStringify(o, "")), nil }

func isArrayIndex(k string) bool {
	if k == "0" {
		return true
	}
	if k == "" || k[0] < '1' || k[0] > '9' || len(k) > 10 {
		return false
	}
	for i := 0; i < len(k); i++ {
		if k[i] < '0' || k[i] > '9' {
			return false
		}
	}
	return arrayIndexValue(k) < 4294967295
}

func arrayIndexValue(k string) uint64 {
	n, _ := strconv.ParseUint(k, 10, 64)
	return n
}

// Parses text as JSON.parse does, objects as *Object, numbers as float64
func jsParse(text string) (any, error) {
	dec := json.NewDecoder(strings.NewReader(text))
	dec.UseNumber()
	v, err := jsParseValue(dec)
	if err != nil {
		return nil, err
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, errors.New("Unexpected non-whitespace character after JSON")
	}
	return v, nil
}

func jsParseValue(dec *json.Decoder) (any, error) {
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	switch t := tok.(type) {
	case json.Delim:
		switch t {
		case '{':
			obj := NewObject()
			for dec.More() {
				kt, err := dec.Token()
				if err != nil {
					return nil, err
				}
				key, _ := kt.(string)
				v, err := jsParseValue(dec)
				if err != nil {
					return nil, err
				}
				obj.Set(key, v)
			}
			if _, err := dec.Token(); err != nil {
				return nil, err
			}
			return obj, nil
		case '[':
			arr := []any{}
			for dec.More() {
				v, err := jsParseValue(dec)
				if err != nil {
					return nil, err
				}
				arr = append(arr, v)
			}
			if _, err := dec.Token(); err != nil {
				return nil, err
			}
			return arr, nil
		}
		return nil, errors.New("Unexpected token in JSON")
	case json.Number:
		f, err := strconv.ParseFloat(string(t), 64)
		if err != nil && !errors.Is(err, strconv.ErrRange) {
			return nil, err
		}
		return f, nil
	}
	return tok, nil
}

// JSON.stringify text of v, indented by indent when it is not empty
func jsStringify(v any, indent string) string {
	var b bytes.Buffer
	jsWrite(&b, v, indent, "")
	return b.String()
}

func jsWrite(b *bytes.Buffer, v any, indent, cur string) {
	switch t := v.(type) {
	case nil:
		b.WriteString("null")
	case bool:
		b.WriteString(strconv.FormatBool(t))
	case float64:
		if math.IsInf(t, 0) || math.IsNaN(t) {
			b.WriteString("null")
		} else {
			b.WriteString(jsNumber(t))
		}
	case int:
		b.WriteString(strconv.Itoa(t))
	case int64:
		b.WriteString(strconv.FormatInt(t, 10))
	case string:
		jsQuote(b, t)
	case *Object:
		keys := t.Keys()
		if len(keys) == 0 {
			b.WriteString("{}")
			return
		}
		inner := cur + indent
		b.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				b.WriteByte(',')
			}
			if indent != "" {
				b.WriteString("\n" + inner)
			}
			jsQuote(b, k)
			b.WriteByte(':')
			if indent != "" {
				b.WriteByte(' ')
			}
			jsWrite(b, t.vals[k], indent, inner)
		}
		if indent != "" {
			b.WriteString("\n" + cur)
		}
		b.WriteByte('}')
	case []any:
		if len(t) == 0 {
			b.WriteString("[]")
			return
		}
		inner := cur + indent
		b.WriteByte('[')
		for i, e := range t {
			if i > 0 {
				b.WriteByte(',')
			}
			if indent != "" {
				b.WriteString("\n" + inner)
			}
			jsWrite(b, e, indent, inner)
		}
		if indent != "" {
			b.WriteString("\n" + cur)
		}
		b.WriteByte(']')
	default:
		raw, _ := json.Marshal(t)
		b.Write(raw)
	}
}

// Number.prototype.toString text of a finite number
func jsNumber(f float64) string {
	if f == 0 {
		return "0"
	}
	if f < 0 {
		return "-" + jsNumber(-f)
	}
	e := strconv.FormatFloat(f, 'e', -1, 64)
	mant, expText, _ := strings.Cut(e, "e")
	digits := strings.Replace(mant, ".", "", 1)
	exp, _ := strconv.Atoi(expText)
	k := len(digits)
	n := exp + 1
	switch {
	case k <= n && n <= 21:
		return digits + strings.Repeat("0", n-k)
	case 0 < n && n <= 21:
		return digits[:n] + "." + digits[n:]
	case -6 < n && n <= 0:
		return "0." + strings.Repeat("0", -n) + digits
	}
	sign := "+"
	if n-1 < 0 {
		sign = "-"
	}
	abs := n - 1
	if abs < 0 {
		abs = -abs
	}
	if k == 1 {
		return digits + "e" + sign + strconv.Itoa(abs)
	}
	return digits[:1] + "." + digits[1:] + "e" + sign + strconv.Itoa(abs)
}

// Writes s as a JSON.stringify string literal
func jsQuote(b *bytes.Buffer, s string) {
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\b':
			b.WriteString(`\b`)
		case '\f':
			b.WriteString(`\f`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		default:
			if r < 0x20 {
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

// String length in UTF-16 code units as JavaScript counts it
func jsLength(s string) int {
	n := 0
	for _, r := range s {
		n += len(utf16.Encode([]rune{r}))
	}
	return n
}

// Trims the whitespace JavaScript String.prototype.trim removes
func jsTrim(s string) string {
	return strings.TrimFunc(s, func(r rune) bool {
		switch r {
		case '\t', '\n', '\v', '\f', '\r', ' ', 0xa0, 0x1680, 0x2028, 0x2029, 0x202f, 0x205f, 0x3000, 0xfeff:
			return true
		}
		return r >= 0x2000 && r <= 0x200a
	})
}
