package state

import (
	"github.com/nickheyer/icebath/pkg/internal/pyjson"

	"encoding/json"
	"errors"
	"fmt"
	"math"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Holds the json.dumps keyword arguments state code passes
type pyJSON struct {
	sortKeys    bool
	ensureASCII bool
	allowNaN    bool
	itemSep     string
	keySep      string
}

// Mirrors json.dumps with no keyword arguments
var pyJSONDefault = pyJSON{ensureASCII: true, allowNaN: true, itemSep: ", ", keySep: ": "}

// One entry of an insertion ordered JSON object
type pyKeyValue struct {
	Key   string
	Value any
}

// A JSON object that keeps insertion order like a Python dict
type pyObject []pyKeyValue

var errPyJSONNaN = errors.New("Out of range float values are not JSON compliant")

// Encodes a value the way Python's json.dumps does under the given options
func pyDumps(v any, o pyJSON) (string, error) {
	var b strings.Builder
	if err := o.encode(&b, v); err != nil {
		return "", err
	}
	return b.String(), nil
}

// Writes one value in json.dumps form
func (o pyJSON) encode(b *strings.Builder, v any) error {
	switch x := v.(type) {
	case nil:
		b.WriteString("null")
	case bool:
		if x {
			b.WriteString("true")
		} else {
			b.WriteString("false")
		}
	case string:
		o.encodeString(b, x)
	case json.Number:
		b.WriteString(x.String())
	case float64:
		return o.encodeFloat(b, x)
	case float32:
		return o.encodeFloat(b, float64(x))
	case int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64:
		fmt.Fprint(b, x)
	case pyObject:
		return o.encodeObject(b, x)
	case *pyjson.Object:
		if x == nil {
			b.WriteString("null")
			return nil
		}
		obj := make(pyObject, 0, len(x.Keys))
		for _, k := range x.Keys {
			obj = append(obj, pyKeyValue{k, x.Vals[k]})
		}
		return o.encodeObject(b, obj)
	case map[string]any:
		obj := make(pyObject, 0, len(x))
		for k, val := range x {
			obj = append(obj, pyKeyValue{k, val})
		}
		sort.Slice(obj, func(i, j int) bool { return obj[i].Key < obj[j].Key })
		return o.encodeObject(b, obj)
	case []any:
		return o.encodeList(b, len(x), func(i int) any { return x[i] })
	default:
		rv := reflect.ValueOf(v)
		switch rv.Kind() {
		case reflect.Slice, reflect.Array:
			return o.encodeList(b, rv.Len(), func(i int) any { return rv.Index(i).Interface() })
		case reflect.Map:
			if rv.Type().Key().Kind() != reflect.String {
				return fmt.Errorf("keys must be str, int, float, bool or None, not %s", rv.Type().Key())
			}
			obj := make(pyObject, 0, rv.Len())
			iter := rv.MapRange()
			for iter.Next() {
				obj = append(obj, pyKeyValue{iter.Key().String(), iter.Value().Interface()})
			}
			sort.Slice(obj, func(i, j int) bool { return obj[i].Key < obj[j].Key })
			return o.encodeObject(b, obj)
		case reflect.Pointer:
			if rv.IsNil() {
				b.WriteString("null")
				return nil
			}
			return o.encode(b, rv.Elem().Interface())
		}
		return fmt.Errorf("Object of type %T is not JSON serializable", v)
	}
	return nil
}

// Writes a float as Python's float repr, refusing non finite values unless allowed
func (o pyJSON) encodeFloat(b *strings.Builder, f float64) error {
	switch {
	case math.IsNaN(f):
		if !o.allowNaN {
			return fmt.Errorf("%w: %s", errPyJSONNaN, "nan")
		}
		b.WriteString("NaN")
	case math.IsInf(f, 1):
		if !o.allowNaN {
			return fmt.Errorf("%w: %s", errPyJSONNaN, "inf")
		}
		b.WriteString("Infinity")
	case math.IsInf(f, -1):
		if !o.allowNaN {
			return fmt.Errorf("%w: %s", errPyJSONNaN, "-inf")
		}
		b.WriteString("-Infinity")
	default:
		b.WriteString(pyFloatRepr(f))
	}
	return nil
}

// Writes a list with the item separator
func (o pyJSON) encodeList(b *strings.Builder, n int, at func(int) any) error {
	b.WriteByte('[')
	for i := 0; i < n; i++ {
		if i > 0 {
			b.WriteString(o.itemSep)
		}
		if err := o.encode(b, at(i)); err != nil {
			return err
		}
	}
	b.WriteByte(']')
	return nil
}

// Writes an object in insertion order, or key order when sort_keys is set
func (o pyJSON) encodeObject(b *strings.Builder, obj pyObject) error {
	if o.sortKeys {
		obj = append(pyObject{}, obj...)
		sort.SliceStable(obj, func(i, j int) bool { return obj[i].Key < obj[j].Key })
	}
	b.WriteByte('{')
	for i, kv := range obj {
		if i > 0 {
			b.WriteString(o.itemSep)
		}
		o.encodeString(b, kv.Key)
		b.WriteString(o.keySep)
		if err := o.encode(b, kv.Value); err != nil {
			return err
		}
	}
	b.WriteByte('}')
	return nil
}

// Writes a quoted string with json.dumps escapes, surrogates carried as WTF-8 passing through
func (o pyJSON) encodeString(b *strings.Builder, s string) {
	b.WriteByte('"')
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		if r == utf8.RuneError && size == 1 {
			if unit, ok := wtf8Surrogate(s[i:]); ok {
				if o.ensureASCII {
					fmt.Fprintf(b, `\u%04x`, unit)
				} else {
					b.WriteString(s[i : i+3])
				}
				i += 3
				continue
			}
		}
		i += size
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		case '\b':
			b.WriteString(`\b`)
		case '\f':
			b.WriteString(`\f`)
		default:
			switch {
			case r < 0x20:
				fmt.Fprintf(b, `\u%04x`, r)
			case o.ensureASCII && r > 0x7e:
				if r >= 0x10000 {
					hi, lo := utf16Pair(r)
					fmt.Fprintf(b, `\u%04x\u%04x`, hi, lo)
				} else {
					fmt.Fprintf(b, `\u%04x`, r)
				}
			default:
				b.WriteRune(r)
			}
		}
	}
	b.WriteByte('"')
}

