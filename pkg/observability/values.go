package observability

import (
	"encoding/json"
	"math"
	"math/big"
	"sort"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// ValueSet is a closed vocabulary of string values one dimension may hold
type ValueSet map[string]struct{}

// allowedValues is any closed enum, static or catalog backed
type allowedValues interface {
	Has(value string) bool
}

// Builds a set holding exactly the given values
func Set(values ...string) ValueSet {
	out := make(ValueSet, len(values))
	for _, v := range values {
		out[v] = struct{}{}
	}
	return out
}

// Reports whether the value belongs to the set
func (s ValueSet) Has(value string) bool {
	_, ok := s[value]
	return ok
}

// Returns a new set holding this set plus every other set and loose value
func (s ValueSet) Union(others ...ValueSet) ValueSet {
	out := make(ValueSet, len(s))
	for v := range s {
		out[v] = struct{}{}
	}
	for _, o := range others {
		for v := range o {
			out[v] = struct{}{}
		}
	}
	return out
}

// Returns a new set holding this set without the given values
func (s ValueSet) Minus(values ...string) ValueSet {
	out := s.Union()
	for _, v := range values {
		delete(out, v)
	}
	return out
}

// Returns a new set holding this set without every member of the others
func (s ValueSet) Difference(others ...ValueSet) ValueSet {
	out := s.Union()
	for _, o := range others {
		for v := range o {
			delete(out, v)
		}
	}
	return out
}

// Reports whether both sets hold exactly the same values
func (s ValueSet) Equal(o ValueSet) bool {
	if len(s) != len(o) {
		return false
	}
	for v := range s {
		if !o.Has(v) {
			return false
		}
	}
	return true
}

// Members in ascending order
func (s ValueSet) Sorted() []string {
	out := make([]string, 0, len(s))
	for v := range s {
		out = append(out, v)
	}
	sort.Strings(out)
	return out
}

// Set of a bool map's true keys, the shape pkg/tools exports its name registries in
func setOfKeys(m map[string]bool) ValueSet {
	out := make(ValueSet, len(m))
	for k, ok := range m {
		if ok {
			out[k] = struct{}{}
		}
	}
	return out
}

// Set of a map's keys
func keysOf[V any](m map[string]V) ValueSet {
	out := make(ValueSet, len(m))
	for k := range m {
		out[k] = struct{}{}
	}
	return out
}

// RelayEvent is one Relay mark or scope event as the subscriber receives it, nil reading as None
type RelayEvent struct {
	Kind            any
	Name            any
	Category        any
	ScopeCategory   any
	CategoryProfile any
	Metadata        any
	Data            any
}

// Python str(value or "") of one loosely typed value
func pyText(value any) string {
	if !pyrepr.Truthy(value) {
		return ""
	}
	if s, ok := value.(string); ok {
		return s
	}
	return pyrepr.Str(value)
}

// Stripped lower case text of a value, the shared normalization of every enum producer
func norm(value any) string {
	return pystr.Lower(pystr.Strip(pyText(value)))
}

// The normalized value when allowed, unknown otherwise
func allowlisted(normalized string, allowed allowedValues) string {
	if allowed.Has(normalized) {
		return normalized
	}
	return "unknown"
}

// threshold is one bucket boundary, values below upper read label
type threshold struct {
	upper float64
	label string
}

// First label whose upper bound exceeds the value, last when none does
func bucket(value float64, thresholds []threshold, last string) string {
	for _, t := range thresholds {
		if value < t.upper {
			return t.label
		}
	}
	return last
}

// Dict view of a decoded JSON object or a plain map
func asDict(v any) (map[string]any, bool) {
	switch d := v.(type) {
	case map[string]any:
		return d, true
	case *pyjson.Object:
		if d == nil {
			return nil, false
		}
		return d.Vals, true
	}
	return nil, false
}

// Python int value of a non bool integer, big ints included
func pyInt(v any) (*big.Int, bool) {
	if _, isBool := v.(bool); isBool {
		return nil, false
	}
	if n, ok := v.(json.Number); ok {
		if _, err := n.Int64(); err != nil {
			if _, ok := new(big.Int).SetString(string(n), 10); !ok {
				return nil, false
			}
		}
	}
	return pyrepr.Int(v)
}

// Python float(value) of a non bool int or float, false for anything else or an overflowing int
func pyNumber(v any) (float64, bool) {
	switch n := v.(type) {
	case bool:
		return 0, false
	case float64:
		return n, true
	case float32:
		return float64(n), true
	case json.Number:
		if i, ok := pyInt(n); ok {
			return bigFloat(i)
		}
		f, err := n.Float64()
		return f, err == nil
	}
	if i, ok := pyInt(v); ok {
		return bigFloat(i)
	}
	return 0, false
}

// float(int) with CPython's OverflowError for ints beyond the double range
func bigFloat(i *big.Int) (float64, bool) {
	f, _ := new(big.Float).SetInt(i).Float64()
	if math.IsInf(f, 0) {
		return 0, false
	}
	return f, true
}

// Finite non negative number of a value, false for bools, non numbers and NaN or infinity
func nonNegativeNumber(value any) (float64, bool) {
	number, ok := pyNumber(value)
	if !ok || math.IsNaN(number) || math.IsInf(number, 0) || number < 0 {
		return 0, false
	}
	return number, true
}

// Python max(0, int(count)) over an int count
func clampCount(count int) float64 {
	if count < 0 {
		return 0
	}
	return float64(count)
}

// Python time.time() as float seconds
func pyTime() float64 { return float64(time.Now().UnixNano()) / 1e9 }

// Python truth value of a loosely typed value
func pyTruthy(v any) bool { return pyrepr.Truthy(v) }

// Keys of a decoded dict in insertion order, sorted for a plain map that keeps none
func pyDictKeys(raw any, d map[string]any) []string {
	if obj, ok := raw.(*pyjson.Object); ok && obj != nil {
		return obj.Keys
	}
	keys := make([]string, 0, len(d))
	for key := range d {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
