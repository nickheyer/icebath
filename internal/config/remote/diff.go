package remote

import (
	"maps"
	"math"
	"math/big"
	"slices"
	"strconv"
	"strings"
)

// Key and the value a write sets there
type pathValue struct {
	path  Path
	value any
}

// Values a write sets by key, in the order they were first set
type pathMap struct {
	entries []pathValue
	index   map[string]int
}

func pathKey(p Path) string {
	var b strings.Builder
	for _, s := range p {
		b.WriteString(strconv.Itoa(len(s)))
		b.WriteByte(':')
		b.WriteString(s)
	}
	return b.String()
}

func (m *pathMap) set(p Path, v any) {
	if m.index == nil {
		m.index = map[string]int{}
	}
	k := pathKey(p)
	if i, ok := m.index[k]; ok {
		m.entries[i].value = v
		return
	}
	m.index[k] = len(m.entries)
	m.entries = append(m.entries, pathValue{append(Path{}, p...), v})
}

func (m *pathMap) get(p Path) (any, bool) {
	if m.index == nil {
		return nil, false
	}
	i, ok := m.index[pathKey(p)]
	if !ok {
		return nil, false
	}
	return m.entries[i].value, true
}

func (m *pathMap) has(p Path) bool {
	_, ok := m.get(p)
	return ok
}

func (m *pathMap) update(o *pathMap) {
	for _, e := range o.entries {
		m.set(e.path, e.value)
	}
}

func sorted(m map[string]any) []string {
	return slices.Sorted(maps.Keys(m))
}

// Whether two JSON values are the same: numbers by value, a bool never a number
func jsonEqual(a, b any) bool {
	ab, aBool := a.(bool)
	bb, bBool := b.(bool)
	if aBool || bBool {
		return aBool && bBool && ab == bb
	}
	if an, ok := number(a); ok {
		bn, ok := number(b)
		return ok && numberEqual(an, bn)
	}
	switch x := a.(type) {
	case map[string]any:
		y, ok := b.(map[string]any)
		if !ok || len(x) != len(y) {
			return false
		}
		for k, v := range x {
			w, ok := y[k]
			if !ok || !jsonEqual(v, w) {
				return false
			}
		}
		return true
	case []any:
		y, ok := b.([]any)
		return ok && slices.EqualFunc(x, y, jsonEqual)
	case string:
		y, ok := b.(string)
		return ok && x == y
	case nil:
		return b == nil
	}
	return false
}

// Number as an exact integer or a float
func number(v any) (any, bool) {
	switch n := v.(type) {
	case int:
		return big.NewInt(int64(n)), true
	case int64:
		return big.NewInt(n), true
	case uint64:
		return new(big.Int).SetUint64(n), true
	case float64:
		if n == math.Trunc(n) && !math.IsInf(n, 0) && math.Abs(n) < 1<<63 {
			return big.NewInt(int64(n)), true
		}
		return n, true
	}
	return nil, false
}

func numberEqual(a, b any) bool {
	ai, aInt := a.(*big.Int)
	bi, bInt := b.(*big.Int)
	if aInt && bInt {
		return ai.Cmp(bi) == 0
	}
	af, aFloat := a.(float64)
	bf, bFloat := b.(float64)
	return aFloat && bFloat && af == bf
}

// Sets and unsets that turn base into next, a changed list set whole
func diff(base, next map[string]any, prefix Path) (*pathMap, []Path) {
	sets := &pathMap{}
	var unsets []Path
	for _, key := range sorted(next) {
		value := next[key]
		p := extend(prefix, key)
		old, inBase := base[key]
		oldMap, oldIsMap := old.(map[string]any)
		newMap, newIsMap := value.(map[string]any)
		switch {
		case !inBase:
			sets.set(p, value)
		case oldIsMap && newIsMap:
			s, u := diff(oldMap, newMap, p)
			sets.update(s)
			unsets = append(unsets, u...)
		case !jsonEqual(old, value):
			sets.set(p, value)
		}
	}
	for _, key := range sorted(base) {
		if _, ok := next[key]; !ok {
			unsets = append(unsets, extend(prefix, key))
		}
	}
	return sets, unsets
}

