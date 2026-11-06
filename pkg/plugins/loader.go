package plugins

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/pm"
	"github.com/nickheyer/icebath/pkg/tools"
)

const (
	nsParent              = "icebath_plugins"
	loadTimeoutDefault    = 10.0
	maxLoadTimeout        = 600.0
	maxAbandonedLoaders   = 8
	loaderGoroutinePrefix = "plugin-load:"
)

var (
	moduleNamespaceMu sync.Mutex
	bareModuleScope   = map[string]string{}
	abandonedMu       sync.Mutex
	abandonedLoaders  []*abandonedLoader
	pluginHostMu      sync.Mutex
)

// Load worker that overran its deadline and still runs
type abandonedLoader struct {
	name string
	done chan struct{}
}

func (a *abandonedLoader) alive() bool {
	select {
	case <-a.done:
		return false
	default:
		return true
	}
}

// Raised on the loading goroutine when a plugin import and register overran its deadline
type PluginLoadTimeout struct{ Msg string }

func (e *PluginLoadTimeout) Error() string { return e.Msg }

// A plugin panic recovered during import or register, the Go form of SystemExit
type PluginPanicError struct {
	Value any
	Stack []byte
}

func (e *PluginPanicError) Error() string { return fmt.Sprint(e.Value) }

// Contexts loading on one deadline worker, nested loads joining it
type loadScope struct {
	mu   sync.Mutex
	ctxs []*Context
}

type loadScopeKey struct{}

// Whether ctx runs on a deadline worker where re-entrant discovery must not block on its parent
func inPluginLoadWorker(ctx context.Context) bool {
	return ctx.Value(loadScopeKey{}) != nil
}

// Per plugin load deadline from plugins.load_timeout_seconds, zero meaning inline loads
func resolvePluginLoadTimeout(h string) float64 {
	cfg, err := loadConfig(h)
	if err != nil {
		return loadTimeoutDefault
	}
	section, ok := cfg["plugins"].(map[string]any)
	if !ok || section["load_timeout_seconds"] == nil {
		return loadTimeoutDefault
	}
	timeout, ok := pyFloat(section["load_timeout_seconds"])
	if !ok || math.IsNaN(timeout) {
		logWarn(fmt.Sprintf("plugins.load_timeout_seconds is not a number; using default %gs", loadTimeoutDefault))
		return loadTimeoutDefault
	}
	if timeout < 0 {
		logWarn(fmt.Sprintf("plugins.load_timeout_seconds=%g is negative; using default %gs", timeout, loadTimeoutDefault))
		return loadTimeoutDefault
	}
	if timeout > maxLoadTimeout {
		logWarn(fmt.Sprintf("plugins.load_timeout_seconds=%g exceeds max %gs; clamping", timeout, maxLoadTimeout))
		return maxLoadTimeout
	}
	return timeout
}

// Drops finished abandoned loaders and refuses the load once the live cap is reached
func reserveAbandonedLoaderSlot() error {
	abandonedMu.Lock()
	defer abandonedMu.Unlock()
	live := abandonedLoaders[:0]
	for _, a := range abandonedLoaders {
		if a.alive() {
			live = append(live, a)
		}
	}
	abandonedLoaders = live
	if len(abandonedLoaders) < maxAbandonedLoaders {
		return nil
	}
	return &PluginLoadTimeout{Msg: fmt.Sprintf("not loaded: %d abandoned plugin loader thread(s) are still running (plugins.load_timeout_seconds); restart icebath to retry", maxAbandonedLoaders)}
}

