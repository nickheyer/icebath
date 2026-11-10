package pm

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"

	"github.com/pelletier/go-toml/v2"
	"github.com/pelletier/go-toml/v2/unstable"
)

// TOMLDecodeError is a document tomllib refuses, a ValueError in Python
type TOMLDecodeError struct{ Message string }

func (e *TOMLDecodeError) Error() string { return e.Message }

// Parses TOML like tomllib loads, every table an ordered object keeping document key order
func tomlLoads(text string) (*OrderedObject, error) {
	var plain map[string]any
	if err := toml.Unmarshal([]byte(text), &plain); err != nil {
		return nil, &TOMLDecodeError{Message: err.Error()}
	}
	order := tomlKeyOrder([]byte(text))
	return orderTOML(plain, "", order).(*OrderedObject), nil
}

// TOMLLoads exports tomlLoads
func TOMLLoads(text string) (*OrderedObject, error) { return tomlLoads(text) }

// First appearance order of every table's keys, keyed by table path with array elements indexed
func tomlKeyOrder(data []byte) map[string][]string {
	order := map[string][]string{}
	seen := map[string]bool{}
	counts := map[string]int{}
	record := func(parent, key string) string {
		path := parent + "\x00" + key
		if !seen[path] {
			seen[path] = true
			order[parent] = append(order[parent], key)
		}
		return path
	}
	descend := func(parent, key string) string {
		path := record(parent, key)
		if n := counts[path]; n > 0 {
			path += "\x01" + strconv.Itoa(n-1)
		}
		return path
	}
	var walkValue func(path string, v *unstable.Node)
	walkKeyValue := func(base string, kv *unstable.Node) {
		path := base
		keys := kv.Key()
		for keys.Next() {
			path = descend(path, string(keys.Node().Data))
		}
		walkValue(path, kv.Value())
	}
	walkValue = func(path string, v *unstable.Node) {
		switch v.Kind {
		case unstable.InlineTable:
			children := v.Children()
			for children.Next() {
				walkKeyValue(path, children.Node())
			}
		case unstable.Array:
			children := v.Children()
			for i := 0; children.Next(); i++ {
				walkValue(path+"\x01"+strconv.Itoa(i), children.Node())
			}
		}
	}
	var p unstable.Parser
	p.Reset(data)
	current := ""
	for p.NextExpression() {
		e := p.Expression()
		switch e.Kind {
		case unstable.Table:
			current = ""
			keys := e.Key()
			for keys.Next() {
				current = descend(current, string(keys.Node().Data))
			}
		case unstable.ArrayTable:
			var parts []string
			keys := e.Key()
			for keys.Next() {
				parts = append(parts, string(keys.Node().Data))
			}
			current = ""
			for _, k := range parts[:len(parts)-1] {
				current = descend(current, k)
			}
			path := record(current, parts[len(parts)-1])
			counts[path]++
			current = path + "\x01" + strconv.Itoa(counts[path]-1)
		case unstable.KeyValue:
			walkKeyValue(current, e)
		}
	}
	return order
}

// Rebuilds decoded tables as ordered objects following the recorded key order
func orderTOML(v any, path string, order map[string][]string) any {
	switch t := v.(type) {
	case map[string]any:
		obj := NewOrderedObject()
		for _, k := range order[path] {
			if child, ok := t[k]; ok {
				obj.Set(k, orderTOML(child, path+"\x00"+k, order))
			}
		}
		rest := sortedKeys(t)
		for _, k := range rest {
			if _, ok := obj.Values[k]; !ok {
				obj.Set(k, orderTOML(t[k], path+"\x00"+k, order))
			}
		}
		return obj
	case []any:
		out := make([]any, len(t))
		for i, e := range t {
			out[i] = orderTOML(e, path+"\x01"+strconv.Itoa(i), order)
		}
		return out
	}
	return v
}

