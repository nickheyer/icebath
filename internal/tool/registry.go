package tool

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/model/secret"
)

type Registry struct {
	mu            sync.RWMutex
	toolsets      []Toolset
	static        map[string]int
	config        *config.Store
	platforms     func(name string) bool
	plugins       PluginToolsets
	credentials   Credentials
	secrets       *secret.Scope
	global        *slotMap
	profiles      map[string]*slotMap
	sessions      map[string]*slotMap
	policies      map[policyKey]*OverridePolicy
	pluginScopes  map[string][]string
	toolsetChecks map[string]*Check
	aliases       []toolsetAlias
	generation    atomic.Uint64
	states        map[string]*State
	endHooks      []func(ctx context.Context, key string)
	resolveMu     sync.Mutex
	resolveMemo   map[resolveKey][]string
	warnMu        sync.Mutex
	warnedBundles map[string]bool
	warnedSaved   map[string]bool
	checks        checkCache
	mcp           MCPServers
	defs          defsMemo
}

type RegistryOptions struct {
	Toolsets    []Toolset
	Config      *config.Store
	Platforms   func(name string) bool
	Plugins     PluginToolsets
	Credentials Credentials
	Secrets     *secret.Scope
	MCP         MCPServers
}

type Entry struct {
	Tool  Tool
	Scope Scope
}

type OverridePolicy struct {
	allowed bool
}

type policyKey struct {
	profile string
	owner   string
}

type toolsetAlias struct {
	alias   string
	toolset string
}

type slotMap struct {
	order   []string
	entries map[string]*Entry
}

func newSlotMap() *slotMap {
	return &slotMap{entries: map[string]*Entry{}}
}

func (m *slotMap) get(name string) *Entry {
	if m == nil {
		return nil
	}
	return m.entries[name]
}

func (m *slotMap) set(name string, e *Entry) {
	if _, ok := m.entries[name]; !ok {
		m.order = append(m.order, name)
	}
	m.entries[name] = e
}

func (m *slotMap) del(name string) {
	if _, ok := m.entries[name]; !ok {
		return
	}
	delete(m.entries, name)
	m.order = slices.DeleteFunc(m.order, func(s string) bool { return s == name })
}

func (m *slotMap) len() int {
	if m == nil {
		return 0
	}
	return len(m.entries)
}

func (m *slotMap) list() []*Entry {
	if m == nil {
		return nil
	}
	out := make([]*Entry, 0, len(m.order))
	for _, name := range m.order {
		out = append(out, m.entries[name])
	}
	return out
}

func (s Scope) global() bool {
	return s.Profile == "" && s.Session == ""
}

func NewRegistry(o RegistryOptions) *Registry {
	r := &Registry{
		static:        map[string]int{},
		config:        o.Config,
		platforms:     o.Platforms,
		plugins:       o.Plugins,
		credentials:   o.Credentials,
		secrets:       o.Secrets,
		global:        newSlotMap(),
		profiles:      map[string]*slotMap{},
		sessions:      map[string]*slotMap{},
		policies:      map[policyKey]*OverridePolicy{},
		pluginScopes:  map[string][]string{},
		toolsetChecks: map[string]*Check{},
		states:        map[string]*State{},
		resolveMemo:   map[resolveKey][]string{},
		warnedBundles: map[string]bool{},
		warnedSaved:   map[string]bool{},
		checks:        newCheckCache(),
		mcp:           o.MCP,
		defs:          defsMemo{entries: map[defsKey]defsEntry{}},
	}
	for _, t := range o.Toolsets {
		if _, dup := r.static[t.Name]; dup {
			r.toolsets[r.static[t.Name]] = t.clone()
			continue
		}
		r.static[t.Name] = len(r.toolsets)
		r.toolsets = append(r.toolsets, t.clone())
	}
	return r
}

func (r *Registry) staticToolset(name string) (Toolset, bool) {
	i, ok := r.static[name]
	if !ok {
		return Toolset{}, false
	}
	return r.toolsets[i].clone(), true
}

func (r *Registry) slot(at Scope, create bool) *slotMap {
	switch {
	case at.Session != "":
		m := r.sessions[at.Session]
		if m == nil && create {
			m = newSlotMap()
			r.sessions[at.Session] = m
		}
		return m
	case at.Profile != "":
		m := r.profiles[at.Profile]
		if m == nil && create {
			m = newSlotMap()
			r.profiles[at.Profile] = m
		}
		return m
	}
	return r.global
}

