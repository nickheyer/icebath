package plugins

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
)

// Callback a live host uses to accept a plugin triggered turn
type MessageInjector func(ctx context.Context, kwargs map[string]any) bool

// Injector together with the lifecycle owner that published it
type ownedInjector struct {
	owner    any
	injector MessageInjector
}

var (
	pluginManagersMu         sync.Mutex
	pluginManagersByHome     = map[string]*Manager{}
	pluginManagerSlot        *Manager
	publishedGatewayMu       sync.Mutex
	publishedGatewayInjector *ownedInjector
	publishedTUIMu           sync.Mutex
	publishedTUIInjector     *ownedInjector
)

// Resolved active home used to key per profile plugin managers
func pluginHomeKey(ctx context.Context) string { return home.Key(home.Dir(ctx)) }

// Every manager this process created, the single slot one included
func knownPluginManagers() []*Manager {
	pluginManagersMu.Lock()
	defer pluginManagersMu.Unlock()
	return knownPluginManagersLocked()
}

// Distinct managers by home plus the single slot one, caller holding the registry lock
func knownPluginManagersLocked() []*Manager {
	seen := map[*Manager]bool{}
	var out []*Manager
	for _, key := range sortedKeys(pluginManagersByHome) {
		if m := pluginManagersByHome[key]; !seen[m] {
			seen[m] = true
			out = append(out, m)
		}
	}
	if pluginManagerSlot != nil && !seen[pluginManagerSlot] {
		out = append(out, pluginManagerSlot)
	}
	return out
}

// Sorted keys of a string keyed map
func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sortStrings(keys)
	return keys
}

// Plugin manager of the active profile, created once per resolved home and stamped with published hosts
func GetPluginManager(ctx context.Context) *Manager {
	current := pluginHomeKey(ctx)
	pluginManagersMu.Lock()
	adopted := pluginManagerSlot != nil
	if adopted {
		for _, m := range pluginManagersByHome {
			if m == pluginManagerSlot {
				adopted = false
				break
			}
		}
	}
	var manager *Manager
	if adopted {
		pluginManagersByHome[current] = pluginManagerSlot
		manager = pluginManagerSlot
	} else {
		manager = pluginManagersByHome[current]
		if manager == nil {
			manager = NewManager(current)
			pluginManagersByHome[current] = manager
		}
		pluginManagerSlot = manager
	}
	pluginManagersMu.Unlock()
	attachPublishedGatewayHost(manager)
	attachPublishedTUIHost(manager)
	return manager
}

// Drops every cached manager after unloading it, clears published hosts and dashboard auth providers
func ResetPluginManagersForTests() {
	pluginManagersMu.Lock()
	for _, m := range knownPluginManagersLocked() {
		clearPluginSubmodules(m)
		m.Unload(nil)
	}
	clear(pluginManagersByHome)
	pluginManagerSlot = nil
	pluginManagersMu.Unlock()
	publishedGatewayMu.Lock()
	publishedGatewayInjector = nil
	publishedGatewayMu.Unlock()
	publishedTUIMu.Lock()
	publishedTUIInjector = nil
	publishedTUIMu.Unlock()
	if s := installedServices(); s != nil && s.ClearDashboardAuthProviders != nil {
		s.ClearDashboardAuthProviders()
	}
}

// Purges the hosted modules of a manager directory plugins and frees the bare module names its home claimed
func clearPluginSubmodules(m *Manager) {
	if m == nil {
		return
	}
	m.mu.RLock()
	var names []string
	for _, loaded := range m.plugins {
		if strings.HasPrefix(loaded.Module, nsParent+".") {
			names = append(names, loaded.Module)
		}
	}
	m.mu.RUnlock()
	for _, name := range names {
		moduleNamespaceMu.Lock()
		if bareModuleScope[name] == m.home {
			delete(bareModuleScope, name)
		}
		moduleNamespaceMu.Unlock()
	}
}

// Reports whether a live gateway can accept plugin triggered turns
func (m *Manager) HasGatewayMessageInjector() bool {
	m.injectorMu.Lock()
	defer m.injectorMu.Unlock()
	return m.gatewayInjector != nil
}

