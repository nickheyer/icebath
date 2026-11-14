package plugins

import (
	"context"
	"fmt"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Interactive CLI the manager injects plugin messages into and dispatches tools for
type CLIHost interface {
	AgentRunning() bool
	QueueInterrupt(msg string)
	QueuePending(msg string)
	Agent() any
}

// Entry and deferred loader occupying one platform registry slot
type PlatformSlot struct {
	Entry  any
	Loader *DeferredPlatformLoader
}

// Lazy loader of a bundled platform plugin run when the registry is first asked for it
type DeferredPlatformLoader struct {
	Load func(ctx context.Context)
}

// Gateway platform registry owned by the gateway package
type PlatformRegistry interface {
	CoreShipsPlatform(name string) bool
	NewEntry(spec PlatformSpec) (any, error)
	Snapshot(name, scope string) PlatformSlot
	Register(entry any, scope string)
	RegisterDeferred(name string, loader *DeferredPlatformLoader, scope string)
	NoteActivationEnv(name string, env []string)
	IsDeferredLoadCancelled(name, scope string) bool
	Restore(name string, current, previous PlatformSlot, scope string) bool
	Unregister(name string)
}

// Process wide dashboard auth provider registry owned by the web server
type DashboardAuthRegistry interface {
	RegisterGlobal(provider any) (displayName string, err error)
	UnregisterGlobal(name string, provider any)
}

// Plugin registered secret source as the dotenv orchestrator sees it
type SecretSourceInfo struct {
	Name      string
	IsEnabled func(section map[string]any) (bool, error)
}

// Secret source orchestrator the manager re-pulls after discovery
type SecretSources interface {
	ListPluginSources() []SecretSourceInfo
	ResetCache(h string) error
	LoadDotenv(ctx context.Context, h string) error
	RefreshInstalledScope(ctx context.Context, h string) error
}

// Platform declaration registry of portable MCP servers
type PlatformDeclarations interface {
	Register(name string, app, requires map[string]any) error
	Unregister(name string)
}

type discoveryHeldKey struct{ m *Manager }

type discoverySuppressedKey struct{}

// Context in which discovery is a no op so the scope manager stays undiscovered
func SuppressPluginDiscovery(ctx context.Context) context.Context {
	return context.WithValue(ctx, discoverySuppressedKey{}, true)
}

// Whether discovery is suppressed in this context
func pluginDiscoverySuppressed(ctx context.Context) bool {
	v, _ := ctx.Value(discoverySuppressedKey{}).(bool)
	return v
}

// Context bound to this manager home holding its discovery lock, locking unless already held
func (m *Manager) lockDiscovery(ctx context.Context) (context.Context, func()) {
	ctx = pluginHomeScope(ctx, m.home)
	if ctx.Value(discoveryHeldKey{m}) != nil {
		return ctx, func() {}
	}
	m.discoveryMu.Lock()
	return context.WithValue(ctx, discoveryHeldKey{m}, true), m.discoveryMu.Unlock
}

// Scans every plugin source and loads each plugin, force unloading first so config changes apply
func (m *Manager) DiscoverAndLoad(ctx context.Context, force bool) {
	if config.SafeWorkerEnabled() || pluginDiscoverySuppressed(ctx) {
		return
	}
	m.mu.RLock()
	discovered := m.discovered
	m.mu.RUnlock()
	if discovered && !force && inPluginLoadWorker(ctx) {
		return
	}
	loadedBefore, ok := m.discoverAndLoadLocked(ctx, force)
	if ok {
		m.notifyPluginLoaded(ctx, loadedBefore)
	}
}

// Marks the manager discovered without a sweep, the hermes test pin of a zero plugin runtime
func (m *Manager) MarkDiscoveredForTests() { m.setDiscovered(true) }

// Locked sweep body, false when nothing ran, safe mode skipped it or an interrupt cancelled it
func (m *Manager) discoverAndLoadLocked(ctx context.Context, force bool) (map[string]bool, bool) {
	ctx, unlock := m.lockDiscovery(ctx)
	defer unlock()
	m.mu.Lock()
	if m.discovered && !force {
		m.mu.Unlock()
		return nil, false
	}
	loadedBefore := map[string]bool{}
	for k, p := range m.plugins {
		if p.Error == "" && !p.Deferred {
			loadedBefore[k] = true
		}
	}
	m.mu.Unlock()
	if force {
		m.unloadScoped(nil)
	}
	if envEnabled("ICEBATH_SAFE_MODE") {
		logInfo("ICEBATH_SAFE_MODE=1 — plugin discovery skipped")
		m.setDiscovered(true)
		return nil, false
	}
	m.setDiscovered(true)
	completed := false
	defer func() {
		if !completed {
			m.setDiscovered(false)
		}
	}()
	if err := m.discoverAndLoadInner(ctx); err != nil {
		defer raiseUserInterrupt(err)
		return nil, false
	}
	m.evictStalePersistentRegistrations()
	m.refreshSecretSourcesAfterDiscovery(ctx)
	if force {
		m.reRegisterConfigHooksAfterForce(ctx)
	}
	completed = true
	return loadedBefore, true
}

func (m *Manager) setDiscovered(v bool) {
	m.mu.Lock()
	m.discovered = v
	m.mu.Unlock()
}

// Restores config owned shell hooks and outbound webhooks after a force clear, each guarded independently
func (m *Manager) reRegisterConfigHooksAfterForce(ctx context.Context) {
	steps := []struct {
		label string
		fn    func() error
	}{{"shell-hook", func() error { return reRegisterConfigHooks(ctx) }}, {"outbound-webhook", func() error { return ReRegisterOutboundWebhooks(ctx) }}}
	for _, step := range steps {
		if err := callGuarded(step.fn); err != nil {
			logDebug(fmt.Sprintf("force-reload %s re-register skipped: %v", step.label, err))
		}
	}
}

// Re-applies secret sources when a plugin source is enabled or one was removed since the last reconcile
func (m *Manager) refreshSecretSourcesAfterDiscovery(ctx context.Context) {
	s := installedServices()
	if s == nil || s.SecretSources == nil {
		return
	}
	sources := s.SecretSources.ListPluginSources()
	var enabledNames []string
	if len(sources) > 0 {
		secrets := map[string]any{}
		if cfg, err := loadConfig(m.home); err == nil {
			if sec, ok := cfg["secrets"].(map[string]any); ok {
				secrets = sec
			}
		}
		for _, src := range sources {
			section, _ := secrets[src.Name].(map[string]any)
			if section == nil {
				section = map[string]any{}
			}
			var on bool
			if src.IsEnabled != nil && callGuarded(func() error {
				var err error
				on, err = src.IsEnabled(section)
				return err
			}) == nil && on {
				enabledNames = append(enabledNames, src.Name)
			}
		}
	}
	m.mu.Lock()
	if len(enabledNames) == 0 && !m.pluginSecretSourcesReconciled {
		m.mu.Unlock()
		return
	}
	if len(enabledNames) > 0 {
		m.pluginSecretSourcesReconciled = true
	}
	m.mu.Unlock()
	h := m.home
	err := callGuarded(func() error {
		if err := s.SecretSources.ResetCache(h); err != nil {
			return err
		}
		if err := s.SecretSources.LoadDotenv(ctx, h); err != nil {
			return err
		}
		return s.SecretSources.RefreshInstalledScope(ctx, h)
	})
	if err != nil {
		logDebug(fmt.Sprintf("secret source re-apply after discovery failed: %v", err))
		return
	}
	if len(enabledNames) == 0 {
		m.mu.Lock()
		m.pluginSecretSourcesReconciled = false
		m.mu.Unlock()
	}
	sort.Strings(enabledNames)
	names := strings.Join(enabledNames, ", ")
	if names == "" {
		names = "<none — reconciled removed plugin sources>"
	}
	logDebug("Re-applied secret sources after plugin discovery for: " + names)
}

// Collects, gates and loads every manifest in dependency order, an error only for a user interrupt
func (m *Manager) discoverAndLoadInner(ctx context.Context) error {
	manifests := CollectDirectoryManifests(m.home)
	directoryKeys := map[string]bool{}
	for _, mf := range manifests {
		directoryKeys[mf.ID()] = true
	}
	entryPoints := 0
	for _, mf := range m.scanEntryPoints(ctx) {
		if !directoryKeys[mf.ID()] {
			manifests = append(manifests, mf)
			entryPoints++
		}
	}
	logDebug(fmt.Sprintf("  entrypoints: %d manifest(s)", entryPoints))
	cfg := loadConfigOrEmpty(m.home)
	disabled := DisabledPlugins(m.home)
	enabled, enabledPresent := enabledPlugins(cfg)
	var stale []string
	for name := range enabled {
		if legacyRelayPluginKeys[name] {
			stale = append(stale, name)
		}
	}
	if len(stale) > 0 {
		sort.Strings(stale)
		logWarn(fmt.Sprintf("Removed icebath plugin %s is still listed in plugins.enabled; remove it and configure a standard user or system Relay plugins.toml, or use %s for an explicit user-file override",
			strings.Join(stale, ", "), tools.RelayPluginsConfigEnv))
	}
	winners := ResolveManifestWinners(manifests)
	var order []string
	seen := map[string]bool{}
	for _, mf := range manifests {
		if key := mf.ID(); !seen[key] {
			seen[key] = true
			order = append(order, key)
		}
	}
	toLoad := map[string]*Manifest{}
	for _, key := range order {
		mf, ok := winners[key]
		if !ok {
			continue
		}
		load, err := m.gateManifest(ctx, mf, disabled, enabled, enabledPresent)
		if err != nil {
			return err
		}
		if load {
			toLoad[key] = mf
		}
	}
	for _, key := range ResolveLoadOrder(toLoad) {
		mf := toLoad[key]
		m.warnPythonDependencies(ctx, mf)
		m.validatePluginConfigSchema(mf)
		if err := m.loadPlugin(ctx, mf); err != nil {
			return err
		}
	}
	if len(manifests) > 0 {
		m.mu.RLock()
		found, on := len(m.plugins), 0
		for _, p := range m.plugins {
			if p.Enabled {
				on++
			}
		}
		m.mu.RUnlock()
		logInfo(fmt.Sprintf("Plugin discovery complete: %d found, %d enabled", found, on))
	}
	return nil
}

// Routes one winning manifest to load now, defer or a skipped placeholder, true only for the ordered pass
func (m *Manager) gateManifest(ctx context.Context, mf *Manifest, disabled, enabled map[string]bool, enabledPresent bool) (bool, error) {
	verdict := GateManifest(m.home, mf, disabled, enabled, enabledPresent)
	var err error
	switch verdict.Action {
	case "load":
		return true, nil
	case "load_now":
		err = m.loadPlugin(ctx, mf)
	case "defer":
		err = m.registerDeferredPlatform(ctx, mf)
	default:
		m.mu.Lock()
		m.putPluginLocked(mf.ID(), &LoadedPlugin{Manifest: mf, Enabled: verdict.Enabled, Error: verdict.Error})
		m.mu.Unlock()
	}
	logGate(verdict)
	return false, err
}

// Subscribes a process listener to sweeps that load plugins this process did not have, returning its unsubscribe
func (m *Manager) OnPluginLoaded(callback func(summaries []map[string]any)) (func(), error) {
	if callback == nil {
		return nil, fmt.Errorf("on_plugin_loaded requires a callable")
	}
	entry := &loadedListener{fn: callback}
	m.mu.Lock()
	m.pluginLoadedListeners = append(m.pluginLoadedListeners, entry)
	m.mu.Unlock()
	return func() {
		m.mu.Lock()
		m.pluginLoadedListeners, _ = removeIdentity(m.pluginLoadedListeners, entry)
		m.mu.Unlock()
	}, nil
}

type loadedListener struct {
	fn func([]map[string]any)
}

// Fires every plugin loaded listener with the summaries of plugins new since the sweep began
func (m *Manager) notifyPluginLoaded(ctx context.Context, loadedBefore map[string]bool) {
	m.mu.RLock()
	listeners := append([]*loadedListener(nil), m.pluginLoadedListeners...)
	m.mu.RUnlock()
	if len(listeners) == 0 {
		return
	}
	var summaries []map[string]any
	for _, s := range ActivationSummaries(ctx, m) {
		if key, _ := s["key"].(string); !loadedBefore[key] {
			summaries = append(summaries, s)
		}
	}
	if len(summaries) == 0 {
		return
	}
	for _, l := range listeners {
		if err := callGuarded(func() error { l.fn(summaries); return nil }); err != nil {
			_, name := callbackNames(l.fn)
			logWarn(fmt.Sprintf("plugin-loaded listener %s raised", name), "error", err)
		}
	}
}

// Manager level approval transport registration without unload tracking
func (m *Manager) RegisterApprovalTransport(ctx context.Context, name string, present tools.ApprovalPresentFunc, pluginID string) error {
	clean, err := cleanApprovalTransportName(name, present)
	if err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if existing := m.approvalTransports[clean]; existing != nil {
		return fmt.Errorf("approval transport %s is already registered by %s", config.PyRepr(clean), config.PyRepr(existing.PluginID))
	}
	m.approvalTransports[clean] = &tools.RegisteredApprovalTransport{Name: clean, Present: present, PluginID: pluginID, ProfileHome: resolvedHome(ctx)}
	logInfo(fmt.Sprintf("Plugin %s registered approval transport: %s", pluginID, clean))
	return nil
}

// Approval transport registered under a name, only inside the profile that registered it
func (m *Manager) GetApprovalTransport(ctx context.Context, name string) *tools.RegisteredApprovalTransport {
	m.mu.RLock()
	registered := m.approvalTransports[strings.ToLower(pyStrip(name))]
	m.mu.RUnlock()
	if registered == nil || registered.ProfileHome != resolvedHome(ctx) {
		return nil
	}
	return registered
}

// Transport of the env's profile after the lazy discovery a first approval needs, the hermes approval_prompt seam
func resolveRegisteredApprovalTransport(env *tools.Env, name string) (*tools.RegisteredApprovalTransport, error) {
	ctx := context.Background()
	if env != nil && env.Home != "" {
		ctx = home.WithOverride(ctx, env.Home)
	}
	return deliveryManager(ctx).GetApprovalTransport(ctx, name), nil
}

func init() {
	tools.RegisterApprovalTransportResolver(resolveRegisteredApprovalTransport)
}

// Whether config enables a portable package with MCP servers, probed without loading plugins
func (m *Manager) HasEnabledPortableMCP(rawConfig map[string]any) bool {
	if envEnabled("ICEBATH_SAFE_MODE") {
		return false
	}
	pluginsConfig, ok := rawConfig["plugins"].(map[string]any)
	if !ok {
		return false
	}
	names := func(v any) map[string]bool {
		out := map[string]bool{}
		if list, ok := v.([]any); ok {
			for _, item := range list {
				if s, ok := item.(string); ok {
					out[s] = true
				}
			}
		}
		return out
	}
	enabled, disabled := names(pluginsConfig["enabled"]), names(pluginsConfig["disabled"])
	if len(enabled) == 0 {
		return false
	}
	byKey := map[string]*Manifest{}
	var order []string
	for _, mf := range CollectDirectoryManifests(m.home) {
		if _, ok := byKey[mf.ID()]; !ok {
			order = append(order, mf.ID())
		}
		byKey[mf.ID()] = mf
	}
	for _, key := range order {
		mf := byKey[key]
		ids := map[string]bool{key: true, mf.Name: true}
		if !mf.Portable || intersects(ids, disabled) || !intersects(ids, enabled) {
			continue
		}
		namespace := mf.DataNamespace
		if namespace == "" {
			namespace = key
		}
		var found bool
		if callGuarded(func() error {
			servers, err := discoverPortableMCP(mf.Path, filepath.Join(m.home, "plugin-data", namespace), &[]AgentPluginDiagnostic{}, false)
			found = len(servers) > 0
			return err
		}) == nil && found {
			return true
		}
	}
	return false
}

// Slack action handlers for the Slack adapter to wire at connect
func (m *Manager) GetSlackActionHandlers() []*SlackActionHandler {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return append([]*SlackActionHandler(nil), m.slackActionHandlers...)
}

// Handler factories registered for one platform
func (m *Manager) GetPlatformHandlerFactories(platform string) []*PlatformHandlerFactory {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return append([]*PlatformHandlerFactory(nil), m.platformHandlerFactories[strings.ToLower(pyStrip(platform))]...)
}

// Handler factories registered for telegram
func (m *Manager) GetTelegramHandlerFactories() []*PlatformHandlerFactory {
	return m.GetPlatformHandlerFactories("telegram")
}

// Info rows of every discovered plugin sorted by key
func (m *Manager) ListPlugins() []map[string]any {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := []map[string]any{}
	for _, key := range sortedKeys(m.plugins) {
		p := m.plugins[key]
		var errVal any
		if p.Error != "" {
			errVal = p.Error
		}
		out = append(out, map[string]any{
			"name": p.Manifest.Name, "key": p.Manifest.ID(), "kind": p.Manifest.Kind, "version": p.Manifest.Version,
			"description": p.Manifest.Description, "source": p.Manifest.Source, "enabled": p.Enabled,
			"tools": len(p.ToolsRegistered), "hooks": len(p.HooksRegistered), "middleware": len(p.MiddlewareRegistered),
			"commands": len(p.CommandsRegistered), "error": errVal,
		})
	}
	return out
}

// Keys of the enabled plugins whose manifest declares the capability, sorted
func (m *Manager) EnabledPluginsDeclaring(capability string) []string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := []string{}
	for _, key := range sortedKeys(m.plugins) {
		if p := m.plugins[key]; p.Enabled && p.Manifest != nil && slices.Contains(p.Manifest.Capabilities, capability) {
			out = append(out, key)
		}
	}
	return out
}

