package portal

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
	"time"
)

type object struct {
	keys []string
	vals map[string]any
}

func newObject() *object { return &object{vals: map[string]any{}} }

func pairs(kv ...any) *object {
	o := newObject()
	for i := 0; i+1 < len(kv); i += 2 {
		o.set(kv[i].(string), kv[i+1])
	}
	return o
}

func (o *object) has(k string) bool {
	if o == nil {
		return false
	}
	_, ok := o.vals[k]
	return ok
}

func (o *object) get(k string) any {
	if o == nil {
		return nil
	}
	return o.vals[k]
}

func (o *object) str(k string) string {
	s, _ := o.get(k).(string)
	return s
}

func (o *object) obj(k string) *object {
	child, _ := o.get(k).(*object)
	return child
}

func (o *object) list(k string) []any {
	items, _ := o.get(k).([]any)
	return items
}

func (o *object) set(k string, v any) {
	if _, ok := o.vals[k]; !ok {
		o.keys = append(o.keys, k)
	}
	o.vals[k] = normalize(v)
}

func (o *object) del(k string) {
	if o == nil {
		return
	}
	if _, ok := o.vals[k]; !ok {
		return
	}
	delete(o.vals, k)
	for i, name := range o.keys {
		if name == k {
			o.keys = append(o.keys[:i:i], o.keys[i+1:]...)
			return
		}
	}
}

func (o *object) size() int {
	if o == nil {
		return 0
	}
	return len(o.keys)
}

func (o *object) clone() *object {
	if o == nil {
		return nil
	}
	out := newObject()
	for _, k := range o.keys {
		out.set(k, cloneValue(o.vals[k]))
	}
	return out
}

func (o *object) without(skip ...string) *object {
	out := newObject()
	for _, k := range o.keys {
		drop := false
		for _, s := range skip {
			if s == k {
				drop = true
			}
		}
		if !drop {
			out.set(k, cloneValue(o.vals[k]))
		}
	}
	return out
}

func (o *object) merge(src *object) {
	if src == nil {
		return
	}
	for _, k := range src.keys {
		o.set(k, cloneValue(src.vals[k]))
	}
}

func (o *object) plain() map[string]any {
	out := make(map[string]any, o.size())
	if o == nil {
		return out
	}
	for _, k := range o.keys {
		out[k] = plainValue(o.vals[k])
	}
	return out
}

func (o *object) raw() json.RawMessage {
	if o == nil {
		return json.RawMessage("null")
	}
	data, err := o.MarshalJSON()
	if err != nil {
		return json.RawMessage("null")
	}
	return data
}

func (o *object) MarshalJSON() ([]byte, error) {
	if o == nil {
		return []byte("null"), nil
	}
	var buf bytes.Buffer
	buf.WriteByte('{')
	for i, k := range o.keys {
		if i > 0 {
			buf.WriteByte(',')
		}
		if err := writeJSON(&buf, k); err != nil {
			return nil, err
		}
		buf.WriteByte(':')
		if err := writeJSON(&buf, o.vals[k]); err != nil {
			return nil, err
		}
	}
	buf.WriteByte('}')
	return buf.Bytes(), nil
}

func spacedJSON(v any) string {
	var buf bytes.Buffer
	switch t := v.(type) {
	case *object:
		if t == nil {
			return "null"
		}
		buf.WriteByte('{')
		for i, k := range t.keys {
			if i > 0 {
				buf.WriteString(", ")
			}
			_ = writeJSON(&buf, k)
			buf.WriteString(": ")
			buf.WriteString(spacedJSON(t.vals[k]))
		}
		buf.WriteByte('}')
	case []any:
		buf.WriteByte('[')
		for i, item := range t {
			if i > 0 {
				buf.WriteString(", ")
			}
			buf.WriteString(spacedJSON(item))
		}
		buf.WriteByte(']')
	default:
		_ = writeJSON(&buf, v)
	}
	return buf.String()
}

func sortedSpacedJSON(fields map[string]any) string {
	o := newObject()
	for _, k := range slices.Sorted(maps.Keys(fields)) {
		o.set(k, fields[k])
	}
	return spacedJSON(o)
}

func writeJSON(buf *bytes.Buffer, v any) error {
	switch t := v.(type) {
	case *object:
		data, err := t.MarshalJSON()
		if err != nil {
			return err
		}
		buf.Write(data)
		return nil
	case []any:
		buf.WriteByte('[')
		for i, item := range t {
			if i > 0 {
				buf.WriteByte(',')
			}
			if err := writeJSON(buf, item); err != nil {
				return err
			}
		}
		buf.WriteByte(']')
		return nil
	}
	enc := json.NewEncoder(buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return err
	}
	buf.Truncate(buf.Len() - 1)
	return nil
}

