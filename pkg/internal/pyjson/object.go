package pyjson

import (
	"fmt"
	"sort"
)

// Ordered object built from alternating string keys and values in dict literal order
func ObjectOf(pairs ...any) *Object {
	if len(pairs)%2 != 0 {
		panic("pyjson.ObjectOf needs key value pairs")
	}
	o := NewObject()
	for i := 0; i < len(pairs); i += 2 {
		key, ok := pairs[i].(string)
		if !ok {
			panic(fmt.Sprintf("pyjson.ObjectOf key %v is not a string", pairs[i]))
		}
		o.Set(key, pairs[i+1])
	}
	return o
}

// Shallow copy with the same key order, the dict.copy analogue
func (o *Object) Copy() *Object {
	c := &Object{Keys: append([]string(nil), o.Keys...), Vals: make(map[string]any, len(o.Vals))}
	for k, v := range o.Vals {
		c.Vals[k] = v
	}
	return c
}

// Sets every key of other in its order, existing keys keeping their position as dict.update does
func (o *Object) Update(other *Object) {
	if other == nil {
		return
	}
	for _, k := range other.Keys {
		o.Set(k, other.Vals[k])
	}
}

// Removes key and returns its value and whether it was present, the dict.pop analogue
func (o *Object) Pop(key string) (any, bool) {
	v, ok := o.Vals[key]
	if ok {
		o.Delete(key)
	}
	return v, ok
}

// Deep copy keeping every Object ordered, copying maps and lists and leaving scalars unchanged
func Clone(v any) any {
	switch t := v.(type) {
	case *Object:
		if t == nil {
			return t
		}
		c := &Object{Keys: append([]string(nil), t.Keys...), Vals: make(map[string]any, len(t.Vals))}
		for k, val := range t.Vals {
			c.Vals[k] = Clone(val)
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

// Object view of a dict value, a plain map entering with sorted keys, false for anything else
func AsObject(v any) (*Object, bool) {
	switch t := v.(type) {
	case *Object:
		return t, t != nil
	case map[string]any:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		o := NewObject()
		for _, k := range keys {
			o.Set(k, t[k])
		}
		return o, true
	}
	return nil, false
}

// Read view of a dict value, a plain map itself or the value map of an Object, false for anything else
func Dict(v any) (map[string]any, bool) {
	switch t := v.(type) {
	case map[string]any:
		return t, true
	case *Object:
		if t == nil {
			return nil, false
		}
		if t.Vals == nil {
			return map[string]any{}, true
		}
		return t.Vals, true
	}
	return nil, false
}
