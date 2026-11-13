package types

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"reflect"
	"sort"
	"strings"

	"github.com/google/uuid"
)

// SerdeDecode decodes a JSON value into out as serde_json from_value does, with serde error texts
func SerdeDecode(v any, out any) error {
	rv := reflect.ValueOf(out)
	if rv.Kind() != reflect.Pointer || rv.IsNil() {
		return errors.New("SerdeDecode needs a non-nil pointer")
	}
	return decodeInto(normalizeJSON(v), rv.Elem())
}

// SerdeUnmarshal parses bytes and decodes them as serde_json from_slice does
func SerdeUnmarshal(b []byte, out any) error {
	v, err := DecodeJSON(b)
	if err != nil {
		return err
	}
	return SerdeDecode(v, out)
}

// DescribeUnexpected renders a JSON value as serde_json names it in invalid type errors
func DescribeUnexpected(v any) string {
	switch x := normalizeJSON(v).(type) {
	case nil:
		return "null"
	case bool:
		return fmt.Sprintf("boolean `%t`", x)
	case string:
		return "string " + RustStrDebug(x)
	case []any:
		return "sequence"
	case map[string]any:
		return "map"
	}
	if n, ok := classifyNumber(v); ok {
		if n.kind == numberFloat {
			return "floating point `" + FormatF64(n.f) + "`"
		}
		return "integer `" + n.String() + "`"
	}
	return "map"
}

// InvalidTypeError is the serde invalid type error for a value
func InvalidTypeError(v any, expected string) error {
	return fmt.Errorf("invalid type: %s, expected %s", DescribeUnexpected(v), expected)
}

// InvalidValueError is the serde invalid value error for a value
func InvalidValueError(v any, expected string) error {
	return fmt.Errorf("invalid value: %s, expected %s", DescribeUnexpected(v), expected)
}

// InvalidLengthError is the serde invalid length error
func InvalidLengthError(n int, expected string) error {
	return fmt.Errorf("invalid length %d, expected %s", n, expected)
}

// MissingFieldError is the serde missing field error
func MissingFieldError(field string) error {
	return fmt.Errorf("missing field `%s`", field)
}

// DuplicateFieldError is the serde duplicate field error
func DuplicateFieldError(field string) error {
	return fmt.Errorf("duplicate field `%s`", field)
}

// UnknownFieldError is the serde unknown field error listing the accepted names
func UnknownFieldError(field string, expected []string) error {
	if len(expected) == 0 {
		return fmt.Errorf("unknown field `%s`, there are no fields", field)
	}
	return fmt.Errorf("unknown field `%s`, expected %s", field, oneOf(expected))
}

// UnknownVariantError is the serde unknown variant error listing the accepted names
func UnknownVariantError(variant string, expected []string) error {
	if len(expected) == 0 {
		return fmt.Errorf("unknown variant `%s`, there are no variants", variant)
	}
	return fmt.Errorf("unknown variant `%s`, expected %s", variant, oneOf(expected))
}

func oneOf(names []string) string {
	switch len(names) {
	case 1:
		return "`" + names[0] + "`"
	case 2:
		return "`" + names[0] + "` or `" + names[1] + "`"
	}
	parts := make([]string, len(names))
	for i, n := range names {
		parts[i] = "`" + n + "`"
	}
	return "one of " + strings.Join(parts, ", ")
}

// UntaggedNoMatchError is the serde error when no untagged variant accepts the data
func UntaggedNoMatchError(enum string) error {
	return fmt.Errorf("data did not match any variant of untagged enum %s", enum)
}

var (
	serdeDeType       = reflect.TypeFor[serdeDeserializer]()
	interfaceDecoders = map[reflect.Type]func(v any) (any, error){}
)

// Registers the decoder used for fields of a sealed interface type
func registerInterfaceDecoder[T any](decode func(v any) (T, error)) {
	interfaceDecoders[reflect.TypeFor[T]()] = func(v any) (any, error) {
		out, err := decode(v)
		if err != nil {
			return nil, err
		}
		return out, nil
	}
}

