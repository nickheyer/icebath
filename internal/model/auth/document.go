package auth

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"math"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"
)

type doc struct {
	keys []string
	vals map[string]any
}

func newDoc() *doc { return &doc{vals: map[string]any{}} }

func (d *doc) has(key string) bool {
	if d == nil {
		return false
	}
	_, ok := d.vals[key]
	return ok
}

func (d *doc) value(key string) any {
	if d == nil {
		return nil
	}
	return d.vals[key]
}

func (d *doc) set(key string, v any) {
	if _, ok := d.vals[key]; !ok {
		d.keys = append(d.keys, key)
	}
	d.vals[key] = v
}

func (d *doc) del(key string) bool {
	if d == nil {
		return false
	}
	if _, ok := d.vals[key]; !ok {
		return false
	}
	delete(d.vals, key)
	if i := slices.Index(d.keys, key); i >= 0 {
		d.keys = slices.Delete(d.keys, i, i+1)
	}
	return true
}

func (d *doc) str(key string) string {
	s, _ := d.value(key).(string)
	return s
}

func (d *doc) text(key string) string { return strings.TrimSpace(textOf(d.value(key))) }

func (d *doc) obj(key string) *doc {
	o, _ := d.value(key).(*doc)
	return o
}

func (d *doc) list(key string) []any {
	l, _ := d.value(key).([]any)
	return l
}

func (d *doc) section(key string) *doc {
	if o := d.obj(key); o != nil {
		return o
	}
	o := newDoc()
	d.set(key, o)
	return o
}

func (d *doc) size() int {
	if d == nil {
		return 0
	}
	return len(d.keys)
}

func (d *doc) clone() *doc {
	if d == nil {
		return nil
	}
	return &doc{keys: slices.Clone(d.keys), vals: maps.Clone(d.vals)}
}

func deepCopy(v any) any {
	switch t := v.(type) {
	case *doc:
		if t == nil {
			return nil
		}
		c := newDoc()
		for _, k := range t.keys {
			c.set(k, deepCopy(t.vals[k]))
		}
		return c
	case []any:
		c := make([]any, len(t))
		for i, e := range t {
			c[i] = deepCopy(e)
		}
		return c
	}
	return v
}

func textOf(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case json.Number:
		if f, err := t.Float64(); err == nil && f == 0 {
			return ""
		}
		return t.String()
	case bool:
		if t {
			return "True"
		}
	}
	return ""
}

func truthy(v any) bool {
	switch t := v.(type) {
	case nil:
		return false
	case string:
		return t != ""
	case bool:
		return t
	case json.Number:
		f, err := t.Float64()
		return err != nil || f != 0
	case []any:
		return len(t) > 0
	case *doc:
		return t.size() > 0
	}
	return true
}

func numberOf(v any) (float64, bool) {
	n, ok := v.(json.Number)
	if !ok {
		return 0, false
	}
	f, err := n.Float64()
	return f, err == nil
}

func intOf(v any) (int64, bool) {
	n, ok := v.(json.Number)
	if !ok {
		return 0, false
	}
	if i, err := n.Int64(); err == nil {
		return i, true
	}
	f, err := n.Float64()
	if err != nil || math.IsNaN(f) || math.IsInf(f, 0) {
		return 0, false
	}
	return int64(f), true
}

func intNum(n int64) json.Number { return json.Number(strconv.FormatInt(n, 10)) }

func floatNum(f float64) json.Number {
	s := strconv.FormatFloat(f, 'f', -1, 64)
	if !strings.ContainsAny(s, ".eEnN") {
		s += ".0"
	}
	return json.Number(s)
}

