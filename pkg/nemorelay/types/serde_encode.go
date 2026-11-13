package types

import (
	"encoding"
	"encoding/json"
	"fmt"
	"math"
	"reflect"
	"sort"
	"strconv"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
)

// orderedObject is a JSON object whose entries serialize in insertion order
type orderedObject []orderedEntry

type orderedEntry struct {
	key   string
	value any
}

// Appends an entry and returns the extended object
func (o orderedObject) with(key string, value any) orderedObject {
	return append(o, orderedEntry{key: key, value: value})
}

// EncodeJSON serializes a value byte for byte as serde_json to_vec does
func EncodeJSON(v any) ([]byte, error) {
	e := &encoder{}
	if err := e.encode(v); err != nil {
		return nil, err
	}
	return e.buf, nil
}

// EncodeJSONString serializes a value as serde_json to_string does
func EncodeJSONString(v any) (string, error) {
	b, err := EncodeJSON(v)
	return string(b), err
}

// EncodeJSONPretty serializes a value as serde_json to_vec_pretty does
func EncodeJSONPretty(v any) ([]byte, error) {
	e := &encoder{pretty: true}
	if err := e.encode(v); err != nil {
		return nil, err
	}
	return e.buf, nil
}

// ToJSONValue converts a value into decoded JSON as serde_json to_value does
func ToJSONValue(v any) (any, error) {
	b, err := EncodeJSON(v)
	if err != nil {
		return nil, err
	}
	return DecodeJSON(b)
}

// MustJSONValue converts a value into decoded JSON for types whose serialization cannot fail
func MustJSONValue(v any) any {
	out, err := ToJSONValue(v)
	if err != nil {
		panic(fmt.Sprintf("serializing to JSON should not fail: %v", err))
	}
	return out
}

type encoder struct {
	buf    []byte
	pretty bool
	indent int
}

type containerState struct {
	first bool
}

func (e *encoder) newline() {
	e.buf = append(e.buf, '\n')
	for i := 0; i < e.indent; i++ {
		e.buf = append(e.buf, ' ', ' ')
	}
}

func (e *encoder) beginObject() *containerState {
	e.buf = append(e.buf, '{')
	e.indent++
	return &containerState{first: true}
}

func (e *encoder) key(s *containerState, k string) {
	if !s.first {
		e.buf = append(e.buf, ',')
	}
	if e.pretty {
		e.newline()
	}
	s.first = false
	e.writeString(k)
	e.buf = append(e.buf, ':')
	if e.pretty {
		e.buf = append(e.buf, ' ')
	}
}

func (e *encoder) endObject(s *containerState) {
	e.indent--
	if e.pretty && !s.first {
		e.newline()
	}
	e.buf = append(e.buf, '}')
}

func (e *encoder) beginArray() *containerState {
	e.buf = append(e.buf, '[')
	e.indent++
	return &containerState{first: true}
}

func (e *encoder) element(s *containerState) {
	if !s.first {
		e.buf = append(e.buf, ',')
	}
	if e.pretty {
		e.newline()
	}
	s.first = false
}

func (e *encoder) endArray(s *containerState) {
	e.indent--
	if e.pretty && !s.first {
		e.newline()
	}
	e.buf = append(e.buf, ']')
}

const hexDigits = "0123456789abcdef"

// Writes a JSON string with serde_json escaping rules
func (e *encoder) writeString(s string) {
	e.buf = append(e.buf, '"')
	for i := 0; i < len(s); {
		c := s[i]
		if c < utf8.RuneSelf {
			switch c {
			case '"':
				e.buf = append(e.buf, '\\', '"')
			case '\\':
				e.buf = append(e.buf, '\\', '\\')
			case '\b':
				e.buf = append(e.buf, '\\', 'b')
			case '\f':
				e.buf = append(e.buf, '\\', 'f')
			case '\n':
				e.buf = append(e.buf, '\\', 'n')
			case '\r':
				e.buf = append(e.buf, '\\', 'r')
			case '\t':
				e.buf = append(e.buf, '\\', 't')
			default:
				if c < 0x20 {
					e.buf = append(e.buf, '\\', 'u', '0', '0', hexDigits[c>>4], hexDigits[c&0xf])
				} else {
					e.buf = append(e.buf, c)
				}
			}
			i++
			continue
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		if r == utf8.RuneError && size == 1 {
			e.buf = utf8.AppendRune(e.buf, utf8.RuneError)
		} else {
			e.buf = append(e.buf, s[i:i+size]...)
		}
		i += size
	}
	e.buf = append(e.buf, '"')
}

func (e *encoder) writeFloat(f float64) {
	e.buf = append(e.buf, FormatF64(f)...)
}

func (e *encoder) writeFloat32(f float32) {
	if math.IsNaN(float64(f)) || math.IsInf(float64(f), 0) {
		e.buf = append(e.buf, "null"...)
		return
	}
	e.buf = append(e.buf, formatShortestFloat32(f)...)
}

// Formats a float32 the way serde_json does through zmij
func formatShortestFloat32(f float32) string {
	s := strconv.FormatFloat(float64(f), 'e', -1, 32)
	g, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return FormatF64(float64(f))
	}
	return formatShortestFloat(g, -6, 12, true, true)
}

