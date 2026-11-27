package speech

import (
	"slices"
	"sync"
)

// Loaded models a local voice keeps at once
const residentMax = 3

// Model of a local voice that holds memory until closed
type Model interface {
	Close()
}

// Models a local voice keeps loaded by what they were loaded with, the least recently used closed once more than three are held, each
// closed after the request it serves
type Resident[T Model] struct {
	mu   sync.Mutex
	keys []string
	held map[string]T
}

// Model of the key, loaded now when it isn't held
func (r *Resident[T]) Load(key string, load func() (T, error)) (T, error) {
	r.mu.Lock()
	if model, ok := r.held[key]; ok {
		r.keys = append(slices.DeleteFunc(r.keys, func(k string) bool { return k == key }), key)
		r.mu.Unlock()
		return model, nil
	}
	r.mu.Unlock()
	model, err := load()
	if err != nil {
		return model, err
	}
	r.mu.Lock()
	if r.held == nil {
		r.held = map[string]T{}
	}
	if _, ok := r.held[key]; ok {
		r.keys = slices.DeleteFunc(r.keys, func(k string) bool { return k == key })
	}
	r.held[key] = model
	r.keys = append(r.keys, key)
	var evicted []T
	for len(r.keys) > residentMax {
		evicted = append(evicted, r.held[r.keys[0]])
		delete(r.held, r.keys[0])
		r.keys = r.keys[1:]
	}
	r.mu.Unlock()
	for _, old := range evicted {
		go old.Close()
	}
	return model, nil
}

// Closes every model held, how many there were
func (r *Resident[T]) Clear() int {
	r.mu.Lock()
	held := r.held
	r.keys, r.held = nil, nil
	r.mu.Unlock()
	for _, model := range held {
		go model.Close()
	}
	return len(held)
}
