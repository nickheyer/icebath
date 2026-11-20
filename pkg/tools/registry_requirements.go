package tools

import (
	"context"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

// Entries in merged dict order with the toolset checks overlaid by each entry's check, read under one lock
func (r *Registry) snapshotState(ctx context.Context, scope string) ([]*Def, map[string]func(*Env) bool) {
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
	entries := make([]*Def, len(order))
	checks := make(map[string]func(*Env) bool, len(e.toolsetChecks))
	for k, v := range e.toolsetChecks {
		checks[k] = v
	}
	for i, n := range order {
		entries[i] = merged[n]
		if entries[i].Check != nil {
			checks[entries[i].Toolset] = entries[i].Check
		}
	}
	return entries, checks
}

// ToolsetRequirements returns a TOOLSET_REQUIREMENTS compatible row per toolset from one consistent snapshot
func (r *Registry) ToolsetRequirements(ctx context.Context, scope string) map[string]map[string]any {
	entries, checks := r.snapshotState(ctx, ScopeKey(scope))
	out := map[string]map[string]any{}
	for toolset, members := range grouped(entries) {
		tools := make([]string, len(members))
		for i, d := range members {
			tools[i] = d.Name
		}
		var check any
		if fn := checks[toolset]; fn != nil {
			check = fn
		}
		envVars := uniqueEnv(members)
		if envVars == nil {
			envVars = []string{}
		}
		out[toolset] = map[string]any{"name": toolset, "env_vars": envVars, "check_fn": check, "setup_url": nil, "tools": tools}
	}
	return out
}

// ToolResult encodes the data object like json.dumps with ensure_ascii off
func ToolResult(data any) string {
	if data == nil {
		data = pyjson.NewObject()
	}
	s, err := pyjson.Dumps(data, pyjson.EnsureASCII(false))
	if err != nil {
		return ErrorResult(err)
	}
	return s
}
