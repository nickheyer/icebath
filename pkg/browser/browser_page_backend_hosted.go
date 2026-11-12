package browser

import (
	"context"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/plugins"
)

// Base reference of the objects register_browser_page_backend receives from an in process plugin in the host
var hostedBrowserPageBackendBase = plugins.HostInProcessObjectBases["register_browser_page_backend"]

// Python browser page backend an in process plugin registered, living in the plugin host and called by reference
type hostedBrowserPageBackend struct{ obj *plugins.HostedObject }

// Plugin host object subclassing BrowserPageBackend as a backend, any other value unchanged for the wrong type warning
func adaptHostedBrowserPageBackend(p any) any {
	obj, ok := p.(*plugins.HostedObject)
	if !ok || obj == nil || obj.Base != hostedBrowserPageBackendBase || obj.Identity["is_base"] != true {
		return p
	}
	return hostedBrowserPageBackend{obj: obj}
}

// Attribute read in the host, a failing read raising like the Python attribute access
func (h hostedBrowserPageBackend) mustGet(name string) any {
	v, err := h.obj.Get(context.Background(), name)
	if err != nil {
		panic(err)
	}
	return v
}

func (h hostedBrowserPageBackend) Name() string { return config.PyStr(h.mustGet("name")) }

func (h hostedBrowserPageBackend) DisplayName() string {
	return config.PyStr(h.mustGet("display_name"))
}

// Setup schema the plugin declares, a non dict answer reading as no schema
func (h hostedBrowserPageBackend) GetSetupSchema() map[string]any {
	v, err := h.obj.Call(context.Background(), "get_setup_schema", nil, nil)
	if err != nil {
		panic(err)
	}
	schema, _ := v.(map[string]any)
	return schema
}

// Plugin's own answer whether it holds the task's live page, raising like the plugin method does
func (h hostedBrowserPageBackend) HasPage(taskID string) bool {
	v, err := h.obj.Call(context.Background(), "has_page", []any{taskID}, nil)
	if err != nil {
		panic(err)
	}
	return config.PyTruthy(v)
}

// Evaluates the expression on the task's page in the host with secret passed by keyword as the base declares it
func (h hostedBrowserPageBackend) Evaluate(taskID, expression string, secret bool) (any, error) {
	return h.obj.Call(context.Background(), "evaluate", []any{taskID, expression}, map[string]any{"secret": secret})
}

// Scoped registry view that wraps plugin host objects as BrowserPageBackend values before delegating
type hostedBrowserPageBackendScopedRegistry struct{ inner plugins.ScopedRegistry }

func (s hostedBrowserPageBackendScopedRegistry) Accepts(p any) bool {
	return s.inner.Accepts(adaptHostedBrowserPageBackend(p))
}
func (s hostedBrowserPageBackendScopedRegistry) ProviderName(p any) string {
	return s.inner.ProviderName(adaptHostedBrowserPageBackend(p))
}
func (s hostedBrowserPageBackendScopedRegistry) Identity(p any) error {
	return s.inner.Identity(adaptHostedBrowserPageBackend(p))
}
func (s hostedBrowserPageBackendScopedRegistry) Snapshot(name, scope string) any {
	return s.inner.Snapshot(name, scope)
}
func (s hostedBrowserPageBackendScopedRegistry) Register(p any, scope string) (bool, error) {
	return s.inner.Register(adaptHostedBrowserPageBackend(p), scope)
}
func (s hostedBrowserPageBackendScopedRegistry) Restore(name string, current, replacement any, scope string) bool {
	return s.inner.Restore(name, adaptHostedBrowserPageBackend(current), adaptHostedBrowserPageBackend(replacement), scope)
}

// Plugin facing registry that register_browser_page_backend writes into, adapting hosted Python backends
func BrowserPageBackendScopedRegistry() plugins.ScopedRegistry {
	return hostedBrowserPageBackendScopedRegistry{BrowserPageBackendRegistry.Scoped()}
}
