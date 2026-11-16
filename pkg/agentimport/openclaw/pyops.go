package openclaw

import (
	"errors"
	"fmt"
	"math"
	"math/big"
	"sort"
	"strings"
	"unicode"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Python exception raised by the engine, str giving the message the script would print
type PyError struct {
	Type string
	Msg  string
}

func (e *PyError) Error() string { return e.Msg }

// Python exception class name of the error
func (e *PyError) PyTypeName() string { return e.Type }

// Carrier of a raised exception through a panic, recovered at the engine entry points
type raised struct{ err error }

// Unwinds to the nearest catch with err as the raised exception
func raise(err error) { panic(raised{err}) }

// Raises a Python exception of the given type and message
func raisef(typ, format string, args ...any) {
	raise(&PyError{Type: typ, Msg: fmt.Sprintf(format, args...)})
}

// Converts a raised exception into the returned error, leaving any other panic running
func catch(errp *error) {
	if r := recover(); r != nil {
		if x, ok := r.(raised); ok {
			*errp = x.err
			return
		}
		panic(r)
	}
}

// Runs fn and returns the exception it raised
func try(fn func()) (err error) {
	defer catch(&err)
	fn()
	return nil
}

// Reports whether v is a dict of either the JSON or the YAML value model
func isDict(v any) bool {
	switch v.(type) {
	case *pyjson.Object, map[string]any:
		return v != nil && !isNilObject(v)
	}
	return false
}

// Reports whether v is a typed nil JSON object
func isNilObject(v any) bool {
	o, ok := v.(*pyjson.Object)
	return ok && o == nil
}

// Python dict.get on v, AttributeError when v is not a dict
func dget(v any, key string, def any) any {
	switch d := v.(type) {
	case *pyjson.Object:
		if d != nil {
			if x, ok := d.Get(key); ok {
				return x
			}
			return def
		}
	case map[string]any:
		if x, ok := d[key]; ok {
			return x
		}
		return def
	}
	raisef("AttributeError", "'%s' object has no attribute 'get'", pyrepr.TypeName(v))
	return nil
}

// Python d[key] on a dict known to hold the key
func dindex(v any, key string) any {
	switch d := v.(type) {
	case *pyjson.Object:
		if x, ok := d.Get(key); ok {
			return x
		}
	case map[string]any:
		if x, ok := d[key]; ok {
			return x
		}
	case string:
		raisef("TypeError", "string indices must be integers, not 'str'")
	case []any:
		raisef("TypeError", "list indices must be integers or slices, not str")
	default:
		raisef("TypeError", "'%s' object is not subscriptable", pyrepr.TypeName(v))
	}
	raise(&PyError{Type: "KeyError", Msg: pyrepr.Repr(key)})
	return nil
}

// Python iteration of a value, dict keys, list elements or str characters, TypeError otherwise
func pyIter(v any) []any {
	switch x := v.(type) {
	case []any:
		return x
	case *pyjson.Object, map[string]any:
		items := ditems(x)
		out := make([]any, len(items))
		for i, e := range items {
			out[i] = e.Key
		}
		return out
	case string:
		var out []any
		for _, r := range x {
			out = append(out, string(r))
		}
		return out
	}
	raisef("TypeError", "'%s' object is not iterable", pyrepr.TypeName(v))
	return nil
}

// Python v.lower() on a value, AttributeError when it is not a str
func lowerAttr(v any) string {
	s, ok := v.(string)
	if !ok {
		raisef("AttributeError", "'%s' object has no attribute 'lower'", pyrepr.TypeName(v))
	}
	return lower(s)
}

// Raises the TypeError Python gives when an unhashable value is looked up in a set or dict
func requireHashable(v any) {
	switch v.(type) {
	case []any, *pyjson.Object, map[string]any:
		raisef("TypeError", "unhashable type: '%s'", pyrepr.TypeName(v))
	}
}

// Python d[key] = value, a new key appended to the insertion order
func dset(v any, key string, value any) {
	switch d := v.(type) {
	case *pyjson.Object:
		d.Set(key, value)
		return
	case map[string]any:
		_, existed := d[key]
		d[key] = value
		if !existed {
			config.AppendMappingKey(d, key)
		}
		return
	case []any:
		raisef("TypeError", "list indices must be integers or slices, not str")
	case string:
		raisef("TypeError", "'str' object does not support item assignment")
	}
	raisef("TypeError", "'%s' object does not support item assignment", pyrepr.TypeName(v))
}

// Python d.setdefault(key, {}) returning the stored value
func dsetdefaultDict(v any, key string) any {
	if !isDict(v) {
		raisef("AttributeError", "'%s' object has no attribute 'setdefault'", pyrepr.TypeName(v))
	}
	if x, ok := dlookup(v, key); ok {
		return x
	}
	fresh := newYAMLMap()
	dset(v, key, fresh)
	return fresh
}

// Value under key and whether the dict holds it
func dlookup(v any, key string) (any, bool) {
	switch d := v.(type) {
	case *pyjson.Object:
		if d != nil {
			return d.Get(key)
		}
	case map[string]any:
		x, ok := d[key]
		return x, ok
	}
	return nil, false
}

// Python key in container over dicts, lists and strings
func contains(container any, key any) bool {
	switch c := container.(type) {
	case *pyjson.Object, map[string]any:
		k, ok := key.(string)
		if !ok {
			return false
		}
		_, has := dlookup(c, k)
		return has
	case []any:
		for _, e := range c {
			if pyrepr.Equal(e, key) {
				return true
			}
		}
		return false
	case string:
		k, ok := key.(string)
		if !ok {
			raisef("TypeError", "'in <string>' requires string as left operand, not %s", pyrepr.TypeName(key))
		}
		return strings.Contains(c, k)
	}
	raisef("TypeError", "argument of type '%s' is not iterable", pyrepr.TypeName(container))
	return false
}

// One key and value of a dict in insertion order
type kv struct {
	Key string
	Val any
}

// Python d.items() in insertion order, AttributeError when v is not a dict
func ditems(v any) []kv {
	switch d := v.(type) {
	case *pyjson.Object:
		if d != nil {
			out := make([]kv, 0, len(d.Keys))
			for _, k := range d.Keys {
				out = append(out, kv{k, d.Vals[k]})
			}
			return out
		}
	case map[string]any:
		keys := config.MappingKeys(d)
		out := make([]kv, 0, len(keys))
		for _, k := range keys {
			out = append(out, kv{k, d[k]})
		}
		return out
	}
	raisef("AttributeError", "'%s' object has no attribute 'items'", pyrepr.TypeName(v))
	return nil
}

// Python len of a sized value
func pyLen(v any) int {
	switch x := v.(type) {
	case *pyjson.Object:
		return x.Len()
	case map[string]any:
		return len(x)
	case []any:
		return len(x)
	case string:
		return pystr.Len(x)
	}
	raisef("TypeError", "object of type '%s' has no len()", pyrepr.TypeName(v))
	return 0
}

// Python truth value
func truthy(v any) bool { return pyrepr.Truthy(v) }

// Python a or b
func or(a, b any) any {
	if truthy(a) {
		return a
	}
	return b
}

// Python str() of a value
func pyStr(v any) string {
	if o, ok := v.(*pyjson.Object); ok {
		return pyrepr.Repr(o)
	}
	return pyrepr.Str(v)
}

// Python str.strip
func strip(s string) string { return pystr.Strip(s) }

// Python str.lower
func lower(s string) string { return pystr.Lower(s) }

// Python str.upper
func upper(s string) string { return pystr.Upper(s) }

// Python str.islower, true with at least one cased character and none uppercase or titlecase
func isLower(s string) bool {
	cased := false
	for _, r := range s {
		if unicode.IsUpper(r) || unicode.IsTitle(r) {
			return false
		}
		if unicode.IsLower(r) {
			cased = true
		}
	}
	return cased
}

// Python value of v == s for a str literal s
func eqStr(v any, s string) bool {
	x, ok := v.(string)
	return ok && x == s
}

// Python v is False
func isFalse(v any) bool {
	b, ok := v.(bool)
	return ok && !b
}

// Fresh YAML mapping
func newYAMLMap() map[string]any { return map[string]any{} }

// Deep copy of a JSON value into the YAML value model, insertion order kept on every mapping
func toYAML(v any) any {
	switch x := v.(type) {
	case *pyjson.Object:
		if x == nil {
			return nil
		}
		m := make(map[string]any, len(x.Keys))
		for _, k := range x.Keys {
			m[k] = toYAML(x.Vals[k])
		}
		config.SetMappingKeys(m, x.Keys)
		return m
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = toYAML(e)
		}
		return out
	}
	return v
}

