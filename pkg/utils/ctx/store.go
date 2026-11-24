package ctxutil

import (
	"context"
	"slices"
	"sync"
	"sync/atomic"
)

// Mutable set of variable values a context carries: a Set made through the context is seen by all later code running in it, and Copy forks it so later sets on either side stay apart
type Store struct {
	mu    sync.Mutex
	items atomic.Pointer[[]Item]
}

// Variable and the value a Store holds for it
type Item struct {
	Var   Variable
	Value any
}

// Variable of any value type, the form a Store lists its entries in
type Variable interface {
	Name() string
	isVariable()
}

type storeKey struct{}

// Store of the code that never entered one, shared by the whole process
var defaultStore = NewStore()

// Empty store
func NewStore() *Store { return &Store{} }

// Context running in s, every variable read or set through it using s
func WithStore(ctx context.Context, s *Store) context.Context {
	return context.WithValue(ctx, storeKey{}, s)
}

// Store ctx runs in, the process wide default store when it entered none
func StoreOf(ctx context.Context) *Store {
	if ctx != nil {
		if s, ok := ctx.Value(storeKey{}).(*Store); ok && s != nil {
			return s
		}
	}
	return defaultStore
}

// Independent copy of the store ctx runs in
func CopyStore(ctx context.Context) *Store { return StoreOf(ctx).Copy() }

// Context running in a copy of the store ctx runs in, so sets on either side stay apart from then on
func Fork(ctx context.Context) context.Context { return WithStore(ctx, CopyStore(ctx)) }

// Independent copy holding the same values, made in constant time
func (s *Store) Copy() *Store {
	c := &Store{}
	c.items.Store(s.items.Load())
	return c
}

// Every variable s holds a value for, with that value, in the order they were first set
func (s *Store) Items() []Item {
	if p := s.items.Load(); p != nil {
		return slices.Clone(*p)
	}
	return nil
}

// Takes on every value src holds, variables new to s going last in src's order
func (s *Store) Overlay(src *Store) {
	p := src.items.Load()
	if p == nil || src == s {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	next := s.snapshot()
	for _, it := range *p {
		if i := indexOf(next, it.Var); i >= 0 {
			next[i].Value = it.Value
		} else {
			next = append(next, it)
		}
	}
	s.items.Store(&next)
}

// Restore point: calling the returned func puts every value of s back to what it holds now
func (s *Store) Checkpoint() (restore func()) {
	saved := s.items.Load()
	return func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.items.Store(saved)
	}
}

// Value s holds for v, false when it holds none
func (s *Store) lookup(v Variable) (any, bool) {
	if p := s.items.Load(); p != nil {
		if i := indexOf(*p, v); i >= 0 {
			return (*p)[i].Value, true
		}
	}
	return nil, false
}

// Stores val for v, or forgets v when present is false, giving back what s held for v before
func (s *Store) swap(v Variable, val any, present bool) (old any, had bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var cur []Item
	if p := s.items.Load(); p != nil {
		cur = *p
	}
	i := indexOf(cur, v)
	if i >= 0 {
		old, had = cur[i].Value, true
	}
	var next []Item
	switch {
	case present && had:
		next = slices.Clone(cur)
		next[i].Value = val
	case present:
		next = append(slices.Clip(cur), Item{Var: v, Value: val})
	case had:
		next = slices.Delete(slices.Clone(cur), i, i+1)
	default:
		return old, had
	}
	s.items.Store(&next)
	return old, had
}

// Private copy of the current items for a writer holding mu to change and publish
func (s *Store) snapshot() []Item {
	if p := s.items.Load(); p != nil {
		return slices.Clone(*p)
	}
	return nil
}

func indexOf(items []Item, v Variable) int {
	for i, it := range items {
		if it.Var == v {
			return i
		}
	}
	return -1
}
