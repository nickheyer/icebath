package plugins

import (
	"fmt"
	"path/filepath"
	"runtime/debug"

	"github.com/nickheyer/icebath/pkg/tools"
)

// Plugin name and resolved source file shared by the general and memory loaders
type hookSource struct{ name, source string }

// Identity of a plugin by name and module file, nil when the module has no file
func hookSourceOf(name, file string) *hookSource {
	if file == "" {
		return nil
	}
	resolved, err := filepath.Abs(file)
	if err != nil {
		resolved = file
	}
	if real, err := filepath.EvalSymlinks(resolved); err == nil {
		resolved = real
	}
	return &hookSource{name: name, source: resolved}
}

// One host owned registration with the inverse that unwinds it
type Registration struct {
	Kind       string
	Key        string
	Release    func() error
	PluginKey  string
	Persistent bool
	HoldStream bool
	disposed   bool
	onDispose  func(*Registration)
}

// Reports whether the handle still owns an active registration
func (r *Registration) Active() bool { return !r.disposed }

// Releases the registration once, repeated disposal being harmless
func (r *Registration) Dispose() error {
	if r.disposed {
		return nil
	}
	r.disposed = true
	defer func() {
		if r.onDispose != nil {
			r.onDispose(r)
		}
	}()
	return callGuarded(r.Release)
}

// Runs fn and turns a panic into an error carrying the panic value
func callGuarded(fn func() error) (err error) {
	defer func() {
		if p := recover(); p != nil {
			if escaped, ok := interruptFromPanic(p); ok {
				err = escaped
				return
			}
			err = fmt.Errorf("%v", p)
			logDebug("recovered plugin panic", "stack", string(debug.Stack()))
		}
	}()
	if fn == nil {
		return nil
	}
	return fn()
}

// Records one registration under the plugin key, persistent ones kept out of the unload order
func (m *Manager) trackRegistration(manifest *Manifest, kind, key string, release func() error, persistent bool) *Registration {
	r := &Registration{Kind: kind, Key: key, Release: release, PluginKey: manifest.ID(), Persistent: persistent}
	r.onDispose = func(disposed *Registration) { m.forgetRegistrations([]*Registration{disposed}) }
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ownershipLedger[r.PluginKey] = append(m.ownershipLedger[r.PluginKey], r)
	if !persistent {
		m.registrationOrder = append(m.registrationOrder, r)
	}
	return r
}

// Leases one kind scope name slot of a scope keyed process global registry
func (m *Manager) trackScopedRegistration(manifest *Manifest, kind, name string, restore func(name string, current, replacement any, scope string) error,
	current, previous any, finalize func() error) *Registration {
	scope := m.home
	lease := replacementCoordinator.Acquire([3]string{kind, scope, name}, current, previous,
		func(replacement any) error { return restore(name, current, replacement, scope) }, finalize)
	return m.trackRegistration(manifest, kind, name, lease.Dispose, false)
}

// Live persistent registrations across every plugin, caller holding the lock
func (m *Manager) activePersistentLocked() []*Registration {
	var out []*Registration
	for _, owned := range m.ownershipLedger {
		for _, r := range owned {
			if r.Persistent && r.Active() {
				out = append(out, r)
			}
		}
	}
	return out
}

// Disposes parked persistent handles whose plugin did not re-register the same kind and key
func (m *Manager) evictStalePersistentRegistrations() {
	m.mu.Lock()
	if len(m.persistentCarryover) == 0 {
		m.mu.Unlock()
		return
	}
	parked := m.persistentCarryover
	m.persistentCarryover = nil
	current := map[[2]string]bool{}
	for _, r := range m.activePersistentLocked() {
		current[[2]string{r.Kind, r.Key}] = true
	}
	m.mu.Unlock()
	var stale []*Registration
	for _, r := range parked {
		if r.Active() && !current[[2]string{r.Kind, r.Key}] {
			stale = append(stale, r)
		}
	}
	for _, r := range stale {
		logInfo(fmt.Sprintf("Evicting persistent registration %s/%s: plugin '%s' no longer supplies it after re-discovery",
			r.Kind, r.Key, r.PluginKey))
	}
	m.disposeRegistrations(stale)
}

// Disposes parked persistent handles whose plugin did not re-register, the forced rediscovery eviction pass
func (m *Manager) EvictStalePersistentRegistrations() { m.evictStalePersistentRegistrations() }

// Registrations enrolled in reverse order teardown, in registration order
func (m *Manager) RegistrationOrder() []*Registration {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]*Registration(nil), m.registrationOrder...)
}

// Persistent handles the last unload all parked for the rediscovery eviction pass
func (m *Manager) PersistentCarryover() []*Registration {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]*Registration(nil), m.persistentCarryover...)
}

// Registrations the ownership ledger attributes to one plugin key
func (m *Manager) OwnedRegistrations(pluginKey string) []*Registration {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]*Registration(nil), m.ownershipLedger[pluginKey]...)
}