// Runs a plugin import and register under the per plugin deadline, abandoning its contexts on timeout
func runWithLoadDeadline(ctx context.Context, pluginKey string, pctx *Context, fn func(context.Context) (bool, error)) (bool, error) {
	if outer, ok := ctx.Value(loadScopeKey{}).(*loadScope); ok {
		outer.mu.Lock()
		outer.ctxs = append(outer.ctxs, pctx)
		outer.mu.Unlock()
		defer func() {
			outer.mu.Lock()
			for i, c := range outer.ctxs {
				if c == pctx {
					outer.ctxs = append(outer.ctxs[:i], outer.ctxs[i+1:]...)
					break
				}
			}
			outer.mu.Unlock()
		}()
		return guardedLoad(ctx, fn)
	}
	timeout := resolvePluginLoadTimeout(home.Dir(ctx))
	if timeout <= 0 {
		return guardedLoad(ctx, fn)
	}
	if err := reserveAbandonedLoaderSlot(); err != nil {
		return false, err
	}
	scope := &loadScope{ctxs: []*Context{pctx}}
	worker := &abandonedLoader{name: loaderGoroutinePrefix + pluginKey, done: make(chan struct{})}
	var outcome bool
	var failure error
	go func() {
		defer close(worker.done)
		outcome, failure = guardedLoad(context.WithValue(ctx, loadScopeKey{}, scope), fn)
	}()
	timer := time.NewTimer(time.Duration(timeout * float64(time.Second)))
	defer timer.Stop()
	select {
	case <-worker.done:
		return outcome, failure
	case <-timer.C:
	}
	scope.mu.Lock()
	for _, c := range scope.ctxs {
		c.abandonLoad()
	}
	scope.mu.Unlock()
	abandonedMu.Lock()
	abandonedLoaders = append(abandonedLoaders, worker)
	abandonedMu.Unlock()
	return false, &PluginLoadTimeout{Msg: fmt.Sprintf("load timed out after %gs (import + register() never returned)", timeout)}
}

// Runs a load function converting a panic into a PluginPanicError
func guardedLoad(ctx context.Context, fn func(context.Context) (bool, error)) (ok bool, err error) {
	defer func() {
		if p := recover(); p != nil {
			if escaped, isInterrupt := interruptFromPanic(p); isInterrupt {
				ok, err = false, escaped
				return
			}
			ok, err = false, &PluginPanicError{Value: p, Stack: debug.Stack()}
		}
	}()
	return fn(ctx)
}

// Human readable load failure naming a recovered panic the way SystemExit is named
func loadErrorText(err error) string {
	var p *PluginPanicError
	if errors.As(err, &p) {
		return fmt.Sprintf("SystemExit(%s) raised during import/register()", config.PyRepr(p.Value))
	}
	return err.Error()
}

// Why this icebath is not one the plugin accepts, empty when it fits
func icebathFitError(mf *Manifest) string {
	if reason := RequiresVersionError(mf.RequiresIcebath); reason != "" {
		return reason
	}
	if mf.Path == "" || !isDir(mf.Path) {
		return ""
	}
	return coreRequirementError(mf.Path)
}

// Applicable core requirement refusal of a plugin directory, empty when its declaration cannot be read
func coreRequirementError(pluginDir string) string {
	declaration, err := pm.ReadPythonDeclaration(pluginDir)
	if err != nil {
		return ""
	}
	reason, err := pm.CoreRequirementError(context.Background(), declaration)
	if err != nil {
		return ""
	}
	return reason
}

// Reports missing Python dependencies without installing during discovery
func (m *Manager) warnPythonDependencies(ctx context.Context, mf *Manifest) {
	missing := missingPythonDependencies(ctx, mf)
	if len(missing) == 0 {
		return
	}
	key := mf.ID()
	if installedInSelectedEnvironment(ctx, missing) {
		logWarn(fmt.Sprintf("Plugin %s's Python dependencies were installed after this process started: %s. A running process never switches dependency environments; restart icebath (icebath gateway restart for the gateway) to load them.",
			key, strings.Join(missing, ", ")))
		return
	}
	logWarn(fmt.Sprintf("Plugin %s declares Python dependencies that are not installed: %s. For an enabled plugin, run icebath pm repair, then restart icebath. Discovery does not install dependencies.",
		key, strings.Join(missing, ", ")))
}

// Warns without blocking on plugin settings that violate the manifest config schema
func (m *Manager) validatePluginConfigSchema(mf *Manifest) {
	if len(mf.ConfigSchema) == 0 {
		return
	}
	pluginID := mf.ID()
	settings := map[string]any{}
	if cfg, err := loadConfig(m.home); err == nil {
		entry := pluginSettingsEntry(cfg, pluginID)
		raw, ok := entry["settings"].(map[string]any)
		if !ok {
			raw, ok = entry["config"].(map[string]any)
		}
		if ok {
			settings = raw
		}
	}
	for _, warning := range ValidateConfigSchema(pluginID, mf.ConfigSchema, settings) {
		logWarn(fmt.Sprintf("Plugin %s config: %s", pluginID, warning))
	}
}

