package tools

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

// ScopeKey canonicalizes a profile home into the key scoped registrations live under
func ScopeKey(path string) string {
	if path == "" {
		return ""
	}
	return home.Key(path)
}

type sessionScopeCtxKey struct{}

// WithSessionToolScope binds an execution owned tool overlay that reads merge over the profile registry
func WithSessionToolScope(ctx context.Context, scope string) context.Context {
	return context.WithValue(ctx, sessionScopeCtxKey{}, scope)
}

// CurrentSessionToolScope returns the session scope exactly as bound, empty when none is bound
func CurrentSessionToolScope(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	if s, ok := ctx.Value(sessionScopeCtxKey{}).(string); ok {
		return s
	}
	return ""
}

// SessionToolScope returns the overlay slot key of the bound session scope, keyed per read like registry writes
func SessionToolScope(ctx context.Context) string {
	return home.NormalizeScope(CurrentSessionToolScope(ctx))
}

// PluginOverridePolicy is one identity bearing operator opt in for a plugin generation
type PluginOverridePolicy struct {
	Allowed bool
}

// ErrOverrideDenied reports a plugin override attempt without operator opt in
var ErrOverrideDenied = errors.New("plugin override denied")

type policyKey struct {
	scope, namespace string
}

// registryExt carries the scoped state def.go's Registry struct does not hold
type registryExt struct {
	mu            sync.RWMutex
	scoped        map[string]map[string]*Def
	toolsetChecks map[string]func(*Env) bool
	aliases       map[string]string
	policies      map[policyKey]*PluginOverridePolicy
	moduleScopes  map[string]map[string]bool
	generation    int64
}

var (
	extMu   sync.Mutex
	extByID = map[*Registry]*registryExt{}
)

// ext returns the scoped extension for this registry, creating it on first use
func (r *Registry) ext() *registryExt {
	extMu.Lock()
	defer extMu.Unlock()
	e, ok := extByID[r]
	if !ok {
		e = &registryExt{
			scoped:        map[string]map[string]*Def{},
			toolsetChecks: map[string]func(*Env) bool{},
			aliases:       map[string]string{},
			policies:      map[policyKey]*PluginOverridePolicy{},
			moduleScopes:  map[string]map[string]bool{},
		}
		extByID[r] = e
	}
	return e
}

// Generation changes on every registration, removal, alias, or scope move
func (r *Registry) Generation() int64 {
	e := r.ext()
	e.mu.RLock()
	defer e.mu.RUnlock()
	r.mu.RLock()
	n := len(r.tools)
	r.mu.RUnlock()
	return e.generation*4096 + int64(n)
}

func (e *registryExt) bump() { e.generation++ }

// slot returns the registration map for a scope, the global map when scope is empty
func (r *Registry) slot(scope string, create bool) map[string]*Def {
	if scope == "" {
		return r.tools
	}
	e := r.ext()
	m := e.scoped[scope]
	if m == nil && create {
		m = map[string]*Def{}
		e.scoped[scope] = m
	}
	return m
}

// mergedTools returns global tools overlaid with one profile's tools and the context session overlay
func (r *Registry) mergedTools(ctx context.Context, scope string) map[string]*Def {
	e := r.ext()
	e.mu.RLock()
	defer e.mu.RUnlock()
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.mergedToolsLocked(ctx, e, scope)
}

// Merged view built while the caller holds both registry locks
func (r *Registry) mergedToolsLocked(ctx context.Context, e *registryExt, scope string) map[string]*Def {
	out := make(map[string]*Def, len(r.tools)+8)
	for k, v := range r.tools {
		if r.hiddenLocked(k, v) {
			continue
		}
		out[k] = v
	}
	for k, v := range e.scoped[scope] {
		out[k] = v
	}
	if s := SessionToolScope(ctx); s != "" {
		for k, v := range e.scoped[s] {
			out[k] = v
		}
	}
	return out
}

