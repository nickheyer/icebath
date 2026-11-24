package orderedutil

import (
	"iter"
	"sort"
)

// Every key in insertion order as a new slice the caller owns, nil when there are none
func (m *Map[K, V]) Keys() []K {
	if m.Len() == 0 {
		return nil
	}
	keys := make([]K, 0, len(m.index))
	for _, e := range m.entries[m.head:] {
		if e.live {
			keys = append(keys, e.key)
		}
	}
	return keys
}

// Every value in insertion order as a new slice the caller owns, nil only for a nil map
func (m *Map[K, V]) Values() []V {
	if m == nil {
		return nil
	}
	vals := make([]V, 0, len(m.index))
	for _, e := range m.entries[m.head:] {
		if e.live {
			vals = append(vals, e.val)
		}
	}
	return vals
}

// Key value pairs in insertion order; the loop body may change the map, pairs it deletes before they are reached are skipped and pairs it adds come last
func (m *Map[K, V]) All() iter.Seq2[K, V] {
	return func(yield func(K, V) bool) {
		if m == nil {
			return
		}
		i, last, started := m.head, uint64(0), false
		for {
			if started && (i > len(m.entries) || m.entries[i-1].seq != last) {
				i = m.after(last)
			}
			for i < len(m.entries) && !m.entries[i].live {
				i++
			}
			if i >= len(m.entries) {
				return
			}
			e := m.entries[i]
			last, started, i = e.seq, true, i+1
			if !yield(e.key, e.val) {
				return
			}
		}
	}
}

// Earliest stored key still present and its value, false when the map is empty
func (m *Map[K, V]) First() (K, V, bool) {
	if m != nil {
		for _, e := range m.entries[m.head:] {
			if e.live {
				return e.key, e.val, true
			}
		}
	}
	var (
		k K
		v V
	)
	return k, v, false
}

// Position of the first entry stored after the entry numbered seq, entries being in seq order
func (m *Map[K, V]) after(seq uint64) int {
	return sort.Search(len(m.entries), func(j int) bool { return m.entries[j].seq > seq })
}