var (
	timeType          = reflect.TypeFor[time.Time]()
	uuidType          = reflect.TypeFor[uuid.UUID]()
	jsonMarshalerType = reflect.TypeFor[json.Marshaler]()
	textMarshalerType = reflect.TypeFor[encoding.TextMarshaler]()
	serdeSerType      = reflect.TypeFor[serdeSerializer]()
	rawMessageType    = reflect.TypeFor[json.RawMessage]()
)

// Encodes any supported value
func (e *encoder) encode(v any) error {
	switch x := v.(type) {
	case nil:
		e.buf = append(e.buf, "null"...)
		return nil
	case taggedStruct:
		s := e.beginObject()
		e.key(s, x.tagKey)
		e.writeString(x.tagValue)
		if err := e.encodeStructFields(s, reflect.ValueOf(x.value)); err != nil {
			return err
		}
		e.endObject(s)
		return nil
	case orderedObject:
		s := e.beginObject()
		for _, entry := range x {
			e.key(s, entry.key)
			if err := e.encode(entry.value); err != nil {
				return err
			}
		}
		e.endObject(s)
		return nil
	case map[string]any:
		s := e.beginObject()
		for _, k := range sortedMapKeys(x) {
			e.key(s, k)
			if err := e.encode(x[k]); err != nil {
				return err
			}
		}
		e.endObject(s)
		return nil
	case []any:
		s := e.beginArray()
		for _, item := range x {
			e.element(s)
			if err := e.encode(item); err != nil {
				return err
			}
		}
		e.endArray(s)
		return nil
	case string:
		e.writeString(x)
		return nil
	case bool:
		e.buf = strconv.AppendBool(e.buf, x)
		return nil
	case float64:
		e.writeFloat(x)
		return nil
	case json.Number:
		n, ok := parseNumberText(string(x))
		if !ok {
			return fmt.Errorf("invalid number %q", string(x))
		}
		e.writeNumber(n)
		return nil
	case uuid.UUID:
		e.writeString(x.String())
		return nil
	case time.Time:
		e.writeString(formatRFC3339(x, true, false))
		return nil
	}
	return e.encodeReflect(reflect.ValueOf(v))
}

func (e *encoder) writeNumber(n jsonNumber) {
	switch n.kind {
	case numberPosInt:
		e.buf = strconv.AppendUint(e.buf, n.u, 10)
	case numberNegInt:
		e.buf = strconv.AppendInt(e.buf, n.i, 10)
	default:
		e.writeFloat(n.f)
	}
}

// Encodes a value through reflection following serde field rules
func (e *encoder) encodeReflect(rv reflect.Value) error {
	if !rv.IsValid() {
		e.buf = append(e.buf, "null"...)
		return nil
	}
	t := rv.Type()
	if t.Implements(serdeSerType) && !(rv.Kind() == reflect.Pointer && rv.IsNil()) && !(rv.Kind() == reflect.Interface && rv.IsNil()) {
		inter, err := rv.Interface().(serdeSerializer).serdeSerialize()
		if err != nil {
			return err
		}
		return e.encode(inter)
	}
	if rv.Kind() != reflect.Pointer && reflect.PointerTo(t).Implements(serdeSerType) {
		ptr := reflect.New(t)
		ptr.Elem().Set(rv)
		inter, err := ptr.Interface().(serdeSerializer).serdeSerialize()
		if err != nil {
			return err
		}
		return e.encode(inter)
	}
	switch t {
	case timeType:
		e.writeString(formatRFC3339(rv.Interface().(time.Time), true, false))
		return nil
	case uuidType:
		e.writeString(rv.Interface().(uuid.UUID).String())
		return nil
	case rawMessageType:
		return e.encodeRaw(rv.Bytes())
	}
	switch rv.Kind() {
	case reflect.Pointer, reflect.Interface:
		if rv.IsNil() {
			e.buf = append(e.buf, "null"...)
			return nil
		}
		return e.encodeReflect(rv.Elem())
	}
	ownPackage := t.PkgPath() == typesPkgPath
	if !ownPackage && t.Implements(jsonMarshalerType) {
		b, err := rv.Interface().(json.Marshaler).MarshalJSON()
		if err != nil {
			return err
		}
		return e.encodeRaw(b)
	}
	if !ownPackage && t.Implements(textMarshalerType) {
		b, err := rv.Interface().(encoding.TextMarshaler).MarshalText()
		if err != nil {
			return err
		}
		e.writeString(string(b))
		return nil
	}
	switch rv.Kind() {
	case reflect.Bool:
		e.buf = strconv.AppendBool(e.buf, rv.Bool())
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		e.buf = strconv.AppendInt(e.buf, rv.Int(), 10)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		e.buf = strconv.AppendUint(e.buf, rv.Uint(), 10)
	case reflect.Float32:
		e.writeFloat32(float32(rv.Float()))
	case reflect.Float64:
		e.writeFloat(rv.Float())
	case reflect.String:
		e.writeString(rv.String())
	case reflect.Slice:
		return e.encodeSeq(rv)
	case reflect.Array:
		return e.encodeSeq(rv)
	case reflect.Map:
		return e.encodeMap(rv)
	case reflect.Struct:
		s := e.beginObject()
		if err := e.encodeStructFields(s, rv); err != nil {
			return err
		}
		e.endObject(s)
	default:
		return fmt.Errorf("unsupported type %s", t)
	}
	return nil
}