// Loads one plugin with the manager home bound and its discovery lock held
func (m *Manager) loadPlugin(ctx context.Context, mf *Manifest) error {
	ctx, unlock := m.lockDiscovery(ctx)
	defer unlock()
	return m.loadPluginScoped(ctx, mf)
}

// Loads one plugin, recording its failure and disposing partial registrations, an error only for an interrupt
func (m *Manager) loadPluginScoped(ctx context.Context, mf *Manifest) error {
	loaded := &LoadedPlugin{Manifest: mf}
	key := mf.ID()
	logDebug(fmt.Sprintf("Loading plugin '%s' (source=%s, kind=%s, path=%s)", key, mf.Source, mf.Kind, mf.Path))
	if mf.Portable {
		m.loadPortablePlugin(ctx, mf, loaded)
		return nil
	}
	if reason := icebathFitError(mf); reason != "" {
		loaded.Error = reason
		logWarn(fmt.Sprintf("Plugin '%s' skipped: %s", key, reason))
		m.storeLoaded(key, loaded)
		return nil
	}
	m.mu.RLock()
	start := len(m.registrationOrder)
	m.mu.RUnlock()
	moduleName := m.policyModuleName(mf)
	m.trackToolOverridePolicy(mf, moduleName)
	pctx := newContext(mf, m)
	pctx.module = moduleName
	inHost := m.runsInPluginHost(mf)
	importAndRegister := func(ctx context.Context) (bool, error) {
		m.registerDeclaredLocales(mf, pctx)
		if isManifestOnlyLanguagePack(mf) {
			return true, nil
		}
		m.mu.Lock()
		predeclared, hasPredeclared := m.predeclaredModules[key]
		delete(m.predeclaredModules, key)
		m.mu.Unlock()
		loaded.Module = moduleName
		if hasPredeclared {
			loaded.Module = predeclared
		}
		if inHost {
			if mf.Path != "" && !isFile(filepath.Join(mf.Path, "__init__.py")) {
				return false, &FileNotFoundError{Msg: "No __init__.py in " + mf.Path}
			}
			host, err := m.pluginHost()
			if err != nil {
				return false, err
			}
			return true, host.Load(ctx, mf, pctx, moduleName, mf.Source != "user" && mf.Source != "project")
		}
		plugin, ok := bundledPlugin(key)
		if !ok {
			loaded.Error = "no register() function"
			logWarn(fmt.Sprintf("Plugin '%s' has no register() function", mf.Name))
			return false, nil
		}
		return true, plugin.Register(pctx)
	}
	ok, err := runWithLoadDeadline(ctx, key, pctx, importAndRegister)
	if err != nil && isUserInterrupt(ctx, err) {
		return err
	}
	if err == nil && ok {
		m.attributeRegistrations(loaded, key, start)
		loaded.Enabled = true
		m.dropFallbackHooks(hookSourceOf(mf.Name, loaded.Module))
	}
	if err != nil {
		m.mu.RLock()
		var owned []*Registration
		for _, r := range m.registrationOrder {
			if r.PluginKey == key {
				owned = append(owned, r)
			}
		}
		m.mu.RUnlock()
		m.disposeRegistrations(owned)
		m.forgetRegistrations(owned)
		loaded.Error = loadErrorText(err)
		m.removePluginSubscriptions(key)
		logWarn(fmt.Sprintf("Failed to load plugin '%s': %s", mf.Name, loadErrorText(err)), excInfo(err)...)
	}
	if !loaded.Enabled {
		m.mu.Lock()
		delete(m.predeclaredTools, key)
		m.mu.Unlock()
	}
	m.storeLoaded(key, loaded)
	return nil
}

func (m *Manager) storeLoaded(key string, loaded *LoadedPlugin) {
	m.mu.Lock()
	m.plugins[key] = loaded
	m.mu.Unlock()
}

// Whether every plugin carrying Python code goes to the profile plugin host, compiled-in plugins staying in process
func (m *Manager) runsInPluginHost(mf *Manifest) bool {
	_, compiled := bundledPlugin(mf.ID())
	return !compiled || mf.Path != ""
}

// This manager plugin host, created once on first use and retried after a failed creation
func (m *Manager) pluginHost() (*PluginHost, error) {
	pluginHostMu.Lock()
	defer pluginHostMu.Unlock()
	if m.pluginHostInstance != nil {
		return m.pluginHostInstance, nil
	}
	host, err := newPluginHost(m)
	if err != nil {
		return nil, err
	}
	m.pluginHostInstance = host
	return host, nil
}

