package orderedutil

// New map of the inputs' pairs taken left to right, a later value for a key replacing the earlier one in its first place; nil inputs read as empty
func Merge[K comparable, V any](maps ...*Map[K, V]) *Map[K, V] {
	n := 0
	for _, m := range maps {
		n += m.Len()
	}
	out := &Map[K, V]{index: make(map[K]int, n), entries: make([]entry[K, V], 0, n)}
	for _, m := range maps {
		for k, v := range m.All() {
			out.Set(k, v)
		}
	}
	return out
}

// New map holding the same pairs in the same order, empty for a nil map
func (m *Map[K, V]) Clone() *Map[K, V] { return Merge(m) }
