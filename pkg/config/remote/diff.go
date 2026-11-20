package remote

import (
	"math"
	"math/big"
	"strconv"
	"strings"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

// One path and the value a write sets there
type PathValue struct {
	Path  Path
	Value any
}

// Insertion ordered map from path to value with Python dict update semantics
type PathMap struct {
	entries []PathValue
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

// Stores value at p, keeping the position of a path already present
func (m *PathMap) Set(p Path, v any) {
	if m.index == nil {
		m.index = map[string]int{}
	}
	k := pathKey(p)
	if i, ok := m.index[k]; ok {
		m.entries[i].Value = v
		return
	}
	m.index[k] = len(m.entries)
	m.entries = append(m.entries, PathValue{append(Path{}, p...), v})
}

// Stores value at p only when p is absent
func (m *PathMap) SetDefault(p Path, v any) {
	if !m.Has(p) {
		m.Set(p, v)
	}
}

func (m *PathMap) Get(p Path) (any, bool) {
	if m == nil || m.index == nil {
		return nil, false
	}
	i, ok := m.index[pathKey(p)]
	if !ok {
		return nil, false
	}
	return m.entries[i].Value, true
}

func (m *PathMap) Has(p Path) bool {
	_, ok := m.Get(p)
	return ok
}

func (m *PathMap) Len() int {
	if m == nil {
		return 0
	}
	return len(m.entries)
}

// Entries in insertion order
func (m *PathMap) Entries() []PathValue {
	if m == nil {
		return nil
	}
	return m.entries
}

func (m *PathMap) update(o *PathMap) {
	for _, e := range o.Entries() {
		m.Set(e.Path, e.Value)
	}
}

// Structural JSON equality with numbers by value and a bool never equal to a number
func JSONEqual(a, b any) bool {
	ab, aBool := a.(bool)
	bb, bBool := b.(bool)
	if aBool || bBool {
		return aBool && bBool && ab == bb
	}
	if an, ok := asNumber(a); ok {
		if bn, ok := asNumber(b); ok {
			return numberEqual(an, bn)
		}
		return false
	}
	switch x := a.(type) {
	case map[string]any:
		y, ok := b.(map[string]any)
		if !ok || len(x) != len(y) {
			return false
		}
		for k, v := range x {
			w, ok := y[k]
			if !ok || !JSONEqual(v, w) {
				return false
			}
		}
		return true
	case []any:
		y, ok := b.([]any)
		if !ok || len(x) != len(y) {
			return false
		}
		for i := range x {
			if !JSONEqual(x[i], y[i]) {
				return false
			}
		}
		return true
	case string:
		y, ok := b.(string)
		return ok && x == y
	case nil:
		return b == nil
	}
	return false
}

// Number as an exact big.Int or a float64
func asNumber(v any) (any, bool) {
	switch n := v.(type) {
	case int:
		return big.NewInt(int64(n)), true
	case int64:
		return big.NewInt(n), true
	case uint64:
		return new(big.Int).SetUint64(n), true
	case *big.Int:
		return n, true
	case float64:
		return n, true
	}
	return nil, false
}

// Exact comparison of two numbers the way Python compares int and float
func numberEqual(a, b any) bool {
	af, aFloat := a.(float64)
	bf, bFloat := b.(float64)
	switch {
	case aFloat && bFloat:
		return af == bf
	case aFloat:
		return floatEqualsInt(af, b.(*big.Int))
	case bFloat:
		return floatEqualsInt(bf, a.(*big.Int))
	}
	return a.(*big.Int).Cmp(b.(*big.Int)) == 0
}

func floatEqualsInt(f float64, n *big.Int) bool {
	if math.IsNaN(f) || math.IsInf(f, 0) || f != math.Trunc(f) {
		return false
	}
	i, _ := big.NewFloat(f).Int(nil)
	return i.Cmp(n) == 0
}

func asDoc(v any) (map[string]any, bool) {
	m, ok := v.(map[string]any)
	return m, ok
}

// Sets and unsets turning base into new, a changed list being one whole list set
func Diff(base, new map[string]any, prefix Path) (*PathMap, []Path) {
	sets := &PathMap{}
	var unsets []Path
	for _, key := range config.MappingKeys(new) {
		value := new[key]
		p := extend(prefix, key)
		old, inBase := base[key]
		oldMap, oldIsMap := asDoc(old)
		newMap, newIsMap := asDoc(value)
		switch {
		case !inBase:
			sets.Set(p, value)
		case oldIsMap && newIsMap:
			s, u := Diff(oldMap, newMap, p)
			sets.update(s)
			unsets = append(unsets, u...)
		case !JSONEqual(old, value):
			sets.Set(p, value)
		}
	}
	for _, key := range config.MappingKeys(base) {
		if _, ok := new[key]; !ok {
			unsets = append(unsets, extend(prefix, key))
		}
	}
	return sets, unsets
}

// Diff as a key level intent, a non empty mapping the base did not hold as a mapping expanded to its leaves
func IntentDiff(base, new map[string]any, prefix Path) (*PathMap, []Path) {
	sets := &PathMap{}
	var unsets []Path
	for _, key := range config.MappingKeys(new) {
		value := new[key]
		p := extend(prefix, key)
		old, inBase := base[key]
		oldMap, oldIsMap := asDoc(old)
		newMap, newIsMap := asDoc(value)
		switch {
		case newIsMap && len(newMap) > 0 && !oldIsMap:
			s, _ := IntentDiff(map[string]any{}, newMap, p)
			sets.update(s)
		case !inBase:
			sets.Set(p, value)
		case oldIsMap && newIsMap:
			s, u := IntentDiff(oldMap, newMap, p)
			sets.update(s)
			unsets = append(unsets, u...)
		case !JSONEqual(old, value):
			sets.Set(p, value)
		}
	}
	for _, key := range config.MappingKeys(base) {
		if _, ok := new[key]; !ok {
			unsets = append(unsets, extend(prefix, key))
		}
	}
	return sets, unsets
}

// Applies an intent edit to doc in place, an empty mapping set keeping a mapping already there
func ApplyIntent(doc map[string]any, sets *PathMap, unsets []Path) {
	for _, e := range sets.Entries() {
		if m, ok := asDoc(e.Value); ok && len(m) == 0 {
			if cur, found := getPath(doc, e.Path); found {
				if _, isMap := asDoc(cur); isMap {
					continue
				}
			}
		}
		setPath(doc, e.Path, config.Clone(e.Value))
	}
	for _, p := range unsets {
		popPath(doc, p)
	}
}

func setPath(doc map[string]any, path Path, value any) {
	node := doc
	for _, seg := range path[:len(path)-1] {
		next, ok := asDoc(node[seg])
		if !ok {
			if _, had := node[seg]; !had {
				config.AppendMappingKey(node, seg)
			}
			next = map[string]any{}
			node[seg] = next
		}
		node = next
	}
	last := path[len(path)-1]
	if _, had := node[last]; !had {
		config.AppendMappingKey(node, last)
	}
	node[last] = value
}

func popPath(doc map[string]any, path Path) bool {
	node := doc
	for _, seg := range path[:len(path)-1] {
		next, ok := asDoc(node[seg])
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

func getPath(doc map[string]any, path Path) (any, bool) {
	var node any = doc
	for _, seg := range path {
		m, ok := asDoc(node)
		if !ok {
			return nil, false
		}
		if node, ok = m[seg]; !ok {
			return nil, false
		}
	}
	return node, true
}

// Copies of both documents without any locked path, plus the locked paths whose new value differed
func StripLocked(base, new map[string]any, locks []Lock) (map[string]any, map[string]any, []Path) {
	base, _ = config.Clone(base).(map[string]any)
	new, _ = config.Clone(new).(map[string]any)
	var changed []Path
	for _, lock := range locks {
		baseVal, inBase := getPath(base, lock.Path)
		newVal, inNew := getPath(new, lock.Path)
		if inBase != inNew || (inNew && !JSONEqual(baseVal, newVal)) {
			changed = append(changed, lock.Path)
		}
		popPath(base, lock.Path)
		popPath(new, lock.Path)
	}
	return base, new, changed
}

// Visits the set path and every path inside value, descending through objects only
func touched(path Path, value any, visit func(Path, any) bool) bool {
	if !visit(path, value) {
		return false
	}
	if m, ok := asDoc(value); ok {
		for _, k := range config.MappingKeys(m) {
			if !touched(extend(path, k), m[k], visit) {
				return false
			}
		}
	}
	return true
}

// Lock that refuses this write, false when every lock allows it
func WriteCheck(sets *PathMap, unsets []Path, locks []Lock) (Lock, bool) {
	for _, p := range unsets {
		for _, lock := range locks {
			if Covers(lock.Path, p) {
				return lock, true
			}
		}
	}
	var refused Lock
	var found bool
	for _, e := range sets.Entries() {
		touched(e.Path, e.Value, func(t Path, tv any) bool {
			for _, lock := range locks {
				if Covers(lock.Path, t) {
					refused, found = lock, true
					return false
				}
				if _, isMap := asDoc(tv); len(t) < len(lock.Path) && Covers(t, lock.Path) && !isMap {
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

// Wire set object keyed by encoded path and the encoded unset list
func EncodeChanges(sets *PathMap, unsets []Path) (*pyjson.Object, []string) {
	setBody := pyjson.NewObject()
	for _, e := range sets.Entries() {
		setBody.Set(Encode(e.Path), wireObject(e.Value))
	}
	unsetBody := make([]string, len(unsets))
	for i, p := range unsets {
		unsetBody[i] = Encode(p)
	}
	return setBody, unsetBody
}

// Value with every mapping as an ordered JSON object in its recorded key order
func wireObject(v any) any {
	switch c := v.(type) {
	case map[string]any:
		obj := pyjson.NewObject()
		for _, k := range config.MappingKeys(c) {
			obj.Set(k, wireObject(c[k]))
		}
		return obj
	case []any:
		out := make([]any, len(c))
		for i, e := range c {
			out[i] = wireObject(e)
		}
		return out
	}
	return v
}
