package tuigateway

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// One wire field of a struct type, where it sits and whether a strict fit requires it
type wireField struct {
	index     []int
	typ       reflect.Type
	required  bool
	omitZero  bool
	omitEmpty bool
	order     int
}

var wireFieldCache, extraFieldCache sync.Map

// Index of the field that keeps the keys an open object declares no field for, nil for a closed object
func extraField(t reflect.Type) []int {
	if cached, ok := extraFieldCache.Load(t); ok {
		return cached.([]int)
	}
	var index []int
	for _, f := range reflect.VisibleFields(t) {
		if f.Tag.Get("wire") == "extra" && f.Type == reflect.TypeFor[map[string]any]() {
			index = f.Index
		}
	}
	extraFieldCache.Store(t, index)
	return index
}

// Wire fields of a struct type keyed by JSON name, embedded structs flattened
func wireFields(t reflect.Type) map[string]wireField {
	for t != nil && t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t == nil || t.Kind() != reflect.Struct {
		return nil
	}
	if cached, ok := wireFieldCache.Load(t); ok {
		return cached.(map[string]wireField)
	}
	fields := map[string]wireField{}
	order := 0
	for _, f := range reflect.VisibleFields(t) {
		tag, tagged := f.Tag.Lookup("json")
		if !f.IsExported() || tag == "-" {
			continue
		}
		name, opts, _ := strings.Cut(tag, ",")
		if f.Anonymous && name == "" && indirect(f.Type).Kind() == reflect.Struct {
			continue
		}
		if !tagged || name == "" {
			name = f.Name
		}
		if _, shadowed := fields[name]; shadowed {
			continue
		}
		optional := slices.Contains(strings.Split(opts, ","), "omitempty") || slices.Contains(strings.Split(opts, ","), "omitzero")
		fields[name] = wireField{index: f.Index, typ: f.Type, required: !optional, order: order,
			omitZero: slices.Contains(strings.Split(opts, ","), "omitzero"), omitEmpty: slices.Contains(strings.Split(opts, ","), "omitempty")}
		order++
	}
	wireFieldCache.Store(t, fields)
	return fields
}

// Type a pointer points at, the type itself otherwise
func indirect(t reflect.Type) reflect.Type {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	return t
}

// Allowed values of a typed string enum, nil for a free string
func enumValues(t reflect.Type) []string {
	if enum, ok := reflect.Zero(t).Interface().(interface{ Values() []string }); ok {
		return enum.Values()
	}
	return nil
}

var (
	rawMessageType  = reflect.TypeFor[json.RawMessage]()
	unmarshalerType = reflect.TypeFor[json.Unmarshaler]()
	marshalerType   = reflect.TypeFor[json.Marshaler]()
)

// JSON text of a wire value, raw bytes kept as they are
func wireBytes(raw any) ([]byte, error) {
	switch b := raw.(type) {
	case []byte:
		return b, nil
	case json.RawMessage:
		return b, nil
	}
	return json.Marshal(raw)
}

// Generic JSON value of a text with numbers kept exact
func decodeGeneric(data []byte) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, &FitError{Problem: "not JSON: " + err.Error()}
	}
	return v, nil
}

// Decodes a wire value into out, refusing unknown keys, missing required keys and wrong types with the field named
func Strict(raw any, out any) error { return fitInto(raw, out, true) }

// Decodes a wire value into out, unknown keys and missing keys tolerated and a wrong type refused
func Loose(raw any, out any) error { return fitInto(raw, out, false) }

// Checks a wire value against a contract type and decodes it into out
func fitInto(raw any, out any, strict bool) error {
	data, err := wireBytes(raw)
	if err != nil {
		return &FitError{Problem: "not encodable as JSON: " + err.Error()}
	}
	value, err := decodeGeneric(data)
	if err != nil {
		return err
	}
	if err := fit(value, reflect.TypeOf(out).Elem(), "", strict); err != nil {
		return err
	}
	if err := json.Unmarshal(data, out); err != nil {
		return &FitError{Problem: err.Error()}
	}
	keepExtras(value, reflect.ValueOf(out).Elem())
	return nil
}

