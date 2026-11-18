package browser

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/nickheyer/icebath/pkg/plugins"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Registers the browser provider kinds plugins load into
func init() {
	plugins.RegisterProviderRegistry("browser_provider", func() plugins.ScopedRegistry { return BrowserProviderRegistry.Scoped() })
	plugins.RegisterProviderRegistry("browser_page_backend", BrowserPageBackendScopedRegistry)
	plugins.RegisterProviderRegistry("login_backend", LoginBackendScopedRegistry)
}

// Cloud browser providers plugins register, consumed by the cloud provider selection
var BrowserProviderRegistry = provider.NewProviderRegistry[BrowserProvider](provider.ProviderRegistryOptions{Label: "Browser", Logger: pkgLog()})

// Browser page backends plugins register, consumed by browser_vault before the CDP session
var BrowserPageBackendRegistry = provider.NewProviderRegistry[BrowserPageBackend](provider.ProviderRegistryOptions{
	Label: "Browser page backend", Logger: pkgLog()})

// Only these names are auto eligible, a shared web key never routes to a paid cloud browser on its own
var legacyBrowserPreference = []string{"browser-use", "browserbase"}

// Configured provider even when unavailable, local as none, else the first available legacy provider
func resolveBrowserProvider(ctx context.Context, env *tools.Env, configured string) BrowserProvider {
	snapshot := BrowserProviderRegistry.Merged(ctx, "")
	if configured == "local" {
		return nil
	}
	if configured != "" {
		if p, ok := snapshot.Get(configured); ok {
			return p
		}
		pkgLog().Debug(fmt.Sprintf("browser cloud_provider '%s' configured but not registered; falling back to auto-detect", configured))
	}
	for _, legacy := range legacyBrowserPreference {
		if p, ok := snapshot.Get(legacy); ok && provider.IsAvailableSafe(boundAvailability{p, env}, pkgLog(),
			"Browser provider %s.is_available() raised %s — treating as unavailable", slog.LevelWarn, true) {
			return p
		}
	}
	return nil
}

// The registered backend holding the task's live page, a probe that panics meaning no page there
func backendForTask(ctx context.Context, taskID string) BrowserPageBackend {
	for _, backend := range BrowserPageBackendRegistry.ListProviders(ctx, "") {
		if hasPageSafe(backend, taskID) {
			return backend
		}
	}
	return nil
}

func hasPageSafe(backend BrowserPageBackend, taskID string) (held bool) {
	defer func() {
		if r := recover(); r != nil {
			pkgLog().Debug(fmt.Sprintf("browser page backend %s.has_page raised %v", backend.Name(), r))
			held = false
		}
	}()
	return backend.HasPage(taskID)
}