// SKILL.md path of a qualified plugin skill
func (m *Manager) FindPluginSkill(qualified string) (string, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if entry := m.pluginSkills[qualified]; entry != nil {
		return entry.Path, true
	}
	return "", false
}

// Sorted bare names of every skill a plugin registered
func (m *Manager) ListPluginSkills(pluginName string) []string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := []string{}
	for q, e := range m.pluginSkills {
		if strings.HasPrefix(q, pluginName+":") {
			out = append(out, e.BareName)
		}
	}
	sort.Strings(out)
	return out
}

// Progressive disclosure metadata of every registered plugin skill sorted by qualified name
func (m *Manager) ListPluginSkillMetadata() []map[string]any {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := []map[string]any{}
	for _, q := range sortedKeys(m.pluginSkills) {
		e := m.pluginSkills[q]
		fm := map[string]any{}
		for k, v := range e.Frontmatter {
			fm[k] = v
		}
		out = append(out, map[string]any{"name": q, "description": e.Description, "category": "plugin", "frontmatter": fm})
	}
	return out
}

// Plugin registered automation blueprints sorted by key
func (m *Manager) ListAutomationBlueprints() []any {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := []any{}
	for _, key := range sortedKeys(m.automationBlueprints) {
		out = append(out, m.automationBlueprints[key].value)
	}
	return out
}

