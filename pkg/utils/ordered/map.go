package orderedutil

// Key to value mapping that remembers the order keys were first stored in; storing an existing key keeps its place, deleting it forgets the place, a nil *Map reads as empty and the zero Map is ready to use
type Map[K comparable, V any] struct {
	index   map[K]int
	entries []entry[K, V]
	head    int
	dead    int
	seq     uint64
}

// One stored pair, or the hole a deleted pair leaves until the entries are compacted; seq grows with every new entry so iterators can find their place again after a compaction
type entry[K comparable, V any] struct {
	key  K
	val  V
	seq  uint64
	live bool
}

// Empty map
func NewMap[K comparable, V any]() *Map[K, V] { return &Map[K, V]{} }

// Value stored under k, false when k is absent
func (m *Map[K, V]) Get(k K) (V, bool) {
	if m != nil {
		if i, ok := m.index[k]; ok {
			return m.entries[i].val, true
		}
	}
	var zero V
	return zero, false
}

// Whether k is present
func (m *Map[K, V]) Has(k K) bool {
	if m == nil {
		return false
	}
	_, ok := m.index[k]
	return ok
}

// Stores v under k, a new key going last and an existing key keeping its place
func (m *Map[K, V]) Set(k K, v V) {
	if i, ok := m.index[k]; ok {
		m.entries[i].val = v
		return
	}
	if m.index == nil {
		m.index = make(map[K]int)
	}
	m.index[k] = len(m.entries)
	m.entries = append(m.entries, entry[K, V]{key: k, val: v, seq: m.seq, live: true})
	m.seq++
}

// Removes k in constant amortized time, true when it was present
func (m *Map[K, V]) Delete(k K) bool {
	if m == nil {
		return false
	}
	i, ok := m.index[k]
	if !ok {
		return false
	}
	delete(m.index, k)
	m.entries[i] = entry[K, V]{seq: m.entries[i].seq}
	m.dead++
	if i == m.head {
		for m.head < len(m.entries) && !m.entries[m.head].live {
			m.head++
		}
	}
	if 2*m.dead > len(m.entries) {
		m.compact()
	}
	return true
}

// Number of keys
func (m *Map[K, V]) Len() int {
	if m == nil {
		return 0
	}
	return len(m.index)
}

// Removes every key, the map keeping its identity for everyone holding it
func (m *Map[K, V]) Clear() {
	if m == nil {
		return
	}
	clear(m.index)
	clear(m.entries)
	m.entries = m.entries[:0]
	m.head, m.dead = 0, 0
}

// Closes the holes deleted entries left, giving memory back once most of the backing array is unused
func (m *Map[K, V]) compact() {
	old, n := m.entries, len(m.index)
	shrink := cap(old) > 64 && 4*n < cap(old)
	live := old[:0]
	if shrink {
		live = make([]entry[K, V], 0, 2*n)
	}
	for _, e := range old[m.head:] {
		if e.live {
			m.index[e.key] = len(live)
			live = append(live, e)
		}
	}
	if !shrink {
		clear(old[len(live):])
	}
	m.entries = live
	m.head, m.dead = 0, 0
}
