package tools

import "container/list"

// Most documents a client keeps open, the least recently touched closed past it and opened again on their next touch
const maxDocuments = 64

// What a client synced of one document and the diagnostics it holds for it; versions double as freshness tags, -1 meaning none
type document struct {
	version int
	text    string
	push    []diagnostic
	pull    []diagnostic
	// Versions the push and the pull stores were reported at
	pushed, pulled int
	// The server already sent the first push, which a seeding server sends before it looked at the text
	seeded bool
}

func newDocument(version int, text string) *document {
	return &document{version: version, text: text, pushed: -1, pulled: -1}
}

func (d *document) freshPush(version int) bool { return d.pushed >= version }
func (d *document) freshPull(version int) bool { return d.pulled >= version }
func (d *document) fresh(version int) bool     { return d.freshPush(version) || d.freshPull(version) }

// Documents by path, least recently touched first
type documents struct {
	order *list.List
	index map[string]*list.Element
}

type slot struct {
	path string
	doc  *document
}

func newDocuments() *documents {
	return &documents{order: list.New(), index: map[string]*list.Element{}}
}

func (m *documents) get(path string) *document {
	if e, ok := m.index[path]; ok {
		return e.Value.(*slot).doc
	}
	return nil
}

// Document of the path, one with no version made when there is none, where it stays in its place
func (m *documents) ensure(path string) *document {
	if d := m.get(path); d != nil {
		return d
	}
	d := newDocument(-1, "")
	m.index[path] = m.order.PushBack(&slot{path, d})
	return d
}

// Keeps the document as the most recently touched
func (m *documents) touch(path string, d *document) {
	if e, ok := m.index[path]; ok {
		m.order.Remove(e)
	}
	m.index[path] = m.order.PushBack(&slot{path, d})
}

func (m *documents) len() int { return m.order.Len() }

// Removes and returns the least recently touched document
func (m *documents) oldest() (string, *document) {
	front := m.order.Front()
	e := front.Value.(*slot)
	m.order.Remove(front)
	delete(m.index, e.path)
	return e.path, e.doc
}