// Whether any portable MCP server is registered
func (m *Manager) HasPortableMCPServers() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.portableMCPServers) > 0
}

// Defensive copy of the enabled portable MCP server configs
func (m *Manager) GetPortableMCPServers() map[string]map[string]any {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := map[string]map[string]any{}
	for name, cfg := range m.portableMCPServers {
		copied := map[string]any{}
		for k, v := range cfg {
			copied[k] = v
		}
		out[name] = copied
	}
	return out
}

// Owning plugin key of every portable MCP server
func (m *Manager) GetPortableMCPServerPlugins() map[string]string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := map[string]string{}
	for k, v := range m.portableMCPServerPlugins {
		out[k] = v
	}
	return out
}

// Drops a stale plugin skill entry, ignoring missing names
func (m *Manager) RemovePluginSkill(qualified string) {
	m.mu.Lock()
	delete(m.pluginSkills, qualified)
	m.mu.Unlock()
}

// Plugin registered context engine, nil when none
func (m *Manager) ContextEngine() any {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.contextEngine
}

// Gives a command name absent from the table the next insertion rank, caller holding the lock
func (m *Manager) notePluginCommandOrderLocked(name string) {
	if _, present := m.pluginCommands[name]; present {
		return
	}
	m.pluginCommandSeq[name] = m.nextPluginCommandSeq
	m.nextPluginCommandSeq++
}

