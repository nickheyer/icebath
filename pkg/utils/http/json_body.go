package httputil

import (
	"encoding"
	"encoding/json"
	"fmt"
	"math"
	"reflect"
	"strconv"

	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
)

// Request body of v as compact UTF-8 JSON, object keys in their order, an error for a NaN or infinite number anywhere in it
func JSONBody(v any) ([]byte, error) {
	if err := checkFinite(v, 0); err != nil {
		return nil, err
	}
	text, err := jsonutil.Encode(v)
	if err != nil {
		return nil, err
	}
	return []byte(text), nil
}

// Nesting past which checkFinite stops looking, the encoder reporting a cycle there itself
const maxFiniteDepth = 1000

// First NaN or infinite float inside v, which JSON has no form for; the decoded shapes without reflection, typed values through it
func checkFinite(v any, depth int) error {
	if depth > maxFiniteDepth {
		return nil
	}
	switch t := v.(type) {
	case nil, string, bool, int, int64, int32, int16, int8, uint, uint64, uint32, uint16, uint8, json.Number, json.RawMessage, []string, []byte:
		return nil
	case float64:
		return finiteFloat(t)
	case float32:
		return finiteFloat(float64(t))
	case *jsonutil.Object:
		for _, item := range t.All() {
			if err := checkFinite(item, depth+1); err != nil {
				return err
			}
		}
		return nil
	case map[string]any:
		for _, item := range t {
			if err := checkFinite(item, depth+1); err != nil {
				return err
			}
		}
		return nil
	case []any:
		for _, item := range t {
			if err := checkFinite(item, depth+1); err != nil {
				return err
			}
		}
		return nil
	case []map[string]any:
		for _, item := range t {
			if err := checkFinite(item, depth+1); err != nil {
				return err
			}
		}
		return nil
	case []float64:
		for _, item := range t {
			if err := finiteFloat(item); err != nil {
				return err
			}
		}
		return nil
	case json.Marshaler, encoding.TextMarshaler, error:
		return nil
	}
	return finiteValue(reflect.ValueOf(v), depth)
}

func finiteFloat(f float64) error {
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return fmt.Errorf("JSON body cannot hold the number %s", strconv.FormatFloat(f, 'g', -1, 64))
	}
	return nil
}

var (
	marshalerType     = reflect.TypeFor[json.Marshaler]()
	textMarshalerType = reflect.TypeFor[encoding.TextMarshaler]()
	errorType         = reflect.TypeFor[error]()
	objectPtrType     = reflect.TypeFor[*jsonutil.Object]()
	objectType        = reflect.TypeFor[jsonutil.Object]()
)

// Whether values of the type write their own JSON or, as errors, their text, so no float inside them is encoded as a number
func writesItself(t reflect.Type) bool {
	return t.Implements(marshalerType) || t.Implements(textMarshalerType) || t.Implements(errorType) ||
		t.Kind() != reflect.Pointer && (reflect.PointerTo(t).Implements(marshalerType) || reflect.PointerTo(t).Implements(textMarshalerType))
}

// Whether an unexported field is an embedded struct or pointer to one, whose exported fields JSON encoding still writes
func embeddedStruct(field reflect.StructField) bool {
	t := field.Type
	if t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	return field.Anonymous && t.Kind() == reflect.Struct
}

// checkFinite over a typed value: the floats of structs (the exported fields JSON encoding writes), maps, slices, arrays, pointers and interfaces
func finiteValue(v reflect.Value, depth int) error {
	if !v.IsValid() || depth > maxFiniteDepth {
		return nil
	}
	switch v.Type() {
	case objectPtrType:
		if v.IsNil() || !v.CanInterface() {
			return nil
		}
		o, _ := reflect.TypeAssert[*jsonutil.Object](v)
		return checkFinite(o, depth+1)
	case objectType:
		if !v.CanInterface() {
			return nil
		}
		o, _ := reflect.TypeAssert[jsonutil.Object](v)
		return checkFinite(&o, depth+1)
	}
	if writesItself(v.Type()) {
		return nil
	}
	switch v.Kind() {
	case reflect.Float32, reflect.Float64:
		return finiteFloat(v.Float())
	case reflect.Interface, reflect.Pointer:
		if v.IsNil() {
			return nil
		}
		return finiteValue(v.Elem(), depth+1)
	case reflect.Struct:
		t := v.Type()
		for i := range t.NumField() {
			field := t.Field(i)
			if field.Tag.Get("json") == "-" || !field.IsExported() && !embeddedStruct(field) {
				continue
			}
			if err := finiteValue(v.Field(i), depth+1); err != nil {
				return err
			}
		}
	case reflect.Map:
		for iter := v.MapRange(); iter.Next(); {
			if err := finiteValue(iter.Value(), depth+1); err != nil {
				return err
			}
		}
	case reflect.Slice, reflect.Array:
		if v.Type().Elem().Kind() == reflect.Uint8 {
			return nil
		}
		for i := range v.Len() {
			if err := finiteValue(v.Index(i), depth+1); err != nil {
				return err
			}
		}
	}
	return nil
}
