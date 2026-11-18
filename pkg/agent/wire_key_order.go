package agent

import (
	"reflect"
	"sort"

	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/state"
)

// Dict key order of one API message copy following insertion semantics through the request builder steps
type wireKeyTrack struct {
	keys []string
}

func newWireKeyTrack(src []string, msg map[string]any) *wireKeyTrack {
	t := &wireKeyTrack{keys: append([]string(nil), src...)}
	t.sync(msg)
	return t
}

// Drops keys a step removed and appends the keys it added, preferred keys first and the rest sorted
func (t *wireKeyTrack) sync(msg map[string]any, preferred ...string) {
	kept := make([]string, 0, len(msg))
	listed := make(map[string]bool, len(t.keys))
	for _, k := range t.keys {
		listed[k] = true
		if _, ok := msg[k]; ok {
			kept = append(kept, k)
		}
	}
	var added []string
	for _, k := range preferred {
		if _, ok := msg[k]; ok && !listed[k] {
			added = append(added, k)
			listed[k] = true
		}
	}
	var rest []string
	for k := range msg {
		if !listed[k] && k != state.WireKeyOrder {
			rest = append(rest, k)
		}
	}
	sort.Strings(rest)
	t.keys = append(append(kept, added...), rest...)
}

// Moves key to the end when a step popped it and set a new value
func (t *wireKeyTrack) reinserted(msg map[string]any, key string, before any) {
	after, ok := msg[key]
	if !ok || before == nil || sameListValue(before, after) {
		return
	}
	for i, k := range t.keys {
		if k == key {
			t.keys = append(append(t.keys[:i:i], t.keys[i+1:]...), key)
			return
		}
	}
}

// Whether two list values are the same list
func sameListValue(a, b any) bool {
	x, okA := a.([]any)
	y, okB := b.([]any)
	if !okA || !okB {
		return false
	}
	return len(x) == len(y) && (len(x) == 0 || &x[0] == &y[0])
}

// Records the tracked key order on the copy when the body encoder would place its keys differently
func (t *wireKeyTrack) attach(msg map[string]any) {
	delete(msg, state.WireKeyOrder)
	t.sync(msg)
	if provider.ChatWireNeedsKeyOrder(msg, t.keys) {
		msg[state.WireKeyOrder] = append([]string(nil), t.keys...)
	}
}

// Copies of the messages without their recorded key order and a function putting it back on returned copies
func withoutWireKeyOrders(msgs []map[string]any) ([]map[string]any, func([]map[string]any) []map[string]any) {
	recorded := map[uintptr][]string{}
	out := make([]map[string]any, len(msgs))
	for i, m := range msgs {
		keys, ok := m[state.WireKeyOrder].([]string)
		if !ok {
			out[i] = m
			continue
		}
		c := make(map[string]any, len(m))
		for k, v := range m {
			if k != state.WireKeyOrder {
				c[k] = v
			}
		}
		out[i] = c
		recorded[reflect.ValueOf(c).Pointer()] = keys
	}
	restore := func(selected []map[string]any) []map[string]any {
		for _, m := range selected {
			if m == nil {
				continue
			}
			if keys, ok := recorded[reflect.ValueOf(m).Pointer()]; ok {
				m[state.WireKeyOrder] = keys
			}
		}
		return selected
	}
	return out, restore
}
