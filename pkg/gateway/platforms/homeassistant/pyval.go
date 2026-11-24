package homeassistant

import (
	"fmt"
	"math"
	"reflect"
	"sort"
	"strconv"
	"strings"

	"github.com/nickheyer/icebath/pkg/errchain"
	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
)

// Error carrying a kind label beside its message
type pyErr struct{ kind, msg string }

func (e *pyErr) Error() string     { return e.msg }
func (e *pyErr) ErrorKind() string { return e.kind }

// AttributeError of a value of the wrong kind, naming the field and the kind it needs
func attrErr(v any, field, want string) error {
	return &pyErr{"AttributeError", fmt.Sprintf("%s must be %s, got %s", field, want, errchain.Kind(v))}
}

// Whether a decoded value is a Python dict
func isDict(v any) bool {
	switch v.(type) {
	case *jsonutil.Object, map[string]any:
		return true
	}
	return false
}

// dict get of a decoded value, the AttributeError a non dict raises
func pyGet(v any, key string, def any) (any, error) {
	switch m := v.(type) {
	case *jsonutil.Object:
		if m != nil {
			if val, ok := m.Get(key); ok {
				return val, nil
			}
			return def, nil
		}
	case map[string]any:
		if val, ok := m[key]; ok {
			return val, nil
		}
		return def, nil
	}
	return nil, &pyErr{"AttributeError", fmt.Sprintf("expected a mapping holding %s, got %s", key, errchain.Kind(v))}
}

// Subscript of a decoded value by a str key with the KeyError or TypeError Python raises
func pyIndex(v any, key string) (any, error) {
	switch m := v.(type) {
	case *jsonutil.Object:
		if m != nil {
			if val, ok := m.Get(key); ok {
				return val, nil
			}
			return nil, &pyErr{"KeyError", "missing " + key}
		}
	case map[string]any:
		if val, ok := m[key]; ok {
			return val, nil
		}
		return nil, &pyErr{"KeyError", "missing " + key}
	}
	return nil, &pyErr{"TypeError", fmt.Sprintf("expected a mapping holding %s, got %s", key, errchain.Kind(v))}
}

