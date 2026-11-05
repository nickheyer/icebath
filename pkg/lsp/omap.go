package lsp

import "container/list"

// Insertion ordered map with Python dict semantics, assignment to a present key keeping its position
type omap[K comparable, V any] struct {
	index map[K]*list.Element
	order *list.List
}

type omapEntry[K comparable, V any] struct {
	key K
	val V
}

func newOmap[K comparable, V any]() *omap[K, V] {
	return &omap[K, V]{index: map[K]*list.Element{}, order: list.New()}
}

func (m *omap[K, V]) get(k K) (V, bool) {
	if e, ok := m.index[k]; ok {
		return e.Value.(*omapEntry[K, V]).val, true
	}
	var zero V
	return zero, false
}

func (m *omap[K, V]) set(k K, v V) {
	if e, ok := m.index[k]; ok {
		e.Value.(*omapEntry[K, V]).val = v
		return
	}
	m.index[k] = m.order.PushBack(&omapEntry[K, V]{k, v})
}

func (m *omap[K, V]) pop(k K) (V, bool) {
	e, ok := m.index[k]
	if !ok {
		var zero V
		return zero, false
	}
	delete(m.index, k)
	m.order.Remove(e)
	return e.Value.(*omapEntry[K, V]).val, true
}

func (m *omap[K, V]) len() int { return len(m.index) }

// Keys in insertion order
func (m *omap[K, V]) keys() []K {
	out := make([]K, 0, len(m.index))
	for e := m.order.Front(); e != nil; e = e.Next() {
		out = append(out, e.Value.(*omapEntry[K, V]).key)
	}
	return out
}

// Oldest key, false when empty
func (m *omap[K, V]) first() (K, bool) {
	if e := m.order.Front(); e != nil {
		return e.Value.(*omapEntry[K, V]).key, true
	}
	var zero K
	return zero, false
}

func (m *omap[K, V]) clear() {
	m.index = map[K]*list.Element{}
	m.order.Init()
}
