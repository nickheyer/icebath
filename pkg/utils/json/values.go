package jsonutil

import (
	"reflect"
	"slices"
)

// The object v holds: an *Object itself, a map[string]any copied into a new object with its keys sorted; false for anything else
func AsObject(v any) (*Object, bool) {
	switch t := v.(type) {
	case *Object:
		return t, t != nil
	case map[string]any:
		return objectOfMap(t, func(v any) any { return v }), true
	}
	return nil, false
}

// The plain map v holds: a map[string]any itself, an *Object's keys and values in a new map; false for anything else
func AsMap(v any) (map[string]any, bool) {
	switch t := v.(type) {
	case map[string]any:
		return t, true
	case *Object:
		if t == nil {
			return nil, false
		}
		return t.Map(), true
	}
	return nil, false
}

// Copy of a decoded value with every *Object turned into a map[string]any at any depth, maps and slices copied, other values as they are
func Plain(v any) any {
	switch t := v.(type) {
	case *Object:
		if t == nil {
			return nil
		}
		out := make(map[string]any, len(t.keys))
		for i, k := range t.keys {
			out[k] = Plain(t.vals[i])
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

// Copy of a value with every string-keyed map at any depth turned into an *Object with its keys sorted and every slice or array whose elements are not bytes into []any, other values as they are
func Ordered(v any) any {
	switch t := v.(type) {
	case nil, string, bool, int, int64, float64, []byte:
		return v
	case *Object:
		if t == nil {
			return nil
		}
		if len(t.keys) == 0 {
			return &Object{}
		}
		out := &Object{keys: slices.Clone(t.keys), vals: make([]any, len(t.vals))}
		for i, val := range t.vals {
			out.vals[i] = Ordered(val)
		}
		if t.index != nil {
			out.buildIndex()
		}
		return out
	case map[string]any:
		return objectOfMap(t, Ordered)
	case []any:
		out := make([]any, len(t))
		for i, val := range t {
			out[i] = Ordered(val)
		}
		return out
	}
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.Map:
		if rv.Type().Key().Kind() != reflect.String {
			return v
		}
		if rv.Len() == 0 {
			return &Object{}
		}
		keys := make([]string, 0, rv.Len())
		for _, k := range rv.MapKeys() {
			keys = append(keys, k.String())
		}
		slices.Sort(keys)
		out := &Object{keys: keys, vals: make([]any, len(keys))}
		keyType := rv.Type().Key()
		for i, k := range keys {
			out.vals[i] = Ordered(rv.MapIndex(reflect.ValueOf(k).Convert(keyType)).Interface())
		}
		if len(keys) > indexMin {
			out.buildIndex()
		}
		return out
	case reflect.Slice, reflect.Array:
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

// Object of m's keys in sorted order with each value passed through conv
func objectOfMap(m map[string]any, conv func(any) any) *Object {
	if len(m) == 0 {
		return &Object{}
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	o := &Object{keys: keys, vals: make([]any, len(keys))}
	for i, k := range keys {
		o.vals[i] = conv(m[k])
	}
	if len(keys) > indexMin {
		o.buildIndex()
	}
	return o
}

// Deep copy of a decoded value: every *Object, map[string]any and []any copied at any depth, other values as they are
func Clone(v any) any {
	switch t := v.(type) {
	case *Object:
		if t == nil {
			return t
		}
		if len(t.keys) == 0 {
			return &Object{}
		}
		c := &Object{keys: slices.Clone(t.keys), vals: make([]any, len(t.vals))}
		for i, val := range t.vals {
			c.vals[i] = Clone(val)
		}
		if t.index != nil {
			c.buildIndex()
		}
		return c
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, val := range t {
			out[k] = Clone(val)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, val := range t {
			out[i] = Clone(val)
		}
		return out
	}
	return v
}
