package nemorelay

import (
	"cmp"
	"errors"
	"maps"
	"slices"
)

// One named and prioritized registration payload
type registryRecord[T any] struct {
	name     string
	priority int
	payload  T
}

// Request intercept callable paired with its chain-breaking flag
type requestIntercept[F any] struct {
	breakChain bool
	callable   F
}

type registryStorage[T any] struct {
	entries    map[string]registryRecord[T]
	sortedKeys []string
}

// Copy-on-write registry ordered by priority then name
type sortedRegistry[T any] struct {
	storage *registryStorage[T]
}

// Adds an entry and rejects duplicate names
func (r *sortedRegistry[T]) register(entry registryRecord[T]) error {
	if r.storage != nil {
		if _, exists := r.storage.entries[entry.name]; exists {
			return errors.New(entry.name + " already exists")
		}
	}
	next := &registryStorage[T]{entries: map[string]registryRecord[T]{}}
	if r.storage != nil {
		next.entries = maps.Clone(r.storage.entries)
	}
	next.entries[entry.name] = entry
	next.resort()
	r.storage = next
	return nil
}

// Removes an entry and reports whether it existed
func (r *sortedRegistry[T]) deregister(name string) bool {
	if r.storage == nil {
		return false
	}
	if _, exists := r.storage.entries[name]; !exists {
		return false
	}
	next := &registryStorage[T]{entries: maps.Clone(r.storage.entries)}
	delete(next.entries, name)
	next.resort()
	r.storage = next
	return true
}

// Returns entries ordered by priority then name
func (r sortedRegistry[T]) sortedValues() []registryRecord[T] {
	if r.storage == nil {
		return nil
	}
	values := make([]registryRecord[T], 0, len(r.storage.sortedKeys))
	for _, key := range r.storage.sortedKeys {
		if entry, ok := r.storage.entries[key]; ok {
			values = append(values, entry)
		}
	}
	return values
}

func (r sortedRegistry[T]) isEmpty() bool {
	return r.storage == nil || len(r.storage.entries) == 0
}

func (r sortedRegistry[T]) values() []registryRecord[T] {
	if r.storage == nil {
		return nil
	}
	return slices.Collect(maps.Values(r.storage.entries))
}

func (s *registryStorage[T]) resort() {
	keys := slices.Collect(maps.Keys(s.entries))
	slices.SortFunc(keys, func(left, right string) int {
		if order := cmp.Compare(s.entries[left].priority, s.entries[right].priority); order != 0 {
			return order
		}
		return cmp.Compare(left, right)
	})
	s.sortedKeys = keys
}
