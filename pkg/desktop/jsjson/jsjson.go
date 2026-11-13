package jsjson

import (
	"encoding/json"
	"math"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

// OrderedObject is a JSON object that keeps its key order the way a JS object literal does
type OrderedObject struct {
	keys []string
	vals map[string]any
}

// Object of alternating keys and values, nil values left out as undefined is
func NewOrderedObject(kv ...any) *OrderedObject {
	o := &OrderedObject{vals: map[string]any{}}
	for i := 0; i+1 < len(kv); i += 2 {
		o.Set(kv[i].(string), kv[i+1])
	}
	return o
}

// Sets a key, appending it on first use, a nil value removes it
func (o *OrderedObject) Set(key string, v any) {
	if v == nil {
		if _, ok := o.vals[key]; ok {
			delete(o.vals, key)
			for i, k := range o.keys {
				if k == key {
					o.keys = append(o.keys[:i], o.keys[i+1:]...)
					break
				}
			}
		}
		return
	}
	if _, ok := o.vals[key]; !ok {
		o.keys = append(o.keys, key)
	}
	o.vals[key] = v
}

// Value of a key, nil when absent
func (o *OrderedObject) Get(key string) any { return o.vals[key] }

// MarshalJSON writes the keys in insertion order
func (o *OrderedObject) MarshalJSON() ([]byte, error) {
	return []byte(Stringify(o, "", "")), nil
}

// JSON.stringify of a value, with the indent JSON.stringify(v, null, n) uses when indent is set
func Stringify(v any, indent, prefix string) string {
	var b strings.Builder
	writeJS(&b, v, indent, prefix)
	return b.String()
}

func writeJS(b *strings.Builder, v any, indent, prefix string) {
	inner := prefix + indent
	nl, sep := "", ":"
	if indent != "" {
		nl, sep = "\n", ": "
	}
	switch x := v.(type) {
	case nil:
		b.WriteString("null")
	case bool:
		b.WriteString(strconv.FormatBool(x))
	case string:
		writeJSString(b, x)
	case float64:
		b.WriteString(Number(x))
	case int:
		b.WriteString(strconv.Itoa(x))
	case int64:
		b.WriteString(strconv.FormatInt(x, 10))
	case []string:
		arr := make([]any, len(x))
		for i, s := range x {
			arr[i] = s
		}
		writeJS(b, arr, indent, prefix)
	case []any:
		if len(x) == 0 {
			b.WriteString("[]")
			return
		}
		b.WriteString("[")
		for i, e := range x {
			if i > 0 {
				b.WriteString(",")
			}
			b.WriteString(nl + inner)
			writeJS(b, e, indent, inner)
		}
		b.WriteString(nl + prefix + "]")
	case *OrderedObject:
		if len(x.keys) == 0 {
			b.WriteString("{}")
			return
		}
		b.WriteString("{")
		for i, k := range x.keys {
			if i > 0 {
				b.WriteString(",")
			}
			b.WriteString(nl + inner)
			writeJSString(b, k)
			b.WriteString(sep)
			writeJS(b, x.vals[k], indent, inner)
		}
		b.WriteString(nl + prefix + "}")
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		o := &OrderedObject{keys: keys, vals: x}
		writeJS(b, o, indent, prefix)
	default:
		raw, err := json.Marshal(x)
		if err != nil {
			b.WriteString("null")
			return
		}
		var decoded any
		json.Unmarshal(raw, &decoded)
		writeJS(b, decoded, indent, prefix)
	}
}

// JSON.stringify quoting, control characters escaped and lone surrogates kept escaped
func writeJSString(b *strings.Builder, s string) {
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
			b.WriteString(hexByte(byte(r)))
		case r == utf8.RuneError && size == 1:
			b.WriteString(`\ufffd`)
		default:
			b.WriteString(s[i : i+size])
		}
		i += size
	}
	b.WriteByte('"')
}

// JSON.parse of text into maps, slices, float64, strings and bools
func Parse(text string) (any, error) {
	var v any
	dec := json.NewDecoder(strings.NewReader(text))
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	if dec.More() {
		return nil, &json.SyntaxError{}
	}
	return v, nil
}

// JSON.parse keeping object key order, objects as OrderedObject
func ParseOrdered(text string) (any, error) {
	if _, err := Parse(text); err != nil {
		return nil, err
	}
	dec := json.NewDecoder(strings.NewReader(text))
	return decodeOrdered(dec)
}

func decodeOrdered(dec *json.Decoder) (any, error) {
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	switch t := tok.(type) {
	case json.Delim:
		if t == '{' {
			o := NewOrderedObject()
			for dec.More() {
				keyTok, err := dec.Token()
				if err != nil {
					return nil, err
				}
				v, err := decodeOrdered(dec)
				if err != nil {
					return nil, err
				}
				o.setRaw(keyTok.(string), v)
			}
			_, err := dec.Token()
			return o, err
		}
		arr := []any{}
		for dec.More() {
			v, err := decodeOrdered(dec)
			if err != nil {
				return nil, err
			}
			arr = append(arr, v)
		}
		_, err := dec.Token()
		return arr, err
	}
	return tok, nil
}

// Sets a key keeping a JSON null as a present null value
func (o *OrderedObject) setRaw(key string, v any) {
	if _, ok := o.vals[key]; !ok {
		o.keys = append(o.keys, key)
	}
	o.vals[key] = v
}

// Whether the object holds a key, a present null included
func (o *OrderedObject) Has(key string) bool {
	_, ok := o.vals[key]
	return ok
}

// Sets a key to a value or a JSON null, appending it on first use
func (o *OrderedObject) SetRaw(key string, v any) { o.setRaw(key, v) }

// Removes a key as the delete operator does
func (o *OrderedObject) Delete(key string) {
	if _, ok := o.vals[key]; !ok {
		return
	}
	delete(o.vals, key)
	for i, k := range o.keys {
		if k == key {
			o.keys = append(o.keys[:i], o.keys[i+1:]...)
			return
		}
	}
}

// Copy of the object with the same key order, values shared
func (o *OrderedObject) Clone() *OrderedObject {
	c := &OrderedObject{keys: append([]string(nil), o.keys...), vals: make(map[string]any, len(o.vals))}
	for k, v := range o.vals {
		c.vals[k] = v
	}
	return c
}

// JS String() of a number
func Number(f float64) string {
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
	abs := math.Abs(f)
	if abs >= 1e21 || abs < 1e-6 {
		s := strconv.FormatFloat(f, 'g', -1, 64)
		s = strings.Replace(s, "e+0", "e+", 1)
		s = strings.Replace(s, "e-0", "e-", 1)
		return s
	}
	return strconv.FormatFloat(f, 'f', -1, 64)
}

func hexByte(c byte) string {
	const digits = "0123456789abcdef"
	return string([]byte{digits[c>>4], digits[c&15]})
}