// Python repr of a value tomllib produces
func tomlRepr(v any) string {
	switch t := v.(type) {
	case string:
		return pyrepr.StrRepr(t)
	case bool:
		if t {
			return "True"
		}
		return "False"
	case int64:
		return strconv.FormatInt(t, 10)
	case float64:
		switch {
		case math.IsInf(t, 1):
			return "inf"
		case math.IsInf(t, -1):
			return "-inf"
		case math.IsNaN(t):
			return "nan"
		}
		return pyrepr.Float(t)
	case []any:
		parts := make([]string, len(t))
		for i, e := range t {
			parts[i] = tomlRepr(e)
		}
		return "[" + strings.Join(parts, ", ") + "]"
	case *OrderedObject:
		parts := make([]string, len(t.Keys))
		for i, k := range t.Keys {
			parts[i] = pyrepr.StrRepr(k) + ": " + tomlRepr(t.Values[k])
		}
		return "{" + strings.Join(parts, ", ") + "}"
	case toml.LocalDate:
		return fmt.Sprintf("datetime.date(%d, %d, %d)", t.Year, t.Month, t.Day)
	case toml.LocalTime:
		return "datetime.time(" + clockRepr(t.Hour, t.Minute, t.Second, t.Nanosecond/1000) + ")"
	case toml.LocalDateTime:
		return fmt.Sprintf("datetime.datetime(%d, %d, %d, %s)", t.Year, t.Month, t.Day, clockRepr(t.Hour, t.Minute, t.Second, t.Nanosecond/1000))
	case time.Time:
		_, offset := t.Zone()
		zone := "datetime.timezone.utc"
		if offset != 0 {
			zone = "datetime.timezone(" + timedeltaRepr(offset) + ")"
		}
		clock := clockRepr(t.Hour(), t.Minute(), t.Second(), t.Nanosecond()/1000)
		return fmt.Sprintf("datetime.datetime(%d, %d, %d, %s, tzinfo=%s)", t.Year(), int(t.Month()), t.Day(), clock, zone)
	}
	return fmt.Sprint(v)
}

// Clock fields of a time or datetime repr, trailing zero second and microsecond fields omitted
func clockRepr(hour, minute, second, micro int) string {
	switch {
	case micro != 0:
		return fmt.Sprintf("%d, %d, %d, %d", hour, minute, second, micro)
	case second != 0:
		return fmt.Sprintf("%d, %d, %d", hour, minute, second)
	}
	return fmt.Sprintf("%d, %d", hour, minute)
}

// Python repr of a timedelta of whole seconds, normalized to days and nonnegative seconds
func timedeltaRepr(seconds int) string {
	days := seconds / 86400
	rest := seconds % 86400
	if rest < 0 {
		days--
		rest += 86400
	}
	var parts []string
	if days != 0 {
		parts = append(parts, fmt.Sprintf("days=%d", days))
	}
	if rest != 0 {
		parts = append(parts, fmt.Sprintf("seconds=%d", rest))
	}
	if len(parts) == 0 {
		return "datetime.timedelta(0)"
	}
	return "datetime.timedelta(" + strings.Join(parts, ", ") + ")"
}

// Python type name of a tomllib value
func tomlTypeName(v any) string {
	switch v.(type) {
	case *OrderedObject:
		return "dict"
	case []any:
		return "list"
	case string:
		return "str"
	case int64:
		return "int"
	case float64:
		return "float"
	case bool:
		return "bool"
	case toml.LocalDate:
		return "datetime.date"
	case toml.LocalTime:
		return "datetime.time"
	}
	return "datetime.datetime"
}

// Value of key in a decoded table the way dict get with a default reads it, refusing a non table
func tomlGet(v any, key string, fallback any) (any, error) {
	obj, ok := v.(*OrderedObject)
	if !ok {
		return nil, &PyError{Class: "AttributeError", Message: fmt.Sprintf("'%s' object has no attribute 'get'", tomlTypeName(v))}
	}
	if value, ok := obj.Values[key]; ok {
		return value, nil
	}
	return fallback, nil
}

// Value of key in a decoded table the way subscripting reads it, a KeyError when absent
func tomlIndex(v any, key string) (any, error) {
	obj, ok := v.(*OrderedObject)
	if !ok {
		if _, isList := v.([]any); isList {
			return nil, &PyError{Class: "TypeError", Message: "list indices must be integers or slices, not str"}
		}
		if _, isStr := v.(string); isStr {
			return nil, &PyError{Class: "TypeError", Message: "string indices must be integers, not 'str'"}
		}
		return nil, &PyError{Class: "TypeError", Message: fmt.Sprintf("'%s' object is not subscriptable", tomlTypeName(v))}
	}
	value, ok := obj.Values[key]
	if !ok {
		return nil, &PyError{Class: "KeyError", Message: pyrepr.StrRepr(key)}
	}
	return value, nil
}