// Publishes a live gateway injector and its lifecycle owner
func (m *Manager) SetGatewayMessageInjector(owner any, injector MessageInjector) {
	m.injectorMu.Lock()
	m.gatewayInjector = &ownedInjector{owner: owner, injector: injector}
	m.injectorMu.Unlock()
}

// Clears the gateway injector only while it still belongs to the owner
func (m *Manager) ClearGatewayMessageInjector(owner any) {
	m.injectorMu.Lock()
	if m.gatewayInjector != nil && m.gatewayInjector.owner == owner {
		m.gatewayInjector = nil
	}
	m.injectorMu.Unlock()
}

// Submits a plugin triggered turn to the live gateway
func (m *Manager) InjectGatewayMessage(ctx context.Context, kwargs map[string]any) bool {
	m.injectorMu.Lock()
	registered := m.gatewayInjector
	m.injectorMu.Unlock()
	return registered != nil && registered.injector(ctx, kwargs)
}

// Reports whether a live TUI or desktop host can accept plugin triggered turns
func (m *Manager) HasTUIMessageInjector() bool {
	m.injectorMu.Lock()
	defer m.injectorMu.Unlock()
	return m.tuiInjector != nil
}

// Publishes a live TUI or desktop injector without touching the gateway slot
func (m *Manager) SetTUIMessageInjector(owner any, injector MessageInjector) {
	m.injectorMu.Lock()
	m.tuiInjector = &ownedInjector{owner: owner, injector: injector}
	m.injectorMu.Unlock()
}

// Clears the TUI injector only while it still belongs to the owner
func (m *Manager) ClearTUIMessageInjector(owner any) {
	m.injectorMu.Lock()
	if m.tuiInjector != nil && m.tuiInjector.owner == owner {
		m.tuiInjector = nil
	}
	m.injectorMu.Unlock()
}

// Submits a plugin triggered turn to the live TUI or desktop host
func (m *Manager) InjectTUIMessage(ctx context.Context, kwargs map[string]any) bool {
	m.injectorMu.Lock()
	registered := m.tuiInjector
	m.injectorMu.Unlock()
	return registered != nil && registered.injector(ctx, kwargs)
}

// Remembers the process gateway host and stamps every existing manager with it
func PublishGatewayMessageHost(owner any, injector MessageInjector) {
	publishedGatewayMu.Lock()
	defer publishedGatewayMu.Unlock()
	publishedGatewayInjector = &ownedInjector{owner: owner, injector: injector}
	for _, m := range knownPluginManagers() {
		m.SetGatewayMessageInjector(owner, injector)
	}
}

// Forgets this owner's gateway host without clobbering a newer runner
func ClearPublishedGatewayMessageHost(owner any) {
	publishedGatewayMu.Lock()
	defer publishedGatewayMu.Unlock()
	if publishedGatewayInjector != nil && publishedGatewayInjector.owner == owner {
		publishedGatewayInjector = nil
	}
	for _, m := range knownPluginManagers() {
		m.ClearGatewayMessageInjector(owner)
	}
}

// Gives a newly resolved manager the live gateway host when its slot is empty
func attachPublishedGatewayHost(m *Manager) {
	publishedGatewayMu.Lock()
	defer publishedGatewayMu.Unlock()
	if host := publishedGatewayInjector; host != nil && !m.HasGatewayMessageInjector() {
		m.SetGatewayMessageInjector(host.owner, host.injector)
	}
}

// Remembers the process TUI host and stamps every existing manager with it
func PublishTUIMessageHost(owner any, injector MessageInjector) {
	publishedTUIMu.Lock()
	publishedTUIInjector = &ownedInjector{owner: owner, injector: injector}
	publishedTUIMu.Unlock()
	for _, m := range knownPluginManagers() {
		m.SetTUIMessageInjector(owner, injector)
	}
}

// Forgets the process TUI host and clears it where this owner still holds the slot
func ClearPublishedTUIMessageHost(owner any) {
	publishedTUIMu.Lock()
	if publishedTUIInjector != nil && publishedTUIInjector.owner == owner {
		publishedTUIInjector = nil
	}
	publishedTUIMu.Unlock()
	for _, m := range knownPluginManagers() {
		m.ClearTUIMessageInjector(owner)
	}
}