// Keys of a dict in iteration order, a plain map's keys sorted
func dictKeys(v any) []string {
	switch m := v.(type) {
	case *jsonutil.Object:
		return append([]string(nil), m.Keys()...)
	case map[string]any:
		keys := make([]string, 0, len(m))
		for k := range m {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		return keys
	}
	return nil
}

// Items a for loop over a decoded value yields, the TypeError of a value Python cannot iterate
func pyIter(v any) ([]any, error) {
	switch t := v.(type) {
	case []any:
		return t, nil
	case *jsonutil.Object, map[string]any:
		keys := dictKeys(t)
		out := make([]any, len(keys))
		for i, k := range keys {
			out[i] = k
		}
		return out, nil
	case string:
		out := make([]any, 0, len(t))
		for _, r := range t {
			out = append(out, string(r))
		}
		return out, nil
	}
	if rv := reflect.ValueOf(v); v != nil && (rv.Kind() == reflect.Slice || rv.Kind() == reflect.Array) {
		out := make([]any, rv.Len())
		for i := range out {
			out[i] = rv.Index(i).Interface()
		}
		return out, nil
	}
	return nil, &pyErr{"TypeError", fmt.Sprintf("expected a list, got %s", errchain.Kind(v))}
}

// Integer of a base ten text with surrounding space ignored, false when it is not one or lies outside the int64 range
func parseIntStr(s string) (int64, bool) {
	n, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	return n, err == nil
}

// Hash identity of a dict key or set element, the TypeError naming that use for an unhashable value
func hashKeyAs(v any, use string) (any, error) {
	hk, err := hashKey(v)
	if err != nil {
		return nil, &pyErr{"TypeError", fmt.Sprintf("%s cannot be a %s", errchain.Kind(v), use)}
	}
	return hk, nil
}

// Hash identity of a value with equal numbers sharing one key, the TypeError of an unhashable value
func hashKey(v any) (any, error) {
	switch t := v.(type) {
	case nil:
		return nil, nil
	case string:
		return t, nil
	case bool:
		if t {
			return int64(1), nil
		}
		return int64(0), nil
	case float64:
		if !math.IsInf(t, 0) && !math.IsNaN(t) && t == math.Trunc(t) && math.Abs(t) < math.MaxInt64 {
			return int64(t), nil
		}
		return t, nil
	case []any, *jsonutil.Object, map[string]any:
		return nil, &pyErr{"TypeError", fmt.Sprintf("expected a single value, got %s", errchain.Kind(v))}
	}
	if n, ok := anyutil.Int(v); ok {
		return n, nil
	}
	if !reflect.TypeOf(v).Comparable() {
		return nil, &pyErr{"TypeError", fmt.Sprintf("expected a single value, got %s", errchain.Kind(v))}
	}
	return v, nil
}

// Python dict over hashable keys, the stored value of each key returned by lookups
type keyedMap map[any]any

// Value under a key with whether it is present, the TypeError of an unhashable key
func (m keyedMap) get(k any) (any, bool, error) {
	hk, err := hashKeyAs(k, "mapping key")
	if err != nil {
		return nil, false, err
	}
	v, ok := m[hk]
	return v, ok, nil
}

// Stores a value under a key, the TypeError of an unhashable key
func (m keyedMap) set(k, v any) error {
	hk, err := hashKeyAs(k, "mapping key")
	if err != nil {
		return err
	}
	m[hk] = v
	return nil
}

// JSON object key text of a mapping key
func jsonKey(k any) (string, error) {
	switch t := k.(type) {
	case string:
		return t, nil
	case bool:
		if t {
			return "true", nil
		}
		return "false", nil
	case nil:
		return "null", nil
	case float64:
		switch {
		case math.IsNaN(t):
			return "NaN", nil
		case math.IsInf(t, 1):
			return "Infinity", nil
		case math.IsInf(t, -1):
			return "-Infinity", nil
		}
		return strconv.FormatFloat(t, 'f', -1, 64), nil
	}
	if n, ok := anyutil.Int(k); ok {
		return strconv.FormatInt(n, 10), nil
	}
	return "", &pyErr{"TypeError", fmt.Sprintf("a mapping key must be text, a number, true or false or nothing, got %s", errchain.Kind(k))}
}

// Pairs dict() reads from a list element, the errors dict raises for a bad element
func updatePair(index int, elem any) (any, any, error) {
	var parts []any
	switch t := elem.(type) {
	case string, []any, *jsonutil.Object, map[string]any:
		parts, _ = pyIter(t)
	default:
		return nil, nil, &pyErr{"TypeError", fmt.Sprintf("item %d must be a pair of key and value, got %s", index, errchain.Kind(elem))}
	}
	if len(parts) != 2 {
		return nil, nil, &pyErr{"ValueError", fmt.Sprintf("item %d must be a pair of key and value, got %d values", index, len(parts))}
	}
	return parts[0], parts[1], nil
}

// dict(data) as an ordered JSON object, equal keys merged and keys that are not text written as JSON object keys
func pyDict(data any) (*jsonutil.Object, error) {
	out := jsonutil.NewObject()
	if !anyutil.IsTrue(data) {
		return out, nil
	}
	if isDict(data) {
		for _, k := range dictKeys(data) {
			v, _ := pyGet(data, k, nil)
			out.Set(k, v)
		}
		return out, nil
	}
	var elems []any
	switch t := data.(type) {
	case []any, string:
		elems, _ = pyIter(t)
	default:
		return nil, &pyErr{"TypeError", fmt.Sprintf("expected a mapping or a list of pairs, got %s", errchain.Kind(data))}
	}
	type entry struct {
		key any
		val any
	}
	var order []any
	entries := map[any]*entry{}
	for i, elem := range elems {
		k, v, err := updatePair(i, elem)
		if err != nil {
			return nil, err
		}
		hk, err := hashKeyAs(k, "mapping key")
		if err != nil {
			return nil, err
		}
		if e, ok := entries[hk]; ok {
			e.val = v
			continue
		}
		entries[hk] = &entry{key: k, val: v}
		order = append(order, hk)
	}
	for _, hk := range order {
		e := entries[hk]
		text, err := jsonKey(e.key)
		if err != nil {
			return nil, err
		}
		out.Set(text, e.val)
	}
	return out, nil
}
