package tools

import (
	"context"
	"sort"
	"sync"
)

type slotOrder struct {
	next uint64
	seq  map[string]map[string]uint64
}

var (
	slotOrderMu sync.Mutex
	slotOrders  = map[*Registry]*slotOrder{}
)

func (r *Registry) slotOrderLocked() *slotOrder {
	if slotOrders == nil {
		slotOrders = map[*Registry]*slotOrder{}
	}
	o := slotOrders[r]
	if o == nil {
		o = &slotOrder{seq: map[string]map[string]uint64{}}
		slotOrders[r] = o
	}
	return o
}

// Records a slot insertion so the name keeps its first registration position until it is removed
func (r *Registry) noteSlotWrite(scope, name string) {
	slotOrderMu.Lock()
	defer slotOrderMu.Unlock()
	o := r.slotOrderLocked()
	m := o.seq[scope]
	if m == nil {
		m = map[string]uint64{}
		o.seq[scope] = m
	}
	if _, ok := m[name]; !ok {
		o.next++
		m[name] = o.next
	}
}

// Forgets a removed name so a later registration of it appends like a Python dict key
func (r *Registry) noteSlotDelete(scope, name string) {
	slotOrderMu.Lock()
	defer slotOrderMu.Unlock()
	if m := r.slotOrderLocked().seq[scope]; m != nil {
		delete(m, name)
	}
}

// Slot names in insertion order with vanished names forgotten and unrecorded names appended by name
func (r *Registry) orderedSlotNamesLocked(scope string, slot map[string]*Def) []string {
	o := r.slotOrderLocked()
	m := o.seq[scope]
	if m == nil {
		m = map[string]uint64{}
		o.seq[scope] = m
	}
	for n := range m {
		if _, ok := slot[n]; !ok {
			delete(m, n)
		}
	}
	var unseen []string
	for n := range slot {
		if _, ok := m[n]; !ok {
			unseen = append(unseen, n)
		}
	}
	sort.Strings(unseen)
	for _, n := range unseen {
		o.next++
		m[n] = o.next
	}
	names := make([]string, 0, len(slot))
	for n := range slot {
		names = append(names, n)
	}
	sort.Slice(names, func(i, j int) bool { return m[names[i]] < m[names[j]] })
	return names
}

// Visible entries in merged dict order, global first then the profile and session overlays
func (r *Registry) orderedSnapshot(ctx context.Context, scope string) []*Def {
	e := r.ext()
	e.mu.RLock()
	defer e.mu.RUnlock()
	r.mu.RLock()
	defer r.mu.RUnlock()
	slotOrderMu.Lock()
	defer slotOrderMu.Unlock()
	var order []string
	merged := map[string]*Def{}
	add := func(slotScope string, slot map[string]*Def, global bool) {
		for _, n := range r.orderedSlotNamesLocked(slotScope, slot) {
			d := slot[n]
			if global && r.hiddenLocked(n, d) {
				continue
			}
			if _, ok := merged[n]; !ok {
				order = append(order, n)
			}
			merged[n] = d
		}
	}
	add("", r.tools, true)
	if scope != "" {
		add(scope, e.scoped[scope], false)
	}
	if s := SessionToolScope(ctx); s != "" {
		add(s, e.scoped[s], false)
	}
	out := make([]*Def, len(order))
	for i, n := range order {
		out[i] = merged[n]
	}
	return out
}

// AvailableToolsetNames lists visible toolsets in the order their first tool was registered
func (r *Registry) AvailableToolsetNames(ctx context.Context, env *Env) []string {
	seen := map[string]bool{}
	var out []string
	for _, d := range r.orderedSnapshot(ctx, scopeOf(env)) {
		if !seen[d.Toolset] {
			seen[d.Toolset] = true
			out = append(out, d.Toolset)
		}
	}
	return out
}