// Copies the undeclared keys of every open object in a generic value into its extra field
func keepExtras(v any, rv reflect.Value) {
	switch rv.Kind() {
	case reflect.Pointer:
		if !rv.IsNil() {
			keepExtras(v, rv.Elem())
		}
	case reflect.Slice, reflect.Array:
		items, _ := v.([]any)
		for i := 0; i < rv.Len() && i < len(items); i++ {
			keepExtras(items[i], rv.Index(i))
		}
	case reflect.Map:
		obj, _ := v.(map[string]any)
		if rv.Type().Elem().Kind() == reflect.Interface {
			return
		}
		for _, key := range rv.MapKeys() {
			if item, present := obj[key.String()]; present {
				elem := reflect.New(rv.Type().Elem()).Elem()
				elem.Set(rv.MapIndex(key))
				keepExtras(item, elem)
				rv.SetMapIndex(key, elem)
			}
		}
	case reflect.Struct:
		obj, _ := v.(map[string]any)
		fields := wireFields(rv.Type())
		if index := extraField(rv.Type()); index != nil {
			extra := map[string]any{}
			for key, item := range obj {
				if _, declared := fields[key]; !declared {
					extra[key] = plainNumbers(item)
				}
			}
			if len(extra) > 0 {
				rv.FieldByIndex(index).Set(reflect.ValueOf(extra))
			}
		}
		for name, f := range fields {
			if item, present := obj[name]; present {
				if field, err := rv.FieldByIndexErr(f.index); err == nil {
					keepExtras(item, field)
				}
			}
		}
	}
}

// Checks a wire value strictly against a type
func fitsStrictly(raw any, t reflect.Type) error {
	data, err := wireBytes(raw)
	if err != nil {
		return &FitError{Problem: "not encodable as JSON: " + err.Error()}
	}
	value, err := decodeGeneric(data)
	if err != nil {
		return err
	}
	return fit(value, t, "", true)
}

// Params a server route received, refused with the method and field named when they do not fit
func (e *Entry) FitParams(raw any) error {
	if err := fitsStrictly(paramsOrEmpty(raw), e.Params); err != nil {
		return &ParamsRefusal{Method: e.Name, Fit: err}
	}
	return nil
}

// Params of a request, an absent params member read as no params
func paramsOrEmpty(raw any) any {
	if raw == nil {
		return map[string]any{}
	}
	return raw
}

// Checks a result or payload strictly against the entry's type, the misfit naming the field
func (e *Entry) CheckResult(raw any) error { return fitsStrictly(raw, e.Result) }

// Words naming what a type takes on the wire
func wantText(t reflect.Type) string {
	switch t.Kind() {
	case reflect.String:
		return "text"
	case reflect.Bool:
		return "true or false"
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64, reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return "a whole number"
	case reflect.Float32, reflect.Float64:
		return "a number"
	case reflect.Slice, reflect.Array:
		return "a list"
	case reflect.Map, reflect.Struct:
		return "an object"
	case reflect.Pointer:
		return wantText(t.Elem()) + " or null"
	}
	return t.String()
}

// Words naming what a generic JSON value is
func gotText(v any) string {
	switch x := v.(type) {
	case nil:
		return "null"
	case string:
		return "text " + strconv.Quote(x)
	case bool:
		return strconv.FormatBool(x)
	case json.Number:
		return "the number " + x.String()
	case []any:
		return "a list"
	case map[string]any:
		return "an object"
	}
	return fmt.Sprintf("%T", v)
}

// Misfit of a value against what a type wants
func mismatch(path string, t reflect.Type, v any) error {
	return &FitError{Field: path, Problem: "wants " + wantText(t) + ", got " + gotText(v)}
}