// Whether a provides_locales plugin ships no Python and is complete from its locale files
func isManifestOnlyLanguagePack(mf *Manifest) bool {
	return len(mf.ProvidesLocales) > 0 && mf.Path != "" && !isFile(filepath.Join(mf.Path, "__init__.py"))
}

// Registers the locales directory of a provides_locales plugin, warning for a declared id with no file
func (m *Manager) registerDeclaredLocales(mf *Manifest, pctx *Context) {
	if len(mf.ProvidesLocales) == 0 || mf.Path == "" {
		return
	}
	dir := filepath.Join(mf.Path, "locales")
	registered := map[string]bool{}
	for _, h := range pctx.RegisterLocaleDir(dir, mf.LocaleMetadata) {
		lang, _, _ := strings.Cut(h.Key, ".")
		registered[lang] = true
	}
	for _, lang := range mf.ProvidesLocales {
		if !registered[lang] {
			logWarn(fmt.Sprintf("Plugin '%s' declares provides_locales %s but %s has no %s.yaml", mf.Name, config.PyRepr(lang), dir, lang))
		}
	}
}

// Installs the plugin tool override policy in the tool registry as a ledger owned lease
func (m *Manager) trackToolOverridePolicy(mf *Manifest, moduleName string) {
	scope := m.home
	replacementCoordinator.Transaction(func() {
		previous := tools.Default.SnapshotPluginOverridePolicy(moduleName, scope)
		current := tools.Default.RegisterPluginOverridePolicy(moduleName, newContext(mf, m).toolOverrideAllowed(""), scope)
		var prev any
		if previous != nil {
			prev = previous
		}
		lease := replacementCoordinator.Acquire([3]string{"tool_override_policy", scope, moduleName}, current, prev, func(replacement any) error {
			r, _ := replacement.(*tools.PluginOverridePolicy)
			tools.Default.RestorePluginOverridePolicy(moduleName, current, r, scope)
			return nil
		}, nil)
		m.trackRegistration(mf, "tool_override_policy", moduleName, lease.Dispose, false)
	})
}

// Fills the registered name lists of a loaded plugin from the ledger slice its register produced
func (m *Manager) attributeRegistrations(loaded *LoadedPlugin, key string, start int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var registrations []*Registration
	if start <= len(m.registrationOrder) {
		for _, r := range m.registrationOrder[start:] {
			if r.PluginKey == key && r.Active() {
				registrations = append(registrations, r)
			}
		}
	}
	keys := func(kind string) []string {
		out := []string{}
		for _, r := range registrations {
			if r.Kind == kind {
				out = append(out, r.Key)
			}
		}
		return out
	}
	var predeclared []string
	for _, t := range m.predeclaredTools[key] {
		if m.pluginToolNames[t] {
			predeclared = append(predeclared, t)
		}
	}
	delete(m.predeclaredTools, key)
	have := toSet(predeclared)
	loaded.ToolsRegistered = append([]string{}, predeclared...)
	for _, k := range keys("tool") {
		if !have[k] {
			loaded.ToolsRegistered = append(loaded.ToolsRegistered, k)
		}
	}
	loaded.HooksRegistered = keys("hook")
	loaded.MiddlewareRegistered = keys("middleware")
	loaded.CommandsRegistered = keys("command")
	cliKeys := toSet(keys("cli_command"))
	cli := 0
	for c := range m.cliCommands {
		if cliKeys[c] {
			cli++
		}
	}
	logDebug(fmt.Sprintf("  registered: %d tool(s), %d hook(s), %d middleware, %d slash command(s), %d CLI command(s)",
		len(loaded.ToolsRegistered), len(loaded.HooksRegistered), len(loaded.MiddlewareRegistered), len(loaded.CommandsRegistered), cli))
}