// Registered plugin command names in insertion order, a name replaced in place keeping its rank
func (m *Manager) PluginCommandOrder() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]string, 0, len(m.pluginCommands))
	for k := range m.pluginCommands {
		out = append(out, k)
	}
	sort.SliceStable(out, func(i, j int) bool {
		si, iok := m.pluginCommandSeq[out[i]]
		sj, jok := m.pluginCommandSeq[out[j]]
		if iok != jok {
			return iok
		}
		if si != sj {
			return si < sj
		}
		return out[i] < out[j]
	})
	return out
}

// Plugin slash commands by name
func (m *Manager) PluginCommands() map[string]*PluginCommand {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := map[string]*PluginCommand{}
	for k, v := range m.pluginCommands {
		out[k] = v
	}
	return out
}

// Plugin CLI subcommands by name
func (m *Manager) CLICommands() map[string]*CLICommand {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := map[string]*CLICommand{}
	for k, v := range m.cliCommands {
		out[k] = v
	}
	return out
}

// Plugin auxiliary task entries by key
func (m *Manager) AuxiliaryTasks() map[string]map[string]any {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := map[string]map[string]any{}
	for k, v := range m.auxTasks {
		copied := map[string]any{}
		for fk, fv := range v.fields {
			copied[fk] = fv
		}
		out[k] = copied
	}
	return out
}