// Python int() of a config value with the CPython errors
func pyInt(v any) (int, error) {
	switch x := v.(type) {
	case bool:
		if x {
			return 1, nil
		}
		return 0, nil
	case string:
		n, ok := pystr.IntText(x)
		if !ok {
			return 0, &PyError{Type: "ValueError", Msg: "invalid literal for int() with base 10: " + pyrepr.Repr(x)}
		}
		if !n.IsInt64() {
			return 0, &PyError{Type: "OverflowError", Msg: "Python int too large to convert to C ssize_t"}
		}
		return int(n.Int64()), nil
	case float64:
		if math.IsNaN(x) {
			return 0, &PyError{Type: "ValueError", Msg: "cannot convert float NaN to integer"}
		}
		if math.IsInf(x, 0) {
			return 0, &PyError{Type: "OverflowError", Msg: "cannot convert float infinity to integer"}
		}
		return int(math.Trunc(x)), nil
	}
	if n, ok := pyrepr.Int(v); ok {
		if !n.IsInt64() {
			return 0, &PyError{Type: "OverflowError", Msg: "Python int too large to convert to C ssize_t"}
		}
		return int(n.Int64()), nil
	}
	return 0, &PyError{Type: "TypeError", Msg: fmt.Sprintf("int() argument must be a string, a bytes-like object or a real number, not '%s'", pyrepr.TypeName(v))}
}