// Loads the validated skills and MCP servers of a portable package without running plugin code
func (m *Manager) loadPortablePlugin(ctx context.Context, mf *Manifest, loaded *LoadedPlugin) {
	key := mf.ID()
	err := callGuarded(func() error {
		pkg, err := LoadAgentPlugin(mf.Path, filepath.Join(home.Dir(ctx), "plugin-data", mf.DataNamespace))
		if err != nil {
			return err
		}
		pctx := newContext(mf, m)
		for _, d := range pkg.Diagnostics {
			logWarn(fmt.Sprintf("Agent Plugin '%s' [%s]: %s", key, d.Scope, d.Message))
		}
		for _, skill := range pkg.Skills {
			if _, err := pctx.RegisterSkill(skill.Name, skill.SkillMD, skill.Description, skill.Frontmatter); err != nil {
				logWarn(fmt.Sprintf("Agent Plugin '%s' skill '%s' skipped: %v", key, skill.Name, err))
			}
		}
		return m.registerPortableMCP(mf, pkg)
	})
	if err != nil {
		loaded.Error = loadErrorText(err)
		logWarn(fmt.Sprintf("Agent Plugin '%s' disabled: %s", key, loaded.Error))
	} else {
		loaded.Enabled = true
	}
	m.storeLoaded(key, loaded)
}

// Registers the MCP servers of a portable package, rolling every one back when a step fails
func (m *Manager) registerPortableMCP(mf *Manifest, pkg *AgentPluginPackage) (err error) {
	key := mf.ID()
	decls, _ := needService("PlatformDeclarations", func(s *Services) PlatformDeclarations { return s.PlatformDeclarations })
	var registered []string
	release := func(name string) {
		m.mu.Lock()
		delete(m.portableMCPServers, name)
		delete(m.portableMCPServerPlugins, name)
		m.mu.Unlock()
		if decls != nil {
			decls.Unregister(name)
		}
		clearPortableLiveness(name)
	}
	defer func() {
		if p := recover(); p != nil {
			err = &PluginPanicError{Value: p}
		}
		if err != nil {
			for _, name := range registered {
				release(name)
			}
		}
	}()
	for _, server := range sortedKeys(pkg.MCPServers) {
		internal := portableMCPServerName(server)
		m.mu.Lock()
		if _, taken := m.portableMCPServers[internal]; taken {
			owner := m.portableMCPServerPlugins[internal]
			m.mu.Unlock()
			if owner == "" {
				owner = "?"
			}
			logWarn(fmt.Sprintf("Agent Plugin '%s' MCP server '%s' skipped: name already taken by plugin '%s'; rename one server", key, internal, owner))
			continue
		}
		copied := map[string]any{}
		for k, v := range pkg.MCPServers[server] {
			copied[k] = v
		}
		m.portableMCPServers[internal] = copied
		m.portableMCPServerPlugins[internal] = key
		m.mu.Unlock()
		registered = append(registered, internal)
		if decl, ok := pkg.ServerDeclarations[server]; ok {
			if decls == nil {
				return &ServiceMissingError{Name: "PlatformDeclarations"}
			}
			decls.Register(internal, decl.App, decl.Requires)
			setPortableLiveness(internal, decl.Liveness)
		}
	}
	for _, name := range registered {
		m.trackRegistration(mf, "portable_mcp", name, func() error { release(name); return nil }, false)
	}
	return nil
}

// Profile safe module namespace of a plugin, the bare name for its first scope and a digest suffix otherwise
func (m *Manager) directoryModuleName(mf *Manifest) string {
	slug := strings.ReplaceAll(strings.ReplaceAll(mf.ID(), "/", "__"), "-", "_")
	bare := nsParent + "." + slug
	moduleNamespaceMu.Lock()
	defer moduleNamespaceMu.Unlock()
	if owner, ok := bareModuleScope[bare]; !ok || owner == m.home {
		bareModuleScope[bare] = m.home
		return bare
	}
	digest := sha256.Sum256([]byte(m.home))
	return bare + "__home_" + hex.EncodeToString(digest[:])[:12]
}

// Module prefix whose callbacks inherit the plugin policy
func (m *Manager) policyModuleName(mf *Manifest) string { return m.directoryModuleName(mf) }

// Policy module of a context, assigned by the loader or derived from its manifest
func (c *Context) policyModule() string {
	if c.module != "" {
		return c.module
	}
	return c.manager.policyModuleName(c.manifest)
}

// Platform name of a manifest without importing the adapter
func platformNameFromManifest(mf *Manifest) string {
	if strings.HasSuffix(mf.Name, "-platform") {
		return strings.TrimSuffix(mf.Name, "-platform")
	}
	if mf.Path != "" {
		return filepath.Base(mf.Path)
	}
	return mf.Name
}