func (e *encoder) encodeSeq(rv reflect.Value) error {
	s := e.beginArray()
	for i := 0; i < rv.Len(); i++ {
		e.element(s)
		if err := e.encodeReflect(rv.Index(i)); err != nil {
			return err
		}
	}
	e.endArray(s)
	return nil
}

type mapKey struct {
	text  string
	num   int64
	unum  uint64
	value reflect.Value
}

// Encodes a map with keys sorted the way a BTreeMap orders them
func (e *encoder) encodeMap(rv reflect.Value) error {
	keys, err := sortedReflectKeys(rv)
	if err != nil {
		return err
	}
	s := e.beginObject()
	for _, k := range keys {
		e.key(s, k.text)
		if err := e.encodeReflect(rv.MapIndex(k.value)); err != nil {
			return err
		}
	}
	e.endObject(s)
	return nil
}

func sortedReflectKeys(rv reflect.Value) ([]mapKey, error) {
	kt := rv.Type().Key()
	keys := make([]mapKey, 0, rv.Len())
	for _, k := range rv.MapKeys() {
		mk := mapKey{value: k}
		switch kt.Kind() {
		case reflect.String:
			mk.text = k.String()
		case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
			mk.num = k.Int()
			mk.text = strconv.FormatInt(mk.num, 10)
		case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
			mk.unum = k.Uint()
			mk.text = strconv.FormatUint(mk.unum, 10)
		default:
			if tm, ok := k.Interface().(encoding.TextMarshaler); ok {
				b, err := tm.MarshalText()
				if err != nil {
					return nil, err
				}
				mk.text = string(b)
			} else {
				return nil, fmt.Errorf("key must be a string")
			}
		}
		keys = append(keys, mk)
	}
	sort.Slice(keys, func(i, j int) bool {
		switch kt.Kind() {
		case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
			return keys[i].num < keys[j].num
		case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
			return keys[i].unum < keys[j].unum
		}
		return keys[i].text < keys[j].text
	})
	return keys, nil
}

// Writes struct fields in declaration order with flattened fields inlined
func (e *encoder) encodeStructFields(s *containerState, rv reflect.Value) error {
	m := metaOf(rv.Type())
	for i := range m.fields {
		f := &m.fields[i]
		fv := rv.FieldByIndex(f.index)
		if f.skipOnSerialize(fv, rv) {
			continue
		}
		if f.flatten {
			if err := e.encodeFlattened(s, fv); err != nil {
				return err
			}
			continue
		}
		e.key(s, f.name)
		if f.option && f.isNone(fv, rv) {
			e.buf = append(e.buf, "null"...)
			continue
		}
		if f.timestamp {
			leap := f.leapIndex != nil && rv.FieldByIndex(f.leapIndex).Bool()
			e.writeString(formatRFC3339(fv.Interface().(time.Time), false, leap))
			continue
		}
		if err := e.encodeReflect(fv); err != nil {
			return err
		}
	}
	return nil
}

// Writes the entries of a flattened map or struct into the enclosing object
func (e *encoder) encodeFlattened(s *containerState, fv reflect.Value) error {
	for fv.Kind() == reflect.Pointer || fv.Kind() == reflect.Interface {
		if fv.IsNil() {
			return nil
		}
		fv = fv.Elem()
	}
	switch fv.Kind() {
	case reflect.Map:
		keys, err := sortedReflectKeys(fv)
		if err != nil {
			return err
		}
		for _, k := range keys {
			e.key(s, k.text)
			if err := e.encodeReflect(fv.MapIndex(k.value)); err != nil {
				return err
			}
		}
		return nil
	case reflect.Struct:
		return e.encodeStructFields(s, fv)
	}
	return fmt.Errorf("can only flatten structs and maps (got %s)", fv.Type())
}

// Re-encodes raw JSON bytes canonically, keeping object entry order
func (e *encoder) encodeRaw(b []byte) error {
	v, err := decodeJSONOrdered(b)
	if err != nil {
		return err
	}
	return e.encode(v)
}

var typesPkgPath = reflect.TypeFor[orderedEntry]().PkgPath()

// taggedStruct serializes an internally tagged struct variant with its tag entry first
type taggedStruct struct {
	tagKey   string
	tagValue string
	value    any
}
