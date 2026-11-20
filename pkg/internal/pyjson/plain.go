package pyjson

import (
	"math/big"
	"reflect"
	"sort"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

// Decoded JSON with every Object turned into a plain map and every map and list copied, scalars unchanged
func Plain(v any) any {
	switch t := v.(type) {
	case *Object:
		if t == nil {
			return nil
		}
		out := make(map[string]any, len(t.Vals))
		for k, val := range t.Vals {
			out[k] = Plain(val)
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, val := range t {
			out[k] = Plain(val)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, val := range t {
			out[i] = Plain(val)
		}
		return out
	}
	return v
}

// Python decoded value in the DecodeWTF8 shapes, dicts as plain maps and every int or float as float64
func WTF8Shape(v any) any {
	switch t := v.(type) {
	case *Object:
		if t == nil {
			return nil
		}
		out := make(map[string]any, len(t.Vals))
		for k, val := range t.Vals {
			out[k] = WTF8Shape(val)
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, val := range t {
			out[k] = WTF8Shape(val)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, val := range t {
			out[i] = WTF8Shape(val)
		}
		return out
	case int64:
		return float64(t)
	case *big.Int:
		if t == nil {
			return nil
		}
		f, _ := new(big.Float).SetInt(t).Float64()
		return f
	}
	return v
}

// Value with every string keyed dict at any depth turned into an Object, plain map keys sorted
func Ordered(v any) any {
	switch t := v.(type) {
	case nil, string, bool, float64, int, int64, *big.Int:
		return v
	case *Object:
		if t == nil {
			return nil
		}
		out := &Object{Keys: append([]string(nil), t.Keys...), Vals: make(map[string]any, len(t.Vals))}
		for k, val := range t.Vals {
			out.Vals[k] = Ordered(val)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, val := range t {
			out[i] = Ordered(val)
		}
		return out
	case pyrepr.Mapping:
		out := NewObject()
		for k, val := range t.PyItems() {
			key, isString := k.(string)
			if !isString {
				return v
			}
			out.Set(key, Ordered(val))
		}
		return out
	}
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.Map:
		if rv.Type().Key().Kind() != reflect.String {
			return v
		}
		if rv.IsNil() {
			return NewObject()
		}
		keys := make([]string, 0, rv.Len())
		for _, k := range rv.MapKeys() {
			keys = append(keys, k.String())
		}
		sort.Strings(keys)
		out := NewObject()
		for _, k := range keys {
			out.Set(k, Ordered(rv.MapIndex(reflect.ValueOf(k).Convert(rv.Type().Key())).Interface()))
		}
		return out
	case reflect.Slice:
		if rv.Type().Elem().Kind() == reflect.Uint8 {
			return v
		}
		out := make([]any, rv.Len())
		for i := range out {
			out[i] = Ordered(rv.Index(i).Interface())
		}
		return out
	}
	return v
}