// Gives a newly resolved manager the process TUI host when its slot is empty
func attachPublishedTUIHost(m *Manager) {
	publishedTUIMu.Lock()
	host := publishedTUIInjector
	publishedTUIMu.Unlock()
	if host != nil {
		m.injectorMu.Lock()
		if m.tuiInjector == nil {
			m.tuiInjector = host
		}
		m.injectorMu.Unlock()
	}
}

// Manager of the active profile after lazily discovering its plugins
func deliveryManager(ctx context.Context) *Manager {
	m := GetPluginManager(ctx)
	m.mu.RLock()
	discovered := m.discovered
	m.mu.RUnlock()
	if !discovered {
		joinBackgroundDiscovery(ctx)
		m.DiscoverAndLoad(ctx, false)
	}
	return m
}

// Invokes a hook on the active profile's manager after lazy discovery
func InvokeHook(ctx context.Context, hookName string, kwargs map[string]any) []any {
	return deliveryManager(ctx).InvokeHook(ctx, hookName, kwargs)
}

// Loop side hook invocation on the active profile's manager after lazy discovery
func AInvokeHook(ctx context.Context, hookName string, kwargs map[string]any) []any {
	return deliveryManager(ctx).AInvokeHook(ctx, hookName, kwargs)
}

// Renders the active profile's plugin prompt sections after ensuring discovery
func RenderSystemPromptSections(ctx context.Context, sessionInfo map[string]any) []RenderedSystemPromptSection {
	return ensurePluginsDiscovered(ctx, false).RenderSystemPromptSections(sessionInfo)
}

// Invokes a middleware observer kind on the active profile's manager after lazy discovery
func InvokeMiddleware(ctx context.Context, kind, payloadKey string, kwargs map[string]any) []any {
	return deliveryManager(ctx).InvokeMiddleware(kind, payloadKey, kwargs)
}

// Reports whether the active profile has middleware of a kind after lazy discovery
func HasMiddleware(ctx context.Context, kind string) bool {
	return deliveryManager(ctx).HasMiddleware(kind)
}

// Reports whether the active profile has callbacks for a hook after lazy discovery
func HasHook(ctx context.Context, hookName string) bool {
	return deliveryManager(ctx).HasHook(hookName)
}

// Callback snapshot of a hook on the active profile's manager without triggering discovery
func IterHookCallbacks(ctx context.Context, hookName string) []HookFunc {
	return GetPluginManager(ctx).IterHookCallbacks(hookName)
}

// Reports whether the active profile holds its reply stream for an output transform, after lazy discovery
func StreamHeld(ctx context.Context) bool { return deliveryManager(ctx).HoldsStream() }

// Held output transforms of the active profile's manager after lazy discovery
func HeldStreamTransforms(ctx context.Context) []*hookEntry {
	return deliveryManager(ctx).HeldTransforms()
}

// Fires the observer only pre_command hook, logging and ignoring any directive a callback returns
func FirePreCommandHook(ctx context.Context, surface, command, aliasUsed, argsRaw string, sessionKey, platform *string) {
	defer func() {
		if r := recover(); r != nil {
			if escaped, ok := interruptFromPanic(r); ok {
				panic(escaped)
			}
			logDebug(fmt.Sprintf("pre_command hook dispatch failed (non-fatal): %v", r))
		}
	}()
	m := GetPluginManager(ctx)
	if !m.HasHook("pre_command") {
		return
	}
	kwargs := map[string]any{"surface": surface, "command": command, "alias_used": aliasUsed, "args_raw": argsRaw,
		"session_key": optionalString(sessionKey), "platform": optionalString(platform)}
	for _, result := range m.InvokeHook(ctx, "pre_command", kwargs) {
		d, ok := result.(map[string]any)
		if !ok {
			continue
		}
		_, hasAction := d["action"]
		_, hasDecision := d["decision"]
		if hasAction || hasDecision {
			logDebug(fmt.Sprintf("pre_command is observer-only in v1: ignoring directive %s for /%s (surface=%s). Block/rewrite will arrive with the command middleware variant (#64204/#64231).",
				config.PyRepr(d), command, surface))
		}
	}
}

// Nil for an absent optional string, the value otherwise
func optionalString(s *string) any {
	if s == nil {
		return nil
	}
	return *s
}