// Decodes a JSON value into a reflected Go value with serde semantics
func decodeInto(v any, rv reflect.Value) error {
	t := rv.Type()
	if rv.CanAddr() && reflect.PointerTo(t).Implements(serdeDeType) {
		return rv.Addr().Interface().(serdeDeserializer).serdeDeserialize(v)
	}
	if t.Kind() == reflect.Interface {
		if dec, ok := interfaceDecoders[t]; ok {
			out, err := dec(v)
			if err != nil {
				return err
			}
			if out == nil {
				rv.SetZero()
			} else {
				rv.Set(reflect.ValueOf(out))
			}
			return nil
		}
		if t.NumMethod() == 0 {
			if v == nil {
				rv.SetZero()
			} else {
				rv.Set(reflect.ValueOf(v))
			}
			return nil
		}
		return fmt.Errorf("cannot decode into %s", t)
	}
	switch t {
	case uuidType:
		u, err := decodeUUID(v)
		if err != nil {
			return err
		}
		rv.Set(reflect.ValueOf(u))
		return nil
	case rawMessageType:
		b, err := EncodeJSON(v)
		if err != nil {
			return err
		}
		rv.SetBytes(b)
		return nil
	}
	switch t.Kind() {
	case reflect.Pointer:
		if v == nil {
			rv.SetZero()
			return nil
		}
		nv := reflect.New(t.Elem())
		if err := decodeInto(v, nv.Elem()); err != nil {
			return err
		}
		rv.Set(nv)
		return nil
	case reflect.String:
		s, ok := v.(string)
		if !ok {
			return InvalidTypeError(v, "a string")
		}
		rv.SetString(s)
		return nil
	case reflect.Bool:
		b, ok := v.(bool)
		if !ok {
			return InvalidTypeError(v, "a boolean")
		}
		rv.SetBool(b)
		return nil
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return decodeSigned(v, rv)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return decodeUnsigned(v, rv)
	case reflect.Float32, reflect.Float64:
		n, ok := classifyNumber(v)
		if !ok {
			return InvalidTypeError(v, floatName(t))
		}
		rv.SetFloat(n.asF64())
		return nil
	case reflect.Slice:
		arr, ok := v.([]any)
		if !ok {
			return InvalidTypeError(v, "a sequence")
		}
		out := reflect.MakeSlice(t, len(arr), len(arr))
		for i, item := range arr {
			if err := decodeInto(item, out.Index(i)); err != nil {
				return err
			}
		}
		rv.Set(out)
		return nil
	case reflect.Map:
		obj, ok := v.(map[string]any)
		if !ok {
			return InvalidTypeError(v, "a map")
		}
		out := reflect.MakeMapWithSize(t, len(obj))
		for _, k := range sortedMapKeys(obj) {
			ev := reflect.New(t.Elem()).Elem()
			if err := decodeInto(obj[k], ev); err != nil {
				return err
			}
			kv := reflect.New(t.Key()).Elem()
			if t.Key().Kind() != reflect.String {
				return fmt.Errorf("cannot decode map keys of %s", t.Key())
			}
			kv.SetString(k)
			out.SetMapIndex(kv, ev)
		}
		rv.Set(out)
		return nil
	case reflect.Struct:
		return decodeStruct(v, rv)
	}
	return fmt.Errorf("cannot decode into %s", t)
}

func floatName(t reflect.Type) string {
	if t.Kind() == reflect.Float32 {
		return "f32"
	}
	return "f64"
}

func intName(t reflect.Type) string {
	switch t.Kind() {
	case reflect.Int8:
		return "i8"
	case reflect.Int16:
		return "i16"
	case reflect.Int32:
		return "i32"
	case reflect.Uint8:
		return "u8"
	case reflect.Uint16:
		return "u16"
	case reflect.Uint32:
		return "u32"
	case reflect.Uint, reflect.Uint64:
		return "u64"
	}
	return "i64"
}

func decodeSigned(v any, rv reflect.Value) error {
	name := intName(rv.Type())
	n, ok := classifyNumber(v)
	if !ok || n.kind == numberFloat {
		return InvalidTypeError(v, name)
	}
	bits := rv.Type().Bits()
	minV, maxV := int64(-1)<<(bits-1), int64(1)<<(bits-1)-1
	if bits == 64 {
		minV, maxV = math.MinInt64, math.MaxInt64
	}
	if n.kind == numberPosInt {
		if n.u > uint64(maxV) {
			return InvalidValueError(v, name)
		}
		rv.SetInt(int64(n.u))
		return nil
	}
	if n.i < minV {
		return InvalidValueError(v, name)
	}
	rv.SetInt(n.i)
	return nil
}

func decodeUnsigned(v any, rv reflect.Value) error {
	name := intName(rv.Type())
	n, ok := classifyNumber(v)
	if !ok || n.kind == numberFloat {
		return InvalidTypeError(v, name)
	}
	if n.kind == numberNegInt {
		return InvalidValueError(v, name)
	}
	bits := rv.Type().Bits()
	if bits < 64 && n.u > uint64(1)<<bits-1 {
		return InvalidValueError(v, name)
	}
	rv.SetUint(n.u)
	return nil
}

