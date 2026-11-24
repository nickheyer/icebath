package langfuse

import (
	"encoding/json"
	"math"
	"math/big"
	"reflect"
	"sort"
	"strconv"

	"github.com/nickheyer/icebath/pkg/state"
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
)

// One dict entry in iteration order
type item struct {
	key string
	val any
}

// Entries of a dict value, an object in insertion order and plain maps in sorted key order
func dictItems(v any) ([]item, bool) {
	switch t := v.(type) {
	case nil:
		return nil, false
	case *jsonutil.Object:
		if t == nil {
			return nil, false
		}
		out := make([]item, 0, t.Len())
		for k, val := range t.All() {
			out = append(out, item{k, val})
		}
		return out, true
	case jsonutil.Object:
		return dictItems(&t)
	case map[string]any:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		out := make([]item, 0, len(keys))
		for _, k := range keys {
			out = append(out, item{k, t[k]})
		}
		return out, true
	}
	rv := reflect.ValueOf(v)
	if rv.Kind() != reflect.Map || rv.Type().Key().Kind() != reflect.String {
		return nil, false
	}
	keys := rv.MapKeys()
	sort.Slice(keys, func(i, j int) bool { return keys[i].String() < keys[j].String() })
	out := make([]item, 0, len(keys))
	for _, k := range keys {
		out = append(out, item{k.String(), rv.MapIndex(k).Interface()})
	}
	return out, true
}

// Value under key of a dict value and whether the key is present
func dictGet(v any, key string) (any, bool) {
	switch t := v.(type) {
	case *jsonutil.Object:
		if t == nil {
			return nil, false
		}
		return t.Get(key)
	case map[string]any:
		val, ok := t[key]
		return val, ok
	}
	items, ok := dictItems(v)
	if !ok {
		return nil, false
	}
	for _, it := range items {
		if it.key == key {
			return it.val, true
		}
	}
	return nil, false
}

// dict.get of key, nil when the value is not a dict or lacks the key
func dget(v any, key string) any {
	val, _ := dictGet(v, key)
	return val
}

// Reports whether a value is a Python dict
func isDict(v any) bool {
	_, ok := dictItems(v)
	return ok
}

// Elements of a list, tuple or set value, false for strings, bytes and everything else
func listItems(v any) ([]any, bool) {
	switch t := v.(type) {
	case nil, string, []byte:
		return nil, false
	case []any:
		return t, true
	case []map[string]any:
		out := make([]any, len(t))
		for i, m := range t {
			out[i] = m
		}
		return out, true
	case []*jsonutil.Object:
		out := make([]any, len(t))
		for i, o := range t {
			out[i] = o
		}
		return out, true
	case []string:
		out := make([]any, len(t))
		for i, s := range t {
			out[i] = s
		}
		return out, true
	}
	rv := reflect.ValueOf(v)
	if rv.Kind() != reflect.Slice && rv.Kind() != reflect.Array {
		return nil, false
	}
	out := make([]any, rv.Len())
	for i := range out {
		out[i] = rv.Index(i).Interface()
	}
	return out, true
}

// Reports whether a value is a Python int or float, bool excluded
func isNumber(v any) bool {
	switch v.(type) {
	case int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64, float32, float64, json.Number, *big.Int:
		return true
	}
	return false
}

// Python int value of an int shaped value, integral float64 narrowed back to int64
func pyIntValue(v any) (any, bool) {
	switch t := v.(type) {
	case bool, int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64:
		return t, true
	case *big.Int:
		return t, t != nil
	case json.Number:
		if n, err := strconv.ParseInt(t.String(), 10, 64); err == nil {
			return n, true
		}
		if n, ok := new(big.Int).SetString(t.String(), 10); ok {
			return n, true
		}
		return nil, false
	case float64:
		if t == math.Trunc(t) && !math.IsInf(t, 0) && math.Abs(t) < 1<<63 {
			return int64(t), true
		}
	}
	return nil, false
}

// Float value of a numeric value
func toFloat(v any) (float64, bool) {
	switch t := v.(type) {
	case float64:
		return t, true
	case float32:
		return float64(t), true
	case json.Number:
		f, err := t.Float64()
		return f, err == nil
	case *big.Int:
		f, _ := new(big.Float).SetInt(t).Float64()
		return f, true
	case bool:
		if t {
			return 1, true
		}
		return 0, true
	}
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return float64(rv.Int()), true
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return float64(rv.Uint()), true
	}
	return 0, false
}

// Python int() of a numeric value truncating floats, zero for anything else
func toInt(v any) int {
	if n, ok := pyIntValue(v); ok {
		switch t := n.(type) {
		case bool:
			if t {
				return 1
			}
			return 0
		case *big.Int:
			return int(t.Int64())
		}
		f, _ := toFloat(n)
		return int(f)
	}
	if f, ok := toFloat(v); ok && !math.IsNaN(f) && !math.IsInf(f, 0) {
		return int(f)
	}
	return 0
}

// Attribute of an object the agent hands over as a dict or struct, nil when absent
func getattr(v any, name string) any {
	if v == nil {
		return nil
	}
	return state.ToolCallField(v, name)
}

// Attribute of a value that only objects carry, a dict never answering as Python attribute lookup does
func objectAttr(v any, name string) any {
	if v == nil || isDict(v) {
		return nil
	}
	if _, ok := listItems(v); ok {
		return nil
	}
	switch v.(type) {
	case string, []byte, bool:
		return nil
	}
	if isNumber(v) {
		return nil
	}
	return state.ToolCallField(v, name)
}

// Python string equality of a value with a string, false for non strings
func strEq(v any, s string) bool {
	t, ok := v.(string)
	return ok && t == s
}

// Struct converted to its field dict the way vars() exposes attributes, false for non structs
func structVars(v any) (any, bool) {
	rv := reflect.ValueOf(v)
	for rv.Kind() == reflect.Pointer {
		if rv.IsNil() {
			return nil, false
		}
		rv = rv.Elem()
	}
	if rv.Kind() != reflect.Struct {
		return nil, false
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, false
	}
	decoded, err := jsonutil.Decode(string(raw))
	if err != nil {
		return nil, false
	}
	if !isDict(decoded) {
		return nil, false
	}
	return decoded, true
}