// Checks a generic JSON value against a Go type, naming the first field that does not fit
func fit(v any, t reflect.Type, path string, strict bool) error {
	if t == rawMessageType || t.Kind() == reflect.Interface {
		return nil
	}
	if v == nil {
		if t.Kind() == reflect.Pointer || !strict {
			return nil
		}
		return mismatch(path, t, v)
	}
	if t.Kind() != reflect.Pointer && reflect.PointerTo(t).Implements(unmarshalerType) {
		data, _ := json.Marshal(v)
		if err := reflect.New(t).Interface().(json.Unmarshaler).UnmarshalJSON(data); err != nil {
			return &FitError{Field: path, Problem: err.Error()}
		}
		return nil
	}
	if union, isUnion := reflect.Zero(t).Interface().(interface{ Variants() []reflect.Type }); isUnion {
		return fitVariant(v, union.Variants(), path, strict)
	}
	switch t.Kind() {
	case reflect.Pointer:
		return fit(v, t.Elem(), path, strict)
	case reflect.String:
		s, ok := v.(string)
		if !ok {
			return mismatch(path, t, v)
		}
		if values := enumValues(t); values != nil && !slices.Contains(values, s) {
			return &FitError{Field: path, Problem: "wants one of " + enumText(values) + ", got " + strconv.Quote(s)}
		}
	case reflect.Bool:
		if _, ok := v.(bool); !ok {
			return mismatch(path, t, v)
		}
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		n, ok := v.(json.Number)
		i, err := strconv.ParseInt(string(n), 10, 64)
		if !ok || err != nil || reflect.Zero(t).OverflowInt(i) {
			return mismatch(path, t, v)
		}
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		n, ok := v.(json.Number)
		u, err := strconv.ParseUint(string(n), 10, 64)
		if !ok || err != nil || reflect.Zero(t).OverflowUint(u) {
			return mismatch(path, t, v)
		}
	case reflect.Float32, reflect.Float64:
		n, ok := v.(json.Number)
		if _, err := strconv.ParseFloat(string(n), 64); !ok || err != nil {
			return mismatch(path, t, v)
		}
	case reflect.Slice, reflect.Array:
		items, ok := v.([]any)
		if !ok {
			return mismatch(path, t, v)
		}
		for i, item := range items {
			if err := fit(item, t.Elem(), indexPath(path, i), strict); err != nil {
				return err
			}
		}
	case reflect.Map:
		obj, ok := v.(map[string]any)
		if !ok || t.Key().Kind() != reflect.String {
			return mismatch(path, t, v)
		}
		for _, key := range sortedKeys(obj) {
			if err := fit(obj[key], t.Elem(), joinPath(path, key), strict); err != nil {
				return err
			}
		}
	case reflect.Struct:
		obj, ok := v.(map[string]any)
		if !ok {
			return mismatch(path, t, v)
		}
		fields := wireFields(t)
		for _, key := range sortedKeys(obj) {
			f, known := fields[key]
			if !known {
				if strict && extraField(t) == nil {
					return &FitError{Field: joinPath(path, key), Problem: problemUnknown}
				}
				continue
			}
			if obj[key] == nil && !f.required {
				continue
			}
			if err := fit(obj[key], f.typ, joinPath(path, key), strict); err != nil {
				return err
			}
		}
		if strict {
			for _, name := range fieldNames(t) {
				if _, present := obj[name]; !present && fields[name].required {
					return &FitError{Field: joinPath(path, name), Problem: problemRequired}
				}
			}
		}
	default:
		return &FitError{Field: path, Problem: "has no wire form for " + t.String()}
	}
	return nil
}

// Checks a value against the variants of a union, fitting when one variant fits and naming every misfit otherwise
func fitVariant(v any, variants []reflect.Type, path string, strict bool) error {
	misfits := make([]string, 0, len(variants))
	for _, variant := range variants {
		err := fit(v, variant, path, strict)
		if err == nil {
			return nil
		}
		misfits = append(misfits, err.Error())
	}
	return &FitError{Field: path, Problem: "fits no variant (" + strings.Join(misfits, " | ") + ")"}
}