// Decodes a hyphenated, simple, braced or urn UUID string with uuid crate error texts
func decodeUUID(v any) (uuid.UUID, error) {
	s, ok := v.(string)
	if !ok {
		return uuid.UUID{}, InvalidTypeError(v, "a UUID string")
	}
	u, err := ParseUUID(s)
	if err != nil {
		return uuid.UUID{}, fmt.Errorf("UUID parsing failed: %s", err.Error())
	}
	return u, nil
}

type flatEntry struct {
	key   string
	value any
	taken bool
}

// Decodes a struct from a JSON object or array following serde derive rules
func decodeStruct(v any, rv reflect.Value) error {
	rv.SetZero()
	m := metaOf(rv.Type())
	expect := "struct " + m.rustName
	switch x := v.(type) {
	case map[string]any:
		entries := make([]*flatEntry, 0, len(x))
		for _, k := range sortedMapKeys(x) {
			entries = append(entries, &flatEntry{key: k, value: x[k]})
		}
		return decodeStructEntries(entries, rv, m, false)
	case []any:
		if m.hasFlatten {
			return InvalidTypeError(v, expect)
		}
		return decodeStructSeq(x, rv, m, expect)
	}
	return InvalidTypeError(v, expect)
}

// Decodes struct fields from ordered entries, taking only known keys when flattened
func decodeStructEntries(entries []*flatEntry, rv reflect.Value, m *structMeta, flattened bool) error {
	seen := make([]bool, len(m.fields))
	var collect []*flatEntry
	for _, entry := range entries {
		if entry.taken {
			continue
		}
		idx := -1
		for i := range m.fields {
			f := &m.fields[i]
			if !f.flatten && !f.serdeSkip && f.name == entry.key {
				idx = i
				break
			}
		}
		if idx < 0 {
			if flattened {
				continue
			}
			if m.hasFlatten {
				collect = append(collect, entry)
				continue
			}
			if m.denyUnknown {
				return UnknownFieldError(entry.key, m.wireNames)
			}
			continue
		}
		if flattened {
			entry.taken = true
		}
		if seen[idx] {
			return DuplicateFieldError(entry.key)
		}
		seen[idx] = true
		f := &m.fields[idx]
		if err := decodeField(entry.value, rv, f); err != nil {
			return err
		}
	}
	for i := range m.fields {
		f := &m.fields[i]
		if f.flatten || f.serdeSkip || seen[i] {
			continue
		}
		if err := missingField(rv, f); err != nil {
			return err
		}
	}
	for i := range m.fields {
		f := &m.fields[i]
		if !f.flatten {
			continue
		}
		fv := rv.FieldByIndex(f.index)
		if err := decodeFlattened(collect, fv); err != nil {
			return err
		}
	}
	if m.denyUnknown && m.hasFlatten {
		for _, entry := range collect {
			if !entry.taken {
				return UnknownFieldError(entry.key, m.wireNames)
			}
		}
	}
	return nil
}

// Decodes one present field value, setting its present flag when the field tracks one
func decodeField(value any, rv reflect.Value, f *fieldMeta) error {
	fv := rv.FieldByIndex(f.index)
	if f.timestamp {
		p, err := decodeEventTimestamp(value)
		if err != nil {
			return err
		}
		fv.Set(reflect.ValueOf(p.Time))
		if f.leapIndex != nil {
			rv.FieldByIndex(f.leapIndex).SetBool(p.Leap)
		}
		return nil
	}
	if f.option && value == nil {
		fv.SetZero()
		return nil
	}
	return decodeInto(value, fv)
}

// Applies serde missing field rules for one field
func missingField(rv reflect.Value, f *fieldMeta) error {
	fv := rv.FieldByIndex(f.index)
	if f.hasDefault {
		if f.defaultText != nil {
			fv.SetString(*f.defaultText)
		} else {
			fv.SetZero()
		}
		return nil
	}
	if f.option {
		fv.SetZero()
		return nil
	}
	return MissingFieldError(f.name)
}

// Feeds the leftover entries of a flattening struct into a flattened map or struct
func decodeFlattened(collect []*flatEntry, fv reflect.Value) error {
	switch fv.Kind() {
	case reflect.Map:
		t := fv.Type()
		out := reflect.MakeMap(t)
		for _, entry := range collect {
			if entry.taken {
				continue
			}
			ev := reflect.New(t.Elem()).Elem()
			if err := decodeInto(entry.value, ev); err != nil {
				return err
			}
			out.SetMapIndex(reflect.ValueOf(entry.key).Convert(t.Key()), ev)
		}
		fv.Set(out)
		return nil
	case reflect.Struct:
		return decodeStructEntries(collect, fv, metaOf(fv.Type()), true)
	}
	return fmt.Errorf("cannot flatten into %s", fv.Type())
}

