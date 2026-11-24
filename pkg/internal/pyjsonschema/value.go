package pyjsonschema

import (
	"encoding/json"
	"fmt"
	"math"
	"math/big"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

// Deep copy in the Python value model, dicts ordered, ints as int64 or big.Int and floats as float64
func pyValue(v any) any {
	switch x := v.(type) {
	case nil, bool, string, int64, float64:
		return x
	case *big.Int:
		if x == nil {
			return nil
		}
		if x.IsInt64() {
			return x.Int64()
		}
		return new(big.Int).Set(x)
	case float32:
		return float64(x)
	case json.Number:
		s := string(x)
		if strings.ContainsAny(s, ".eE") {
			f, _ := strconv.ParseFloat(s, 64)
			return f
		}
		n, ok := new(big.Int).SetString(s, 10)
		if !ok {
			return s
		}
		return pyValue(n)
	case []any:
		out := make([]any, len(x))
		for i, item := range x {
			out[i] = pyValue(item)
		}
		return out
	case *pyjson.Object:
		if x == nil {
			return nil
		}
		o := &pyjson.Object{Keys: make([]string, 0, len(x.Keys)), Vals: make(map[string]any, len(x.Keys))}
		for _, k := range x.Keys {
			o.Keys = append(o.Keys, k)
			o.Vals[k] = pyValue(x.Vals[k])
		}
		return o
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		o := &pyjson.Object{Keys: keys, Vals: make(map[string]any, len(x))}
		for _, k := range keys {
			o.Vals[k] = pyValue(x[k])
		}
		return o
	case pyrepr.Mapping:
		o := pyjson.NewObject()
		for k, val := range x.PyItems() {
			o.Set(pyrepr.Str(k), pyValue(val))
		}
		return o
	}
	if n, ok := pyrepr.Int(v); ok {
		return pyValue(n)
	}
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.Float32, reflect.Float64:
		return rv.Float()
	case reflect.Slice, reflect.Array:
		out := make([]any, rv.Len())
		for i := range out {
			out[i] = pyValue(rv.Index(i).Interface())
		}
		return out
	case reflect.Map:
		if rv.Type().Key().Kind() == reflect.String {
			m := make(map[string]any, rv.Len())
			for _, k := range rv.MapKeys() {
				m[k.String()] = rv.MapIndex(k).Interface()
			}
			return pyValue(m)
		}
	}
	return v
}

// Python type name of a value of the model
func typeName(v any) string {
	switch v.(type) {
	case *pyjson.Object:
		return "dict"
	case []any:
		return "list"
	case unboolSentinel:
		return "object"
	}
	return pyrepr.TypeName(v)
}

func repr(v any) string { return pyrepr.Repr(v) }

func isDict(v any) bool {
	o, ok := v.(*pyjson.Object)
	return ok && o != nil
}

func asDict(v any) *pyjson.Object {
	o, _ := v.(*pyjson.Object)
	return o
}

func isList(v any) bool {
	_, ok := v.([]any)
	return ok
}

func isStr(v any) bool {
	_, ok := v.(string)
	return ok
}

func isBool(v any) bool {
	_, ok := v.(bool)
	return ok
}

// Membership test of key in a dict the way the in operator runs on a dict of str keys
func dictHas(d *pyjson.Object, key any) bool {
	switch k := key.(type) {
	case string:
		_, ok := d.Vals[k]
		return ok
	case []any, *pyjson.Object:
		raise("TypeError", "unhashable type: '"+typeName(k)+"'")
	}
	return false
}

// Python len of a str, list or dict, TypeError for the rest
func pyLen(v any) int {
	switch x := v.(type) {
	case string:
		return utf8.RuneCountInString(x)
	case []any:
		return len(x)
	case *pyjson.Object:
		return len(x.Keys)
	}
	raise("TypeError", "object of type '"+typeName(v)+"' has no len()")
	return 0
}

