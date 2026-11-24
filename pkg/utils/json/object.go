package jsonutil

import (
	"bytes"
	"fmt"
	"iter"
	"maps"
	"slices"
)

// Objects with more keys than this keep a map from key to position beside the key list
const indexMin = 8

// JSON object that keeps its keys in insertion order, a key set again keeping its first position
type Object struct {
	keys []string
	vals []any
	// Position of each key once there are more than indexMin keys, nil below; a function of keys, so reflect.DeepEqual compares content only
	index map[string]int
}

// Empty object ready for Set
func NewObject() *Object { return &Object{} }

// Object of alternating string keys and values in the order given; panics on an odd count or a key that is not a string
func ObjectOf(pairs ...any) *Object {
	if len(pairs)%2 != 0 {
		panic("jsonutil.ObjectOf: odd number of arguments")
	}
	n := len(pairs) / 2
	if n == 0 {
		return &Object{}
	}
	o := &Object{keys: make([]string, 0, n), vals: make([]any, 0, n)}
	for i := 0; i < len(pairs); i += 2 {
		key, ok := pairs[i].(string)
		if !ok {
			panic(fmt.Sprintf("jsonutil.ObjectOf: key %d is %T, not a string", i/2, pairs[i]))
		}
		o.Set(key, pairs[i+1])
	}
	return o
}

// Position of key, -1 when absent
func (o *Object) find(key string) int {
	if o.index != nil {
		if i, ok := o.index[key]; ok {
			return i
		}
		return -1
	}
	for i, k := range o.keys {
		if k == key {
			return i
		}
	}
	return -1
}

// Value stored under key and whether the key is present
func (o *Object) Get(key string) (any, bool) {
	if o == nil {
		return nil, false
	}
	if i := o.find(key); i >= 0 {
		return o.vals[i], true
	}
	return nil, false
}

// Value stored under key, nil when the key is absent
func (o *Object) Value(key string) any {
	if o == nil {
		return nil
	}
	if i := o.find(key); i >= 0 {
		return o.vals[i]
	}
	return nil
}

// Value stored under key, def when the key is absent
func (o *Object) ValueOr(key string, def any) any {
	if o == nil {
		return def
	}
	if i := o.find(key); i >= 0 {
		return o.vals[i]
	}
	return def
}

// Whether key is present
func (o *Object) Has(key string) bool { return o != nil && o.find(key) >= 0 }

// Stores v under key, a new key going last and an existing one keeping its position
func (o *Object) Set(key string, v any) {
	if i := o.find(key); i >= 0 {
		o.vals[i] = v
		return
	}
	o.keys = append(o.keys, key)
	o.vals = append(o.vals, v)
	switch n := len(o.keys); {
	case o.index != nil:
		o.index[key] = n - 1
	case n > indexMin:
		o.buildIndex()
	}
}

func (o *Object) buildIndex() {
	o.index = make(map[string]int, len(o.keys))
	for i, k := range o.keys {
		o.index[k] = i
	}
}

// Sets every key of other in other's order, keys already present keeping their position
func (o *Object) Merge(other *Object) {
	if other == nil || other == o {
		return
	}
	for i, k := range other.keys {
		o.Set(k, other.vals[i])
	}
}

// Removes key, reporting whether it was present; slices Keys returned earlier keep their contents, and an iteration of All in progress goes on over the keys it started with
func (o *Object) Delete(key string) bool {
	if o == nil {
		return false
	}
	i := o.find(key)
	if i < 0 {
		return false
	}
	o.removeAt(i)
	return true
}

// Removes key and gives back the value it held and whether it was present
func (o *Object) Take(key string) (any, bool) {
	if o == nil {
		return nil, false
	}
	i := o.find(key)
	if i < 0 {
		return nil, false
	}
	v := o.vals[i]
	o.removeAt(i)
	return v, true
}

// Removes the key at position i into fresh arrays, so slices Keys and All handed out keep their contents; linear in the number of keys
func (o *Object) removeAt(i int) {
	last := len(o.keys) - 1
	if last == 0 {
		o.keys, o.vals, o.index = nil, nil, nil
		return
	}
	if o.index != nil {
		delete(o.index, o.keys[i])
	}
	if i == last {
		o.keys, o.vals = o.keys[:last:last], o.vals[:last:last]
	} else {
		o.keys = slices.Concat(o.keys[:i], o.keys[i+1:])
		o.vals = slices.Concat(o.vals[:i], o.vals[i+1:])
	}
	switch {
	case last <= indexMin:
		o.index = nil
	case o.index != nil:
		for j := i; j < last; j++ {
			o.index[o.keys[j]] = j
		}
	}
}

// Moves the value under from to the key to at the same position; false when from is absent or to is already present
func (o *Object) Rename(from, to string) bool {
	if o == nil {
		return false
	}
	i := o.find(from)
	if i < 0 || (from != to && o.find(to) >= 0) {
		return false
	}
	if from == to {
		return true
	}
	o.keys = slices.Clone(o.keys)
	o.keys[i] = to
	if o.index != nil {
		delete(o.index, from)
		o.index[to] = i
	}
	return true
}

// Number of keys
func (o *Object) Len() int {
	if o == nil {
		return 0
	}
	return len(o.keys)
}

// Keys in insertion order; the slice belongs to the object and must not be modified
func (o *Object) Keys() []string {
	if o == nil {
		return nil
	}
	return o.keys
}

// Keys and values in insertion order over the keys present when the iteration started: one deleted on the way is still visited, one added is not
func (o *Object) All() iter.Seq2[string, any] {
	return func(yield func(string, any) bool) {
		if o == nil {
			return
		}
		keys, vals := o.keys, o.vals
		for i, k := range keys {
			if !yield(k, vals[i]) {
				return
			}
		}
	}
}

// Shallow copy with the same key order, nil for a nil object
func (o *Object) Clone() *Object {
	if o == nil {
		return nil
	}
	return &Object{keys: slices.Clone(o.keys), vals: slices.Clone(o.vals), index: maps.Clone(o.index)}
}

// Keys and values as a plain map, a shallow copy; nil for a nil object
func (o *Object) Map() map[string]any {
	if o == nil {
		return nil
	}
	m := make(map[string]any, len(o.keys))
	for i, k := range o.keys {
		m[k] = o.vals[i]
	}
	return m
}

// Compact JSON text of the object, values with no JSON form written as their fmt text and a failure as %!(error)
func (o *Object) String() string {
	s, err := Encode(o, Convert(func(v any) (any, error) { return fmt.Sprint(v), nil }))
	if err != nil {
		return "%!(" + err.Error() + ")"
	}
	return s
}

// Compact JSON of the object with its keys in insertion order
func (o *Object) MarshalJSON() ([]byte, error) {
	if o == nil {
		return []byte("null"), nil
	}
	return Append(nil, o)
}

// Replaces the contents with the JSON object in data, leaving them as they are for JSON null
func (o *Object) UnmarshalJSON(data []byte) error {
	if string(bytes.TrimSpace(data)) == "null" {
		return nil
	}
	obj, err := DecodeObject(data)
	if err != nil {
		return err
	}
	*o = *obj
	return nil
}