// Registers a lazy platform loader with a visible placeholder, eager loading when the registry refuses it
func (m *Manager) registerDeferredPlatform(ctx context.Context, mf *Manifest) error {
	key := mf.ID()
	loaded := &LoadedPlugin{Manifest: mf, Enabled: true, Deferred: true}
	m.storeLoaded(key, loaded)
	if !m.leaseDeferredPlatform(mf, key) {
		return m.loadPlugin(ctx, mf)
	}
	m.registerDeferredPlatformTools(ctx, mf, loaded)
	return nil
}

// Re-leases the deferred loader of a failed platform plugin so the next lookup retries it
func (m *Manager) RearmFailedPlatform(ctx context.Context, platformName string) bool {
	ctx, unlock := m.lockDiscovery(ctx)
	defer unlock()
	var failed *Manifest
	m.mu.RLock()
	for _, key := range sortedKeys(m.plugins) {
		p := m.plugins[key]
		if p.Error != "" && !p.Enabled && p.Manifest.Kind == "platform" && platformNameFromManifest(p.Manifest) == platformName {
			failed = p.Manifest
			break
		}
	}
	m.mu.RUnlock()
	if failed == nil || icebathFitError(failed) != "" {
		return false
	}
	cfg := loadConfigOrEmpty(m.home)
	enabled, present := enabledPlugins(cfg)
	if action := GateManifest(m.home, failed, DisabledPlugins(m.home), enabled, present).Action; action != "defer" && action != "load" {
		return false
	}
	abandonedMu.Lock()
	for _, a := range abandonedLoaders {
		if a.alive() && a.name == loaderGoroutinePrefix+failed.ID() {
			abandonedMu.Unlock()
			return false
		}
	}
	abandonedMu.Unlock()
	logInfo("Re-arming failed platform plugin load: " + platformName)
	if err := m.registerDeferredPlatform(ctx, failed); err != nil {
		logDebug(fmt.Sprintf("re-armed platform %s load interrupted: %v", platformName, err))
	}
	return true
}

// Publishes the deferred loader as a ledger owned lease, false when the registry refused it
func (m *Manager) leaseDeferredPlatform(mf *Manifest, key string) bool {
	name := platformNameFromManifest(mf)
	var ok bool
	replacementCoordinator.Transaction(func() {
		err := callGuarded(func() error {
			registry, err := needService("Platforms", func(s *Services) PlatformRegistry { return s.Platforms })
			if err != nil {
				return err
			}
			scope := m.home
			loader := &DeferredPlatformLoader{}
			loader.Load = func(ctx context.Context) {
				ctx, unlock := m.lockDiscovery(ctx)
				defer unlock()
				if registry.IsDeferredLoadCancelled(name, scope) {
					return
				}
				if err := m.loadPluginScoped(ctx, mf); err != nil {
					logDebug(fmt.Sprintf("deferred platform %s load interrupted: %v", name, err))
				}
			}
			previous := registry.Snapshot(name, scope)
			registry.RegisterDeferred(name, loader, scope)
			env := make([]string, 0, len(mf.RequiresEnv))
			for _, e := range mf.RequiresEnv {
				env = append(env, e.Name)
			}
			registry.NoteActivationEnv(name, env)
			current := registry.Snapshot(name, scope)
			if current.Entry == nil && current.Loader == loader {
				m.leasePlatform(mf, name, registry, current, previous)
			}
			logDebug(fmt.Sprintf("Registered deferred platform loader: %s (plugin=%s)", name, key))
			return nil
		})
		if err != nil {
			logDebug(fmt.Sprintf("Deferred platform registration failed for '%s'; eager-loading", key), "error", err)
			return
		}
		ok = true
	})
	return ok
}

// Compiled-in plugin that pre-registers client tools of a deferred platform without its adapter
type ToolsRegistrar interface {
	RegisterTools(ctx *Context) error
}