// Elements of a decoded value iterated the way a for loop iterates it
func tomlIter(v any) ([]any, error) {
	switch t := v.(type) {
	case []any:
		return t, nil
	case *OrderedObject:
		out := make([]any, len(t.Keys))
		for i, k := range t.Keys {
			out[i] = k
		}
		return out, nil
	case string:
		var out []any
		for _, r := range t {
			out = append(out, string(r))
		}
		return out, nil
	}
	return nil, &PyError{Class: "TypeError", Message: fmt.Sprintf("'%s' object is not iterable", tomlTypeName(v))}
}

// Python equality of two tomllib values, bools and ints and floats comparing as numbers
func tomlEqual(a, b any) bool {
	if equal, numeric := tomlNumbersEqual(a, b); numeric {
		return equal
	}
	switch x := a.(type) {
	case string:
		y, ok := b.(string)
		return ok && x == y
	case []any:
		y, ok := b.([]any)
		if !ok || len(x) != len(y) {
			return false
		}
		for i := range x {
			if !tomlEqual(x[i], y[i]) {
				return false
			}
		}
		return true
	case *OrderedObject:
		y, ok := b.(*OrderedObject)
		if !ok || len(x.Keys) != len(y.Keys) {
			return false
		}
		for _, key := range x.Keys {
			other, found := y.Values[key]
			if !found || !tomlEqual(x.Values[key], other) {
				return false
			}
		}
		return true
	case toml.LocalDate:
		y, ok := b.(toml.LocalDate)
		return ok && x == y
	case toml.LocalTime:
		y, ok := b.(toml.LocalTime)
		return ok && tomlClockEqual(x, y)
	case toml.LocalDateTime:
		y, ok := b.(toml.LocalDateTime)
		return ok && x.LocalDate == y.LocalDate && tomlClockEqual(x.LocalTime, y.LocalTime)
	case time.Time:
		y, ok := b.(time.Time)
		return ok && x.Unix() == y.Unix() && x.Nanosecond()/1000 == y.Nanosecond()/1000
	}
	return false
}

// Equality of two clock times at the microsecond resolution tomllib keeps
func tomlClockEqual(a, b toml.LocalTime) bool {
	return a.Hour == b.Hour && a.Minute == b.Minute && a.Second == b.Second && a.Nanosecond/1000 == b.Nanosecond/1000
}

// Python numeric equality of two values, numeric false when either is not a bool, int or float
func tomlNumbersEqual(a, b any) (equal, numeric bool) {
	ai, af, aFloat, aOK := tomlNumber(a)
	bi, bf, bFloat, bOK := tomlNumber(b)
	if !aOK || !bOK {
		return false, false
	}
	switch {
	case !aFloat && !bFloat:
		return ai == bi, true
	case aFloat && bFloat:
		return af == bf, true
	case aFloat:
		return tomlFloatIsInt(af, bi), true
	}
	return tomlFloatIsInt(bf, ai), true
}

// Integer or float view of a numeric tomllib value, a bool counting as the int it subclasses
func tomlNumber(v any) (i int64, f float64, isFloat, ok bool) {
	switch x := v.(type) {
	case bool:
		if x {
			return 1, 0, false, true
		}
		return 0, 0, false, true
	case int64:
		return x, 0, false, true
	case float64:
		return 0, x, true, true
	}
	return 0, 0, false, false
}

// Exact comparison of a float with an int the way Python compares them without rounding the int
func tomlFloatIsInt(f float64, i int64) bool {
	if math.IsNaN(f) || math.IsInf(f, 0) || f != math.Trunc(f) || f < -9.223372036854775808e18 || f >= 9.223372036854775808e18 {
		return false
	}
	return int64(f) == i
}

// Python in operator over a tomllib container, a TypeError for a value that is no container
func tomlContains(container any, item string) (bool, error) {
	switch c := container.(type) {
	case *OrderedObject:
		_, found := c.Values[item]
		return found, nil
	case []any:
		for _, element := range c {
			if tomlEqual(element, item) {
				return true, nil
			}
		}
		return false, nil
	case string:
		return strings.Contains(c, item), nil
	}
	return false, &PyError{Class: "TypeError", Message: fmt.Sprintf("argument of type '%s' is not a container or iterable", tomlTypeName(container))}
}

// TypeError a set raises for an unhashable tomllib value, nil for a hashable one
func tomlUnhashable(v any) error {
	switch v.(type) {
	case *OrderedObject, []any:
		return &PyError{Class: "TypeError", Message: fmt.Sprintf("cannot use '%s' as a set element (unhashable type: '%s')", tomlTypeName(v), tomlTypeName(v))}
	}
	return nil
}