// Numeric value of an int, bool or float for arithmetic, false for any other type
func pyNumber(v any) (*big.Int, float64, bool, bool) {
	switch x := v.(type) {
	case bool:
		if x {
			return big.NewInt(1), 0, false, true
		}
		return big.NewInt(0), 0, false, true
	case float64:
		return nil, x, true, true
	}
	if n, ok := pyrepr.Int(v); ok {
		return n, 0, false, true
	}
	return nil, 0, false, false
}

// Python min(v // 10, 200), Euclidean division by a positive divisor being floor division
func floorDiv10Min200(v any) any {
	n, f, isFloat, ok := pyNumber(v)
	if !ok {
		raisef("TypeError", "unsupported operand type(s) for //: '%s' and 'int'", pyrepr.TypeName(v))
	}
	if isFloat {
		q := math.Floor(f / 10)
		if math.IsInf(f, 0) {
			q = math.NaN()
		}
		if 200 < q {
			return int64(200)
		}
		return q
	}
	q, m := new(big.Int), new(big.Int)
	q.DivMod(n, big.NewInt(10), m)
	if q.Cmp(big.NewInt(200)) > 0 {
		return int64(200)
	}
	if q.IsInt64() {
		return q.Int64()
	}
	return q
}

// Python list(dict.fromkeys(values)), TypeError for an unhashable element
func uniqueInOrder(values []any) []any {
	out := make([]any, 0, len(values))
	for _, v := range values {
		switch v.(type) {
		case []any, *pyjson.Object, map[string]any:
			raisef("TypeError", "unhashable type: '%s'", pyrepr.TypeName(v))
		}
		dup := false
		for _, seen := range out {
			if pyrepr.Equal(seen, v) {
				dup = true
				break
			}
		}
		if !dup {
			out = append(out, v)
		}
	}
	return out
}

// Python sorted over str or numeric values, TypeError for a mix
func pySorted(values []any) []any {
	out := append([]any(nil), values...)
	allStr := true
	for _, v := range out {
		if _, ok := v.(string); !ok {
			allStr = false
		}
	}
	if allStr {
		sort.SliceStable(out, func(i, j int) bool { return out[i].(string) < out[j].(string) })
		return out
	}
	for i := range out {
		if _, _, _, ok := pyNumber(out[i]); !ok {
			for j := range out {
				if pyrepr.TypeName(out[j]) != pyrepr.TypeName(out[i]) {
					raisef("TypeError", "'<' not supported between instances of '%s' and '%s'", pyrepr.TypeName(out[j]), pyrepr.TypeName(out[i]))
				}
			}
			raisef("TypeError", "'<' not supported between instances of '%s' and '%s'", pyrepr.TypeName(out[i]), pyrepr.TypeName(out[i]))
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return numLess(out[i], out[j]) })
	return out
}

// Python a < b over ints, bools and floats
func numLess(a, b any) bool {
	an, af, aFloat, _ := pyNumber(a)
	bn, bf, bFloat, _ := pyNumber(b)
	if !aFloat && !bFloat {
		return an.Cmp(bn) < 0
	}
	if !aFloat {
		af, _ = new(big.Float).SetInt(an).Float64()
	}
	if !bFloat {
		bf, _ = new(big.Float).SetInt(bn).Float64()
	}
	return af < bf
}

// Text of an exception as Python str(exc) renders it
func excText(err error) string {
	if err == nil {
		return ""
	}
	var pe *PyError
	if errors.As(err, &pe) {
		return pe.Msg
	}
	return osErrText(err)
}