// Pre-registers the client tools a deferred platform declares in provides_tools
func (m *Manager) registerDeferredPlatformTools(ctx context.Context, mf *Manifest, loaded *LoadedPlugin) {
	if len(mf.ProvidesTools) == 0 {
		return
	}
	key := mf.ID()
	declared := config.PyRepr(anyList(mf.ProvidesTools))
	plugin, compiled := bundledPlugin(key)
	if mf.Path != "" || !compiled {
		if mf.Path == "" || !isFile(filepath.Join(mf.Path, "tools.py")) {
			logWarn(fmt.Sprintf("Plugin '%s' declares provides_tools %s but has no tools.py; those tools will not be available in CLI/TUI sessions.", key, declared))
			return
		}
	}
	m.mu.RLock()
	before := map[string]bool{}
	for t := range m.pluginToolNames {
		before[t] = true
	}
	m.mu.RUnlock()
	credit := func() []string {
		m.mu.Lock()
		defer m.mu.Unlock()
		var registered []string
		for _, t := range sortedSet(m.pluginToolNames) {
			if !before[t] {
				registered = append(registered, t)
			}
		}
		if len(registered) > 0 {
			loaded.ToolsRegistered = registered
			m.predeclaredTools[key] = registered
		}
		return registered
	}
	moduleName := m.policyModuleName(mf)
	err := callGuarded(func() error {
		loaded.Module = moduleName
		m.mu.Lock()
		m.predeclaredModules[key] = moduleName
		m.mu.Unlock()
		pctx := newContext(mf, m)
		pctx.module = moduleName
		if mf.Path != "" || !compiled {
			host, err := m.pluginHost()
			if err != nil {
				return err
			}
			if err := host.LoadTools(ctx, mf, pctx, moduleName); err != nil {
				return err
			}
		} else {
			registrar, ok := plugin.(ToolsRegistrar)
			if !ok {
				logWarn(fmt.Sprintf("Plugin '%s' declares provides_tools %s but its tools.py has no register_tools(ctx); those tools will not be available in CLI/TUI sessions.", key, declared))
				return nil
			}
			if err := registrar.RegisterTools(pctx); err != nil {
				return err
			}
		}
		registered := credit()
		logDebug(fmt.Sprintf("Deferred platform '%s': pre-registered %d client tool(s) %s", key, len(registered), config.PyRepr(anyList(registered))))
		return nil
	})
	if err == nil {
		return
	}
	partial, total := credit(), len(mf.ProvidesTools)
	complete := len(partial) >= total
	var where string
	switch {
	case len(partial) == 0:
		where = fmt.Sprintf("before registering any of its %d declared tool(s)", total)
	case complete:
		where = fmt.Sprintf("after registering all %d declared tool(s)", total)
	default:
		where = fmt.Sprintf("after registering %d of %d declared tool(s)", len(partial), total)
	}
	tail := ""
	if !complete {
		tail = " The remainder will be missing from CLI/TUI sessions."
	}
	logWarn(fmt.Sprintf("Plugin '%s': client-tool pre-registration failed %s (%v).%s", key, where, err, tail), excInfo(err)...)
}

func isFile(path string) bool {
	st, err := os.Stat(path)
	return err == nil && st.Mode().IsRegular()
}

func isDir(path string) bool {
	st, err := os.Stat(path)
	return err == nil && st.IsDir()
}

func pathExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// Whether err is the user interrupt of ctx, a cancelled run context or a raised tool interrupt bit
func isUserInterrupt(ctx context.Context, err error) bool {
	if err == nil {
		return false
	}
	var escaped *UserInterrupt
	if errors.As(err, &escaped) {
		return true
	}
	if errors.Is(err, context.Canceled) && ctx.Err() != nil {
		return true
	}
	return errors.Is(err, ErrUserInterrupt) || tools.IsInterrupted(ctx)
}

// Error a plugin callback returns to propagate a user interrupt instead of reporting a failure
var ErrUserInterrupt = errors.New("user interrupt")

// UserInterrupt is the panic value a user interrupt escapes plugin dispatch with, recovered at the turn boundary
type UserInterrupt struct{ Err error }

func (u *UserInterrupt) Error() string { return u.Err.Error() }

func (u *UserInterrupt) Unwrap() error { return u.Err }

// User interrupt a recovered panic value carries, a UserInterrupt or an error wrapping ErrUserInterrupt
func interruptFromPanic(p any) (*UserInterrupt, bool) {
	if escaped, ok := p.(*UserInterrupt); ok {
		return escaped, true
	}
	if err, ok := p.(error); ok && errors.Is(err, ErrUserInterrupt) {
		return &UserInterrupt{Err: err}, true
	}
	return nil, false
}

// Unwinds the caller with a user interrupt the way KeyboardInterrupt escapes plugin isolation
func raiseUserInterrupt(err error) {
	var escaped *UserInterrupt
	if errors.As(err, &escaped) {
		panic(escaped)
	}
	panic(&UserInterrupt{Err: err})
}
