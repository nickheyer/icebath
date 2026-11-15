package homeassistant

import (
	"fmt"
	"math"
	"math/big"
	"sort"
	"strconv"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

// Python exception carrying its class name and its str
type pyErr struct{ kind, msg string }

func (e *pyErr) Error() string      { return e.msg }
func (e *pyErr) PyTypeName() string { return e.kind }

// AttributeError an attribute read raises on a value of another type
func attrErr(v any, attr string) error {
	return &pyErr{"AttributeError", fmt.Sprintf("'%s' object has no attribute '%s'", pyrepr.TypeName(v), attr)}
}

// Whether a decoded value is a Python dict
func isDict(v any) bool {
	switch v.(type) {
	case *pyjson.Object, map[string]any:
		return true
	}
	return false
}

// dict get of a decoded value, the AttributeError a non dict raises
func pyGet(v any, key string, def any) (any, error) {
	switch m := v.(type) {
	case *pyjson.Object:
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
	return nil, attrErr(v, "get")
}

// Subscript of a decoded value by a str key with the KeyError or TypeError Python raises
func pyIndex(v any, key string) (any, error) {
	switch m := v.(type) {
	case *pyjson.Object:
		if m != nil {
			if val, ok := m.Get(key); ok {
				return val, nil
			}
			return nil, &pyErr{"KeyError", pyrepr.Repr(key)}
		}
	case map[string]any:
		if val, ok := m[key]; ok {
			return val, nil
		}
		return nil, &pyErr{"KeyError", pyrepr.Repr(key)}
	case string:
		return nil, &pyErr{"TypeError", "string indices must be integers, not 'str'"}
	case []any:
		return nil, &pyErr{"TypeError", "list indices must be integers or slices, not str"}
	}
	return nil, &pyErr{"TypeError", fmt.Sprintf("'%s' object is not subscriptable", pyrepr.TypeName(v))}
}

// Keys of a dict in iteration order, a plain map's keys sorted
func dictKeys(v any) []string {
	switch m := v.(type) {
	case *pyjson.Object:
		return append([]string(nil), m.Keys...)
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
	case *pyjson.Object, map[string]any:
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
	return nil, &pyErr{"TypeError", fmt.Sprintf("'%s' object is not iterable", pyrepr.TypeName(v))}
}

// Distinct key type of an int past the int64 range
type bigKey string

// Python int of a base ten str, false past the 4300 digit conversion limit like int()
func parseIntStr(s string) (*big.Int, bool) {
	n, ok := pyint.Parse(s)
	if !ok || len(strings.TrimPrefix(n.String(), "-")) > 4300 {
		return nil, false
	}
	return n, true
}

// int() of a config value, a str past the 4300 digit limit refused with the ValueError int raises
func pyIntOf(v any) (*big.Int, error) {
	s, ok := v.(string)
	if !ok {
		return pyint.Of(v)
	}
	n, err := pyint.Of(s)
	if err != nil {
		return nil, err
	}
	if digits := len(strings.TrimPrefix(n.String(), "-")); digits > 4300 {
		return nil, &pyErr{"ValueError", fmt.Sprintf("Exceeds the limit (4300 digits) for integer string conversion: value has %d digits; "+
			"use sys.set_int_max_str_digits() to increase the limit", digits)}
	}
	return n, nil
}

// Hash identity of a dict key or set element, the TypeError naming that use for an unhashable value
func hashKeyAs(v any, use string) (any, error) {
	hk, err := hashKey(v)
	if err != nil {
		name := pyrepr.TypeName(v)
		return nil, &pyErr{"TypeError", fmt.Sprintf("cannot use '%s' as a %s (unhashable type: '%s')", name, use, name)}
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
	case []any, *pyjson.Object, map[string]any:
		return nil, &pyErr{"TypeError", fmt.Sprintf("unhashable type: '%s'", pyrepr.TypeName(v))}
	}
	if n, ok := pyrepr.Int(v); ok {
		if n.IsInt64() {
			return n.Int64(), nil
		}
		return bigKey(n.String()), nil
	}
	return v, nil
}

// Python dict over hashable keys, the stored value of each key returned by lookups
type keyedMap map[any]any

// Value under a key with whether it is present, the TypeError of an unhashable key
func (m keyedMap) get(k any) (any, bool, error) {
	hk, err := hashKeyAs(k, "dict key")
	if err != nil {
		return nil, false, err
	}
	v, ok := m[hk]
	return v, ok, nil
}

// Stores a value under a key, the TypeError of an unhashable key
func (m keyedMap) set(k, v any) error {
	hk, err := hashKeyAs(k, "dict key")
	if err != nil {
		return err
	}
	m[hk] = v
	return nil
}

// Python set built from a config value, its truthiness and membership by hash identity
type pySet struct{ items map[any]bool }

// set() of a config value, characters of a str and keys of a dict, the TypeError of a non iterable
func newPySet(v any) (pySet, error) {
	s := pySet{items: map[any]bool{}}
	items, err := pyIter(v)
	if err != nil {
		return s, err
	}
	for _, item := range items {
		hk, err := hashKeyAs(item, "set element")
		if err != nil {
			return s, err
		}
		s.items[hk] = true
	}
	return s, nil
}

// Whether the set is non empty
func (s pySet) truthy() bool { return len(s.items) > 0 }

// Membership of a value, the TypeError of an unhashable probe
func (s pySet) has(v any) (bool, error) {
	hk, err := hashKeyAs(v, "set element")
	if err != nil {
		return false, err
	}
	return s.items[hk], nil
}

// Python int of an isinstance int value excluding bool, false for anything else
func strictInt(v any) (*big.Int, bool) {
	if _, isBool := v.(bool); isBool {
		return nil, false
	}
	return pyrepr.Int(v)
}

// JSON object key text json.dumps writes for a dict key
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
		return pyrepr.Float(t), nil
	}
	if n, ok := pyrepr.Int(k); ok {
		return n.String(), nil
	}
	return "", &pyErr{"TypeError", fmt.Sprintf("keys must be str, int, float, bool or None, not %s", pyrepr.TypeName(k))}
}

// Pairs dict() reads from a list element, the errors dict raises for a bad element
func updatePair(index int, elem any) (any, any, error) {
	var parts []any
	switch t := elem.(type) {
	case string, []any, *pyjson.Object, map[string]any:
		parts, _ = pyIter(t)
	default:
		return nil, nil, &pyErr{"TypeError", "cannot convert dictionary update sequence element #" + strconv.Itoa(index) + " to a sequence"}
	}
	if len(parts) != 2 {
		return nil, nil, &pyErr{"ValueError", fmt.Sprintf("dictionary update sequence element #%d has length %d; 2 is required", index, len(parts))}
	}
	return parts[0], parts[1], nil
}

// dict(data) as an ordered JSON object, equal keys merged and non str keys written as json.dumps writes them
func pyDict(data any) (*pyjson.Object, error) {
	out := pyjson.NewObject()
	if !pyrepr.Truthy(data) {
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
		return nil, &pyErr{"TypeError", fmt.Sprintf("'%s' object is not iterable", pyrepr.TypeName(data))}
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
		hk, err := hashKeyAs(k, "dict key")
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