// UTF-16 surrogate pair of a supplementary code point
func utf16Pair(r rune) (rune, rune) {
	r -= 0x10000
	return 0xd800 + (r>>10)&0x3ff, 0xdc00 + r&0x3ff
}

// Lone surrogate code unit encoded as WTF-8 at the start of s
func wtf8Surrogate(s string) (rune, bool) {
	if len(s) < 3 || s[0] != 0xed || s[1] < 0xa0 || s[1] > 0xbf || s[2] < 0x80 || s[2] > 0xbf {
		return 0, false
	}
	return rune(s[0]&0x0f)<<12 | rune(s[1]&0x3f)<<6 | rune(s[2]&0x3f), true
}

// Python float repr, shortest round trip digits with repr's fixed or exponent layout
func pyFloatRepr(f float64) string {
	if f == 0 {
		if math.Signbit(f) {
			return "-0.0"
		}
		return "0.0"
	}
	digits := strconv.FormatFloat(math.Abs(f), 'e', -1, 64)
	mant, expText, _ := strings.Cut(digits, "e")
	exp, _ := strconv.Atoi(expText)
	mant = strings.Replace(mant, ".", "", 1)
	sign := ""
	if f < 0 {
		sign = "-"
	}
	if exp < -4 || exp >= 16 {
		out := mant[:1]
		if len(mant) > 1 {
			out += "." + mant[1:]
		}
		esign := "+"
		if exp < 0 {
			esign = "-"
			exp = -exp
		}
		return fmt.Sprintf("%s%se%s%02d", sign, out, esign, exp)
	}
	if exp < 0 {
		return sign + "0." + strings.Repeat("0", -exp-1) + mant
	}
	if len(mant) <= exp+1 {
		return sign + mant + strings.Repeat("0", exp+1-len(mant)) + ".0"
	}
	return sign + mant[:exp+1] + "." + mant[exp+1:]
}

// Python repr of a str, choosing quotes and escapes the way CPython does
func pyRepr(s string) string {
	quote := byte('\'')
	if strings.ContainsRune(s, '\'') && !strings.ContainsRune(s, '"') {
		quote = '"'
	}
	var b strings.Builder
	b.WriteByte(quote)
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		if r == utf8.RuneError && size == 1 {
			if unit, ok := wtf8Surrogate(s[i:]); ok {
				fmt.Fprintf(&b, `\u%04x`, unit)
				i += 3
				continue
			}
			fmt.Fprintf(&b, `\x%02x`, s[i])
			i++
			continue
		}
		i += size
		switch {
		case r == rune(quote) || r == '\\':
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
		case r < 0x7f || unicode.IsPrint(r):
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

// Python str() of a scalar used inside f-strings
func pyFormat(v any) string {
	switch x := v.(type) {
	case nil:
		return "None"
	case bool:
		if x {
			return "True"
		}
		return "False"
	case string:
		return x
	case float64:
		if math.IsNaN(x) {
			return "nan"
		}
		if math.IsInf(x, 0) {
			if x > 0 {
				return "inf"
			}
			return "-inf"
		}
		return pyFloatRepr(x)
	}
	return fmt.Sprint(v)
}