// Decodes a struct from a JSON array positionally as serde visit_seq does
func decodeStructSeq(arr []any, rv reflect.Value, m *structMeta, expect string) error {
	count := 0
	for i := range m.fields {
		if !m.fields[i].serdeSkip {
			count++
		}
	}
	pos := 0
	for i := range m.fields {
		f := &m.fields[i]
		if f.serdeSkip {
			continue
		}
		if pos >= len(arr) {
			if f.hasDefault {
				if err := missingField(rv, f); err != nil {
					return err
				}
				pos++
				continue
			}
			return InvalidLengthError(pos, fmt.Sprintf("%s with %d elements", expect, count))
		}
		if err := decodeField(arr[pos], rv, f); err != nil {
			return err
		}
		pos++
	}
	if len(arr) > count {
		return InvalidLengthError(len(arr), "fewer elements in array")
	}
	return nil
}

// Reads the entries of an internally tagged enum value, returning the tag and remaining content
func splitInternalTag(v any, tag, expect string) (any, any, error) {
	switch x := normalizeJSON(v).(type) {
	case map[string]any:
		tagValue, ok := x[tag]
		if !ok {
			return nil, nil, MissingFieldError(tag)
		}
		rest := make(map[string]any, len(x))
		for k, e := range x {
			if k != tag {
				rest[k] = e
			}
		}
		return tagValue, rest, nil
	case []any:
		if len(x) == 0 {
			return nil, nil, MissingFieldError(tag)
		}
		return x[0], append([]any{}, x[1:]...), nil
	}
	return nil, nil, InvalidTypeError(v, expect)
}

// Resolves a variant identifier from a tag value against the variant names
func variantIndex(tag any, names []string) (int, error) {
	switch x := tag.(type) {
	case string:
		for i, n := range names {
			if n == x {
				return i, nil
			}
		}
		return -1, UnknownVariantError(x, names)
	}
	if n, ok := classifyNumber(tag); ok && n.kind == numberPosInt {
		if n.u < uint64(len(names)) {
			return int(n.u), nil
		}
		return -1, InvalidValueError(tag, fmt.Sprintf("variant index 0 <= i < %d", len(names)))
	}
	return -1, InvalidTypeError(tag, "variant identifier")
}

// Decodes an externally tagged enum value into its variant name and optional content
func splitExternalTag(v any) (string, any, bool, error) {
	switch x := normalizeJSON(v).(type) {
	case string:
		return x, nil, false, nil
	case map[string]any:
		if len(x) != 1 {
			return "", nil, false, InvalidValueError(v, "map with a single key")
		}
		for k, e := range x {
			return k, e, true, nil
		}
	}
	return "", nil, false, InvalidTypeError(v, "string or map")
}

// Checks the content of a unit variant given in map form
func unitVariantContent(content any, hasContent bool) error {
	if hasContent && content != nil {
		return InvalidTypeError(content, "unit")
	}
	return nil
}

// Decodes a unit-only enum written as a string or single key map
func decodeUnitEnum(v any, names []string) (int, error) {
	name, content, has, err := splitExternalTag(v)
	if err != nil {
		return -1, err
	}
	idx, err := variantIndex(name, names)
	if err != nil {
		return -1, err
	}
	if err := unitVariantContent(content, has); err != nil {
		return -1, err
	}
	return idx, nil
}

// Decodes a struct variant body into a variant struct
func decodeVariantStruct(content any, out any, expect string) error {
	rv := reflect.ValueOf(out).Elem()
	rv.SetZero()
	m := metaOf(rv.Type())
	switch x := content.(type) {
	case map[string]any:
		entries := make([]*flatEntry, 0, len(x))
		for _, k := range sortedMapKeys(x) {
			entries = append(entries, &flatEntry{key: k, value: x[k]})
		}
		return decodeStructEntries(entries, rv, m, false)
	case []any:
		if m.hasFlatten {
			return InvalidTypeError(content, expect)
		}
		return decodeStructSeq(x, rv, m, expect)
	}
	return InvalidTypeError(content, expect)
}

// Reports whether a JSON value is a plain decoded JSON value
func isPlainJSON(v any) bool {
	switch v.(type) {
	case nil, bool, string, []any, map[string]any, json.Number, float64, uint64, int64:
		return true
	}
	return false
}

func sortedStrings(in []string) []string {
	out := append([]string{}, in...)
	sort.Strings(out)
	return out
}

// Returns the addressable value a pointer refers to
func reflectValueOf(p any) reflect.Value {
	return reflect.ValueOf(p).Elem()
}
