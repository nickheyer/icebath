package tool

import (
	"context"
	"slices"
	"sort"
	"sync"
	"time"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/model/wire"
)

const defsMemoMax = 8

type defsKey struct {
	scope         Scope
	selection     string
	kanbanWorker  bool
	generation    uint64
	revision      config.Revision
	skip          bool
	contextLength int
	epoch         uint64
}

type defsEntry struct {
	defs   Definitions
	expiry time.Time
}

type defsMemo struct {
	mu      sync.Mutex
	order   []defsKey
	entries map[defsKey]defsEntry
}

type kanbanSelectionKey struct{}

func WithKanbanRequested(ctx context.Context, requested bool) context.Context {
	return context.WithValue(ctx, kanbanSelectionKey{}, requested)
}

func KanbanRequested(ctx context.Context) (requested, set bool) {
	v, ok := ctx.Value(kanbanSelectionKey{}).(bool)
	return v, ok
}

func (d Definitions) copy() Definitions {
	d.Tools = slices.Clone(d.Tools)
	d.Names = slices.Clone(d.Names)
	return d
}

func (m *defsMemo) live(k defsKey) (Definitions, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.entries[k]
	if !ok || !e.expiry.IsZero() && !time.Now().Before(e.expiry) {
		return Definitions{}, false
	}
	return e.defs.copy(), true
}

func (m *defsMemo) store(k defsKey, d Definitions, expiry time.Time) Definitions {
	m.mu.Lock()
	defer m.mu.Unlock()
	if e, ok := m.entries[k]; ok && (e.expiry.IsZero() || time.Now().Before(e.expiry)) {
		return e.defs.copy()
	}
	if _, present := m.entries[k]; !present {
		if len(m.order) >= defsMemoMax {
			delete(m.entries, m.order[0])
			m.order = m.order[1:]
		}
		m.order = append(m.order, k)
	}
	m.entries[k] = defsEntry{defs: d, expiry: expiry}
	return d.copy()
}

func (r *Registry) definitionsKey(ctx context.Context, s Selection, o DefinitionOptions) defsKey {
	k := defsKey{
		scope:         s.Scope,
		selection:     SelectionKey(s),
		kanbanWorker:  s.KanbanWorker,
		generation:    r.Generation(),
		skip:          o.SkipToolSearch,
		contextLength: o.ContextLength,
		epoch:         r.checkEpoch(),
	}
	if r.config != nil {
		if rev, ok := r.config.ConfigRevision(ctx); ok {
			k.revision = rev
		}
	}
	return k
}

func (r *Registry) Definitions(ctx context.Context, s Selection, o DefinitionOptions) (Definitions, error) {
	key := r.definitionsKey(ctx, s, o)
	if d, ok := r.defs.live(key); ok {
		return d, nil
	}
	d, expiry, err := r.computeDefinitions(ctx, s, o)
	if err != nil {
		return Definitions{}, err
	}
	return r.defs.store(key, d, expiry), nil
}

func (r *Registry) computeDefinitions(ctx context.Context, s Selection, o DefinitionOptions) (Definitions, time.Time, error) {
	at := s.Scope
	selected := r.selected(s)
	r.mu.RLock()
	entries := r.merged(at)
	r.mu.RUnlock()
	byName := make(map[string]*Entry, len(entries))
	for _, e := range entries {
		byName[e.Tool.Name] = e
	}
	names := make([]string, 0, len(selected))
	for n, on := range selected {
		if on {
			names = append(names, n)
		}
	}
	sort.Strings(names)
	ctx = WithKanbanRequested(ctx, slices.Contains(s.Enabled, "kanban"))
	pass := r.newPass(at, entries)
	var available []*Entry
	for _, n := range names {
		e := byName[n]
		if e == nil {
			continue
		}
		if e.Tool.Check != nil && !pass.ok(ctx, e.Tool.Check) {
			continue
		}
		available = append(available, e)
	}
	snapshot := make(Names, len(available))
	for _, e := range available {
		snapshot[e.Tool.Name] = true
	}
	var tools []wire.Tool
	var resolved []string
	for _, e := range available {
		t := wire.Tool{Name: e.Tool.Name, Description: e.Tool.Description, Parameters: e.Tool.Parameters}
		if e.Tool.DynamicSchema != nil {
			override, keep := e.Tool.DynamicSchema(ctx, snapshot)
			if !keep {
				continue
			}
			if override != nil {
				if override.Description != nil {
					t.Description = *override.Description
				}
				if override.Parameters != nil {
					t.Parameters = override.Parameters
				}
			}
		}
		resolved = append(resolved, t.Name)
		tools = append(tools, t)
	}
	for i := range tools {
		tools[i].Parameters = sanitizeParameters(tools[i].Name, tools[i].Parameters)
	}
	d := Definitions{Tools: tools, Names: resolved, Search: Assembly{Listing: "none"}}
	if !o.SkipToolSearch {
		cfg, err := r.searchConfig(ctx)
		if err != nil {
			return Definitions{}, time.Time{}, err
		}
		if cfg.enabled != "off" {
			out, asm, err := r.assemble(ctx, pass, at, tools, o.ContextLength, cfg)
			if err != nil {
				return Definitions{}, time.Time{}, err
			}
			d.Tools, d.Search = out, asm
		}
	}
	return d, pass.expiry, nil
}