// Removes the last identical element of a registration list
func removeIdentity[T any](values []*T, target *T) ([]*T, bool) {
	for i := len(values) - 1; i >= 0; i-- {
		if values[i] == target {
			return append(values[:i:i], values[i+1:]...), true
		}
	}
	return values, false
}

// Drops one callback from a keyed list, removing the key once its list is empty
func removeCallback[T any](m *Manager, mapping map[string][]*T, key string, callback *T) {
	m.mu.Lock()
	defer m.mu.Unlock()
	callbacks, ok := mapping[key]
	if !ok {
		return
	}
	callbacks, _ = removeIdentity(callbacks, callback)
	if len(callbacks) == 0 {
		delete(mapping, key)
	} else {
		mapping[key] = callbacks
	}
}

// Restores a manager mapping slot only while current still occupies it, the zero previous deleting the key
func restoreMapping[V comparable](mapping map[string]V, key string, current, previous V) bool {
	if have, ok := mapping[key]; !ok || have != current {
		return false
	}
	var zero V
	if previous == zero {
		delete(mapping, key)
	} else {
		mapping[key] = previous
	}
	return true
}

// Restores a manager value only while current is still the active one
func restoreValue[V comparable](field *V, current, previous V) bool {
	if *field != current {
		return false
	}
	*field = previous
	return true
}

// Drops a name from a manager name set once no active ledger entry of that kind owns it
func (m *Manager) removeNameIfUnowned(kind string, names map[string]bool, name string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, r := range m.registrationOrder {
		if r.Active() && r.Kind == kind && r.Key == name {
			return
		}
	}
	delete(names, name)
}

// Drops a plugin tool name no live registration owns
func (m *Manager) removeToolNameIfUnowned(name string) {
	m.removeNameIfUnowned("tool", m.pluginToolNames, name)
}

// Drops a plugin platform name no live registration owns
func (m *Manager) removePlatformNameIfUnowned(name string) {
	m.removeNameIfUnowned("platform", m.pluginPlatformNames, name)
}