// Attaches the interactive CLI after discovery
func (m *Manager) SetCLIRef(cli CLIHost) {
	m.mu.Lock()
	m.cliRef = cli
	m.mu.Unlock()
}

// Clears every context owned registry after an unload of all plugins, caller holding the lock
func (m *Manager) resetContextRegistriesLocked() {
	clear(m.cliCommands)
	clear(m.pluginCommands)
	clear(m.pluginCommandSeq)
	clear(m.pluginSkills)
	clear(m.automationBlueprints)
	clear(m.portableMCPServers)
	clear(m.portableMCPServerPlugins)
	clear(m.auxTasks)
	clear(m.approvalTransports)
	m.slackActionHandlers = nil
	clear(m.predeclaredModules)
	clear(m.predeclaredTools)
	clear(m.platformHandlerFactories)
	m.contextEngine = nil
}

// Removes a plugin platform from the gateway registry when the gateway is installed
func unregisterGatewayPlatform(name string) {
	if s := installedServices(); s != nil && s.Platforms != nil {
		if err := callGuarded(func() error { s.Platforms.Unregister(name); return nil }); err != nil {
			logDebug(fmt.Sprintf("unload: platform unregister %s failed: %v", name, err))
		}
	}
}

var (
	portableLivenessMu sync.Mutex
	portableLiveness   = map[string]map[string]any{}
)

// Records or clears the liveness declaration of a portable MCP server
func setPortableLiveness(server string, value map[string]any) {
	portableLivenessMu.Lock()
	defer portableLivenessMu.Unlock()
	if value == nil {
		delete(portableLiveness, server)
		return
	}
	copied := map[string]any{}
	for k, v := range value {
		copied[k] = v
	}
	portableLiveness[server] = copied
}

// Forgets the liveness declaration of a portable MCP server
func clearPortableLiveness(server string) { setPortableLiveness(server, nil) }

// Copy of the liveness declaration of a portable MCP server, nil when it declared none
func PortableLiveness(server string) map[string]any {
	portableLivenessMu.Lock()
	defer portableLivenessMu.Unlock()
	value, ok := portableLiveness[server]
	if !ok {
		return nil
	}
	copied := make(map[string]any, len(value))
	for k, v := range value {
		copied[k] = v
	}
	return copied
}