func (r *Registry) dropEmptySlot(at Scope) {
	switch {
	case at.Session != "":
		if r.sessions[at.Session].len() == 0 {
			delete(r.sessions, at.Session)
		}
	case at.Profile != "":
		if r.profiles[at.Profile].len() == 0 {
			delete(r.profiles, at.Profile)
		}
	}
}

func (r *Registry) lookup(name string, at Scope) *Entry {
	if at.Session != "" {
		if e := r.sessions[at.Session].get(name); e != nil {
			return e
		}
	}
	if at.Profile != "" {
		if e := r.profiles[at.Profile].get(name); e != nil {
			return e
		}
	}
	return r.global.get(name)
}

func (r *Registry) merged(at Scope) []*Entry {
	out := r.global.list()
	index := make(map[string]int, len(out))
	for i, e := range out {
		index[e.Tool.Name] = i
	}
	overlay := func(m *slotMap) {
		for _, e := range m.list() {
			if i, ok := index[e.Tool.Name]; ok {
				out[i] = e
				continue
			}
			index[e.Tool.Name] = len(out)
			out = append(out, e)
		}
	}
	if at.Profile != "" {
		overlay(r.profiles[at.Profile])
	}
	if at.Session != "" {
		overlay(r.sessions[at.Session])
	}
	return out
}

func (r *Registry) toolsetEntries(toolset string, at Scope) []*Entry {
	var out []*Entry
	for _, e := range r.merged(at) {
		if e.Tool.Toolset == toolset {
			out = append(out, e)
		}
	}
	return out
}

func (r *Registry) inOverlays(toolset string) bool {
	for _, group := range []map[string]*slotMap{r.profiles, r.sessions} {
		for _, m := range group {
			for _, e := range m.entries {
				if e.Tool.Toolset == toolset {
					return true
				}
			}
		}
	}
	return false
}

func (r *Registry) dropAliases(toolset string) {
	r.aliases = slices.DeleteFunc(r.aliases, func(a toolsetAlias) bool { return a.toolset == toolset })
}

func (r *Registry) bump() {
	r.generation.Add(1)
}

func (r *Registry) pluginScopeOf(owner string) (Scope, error) {
	scopes := r.pluginScopes[owner]
	switch len(scopes) {
	case 0:
		return Scope{}, nil
	case 1:
		return Scope{Profile: scopes[0]}, nil
	}
	return Scope{}, &RegisterError{Owner: owner, Reason: MultiProfileOwner}
}

func (r *Registry) overrideAllowed(profile, owner string) bool {
	p := r.policies[policyKey{profile, owner}]
	if p == nil && profile != "" {
		p = r.policies[policyKey{"", owner}]
	}
	return p != nil && p.allowed
}

func validateParameters(name string, p json.RawMessage) error {
	trimmed := bytes.TrimSpace(p)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		return nil
	}
	if trimmed[0] == '{' && json.Valid(trimmed) {
		return nil
	}
	kind := "invalid JSON"
	if json.Valid(trimmed) {
		switch trimmed[0] {
		case '[':
			kind = "array"
		case '"':
			kind = "string"
		case 't', 'f':
			kind = "boolean"
		default:
			kind = "number"
		}
	}
	return fmt.Errorf("tool %q: %w, got %s", name, ErrBadSchema, kind)
}

