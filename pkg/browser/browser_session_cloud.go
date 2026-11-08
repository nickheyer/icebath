package browser

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/plugins"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Cache slot of one profile at one registry generation
type cloudCacheKey struct {
	scope string
	gen   provider.ProviderRegistryGeneration
}

// Resolved cloud providers per profile and registry generation, a definitive nil cached as local
var (
	cloudProviderMu  sync.Mutex
	cachedProviders  = map[cloudCacheKey]BrowserProvider{}
	cloudGetProvider = func(env *tools.Env, name string) BrowserProvider {
		return BrowserProviderRegistry.GetProvider(context.Background(), name, cfgOf(env).homeDir())
	}
	cloudRegistryGen   = func(env *tools.Env) provider.ProviderRegistryGeneration { return registryGeneration(env) }
	cloudAutodetectSet = func() []func() BrowserProvider {
		return []func() BrowserProvider{
			func() BrowserProvider { return newBrowserUseProvider() },
			func() BrowserProvider { return newBrowserbaseProvider() },
		}
	}
	ensureBrowserPluginsLoaded = func(env *tools.Env) {
		defer func() {
			if r := recover(); r != nil {
				cfgOf(env).logger().Debug(fmt.Sprintf("Browser plugin discovery failed (non-fatal): %v", r))
			}
		}()
		ctx := context.Background()
		plugins.GetPluginManager(ctx).DiscoverAndLoad(ctx, false)
	}
)

// Registry generation pair of the env's profile scope
func registryGeneration(env *tools.Env) provider.ProviderRegistryGeneration {
	return BrowserProviderRegistry.RegistryGeneration(context.Background(), cfgOf(env).homeDir())
}

// Drops the provider cache of every home or only the given home key
func resetCaches(homeKey string) {
	cloudProviderMu.Lock()
	defer cloudProviderMu.Unlock()
	for k := range cachedProviders {
		if homeKey == "" || k.scope == homeKey {
			delete(cachedProviders, k)
		}
	}
}

// Provider browser.cloud_provider names, a ValueError for an unregistered name, nil on a lookup that raised
func instantiateExplicitProvider(env *tools.Env, providerKey string) (resolved BrowserProvider, err error) {
	s := cfgOf(env)
	raised := false
	func() {
		defer func() {
			if r := recover(); r != nil {
				raised = true
				s.logger().Warn(fmt.Sprintf("Failed to instantiate cloud_provider %s; will retry on next call", pyrepr.Repr(providerKey)), "error", fmt.Sprint(r))
			}
		}()
		ensureBrowserPluginsLoaded(env)
		resolved = cloudGetProvider(env, providerKey)
	}()
	if raised {
		return nil, nil
	}
	if resolved == nil {
		return nil, valueError(tools.SelectionError("browser", "'"+providerKey+"'",
			"no registered browser plugin has that name (install the corresponding plugin or fix the config key spelling)"))
	}
	return resolved, nil
}

// Browser Use then Browserbase from credentials, any raised probe ending the scan with nil
func autodetectProvider(env *tools.Env) (found BrowserProvider) {
	defer func() {
		if r := recover(); r != nil {
			cfgOf(env).logger().Debug("Cloud provider auto-detect failed", "error", fmt.Sprint(r))
			found = nil
		}
	}()
	for _, build := range cloudAutodetectSet() {
		if p := build(); p.IsAvailable(env) {
			return p
		}
	}
	return nil
}

// Provider and whether the answer is definitive, explicit local or camofox and resolved providers being cached
func resolveProviderUncached(env *tools.Env) (BrowserProvider, bool, error) {
	section := cfgOf(env).section()
	providerKey, present := "", false
	if section.Has("cloud_provider") {
		raw := section.Get("cloud_provider")
		value := ""
		if pyrepr.Truthy(raw) {
			value = pyrepr.Str(raw)
		}
		providerKey, present = tools.NormalizeBrowserCloudProvider(value), true
	}
	if present && (providerKey == "local" || providerKey == "camofox") {
		return nil, true, nil
	}
	if providerKey == "nous" {
		providerKey = "browser-use"
	}
	if providerKey != "" {
		resolved, err := instantiateExplicitProvider(env, providerKey)
		if err != nil {
			return nil, false, err
		}
		return resolved, resolved != nil, nil
	}
	if present {
		return nil, false, nil
	}
	resolved := autodetectProvider(env)
	return resolved, resolved != nil, nil
}

// Cloud provider of the env's profile or nil for local browsing, a registry reload mid resolution resolving again
func getCloudProvider(env *tools.Env) (BrowserProvider, error) {
	scope := cfgOf(env).homeKey()
	cloudProviderMu.Lock()
	defer cloudProviderMu.Unlock()
	for {
		before := cloudRegistryGen(env)
		if p, ok := cachedProviders[cloudCacheKey{scope, before}]; ok {
			return p, nil
		}
		resolved, definitive, err := resolveProviderUncached(env)
		if err != nil {
			return nil, err
		}
		if cloudRegistryGen(env) != before {
			continue
		}
		if definitive {
			for k := range cachedProviders {
				if k.scope == scope {
					delete(cachedProviders, k)
				}
			}
			cachedProviders[cloudCacheKey{scope, before}] = resolved
		}
		return resolved, nil
	}
}

// Browser and terminal both local, a CDP override never trusted as local, a raised lookup panicking as Python raises
func isLocalBackend(env *tools.Env) bool {
	if cfgOf(env).cdpOverrideRaw() != "" {
		return false
	}
	p, err := getCloudProvider(env)
	if err != nil {
		panic(err)
	}
	if p != nil {
		return false
	}
	backend, err := tools.TerminalEnvRead(env, "TERMINAL_ENV", "local")
	if err != nil {
		panic(err)
	}
	backend = strings.ToLower(strings.TrimSpace(backend))
	return backend == "local" || backend == ""
}

// Shared metrics label of the backend in session precedence CDP override, local engine, cloud provider, engine
func browserBackendName(env *tools.Env, localEngine string) (string, error) {
	s := cfgOf(env)
	if s.cdpOverrideRaw() != "" {
		return "cdp", nil
	}
	if localEngine != "" {
		return localEngine, nil
	}
	p, err := getCloudProvider(env)
	if err != nil {
		return "", err
	}
	if p != nil {
		if name := p.Name(); name != "" {
			return name, nil
		}
		return "other", nil
	}
	if s.engine() == "lightpanda" {
		return "lightpanda", nil
	}
	return "local", nil
}

// Whether browser.headed asks for a visible window
func isHeadedMode(env *tools.Env) bool { return cfgOf(env).headed() }
