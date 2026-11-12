package files

import (
	"encoding/json"
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
	return []byte(jsonStringify(o, "", "")), nil
}

// JSON.stringify of a value, with the indent JSON.stringify(v, null, n) uses when indent is set
func jsonStringify(v any, indent, prefix string) string {
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
		b.WriteString(jsNumber(x))
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
func jsonParse(text string) (any, error) {
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

// Number(value) of a decoded JSON value for a relational compare, false when it is NaN
func jsToNumber(v any) (float64, bool) {
	switch x := v.(type) {
	case nil:
		return 0, true
	case bool:
		if x {
			return 1, true
		}
		return 0, true
	case float64:
		return x, x == x
	case string:
		t := strings.TrimSpace(x)
		if t == "" {
			return 0, true
		}
		f, err := strconv.ParseFloat(t, 64)
		return f, err == nil
	}
	return 0, false
}

// JSON.parse keeping object key order, objects as OrderedObject
func jsonParseOrdered(text string) (any, error) {
	if _, err := jsonParse(text); err != nil {
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