func valueEqual(a, b any) bool {
	switch x := a.(type) {
	case nil:
		return b == nil
	case string:
		y, ok := b.(string)
		return ok && x == y
	case bool:
		y, ok := b.(bool)
		return ok && x == y
	case json.Number:
		y, ok := b.(json.Number)
		if !ok {
			return false
		}
		if x == y {
			return true
		}
		fx, e1 := x.Float64()
		fy, e2 := y.Float64()
		return e1 == nil && e2 == nil && fx == fy
	case []any:
		y, ok := b.([]any)
		if !ok || len(x) != len(y) {
			return false
		}
		for i := range x {
			if !valueEqual(x[i], y[i]) {
				return false
			}
		}
		return true
	case *doc:
		y, ok := b.(*doc)
		if !ok || x.size() != y.size() {
			return false
		}
		for _, k := range x.keys {
			yv, ok := y.vals[k]
			if !ok || !valueEqual(x.vals[k], yv) {
				return false
			}
		}
		return true
	}
	return false
}

func (d *doc) MarshalJSON() ([]byte, error) {
	var b bytes.Buffer
	if err := encodeValue(&b, d); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

func encodeValue(b *bytes.Buffer, v any) error {
	switch t := v.(type) {
	case nil:
		b.WriteString("null")
	case *doc:
		if t == nil {
			b.WriteString("null")
			return nil
		}
		b.WriteByte('{')
		for i, k := range t.keys {
			if i > 0 {
				b.WriteByte(',')
			}
			if err := encodeString(b, k); err != nil {
				return err
			}
			b.WriteByte(':')
			if err := encodeValue(b, t.vals[k]); err != nil {
				return err
			}
		}
		b.WriteByte('}')
	case []any:
		b.WriteByte('[')
		for i, e := range t {
			if i > 0 {
				b.WriteByte(',')
			}
			if err := encodeValue(b, e); err != nil {
				return err
			}
		}
		b.WriteByte(']')
	case string:
		return encodeString(b, t)
	case bool:
		b.WriteString(strconv.FormatBool(t))
	case json.Number:
		if t == "" {
			b.WriteByte('0')
			return nil
		}
		b.WriteString(string(t))
	default:
		return fmt.Errorf("auth: value of type %T has no JSON form", v)
	}
	return nil
}

func encodeString(b *bytes.Buffer, s string) error {
	raw, err := json.Marshal(s)
	if err != nil {
		return err
	}
	b.Write(raw)
	return nil
}

var errTrailingJSON = errors.New("auth: data after the JSON value")

var errNotUTF8 = errors.New("auth: text is not UTF-8")

func parseJSON(data []byte) (any, error) {
	data = bytes.TrimPrefix(data, []byte{0xEF, 0xBB, 0xBF})
	if !utf8.Valid(data) {
		return nil, errNotUTF8
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	v, err := decodeValue(dec)
	if err != nil {
		return nil, err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, errTrailingJSON
	}
	return v, nil
}

func decodeValue(dec *json.Decoder) (any, error) {
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	delim, ok := tok.(json.Delim)
	if !ok {
		return tok, nil
	}
	switch delim {
	case '{':
		d := newDoc()
		for dec.More() {
			kt, err := dec.Token()
			if err != nil {
				return nil, err
			}
			key, ok := kt.(string)
			if !ok {
				return nil, fmt.Errorf("auth: object member name is %T", kt)
			}
			v, err := decodeValue(dec)
			if err != nil {
				return nil, err
			}
			d.set(key, v)
		}
		if _, err := dec.Token(); err != nil {
			return nil, err
		}
		return d, nil
	case '[':
		list := []any{}
		for dec.More() {
			v, err := decodeValue(dec)
			if err != nil {
				return nil, err
			}
			list = append(list, v)
		}
		if _, err := dec.Token(); err != nil {
			return nil, err
		}
		return list, nil
	}
	return nil, fmt.Errorf("auth: unexpected JSON delimiter %q", rune(delim))
}

func parseObject(data []byte) (*doc, error) {
	v, err := parseJSON(data)
	if err != nil {
		return nil, err
	}
	d, ok := v.(*doc)
	if !ok {
		return nil, errors.New("auth: JSON value is not an object")
	}
	return d, nil
}

func rawOf(d *doc) json.RawMessage {
	if d == nil {
		return nil
	}
	raw, err := d.MarshalJSON()
	if err != nil {
		return nil
	}
	return raw
}