// Reports an int or float that is not a bool
func isNumber(v any) bool {
	switch v.(type) {
	case int64, *big.Int, float64:
		return true
	}
	return false
}

func isIntValue(v any) bool {
	switch v.(type) {
	case int64, *big.Int:
		return true
	}
	return false
}

func isNaN(v any) bool {
	f, ok := v.(float64)
	return ok && math.IsNaN(f)
}

// Integer value of an int or bool
func bigOf(v any) *big.Int {
	switch x := v.(type) {
	case int:
		return big.NewInt(int64(x))
	case int64:
		return big.NewInt(x)
	case *big.Int:
		return x
	case bool:
		if x {
			return big.NewInt(1)
		}
		return big.NewInt(0)
	}
	return nil
}

// Numeric operand of a bool, int or float
func numeric(v any) bool {
	switch v.(type) {
	case int, int64, *big.Int, float64, bool:
		return true
	}
	return false
}

// Exact comparison of two numbers, false when a NaN makes them unordered
func numCmp(a, b any) (int, bool) {
	fa, aFloat := a.(float64)
	fb, bFloat := b.(float64)
	switch {
	case aFloat && bFloat:
		if math.IsNaN(fa) || math.IsNaN(fb) {
			return 0, false
		}
		switch {
		case fa < fb:
			return -1, true
		case fa > fb:
			return 1, true
		}
		return 0, true
	case aFloat:
		c, ok := intFloatCmp(bigOf(b), fa)
		return -c, ok
	case bFloat:
		return intFloatCmp(bigOf(a), fb)
	}
	if x, ok := a.(int64); ok {
		if y, ok := b.(int64); ok {
			switch {
			case x < y:
				return -1, true
			case x > y:
				return 1, true
			}
			return 0, true
		}
	}
	return bigOf(a).Cmp(bigOf(b)), true
}

// Sign of int minus float compared exactly
func intFloatCmp(i *big.Int, f float64) (int, bool) {
	if math.IsNaN(f) {
		return 0, false
	}
	if math.IsInf(f, 1) {
		return -1, true
	}
	if math.IsInf(f, -1) {
		return 1, true
	}
	return new(big.Float).SetInt(i).Cmp(new(big.Float).SetFloat64(f)), true
}

func numLess(a, b any) bool {
	c, ok := numCmp(a, b)
	return ok && c < 0
}

func numEq(a, b any) bool {
	c, ok := numCmp(a, b)
	return ok && c == 0
}

// Stand in object jsonschema unbool puts in place of True and False
type unboolSentinel struct{ v bool }

func unbool(v any) any {
	if b, ok := v.(bool); ok {
		return unboolSentinel{b}
	}
	return v
}

// jsonschema _utils.equal, bools kept apart from ints at every depth, with the frames of its recursion
func equal(st *state, one, two any) bool {
	defer st.call()()
	if pyIs(one, two) {
		return true
	}
	s1, ok1 := one.(string)
	s2, ok2 := two.(string)
	if ok1 || ok2 {
		return ok1 && ok2 && s1 == s2
	}
	st.touch(1)
	l1, ok1 := one.([]any)
	l2, ok2 := two.([]any)
	if ok1 && ok2 {
		st.enter(1)
		defer st.leave(1)
		if len(l1) != len(l2) {
			return false
		}
		st.enter(1)
		defer st.leave(1)
		for i := range l1 {
			if !equal(st, l1[i], l2[i]) {
				return false
			}
		}
		return true
	}
	d1, ok1 := one.(*pyjson.Object)
	d2, ok2 := two.(*pyjson.Object)
	if ok1 && ok2 {
		st.enter(1)
		defer st.leave(1)
		if len(d1.Keys) != len(d2.Keys) {
			return false
		}
		st.enter(1)
		defer st.leave(1)
		for _, k := range d1.Keys {
			v2, ok := d2.Vals[k]
			if !ok || !equal(st, d1.Vals[k], v2) {
				return false
			}
		}
		return true
	}
	st.touch(1)
	return pyEq(unbool(one), unbool(two))
}