// Snapshot returns every visible entry for this scope sorted by name
func (r *Registry) Snapshot(ctx context.Context, scope string) []*Def {
	m := r.mergedTools(ctx, scope)
	out := make([]*Def, 0, len(m))
	for _, d := range m {
		out = append(out, d.resolveSchema())
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Lookup finds a tool through the session overlay, the profile overlay, then the global map
func (r *Registry) Lookup(ctx context.Context, scope, name string) *Def {
	return r.lookupEntry(ctx, scope, name).resolveSchema()
}

// Raw entry through the session overlay, the profile overlay, then the global map
func (r *Registry) lookupEntry(ctx context.Context, scope, name string) *Def {
	e := r.ext()
	e.mu.RLock()
	defer e.mu.RUnlock()
	if s := SessionToolScope(ctx); s != "" {
		if d := e.scoped[s][name]; d != nil {
			return d
		}
	}
	if d := e.scoped[scope][name]; d != nil {
		return d
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.globalLocked(name)
}

// Entry resolves a tool for the environment's profile home
func (r *Registry) Entry(ctx context.Context, env *Env, name string) *Def {
	scope := ""
	if env != nil {
		scope = ScopeKey(env.Home)
	}
	return r.Lookup(ctx, scope, name)
}

// SnapshotRegistration returns one slot's own entry without any global fallback
func (r *Registry) SnapshotRegistration(scope, name string) *Def {
	return r.registrationEntry(scope, name).resolveSchema()
}

// Raw entry of one slot without any global fallback
func (r *Registry) registrationEntry(scope, name string) *Def {
	e := r.ext()
	e.mu.RLock()
	defer e.mu.RUnlock()
	r.mu.RLock()
	defer r.mu.RUnlock()
	if scope == "" {
		return r.globalLocked(name)
	}
	return r.slot(scope, false)[name]
}

// grouped buckets entries by toolset in name order
func grouped(entries []*Def) map[string][]*Def {
	groups := map[string][]*Def{}
	for _, d := range entries {
		groups[d.Toolset] = append(groups[d.Toolset], d)
	}
	return groups
}

// RegisteredToolsetNames lists every toolset any visible entry belongs to
func (r *Registry) RegisteredToolsetNames(ctx context.Context, scope string) []string {
	g := grouped(r.Snapshot(ctx, scope))
	out := make([]string, 0, len(g))
	for k := range g {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// ToolNamesForToolset lists the visible tools registered under one toolset
func (r *Registry) ToolNamesForToolset(ctx context.Context, scope, toolset string) []string {
	var out []string
	for _, d := range r.Snapshot(ctx, scope) {
		if d.Toolset == toolset {
			out = append(out, d.Name)
		}
	}
	sort.Strings(out)
	return out
}

// RegisterToolsetAlias binds an alias to a canonical toolset, replacing a colliding target
func (r *Registry) RegisterToolsetAlias(alias, toolset string) {
	e := r.ext()
	e.mu.Lock()
	defer e.mu.Unlock()
	e.aliases[alias] = toolset
	e.bump()
}

// ToolsetAliases returns a copy of every alias
func (r *Registry) ToolsetAliases() map[string]string {
	e := r.ext()
	e.mu.RLock()
	defer e.mu.RUnlock()
	out := make(map[string]string, len(e.aliases))
	for k, v := range e.aliases {
		out[k] = v
	}
	return out
}

// ToolsetAliasTarget returns the canonical toolset an alias points at or empty
func (r *Registry) ToolsetAliasTarget(alias string) string {
	e := r.ext()
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.aliases[alias]
}

func (e *registryExt) dropAliasesFor(toolset string) {
	for a, t := range e.aliases {
		if t == toolset {
			delete(e.aliases, a)
		}
	}
}

// RegisterPluginOverridePolicy records a plugin namespace's operator opt in for one scope
func (r *Registry) RegisterPluginOverridePolicy(namespace string, allowed bool, scope string) *PluginOverridePolicy {
	e := r.ext()
	e.mu.Lock()
	defer e.mu.Unlock()
	p := &PluginOverridePolicy{Allowed: allowed}
	e.policies[policyKey{ScopeKey(scope), namespace}] = p
	if e.moduleScopes[namespace] == nil {
		e.moduleScopes[namespace] = map[string]bool{}
	}
	e.moduleScopes[namespace][ScopeKey(scope)] = true
	return p
}

// SnapshotPluginOverridePolicy returns one scope's policy without fallback
func (r *Registry) SnapshotPluginOverridePolicy(namespace, scope string) *PluginOverridePolicy {
	e := r.ext()
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.policies[policyKey{ScopeKey(scope), namespace}]
}

// RestorePluginOverridePolicy swaps a policy back only while the current one is still installed
func (r *Registry) RestorePluginOverridePolicy(namespace string, current, previous *PluginOverridePolicy, scope string) bool {
	e := r.ext()
	e.mu.Lock()
	defer e.mu.Unlock()
	k := policyKey{ScopeKey(scope), namespace}
	if e.policies[k] != current {
		return false
	}
	if previous == nil {
		delete(e.policies, k)
	} else {
		e.policies[k] = previous
	}
	return true
}

func (e *registryExt) overrideAllowed(scope, namespace string) bool {
	p := e.policies[policyKey{scope, namespace}]
	if p == nil && scope != "" {
		p = e.policies[policyKey{"", namespace}]
	}
	return p != nil && p.Allowed
}

// pluginNamespaceOf resolves a module path to the longest registered plugin namespace owning it
func (e *registryExt) pluginNamespaceOf(module string) string {
	best := ""
	for ns := range e.moduleScopes {
		if module == ns || strings.HasPrefix(module, ns+".") {
			if len(ns) > len(best) {
				best = ns
			}
		}
	}
	if best == "" && strings.HasPrefix(module, "icebath_plugins.") {
		parts := strings.SplitN(module, ".", 3)
		return parts[0] + "." + parts[1]
	}
	return best
}

// PluginScopeForModule returns the one profile scope a loaded plugin module is bound to
func (r *Registry) PluginScopeForModule(current, module string) (string, error) {
	e := r.ext()
	e.mu.RLock()
	defer e.mu.RUnlock()
	owner := e.pluginNamespaceOf(module)
	if owner == "" {
		owner = module
	}
	scopes := e.moduleScopes[owner]
	if len(scopes) == 0 {
		return "", nil
	}
	if scopes[ScopeKey(current)] {
		return ScopeKey(current), nil
	}
	if len(scopes) == 1 {
		for s := range scopes {
			return s, nil
		}
	}
	return "", &deregisterDeniedError{msg: fmt.Sprintf("Plugin module %s is active in multiple profiles and cannot register outside one of those scopes.", pyrepr.StrRepr(owner))}
}

// RegisterScoped registers a plugin owned tool into a profile overlay under the override policy
func (r *Registry) RegisterScoped(scope string, d Def) error {
	lazy := d.Schema == nil && d.SchemaBuilder != nil
	if err := validateRegistrationSchema(d); !lazy && err != nil {
		return err
	}
	if d.Name == "" || d.Handler == nil {
		return fmt.Errorf("tool %q: registration requires name, schema, and handler", d.Name)
	}
	d.schemaOnce = nil
	if lazy {
		d.schemaOnce = new(sync.Once)
	} else if d.Description == "" {
		if s, ok := d.Schema["description"].(string); ok {
			d.Description = s
		}
	}
	scope = ScopeKey(scope)
	e := r.ext()
	e.mu.Lock()
	defer e.mu.Unlock()
	owner := ""
	if d.Plugin != "" {
		owner = e.pluginNamespaceOf(d.Plugin)
	}
	if scope == "" && owner != "" {
		resolved, err := e.callerScopeLocked(owner, "")
		if err != nil {
			return err
		}
		scope = resolved
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	target := r.slot(scope, true)
	var existing *Def
	if scope != "" {
		existing = target[d.Name]
	}
	if existing == nil {
		existing = r.globalLocked(d.Name)
	}
	denied := owner != "" && !e.overrideAllowed(scope, owner)
	shadowsGlobal := owner != "" && scope != "" && target[d.Name] == nil && r.globalLocked(d.Name) != nil
	if shadowsGlobal {
		if !d.Override {
			return rejectGlobalShadow(owner, d.Name)
		}
		if denied {
			return &overrideDeniedError{owner: owner, name: d.Name}
		}
	}
	if existing != nil && existing.Toolset != d.Toolset {
		if err := crossToolsetVerdict(d, owner, existing.Toolset, denied); err != nil {
			return err
		}
	}
	copy := d
	target[d.Name] = &copy
	r.noteSlotWrite(scope, d.Name)
	if scope == "" && d.Check != nil {
		if _, ok := e.toolsetChecks[d.Toolset]; !ok {
			e.toolsetChecks[d.Toolset] = d.Check
		}
	}
	e.bump()
	return nil
}

// Deregister removes a tool from one scope under the plugin ownership rules
func (r *Registry) Deregister(name, scope, callerPlugin string) error {
	return r.DeregisterIn(context.Background(), name, scope, callerPlugin)
}

// DeregisterIn removes a tool, judging toolset survivors through the home and session overlays of ctx
func (r *Registry) DeregisterIn(ctx context.Context, name, scope, callerPlugin string) error {
	scope = ScopeKey(scope)
	e := r.ext()
	e.mu.Lock()
	defer e.mu.Unlock()
	callerOwner := ""
	if callerPlugin != "" {
		callerOwner = e.pluginNamespaceOf(callerPlugin)
	}
	if callerOwner != "" {
		resolved, err := e.callerScopeLocked(callerOwner, scope)
		if err != nil {
			return err
		}
		scope = resolved
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	target := r.slot(scope, false)
	entry := target[name]
	if scope == "" && r.hiddenLocked(name, entry) {
		entry = nil
	}
	if entry == nil {
		if scope != "" && callerOwner != "" && r.globalLocked(name) != nil {
			return &deregisterDeniedError{msg: fmt.Sprintf("Scoped plugin module %s cannot deregister process-global tool %s; register a scoped override instead.",
				pyrepr.StrRepr(callerPlugin), pyrepr.StrRepr(name))}
		}
		return nil
	}
	if !strings.HasPrefix(entry.Toolset, "mcp-") && callerOwner != "" {
		entryOwner := ""
		if entry.Plugin != "" {
			entryOwner = e.pluginNamespaceOf(entry.Plugin)
		}
		same := entryOwner != "" && entryOwner == callerOwner
		if !same && !e.overrideAllowed(scope, callerOwner) {
			return rejectForeignDeregister(callerPlugin, name, entry.Toolset)
		}
	}
	delete(target, name)
	r.noteSlotDelete(scope, name)
	if scope != "" && len(target) == 0 {
		delete(e.scoped, scope)
	}
	if len(r.toolsetEntriesLocked(ctx, entry.Toolset, scope)) == 0 {
		delete(e.toolsetChecks, entry.Toolset)
		e.dropAliasesFor(entry.Toolset)
	}
	e.bump()
	return nil
}

// Entries of one toolset in merged dict order, an empty scope merging the home and session overlays of ctx
func (r *Registry) toolsetEntriesLocked(ctx context.Context, toolset, scope string) []*Def {
	if ctx == nil {
		ctx = context.Background()
	}
	e := r.ext()
	overlay := scope
	if overlay == "" {
		overlay = ScopeKey(home.Dir(ctx))
	}
	session := ""
	if scope == "" {
		session = SessionToolScope(ctx)
	}
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
	add(overlay, e.scoped[overlay], false)
	if session != "" {
		add(session, e.scoped[session], false)
	}
	var out []*Def
	for _, n := range order {
		if merged[n].Toolset == toolset {
			out = append(out, merged[n])
		}
	}
	return out
}

// MoveToScope hands process global registrations to one profile overlay
func (r *Registry) MoveToScope(names []string, scope string) {
	scope = ScopeKey(scope)
	e := r.ext()
	e.mu.Lock()
	defer e.mu.Unlock()
	r.mu.Lock()
	defer r.mu.Unlock()
	moved := false
	for _, n := range names {
		if d := r.globalLocked(n); d != nil {
			delete(r.tools, n)
			r.noteSlotDelete("", n)
			r.slot(scope, true)[n] = d
			r.noteSlotWrite(scope, n)
			moved = true
		}
	}
	if moved {
		e.bump()
	}
}

// RestoreRegistration puts a previous entry back only while the current one is still installed
func (r *Registry) RestoreRegistration(name string, current, previous *Def, scope string) bool {
	return r.RestoreRegistrationIn(context.Background(), name, current, previous, scope)
}

// RestoreRegistrationIn restores like RestoreRegistration with survivors read through the overlays of ctx
func (r *Registry) RestoreRegistrationIn(ctx context.Context, name string, current, previous *Def, scope string) bool {
	scope = ScopeKey(scope)
	e := r.ext()
	e.mu.Lock()
	defer e.mu.Unlock()
	r.mu.Lock()
	defer r.mu.Unlock()
	target := r.slot(scope, true)
	if target[name] != current {
		return false
	}
	if previous == nil {
		delete(target, name)
		r.noteSlotDelete(scope, name)
	} else {
		target[name] = previous
		r.noteSlotWrite(scope, name)
	}
	if scope != "" && len(target) == 0 {
		delete(e.scoped, scope)
	}
	affected := map[string]bool{current.Toolset: true}
	if previous != nil {
		affected[previous.Toolset] = true
	}
	for ts := range affected {
		surviving := r.toolsetEntriesLocked(ctx, ts, scope)
		if scope == "" {
			var check func(*Env) bool
			for _, d := range surviving {
				if d.Check != nil {
					check = d.Check
					break
				}
			}
			if check == nil {
				delete(e.toolsetChecks, ts)
			} else {
				e.toolsetChecks[ts] = check
			}
		}
		inOverlay := false
		for _, m := range e.scoped {
			for _, d := range m {
				if d.Toolset == ts {
					inOverlay = true
				}
			}
		}
		if len(surviving) == 0 && !inOverlay {
			e.dropAliasesFor(ts)
		}
	}
	e.bump()
	return true
}

// ToolsetChecks returns the first check function recorded per toolset
func (r *Registry) ToolsetChecks() map[string]func(*Env) bool {
	e := r.ext()
	e.mu.RLock()
	defer e.mu.RUnlock()
	out := make(map[string]func(*Env) bool, len(e.toolsetChecks))
	for k, v := range e.toolsetChecks {
		out[k] = v
	}
	return out
}

// AllToolNames lists every visible tool name sorted
func (r *Registry) AllToolNames(ctx context.Context, scope string) []string {
	s := r.Snapshot(ctx, scope)
	out := make([]string, len(s))
	for i, d := range s {
		out[i] = d.Name
	}
	return out
}

// ToolsetForTool returns the toolset a visible tool belongs to or empty
func (r *Registry) ToolsetForTool(ctx context.Context, scope, name string) string {
	if d := r.Lookup(ctx, scope, name); d != nil {
		return d.Toolset
	}
	return ""
}

// Emoji returns a tool's emoji or the fallback
func (r *Registry) Emoji(ctx context.Context, scope, name, fallback string) string {
	if d := r.Lookup(ctx, scope, name); d != nil && d.Emoji != "" {
		return d.Emoji
	}
	return fallback
}

// ToolToToolsetMap maps every visible tool to its toolset
func (r *Registry) ToolToToolsetMap(ctx context.Context, scope string) map[string]string {
	out := map[string]string{}
	for _, d := range r.Snapshot(ctx, scope) {
		out[d.Name] = d.Toolset
	}
	return out
}

// MaxResultSize returns the per tool result cap or the fallback when the tool sets none
func (r *Registry) MaxResultSize(ctx context.Context, scope, name string, fallback int) int {
	if d := r.Lookup(ctx, scope, name); d != nil && d.MaxResultChars != 0 {
		return d.MaxResultChars
	}
	return fallback
}

// Schema returns a visible tool's raw schema bypassing availability checks
func (r *Registry) Schema(ctx context.Context, scope, name string) map[string]any {
	if d := r.Lookup(ctx, scope, name); d != nil {
		return d.Schema
	}
	return nil
}

// uniqueEnv unions required env names across entries in first seen order
func uniqueEnv(entries []*Def) []string {
	var out []string
	seen := map[string]bool{}
	for _, d := range entries {
		for _, v := range d.RequiresEnv {
			if !seen[v] {
				seen[v] = true
				out = append(out, v)
			}
		}
	}
	return out
}

// ToolsetInfo describes one toolset for display surfaces
type ToolsetInfo struct {
	Name         string
	Available    bool
	Tools        []string
	Description  string
	Requirements []string
	Check        func(*Env) bool
}

// AvailableToolsets reports availability, members, and requirements per visible toolset
func (r *Registry) AvailableToolsets(ctx context.Context, env *Env) map[string]ToolsetInfo {
	entries := r.Snapshot(ctx, scopeOf(env))
	checks := r.ToolsetChecks()
	for _, d := range entries {
		if d.Check != nil {
			if _, ok := checks[d.Toolset]; !ok {
				checks[d.Toolset] = d.Check
			}
		}
	}
	out := map[string]ToolsetInfo{}
	for ts, members := range grouped(entries) {
		names := make([]string, len(members))
		for i, m := range members {
			names[i] = m.Name
		}
		out[ts] = ToolsetInfo{
			Name: ts, Available: r.toolsetHasExposableTools(ctx, env, ts, entries),
			Tools: names, Requirements: uniqueEnv(members), Check: checks[ts],
		}
	}
	return out
}

// UnavailableToolset is one toolset no session can expose and why
type UnavailableToolset struct {
	Name    string
	EnvVars []string
	Tools   []string
}

// CheckToolAvailability splits visible toolsets into exposable names and unavailable detail
func (r *Registry) CheckToolAvailability(ctx context.Context, env *Env) ([]string, []UnavailableToolset) {
	entries := r.Snapshot(ctx, scopeOf(env))
	groups := grouped(entries)
	names := make([]string, 0, len(groups))
	for ts := range groups {
		names = append(names, ts)
	}
	sort.Strings(names)
	var available []string
	var unavailable []UnavailableToolset
	for _, ts := range names {
		if r.toolsetHasExposableTools(ctx, env, ts, entries) {
			available = append(available, ts)
			continue
		}
		tools := make([]string, len(groups[ts]))
		for i, d := range groups[ts] {
			tools[i] = d.Name
		}
		unavailable = append(unavailable, UnavailableToolset{Name: ts, EnvVars: groups[ts][0].RequiresEnv, Tools: tools})
	}
	return available, unavailable
}

// IsToolsetAvailable reports whether a toolset has at least one exposable tool
func (r *Registry) IsToolsetAvailable(ctx context.Context, env *Env, toolset string) bool {
	return r.toolsetHasExposableTools(ctx, env, toolset, r.Snapshot(ctx, scopeOf(env)))
}

func scopeOf(env *Env) string {
	if env == nil {
		return ""
	}
	return ScopeKey(env.Home)
}