func (r *Registry) Register(t Tool, at Scope) (*Entry, error) {
	if err := validateParameters(t.Name, t.Parameters); err != nil {
		return nil, err
	}
	owner := t.Owner
	r.mu.Lock()
	defer r.mu.Unlock()
	if at.global() && owner != "" {
		derived, err := r.pluginScopeOf(owner)
		if err != nil {
			return nil, err
		}
		at = derived
	}
	target := r.slot(at, false)
	existing := r.lookup(t.Name, at)
	denied := owner != "" && !r.overrideAllowed(at.Profile, owner)
	if owner != "" && !at.global() && target.get(t.Name) == nil && r.global.get(t.Name) != nil {
		if !t.Override {
			slog.Error(fmt.Sprintf("Tool registration REJECTED: plugin %q attempted to shadow global tool %q without override", owner, t.Name))
			return nil, &RegisterError{Tool: t.Name, Toolset: t.Toolset, Existing: r.global.get(t.Name).Tool.Toolset, Owner: owner, Reason: Shadow, Global: true}
		}
		if denied {
			return nil, &RegisterError{Tool: t.Name, Toolset: t.Toolset, Existing: r.global.get(t.Name).Tool.Toolset, Owner: owner, Reason: OverrideDenied}
		}
	}
	if existing != nil && existing.Tool.Toolset != t.Toolset {
		if !t.Override {
			slog.Error(fmt.Sprintf("Tool registration REJECTED: %q (toolset %q) would shadow existing tool from toolset %q. Register it with override if the replacement is intentional, or deregister the existing tool first.", t.Name, t.Toolset, existing.Tool.Toolset))
			return nil, &RegisterError{Tool: t.Name, Toolset: t.Toolset, Existing: existing.Tool.Toolset, Owner: owner, Reason: Shadow}
		}
		if denied {
			slog.Error(fmt.Sprintf("Tool registration REJECTED: plugin %q attempted to override built-in tool %q (existing toolset %q) without operator opt-in. Set plugins.entries.<plugin_id>.allow_tool_override: true in config.yaml to allow it.", owner, t.Name, existing.Tool.Toolset))
			return nil, &RegisterError{Tool: t.Name, Toolset: t.Toolset, Existing: existing.Tool.Toolset, Owner: owner, Reason: OverrideDenied}
		}
		slog.Info(fmt.Sprintf("Tool %q: toolset %q overriding existing toolset %q (override opt-in)", t.Name, t.Toolset, existing.Tool.Toolset))
	}
	t.RequiresEnv = slices.Clone(t.RequiresEnv)
	e := &Entry{Tool: t, Scope: at}
	r.slot(at, true).set(t.Name, e)
	if at.global() && t.Check != nil && r.toolsetChecks[t.Toolset] == nil {
		r.toolsetChecks[t.Toolset] = t.Check
	}
	r.bump()
	return e, nil
}

func (r *Registry) Deregister(name string, at Scope, owner string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	caller := ""
	if owner != "" {
		s, err := r.pluginScopeOf(owner)
		if err != nil {
			return err
		}
		caller = s.Profile
		if !at.global() && (at.Session != "" || at.Profile != caller) {
			return &RegisterError{Tool: name, Owner: owner, Reason: ForeignScope, Remove: true}
		}
	}
	if at.global() {
		at = Scope{Profile: caller}
	}
	e := r.slot(at, false).get(name)
	if e == nil {
		if !at.global() && owner != "" && r.global.get(name) != nil {
			return &RegisterError{Tool: name, Owner: owner, Reason: ForeignScope, Global: true, Remove: true}
		}
		return nil
	}
	if !strings.HasPrefix(e.Tool.Toolset, "mcp-") {
		same := e.Tool.Owner != "" && e.Tool.Owner == owner
		if owner != "" && !same && !r.overrideAllowed(caller, owner) {
			slog.Error(fmt.Sprintf("Tool deregistration REJECTED: plugin %q attempted to remove tool %q (toolset %q) it does not own, without operator opt-in. Set plugins.entries.%s.allow_tool_override: true in config.yaml to allow it.", owner, name, e.Tool.Toolset, owner))
			return &RegisterError{Tool: name, Existing: e.Tool.Toolset, Owner: owner, Reason: OverrideDenied, Remove: true}
		}
	}
	r.slot(at, false).del(name)
	r.dropEmptySlot(at)
	if len(r.toolsetEntries(e.Tool.Toolset, at)) == 0 {
		delete(r.toolsetChecks, e.Tool.Toolset)
		r.dropAliases(e.Tool.Toolset)
	}
	r.bump()
	slog.Debug("Deregistered tool: " + name)
	return nil
}

