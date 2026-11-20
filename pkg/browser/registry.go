package browser

import (
	"context"
	"fmt"

	"github.com/nickheyer/icebath/pkg/plugins"
	"github.com/nickheyer/icebath/pkg/provider"
)

// Registers the browser provider kinds plugins load into
func init() {
	plugins.RegisterProviderRegistry("browser_provider", BrowserProviderScopedRegistry)
	plugins.RegisterProviderRegistry("browser_page_backend", BrowserPageBackendScopedRegistry)
	plugins.RegisterProviderRegistry("login_backend", LoginBackendScopedRegistry)
}

// Cloud browser providers plugins register, consumed by the cloud provider selection
var BrowserProviderRegistry = provider.NewProviderRegistry[BrowserProvider](provider.ProviderRegistryOptions{Label: "Browser", Logger: pkgLog()})

// Browser page backends plugins register, consumed by browser_vault before the CDP session
var BrowserPageBackendRegistry = provider.NewProviderRegistry[BrowserPageBackend](provider.ProviderRegistryOptions{
	Label: "Browser page backend", Logger: pkgLog()})

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
