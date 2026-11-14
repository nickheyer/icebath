package tool

import (
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strings"
)

type resolveKey struct {
	name         string
	withRegistry bool
	generation   uint64
	at           Scope
}

const resolveMemoMax = 256

func setOf(items ...[]string) map[string]bool {
	out := map[string]bool{}
	for _, list := range items {
		for _, s := range list {
			out[s] = true
		}
	}
	return out
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k, v := range m {
		if v {
			out = append(out, k)
		}
	}
	slices.Sort(out)
	return out
}

func (r *Registry) sessionGated(name string) bool {
	t, ok := r.staticToolset(name)
	return ok && len(t.Platforms) > 0
}

func (r *Registry) namesForToolset(toolset string, at Scope) []string {
	var out []string
	for _, e := range r.merged(at) {
		if e.Tool.Toolset == toolset {
			out = append(out, e.Tool.Name)
		}
	}
	slices.Sort(out)
	return out
}

func (r *Registry) pluginToolsetNames(at Scope) []string {
	seen := map[string]bool{}
	for _, e := range r.merged(at) {
		if _, static := r.static[e.Tool.Toolset]; !static {
			seen[e.Tool.Toolset] = true
		}
	}
	return sortedKeys(seen)
}

func (r *Registry) displayAlias(toolset string) string {
	for _, a := range r.aliases {
		if _, static := r.static[a.alias]; a.toolset == toolset && !static {
			return a.alias
		}
	}
	return ""
}

func (r *Registry) pluginDisplayNames(at Scope) []string {
	var out []string
	for _, name := range r.pluginToolsetNames(at) {
		if alias := r.displayAlias(name); alias != "" {
			out = append(out, alias)
			continue
		}
		out = append(out, name)
	}
	return out
}

func (r *Registry) toolsetNames(at Scope) []string {
	names := setOf(r.pluginDisplayNames(at))
	for _, t := range r.toolsets {
		names[t.Name] = true
	}
	return sortedKeys(names)
}

func (r *Registry) ToolsetNames(at Scope) []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.toolsetNames(at)
}

func (r *Registry) toolsetLocked(name string, at Scope, withRegistry bool) (Toolset, bool) {
	static, isStatic := r.staticToolset(name)
	if !withRegistry {
		return static, isStatic
	}
	if isStatic {
		tools := setOf(static.Tools, r.namesForToolset(name, at))
		if target := r.aliasTarget(name); target != "" && target != name {
			for _, n := range r.namesForToolset(target, at) {
				tools[n] = true
			}
		}
		static.Tools = sortedKeys(tools)
		return static, true
	}
	if slices.Contains(r.pluginToolsetNames(at), name) {
		description := "Plugin toolset: " + name
		if alias := r.displayAlias(name); alias != "" {
			description = fmt.Sprintf("MCP server '%s' tools", alias)
		}
		return Toolset{Name: name, Description: description, Tools: r.namesForToolset(name, at)}, true
	}
	target := r.aliasTarget(name)
	if target == "" {
		return Toolset{}, false
	}
	return Toolset{Name: name, Description: fmt.Sprintf("MCP server '%s' tools", name), Tools: r.namesForToolset(target, at)}, true
}

func (r *Registry) Toolset(name string, at Scope, withRegistry bool) (Toolset, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.toolsetLocked(name, at, withRegistry)
}

func (r *Registry) pluginPlatformBundle(name string, at Scope) []string {
	platform, ok := strings.CutPrefix(name, bundlePrefix)
	if !ok || r.platforms == nil || !r.platforms(platform) {
		return nil
	}
	tools := setOf(CoreTools())
	for _, e := range r.merged(at) {
		if e.Tool.Toolset == platform {
			tools[e.Tool.Name] = true
		}
	}
	return sortedKeys(tools)
}

func (r *Registry) resolveWalk(name string, visited map[string]bool, at Scope, withRegistry bool) []string {
	if name == "all" || name == "*" {
		all := map[string]bool{}
		for _, ts := range r.toolsetNames(at) {
			if r.sessionGated(ts) {
				continue
			}
			for _, t := range r.resolveWalk(ts, maps.Clone(visited), at, withRegistry) {
				all[t] = true
			}
		}
		return sortedKeys(all)
	}
	if visited[name] {
		return nil
	}
	visited[name] = true
	ts, ok := r.toolsetLocked(name, at, withRegistry)
	if !ok {
		if withRegistry {
			return r.pluginPlatformBundle(name, at)
		}
		return nil
	}
	tools := setOf(ts.Tools)
	for _, inc := range ts.Includes {
		for _, t := range r.resolveWalk(inc, visited, at, withRegistry) {
			tools[t] = true
		}
	}
	return sortedKeys(tools)
}

func (r *Registry) resolveLocked(name string, at Scope, withRegistry bool) []string {
	memoized := name != "all" && name != "*"
	if memoized {
		if _, found := r.toolsetLocked(name, at, withRegistry); !found {
			memoized = false
		}
	}
	if !memoized {
		return r.resolveWalk(name, map[string]bool{}, at, withRegistry)
	}
	key := resolveKey{name: name, withRegistry: withRegistry, generation: r.Generation(), at: at}
	r.resolveMu.Lock()
	cached, ok := r.resolveMemo[key]
	r.resolveMu.Unlock()
	if ok {
		return slices.Clone(cached)
	}
	out := r.resolveWalk(name, map[string]bool{}, at, withRegistry)
	r.resolveMu.Lock()
	if len(r.resolveMemo) >= resolveMemoMax {
		clear(r.resolveMemo)
	}
	r.resolveMemo[key] = slices.Clone(out)
	r.resolveMu.Unlock()
	return out
}

