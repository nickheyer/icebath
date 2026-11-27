package tools

import (
	"container/list"
	"context"
	"sync"
	"time"
)

// Sessions late verdicts are kept for, and notes kept per session
const (
	maxOwners   = 64
	maxPerOwner = 32
)

// Notes of one session by path, oldest first
type ownerNotes struct {
	owner string
	order *list.List
	index map[string]*list.Element
}

type pathNote struct {
	path string
	note carried
}

func (n *ownerNotes) pop(path string) (carried, bool) {
	e, ok := n.index[path]
	if !ok {
		return carried{}, false
	}
	delete(n.index, path)
	n.order.Remove(e)
	return e.Value.(*pathNote).note, true
}

func (n *ownerNotes) set(path string, c carried) {
	if e, ok := n.index[path]; ok {
		e.Value.(*pathNote).note = c
		return
	}
	n.index[path] = n.order.PushBack(&pathNote{path, c})
}

// Late verdicts not yet handed to their sessions, and the checks of each session still running
type notes struct {
	mu      sync.Mutex
	owners  *list.List
	byOwner map[string]*list.Element
	running map[string]int
	// Closed and replaced each time a check settles
	settled chan struct{}
}

func newNotes() *notes {
	return &notes{owners: list.New(), byOwner: map[string]*list.Element{}, running: map[string]int{}, settled: make(chan struct{})}
}

// Takes out the notes of an owner, with the lock held
func (n *notes) popOwner(owner string) *ownerNotes {
	e, ok := n.byOwner[owner]
	if !ok {
		return nil
	}
	delete(n.byOwner, owner)
	n.owners.Remove(e)
	return e.Value.(*ownerNotes)
}

// Keeps a note for the owner under the path; a note still kept for the path stays ahead of it, with its baseline
func (n *notes) post(owner, path, text string, before *string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	o := n.popOwner(owner)
	if o == nil {
		o = &ownerNotes{owner: owner, order: list.New(), index: map[string]*list.Element{}}
	}
	if older, ok := o.pop(path); ok && older.text != text {
		text, before = older.text+"\n\n"+text, older.before
	}
	o.set(path, carried{text: text, before: before})
	for o.order.Len() > maxPerOwner {
		front := o.order.Front()
		delete(o.index, front.Value.(*pathNote).path)
		o.order.Remove(front)
	}
	n.byOwner[owner] = n.owners.PushBack(o)
	for n.owners.Len() > maxOwners {
		front := n.owners.Front()
		delete(n.byOwner, front.Value.(*ownerNotes).owner)
		n.owners.Remove(front)
	}
}

// Takes back the owner's note for a path, so a newer edit of the file carries it
func (n *notes) take(owner, path string) *carried {
	if owner == "" {
		return nil
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	e, ok := n.byOwner[owner]
	if !ok {
		return nil
	}
	c, ok := e.Value.(*ownerNotes).pop(path)
	if !ok {
		return nil
	}
	return &c
}

// Keeps notes taken back again, oldest first
func (n *notes) restore(owner, path string, carry []carried) {
	if owner == "" {
		return
	}
	for _, c := range carry {
		n.post(owner, path, c.text, c.before)
	}
}

// Every note of the owner, oldest first, with the lock held
func (n *notes) popLocked(owner string) []string {
	o := n.popOwner(owner)
	if o == nil {
		return nil
	}
	var out []string
	for e := o.order.Front(); e != nil; e = e.Next() {
		out = append(out, e.Value.(*pathNote).note.text)
	}
	return out
}

// Counts a check of the owner as running until settle is called
func (n *notes) track(owner string) (settle func()) {
	n.mu.Lock()
	n.running[owner]++
	n.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			n.mu.Lock()
			defer n.mu.Unlock()
			if n.running[owner]--; n.running[owner] <= 0 {
				delete(n.running, owner)
			}
			close(n.settled)
			n.settled = make(chan struct{})
		})
	}
}

// Forgets the notes of a session that ended
func (n *notes) drop(owner string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.popOwner(owner)
}

// Waits until the owner's running checks settled, the deadline passed or the context ended, then hands out its notes
func (n *notes) settle(ctx context.Context, owner string, deadline time.Time) []string {
	n.mu.Lock()
	for n.running[owner] > 0 {
		remaining := time.Until(deadline)
		if remaining <= 0 || ctx.Err() != nil {
			break
		}
		wake := n.settled
		n.mu.Unlock()
		t := time.NewTimer(remaining)
		select {
		case <-wake:
		case <-t.C:
		case <-ctx.Done():
		}
		t.Stop()
		n.mu.Lock()
	}
	out := n.popLocked(owner)
	n.mu.Unlock()
	return out
}