func (r *Registry) MoveToScope(names []string, at Scope) {
	if at.global() {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	var moved []*Entry
	for _, name := range names {
		if e := r.global.get(name); e != nil {
			r.global.del(name)
			moved = append(moved, &Entry{Tool: e.Tool, Scope: at})
		}
	}
	if len(moved) == 0 {
		return
	}
	target := r.slot(at, true)
	for _, e := range moved {
		target.set(e.Tool.Name, e)
	}
	r.bump()
}

func (r *Registry) Restore(name string, current, previous *Entry, at Scope) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	target := r.slot(at, true)
	if target.get(name) != current {
		r.dropEmptySlot(at)
		return false
	}
	if previous == nil {
		target.del(name)
	} else {
		target.set(name, previous)
	}
	r.dropEmptySlot(at)
	affected := []string{current.Tool.Toolset}
	if previous != nil && previous.Tool.Toolset != current.Tool.Toolset {
		affected = append(affected, previous.Tool.Toolset)
	}
	for _, toolset := range affected {
		surviving := r.toolsetEntries(toolset, at)
		var check *Check
		for _, e := range surviving {
			if e.Tool.Check != nil {
				check = e.Tool.Check
				break
			}
		}
		if at.global() {
			if check == nil {
				delete(r.toolsetChecks, toolset)
			} else {
				r.toolsetChecks[toolset] = check
			}
		}
		if len(surviving) == 0 && !r.inOverlays(toolset) {
			r.dropAliases(toolset)
		}
	}
	r.bump()
	slog.Debug("Restored tool registration: " + name)
	return true
}

func (r *Registry) RegisterAlias(alias, toolset string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i, a := range r.aliases {
		if a.alias != alias {
			continue
		}
		if a.toolset != toolset {
			slog.Warn(fmt.Sprintf("Toolset alias collision: '%s' (%s) overwritten by %s", alias, a.toolset, toolset))
		}
		r.aliases[i].toolset = toolset
		r.bump()
		return
	}
	r.aliases = append(r.aliases, toolsetAlias{alias: alias, toolset: toolset})
	r.bump()
}

func (r *Registry) aliasTarget(alias string) string {
	for _, a := range r.aliases {
		if a.alias == alias {
			return a.toolset
		}
	}
	return ""
}

func (r *Registry) SetOverridePolicy(owner string, at Scope, allowed bool) *OverridePolicy {
	r.mu.Lock()
	defer r.mu.Unlock()
	p := &OverridePolicy{allowed: allowed}
	r.policies[policyKey{at.Profile, owner}] = p
	if !slices.Contains(r.pluginScopes[owner], at.Profile) {
		r.pluginScopes[owner] = append(r.pluginScopes[owner], at.Profile)
	}
	return p
}

func (r *Registry) OverridePolicy(owner string, at Scope) *OverridePolicy {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.policies[policyKey{at.Profile, owner}]
}

func (r *Registry) RestoreOverridePolicy(owner string, at Scope, current, previous *OverridePolicy) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	key := policyKey{at.Profile, owner}
	if r.policies[key] != current {
		return false
	}
	if previous == nil {
		delete(r.policies, key)
	} else {
		r.policies[key] = previous
	}
	return true
}

func (r *Registry) Lookup(name string, at Scope) (*Entry, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	e := r.lookup(name, at)
	return e, e != nil
}

func (r *Registry) Slot(name string, at Scope) (*Entry, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	e := r.slot(at, false).get(name)
	return e, e != nil
}

func (r *Registry) Entries(at Scope) []*Entry {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.merged(at)
}

func (r *Registry) Emoji(name string, at Scope) string {
	if e, ok := r.Lookup(name, at); ok && e.Tool.Emoji != "" {
		return e.Tool.Emoji
	}
	return DefaultEmoji
}

func (r *Registry) Generation() uint64 {
	return r.generation.Load()
}

func (r *Registry) SessionState(key string) *State {
	r.mu.Lock()
	defer r.mu.Unlock()
	st := r.states[key]
	if st == nil {
		st = NewState()
		r.states[key] = st
	}
	return st
}

func (r *Registry) OnSessionEnd(fn func(ctx context.Context, key string)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.endHooks = append(r.endHooks, fn)
}

func (r *Registry) EndSession(ctx context.Context, key string) {
	r.mu.Lock()
	overlay := r.sessions[key]
	delete(r.sessions, key)
	st := r.states[key]
	delete(r.states, key)
	hooks := slices.Clone(r.endHooks)
	if overlay.len() > 0 {
		dropped := map[string]bool{}
		for _, e := range overlay.list() {
			if dropped[e.Tool.Toolset] {
				continue
			}
			dropped[e.Tool.Toolset] = true
			if len(r.toolsetEntries(e.Tool.Toolset, Scope{})) == 0 && !r.inOverlays(e.Tool.Toolset) {
				r.dropAliases(e.Tool.Toolset)
			}
		}
		r.bump()
	}
	r.mu.Unlock()
	if st != nil {
		st.Close(ctx)
	}
	for _, fn := range hooks {
		fn(ctx, key)
	}
}