// Removes registrations from the unload order, the ownership ledger and the memory hook ledger
func (m *Manager) forgetRegistrations(registrations []*Registration) {
	if len(registrations) == 0 {
		return
	}
	ids := map[*Registration]bool{}
	for _, r := range registrations {
		ids[r] = true
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	kept := m.registrationOrder[:0:0]
	for _, r := range m.registrationOrder {
		if !ids[r] {
			kept = append(kept, r)
		}
	}
	m.registrationOrder = kept
	for key, owned := range m.ownershipLedger {
		if remaining := withoutRegistrations(owned, ids); len(remaining) > 0 {
			m.ownershipLedger[key] = remaining
		} else {
			delete(m.ownershipLedger, key)
		}
	}
	for key, owned := range m.memoryHookRegistrations {
		if remaining := withoutRegistrations(owned, ids); len(remaining) > 0 {
			m.memoryHookRegistrations[key] = remaining
		} else {
			delete(m.memoryHookRegistrations, key)
		}
	}
}

// Registrations not in the drop set
func withoutRegistrations(owned []*Registration, drop map[*Registration]bool) []*Registration {
	var out []*Registration
	for _, r := range owned {
		if !drop[r] {
			out = append(out, r)
		}
	}
	return out
}

// Disposes every fallback hook handle a memory loader source holds
func (m *Manager) dropFallbackHooks(src *hookSource) {
	if src == nil {
		return
	}
	m.mu.Lock()
	handles := m.memoryHookRegistrations[*src]
	delete(m.memoryHookRegistrations, *src)
	m.mu.Unlock()
	for _, h := range handles {
		_ = h.Dispose()
	}
}

// Registers a memory loader hook unless general discovery already loaded the same source
func (m *Manager) registerFallbackHook(c *Context, src *hookSource, hookName string, fn HookFunc, holdStream bool) *Registration {
	return m.registerFallbackHookWith(c, src, hookName, func() *Registration { return c.RegisterHook(hookName, fn, holdStream) })
}

// Fallback hook registration through register, a no op handle when general discovery owns the source
func (m *Manager) registerFallbackHookWith(c *Context, src *hookSource, hookName string, register func() *Registration) *Registration {
	if src == nil {
		return register()
	}
	m.mu.RLock()
	owned := false
	for _, loaded := range m.plugins {
		if s := hookSourceOf(loaded.Manifest.Name, loaded.ModuleFile); loaded.Enabled && s != nil && *s == *src {
			owned = true
			break
		}
	}
	m.mu.RUnlock()
	var handle *Registration
	if owned {
		handle = c.track("hook", hookName, func() error { return nil }, false)
	} else {
		handle = register()
	}
	m.mu.Lock()
	m.memoryHookRegistrations[*src] = append(m.memoryHookRegistrations[*src], handle)
	m.mu.Unlock()
	return handle
}

// Disposes registrations in reverse acquisition order, logging each failure
func (m *Manager) disposeRegistrations(registrations []*Registration) {
	for i := len(registrations) - 1; i >= 0; i-- {
		r := registrations[i]
		if err := r.Dispose(); err != nil {
			logWarn(fmt.Sprintf("Failed to unload plugin registration %s/%s: %v", r.PluginKey, r.Key, err), excInfo(err)...)
		}
	}
}

// Unloads one plugin by key, manifest name, manifest or loaded record, or every plugin when nil
func (m *Manager) Unload(plugin any) bool {
	m.discoveryMu.Lock()
	defer m.discoveryMu.Unlock()
	return m.unloadScoped(plugin)
}

// Canonical plugin key of an unload request
func resolvePluginKey(plugin any) string {
	switch p := plugin.(type) {
	case *LoadedPlugin:
		return p.Manifest.ID()
	case *Manifest:
		return p.ID()
	case string:
		return p
	}
	return fmt.Sprint(plugin)
}

// Disposes the targeted registrations in reverse order and reports whether anything was found
func (m *Manager) unloadScoped(plugin any) bool {
	unloadAll := plugin == nil
	var targetKeys map[string]bool
	var registrations []*Registration
	m.mu.RLock()
	if unloadAll {
		targetKeys = map[string]bool{}
		for k := range m.ownershipLedger {
			targetKeys[k] = true
		}
		for k := range m.plugins {
			targetKeys[k] = true
		}
		registrations = append(registrations, m.registrationOrder...)
	} else {
		targetKeys = m.unloadTargetKeysLocked(resolvePluginKey(plugin))
		for _, r := range m.registrationOrder {
			if targetKeys[r.PluginKey] {
				registrations = append(registrations, r)
			}
		}
		for _, key := range sortedSet(targetKeys) {
			for _, r := range m.ownershipLedger[key] {
				if r.Persistent && r.Active() {
					registrations = append(registrations, r)
				}
			}
		}
	}
	m.mu.RUnlock()
	found := len(targetKeys) > 0 || len(registrations) > 0
	m.disposeRegistrations(registrations)
	m.forgetRegistrations(registrations)
	if unloadAll {
		m.resetAfterUnloadAll(registrations)
	} else {
		m.mu.Lock()
		for key := range targetKeys {
			m.dropPluginLocked(key)
		}
		m.mu.Unlock()
	}
	return found
}

// Canonical keys of a targeted unload, the exact key first and otherwise every plugin of that name
func (m *Manager) unloadTargetKeysLocked(requested string) map[string]bool {
	_, inLedger := m.ownershipLedger[requested]
	_, inPlugins := m.plugins[requested]
	if inLedger || inPlugins {
		return map[string]bool{requested: true}
	}
	out := map[string]bool{}
	for key, loaded := range m.plugins {
		if loaded.Manifest.Name == requested {
			out[key] = true
		}
	}
	return out
}

// Sweeps pre-ledger global state and clears every manager registry after an unload of all plugins
func (m *Manager) resetAfterUnloadAll(registrations []*Registration) {
	m.mu.RLock()
	platforms := sortedSet(m.pluginPlatformNames)
	ledgerTools := map[string]bool{}
	for _, r := range registrations {
		if r.Kind == "tool" {
			ledgerTools[r.Key] = true
		}
	}
	var preledgerTools []string
	for name := range m.pluginToolNames {
		if !ledgerTools[name] {
			preledgerTools = append(preledgerTools, name)
		}
	}
	m.mu.RUnlock()
	for _, name := range platforms {
		unregisterGatewayPlatform(name)
	}
	for _, name := range preledgerTools {
		if err := tools.Default.Deregister(name, "", ""); err != nil {
			logDebug(fmt.Sprintf("unload: tool deregister %s failed: %v", name, err))
		}
	}
	m.mu.Lock()
	parked := map[*Registration]bool{}
	for _, r := range m.persistentCarryover {
		parked[r] = true
	}
	for _, r := range m.activePersistentLocked() {
		if !parked[r] {
			m.persistentCarryover = append(m.persistentCarryover, r)
		}
	}
	clear(m.ownershipLedger)
	clear(m.plugins)
	clear(m.pluginSeq)
	clear(m.hooks)
	clear(m.middleware)
	clear(m.pluginToolNames)
	clear(m.pluginPlatformNames)
	clear(m.systemPromptSections)
	m.resetContextRegistriesLocked()
	m.discovered = false
	m.mu.Unlock()
	m.hookTimeoutMu.Lock()
	clear(m.hookRunningCallbacks)
	clear(m.hookAbandoned)
	clear(m.hookTimeoutSuppressedUntil)
	clear(m.hookFailuresReported)
	m.hookTimeoutMu.Unlock()
}
