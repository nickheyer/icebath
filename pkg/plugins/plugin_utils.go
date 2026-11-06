package plugins

import (
	"sync"
	"sync/atomic"
)

// Thread safe lazy slot caching the first successfully built instance, a failed build is retried next call
type SingletonSlot[T any] struct {
	mu    sync.Mutex
	set   atomic.Bool
	value T
}

// Cached instance, running factory at most once under concurrent first calls
func (s *SingletonSlot[T]) Get(factory func() (T, error)) (T, error) {
	if s.set.Load() {
		return s.value, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.set.Load() {
		return s.value, nil
	}
	value, err := factory()
	if err != nil {
		var zero T
		return zero, err
	}
	s.value = value
	s.set.Store(true)
	return value, nil
}

// Cached instance without building it, false when unset
func (s *SingletonSlot[T]) Peek() (T, bool) {
	if !s.set.Load() {
		var zero T
		return zero, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.value, s.set.Load()
}

// Drops the cached instance so the next Get rebuilds it
func (s *SingletonSlot[T]) Reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	var zero T
	s.value = zero
	s.set.Store(false)
}

// Zero argument accessor over a factory that builds once and retries after a failure
type LazyAccessor[T any] struct {
	slot    SingletonSlot[T]
	factory func() (T, error)
}

// Accessor building factory lazily and thread safely on the first call
func LazySingleton[T any](factory func() (T, error)) *LazyAccessor[T] {
	return &LazyAccessor[T]{factory: factory}
}

// Shared instance, built by the first successful call
func (a *LazyAccessor[T]) Get() (T, error) { return a.slot.Get(a.factory) }

// Drops the instance so the next call rebuilds it
func (a *LazyAccessor[T]) Reset() { a.slot.Reset() }