// Python identity of two decoded values, the singletons, small ints, short str and json constants CPython shares
func pyIs(a, b any) bool {
	switch x := a.(type) {
	case nil:
		return b == nil
	case bool:
		y, ok := b.(bool)
		return ok && x == y
	case int64:
		y, ok := b.(int64)
		return ok && x == y && x >= -5 && x <= 256
	case string:
		y, ok := b.(string)
		if !ok || x != y {
			return false
		}
		rs := []rune(x)
		return len(rs) == 0 || len(rs) == 1 && rs[0] < 256
	case float64:
		y, ok := b.(float64)
		return ok && (math.IsNaN(x) && math.IsNaN(y) || math.IsInf(x, 0) && x == y)
	}
	return false
}

// Python == over the model, a NaN equal to itself as the shared json NaN object is
func pyEq(a, b any) bool {
	if isNaN(a) && isNaN(b) {
		return true
	}
	if sa, ok := a.(unboolSentinel); ok {
		sb, ok := b.(unboolSentinel)
		return ok && sa == sb
	}
	if _, ok := b.(unboolSentinel); ok {
		return false
	}
	if numeric(a) && numeric(b) {
		return numEq(a, b)
	}
	switch x := a.(type) {
	case nil:
		return b == nil
	case string:
		y, ok := b.(string)
		return ok && x == y
	case []any:
		y, ok := b.([]any)
		if !ok || len(x) != len(y) {
			return false
		}
		for i := range x {
			if !pyEq(x[i], y[i]) {
				return false
			}
		}
		return true
	case *pyjson.Object:
		y, ok := b.(*pyjson.Object)
		if !ok || len(x.Keys) != len(y.Keys) {
			return false
		}
		for _, k := range x.Keys {
			v2, ok := y.Vals[k]
			if !ok || !pyEq(x.Vals[k], v2) {
				return false
			}
		}
		return true
	}
	return false
}

// Python < over the model, a TypeError for operands without an ordering
func pyLess(a, b any) bool {
	if numeric(a) && numeric(b) {
		return numLess(a, b)
	}
	if sa, ok := a.(string); ok {
		if sb, ok := b.(string); ok {
			return sa < sb
		}
	}
	if la, ok := a.([]any); ok {
		if lb, ok := b.([]any); ok {
			i := 0
			for ; i < len(la) && i < len(lb); i++ {
				if !pyEq(la[i], lb[i]) {
					break
				}
			}
			if i >= len(la) || i >= len(lb) {
				return len(la) < len(lb)
			}
			return pyLess(la[i], lb[i])
		}
	}
	raise("TypeError", fmt.Sprintf("'<' not supported between instances of '%s' and '%s'", typeName(a), typeName(b)))
	return false
}

// jsonschema _utils.uniq, a sorted adjacency scan that falls back to pairwise checks on a TypeError
func uniq(st *state, container []any) bool {
	defer st.call()()
	if unique, sorted := uniqSorted(st, container); sorted {
		return unique
	}
	var seen []any
	for _, e := range container {
		st.touch(1)
		e = unbool(e)
		for _, i := range seen {
			if equal(st, i, e) {
				return false
			}
		}
		seen = append(seen, e)
	}
	return true
}

// Sorted adjacency scan, second result false when sorting raised a TypeError
func uniqSorted(st *state, container []any) (unique, sorted bool) {
	keys := make([]any, len(container))
	if len(container) > 0 {
		st.touch(2)
	} else {
		st.touch(1)
	}
	for i, e := range container {
		keys[i] = unbool(e)
	}
	if !listSort(keys) {
		return false, false
	}
	for i := 0; i+1 < len(keys); i++ {
		if equal(st, keys[i], keys[i+1]) {
			return false, true
		}
	}
	return true, true
}

// Python str of a value as an f-string field renders it
func pyStr(v any) string { return pyrepr.Str(v) }
