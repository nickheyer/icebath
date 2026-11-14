package model

import (
	"container/list"
	"crypto/sha256"
	"sync"
	"time"

	"github.com/nickheyer/icebath/internal/model/wire"
)

const (
	memoLimit        = 4096
	memoActiveWindow = 60 * time.Second
	toolsMemoLimit   = 256
)

type memoKey [sha256.Size]byte

type memoEntry struct {
	key     memoKey
	text    int
	images  int
	touched time.Time
}

type tokenMemo struct {
	mu      sync.Mutex
	entries map[memoKey]*list.Element
	order   list.List
}

func keyOf(members []wire.Member) memoKey {
	h := sha256.New()
	for _, m := range members {
		if persistenceFields[m.Key] {
			continue
		}
		h.Write([]byte(m.Key))
		h.Write([]byte{0})
		h.Write(m.Value)
		h.Write([]byte{0})
	}
	var k memoKey
	h.Sum(k[:0])
	return k
}

func (m *tokenMemo) lookup(key memoKey) (text, images int, ok bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	el, ok := m.entries[key]
	if !ok {
		return 0, 0, false
	}
	e := el.Value.(*memoEntry)
	e.touched = time.Now()
	m.order.MoveToBack(el)
	return e.text, e.images, true
}

func (m *tokenMemo) remember(key memoKey, text, images int) {
	now := time.Now()
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.entries == nil {
		m.entries = map[memoKey]*list.Element{}
	}
	if el, ok := m.entries[key]; ok {
		e := el.Value.(*memoEntry)
		e.text, e.images, e.touched = text, images, now
		m.order.MoveToBack(el)
	} else {
		m.entries[key] = m.order.PushBack(&memoEntry{key: key, text: text, images: images, touched: now})
	}
	idleBefore := now.Add(-memoActiveWindow)
	for m.order.Len() > 1 && m.order.Len() > memoLimit {
		front := m.order.Front()
		e := front.Value.(*memoEntry)
		if !e.touched.Before(idleBefore) {
			break
		}
		m.order.Remove(front)
		delete(m.entries, e.key)
	}
}

func (m *tokenMemo) forget(keys map[memoKey]bool) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for key := range keys {
		if el, ok := m.entries[key]; ok {
			m.order.Remove(el)
			delete(m.entries, key)
			n++
		}
	}
	return n
}

type toolsEntry struct {
	count       int
	first, last string
	tokens      int
}

type toolsMemo struct {
	mu      sync.Mutex
	entries map[*wire.Tool]toolsEntry
	order   []*wire.Tool
}

func (m *toolsMemo) lookup(tools []wire.Tool) (int, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.entries[&tools[0]]
	if !ok || e.count != len(tools) || e.first != tools[0].Name || e.last != tools[len(tools)-1].Name {
		return 0, false
	}
	return e.tokens, true
}

func (m *toolsMemo) remember(tools []wire.Tool, tokens int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.entries == nil {
		m.entries = map[*wire.Tool]toolsEntry{}
	}
	key := &tools[0]
	if _, ok := m.entries[key]; !ok {
		if len(m.entries) >= toolsMemoLimit {
			delete(m.entries, m.order[0])
			m.order = m.order[1:]
		}
		m.order = append(m.order, key)
	}
	m.entries[key] = toolsEntry{count: len(tools), first: tools[0].Name, last: tools[len(tools)-1].Name, tokens: tokens}
}