func (r *Registry) Resolve(name string, at Scope, withRegistry bool) []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.resolveLocked(name, at, withRegistry)
}

func (r *Registry) Toolsets(at Scope) []Toolset {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Toolset, 0, len(r.toolsets))
	index := map[string]int{}
	for _, t := range r.toolsets {
		index[t.Name] = len(out)
		out = append(out, t.clone())
	}
	for _, display := range r.pluginDisplayNames(at) {
		if _, present := index[display]; present {
			continue
		}
		if ts, ok := r.toolsetLocked(display, at, true); ok {
			ts.Name = display
			index[display] = len(out)
			out = append(out, ts)
		}
	}
	for _, a := range r.aliases {
		if _, static := r.static[a.alias]; !static {
			continue
		}
		if ts, ok := r.toolsetLocked(a.alias, at, true); ok {
			out[index[a.alias]] = ts
		}
	}
	return out
}

func (r *Registry) allToolsetNames(at Scope) []string {
	names := make([]string, 0, len(r.toolsets))
	seen := map[string]bool{}
	for _, t := range r.toolsets {
		names = append(names, t.Name)
		seen[t.Name] = true
	}
	for _, display := range r.pluginDisplayNames(at) {
		if seen[display] {
			continue
		}
		if _, ok := r.toolsetLocked(display, at, true); ok {
			seen[display] = true
			names = append(names, display)
		}
	}
	return names
}

func (r *Registry) validLocked(name string, at Scope) bool {
	if name == "all" || name == "*" {
		return true
	}
	if _, ok := r.static[name]; ok {
		return true
	}
	return slices.Contains(r.pluginToolsetNames(at), name) || r.aliasTarget(name) != ""
}

func (r *Registry) Valid(name string, at Scope) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.validLocked(name, at)
}

func (r *Registry) bundleNonCore(name string, at Scope) []string {
	core := setOf(CoreTools())
	ts, ok := r.toolsetLocked(name, at, true)
	remove := map[string]bool{}
	if !ok {
		for _, t := range r.resolveLocked(name, at, true) {
			remove[t] = true
		}
	} else {
		for _, t := range ts.Tools {
			remove[t] = true
		}
		for _, inc := range ts.Includes {
			if it, found := r.toolsetLocked(inc, at, true); found {
				for _, t := range it.Tools {
					remove[t] = true
				}
			}
		}
	}
	for t := range core {
		delete(remove, t)
	}
	return sortedKeys(remove)
}

func (r *Registry) applySelection(tools map[string]bool, names []string, disable bool, at Scope) {
	legacy := legacyToolsets()
	for _, name := range names {
		var resolved []string
		switch {
		case r.validLocked(name, at):
			ts, _ := r.toolsetLocked(name, at, true)
			if disable && (strings.HasPrefix(name, bundlePrefix) || ts.Posture) {
				resolved = r.bundleNonCore(name, at)
				if strings.HasPrefix(name, bundlePrefix) {
					r.warnBundle(name, resolved)
				}
			} else {
				resolved = r.resolveLocked(name, at, true)
			}
		case legacy[name] != nil:
			resolved = legacy[name]
		default:
			continue
		}
		for _, t := range resolved {
			if disable {
				delete(tools, t)
			} else {
				tools[t] = true
			}
		}
	}
}

func (r *Registry) warnBundle(name string, resolved []string) {
	r.warnMu.Lock()
	defer r.warnMu.Unlock()
	if r.warnedBundles[name] {
		return
	}
	r.warnedBundles[name] = true
	listed := "none"
	if len(resolved) > 0 {
		listed = strings.Join(resolved, ", ")
	}
	slog.Info(fmt.Sprintf("agent.disabled_toolsets contains platform-bundle name '%s'; core tools are preserved and only its platform-specific tools (%s) are removed. Bundle names usually belong in `toolsets:`, not `disabled_toolsets`.", name, listed))
}

func (r *Registry) selectLocked(s Selection) map[string]bool {
	at := s.Scope
	tools := map[string]bool{}
	if s.Enabled != nil {
		enabled := slices.Clone(s.Enabled)
		if s.KanbanWorker && !slices.Contains(enabled, "kanban") {
			enabled = append(enabled, "kanban")
		}
		r.applySelection(tools, enabled, false, at)
	} else {
		for _, name := range r.allToolsetNames(at) {
			if r.sessionGated(name) {
				continue
			}
			for _, t := range r.resolveLocked(name, at, true) {
				tools[t] = true
			}
		}
	}
	if disabled := SessionDisabled(s.Disabled, s.Platform, s.Oneshot); len(disabled) > 0 {
		r.applySelection(tools, disabled, true, at)
	}
	return tools
}

func (r *Registry) selected(s Selection) map[string]bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.selectLocked(s)
}

func (r *Registry) dropsLocked(s Selection) map[string]bool {
	out := setOf(s.Drops)
	for _, t := range r.toolsets {
		if len(t.Platforms) == 0 || slices.Contains(t.Platforms, s.Platform) {
			continue
		}
		for _, name := range r.resolveLocked(t.Name, s.Scope, true) {
			out[name] = true
		}
	}
	return out
}

func (r *Registry) Drops(s Selection) []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return sortedKeys(r.dropsLocked(s))
}