// Keys of a JSON object in sorted order
func sortedKeys(obj map[string]any) []string {
	keys := make([]string, 0, len(obj))
	for k := range obj {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// Wire value of a typed value, plain maps, lists, text, booleans, int64 and float64, nil lists and maps empty
func Plain(v any) any { return plainValue(reflect.ValueOf(v)) }

// Wire value of one reflected value
func plainValue(rv reflect.Value) any {
	if !rv.IsValid() {
		return nil
	}
	if (rv.Kind() == reflect.Pointer || rv.Kind() == reflect.Interface || rv.Kind() == reflect.Map || rv.Kind() == reflect.Slice) && rv.IsNil() {
		switch rv.Kind() {
		case reflect.Map:
			return map[string]any{}
		case reflect.Slice:
			return []any{}
		}
		return nil
	}
	if rv.Kind() != reflect.Interface && rv.Type().Implements(marshalerType) {
		data, err := rv.Interface().(json.Marshaler).MarshalJSON()
		if err != nil {
			return nil
		}
		value, _ := decodeGeneric(data)
		return plainNumbers(value)
	}
	switch rv.Kind() {
	case reflect.Pointer, reflect.Interface:
		return plainValue(rv.Elem())
	case reflect.String:
		return rv.String()
	case reflect.Bool:
		return rv.Bool()
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return rv.Int()
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return int64(rv.Uint())
	case reflect.Float32, reflect.Float64:
		return rv.Float()
	case reflect.Slice, reflect.Array:
		out := make([]any, rv.Len())
		for i := range out {
			out[i] = plainValue(rv.Index(i))
		}
		return out
	case reflect.Map:
		out := make(map[string]any, rv.Len())
		iter := rv.MapRange()
		for iter.Next() {
			out[iter.Key().String()] = plainValue(iter.Value())
		}
		return out
	case reflect.Struct:
		fields := wireFields(rv.Type())
		out := make(map[string]any, len(fields))
		if index := extraField(rv.Type()); index != nil {
			for key, item := range rv.FieldByIndex(index).Interface().(map[string]any) {
				out[key] = plainValue(reflect.ValueOf(item))
			}
		}
		for name, f := range fields {
			value, err := rv.FieldByIndexErr(f.index)
			if err != nil || f.omitZero && value.IsZero() || f.omitEmpty && emptyValue(value) {
				continue
			}
			out[name] = plainValue(value)
		}
		return out
	}
	return nil
}

// Whether omitempty leaves a value out
func emptyValue(v reflect.Value) bool {
	switch v.Kind() {
	case reflect.Array, reflect.Map, reflect.Slice, reflect.String:
		return v.Len() == 0
	case reflect.Pointer, reflect.Interface:
		return v.IsNil()
	}
	return v.IsZero()
}

// Generic JSON value with exact numbers turned into int64 when whole and float64 otherwise
func plainNumbers(v any) any {
	switch x := v.(type) {
	case json.Number:
		if i, err := strconv.ParseInt(string(x), 10, 64); err == nil {
			return i
		}
		f, _ := strconv.ParseFloat(string(x), 64)
		return f
	case []any:
		for i := range x {
			x[i] = plainNumbers(x[i])
		}
	case map[string]any:
		for k := range x {
			x[k] = plainNumbers(x[k])
		}
	}
	return v
}

// Copies every field a patch carries, a set pointer, list or map or a non zero value, over the base
func Merge[T any](base *T, patch T) {
	dst, src := reflect.ValueOf(base).Elem(), reflect.ValueOf(patch)
	for i := range src.NumField() {
		if field := src.Field(i); dst.Field(i).CanSet() && !field.IsZero() {
			dst.Field(i).Set(field)
		}
	}
}