func normalize(v any) any {
	switch t := v.(type) {
	case nil, bool, string, json.Number, *object, []any:
		return v
	case int:
		return json.Number(strconv.Itoa(t))
	case int64:
		return json.Number(strconv.FormatInt(t, 10))
	case float64:
		return json.Number(strconv.FormatFloat(t, 'f', -1, 64))
	case []string:
		out := make([]any, len(t))
		for i, s := range t {
			out[i] = s
		}
		return out
	case json.RawMessage:
		parsed, err := parseValue(t)
		if err != nil {
			return nil
		}
		return parsed
	}
	return fmt.Sprint(v)
}

func cloneValue(v any) any {
	switch t := v.(type) {
	case *object:
		return t.clone()
	case []any:
		out := make([]any, len(t))
		for i, item := range t {
			out[i] = cloneValue(item)
		}
		return out
	}
	return v
}

func plainValue(v any) any {
	switch t := v.(type) {
	case *object:
		if t == nil {
			return nil
		}
		return t.plain()
	case []any:
		out := make([]any, len(t))
		for i, item := range t {
			out[i] = plainValue(item)
		}
		return out
	}
	return v
}

func parseValue(data []byte) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(bytes.TrimPrefix(data, []byte("\xef\xbb\xbf"))))
	dec.UseNumber()
	v, err := decodeValue(dec)
	if err != nil {
		return nil, err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("portal: trailing data after JSON value")
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
		o := newObject()
		for dec.More() {
			keyTok, err := dec.Token()
			if err != nil {
				return nil, err
			}
			key, _ := keyTok.(string)
			val, err := decodeValue(dec)
			if err != nil {
				return nil, err
			}
			o.set(key, val)
		}
		if _, err := dec.Token(); err != nil {
			return nil, err
		}
		return o, nil
	case '[':
		items := []any{}
		for dec.More() {
			val, err := decodeValue(dec)
			if err != nil {
				return nil, err
			}
			items = append(items, val)
		}
		if _, err := dec.Token(); err != nil {
			return nil, err
		}
		return items, nil
	}
	return nil, errors.New("portal: unexpected JSON delimiter")
}

func parseObject(data []byte) (*object, bool) {
	if len(bytes.TrimSpace(data)) == 0 {
		return nil, false
	}
	v, err := parseValue(data)
	if err != nil {
		return nil, false
	}
	o, ok := v.(*object)
	return o, ok && o != nil
}

func objectOrEmpty(data []byte) *object {
	if o, ok := parseObject(data); ok {
		return o
	}
	return newObject()
}

func nonEmpty(v any) bool {
	s, ok := v.(string)
	return ok && strings.TrimSpace(s) != ""
}

func present(v any) bool {
	switch t := v.(type) {
	case nil:
		return false
	case string:
		return t != ""
	}
	return true
}

func truthy(v any) bool {
	switch t := v.(type) {
	case nil:
		return false
	case bool:
		return t
	case string:
		return t != ""
	case json.Number:
		f, err := t.Float64()
		return err != nil || f != 0
	case []any:
		return len(t) > 0
	case *object:
		return t.size() > 0
	}
	return true
}

func numeric(v any) (float64, bool) {
	n, ok := v.(json.Number)
	if !ok {
		return 0, false
	}
	f, err := n.Float64()
	if err != nil || math.IsNaN(f) || math.IsInf(f, 0) {
		return 0, false
	}
	return f, true
}

func textual(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case json.Number:
		return t.String()
	}
	return string(valueJSON(v))
}

func valueJSON(v any) json.RawMessage {
	var buf bytes.Buffer
	if err := writeJSON(&buf, normalize(v)); err != nil {
		return json.RawMessage("null")
	}
	return buf.Bytes()
}

func sameValue(a, b any) bool {
	switch x := a.(type) {
	case *object:
		y, ok := b.(*object)
		if !ok || x.size() != y.size() {
			return false
		}
		for _, k := range x.keys {
			if !y.has(k) || !sameValue(x.vals[k], y.vals[k]) {
				return false
			}
		}
		return true
	case []any:
		y, ok := b.([]any)
		if !ok || len(x) != len(y) {
			return false
		}
		for i := range x {
			if !sameValue(x[i], y[i]) {
				return false
			}
		}
		return true
	case json.Number:
		fx, okx := numeric(x)
		fy, oky := numeric(b)
		return okx && oky && fx == fy
	}
	return a == b
}

func isoUTC(t time.Time) string {
	u := t.UTC()
	if u.Nanosecond()/1000 == 0 {
		return u.Format("2006-01-02T15:04:05") + "+00:00"
	}
	return u.Format("2006-01-02T15:04:05.000000") + "+00:00"
}

func isoFromEpoch(epoch float64) string {
	sec, frac := math.Modf(epoch)
	return isoUTC(time.Unix(int64(sec), int64(math.Round(frac*1e6))*1000))
}

func epochOf(t time.Time) float64 {
	return float64(t.UnixNano()) / 1e9
}
