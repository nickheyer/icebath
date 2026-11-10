package config

import (
	"sync"

	"github.com/nickheyer/icebath/pkg/internal/pythread"
)

// One optional env entry under its variable name, the unit an ordered table or fill is made of
type NamedEnvVar struct {
	Name  string
	Entry EnvVar
}

// Fill armed on the table with the callback run once its entries are published
type optionalEnvFill struct {
	run       func() []NamedEnvVar
	published func([]NamedEnvVar)
}

// Insertion ordered optional env table that runs one armed fill before it is first read
type OptionalEnvTable struct {
	mu      sync.RWMutex
	entries map[string]EnvVar
	order   []string
	fillMu  sync.Mutex
	fill    *optionalEnvFill
	filling pythread.Local
}

// Saved table contents and armed fill a test puts back afterwards
type OptionalEnvState struct {
	entries map[string]EnvVar
	order   []string
	fill    *optionalEnvFill
}

// Table holding the declared entries in declaration order with no fill armed
func NewOptionalEnvTable(decl []NamedEnvVar) *OptionalEnvTable {
	t := &OptionalEnvTable{entries: make(map[string]EnvVar, len(decl))}
	for _, e := range decl {
		t.Set(e.Name, e.Entry)
	}
	return t
}

// Arms the fill the next read runs and the callback its published entries are handed to, nil disarms
func (t *OptionalEnvTable) Arm(fill func() []NamedEnvVar, published func([]NamedEnvVar)) {
	t.fillMu.Lock()
	defer t.fillMu.Unlock()
	if fill == nil {
		t.fill = nil
		return
	}
	t.fill = &optionalEnvFill{run: fill, published: published}
}

// Runs the armed fill holding no lock and publishes its entries unless another reader published first
func (t *OptionalEnvTable) ready() {
	t.fillMu.Lock()
	fill := t.fill
	t.fillMu.Unlock()
	if fill == nil {
		return
	}
	if active, _ := t.filling.Get(); active == true {
		return
	}
	t.filling.Set(true)
	entries := func() []NamedEnvVar {
		defer t.filling.Set(false)
		return fill.run()
	}()
	t.fillMu.Lock()
	defer t.fillMu.Unlock()
	if t.fill != fill {
		return
	}
	for _, e := range entries {
		t.Set(e.Name, e.Entry)
	}
	t.fill = nil
	if fill.published != nil {
		fill.published(entries)
	}
}

// Entry of a name after the armed fill ran, false when the table has none
func (t *OptionalEnvTable) Get(name string) (EnvVar, bool) {
	t.ready()
	return t.RawGet(name)
}

// Entry of a name after the armed fill ran, the zero entry when the table has none
func (t *OptionalEnvTable) Entry(name string) EnvVar {
	e, _ := t.Get(name)
	return e
}

// Reports whether the table holds a name after the armed fill ran
func (t *OptionalEnvTable) Has(name string) bool {
	_, ok := t.Get(name)
	return ok
}

// Names in insertion order after the armed fill ran
func (t *OptionalEnvTable) Names() []string {
	t.ready()
	return t.RawNames()
}

// Entry count after the armed fill ran
func (t *OptionalEnvTable) Len() int {
	t.ready()
	t.mu.RLock()
	defer t.mu.RUnlock()
	return len(t.order)
}

// Entries in insertion order after the armed fill ran
func (t *OptionalEnvTable) Items() []NamedEnvVar {
	t.ready()
	t.mu.RLock()
	defer t.mu.RUnlock()
	out := make([]NamedEnvVar, len(t.order))
	for i, name := range t.order {
		out[i] = NamedEnvVar{Name: name, Entry: t.entries[name]}
	}
	return out
}

// Plain map copy of the entries after the armed fill ran
func (t *OptionalEnvTable) Copy() map[string]EnvVar {
	t.ready()
	t.mu.RLock()
	defer t.mu.RUnlock()
	out := make(map[string]EnvVar, len(t.entries))
	for name, e := range t.entries {
		out[name] = e
	}
	return out
}

// Entry of a name as the table stands with no fill run, the read a fill itself makes
func (t *OptionalEnvTable) RawGet(name string) (EnvVar, bool) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	e, ok := t.entries[name]
	return e, ok
}

// Names in insertion order as the table stands with no fill run
func (t *OptionalEnvTable) RawNames() []string {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return append([]string(nil), t.order...)
}

// Stores an entry without running the fill, a new name going last and a known one keeping its place
func (t *OptionalEnvTable) Set(name string, entry EnvVar) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if _, ok := t.entries[name]; !ok {
		t.order = append(t.order, name)
	}
	t.entries[name] = entry
}

// Removes a name without running the fill
func (t *OptionalEnvTable) Delete(name string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if _, ok := t.entries[name]; !ok {
		return
	}
	delete(t.entries, name)
	for i, n := range t.order {
		if n == name {
			t.order = append(t.order[:i:i], t.order[i+1:]...)
			break
		}
	}
}

// Contents and armed fill as they stand, for a test that changes the table
func (t *OptionalEnvTable) Snapshot() OptionalEnvState {
	t.fillMu.Lock()
	fill := t.fill
	t.fillMu.Unlock()
	t.mu.RLock()
	defer t.mu.RUnlock()
	entries := make(map[string]EnvVar, len(t.entries))
	for name, e := range t.entries {
		entries[name] = e
	}
	return OptionalEnvState{entries: entries, order: append([]string(nil), t.order...), fill: fill}
}

// Puts back the contents and armed fill a snapshot captured
func (t *OptionalEnvTable) Restore(s OptionalEnvState) {
	t.fillMu.Lock()
	t.fill = s.fill
	t.fillMu.Unlock()
	t.mu.Lock()
	defer t.mu.Unlock()
	t.entries = make(map[string]EnvVar, len(s.entries))
	for name, e := range s.entries {
		t.entries[name] = e
	}
	t.order = append([]string(nil), s.order...)
}
