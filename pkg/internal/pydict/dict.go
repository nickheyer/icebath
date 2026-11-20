package pydict

import "iter"

// Insertion ordered map, assignment keeps a key's position and deletion forgets it as a Python dict does
type Dict[K comparable, V any] struct {
	keys []K
	vals map[K]V
}

// Empty dict
func New[K comparable, V any]() *Dict[K, V] { return &Dict[K, V]{vals: map[K]V{}} }

// Value of a key, a nil dict reading as empty
func (d *Dict[K, V]) Get(k K) (V, bool) {
	if d == nil {
		var zero V
		return zero, false
	}
	v, ok := d.vals[k]
	return v, ok
}

// Whether the key is present
func (d *Dict[K, V]) Has(k K) bool {
	_, ok := d.Get(k)
	return ok
}

// Stores a value, a new key going last and an existing key keeping its position
func (d *Dict[K, V]) Set(k K, v V) {
	if _, ok := d.vals[k]; !ok {
		d.keys = append(d.keys, k)
	}
	d.vals[k] = v
}

// Removes a key, true when it was present
func (d *Dict[K, V]) Delete(k K) bool {
	if d == nil {
		return false
	}
	if _, ok := d.vals[k]; !ok {
		return false
	}
	delete(d.vals, k)
	for i, key := range d.keys {
		if key == k {
			d.keys = append(d.keys[:i:i], d.keys[i+1:]...)
			break
		}
	}
	return true
}

// Number of keys
func (d *Dict[K, V]) Len() int {
	if d == nil {
		return 0
	}
	return len(d.keys)
}

// Keys in insertion order
func (d *Dict[K, V]) Keys() []K {
	if d == nil {
		return nil
	}
	return append([]K(nil), d.keys...)
}

// Values in insertion order
func (d *Dict[K, V]) Values() []V {
	if d == nil {
		return nil
	}
	out := make([]V, 0, len(d.keys))
	for _, k := range d.keys {
		out = append(out, d.vals[k])
	}
	return out
}

// Key value pairs in insertion order
func (d *Dict[K, V]) All() iter.Seq2[K, V] {
	return func(yield func(K, V) bool) {
		if d == nil {
			return
		}
		for _, k := range d.keys {
			if !yield(k, d.vals[k]) {
				return
			}
		}
	}
}

// New dict of the inputs unpacked left to right, later values winning as in {**a, **b}
func Merge[K comparable, V any](dicts ...*Dict[K, V]) *Dict[K, V] {
	out := New[K, V]()
	for _, d := range dicts {
		for k, v := range d.All() {
			out.Set(k, v)
		}
	}
	return out
}