// Leaves of a mapping a write sets, each by its full key
func leaves(value map[string]any, prefix Path, out *pathMap) {
	for _, key := range sorted(value) {
		p := extend(prefix, key)
		if child, ok := value[key].(map[string]any); ok && len(child) > 0 {
			leaves(child, p, out)
			continue
		}
		out.set(p, value[key])
	}
}

func clone(v any) any {
	switch c := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(c))
		for k, item := range c {
			out[k] = clone(item)
		}
		return out
	case []any:
		out := make([]any, len(c))
		for i, item := range c {
			out[i] = clone(item)
		}
		return out
	}
	return v
}

func cloneDoc(m map[string]any) map[string]any {
	out, _ := clone(m).(map[string]any)
	if out == nil {
		out = map[string]any{}
	}
	return out
}

// Applies sets and unsets to the document in place, an empty mapping set keeping a mapping already there
func apply(doc map[string]any, sets *pathMap, unsets []Path) {
	for _, e := range sets.entries {
		if m, ok := e.value.(map[string]any); ok && len(m) == 0 {
			if cur, found := lookup(doc, e.path); found {
				if _, isMap := cur.(map[string]any); isMap {
					continue
				}
			}
		}
		put(doc, e.path, clone(e.value))
	}
	for _, p := range unsets {
		pop(doc, p)
	}
}

func put(doc map[string]any, path Path, value any) {
	node := doc
	for _, seg := range path[:len(path)-1] {
		next, ok := node[seg].(map[string]any)
		if !ok {
			next = map[string]any{}
			node[seg] = next
		}
		node = next
	}
	node[path[len(path)-1]] = value
}

func pop(doc map[string]any, path Path) bool {
	node := doc
	for _, seg := range path[:len(path)-1] {
		next, ok := node[seg].(map[string]any)
		if !ok {
			return false
		}
		node = next
	}
	if _, ok := node[path[len(path)-1]]; ok {
		delete(node, path[len(path)-1])
		return true
	}
	return false
}

func lookup(doc map[string]any, path Path) (any, bool) {
	var node any = doc
	for _, seg := range path {
		m, ok := node.(map[string]any)
		if !ok {
			return nil, false
		}
		if node, ok = m[seg]; !ok {
			return nil, false
		}
	}
	return node, true
}

// Visits the key a write sets and every key inside its value, through mappings only
func touched(path Path, value any, visit func(Path, any) bool) bool {
	if !visit(path, value) {
		return false
	}
	if m, ok := value.(map[string]any); ok {
		for _, k := range sorted(m) {
			if !touched(extend(path, k), m[k], visit) {
				return false
			}
		}
	}
	return true
}

// Lock that refuses the write, false when every lock allows it
func writeCheck(sets *pathMap, unsets []Path, locks []Lock) (Lock, bool) {
	for _, p := range unsets {
		if lock, ok := covering(locks, p); ok {
			return lock, true
		}
	}
	var refused Lock
	var found bool
	for _, e := range sets.entries {
		touched(e.path, e.value, func(t Path, value any) bool {
			for _, lock := range locks {
				_, isMap := value.(map[string]any)
				if Covers(lock.Path, t) || len(t) < len(lock.Path) && Covers(t, lock.Path) && !isMap {
					refused, found = lock, true
					return false
				}
			}
			return true
		})
		if found {
			return refused, true
		}
	}
	return Lock{}, false
}

// Set object keyed by the plane's spelling of each key, and the list of keys unset
func encodeChanges(sets *pathMap, unsets []Path) (map[string]any, []string) {
	setBody := make(map[string]any, len(sets.entries))
	for _, e := range sets.entries {
		setBody[Encode(e.path)] = e.value
	}
	unsetBody := make([]string, len(unsets))
	for i, p := range unsets {
		unsetBody[i] = Encode(p)
	}
	return setBody, unsetBody
}
